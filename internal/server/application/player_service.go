// Package application contains client-independent server use cases.
//
// The legacy protocol adapters in server/probe are deliberately thin users of
// these services. Business rules in this package can therefore be exercised
// without launching Client.exe or constructing encrypted QQTang packets.
package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"

	"qqtang/internal/game/craftcatalog"
	"qqtang/internal/game/equipment"
	"qqtang/internal/game/petcatalog"
	"qqtang/internal/game/shopcatalog"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

// ProfileRepository owns durable account projections only.
type ProfileRepository interface {
	LoadOrCreate(context.Context, uint32, game.PlayerProfile) (game.PlayerProfile, error)
	Load(context.Context, uint32) (game.PlayerProfile, error)
	Save(context.Context, uint32, game.PlayerProfile) error
	SaveProfiles(context.Context, map[uint32]game.PlayerProfile) error
}

// InventoryRepository owns account item quantities and transactional buys.
type InventoryRepository interface {
	SetInventoryItem(context.Context, uint32, game.ItemInfo) error
	InventoryItem(context.Context, uint32, uint16) (game.ItemInfo, bool, error)
	PurchaseInventoryItem(context.Context, uint32, game.ItemInfo, uint32, bool) (game.PlayerProfile, error)
	ConsumeInventoryItems(context.Context, []persistence.InventoryConsumption) (map[uint32][]game.ItemInfo, error)
	ExchangeInventoryItems(context.Context, uint32, []persistence.InventoryConsumption, []game.ItemInfo) ([]game.ItemInfo, error)
}

// CraftRepository owns atomic forge/combine inventory transactions.
type CraftRepository interface {
	ApplyAvatarForge(context.Context, uint32, craftcatalog.ForgePlan) (game.ItemInfo, uint32, error)
	ApplyCombineRecipe(context.Context, uint32, craftcatalog.CombineRecipe) ([]game.ItemInfo, error)
	LearnCombineRecipe(context.Context, uint32, craftcatalog.CombineRecipe) (game.ItemInfo, error)
	ReconcileLearnedRecipes(context.Context, uint32, *craftcatalog.CombineCatalog) error
}

// EquipmentRepository owns role-specific loadout assignments.
type EquipmentRepository interface {
	LoadEquipment(context.Context, uint32) ([]equipment.Assignment, error)
	ApplyEquipmentChanges(context.Context, uint32, []equipment.Change) error
	ReconcileLegacyEquipment(context.Context, uint32, []equipment.Change, []uint16) error
	SaveItemStatusChanges(context.Context, uint32, game.PlayerProfile, []equipment.Change) error
}

// PetRepository owns independent pet entities and pet-item transactions.
type PetRepository interface {
	ListPets(context.Context, uint32) ([]game.PetInfo, error)
	GrantPet(context.Context, uint32, uint32, string) (game.PetInfo, error)
	DeletePet(context.Context, uint32, uint32) (bool, error)
	AdoptPetFromInventory(context.Context, uint32, uint16, uint32, string) (game.PetInfo, uint32, error)
	LearnPetSkillFromInventory(context.Context, uint32, uint32, uint16, byte, byte) (game.PetInfo, uint32, error)
	FeedPetFromInventory(context.Context, uint32, uint32, uint16, petcatalog.FoodEffect, []uint32) (game.PetInfo, uint32, error)
	SetPetActive(context.Context, uint32, uint32, bool) (game.PetInfo, error)
	RenamePet(context.Context, uint32, uint32, string) (game.PetInfo, error)
}

// PlayerRepository composes the capability-specific persistence contracts used
// by the current facade. PlayerStore satisfies it; future smaller application
// services can depend on only the capability they actually need.
type PlayerRepository interface {
	ProfileRepository
	InventoryRepository
	CraftRepository
	EquipmentRepository
	PetRepository
}

type PlayerService struct {
	repository PlayerRepository
	equipment  *equipment.Catalog
	playerMu   sync.Map // uint32 UIN -> *sync.Mutex
}

func NewPlayerService(repository PlayerRepository, equipmentCatalog *equipment.Catalog) (*PlayerService, error) {
	if repository == nil {
		return nil, fmt.Errorf("player service requires a repository")
	}
	return &PlayerService{repository: repository, equipment: equipmentCatalog}, nil
}

// LoadOrCreate returns the client-facing profile projection. Durable inventory
// ownership stays account-scoped, while equipped state is projected from the
// role-specific loadout table.
func (service *PlayerService) LoadOrCreate(ctx context.Context, uin uint32, seed game.PlayerProfile) (game.PlayerProfile, error) {
	unlock := service.lockPlayer(uin)
	defer unlock()
	profile, err := service.repository.LoadOrCreate(ctx, uin, seed)
	if err != nil {
		return game.PlayerProfile{}, err
	}
	return service.projectEquipmentLocked(ctx, uin, profile)
}

func (service *PlayerService) Load(ctx context.Context, uin uint32) (game.PlayerProfile, error) {
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.loadLocked(ctx, uin)
}

func (service *PlayerService) loadLocked(ctx context.Context, uin uint32) (game.PlayerProfile, error) {
	profile, err := service.repository.Load(ctx, uin)
	if err != nil {
		return game.PlayerProfile{}, err
	}
	return service.projectEquipmentLocked(ctx, uin, profile)
}

