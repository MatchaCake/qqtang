package persistence

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"qqtang/internal/game/craftcatalog"
	"qqtang/internal/protocol/game"
)

func TestInventoryKindLimitAcrossMutationEntrances(t *testing.T) {
	ctx := context.Background()
	const uin uint32 = 1_000_001

	t.Run("purchase", func(t *testing.T) {
		store := openInventoryKindLimitStore(t, uin, game.DefaultPlayerProfile())
		defer store.Close()

		profile, err := store.Load(ctx, uin)
		if err != nil {
			t.Fatal(err)
		}
		profile.GameInfo.Money = 1_000
		if err = store.Save(ctx, uin, profile); err != nil {
			t.Fatal(err)
		}
		if _, err = store.PurchaseInventoryItem(ctx, uin, game.NewPermanentItemInfo(20_001, 1), 100, true); !errors.Is(err, ErrInventoryKindLimitReached) {
			t.Fatalf("new purchase error = %v", err)
		}
		profile, err = store.Load(ctx, uin)
		if err != nil {
			t.Fatal(err)
		}
		if profile.GameInfo.Money != 1_000 || len(profile.Inventory) != game.MaxItemInfoCount {
			t.Fatalf("rejected purchase changed profile = money:%d kinds:%d", profile.GameInfo.Money, len(profile.Inventory))
		}
		if _, err = store.PurchaseInventoryItem(ctx, uin, game.NewPermanentItemInfo(1_000, 1), 100, true); err != nil {
			t.Fatalf("existing purchase error = %v", err)
		}
		profile, err = store.Load(ctx, uin)
		if err != nil {
			t.Fatal(err)
		}
		if profile.GameInfo.Money != 900 || profile.Inventory[0].NumOfItem != 2 {
			t.Fatalf("existing purchase result = money:%d item:%+v", profile.GameInfo.Money, profile.Inventory[0])
		}
	})

	t.Run("gm", func(t *testing.T) {
		store := openInventoryKindLimitStore(t, uin, game.DefaultPlayerProfile())
		defer store.Close()

		if err := store.SetInventoryItem(ctx, uin, game.NewPermanentItemInfo(20_002, 1)); !errors.Is(err, ErrInventoryKindLimitReached) {
			t.Fatalf("new GM item error = %v", err)
		}
		if err := store.SetInventoryItem(ctx, uin, game.NewPermanentItemInfo(1_000, 2)); err != nil {
			t.Fatalf("existing GM item error = %v", err)
		}
		item, found, err := store.InventoryItem(ctx, uin, 1_000)
		if err != nil || !found || item.NumOfItem != 2 {
			t.Fatalf("existing GM item = found:%t item:%+v err:%v", found, item, err)
		}
	})

	t.Run("exchange", func(t *testing.T) {
		store := openInventoryKindLimitStore(t, uin, game.DefaultPlayerProfile(), game.NewPermanentItemInfo(20_003, 2))
		defer store.Close()
		newItem := game.NewPermanentItemInfo(20_004, 1)

		_, err := store.ExchangeInventoryItems(ctx, uin, []InventoryConsumption{{UIN: uin, ItemID: 20_003, Quantity: 1}}, []game.ItemInfo{newItem})
		if !errors.Is(err, ErrInventoryKindLimitReached) {
			t.Fatalf("exchange error = %v", err)
		}
		item, found, err := store.InventoryItem(ctx, uin, 20_003)
		if err != nil || !found || item.NumOfItem != 2 {
			t.Fatalf("rejected exchange material = found:%t item:%+v err:%v", found, item, err)
		}
		if _, found, err = store.InventoryItem(ctx, uin, newItem.ItemID); err != nil || found {
			t.Fatalf("rejected exchange reward = found:%t err:%v", found, err)
		}

		if _, err = store.ExchangeInventoryItems(ctx, uin, []InventoryConsumption{{UIN: uin, ItemID: 20_003, Quantity: 2}}, []game.ItemInfo{newItem}); err != nil {
			t.Fatalf("exchange after freeing a kind = %v", err)
		}
		profile, err := store.Load(ctx, uin)
		if err != nil || len(profile.Inventory) != game.MaxItemInfoCount {
			t.Fatalf("exchange after freeing a kind profile = kinds:%d err:%v", len(profile.Inventory), err)
		}
	})

	t.Run("combine", func(t *testing.T) {
		book := game.NewPermanentItemInfo(20_010, 1)
		book.ItemStatus = game.ItemStatusLearnedRecipe
		store := openInventoryKindLimitStore(t, uin, game.DefaultPlayerProfile(), book, game.NewPermanentItemInfo(20_011, 2))
		defer store.Close()
		recipe := craftcatalog.CombineRecipe{
			BookItemID: 20_010, ProductItemID: 20_012,
			Materials: []craftcatalog.CombineMaterial{{ItemID: 20_011, Quantity: 1}},
		}

		_, err := store.ApplyCombineRecipe(ctx, uin, recipe)
		if !errors.Is(err, ErrInventoryKindLimitReached) {
			t.Fatalf("combine error = %v", err)
		}
		item, found, err := store.InventoryItem(ctx, uin, 20_011)
		if err != nil || !found || item.NumOfItem != 2 {
			t.Fatalf("rejected combine material = found:%t item:%+v err:%v", found, item, err)
		}

		if err = store.SetInventoryItem(ctx, uin, game.NewPermanentItemInfo(20_011, 1)); err != nil {
			t.Fatal(err)
		}
		if _, err = store.ApplyCombineRecipe(ctx, uin, recipe); err != nil {
			t.Fatalf("combine after freeing a kind = %v", err)
		}
		profile, err := store.Load(ctx, uin)
		if err != nil || len(profile.Inventory) != game.MaxItemInfoCount {
			t.Fatalf("combine after freeing a kind profile = kinds:%d err:%v", len(profile.Inventory), err)
		}
		if item, found, err = store.InventoryItem(ctx, uin, recipe.ProductItemID); err != nil || !found || item.NumOfItem != 1 {
			t.Fatalf("combined product = found:%t item:%+v err:%v", found, item, err)
		}
	})
}

func openInventoryKindLimitStore(t *testing.T, uin uint32, profile game.PlayerProfile, extras ...game.ItemInfo) *PlayerStore {
	t.Helper()
	profile.Inventory = inventoryAtKindLimit(extras...)
	store, err := OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(context.Background(), uin, profile); err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store
}

func inventoryAtKindLimit(extras ...game.ItemInfo) []game.ItemInfo {
	items := append([]game.ItemInfo(nil), extras...)
	seen := make(map[uint16]struct{}, len(items))
	for _, item := range items {
		seen[item.ItemID] = struct{}{}
	}
	for itemID := uint16(1_000); len(items) < game.MaxItemInfoCount; itemID++ {
		if _, ok := seen[itemID]; ok {
			continue
		}
		seen[itemID] = struct{}{}
		items = append(items, game.NewPermanentItemInfo(itemID, 1))
	}
	return items
}
