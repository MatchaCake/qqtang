package application

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"qqtang/internal/game/equipment"
	"qqtang/internal/game/itemcatalog"
	roomstate "qqtang/internal/game/room"
	"qqtang/internal/game/shopcatalog"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

type playerSerializationBarrier struct {
	*persistence.PlayerStore
	loadCalls      chan struct{}
	releaseLoad    chan struct{}
	inventoryCalls chan struct{}
}

func (repository *playerSerializationBarrier) Load(ctx context.Context, uin uint32) (game.PlayerProfile, error) {
	repository.loadCalls <- struct{}{}
	<-repository.releaseLoad
	return repository.PlayerStore.Load(ctx, uin)
}

func (repository *playerSerializationBarrier) SetInventoryItem(ctx context.Context, uin uint32, item game.ItemInfo) error {
	repository.inventoryCalls <- struct{}{}
	return repository.PlayerStore.SetInventoryItem(ctx, uin, item)
}

func TestPlayerServiceSerializesAdministrativeWritesAndLoadWithSettlement(t *testing.T) {
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const uin uint32 = 1_000_001
	profile := game.DefaultPlayerProfile()
	if err = store.Save(t.Context(), uin, profile); err != nil {
		t.Fatal(err)
	}
	repository := &playerSerializationBarrier{
		PlayerStore: store, loadCalls: make(chan struct{}, 2), releaseLoad: make(chan struct{}), inventoryCalls: make(chan struct{}, 1),
	}
	service, err := NewPlayerService(repository, nil)
	if err != nil {
		t.Fatal(err)
	}

	settlementDone := make(chan error, 1)
	go func() {
		_, applyErr := service.ApplyCompetitiveSettlements(t.Context(), []CompetitiveSettlementRequest{{
			UIN: uin, Settlement: CompetitiveSettlement{Result: game.GameResultWin},
		}})
		settlementDone <- applyErr
	}()
	select {
	case <-repository.loadCalls:
	case <-time.After(time.Second):
		t.Fatal("settlement did not reach repository load")
	}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- service.SetInventoryItem(t.Context(), uin, game.NewPermanentItemInfo(22, 1))
	}()
	loadDone := make(chan error, 1)
	go func() {
		_, loadErr := service.Load(t.Context(), uin)
		loadDone <- loadErr
	}()
	select {
	case <-repository.inventoryCalls:
		t.Fatal("administrative inventory write bypassed the settlement UIN lock")
	case <-repository.loadCalls:
		t.Fatal("PlayerService.Load bypassed the settlement UIN lock")
	case <-time.After(100 * time.Millisecond):
	}

	close(repository.releaseLoad)
	if err = <-settlementDone; err != nil {
		t.Fatal(err)
	}
	if err = <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err = <-loadDone; err != nil {
		t.Fatal(err)
	}
	stored, err := store.Load(t.Context(), uin)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Inventory) != 1 || stored.Inventory[0].ItemID != 22 || stored.GameInfo.WinNum != 1 {
		t.Fatalf("serialized account result = %+v", stored)
	}
}

