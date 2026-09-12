package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"qqtang/internal/game/craftcatalog"
	"qqtang/internal/game/itemeffect"
	"qqtang/internal/protocol/game"
)

// InventoryConsumption is one account-item debit participating in a larger
// all-or-nothing operation. Duplicate rows are combined before validation.
type InventoryConsumption struct {
	UIN      uint32
	ItemID   uint16
	Quantity uint32
}

func validateInventoryKindLimitTx(ctx context.Context, tx *sql.Tx, uin uint32, itemIDs []uint16) error {
	var kindCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM player_inventory WHERE uin = ? AND quantity > 0`, uin).Scan(&kindCount); err != nil {
		return fmt.Errorf("count inventory kinds for UIN %d: %w", uin, err)
	}
	seen := make(map[uint16]struct{}, len(itemIDs))
	for _, itemID := range itemIDs {
		if itemID == 0 {
			continue
		}
		if _, ok := seen[itemID]; ok {
			continue
		}
		seen[itemID] = struct{}{}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM player_inventory WHERE uin = ? AND item_id = ? AND quantity > 0`, uin, itemID).Scan(&active); errors.Is(err, sql.ErrNoRows) {
			kindCount++
		} else if err != nil {
			return fmt.Errorf("check inventory item %d for UIN %d: %w", itemID, uin, err)
		}
	}
	if kindCount > game.MaxItemInfoCount {
		return fmt.Errorf("UIN %d inventory would contain %d kinds: %w", uin, kindCount, ErrInventoryKindLimitReached)
	}
	return nil
}

