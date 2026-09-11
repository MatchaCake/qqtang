package probe

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"qqtang/internal/clientdata/sceneelement"
	"qqtang/internal/game/craftcatalog"
	"qqtang/internal/game/equipment"
	"qqtang/internal/game/functionitem"
	"qqtang/internal/game/itemcatalog"
	"qqtang/internal/game/itemeffect"
	"qqtang/internal/game/mapdata"
	"qqtang/internal/game/petcatalog"
	"qqtang/internal/game/roledata"
	"qqtang/internal/game/shopcatalog"
	"qqtang/internal/protocol/directory"
	"qqtang/internal/server/application"
	"qqtang/internal/server/persistence"
)

// serverStaticResources contains immutable catalogs derived from the release
// configuration and original client data. Loading them is a bootstrap concern;
// the network runtime must not rediscover or mutate these resources.
type serverStaticResources struct {
	mapCatalog            *mapdata.Catalog
	adventureRules        *mapdata.AdventureRules
	roleRules             *roledata.Rules
	equipmentCatalog      *equipment.Catalog
	forgeCatalog          *craftcatalog.Catalog
	combineCatalog        *craftcatalog.CombineCatalog
	shopCatalog           *shopcatalog.Catalog
	equipmentSeeds        equipment.SeedSet
	itemEntries           []itemcatalog.Entry
	itemEffectCatalog     *itemeffect.Catalog
	breakEggCatalog       *functionitem.BreakEggCatalog
	itemKindsByID         map[uint16]string
	petCardsByItem        map[uint16]petcatalog.CardLink
	petCardItemByType     map[uint32]uint16
	petInnateSkillsByType map[uint32][]byte
	petSkillBooksByItem   map[uint16]petcatalog.SkillBookLink
	petSkillItemByID      map[byte]uint16
	petFoodsByItem        map[uint16]petcatalog.FoodLink
	petExperienceTable    []uint32
	directoryHallPayload  []byte
	gameSectionID         uint16
}

type serverPlayerRuntime struct {
	store   *persistence.PlayerStore
	service *application.PlayerService
}