func TestPlayerServiceRunsAssetLifecycleWithoutClient(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewPlayerService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := game.DefaultPlayerProfile()
	seed.GameInfo.Money = 5000
	profile, err := service.LoadOrCreate(ctx, 1000001, seed)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := shopcatalog.NewCatalog(848,
		[]itemcatalog.RegistryEntry{{ItemID: 20043, Category: "item", ResourceID: 1, Name: "大体力药水"}},
		[]itemcatalog.CommodityEntry{{CommodityID: 90001, Category: "item", ResourceID: 1, Name: "大体力药水"}}, 10)
	if err != nil {
		t.Fatal(err)
	}

	rejected, err := service.PurchaseCommodity(ctx, 1000001, catalog, 90001, PaymentQCoin, true)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != PurchaseExternalFunds || rejected.Message != "Q币或Q点不足" {
		t.Fatalf("unexpected external-currency result: %+v", rejected)
	}
	afterReject, err := service.Load(ctx, 1000001)
	if err != nil {
		t.Fatal(err)
	}
	if afterReject.GameInfo.Money != profile.GameInfo.Money || len(afterReject.Inventory) != 0 {
		t.Fatalf("rejected purchase mutated profile: %+v", afterReject)
	}

	purchased, err := service.PurchaseCommodity(ctx, 1000001, catalog, 90001, PaymentGameMoney, false)
	if err != nil {
		t.Fatal(err)
	}
	if purchased.Status != PurchaseSuccess || purchased.BalanceLeft != 4000 || len(purchased.Profile.Inventory) != 1 {
		t.Fatalf("unexpected sugar purchase result: %+v", purchased)
	}
	profile, err = service.UpdateItemStatuses(ctx, 1000001, purchased.Profile.GameInfo.RoleID, purchased.Profile,
		[]game.ItemStatusChange{{ItemID: 20043, NewStatus: 1}})
	if err != nil {
		t.Fatal(err)
	}
	profile, remaining, err := service.ConsumePreparedItem(ctx, 1000001, profile.GameInfo.RoleID, profile, 20043)
	if err != nil {
		t.Fatal(err)
	}
	if remaining.NumOfItem != 0 {
		t.Fatalf("remaining potion count = %d, want 0", remaining.NumOfItem)
	}

	pet, err := service.GrantPet(ctx, 1000001, 25009, "旺旺狗")
	if err != nil {
		t.Fatal(err)
	}
	pets, err := service.ListPets(ctx, 1000001)
	if err != nil || len(pets) != 1 || pets[0].PetID != pet.PetID {
		t.Fatalf("pet lifecycle mismatch pets=%+v err=%v", pets, err)
	}

	settled, progression, err := service.ApplyAdventureSettlement(ctx, 1000001, profile, AdventureSettlement{
		Result: game.GameResultWin, AdventurePoints: 110, CollectedItems: map[uint32]uint32{30067: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if progression.AppliedPoints != 110 || settled.GameInfo.ExtPoint != profile.GameInfo.ExtPoint+110 {
		t.Fatalf("unexpected adventure progression: %+v", progression)
	}
	loaded, err := service.Load(ctx, 1000001)
	if err != nil {
		t.Fatal(err)
	}
	if index := inventoryIndex(loaded.Inventory, 30067); index < 0 || loaded.Inventory[index].NumOfItem != 3 {
		t.Fatalf("settlement item was not persisted: %+v", loaded.Inventory)
	}
	if removed, err := service.DeletePet(ctx, 1000001, pet.PetID); err != nil || !removed {
		t.Fatalf("delete pet removed=%t err=%v", removed, err)
	}
}

func TestPlayerServiceReportsInventoryKindLimitForShop(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const uin uint32 = 1_000_001
	profile := game.DefaultPlayerProfile()
	profile.GameInfo.Money = 1_000
	for itemID := uint16(1_000); len(profile.Inventory) < game.MaxItemInfoCount; itemID++ {
		profile.Inventory = append(profile.Inventory, game.NewPermanentItemInfo(itemID, 1))
	}
	if err = store.Save(ctx, uin, profile); err != nil {
		t.Fatal(err)
	}
	service, err := NewPlayerService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := shopcatalog.NewCatalog(848,
		[]itemcatalog.RegistryEntry{{ItemID: 20_001, Category: "item", ResourceID: 1, Name: "新道具"}},
		[]itemcatalog.CommodityEntry{{CommodityID: 90_001, Category: "item", ResourceID: 1, Name: "新道具"}},
		shopcatalog.DefaultCommodityLimit)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.PurchaseCommodity(ctx, uin, catalog, 90_001, PaymentGameMoney, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != PurchaseInventoryFull || result.Message != "背包种类已满，最多 500 种" {
		t.Fatalf("inventory-full purchase result = %+v", result)
	}
	after, err := store.Load(ctx, uin)
	if err != nil {
		t.Fatal(err)
	}
	if after.GameInfo.Money != 1_000 || len(after.Inventory) != game.MaxItemInfoCount {
		t.Fatalf("inventory-full purchase mutated profile = money:%d kinds:%d", after.GameInfo.Money, len(after.Inventory))
	}
}

func TestPlayerServiceCommitsCompetitiveRoomAsOneBatch(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewPlayerService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, uin := range []uint32{1_000_001, 1_000_002} {
		if err = store.Save(ctx, uin, game.DefaultPlayerProfile()); err != nil {
			t.Fatal(err)
		}
	}
	results, err := service.ApplyCompetitiveSettlements(ctx, []CompetitiveSettlementRequest{
		{UIN: 1_000_002, Settlement: CompetitiveSettlement{Result: game.GameResultLoss, Points: 10}},
		{UIN: 1_000_001, Settlement: CompetitiveSettlement{Result: game.GameResultWin, Points: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[1_000_001].Profile.GameInfo.WinNum != 1 || results[1_000_002].Profile.GameInfo.LossNum != 1 {
		t.Fatalf("room settlement results = %+v", results)
	}
	for uin, wantWin := range map[uint32]bool{1_000_001: true, 1_000_002: false} {
		profile, loadErr := store.Load(ctx, uin)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if wantWin && profile.GameInfo.WinNum != 1 || !wantWin && profile.GameInfo.LossNum != 1 {
			t.Fatalf("persisted UIN %d result = %+v", uin, profile.GameInfo)
		}
	}
}

func TestPlayerServiceStatusUpdateDoesNotOverwriteANewerShopPurchase(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewPlayerService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed := game.DefaultPlayerProfile()
	seed.GameInfo.Money = 5000
	seed.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(20044, 3)}
	stale, err := service.LoadOrCreate(ctx, 1000001, seed)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := shopcatalog.NewCatalog(848,
		[]itemcatalog.RegistryEntry{{ItemID: 20043, Category: "item", ResourceID: 1, Name: "大体力药水"}},
		[]itemcatalog.CommodityEntry{{CommodityID: 90001, Category: "item", ResourceID: 1, Name: "大体力药水"}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	purchased, err := service.PurchaseCommodity(ctx, 1000001, catalog, 90001, PaymentGameMoney, false)
	if err != nil || purchased.Status != PurchaseSuccess {
		t.Fatalf("purchase = %+v, %v", purchased, err)
	}
	if _, err = service.UpdateItemStatuses(ctx, 1000001, stale.GameInfo.RoleID, stale,
		[]game.ItemStatusChange{{ItemID: 20044, NewStatus: 1}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := service.Load(ctx, 1000001)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GameInfo.Money != 4000 {
		t.Fatalf("money = %d, want 4000", loaded.GameInfo.Money)
	}
	if index := inventoryIndex(loaded.Inventory, 20043); index < 0 || loaded.Inventory[index].NumOfItem != 1 {
		t.Fatalf("newer shop purchase was overwritten: %+v", loaded.Inventory)
	}
	if index := inventoryIndex(loaded.Inventory, 20044); index < 0 || loaded.Inventory[index].ItemStatus != 1 {
		t.Fatalf("status update was not applied: %+v", loaded.Inventory)
	}
}

func TestPlayerServicePersistsRoleEnabledFunctionalItemProjection(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	equipmentCatalog, err := equipment.NewCatalog([]itemcatalog.Entry{
		{ID: 199, RegistryCategory: "platform", Name: "闪电卡"},
		{ID: 467, RegistryCategory: "platform", Name: "贝氏弯刀"},
		{ID: 468, RegistryCategory: "platform", Name: "少林金刚脚"},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewPlayerService(store, equipmentCatalog)
	if err != nil {
		t.Fatal(err)
	}
	const uin uint32 = 1_000_001
	seed := game.DefaultPlayerProfile()
	seed.Inventory = []game.ItemInfo{
		game.NewPermanentItemInfo(199, 1),
		game.NewPermanentItemInfo(467, 1),
		game.NewPermanentItemInfo(468, 1),
	}
	profile, err := service.LoadOrCreate(ctx, uin, seed)
	if err != nil {
		t.Fatal(err)
	}
	profile, err = service.UpdateItemStatuses(ctx, uin, 13, profile, []game.ItemStatusChange{{
		ItemID: 467, NewStatus: game.ItemStatusActive, NewRoleID: 13,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Inventory) != 3 ||
		profile.Inventory[1].ItemStatus != game.ItemStatusActive || profile.Inventory[1].ItemRoleID != 13 {
		t.Fatalf("functional item projection = %+v", profile.Inventory)
	}
	profile, err = service.UpdateItemStatuses(ctx, uin, 13, profile, []game.ItemStatusChange{
		{ItemID: 467, NewStatus: game.ItemStatusAvailable, NewRoleID: 0},
		{ItemID: 468, NewStatus: game.ItemStatusActive, NewRoleID: 13},
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Inventory[0].ItemStatus != game.ItemStatusAvailable || profile.Inventory[0].ItemRoleID != 0 ||
		profile.Inventory[1].ItemStatus != game.ItemStatusAvailable || profile.Inventory[1].ItemRoleID != 0 ||
		profile.Inventory[2].ItemStatus != game.ItemStatusActive || profile.Inventory[2].ItemRoleID != 13 {
		t.Fatalf("single platform-effect slot projection = %+v", profile.Inventory)
	}
	reloaded, err := service.Load(ctx, uin)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Inventory) != 3 ||
		reloaded.Inventory[0] != profile.Inventory[0] ||
		reloaded.Inventory[1] != profile.Inventory[1] ||
		reloaded.Inventory[2] != profile.Inventory[2] {
		t.Fatalf("reloaded functional item = %+v, want %+v", reloaded.Inventory, profile.Inventory)
	}
}

func TestPlayerServiceReconcilesLegacyActivePlatformEffect(t *testing.T) {
	ctx := context.Background()
	newCatalog := func(t *testing.T) *equipment.Catalog {
		t.Helper()
		catalog, err := equipment.NewCatalog([]itemcatalog.Entry{
			{ID: 467, RegistryCategory: "platform", Name: "贝氏弯刀"},
			{ID: 468, RegistryCategory: "platform", Name: "少林金刚脚"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return catalog
	}

	t.Run("imports unoccupied legacy slot", func(t *testing.T) {
		store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		service, err := NewPlayerService(store, newCatalog(t))
		if err != nil {
			t.Fatal(err)
		}
		seed := game.DefaultPlayerProfile()
		legacy := game.NewPermanentItemInfo(468, 1)
		legacy.ItemStatus = game.ItemStatusActive
		legacy.ItemRoleID = 13
		seed.Inventory = []game.ItemInfo{legacy}
		profile, err := service.LoadOrCreate(ctx, 1_000_001, seed)
		if err != nil {
			t.Fatal(err)
		}
		if profile.Inventory[0].ItemStatus != game.ItemStatusActive || profile.Inventory[0].ItemRoleID != 13 {
			t.Fatalf("projected legacy platform effect = %+v", profile.Inventory[0])
		}
		assignments, err := store.LoadEquipment(ctx, 1_000_001)
		if err != nil || len(assignments) != 1 || assignments[0].ItemID != 468 || assignments[0].Slot != equipment.SlotPlatformEffect {
			t.Fatalf("reconciled assignments = %+v, %v", assignments, err)
		}
		raw, err := store.Load(ctx, 1_000_001)
		if err != nil {
			t.Fatal(err)
		}
		if raw.Inventory[0].ItemStatus != game.ItemStatusAvailable || raw.Inventory[0].ItemRoleID != 0 {
			t.Fatalf("legacy raw state was not canonicalized: %+v", raw.Inventory[0])
		}
	})

	t.Run("normalized slot wins conflict", func(t *testing.T) {
		store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		seed := game.DefaultPlayerProfile()
		first := game.NewPermanentItemInfo(467, 1)
		legacy := game.NewPermanentItemInfo(468, 1)
		legacy.ItemStatus = game.ItemStatusActive
		legacy.ItemRoleID = 13
		seed.Inventory = []game.ItemInfo{first, legacy}
		if _, err := store.LoadOrCreate(ctx, 1_000_001, seed); err != nil {
			t.Fatal(err)
		}
		if err := store.ApplyEquipmentChanges(ctx, 1_000_001, []equipment.Change{{
			RoleID: 13, Slot: equipment.SlotPlatformEffect, ItemID: 467, Equipped: true,
		}}); err != nil {
			t.Fatal(err)
		}
		service, err := NewPlayerService(store, newCatalog(t))
		if err != nil {
			t.Fatal(err)
		}
		profile, err := service.Load(ctx, 1_000_001)
		if err != nil {
			t.Fatal(err)
		}
		if profile.Inventory[0].ItemStatus != game.ItemStatusActive || profile.Inventory[0].ItemRoleID != 13 ||
			profile.Inventory[1].ItemStatus != game.ItemStatusAvailable || profile.Inventory[1].ItemRoleID != 0 {
			t.Fatalf("normalized platform slot did not win: %+v", profile.Inventory)
		}
		assignments, err := store.LoadEquipment(ctx, 1_000_001)
		if err != nil || len(assignments) != 1 || assignments[0].ItemID != 467 {
			t.Fatalf("assignments after conflict = %+v, %v", assignments, err)
		}
	})
}

func TestPlayerServiceCollectsAndRestoresStackedMaterialAsOneInventoryRow(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewPlayerService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	const uin uint32 = 1_000_001
	seed := game.DefaultPlayerProfile()
	seed.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(20057, 12)}
	profile, err := service.LoadOrCreate(ctx, uin, seed)
	if err != nil {
		t.Fatal(err)
	}
	profile, err = service.UpdateItemStatuses(ctx, uin, 7, profile, []game.ItemStatusChange{{
		ItemID: 20057, NewStatus: game.ItemStatusCollected,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Inventory) != 1 || profile.Inventory[0].NumOfItem != 12 || profile.Inventory[0].ItemStatus != game.ItemStatusCollected {
		t.Fatalf("collected material stack = %+v", profile.Inventory)
	}
	profile, err = service.UpdateItemStatuses(ctx, uin, 7, profile, []game.ItemStatusChange{{
		ItemID: 20057, NewStatus: game.ItemStatusAvailable,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Inventory) != 1 || profile.Inventory[0].NumOfItem != 12 || profile.Inventory[0].ItemStatus != game.ItemStatusAvailable {
		t.Fatalf("restored material stack = %+v", profile.Inventory)
	}
}

func TestProjectCompetitiveSettlementKeepsAdventureProgressIndependent(t *testing.T) {
	profile := game.DefaultPlayerProfile()
	profile.GameInfo.Point = 100
	profile.GameInfo.ExtPoint = 500
	profile.GameInfo.ExtWinNum = 7
	updated, progression, err := ProjectCompetitiveSettlement(profile, CompetitiveSettlement{Result: game.GameResultWin, Points: 50})
	if err != nil {
		t.Fatal(err)
	}
	if updated.GameInfo.WinNum != profile.GameInfo.WinNum+1 || updated.GameInfo.Point != 150 || progression.AppliedPoints != 50 {
		t.Fatalf("competitive settlement = %+v, progression %+v", updated.GameInfo, progression)
	}
	if updated.GameInfo.ExtPoint != 500 || updated.GameInfo.ExtWinNum != 7 {
		t.Fatalf("competitive settlement changed adventure fields: %+v", updated.GameInfo)
	}
}

func TestProjectCompetitiveSettlementAddsBossCollectedItems(t *testing.T) {
	profile := game.DefaultPlayerProfile()
	profile.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(451, 2)}
	updated, _, err := ProjectCompetitiveSettlement(profile, CompetitiveSettlement{
		Result: game.GameResultWin, CollectedItems: map[uint32]uint32{451: 1, 458: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint16]uint32{451: 3, 458: 1}
	for _, item := range updated.Inventory {
		if quantity, ok := want[item.ItemID]; ok {
			if item.NumOfItem != quantity || !item.Active() {
				t.Fatalf("competitive Boss item %d = %+v, want quantity %d", item.ItemID, item, quantity)
			}
			delete(want, item.ItemID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("competitive Boss items missing after settlement: %+v", want)
	}
}

func TestProjectAdventureSettlementKeepsEveryCompetitiveRecordIndependent(t *testing.T) {
	profile := game.DefaultPlayerProfile()
	profile.GameInfo.WinNum = 5200
	profile.GameInfo.LossNum = 520
	profile.GameInfo.EqualNum = 52
	profile.GameInfo.Point = 1_621_150_000
	profile.GameInfo.Degree = 179
	profile.GameInfo.ExtPoint = 5_200_000
	profile.GameInfo.ExtWinNum = 523
	profile.GameInfo.ExtLossNum = 72
	profile.GameInfo.ExtEqualNum = 9

	for _, result := range []game.GameResultCode{game.GameResultLoss, game.GameResultWin} {
		updated, progression, err := ProjectAdventureSettlement(profile, AdventureSettlement{
			Result: result, AdventurePoints: 110,
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.GameInfo.WinNum != profile.GameInfo.WinNum ||
			updated.GameInfo.LossNum != profile.GameInfo.LossNum ||
			updated.GameInfo.EqualNum != profile.GameInfo.EqualNum ||
			updated.GameInfo.ExtWinNum != profile.GameInfo.ExtWinNum ||
			updated.GameInfo.ExtLossNum != profile.GameInfo.ExtLossNum ||
			updated.GameInfo.ExtEqualNum != profile.GameInfo.ExtEqualNum ||
			updated.GameInfo.Point != profile.GameInfo.Point ||
			updated.GameInfo.Degree != profile.GameInfo.Degree {
			t.Fatalf("adventure result %d changed competitive fields: before=%+v after=%+v", result, profile.GameInfo, updated.GameInfo)
		}
		if progression.AppliedPoints != 110 || updated.GameInfo.ExtPoint != profile.GameInfo.ExtPoint+110 {
			t.Fatalf("adventure result %d progression = %+v profile=%+v", result, progression, updated.GameInfo)
		}
	}
}

func TestProjectCompetitiveBossRewardCapsMoneyIndependently(t *testing.T) {
	profile := game.DefaultPlayerProfile()
	profile.GameInfo.Money = game.MaxGameMoney - 2
	updated, _, err := ProjectCompetitiveSettlement(profile, CompetitiveSettlement{
		Result: game.GameResultWin, Points: 10, MoneyReward: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.GameInfo.Money != game.MaxGameMoney {
		t.Fatalf("boss reward money = %d", updated.GameInfo.Money)
	}
}

func TestWorldRunsLobbyRoomAndMatchLifecycleWithoutClient(t *testing.T) {
	world := NewWorld()
	one := game.DefaultPlayerProfile()
	one.Nickname = "糖一"
	two := game.DefaultPlayerProfile()
	two.PlayerID = 2
	two.Nickname = "糖二"
	if err := world.EnterLobby(1000001, one); err != nil {
		t.Fatal(err)
	}
	if err := world.EnterLobby(1000002, two); err != nil {
		t.Fatal(err)
	}
	lobbyChat, err := world.RouteSectionChat(1000001, one.SectionID, "大厅消息", time.Unix(10, 0))
	if err != nil || len(lobbyChat.Recipients) != 2 {
		t.Fatalf("lobby chat = %+v, %v", lobbyChat, err)
	}
	created, err := world.CreateRoom(1000001, one, roomstate.MatchSettings{
		Map: roomstate.RandomMapSelection(), GameType: roomstate.GameTypeAdventure,
	}, roomstate.Properties{Name: "本地无客户端测试"})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := world.JoinRoom(1000002, two, created.RoomID, two.GameInfo.RoleID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(joined.Members) != 2 {
		t.Fatalf("joined room members=%d, want 2", len(joined.Members))
	}
	if _, err = world.SetReady(1000002, true); err != nil {
		t.Fatal(err)
	}
	started, gameID, err := world.StartMatch(1000001)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != roomstate.PhaseInMatch || gameID == 0 {
		t.Fatalf("unexpected started match: phase=%d game=%d", started.Phase, gameID)
	}
	completed, err := world.CompleteMatch(1000001, gameID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Phase != roomstate.PhasePreparing || len(completed.Members) != 2 {
		t.Fatalf("completed match did not retain room: %+v", completed)
	}
	departure, err := world.LeaveRoom(1000001, roomstate.LeaveVoluntary)
	if err != nil {
		t.Fatal(err)
	}
	if departure.Empty || departure.NewOwnerID != two.PlayerID {
		t.Fatalf("owner migration mismatch: %+v", departure)
	}
	departure, err = world.LeaveRoom(1000002, roomstate.LeaveVoluntary)
	if err != nil || !departure.Empty {
		t.Fatalf("last leave mismatch: %+v err=%v", departure, err)
	}
	snapshot, err := world.LobbySnapshot(one.SectionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Rooms) != 0 || len(snapshot.Players) != 2 {
		t.Fatalf("unexpected final lobby snapshot: %+v", snapshot)
	}
}

func TestWorldProjectsCurrentRoomOwnerPurpleDiamondIdentity(t *testing.T) {
	world := NewWorld()
	one := game.DefaultPlayerProfile()
	one.Identity = uint32(game.IdentityPurpleDiamond)
	two := game.DefaultPlayerProfile()
	two.PlayerID = 2
	two.Identity = 0
	if err := world.EnterLobby(1000001, one); err != nil {
		t.Fatal(err)
	}
	if err := world.EnterLobby(1000002, two); err != nil {
		t.Fatal(err)
	}
	created, err := world.CreateRoom(1000001, one, roomstate.MatchSettings{
		Map: roomstate.RandomMapSelection(), GameType: roomstate.GameTypeCompetitiveNoItem,
	}, roomstate.Properties{Name: "vip-owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = world.JoinRoom(1000002, two, created.RoomID, two.GameInfo.RoleID, 1); err != nil {
		t.Fatal(err)
	}
	snapshot, err := world.LobbySnapshot(one.SectionID, 0)
	if err != nil || len(snapshot.Rooms) != 1 || !snapshot.Rooms[0].IsVIP {
		t.Fatalf("purple-diamond owner room = %+v, %v", snapshot.Rooms, err)
	}
	if _, err = world.LeaveRoom(1000001, roomstate.LeaveVoluntary); err != nil {
		t.Fatal(err)
	}
	snapshot, err = world.LobbySnapshot(one.SectionID, 0)
	if err != nil || len(snapshot.Rooms) != 1 || snapshot.Rooms[0].IsVIP {
		t.Fatalf("ordinary successor room = %+v, %v", snapshot.Rooms, err)
	}
}

func TestWorldReconcilesRoomMemberWhenMembershipIndexIsMissing(t *testing.T) {
	world := NewWorld()
	profile := game.DefaultPlayerProfile()
	const uin uint32 = 1_000_001
	if err := world.EnterLobby(uin, profile); err != nil {
		t.Fatal(err)
	}
	created, err := world.CreateRoom(uin, profile, roomstate.MatchSettings{
		Map: roomstate.RandomMapSelection(), GameType: roomstate.GameTypeAdventure,
	}, roomstate.Properties{Name: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	world.mu.Lock()
	delete(world.memberships, uin)
	world.mu.Unlock()
	if _, err = world.LeaveRoom(uin, roomstate.LeaveDisconnected); err == nil {
		t.Fatal("corrupt membership index unexpectedly completed a normal leave")
	}
	departure, err := world.ReconcileRoomDeparture(uin, created.RoomID, profile.PlayerID, roomstate.LeaveDisconnected)
	if err != nil {
		t.Fatal(err)
	}
	if !departure.Empty || departure.Member.PlayerID != profile.PlayerID {
		t.Fatalf("reconciled departure = %+v", departure)
	}
	if _, active := world.Room(created.RoomID); active {
		t.Fatal("reconciled empty room is still active")
	}
	lobby, err := world.LobbySnapshot(profile.SectionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lobby.Rooms) != 0 || len(lobby.Players) != 1 || lobby.Players[0].RoomID != 0 {
		t.Fatalf("reconciled lobby = %+v", lobby)
	}
}

func TestWorldReusesLowestEmptyLegacyRoomID(t *testing.T) {
	world := NewWorld()
	one := game.DefaultPlayerProfile()
	two := game.DefaultPlayerProfile()
	two.PlayerID = 2
	if err := world.EnterLobby(1000001, one); err != nil {
		t.Fatal(err)
	}
	if err := world.EnterLobby(1000002, two); err != nil {
		t.Fatal(err)
	}
	first, err := world.CreateRoom(1000001, one, roomstate.MatchSettings{Map: roomstate.RandomMapSelection(), GameType: roomstate.GameTypeAdventure}, roomstate.Properties{})
	if err != nil || first.RoomID != 1 {
		t.Fatalf("first room = %+v, %v", first, err)
	}
	second, err := world.CreateRoom(1000002, two, roomstate.MatchSettings{Map: roomstate.RandomMapSelection(), GameType: roomstate.GameTypeAdventure}, roomstate.Properties{})
	if err != nil || second.RoomID != 2 {
		t.Fatalf("second room = %+v, %v", second, err)
	}
	if departure, err := world.LeaveRoom(1000001, roomstate.LeaveVoluntary); err != nil || !departure.Empty {
		t.Fatalf("first room departure = %+v, %v", departure, err)
	}
	third := game.DefaultPlayerProfile()
	third.PlayerID = 3
	if err := world.EnterLobby(1000003, third); err != nil {
		t.Fatal(err)
	}
	reused, err := world.CreateRoom(1000003, third, roomstate.MatchSettings{Map: roomstate.RandomMapSelection(), GameType: roomstate.GameTypeAdventure}, roomstate.Properties{})
	if err != nil || reused.RoomID != 1 {
		t.Fatalf("reused room = %+v, %v", reused, err)
	}
}

func TestWorldSnapshotReportsAuthoritativeTopology(t *testing.T) {
	world := NewWorld()
	one := game.DefaultPlayerProfile()
	two := game.DefaultPlayerProfile()
	two.PlayerID = 2
	if err := world.EnterLobby(1000001, one); err != nil {
		t.Fatal(err)
	}
	if err := world.EnterLobby(1000002, two); err != nil {
		t.Fatal(err)
	}
	created, err := world.CreateRoom(1000001, one, roomstate.MatchSettings{
		Map: roomstate.FixedMapSelection(1649), GameType: roomstate.GameTypeAdventure,
	}, roomstate.Properties{Name: "快照测试"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = world.JoinRoom(1000002, two, created.RoomID, two.GameInfo.RoleID, 2); err != nil {
		t.Fatal(err)
	}
	snapshot, err := world.Snapshot(one.SectionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Lobby.Players) != 2 || len(snapshot.Lobby.Rooms) != 1 || len(snapshot.Rooms) != 1 || len(snapshot.Memberships) != 2 {
		t.Fatalf("world snapshot = %+v", snapshot)
	}
	if snapshot.Rooms[0].RoomID != created.RoomID || len(snapshot.Rooms[0].Members) != 2 || snapshot.Memberships[0].UIN != 1000001 || snapshot.Memberships[1].UIN != 1000002 {
		t.Fatalf("world topology = %+v", snapshot)
	}
}