func (service *PlayerService) Save(ctx context.Context, uin uint32, profile game.PlayerProfile) error {
	if service == nil || service.repository == nil {
		return fmt.Errorf("player service is unavailable")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.repository.Save(ctx, uin, profile)
}

// UpdateProfile holds the same account lock as purchases and settlement from
// the initial read through the final save. Callers change only requested
// fields; unrelated inventory and progression come from the current record.
func (service *PlayerService) UpdateProfile(ctx context.Context, uin uint32, update func(*game.PlayerProfile) error) (game.PlayerProfile, error) {
	if service == nil || service.repository == nil || update == nil {
		return game.PlayerProfile{}, fmt.Errorf("profile update requires a player service and update function")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	profile, err := service.repository.Load(ctx, uin)
	if err != nil {
		return game.PlayerProfile{}, err
	}
	if err := update(&profile); err != nil {
		return game.PlayerProfile{}, err
	}
	if err := service.repository.Save(ctx, uin, profile); err != nil {
		return game.PlayerProfile{}, err
	}
	return service.projectEquipmentLocked(ctx, uin, profile)
}

// ConsumeInventoryItems serializes every involved account in stable UIN order,
// performs one repository transaction, and returns fresh profile projections.
func (service *PlayerService) ConsumeInventoryItems(ctx context.Context, requests []persistence.InventoryConsumption) (map[uint32]game.PlayerProfile, error) {
	if service == nil || service.repository == nil {
		return nil, fmt.Errorf("player service is unavailable")
	}
	uins := make([]uint32, 0, len(requests))
	seen := make(map[uint32]struct{}, len(requests))
	for _, request := range requests {
		if _, exists := seen[request.UIN]; exists {
			continue
		}
		seen[request.UIN] = struct{}{}
		uins = append(uins, request.UIN)
	}
	sort.Slice(uins, func(i, j int) bool { return uins[i] < uins[j] })
	for _, uin := range uins {
		unlock := service.lockPlayer(uin)
		defer unlock()
	}
	if _, err := service.repository.ConsumeInventoryItems(ctx, requests); err != nil {
		return nil, err
	}
	profiles := make(map[uint32]game.PlayerProfile, len(uins))
	for _, uin := range uins {
		profile, err := service.repository.Load(ctx, uin)
		if err != nil {
			return nil, fmt.Errorf("reload UIN %d after inventory consumption: %w", uin, err)
		}
		profile, err = service.projectEquipmentLocked(ctx, uin, profile)
		if err != nil {
			return nil, fmt.Errorf("project UIN %d after inventory consumption: %w", uin, err)
		}
		profiles[uin] = profile
	}
	return profiles, nil
}

// ExchangeInventoryItems applies an account-local consume/grant operation in
// one repository transaction. It is used by active functional props such as
// egg breaking, where consuming only the inputs or granting only the reward
// would corrupt the account after a crash.
func (service *PlayerService) ExchangeInventoryItems(ctx context.Context, uin uint32, consumes []persistence.InventoryConsumption, grants []game.ItemInfo) (game.PlayerProfile, []game.ItemInfo, error) {
	if service == nil || service.repository == nil || uin == 0 {
		return game.PlayerProfile{}, nil, fmt.Errorf("inventory exchange requires an available service and non-zero UIN")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	changed, err := service.repository.ExchangeInventoryItems(ctx, uin, consumes, grants)
	if err != nil {
		return game.PlayerProfile{}, nil, err
	}
	profile, err := service.repository.Load(ctx, uin)
	if err != nil {
		return game.PlayerProfile{}, nil, fmt.Errorf("reload UIN %d after inventory exchange: %w", uin, err)
	}
	profile, err = service.projectEquipmentLocked(ctx, uin, profile)
	if err != nil {
		return game.PlayerProfile{}, nil, fmt.Errorf("project UIN %d after inventory exchange: %w", uin, err)
	}
	return profile, changed, nil
}

func (service *PlayerService) ProjectEquipment(ctx context.Context, uin uint32, profile game.PlayerProfile) (game.PlayerProfile, error) {
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.projectEquipmentLocked(ctx, uin, profile)
}

// projectEquipmentLocked may reconcile one-time legacy equipment state and
// therefore is not a read-only helper. Every caller must own the UIN lock.
func (service *PlayerService) projectEquipmentLocked(ctx context.Context, uin uint32, profile game.PlayerProfile) (game.PlayerProfile, error) {
	if service == nil || service.repository == nil {
		return game.PlayerProfile{}, fmt.Errorf("player service is unavailable")
	}
	if service.equipment == nil {
		return cloneProfile(profile), nil
	}
	assignments, err := service.repository.LoadEquipment(ctx, uin)
	if err != nil {
		return game.PlayerProfile{}, err
	}
	// Older saves stored equipped state directly in ITEM_INFO.status/role_id.
	// The normalized loadout table is authoritative now, but silently clearing
	// those legacy flags would make passive platform effects (199/467/468) and
	// old cosmetics look selected in SQLite while disappearing at login. Import
	// only unambiguous, currently unoccupied role slots, then clear every legacy
	// status-1 equipment flag atomically. Existing normalized assignments always
	// win and 收藏柜 status 2 remains independent.
	legacyChanges, legacyItemIDs := service.legacyEquipmentReconciliation(profile.Inventory, assignments)
	if len(legacyItemIDs) != 0 {
		if err := service.repository.ReconcileLegacyEquipment(ctx, uin, legacyChanges, legacyItemIDs); err != nil {
			return game.PlayerProfile{}, fmt.Errorf("reconcile legacy equipment for UIN %d: %w", uin, err)
		}
		assignments, err = service.repository.LoadEquipment(ctx, uin)
		if err != nil {
			return game.PlayerProfile{}, err
		}
	}
	projected := cloneProfile(profile)
	projected.Inventory = service.equipment.ProjectInventory(projected.Inventory, assignments)
	return projected, nil
}

func (service *PlayerService) legacyEquipmentReconciliation(inventory []game.ItemInfo, assignments []equipment.Assignment) ([]equipment.Change, []uint16) {
	assignedItems := make(map[uint16]struct{}, len(assignments))
	occupied := make(map[string]struct{}, len(assignments))
	for _, assignment := range assignments {
		assignedItems[assignment.ItemID] = struct{}{}
		occupied[fmt.Sprintf("%d/%s", assignment.RoleID, assignment.Slot)] = struct{}{}
	}
	changes := make([]equipment.Change, 0)
	legacyItemIDs := make([]uint16, 0)
	seenLegacy := make(map[uint16]struct{})
	for _, item := range inventory {
		if item.ItemStatus != game.ItemStatusActive {
			continue
		}
		definition, recognized := service.lookupEquipment(item.ItemID)
		if !recognized {
			continue
		}
		if _, seen := seenLegacy[item.ItemID]; !seen {
			legacyItemIDs = append(legacyItemIDs, item.ItemID)
			seenLegacy[item.ItemID] = struct{}{}
		}
		if item.NumOfItem == 0 || item.ItemRoleID == 0 {
			continue
		}
		if _, assigned := assignedItems[item.ItemID]; assigned {
			continue
		}
		position := fmt.Sprintf("%d/%s", item.ItemRoleID, definition.Slot)
		if _, used := occupied[position]; used {
			continue
		}
		changes = append(changes, equipment.Change{
			RoleID: item.ItemRoleID, Slot: definition.Slot, ItemID: item.ItemID, Equipped: true,
		})
		assignedItems[item.ItemID] = struct{}{}
		occupied[position] = struct{}{}
	}
	return changes, legacyItemIDs
}

type PaymentMethod byte

const (
	PaymentQCoin PaymentMethod = iota
	PaymentQPoint
	PaymentGameMoney
	PaymentKubiGem
	PaymentVNet
)

type PurchaseStatus string

const (
	PurchaseSuccess           PurchaseStatus = "success"
	PurchaseUnknownCommodity  PurchaseStatus = "unknown_commodity"
	PurchaseExternalFunds     PurchaseStatus = "external_funds_unavailable"
	PurchaseUnsupportedMethod PurchaseStatus = "unsupported_payment"
	PurchaseInsufficientFunds PurchaseStatus = "insufficient_funds"
	PurchaseAlreadyOwned      PurchaseStatus = "already_owned"
)

type PurchaseResult struct {
	Status      PurchaseStatus
	Message     string
	Product     shopcatalog.Product
	Profile     game.PlayerProfile
	BalanceLeft uint32
}

// PurchaseCommodity is the authoritative local purchase use case. External
// currencies deliberately return a normal business rejection; only sugar
// currency mutates the SQLite account.
func (service *PlayerService) PurchaseCommodity(ctx context.Context, uin uint32, catalog *shopcatalog.Catalog, commodityID uint32, method PaymentMethod, mixed bool) (PurchaseResult, error) {
	if service == nil || service.repository == nil {
		return PurchaseResult{}, fmt.Errorf("player service is unavailable")
	}
	if catalog == nil {
		return PurchaseResult{}, fmt.Errorf("shop catalog is unavailable")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	product, found := catalog.Lookup(commodityID)
	if !found {
		return PurchaseResult{Status: PurchaseUnknownCommodity, Message: "商品不存在或已下架"}, nil
	}
	result := PurchaseResult{Product: product}
	if method != PaymentGameMoney {
		switch method {
		case PaymentQCoin:
			result.Status = PurchaseExternalFunds
			result.Message = "Q币不足"
			if mixed {
				result.Message = "Q币或Q点不足"
			}
		case PaymentQPoint:
			result.Status = PurchaseExternalFunds
			result.Message = "Q点不足"
			if mixed {
				result.Message = "Q点或Q币不足"
			}
		case PaymentKubiGem:
			result.Status = PurchaseExternalFunds
			result.Message = "酷比宝石不足"
		case PaymentVNet:
			result.Status = PurchaseExternalFunds
			result.Message = "该支付方式不可用"
		default:
			result.Status = PurchaseUnsupportedMethod
			result.Message = "未知支付方式"
		}
		return result, nil
	}

	_, cosmetic := service.lookupEquipment(product.ItemID)
	item := game.NewPermanentItemInfo(product.ItemID, product.Quantity)
	updated, err := service.repository.PurchaseInventoryItem(ctx, uin, item, product.SugarPrice, !cosmetic)
	if err != nil {
		switch {
		case errors.Is(err, persistence.ErrInsufficientGameMoney):
			return PurchaseResult{Status: PurchaseInsufficientFunds, Message: "糖币不足", Product: product}, nil
		case errors.Is(err, persistence.ErrInventoryItemOwned):
			return PurchaseResult{Status: PurchaseAlreadyOwned, Message: "该永久道具已在背包中", Product: product}, nil
		default:
			return PurchaseResult{}, err
		}
	}
	updated, err = service.projectEquipmentLocked(ctx, uin, updated)
	if err != nil {
		return PurchaseResult{}, err
	}
	return PurchaseResult{
		Status: PurchaseSuccess, Message: "购买成功", Product: product,
		Profile: updated, BalanceLeft: updated.GameInfo.Money,
	}, nil
}

// UpdateItemStatuses applies the confirmed 0x0085 semantics without knowing
// anything about packets or connections. Cosmetics update role loadouts;
// ordinary prepared props update their account inventory status fields.
func (service *PlayerService) UpdateItemStatuses(ctx context.Context, uin uint32, currentRole byte, profile game.PlayerProfile, changes []game.ItemStatusChange) (game.PlayerProfile, error) {
	if uin == 0 {
		return game.PlayerProfile{}, fmt.Errorf("item status update requires a non-zero UIN")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	current, err := service.repository.Load(ctx, uin)
	if err != nil {
		return game.PlayerProfile{}, fmt.Errorf("reload item-status-change profile: %w", err)
	}
	updated := cloneProfile(current)
	var equipmentChanges []equipment.Change
	for _, change := range changes {
		if change.ItemID == 0 || change.ItemID > math.MaxUint16 {
			return game.PlayerProfile{}, fmt.Errorf("item-status-change item ID %d is outside inventory range", change.ItemID)
		}
		index := inventoryIndex(updated.Inventory, uint16(change.ItemID))
		if index < 0 || updated.Inventory[index].NumOfItem == 0 {
			return game.PlayerProfile{}, fmt.Errorf("item-status-change item %d is not active", change.ItemID)
		}
		if definition, cosmetic := service.lookupEquipment(updated.Inventory[index].ItemID); cosmetic {
			roleID := change.NewRoleID
			if roleID == 0 {
				roleID = currentRole
			}
			if roleID == 0 {
				return game.PlayerProfile{}, fmt.Errorf("item-status-change equipment item %d has no role", change.ItemID)
			}
			equipmentChanges = append(equipmentChanges, equipment.Change{
				RoleID: roleID, Slot: definition.Slot, ItemID: updated.Inventory[index].ItemID,
				Equipped: change.NewStatus == game.ItemStatusActive,
			})
			// Equipment ownership state and role loadouts are orthogonal. SQLite
			// stores status 2 for the shop's 收藏柜, while status 1 is projected
			// exclusively from player_loadouts. Collecting also unequips the item;
			// restoring changes only the ownership-state flag.
			updated.Inventory[index].ItemStatus = change.NewStatus
			if change.NewStatus == game.ItemStatusActive {
				updated.Inventory[index].ItemStatus = game.ItemStatusAvailable
			}
			updated.Inventory[index].ItemRoleID = 0
			continue
		}
		updated.Inventory[index].ItemStatus = change.NewStatus
		updated.Inventory[index].ItemRoleID = change.NewRoleID
	}
	if err := updated.Validate(); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("validate item-status-change profile: %w", err)
	}
	if err := service.repository.SaveItemStatusChanges(ctx, uin, updated, equipmentChanges); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("persist item status and equipment changes: %w", err)
	}
	return service.projectEquipmentLocked(ctx, uin, updated)
}

// SetInventoryItem is the shared account-serialization boundary for
// administrative and gameplay inventory replacement.
func (service *PlayerService) SetInventoryItem(ctx context.Context, uin uint32, item game.ItemInfo) error {
	if service == nil || service.repository == nil {
		return fmt.Errorf("player service is unavailable")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.repository.SetInventoryItem(ctx, uin, item)
}

// ApplyEquipmentChanges serializes administrative loadout changes with match
// settlement and every other account mutation for the same UIN.
func (service *PlayerService) ApplyEquipmentChanges(ctx context.Context, uin uint32, changes []equipment.Change) error {
	if service == nil || service.repository == nil {
		return fmt.Errorf("player service is unavailable")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.repository.ApplyEquipmentChanges(ctx, uin, changes)
}

// ConsumePreparedItem performs the client-independent inventory part of using
// a prepared prop. Battle membership and player identity remain world rules
// checked by the caller.
func (service *PlayerService) ConsumePreparedItem(ctx context.Context, uin uint32, roleID byte, profile game.PlayerProfile, itemID uint16) (game.PlayerProfile, game.ItemInfo, error) {
	if uin == 0 || itemID == 0 {
		return game.PlayerProfile{}, game.ItemInfo{}, fmt.Errorf("prepared item use requires a UIN and item ID")
	}
	if _, cosmetic := service.lookupEquipment(itemID); cosmetic {
		return game.PlayerProfile{}, game.ItemInfo{}, fmt.Errorf("prepared-use-prop item %d is cosmetic equipment", itemID)
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	current, err := service.repository.Load(ctx, uin)
	if err != nil {
		return game.PlayerProfile{}, game.ItemInfo{}, fmt.Errorf("reload prepared-use-prop profile: %w", err)
	}
	updated := cloneProfile(current)
	index := inventoryIndex(updated.Inventory, itemID)
	if index < 0 {
		return game.PlayerProfile{}, game.ItemInfo{}, fmt.Errorf("prepared-use-prop item %d is not in the inventory", itemID)
	}
	item := updated.Inventory[index]
	if !item.Active() || item.ItemStatus == 0 || (item.ItemRoleID != 0 && item.ItemRoleID != roleID) {
		return game.PlayerProfile{}, game.ItemInfo{}, fmt.Errorf("prepared-use-prop item %d is not active in the selected role slot", itemID)
	}
	if err := updated.SetPermanentInventoryItem(itemID, item.NumOfItem-1); err != nil {
		return game.PlayerProfile{}, game.ItemInfo{}, err
	}
	if err := service.repository.Save(ctx, uin, updated); err != nil {
		return game.PlayerProfile{}, game.ItemInfo{}, fmt.Errorf("persist prepared-use-prop inventory: %w", err)
	}
	item.NumOfItem--
	return updated, item, nil
}

type AdventureSettlement struct {
	Result          game.GameResultCode
	AdventurePoints uint32
	CollectedItems  map[uint32]uint32
}

type CompetitiveSettlement struct {
	Result         game.GameResultCode
	Points         uint32
	MoneyReward    uint32
	CollectedItems map[uint32]uint32
}

type CompetitiveSettlementRequest struct {
	UIN        uint32
	Settlement CompetitiveSettlement
}

type AdventureSettlementRequest struct {
	UIN        uint32
	Settlement AdventureSettlement
}

type SettlementResult struct {
	Profile     game.PlayerProfile
	Progression game.ProgressionChange
}

func (service *PlayerService) ApplyCompetitiveSettlement(ctx context.Context, uin uint32, profile game.PlayerProfile, settlement CompetitiveSettlement) (game.PlayerProfile, game.ProgressionChange, error) {
	results, err := service.ApplyCompetitiveSettlements(ctx, []CompetitiveSettlementRequest{{UIN: uin, Settlement: settlement}})
	if err != nil {
		return game.PlayerProfile{}, game.ProgressionChange{}, err
	}
	result := results[uin]
	return result.Profile, result.Progression, nil
}

// ApplyCompetitiveSettlements commits one room's results in a single
// repository transaction. All affected UIN locks are acquired in stable order
// before any profile is loaded, so another application operation cannot
// observe or persist a half-settled room.
func (service *PlayerService) ApplyCompetitiveSettlements(ctx context.Context, requests []CompetitiveSettlementRequest) (map[uint32]SettlementResult, error) {
	if service == nil || service.repository == nil {
		return nil, fmt.Errorf("player service is unavailable")
	}
	uins, byUIN, err := normalizeCompetitiveSettlementRequests(requests)
	if err != nil {
		return nil, err
	}
	for _, uin := range uins {
		unlock := service.lockPlayer(uin)
		defer unlock()
	}
	profiles := make(map[uint32]game.PlayerProfile, len(uins))
	results := make(map[uint32]SettlementResult, len(uins))
	for _, uin := range uins {
		current, loadErr := service.repository.Load(ctx, uin)
		if loadErr != nil {
			return nil, fmt.Errorf("reload competitive settlement profile for UIN %d: %w", uin, loadErr)
		}
		updated, progression, projectErr := ProjectCompetitiveSettlement(current, byUIN[uin])
		if projectErr != nil {
			return nil, projectErr
		}
		profiles[uin] = updated
		results[uin] = SettlementResult{Profile: updated, Progression: progression}
	}
	if err = service.repository.SaveProfiles(ctx, profiles); err != nil {
		return nil, fmt.Errorf("persist competitive room settlement: %w", err)
	}
	return results, nil
}

func ProjectCompetitiveSettlement(profile game.PlayerProfile, settlement CompetitiveSettlement) (game.PlayerProfile, game.ProgressionChange, error) {
	updated := cloneProfile(profile)
	switch settlement.Result {
	case game.GameResultLoss:
		updated.GameInfo.LossNum = saturatingAdd(updated.GameInfo.LossNum, 1)
	case game.GameResultDraw:
		updated.GameInfo.EqualNum = saturatingAdd(updated.GameInfo.EqualNum, 1)
	case game.GameResultWin:
		updated.GameInfo.WinNum = saturatingAdd(updated.GameInfo.WinNum, 1)
	default:
		return game.PlayerProfile{}, game.ProgressionChange{}, fmt.Errorf("unsupported competitive settlement result %d", settlement.Result)
	}
	var progression game.ProgressionChange
	updated.GameInfo, progression = game.ApplyCompetitiveExperience(updated.GameInfo, settlement.Points)
	// GAME_OVER has no Money field. A Boss rule may supply MoneyReward only
	// after separately validating its reward event/configuration; ordinary
	// competitive settlement leaves this zero.
	updated.GameInfo, _ = game.ApplyGameMoneyReward(updated.GameInfo, settlement.MoneyReward)
	if err := addCollectedInventoryItems(&updated, settlement.CollectedItems); err != nil {
		return game.PlayerProfile{}, game.ProgressionChange{}, fmt.Errorf("apply competitive collected items: %w", err)
	}
	if err := updated.Validate(); err != nil {
		return game.PlayerProfile{}, game.ProgressionChange{}, fmt.Errorf("validate competitive settlement profile: %w", err)
	}
	return updated, progression, nil
}

func (service *PlayerService) ApplyAdventureSettlement(ctx context.Context, uin uint32, profile game.PlayerProfile, settlement AdventureSettlement) (game.PlayerProfile, game.ProgressionChange, error) {
	results, err := service.ApplyAdventureSettlements(ctx, []AdventureSettlementRequest{{UIN: uin, Settlement: settlement}})
	if err != nil {
		return game.PlayerProfile{}, game.ProgressionChange{}, err
	}
	result := results[uin]
	return result.Profile, result.Progression, nil
}

// ApplyAdventureSettlements is the adventure counterpart to the competitive
// batch operation. Collected materials and courage for every connected room
// participant either commit together or do not commit at all.
func (service *PlayerService) ApplyAdventureSettlements(ctx context.Context, requests []AdventureSettlementRequest) (map[uint32]SettlementResult, error) {
	if service == nil || service.repository == nil {
		return nil, fmt.Errorf("player service is unavailable")
	}
	uins, byUIN, err := normalizeAdventureSettlementRequests(requests)
	if err != nil {
		return nil, err
	}
	for _, uin := range uins {
		unlock := service.lockPlayer(uin)
		defer unlock()
	}
	profiles := make(map[uint32]game.PlayerProfile, len(uins))
	results := make(map[uint32]SettlementResult, len(uins))
	for _, uin := range uins {
		current, loadErr := service.repository.Load(ctx, uin)
		if loadErr != nil {
			return nil, fmt.Errorf("reload adventure settlement profile for UIN %d: %w", uin, loadErr)
		}
		updated, progression, projectErr := ProjectAdventureSettlement(current, byUIN[uin])
		if projectErr != nil {
			return nil, projectErr
		}
		profiles[uin] = updated
		results[uin] = SettlementResult{Profile: updated, Progression: progression}
	}
	if err = service.repository.SaveProfiles(ctx, profiles); err != nil {
		return nil, fmt.Errorf("persist adventure room settlement: %w", err)
	}
	return results, nil
}

func normalizeCompetitiveSettlementRequests(requests []CompetitiveSettlementRequest) ([]uint32, map[uint32]CompetitiveSettlement, error) {
	if len(requests) == 0 {
		return nil, nil, fmt.Errorf("competitive room settlement is empty")
	}
	byUIN := make(map[uint32]CompetitiveSettlement, len(requests))
	for _, request := range requests {
		if request.UIN == 0 {
			return nil, nil, fmt.Errorf("competitive room settlement contains zero UIN")
		}
		if _, duplicate := byUIN[request.UIN]; duplicate {
			return nil, nil, fmt.Errorf("competitive room settlement duplicates UIN %d", request.UIN)
		}
		byUIN[request.UIN] = request.Settlement
	}
	uins := make([]uint32, 0, len(byUIN))
	for uin := range byUIN {
		uins = append(uins, uin)
	}
	sort.Slice(uins, func(i, j int) bool { return uins[i] < uins[j] })
	return uins, byUIN, nil
}

func normalizeAdventureSettlementRequests(requests []AdventureSettlementRequest) ([]uint32, map[uint32]AdventureSettlement, error) {
	if len(requests) == 0 {
		return nil, nil, fmt.Errorf("adventure room settlement is empty")
	}
	byUIN := make(map[uint32]AdventureSettlement, len(requests))
	for _, request := range requests {
		if request.UIN == 0 {
			return nil, nil, fmt.Errorf("adventure room settlement contains zero UIN")
		}
		if _, duplicate := byUIN[request.UIN]; duplicate {
			return nil, nil, fmt.Errorf("adventure room settlement duplicates UIN %d", request.UIN)
		}
		byUIN[request.UIN] = request.Settlement
	}
	uins := make([]uint32, 0, len(byUIN))
	for uin := range byUIN {
		uins = append(uins, uin)
	}
	sort.Slice(uins, func(i, j int) bool { return uins[i] < uins[j] })
	return uins, byUIN, nil
}

func ProjectAdventureSettlement(profile game.PlayerProfile, settlement AdventureSettlement) (game.PlayerProfile, game.ProgressionChange, error) {
	if settlement.Result != game.GameResultLoss && settlement.Result != game.GameResultWin {
		return game.PlayerProfile{}, game.ProgressionChange{}, fmt.Errorf("unsupported adventure settlement result %d", settlement.Result)
	}
	updated := cloneProfile(profile)
	var progression game.ProgressionChange
	updated.GameInfo, progression = game.ApplyAdventureExperience(updated.GameInfo, settlement.AdventurePoints)
	if err := addCollectedInventoryItems(&updated, settlement.CollectedItems); err != nil {
		return game.PlayerProfile{}, game.ProgressionChange{}, err
	}
	if err := updated.Validate(); err != nil {
		return game.PlayerProfile{}, game.ProgressionChange{}, fmt.Errorf("validate adventure settlement profile: %w", err)
	}
	return updated, progression, nil
}

func addCollectedInventoryItems(updated *game.PlayerProfile, collectedItems map[uint32]uint32) error {
	if updated == nil {
		return fmt.Errorf("collected-item profile is nil")
	}
	for itemID, quantity := range collectedItems {
		if itemID == 0 || itemID > math.MaxUint16 {
			return fmt.Errorf("collected item ID %d is outside uint16 inventory range", itemID)
		}
		index := inventoryIndex(updated.Inventory, uint16(itemID))
		var existing uint32
		if index >= 0 {
			existing = updated.Inventory[index].NumOfItem
		}
		if err := updated.SetPermanentInventoryItem(uint16(itemID), saturatingAdd(existing, quantity)); err != nil {
			return fmt.Errorf("add collected item %d: %w", itemID, err)
		}
	}
	return nil
}

func (service *PlayerService) ListPets(ctx context.Context, uin uint32) ([]game.PetInfo, error) {
	return service.repository.ListPets(ctx, uin)
}

func (service *PlayerService) GrantPet(ctx context.Context, uin, petTypeID uint32, name string) (game.PetInfo, error) {
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.repository.GrantPet(ctx, uin, petTypeID, name)
}

func (service *PlayerService) DeletePet(ctx context.Context, uin, petID uint32) (bool, error) {
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.repository.DeletePet(ctx, uin, petID)
}

type PetInventoryResult struct {
	Pet               game.PetInfo
	ConsumedItemID    uint16
	RemainingQuantity uint32
}

type AvatarForgeResult struct {
	Plan              craftcatalog.ForgePlan
	Item              game.ItemInfo
	RemainingMaterial uint32
	Profile           game.PlayerProfile
}

type CombineResult struct {
	Recipe  craftcatalog.CombineRecipe
	Items   []game.ItemInfo
	Profile game.PlayerProfile
}

func (service *PlayerService) LearnCombineRecipe(ctx context.Context, uin uint32, catalog *craftcatalog.CombineCatalog, bookItemID uint16) (game.ItemInfo, game.PlayerProfile, error) {
	if service == nil || service.repository == nil || catalog == nil || uin == 0 || bookItemID == 0 {
		return game.ItemInfo{}, game.PlayerProfile{}, fmt.Errorf("learn recipe service is unavailable")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	recipe, found := catalog.LookupBook(bookItemID)
	if !found {
		return game.ItemInfo{}, game.PlayerProfile{}, fmt.Errorf("item %d is not a canonical combine book", bookItemID)
	}
	item, err := service.repository.LearnCombineRecipe(ctx, uin, recipe)
	if err != nil {
		return game.ItemInfo{}, game.PlayerProfile{}, err
	}
	profile, err := service.loadLocked(ctx, uin)
	if err != nil {
		return game.ItemInfo{}, game.PlayerProfile{}, fmt.Errorf("reload learned recipe profile: %w", err)
	}
	return item, profile, nil
}

// ReconcileLearnedRecipes rebuilds the server-side recipe index from the
// canonical client representation: an owned combine book whose ITEM_INFO
// status is ItemStatusLearnedRecipe. RequestLearnScroll reaches the server as
// ITEM_STATUS_CHANGE (0x0085), so this reconciliation belongs on that generic
// status path as well as login migration, not only on the older compatibility
// handler that called LearnCombineRecipe directly.
func (service *PlayerService) ReconcileLearnedRecipes(ctx context.Context, uin uint32, catalog *craftcatalog.CombineCatalog) error {
	if service == nil || service.repository == nil || catalog == nil || uin == 0 {
		return fmt.Errorf("learned recipe reconciliation is unavailable")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.repository.ReconcileLearnedRecipes(ctx, uin, catalog)
}

// Combine validates the client-provided material identity list against the
// authoritative recipe. Quantities always come from the recovered v110 table.
func (service *PlayerService) Combine(ctx context.Context, uin uint32, catalog *craftcatalog.CombineCatalog, productItemID uint16, requestedMaterialIDs []uint32) (CombineResult, error) {
	if service == nil || service.repository == nil || catalog == nil || uin == 0 || productItemID == 0 {
		return CombineResult{}, fmt.Errorf("combine service is unavailable")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	recipe, found := catalog.Lookup(productItemID)
	if !found {
		return CombineResult{}, fmt.Errorf("combine product %d has no canonical recipe", productItemID)
	}
	if len(requestedMaterialIDs) != len(recipe.Materials) {
		return CombineResult{Recipe: recipe}, fmt.Errorf("combine product %d sent %d materials, want %d", productItemID, len(requestedMaterialIDs), len(recipe.Materials))
	}
	requested := make(map[uint16]int, len(requestedMaterialIDs))
	for _, rawID := range requestedMaterialIDs {
		if rawID == 0 || rawID > math.MaxUint16 {
			return CombineResult{Recipe: recipe}, fmt.Errorf("combine material ID %d is outside inventory range", rawID)
		}
		requested[uint16(rawID)]++
	}
	for _, material := range recipe.Materials {
		if requested[material.ItemID] != 1 {
			return CombineResult{Recipe: recipe}, fmt.Errorf("combine product %d material list does not match recipe", productItemID)
		}
	}
	items, err := service.repository.ApplyCombineRecipe(ctx, uin, recipe)
	if err != nil {
		return CombineResult{Recipe: recipe}, err
	}
	profile, err := service.loadLocked(ctx, uin)
	if err != nil {
		return CombineResult{}, fmt.Errorf("reload combine profile: %w", err)
	}
	return CombineResult{Recipe: recipe, Items: items, Profile: profile}, nil
}

// ForgeAvatar validates the original avatarforge.ini rules before crossing
// the persistence boundary. Color and visual effect remain independent bytes
// throughout the use case and the legacy response.
func (service *PlayerService) ForgeAvatar(ctx context.Context, uin uint32, catalog *craftcatalog.Catalog, operation craftcatalog.ForgeOperation, targetItemID, materialItemID uint16, entropy io.Reader) (AvatarForgeResult, error) {
	if service == nil || service.repository == nil || catalog == nil || uin == 0 {
		return AvatarForgeResult{}, fmt.Errorf("avatar forge service is unavailable")
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	target, found, err := service.repository.InventoryItem(ctx, uin, targetItemID)
	if err != nil {
		return AvatarForgeResult{}, err
	}
	if !found {
		return AvatarForgeResult{}, fmt.Errorf("avatar forge target %d is not owned", targetItemID)
	}
	plan, err := catalog.Plan(operation, target, materialItemID, entropy)
	if err != nil {
		return AvatarForgeResult{Item: target}, err
	}
	item, remaining, err := service.repository.ApplyAvatarForge(ctx, uin, plan)
	if err != nil {
		if item.ItemID == 0 {
			item = target
		}
		return AvatarForgeResult{Plan: plan, Item: item}, err
	}
	profile, err := service.loadLocked(ctx, uin)
	if err != nil {
		return AvatarForgeResult{}, fmt.Errorf("reload avatar forge profile: %w", err)
	}
	return AvatarForgeResult{Plan: plan, Item: item, RemainingMaterial: remaining, Profile: profile}, nil
}

// AdoptPetFromCard keeps account-item identity and owned-pet identity
// separate. CardLink is the audited itemCFG -> PetCfg mapping.
func (service *PlayerService) AdoptPetFromCard(ctx context.Context, uin uint32, link petcatalog.CardLink) (PetInventoryResult, error) {
	if link.ItemID == 0 || link.ItemID > math.MaxUint16 || link.PetTypeID == 0 {
		return PetInventoryResult{}, fmt.Errorf("invalid pet-card mapping item=%d type=%d", link.ItemID, link.PetTypeID)
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	pet, remaining, err := service.repository.AdoptPetFromInventory(ctx, uin, uint16(link.ItemID), link.PetTypeID, link.Name)
	if err != nil {
		return PetInventoryResult{}, err
	}
	return PetInventoryResult{Pet: pet, ConsumedItemID: uint16(link.ItemID), RemainingQuantity: remaining}, nil
}

// LearnPetSkillFromBook uses itemCFG's category-local resource ID as the
// protocol SkillID. The client stores that value in PET_BASE_INFO.Skills.
func (service *PlayerService) LearnPetSkillFromBook(ctx context.Context, uin, petID uint32, book petcatalog.SkillBookLink) (PetInventoryResult, error) {
	if book.ItemID == 0 || book.ItemID > math.MaxUint16 || book.Skill.SkillID == 0 || book.Skill.Level == 0 || !book.Skill.Learned {
		return PetInventoryResult{}, fmt.Errorf("item %d is not a canonical learned pet skill book", book.ItemID)
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	pet, remaining, err := service.repository.LearnPetSkillFromInventory(
		ctx, uin, petID, uint16(book.ItemID), book.Skill.SkillID, book.Skill.Level,
	)
	if err != nil {
		return PetInventoryResult{}, err
	}
	return PetInventoryResult{Pet: pet, ConsumedItemID: uint16(book.ItemID), RemainingQuantity: remaining}, nil
}

// FeedPet consumes one canonical food item and applies the three original
// pet axes atomically. Incomplete original descriptions are rejected by the
// protocol adapter before reaching this method.
func (service *PlayerService) FeedPet(ctx context.Context, uin, petID uint32, food petcatalog.FoodLink, thresholds []uint32) (PetInventoryResult, error) {
	if food.ItemID == 0 || food.ItemID > math.MaxUint16 || !food.Complete {
		return PetInventoryResult{}, fmt.Errorf("item %d is not pet food with a complete original effect", food.ItemID)
	}
	unlock := service.lockPlayer(uin)
	defer unlock()
	pet, remaining, err := service.repository.FeedPetFromInventory(
		ctx, uin, petID, uint16(food.ItemID), food.Effect, thresholds,
	)
	if err != nil {
		return PetInventoryResult{}, err
	}
	return PetInventoryResult{Pet: pet, ConsumedItemID: uint16(food.ItemID), RemainingQuantity: remaining}, nil
}

func (service *PlayerService) SetPetActive(ctx context.Context, uin, petID uint32, active bool) (game.PetInfo, error) {
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.repository.SetPetActive(ctx, uin, petID, active)
}

func (service *PlayerService) RenamePet(ctx context.Context, uin, petID uint32, name string) (game.PetInfo, error) {
	unlock := service.lockPlayer(uin)
	defer unlock()
	return service.repository.RenamePet(ctx, uin, petID, name)
}

func (service *PlayerService) lockPlayer(uin uint32) func() {
	if service == nil {
		return func() {}
	}
	value, _ := service.playerMu.LoadOrStore(uin, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (service *PlayerService) lookupEquipment(itemID uint16) (equipment.Item, bool) {
	if service == nil || service.equipment == nil {
		return equipment.Item{}, false
	}
	return service.equipment.Lookup(itemID)
}

func cloneProfile(profile game.PlayerProfile) game.PlayerProfile {
	profile.Inventory = append([]game.ItemInfo(nil), profile.Inventory...)
	return profile
}

func inventoryIndex(items []game.ItemInfo, itemID uint16) int {
	for index := range items {
		if items[index].ItemID == itemID {
			return index
		}
	}
	return -1
}

func saturatingAdd(value, increment uint32) uint32 {
	if increment > math.MaxUint32-value {
		return math.MaxUint32
	}
	return value + increment
}