func loadServerStaticResources(config Config) (serverStaticResources, error) {
	var resources serverStaticResources
	loadItems := func(reason string) error {
		if len(resources.itemEntries) != 0 {
			return nil
		}
		entries, err := itemcatalog.Load(config.ClientRoot)
		if err != nil {
			return fmt.Errorf("load client item catalog for %s: %w", reason, err)
		}
		resources.itemEntries = entries
		return nil
	}
	if config.ClientRoot != "" && config.DatabasePath != "" {
		forgePath := filepath.Join(config.ClientRoot, "config", "avatarforge.ini")
		if _, err := os.Stat(forgePath); err == nil {
			if config.ForgeRulesPath == "" {
				return resources, fmt.Errorf("forge_rules_path is required when avatar forge is available")
			}
			resources.forgeCatalog, err = craftcatalog.LoadForge(config.ClientRoot, config.ForgeRulesPath)
			if err != nil {
				return resources, fmt.Errorf("load avatar forge catalog: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return resources, fmt.Errorf("inspect avatar forge config: %w", err)
		}
	}
	if config.CombineRecipesPath != "" {
		var err error
		resources.combineCatalog, err = craftcatalog.LoadCombineJSON(config.CombineRecipesPath)
		if err != nil {
			return resources, fmt.Errorf("load combine recipe catalog: %w", err)
		}
	}
	if config.BreakEggRewardsPath != "" {
		var err error
		resources.breakEggCatalog, err = functionitem.LoadBreakEggCatalog(config.BreakEggRewardsPath)
		if err != nil {
			return resources, err
		}
	}
	if config.DirectoryHall != nil {
		native, err := config.DirectoryHall.NativeConfig()
		if err != nil {
			return resources, err
		}
		resources.directoryHallPayload, err = directory.BuildLocalHallPayload(native)
		if err != nil {
			return resources, fmt.Errorf("build configured directory hall payload: %w", err)
		}
		resources.gameSectionID = native.SectionID
	}
	if config.RoleRulesPath != "" {
		var err error
		resources.roleRules, err = roledata.LoadRules(config.RoleRulesPath)
		if err != nil {
			return resources, err
		}
	}
	if config.EquipmentSeedsPath != "" {
		if err := loadItems("equipment"); err != nil {
			return resources, err
		}
		var err error
		resources.equipmentCatalog, err = equipment.NewCatalog(resources.itemEntries)
		if err != nil {
			return resources, fmt.Errorf("build equipment catalog: %w", err)
		}
		resources.equipmentSeeds, err = equipment.LoadSeeds(config.EquipmentSeedsPath, resources.equipmentCatalog)
		if err != nil {
			return resources, err
		}
	}
	if config.GMHTTPAddress != "" && len(resources.itemEntries) == 0 {
		if err := loadItems("GM tool"); err != nil {
			return resources, err
		}
		if resources.equipmentCatalog == nil {
			var err error
			resources.equipmentCatalog, err = equipment.NewCatalog(resources.itemEntries)
			if err != nil {
				return resources, fmt.Errorf("build GM equipment catalog: %w", err)
			}
		}
	}
	if config.ClientRoot != "" && responseFamilyEnabled(config, func(response ResponseConfig) bool {
		return response.QQTLoginSuccess || response.gameBeginEnabled()
	}) {
		if err := loadItems("item-use policy"); err != nil {
			return resources, err
		}
		resources.itemKindsByID = itemUseKinds(resources.itemEntries)
		pets, err := petcatalog.Load(config.ClientRoot)
		if err != nil {
			return resources, fmt.Errorf("load pet operation catalog: %w", err)
		}
		resources.petCardsByItem, resources.petSkillBooksByItem, resources.petFoodsByItem, resources.petExperienceTable, err = petOperationResources(pets)
		if err != nil {
			return resources, err
		}
		resources.petCardItemByType = make(map[uint32]uint16, len(resources.petCardsByItem))
		resources.petInnateSkillsByType = make(map[uint32][]byte, len(resources.petCardsByItem))
		for itemID, link := range resources.petCardsByItem {
			if existing, duplicate := resources.petCardItemByType[link.PetTypeID]; duplicate && existing != itemID {
				return resources, fmt.Errorf("pet type %d maps to multiple card items %d and %d", link.PetTypeID, existing, itemID)
			}
			resources.petCardItemByType[link.PetTypeID] = itemID
			resources.petInnateSkillsByType[link.PetTypeID] = append([]byte(nil), link.InnateSkills...)
		}
		resources.petSkillItemByID = make(map[byte]uint16, len(resources.petSkillBooksByItem))
		for itemID, link := range resources.petSkillBooksByItem {
			if existing, duplicate := resources.petSkillItemByID[link.Skill.SkillID]; duplicate && existing != itemID {
				return resources, fmt.Errorf("pet skill %d maps to multiple book items %d and %d", link.Skill.SkillID, existing, itemID)
			}
			resources.petSkillItemByID[link.Skill.SkillID] = itemID
		}
	}
	if responseFamilyEnabled(config, func(response ResponseConfig) bool { return response.shopEnabled() }) {
		itemVersion, registryItems, err := itemcatalog.LoadItemRegistry(config.ClientRoot)
		if err != nil {
			return resources, fmt.Errorf("load canonical item registry for shop: %w", err)
		}
		commodityVersion, commodities, err := itemcatalog.LoadCommodityRegistry(config.ClientRoot)
		if err != nil {
			return resources, fmt.Errorf("load commodity registry for shop: %w", err)
		}
		if itemVersion != commodityVersion {
			return resources, fmt.Errorf("shop registry versions disagree: item %d, commodity %d", itemVersion, commodityVersion)
		}
		resources.shopCatalog, err = shopcatalog.NewCatalog(commodityVersion, registryItems, commodities, shopcatalog.DefaultCommodityLimit)
		if err != nil {
			return resources, fmt.Errorf("build authoritative shop catalog: %w", err)
		}
		commodityINIPath := filepath.Join(config.ClientRoot, "config", "Commodity.ini")
		if err = shopcatalog.ValidateCommodityINI(commodityINIPath, commodityVersion, registryItems, commodities, shopcatalog.DefaultCommodityLimit); err != nil {
			return resources, fmt.Errorf("validate client shop catalog: %w", err)
		}
	}
	if responseFamilyEnabled(config, func(response ResponseConfig) bool { return response.gameBeginEnabled() }) {
		var err error
		resources.mapCatalog, err = mapdata.LoadCatalog(config.ClientRoot)
		if err != nil {
			return resources, fmt.Errorf("load adventure map catalog: %w", err)
		}
		if len(resources.mapCatalog.SelectableIDs()) == 0 {
			return resources, fmt.Errorf("adventure map catalog contains no selectable first stages")
		}
		if config.AdventureRulesPath != "" {
			resources.adventureRules, err = mapdata.LoadAdventureRules(config.AdventureRulesPath)
			if err != nil {
				return resources, err
			}
			for _, itemID := range resources.adventureRules.DropItemIDs() {
				if !sceneelement.IsClientID(itemID) {
					return resources, fmt.Errorf("adventure rules drop item ID %d is not recognized by the client scene-element factory", itemID)
				}
			}
		}
		for _, mapID := range resources.mapCatalog.CompetitiveIDs() {
			entry, _ := resources.mapCatalog.CompetitiveMap(mapID)
			for _, rule := range entry.WallItemRules {
				if !sceneelement.IsClientID(rule.SceneID) {
					return resources, fmt.Errorf("competitive map %d wall scene ID %d is not recognized by the client scene-element factory", mapID, rule.SceneID)
				}
			}
		}
	}
	if len(resources.itemEntries) != 0 {
		resources.itemEffectCatalog = itemeffect.NewCatalog(itemeffect.Discover(resources.itemEntries))
	}
	if resources.breakEggCatalog != nil {
		if err := loadItems("break-egg rewards"); err != nil {
			return resources, err
		}
		known := make(map[uint16]struct{}, len(resources.itemEntries))
		for _, entry := range resources.itemEntries {
			if entry.ID > 0 && entry.ID <= math.MaxUint16 {
				known[uint16(entry.ID)] = struct{}{}
			}
		}
		for _, reward := range resources.breakEggCatalog.Rewards() {
			if _, ok := known[reward.ItemID]; !ok {
				return resources, fmt.Errorf("break-egg reward item %d is absent from the installed client catalog", reward.ItemID)
			}
		}
	}
	return resources, nil
}

func itemUseKinds(entries []itemcatalog.Entry) map[uint16]string {
	result := make(map[uint16]string)
	for _, entry := range entries {
		if entry.ID == 0 || entry.ID > math.MaxUint16 || entry.Kind == "" {
			continue
		}
		result[uint16(entry.ID)] = entry.Kind
	}
	return result
}

func petOperationResources(catalog petcatalog.Catalog) (map[uint16]petcatalog.CardLink, map[uint16]petcatalog.SkillBookLink, map[uint16]petcatalog.FoodLink, []uint32, error) {
	cards := make(map[uint16]petcatalog.CardLink, len(catalog.CardLinks))
	for _, link := range catalog.CardLinks {
		if link.ItemID == 0 || link.ItemID > math.MaxUint16 {
			return nil, nil, nil, nil, fmt.Errorf("pet card item ID %d is outside uint16", link.ItemID)
		}
		cards[uint16(link.ItemID)] = link
	}
	books := make(map[uint16]petcatalog.SkillBookLink, len(catalog.SkillBooks))
	for _, link := range catalog.SkillBooks {
		if link.ItemID == 0 || link.ItemID > math.MaxUint16 {
			return nil, nil, nil, nil, fmt.Errorf("pet skill book item ID %d is outside uint16", link.ItemID)
		}
		books[uint16(link.ItemID)] = link
	}
	foods := make(map[uint16]petcatalog.FoodLink, len(catalog.Foods))
	for _, link := range catalog.Foods {
		if link.ItemID == 0 || link.ItemID > math.MaxUint16 {
			return nil, nil, nil, nil, fmt.Errorf("pet food item ID %d is outside uint16", link.ItemID)
		}
		foods[uint16(link.ItemID)] = link
	}
	return cards, books, foods, append([]uint32(nil), catalog.ExperienceThresholds...), nil
}

func responseFamilyEnabled(config Config, enabled func(ResponseConfig) bool) bool {
	for _, listener := range config.Listeners {
		if enabled(listener.Response) {
			return true
		}
	}
	return false
}

func loadServerPlayerRuntime(config Config, resources serverStaticResources) (serverPlayerRuntime, error) {
	var runtime serverPlayerRuntime
	if config.DatabasePath == "" {
		return runtime, nil
	}
	databaseWasPresent := false
	if _, statErr := os.Stat(config.DatabasePath); statErr == nil {
		databaseWasPresent = true
	} else if !os.IsNotExist(statErr) {
		return runtime, fmt.Errorf("inspect SQLite player store: %w", statErr)
	}
	store, err := persistence.OpenPlayerStore(config.DatabasePath)
	if err != nil {
		return runtime, err
	}
	fail := func(err error) (serverPlayerRuntime, error) {
		_ = store.Close()
		return serverPlayerRuntime{}, err
	}
	ctx := context.Background()
	if !databaseWasPresent {
		for _, account := range config.InitialAccounts {
			if _, err = store.LoadOrCreate(ctx, account.UIN, config.initialAccountProfile(account)); err != nil {
				return fail(fmt.Errorf("initialize local account %d: %w", account.UIN, err))
			}
		}
	}
	for _, seed := range resources.equipmentSeeds.Seeds {
		if _, err = store.ApplyEquipmentSeed(ctx, seed); err != nil {
			return fail(fmt.Errorf("initialize equipment seed %q: %w", seed.Key, err))
		}
	}
	service, err := application.NewPlayerService(store, resources.equipmentCatalog)
	if err != nil {
		return fail(err)
	}
	runtime.store = store
	runtime.service = service
	return runtime, nil
}