// ExchangeInventoryItems atomically consumes account items and grants stack
// rewards. The returned rows are absolute post-transaction values for every
// touched item, including zero-quantity tombstones required by the old client.
func (store *PlayerStore) ExchangeInventoryItems(ctx context.Context, uin uint32, consumes []InventoryConsumption, grants []game.ItemInfo) ([]game.ItemInfo, error) {
	if store == nil || store.db == nil || uin == 0 {
		return nil, fmt.Errorf("inventory exchange requires a store and non-zero UIN")
	}
	consumeByID := make(map[uint16]uint64, len(consumes))
	grantByID := make(map[uint16]game.ItemInfo, len(grants))
	changedIDs := make(map[uint16]struct{}, len(consumes)+len(grants))
	for index, consume := range consumes {
		if consume.UIN != uin || consume.ItemID == 0 || consume.Quantity == 0 {
			return nil, fmt.Errorf("inventory exchange consumption %d is invalid: %+v", index, consume)
		}
		consumeByID[consume.ItemID] += uint64(consume.Quantity)
		if consumeByID[consume.ItemID] > math.MaxUint32 {
			return nil, fmt.Errorf("inventory exchange consumption for item %d exceeds uint32", consume.ItemID)
		}
		changedIDs[consume.ItemID] = struct{}{}
	}
	for index, grant := range grants {
		if grant.ItemID == 0 || grant.NumOfItem == 0 {
			return nil, fmt.Errorf("inventory exchange grant %d is invalid: %+v", index, grant)
		}
		current := grantByID[grant.ItemID]
		quantity := uint64(current.NumOfItem) + uint64(grant.NumOfItem)
		if quantity > math.MaxUint32 {
			return nil, fmt.Errorf("inventory exchange grant for item %d exceeds uint32", grant.ItemID)
		}
		if current.ItemID == 0 {
			current = grant
		}
		current.NumOfItem = uint32(quantity)
		current.BuyTime = 0
		current.AvailPeriod = game.LocalPermanentAvailablePeriod
		grantByID[grant.ItemID] = current
		changedIDs[grant.ItemID] = struct{}{}
	}
	if len(changedIDs) == 0 {
		return nil, fmt.Errorf("inventory exchange contains no changes")
	}
	ids := make([]uint16, 0, len(changedIDs))
	for itemID := range changedIDs {
		ids = append(ids, itemID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin inventory exchange for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	var playerExists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM local_players WHERE uin = ?`, uin).Scan(&playerExists); err != nil {
		return nil, fmt.Errorf("inventory exchange player UIN %d is unavailable: %w", uin, err)
	}
	zeroTemplates := make(map[uint16]game.ItemInfo, len(consumeByID))
	for _, itemID := range ids {
		required := consumeByID[itemID]
		if required == 0 {
			continue
		}
		var row game.ItemInfo
		var storedID, quantity, status, roleID, effect, color, buyTime, availablePeriod int64
		err = tx.QueryRowContext(ctx, `SELECT item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
			FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, itemID).Scan(
			&storedID, &quantity, &status, &roleID, &effect, &color, &buyTime, &availablePeriod,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("UIN %d item %d requires %d: %w", uin, itemID, required, ErrInventoryItemMissing)
		}
		if err != nil {
			return nil, fmt.Errorf("load UIN %d item %d for exchange: %w", uin, itemID, err)
		}
		if quantity < 0 || uint64(quantity) < required {
			return nil, fmt.Errorf("UIN %d item %d requires %d: %w", uin, itemID, required, ErrInventoryItemMissing)
		}
		row = game.ItemInfo{ItemID: uint16(storedID), NumOfItem: uint32(quantity), ItemStatus: byte(status), ItemRoleID: byte(roleID), ItemEffect: byte(effect), ItemColor: byte(color), BuyTime: uint32(buyTime), AvailPeriod: uint32(availablePeriod)}
		remaining := uint64(quantity) - required
		row.NumOfItem = uint32(remaining)
		zeroTemplates[itemID] = row
		if remaining == 0 {
			if _, err = tx.ExecContext(ctx, `DELETE FROM player_loadouts WHERE uin = ? AND item_id = ?`, uin, itemID); err != nil {
				return nil, fmt.Errorf("remove exhausted UIN %d item %d loadout: %w", uin, itemID, err)
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, itemID); err != nil {
				return nil, fmt.Errorf("remove exhausted UIN %d item %d: %w", uin, itemID, err)
			}
		} else if _, err = tx.ExecContext(ctx, `UPDATE player_inventory SET quantity = ? WHERE uin = ? AND item_id = ?`, remaining, uin, itemID); err != nil {
			return nil, fmt.Errorf("consume UIN %d item %d for exchange: %w", uin, itemID, err)
		}
	}
	grantIDs := make([]uint16, 0, len(grantByID))
	for itemID := range grantByID {
		grantIDs = append(grantIDs, itemID)
	}
	if err = validateInventoryKindLimitTx(ctx, tx, uin, grantIDs); err != nil {
		return nil, err
	}
	for _, itemID := range ids {
		grant, ok := grantByID[itemID]
		if !ok {
			continue
		}
		var existing uint64
		err = tx.QueryRowContext(ctx, `SELECT quantity FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, itemID).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read reward item %d for UIN %d: %w", itemID, uin, err)
		}
		quantity := existing + uint64(grant.NumOfItem)
		if quantity > math.MaxUint32 {
			return nil, fmt.Errorf("reward item %d quantity %d exceeds uint32", itemID, quantity)
		}
		grant.NumOfItem = uint32(quantity)
		if _, err = tx.ExecContext(ctx, `INSERT INTO player_inventory(
			uin, item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uin, item_id) DO UPDATE SET
			quantity = excluded.quantity,
			available_period = max(player_inventory.available_period, excluded.available_period)`,
			uin, grant.ItemID, grant.NumOfItem, grant.ItemStatus, grant.ItemRoleID,
			grant.ItemEffect, grant.ItemColor, grant.BuyTime, grant.AvailPeriod); err != nil {
			return nil, fmt.Errorf("grant reward item %d to UIN %d: %w", itemID, uin, err)
		}
	}
	changed := make([]game.ItemInfo, 0, len(ids))
	for _, itemID := range ids {
		var row game.ItemInfo
		var storedID, quantity, status, roleID, effect, color, buyTime, availablePeriod int64
		err = tx.QueryRowContext(ctx, `SELECT item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
			FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, itemID).Scan(
			&storedID, &quantity, &status, &roleID, &effect, &color, &buyTime, &availablePeriod,
		)
		if errors.Is(err, sql.ErrNoRows) {
			row = zeroTemplates[itemID]
			row.ItemID = itemID
			row.NumOfItem = 0
		} else if err != nil {
			return nil, fmt.Errorf("read exchanged item %d for UIN %d: %w", itemID, uin, err)
		} else {
			row = game.ItemInfo{ItemID: uint16(storedID), NumOfItem: uint32(quantity), ItemStatus: byte(status), ItemRoleID: byte(roleID), ItemEffect: byte(effect), ItemColor: byte(color), BuyTime: uint32(buyTime), AvailPeriod: uint32(availablePeriod)}
		}
		changed = append(changed, row)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit inventory exchange for UIN %d: %w", uin, err)
	}
	return changed, nil
}

// ConsumeInventoryItems verifies and debits every requested account/item in a
// single SQLite transaction. Multiplayer gates such as a Boss summon must
// never consume only a subset of the participating players' items.
func (store *PlayerStore) ConsumeInventoryItems(ctx context.Context, requests []InventoryConsumption) (map[uint32][]game.ItemInfo, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("player store is nil")
	}
	type itemKey struct {
		uin    uint32
		itemID uint16
	}
	aggregated := make(map[itemKey]uint64, len(requests))
	for index, request := range requests {
		if request.UIN == 0 || request.ItemID == 0 || request.Quantity == 0 {
			return nil, fmt.Errorf("inventory consumption %d is incomplete: %+v", index, request)
		}
		key := itemKey{uin: request.UIN, itemID: request.ItemID}
		aggregated[key] += uint64(request.Quantity)
		if aggregated[key] > math.MaxUint32 {
			return nil, fmt.Errorf("inventory consumption for UIN %d item %d exceeds uint32", request.UIN, request.ItemID)
		}
	}
	if len(aggregated) == 0 {
		return map[uint32][]game.ItemInfo{}, nil
	}
	keys := make([]itemKey, 0, len(aggregated))
	for key := range aggregated {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].uin != keys[j].uin {
			return keys[i].uin < keys[j].uin
		}
		return keys[i].itemID < keys[j].itemID
	})
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin inventory consumption: %w", err)
	}
	defer tx.Rollback()
	result := make(map[uint32][]game.ItemInfo)
	for _, key := range keys {
		var itemID, quantity, status, roleID, effect, color, buyTime, availablePeriod int64
		err = tx.QueryRowContext(ctx, `SELECT item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
			FROM player_inventory WHERE uin = ? AND item_id = ?`, key.uin, key.itemID).Scan(
			&itemID, &quantity, &status, &roleID, &effect, &color, &buyTime, &availablePeriod,
		)
		required := aggregated[key]
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("UIN %d item %d requires %d: %w", key.uin, key.itemID, required, ErrInventoryItemMissing)
		}
		if err != nil {
			return nil, fmt.Errorf("load UIN %d item %d for consumption: %w", key.uin, key.itemID, err)
		}
		if quantity < 0 || uint64(quantity) < required {
			return nil, fmt.Errorf("UIN %d item %d requires %d: %w", key.uin, key.itemID, required, ErrInventoryItemMissing)
		}
		remaining := uint64(quantity) - required
		if remaining == 0 {
			if _, err = tx.ExecContext(ctx, `DELETE FROM player_loadouts WHERE uin = ? AND item_id = ?`, key.uin, key.itemID); err != nil {
				return nil, fmt.Errorf("remove exhausted UIN %d item %d loadout: %w", key.uin, key.itemID, err)
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM player_inventory WHERE uin = ? AND item_id = ?`, key.uin, key.itemID); err != nil {
				return nil, fmt.Errorf("remove exhausted UIN %d item %d: %w", key.uin, key.itemID, err)
			}
		} else if _, err = tx.ExecContext(ctx, `UPDATE player_inventory SET quantity = ? WHERE uin = ? AND item_id = ?`, remaining, key.uin, key.itemID); err != nil {
			return nil, fmt.Errorf("consume UIN %d item %d: %w", key.uin, key.itemID, err)
		}
		result[key.uin] = append(result[key.uin], game.ItemInfo{
			ItemID: uint16(itemID), NumOfItem: uint32(remaining), ItemStatus: byte(status), ItemRoleID: byte(roleID),
			ItemEffect: byte(effect), ItemColor: byte(color), BuyTime: uint32(buyTime), AvailPeriod: uint32(availablePeriod),
		})
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit inventory consumption: %w", err)
	}
	return result, nil
}

func (store *PlayerStore) SetInventoryItem(ctx context.Context, uin uint32, item game.ItemInfo) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("player store is nil")
	}
	if uin == 0 || item.ItemID == 0 {
		return fmt.Errorf("inventory update requires non-zero UIN and item ID")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin inventory update for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	var playerExists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM local_players WHERE uin = ?`, uin).Scan(&playerExists); err != nil {
		return fmt.Errorf("inventory player UIN %d is unavailable: %w", uin, err)
	}
	if item.NumOfItem == 0 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM player_recipes WHERE uin = ? AND book_item_id = ?`, uin, item.ItemID); err != nil {
			return fmt.Errorf("remove learned recipe for book %d: %w", item.ItemID, err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM player_loadouts WHERE uin = ? AND item_id = ?`, uin, item.ItemID); err != nil {
			return fmt.Errorf("remove loadouts for item %d: %w", item.ItemID, err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, item.ItemID); err != nil {
			return fmt.Errorf("remove inventory item %d: %w", item.ItemID, err)
		}
		if item.ItemID == uint16(itemeffect.PetSlotExpansion10ItemID) || item.ItemID == uint16(itemeffect.PetSlotExpansion20ItemID) {
			if err = validatePlayerPetCapacityTx(ctx, tx, uin); err != nil {
				return err
			}
		}
	} else {
		item.BuyTime = 0
		item.AvailPeriod = game.LocalPermanentAvailablePeriod
		if err = validateInventoryKindLimitTx(ctx, tx, uin, []uint16{item.ItemID}); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO player_inventory(
			uin, item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uin, item_id) DO UPDATE SET
			quantity = excluded.quantity,
			item_status = excluded.item_status,
			item_role_id = excluded.item_role_id,
			item_effect = excluded.item_effect,
			item_color = excluded.item_color,
			buy_time = excluded.buy_time,
			available_period = excluded.available_period`,
			uin, item.ItemID, item.NumOfItem, item.ItemStatus, item.ItemRoleID,
			item.ItemEffect, item.ItemColor, item.BuyTime, item.AvailPeriod); err != nil {
			return fmt.Errorf("upsert inventory item %d: %w", item.ItemID, err)
		}
	}
	return tx.Commit()
}

// LearnCombineRecipe records the original client representation of a learned
// book: the owned book remains in inventory and its ItemStatus becomes the
// client's learned-recipe state.
func (store *PlayerStore) LearnCombineRecipe(ctx context.Context, uin uint32, recipe craftcatalog.CombineRecipe) (game.ItemInfo, error) {
	if store == nil || store.db == nil || uin == 0 || recipe.BookItemID == 0 || recipe.ProductItemID == 0 {
		return game.ItemInfo{}, fmt.Errorf("learn recipe requires a player, book, and product")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return game.ItemInfo{}, fmt.Errorf("begin learn recipe for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	var item game.ItemInfo
	var itemID, quantity, status, roleID, effect, color, buyTime, availablePeriod int64
	err = tx.QueryRowContext(ctx, `SELECT item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
		FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, recipe.BookItemID).Scan(
		&itemID, &quantity, &status, &roleID, &effect, &color, &buyTime, &availablePeriod,
	)
	if errors.Is(err, sql.ErrNoRows) || quantity == 0 {
		return game.ItemInfo{}, fmt.Errorf("combine book %d: %w", recipe.BookItemID, ErrInventoryItemMissing)
	}
	if err != nil {
		return game.ItemInfo{}, fmt.Errorf("load combine book %d: %w", recipe.BookItemID, err)
	}
	item = game.ItemInfo{
		ItemID: uint16(itemID), NumOfItem: uint32(quantity), ItemStatus: game.ItemStatusLearnedRecipe, ItemRoleID: byte(roleID),
		ItemEffect: byte(effect), ItemColor: byte(color), BuyTime: uint32(buyTime), AvailPeriod: uint32(availablePeriod),
	}
	if _, err = tx.ExecContext(ctx, `UPDATE player_inventory SET item_status = ? WHERE uin = ? AND item_id = ?`, game.ItemStatusLearnedRecipe, uin, recipe.BookItemID); err != nil {
		return game.ItemInfo{}, fmt.Errorf("mark combine book %d learned: %w", recipe.BookItemID, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO player_recipes(uin, book_item_id, product_item_id, learned_utc)
		VALUES(?, ?, ?, ?) ON CONFLICT(uin, product_item_id) DO UPDATE SET
		book_item_id = excluded.book_item_id, learned_utc = excluded.learned_utc`,
		uin, recipe.BookItemID, recipe.ProductItemID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return game.ItemInfo{}, fmt.Errorf("persist learned combine recipe %d: %w", recipe.ProductItemID, err)
	}
	if err = tx.Commit(); err != nil {
		return game.ItemInfo{}, fmt.Errorf("commit learned combine recipe %d: %w", recipe.ProductItemID, err)
	}
	return item, nil
}

// ReconcileLearnedRecipes maintains the normalized recipe index from the
// inventory state that the original client owns. The v110 client learns a
// scroll by sending ITEM_STATUS_CHANGE with status 4; it does not send a
// separate server-only recipe-row command. Keeping this derivation in one
// transaction also repairs saves created before the canonical path was known.
func (store *PlayerStore) ReconcileLearnedRecipes(ctx context.Context, uin uint32, catalog *craftcatalog.CombineCatalog) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("player store is nil")
	}
	if uin == 0 || catalog == nil || len(catalog.Recipes) == 0 {
		return fmt.Errorf("recipe reconciliation requires a player and combine catalog")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin learned recipe reconciliation for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM player_recipes
		WHERE uin = ? AND NOT EXISTS (
			SELECT 1 FROM player_inventory
			WHERE player_inventory.uin = player_recipes.uin
			  AND player_inventory.item_id = player_recipes.book_item_id
			  AND player_inventory.quantity > 0
			  AND player_inventory.item_status = ?
		)`, uin, game.ItemStatusLearnedRecipe); err != nil {
		return fmt.Errorf("remove stale learned recipes for UIN %d: %w", uin, err)
	}
	learnedUTC := time.Now().UTC().Format(time.RFC3339Nano)
	for _, recipe := range catalog.Recipes {
		if recipe.BookItemID == 0 || recipe.ProductItemID == 0 {
			return fmt.Errorf("combine catalog contains an incomplete recipe")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO player_recipes(uin, book_item_id, product_item_id, learned_utc)
			SELECT ?, ?, ?, ?
			WHERE EXISTS (
				SELECT 1 FROM player_inventory
				WHERE uin = ? AND item_id = ? AND quantity > 0 AND item_status = ?
			)
			ON CONFLICT(uin, product_item_id) DO UPDATE SET
				book_item_id = excluded.book_item_id`,
			uin, recipe.BookItemID, recipe.ProductItemID, learnedUTC,
			uin, recipe.BookItemID, game.ItemStatusLearnedRecipe); err != nil {
			return fmt.Errorf("index learned combine book %d for UIN %d: %w", recipe.BookItemID, uin, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit learned recipe reconciliation for UIN %d: %w", uin, err)
	}
	return nil
}

// PurchaseInventoryItem commits the money deduction and account-item grant in
// one SQLite transaction. Non-stackable products are rejected before money is
// changed when the account already owns the item.
func (store *PlayerStore) PurchaseInventoryItem(ctx context.Context, uin uint32, item game.ItemInfo, price uint32, stackable bool) (game.PlayerProfile, error) {
	if store == nil || store.db == nil {
		return game.PlayerProfile{}, fmt.Errorf("player store is nil")
	}
	if uin == 0 || item.ItemID == 0 || item.NumOfItem == 0 {
		return game.PlayerProfile{}, fmt.Errorf("purchase requires non-zero UIN, item ID, and quantity")
	}
	item.BuyTime = 0
	item.AvailPeriod = game.LocalPermanentAvailablePeriod
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return game.PlayerProfile{}, fmt.Errorf("begin purchase for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()

	var encoded string
	if err = tx.QueryRowContext(ctx, `SELECT profile_json FROM local_players WHERE uin = ?`, uin).Scan(&encoded); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("load purchase profile for UIN %d: %w", uin, err)
	}
	var profile game.PlayerProfile
	if err = json.Unmarshal([]byte(encoded), &profile); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("decode purchase profile for UIN %d: %w", uin, err)
	}

	var existing uint64
	err = tx.QueryRowContext(ctx, `SELECT quantity FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, item.ItemID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return game.PlayerProfile{}, fmt.Errorf("read purchase item %d for UIN %d: %w", item.ItemID, uin, err)
	}
	if err == nil && existing > 0 && !stackable {
		return game.PlayerProfile{}, fmt.Errorf("item %d: %w", item.ItemID, ErrInventoryItemOwned)
	}
	quantity := uint64(item.NumOfItem)
	if err == nil {
		quantity += existing
	}
	if quantity > uint64(^uint32(0)) {
		return game.PlayerProfile{}, fmt.Errorf("purchase item %d quantity %d exceeds uint32", item.ItemID, quantity)
	}
	if profile.GameInfo.Money < price {
		return game.PlayerProfile{}, fmt.Errorf("need %d, have %d: %w", price, profile.GameInfo.Money, ErrInsufficientGameMoney)
	}
	if err = validateInventoryKindLimitTx(ctx, tx, uin, []uint16{item.ItemID}); err != nil {
		return game.PlayerProfile{}, err
	}
	profile.GameInfo.Money -= price
	profileRecord := profile
	profileRecord.Inventory = nil
	updated, err := json.Marshal(profileRecord)
	if err != nil {
		return game.PlayerProfile{}, fmt.Errorf("encode purchase profile for UIN %d: %w", uin, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE local_players SET profile_json = ?, updated_utc = ? WHERE uin = ?`,
		string(updated), time.Now().UTC().Format(time.RFC3339Nano), uin); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("deduct purchase money for UIN %d: %w", uin, err)
	}
	item.NumOfItem = uint32(quantity)
	if _, err = tx.ExecContext(ctx, `INSERT INTO player_inventory(
		uin, item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(uin, item_id) DO UPDATE SET
		quantity = excluded.quantity,
		buy_time = excluded.buy_time,
		available_period = max(player_inventory.available_period, excluded.available_period)`,
		uin, item.ItemID, item.NumOfItem, item.ItemStatus, item.ItemRoleID,
		item.ItemEffect, item.ItemColor, item.BuyTime, item.AvailPeriod); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("grant purchased item %d to UIN %d: %w", item.ItemID, uin, err)
	}
	if err = tx.Commit(); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("commit purchase for UIN %d: %w", uin, err)
	}
	profile, err = store.Load(ctx, uin)
	if err != nil {
		return game.PlayerProfile{}, fmt.Errorf("reload purchased profile for UIN %d: %w", uin, err)
	}
	return profile, nil
}

// InventoryItem returns one item from a stable loaded account snapshot.
func (store *PlayerStore) InventoryItem(ctx context.Context, uin uint32, itemID uint16) (game.ItemInfo, bool, error) {
	profile, err := store.Load(ctx, uin)
	if err != nil {
		return game.ItemInfo{}, false, err
	}
	index := sort.Search(len(profile.Inventory), func(index int) bool { return profile.Inventory[index].ItemID >= itemID })
	if index >= len(profile.Inventory) || profile.Inventory[index].ItemID != itemID {
		return game.ItemInfo{}, false, nil
	}
	return profile.Inventory[index], true, nil
}

// ApplyAvatarForge consumes the requested material and updates both durable
// forge axes in one transaction. A crash can therefore never leave a crystal
// consumed without the color/effect result (or vice versa).
func (store *PlayerStore) ApplyAvatarForge(ctx context.Context, uin uint32, plan craftcatalog.ForgePlan) (game.ItemInfo, uint32, error) {
	if store == nil || store.db == nil || uin == 0 || plan.TargetItemID == 0 || plan.MaterialItemID == 0 || plan.MaterialQuantity == 0 {
		return game.ItemInfo{}, 0, fmt.Errorf("avatar forge requires a player, target, material, and material quantity")
	}
	if plan.TargetItemID == plan.MaterialItemID {
		return game.ItemInfo{}, 0, fmt.Errorf("avatar forge target and material cannot be the same item")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return game.ItemInfo{}, 0, fmt.Errorf("begin avatar forge for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()

	var target game.ItemInfo
	var targetID, quantity, status, roleID, effect, color, buyTime, availablePeriod int64
	err = tx.QueryRowContext(ctx, `SELECT item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
		FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, plan.TargetItemID).Scan(
		&targetID, &quantity, &status, &roleID, &effect, &color, &buyTime, &availablePeriod,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return game.ItemInfo{}, 0, fmt.Errorf("avatar forge target %d: %w", plan.TargetItemID, ErrInventoryItemMissing)
	}
	if err != nil {
		return game.ItemInfo{}, 0, fmt.Errorf("load avatar forge target %d: %w", plan.TargetItemID, err)
	}
	target = game.ItemInfo{
		ItemID: uint16(targetID), NumOfItem: uint32(quantity), ItemStatus: byte(status), ItemRoleID: byte(roleID),
		ItemEffect: byte(effect), ItemColor: byte(color), BuyTime: uint32(buyTime), AvailPeriod: uint32(availablePeriod),
	}
	if !target.Active() {
		return game.ItemInfo{}, 0, fmt.Errorf("avatar forge target %d is inactive: %w", plan.TargetItemID, ErrInventoryItemMissing)
	}
	if target.ItemEffect != plan.PreviousEffect || target.ItemColor != plan.PreviousColor {
		return target, 0, fmt.Errorf(
			"avatar forge target %d changed concurrently: effect/color is %d/%d, expected %d/%d",
			plan.TargetItemID, target.ItemEffect, target.ItemColor, plan.PreviousEffect, plan.PreviousColor,
		)
	}

	var materialQuantity uint64
	err = tx.QueryRowContext(ctx, `SELECT quantity FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, plan.MaterialItemID).Scan(&materialQuantity)
	if errors.Is(err, sql.ErrNoRows) || materialQuantity < uint64(plan.MaterialQuantity) {
		return game.ItemInfo{}, 0, fmt.Errorf("avatar forge material %d requires %d: %w", plan.MaterialItemID, plan.MaterialQuantity, ErrInventoryItemMissing)
	}
	if err != nil {
		return game.ItemInfo{}, 0, fmt.Errorf("load avatar forge material %d: %w", plan.MaterialItemID, err)
	}

	target.ItemEffect = plan.Effect
	target.ItemColor = plan.Color
	if _, err = tx.ExecContext(ctx, `UPDATE player_inventory SET item_effect = ?, item_color = ? WHERE uin = ? AND item_id = ?`,
		target.ItemEffect, target.ItemColor, uin, target.ItemID); err != nil {
		return game.ItemInfo{}, 0, fmt.Errorf("update avatar forge target %d: %w", target.ItemID, err)
	}
	remaining := materialQuantity - uint64(plan.MaterialQuantity)
	if remaining == 0 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM player_loadouts WHERE uin = ? AND item_id = ?`, uin, plan.MaterialItemID); err != nil {
			return game.ItemInfo{}, 0, fmt.Errorf("remove exhausted forge material loadout %d: %w", plan.MaterialItemID, err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, plan.MaterialItemID); err != nil {
			return game.ItemInfo{}, 0, fmt.Errorf("remove exhausted forge material %d: %w", plan.MaterialItemID, err)
		}
	} else if _, err = tx.ExecContext(ctx, `UPDATE player_inventory SET quantity = ? WHERE uin = ? AND item_id = ?`, remaining, uin, plan.MaterialItemID); err != nil {
		return game.ItemInfo{}, 0, fmt.Errorf("consume avatar forge material %d: %w", plan.MaterialItemID, err)
	}
	if err = tx.Commit(); err != nil {
		return game.ItemInfo{}, 0, fmt.Errorf("commit avatar forge for UIN %d: %w", uin, err)
	}
	return target, uint32(remaining), nil
}

// ApplyCombineRecipe performs the complete recipe transaction. Both the
// server's learned-recipe index and the client's canonical book status are
// checked so stale or constructed requests cannot bypass book use.
func (store *PlayerStore) ApplyCombineRecipe(ctx context.Context, uin uint32, recipe craftcatalog.CombineRecipe) ([]game.ItemInfo, error) {
	if store == nil || store.db == nil || uin == 0 || recipe.ProductItemID == 0 || recipe.BookItemID == 0 || len(recipe.Materials) == 0 {
		return nil, fmt.Errorf("combine requires a player and complete recipe")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin combine for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	var bookQuantity uint64
	var bookStatus uint64
	bookErr := tx.QueryRowContext(ctx, `SELECT quantity, item_status FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, recipe.BookItemID).Scan(&bookQuantity, &bookStatus)
	if bookErr != nil || bookQuantity == 0 || bookStatus != uint64(game.ItemStatusLearnedRecipe) {
		return nil, fmt.Errorf("combine recipe %d requires learned book %d: %w", recipe.ProductItemID, recipe.BookItemID, ErrRecipeNotLearned)
	}
	var learned int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM player_recipes WHERE uin = ? AND product_item_id = ?`, uin, recipe.ProductItemID).Scan(&learned)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO player_recipes(uin, book_item_id, product_item_id, learned_utc)
			VALUES(?, ?, ?, ?) ON CONFLICT(uin, product_item_id) DO NOTHING`,
			uin, recipe.BookItemID, recipe.ProductItemID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return nil, fmt.Errorf("record learned combine recipe %d: %w", recipe.ProductItemID, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("load learned combine recipe %d: %w", recipe.ProductItemID, err)
	}

	changes := make([]game.ItemInfo, 0, len(recipe.Materials)+1)
	for _, material := range recipe.Materials {
		var quantity uint64
		if err = tx.QueryRowContext(ctx, `SELECT quantity FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, material.ItemID).Scan(&quantity); err != nil {
			return nil, fmt.Errorf("load combine material %d: %w", material.ItemID, ErrInventoryItemMissing)
		}
		if quantity < uint64(material.Quantity) {
			return nil, fmt.Errorf("combine material %d requires %d, has %d: %w", material.ItemID, material.Quantity, quantity, ErrInventoryItemMissing)
		}
		remaining := quantity - uint64(material.Quantity)
		item := game.NewPermanentItemInfo(material.ItemID, uint32(remaining))
		if remaining == 0 {
			if _, err = tx.ExecContext(ctx, `DELETE FROM player_loadouts WHERE uin = ? AND item_id = ?`, uin, material.ItemID); err != nil {
				return nil, fmt.Errorf("remove exhausted combine material loadout %d: %w", material.ItemID, err)
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, material.ItemID); err != nil {
				return nil, fmt.Errorf("remove exhausted combine material %d: %w", material.ItemID, err)
			}
		} else if _, err = tx.ExecContext(ctx, `UPDATE player_inventory SET quantity = ? WHERE uin = ? AND item_id = ?`, remaining, uin, material.ItemID); err != nil {
			return nil, fmt.Errorf("consume combine material %d: %w", material.ItemID, err)
		}
		changes = append(changes, item)
	}
	if err = validateInventoryKindLimitTx(ctx, tx, uin, []uint16{recipe.ProductItemID}); err != nil {
		return nil, err
	}

	var productQuantity uint64
	err = tx.QueryRowContext(ctx, `SELECT quantity FROM player_inventory WHERE uin = ? AND item_id = ?`, uin, recipe.ProductItemID).Scan(&productQuantity)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load combine product %d: %w", recipe.ProductItemID, err)
	}
	productQuantity++
	if productQuantity > math.MaxUint32 {
		return nil, fmt.Errorf("combine product %d quantity exceeds uint32", recipe.ProductItemID)
	}
	product := game.NewPermanentItemInfo(recipe.ProductItemID, uint32(productQuantity))
	if _, err = tx.ExecContext(ctx, `INSERT INTO player_inventory(
		uin, item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
	) VALUES(?, ?, ?, 0, 0, 0, 0, ?, ?)
	ON CONFLICT(uin, item_id) DO UPDATE SET quantity = excluded.quantity`,
		uin, product.ItemID, product.NumOfItem, uint32(time.Now().Unix()), product.AvailPeriod); err != nil {
		return nil, fmt.Errorf("grant combine product %d: %w", product.ItemID, err)
	}
	changes = append(changes, product)
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit combine for UIN %d: %w", uin, err)
	}
	return changes, nil
}

func (store *PlayerStore) loadInventory(ctx context.Context, uin uint32) ([]game.ItemInfo, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT item_id, quantity, item_status, item_role_id, item_effect, item_color, buy_time, available_period
		FROM player_inventory WHERE uin = ? ORDER BY item_id`, uin)
	if err != nil {
		return nil, fmt.Errorf("load inventory for UIN %d: %w", uin, err)
	}
	defer rows.Close()
	items := make([]game.ItemInfo, 0)
	for rows.Next() {
		var itemID, quantity, status, roleID, effect, color, buyTime, availablePeriod int64
		if err := rows.Scan(&itemID, &quantity, &status, &roleID, &effect, &color, &buyTime, &availablePeriod); err != nil {
			return nil, fmt.Errorf("scan inventory for UIN %d: %w", uin, err)
		}
		items = append(items, game.ItemInfo{
			ItemID: uint16(itemID), NumOfItem: uint32(quantity), ItemStatus: byte(status), ItemRoleID: byte(roleID),
			ItemEffect: byte(effect), ItemColor: byte(color), BuyTime: uint32(buyTime), AvailPeriod: uint32(availablePeriod),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate inventory for UIN %d: %w", uin, err)
	}
	return items, nil
}
