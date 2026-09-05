package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"qqtang/internal/accountauth"
	"qqtang/internal/protocol/game"
)

var (
	ErrAccountExists               = errors.New("player account already exists")
	ErrInsufficientGameMoney       = errors.New("insufficient game money")
	ErrInventoryItemOwned          = errors.New("non-stackable inventory item is already owned")
	ErrInventoryItemMissing        = errors.New("inventory item is missing or exhausted")
	ErrPetAlreadyOwned             = errors.New("pet type is already owned")
	ErrPetCapacityReached          = errors.New("pet capacity is reached")
	ErrPetCapacityReduction        = errors.New("pet capacity cannot be reduced below the owned pet count")
	ErrPetNotFound                 = errors.New("pet is not owned by the player")
	ErrPetSkillKnown               = errors.New("pet already knows this skill")
	ErrPetSkillLevelOccupied       = errors.New("pet already knows a skill of this level")
	ErrRecipeNotLearned            = errors.New("combine recipe is not learned")
	ErrKinAlreadyMember            = errors.New("player already belongs to a kin")
	ErrKinNotMember                = errors.New("player does not belong to this kin")
	ErrKinNotFound                 = errors.New("kin is not found")
	ErrKinNameOwned                = errors.New("kin name is already in use")
	ErrKinPermissionDenied         = errors.New("kin permission denied")
	ErrKinApplicationMissing       = errors.New("kin application is missing")
	ErrKinInvitationMissing        = errors.New("kin invitation is missing")
	ErrKinMemberLimitReached       = errors.New("kin member limit is reached")
	ErrKinCreationItemMissing      = errors.New("kin creation qualification item is missing")
	ErrMarriageNotFound            = errors.New("marriage is not found")
	ErrMarriageAlreadyExists       = errors.New("player is already married")
	ErrMarriageProposalBusy        = errors.New("player already has a pending marriage proposal")
	ErrMarriageProposalMissing     = errors.New("marriage proposal is missing")
	ErrMarriageProposalItemMissing = errors.New("marriage proposal qualification item is missing")
	ErrFriendNotFound              = errors.New("friend relation is not found")
	ErrFriendAlreadyExists         = errors.New("friend relation already exists")
	ErrFriendRequestMissing        = errors.New("friend request is missing")
	ErrFriendLimitReached          = errors.New("friend limit is reached")
)

type PlayerStore struct {
	db *sql.DB
}

func OpenPlayerStore(path string) (*PlayerStore, error) {
	if path == "" {
		return nil, fmt.Errorf("SQLite database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create SQLite directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite player store: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &PlayerStore{db: db}
	if err := store.normalize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// LoadOrCreate keeps JSON as the editable seed format while making SQLite the
// runtime source of truth after the first login for a local UIN.
func (store *PlayerStore) LoadOrCreate(ctx context.Context, uin uint32, seed game.PlayerProfile) (game.PlayerProfile, error) {
	profile, err := store.Load(ctx, uin)
	if err == nil {
		profile.PlayerID, err = store.ensureUniquePlayerID(ctx, uin)
		if err != nil {
			return game.PlayerProfile{}, err
		}
		if err := store.EnsureDefaultPassword(ctx, uin); err != nil {
			return game.PlayerProfile{}, err
		}
		return profile, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return game.PlayerProfile{}, err
	}
	if err := seed.Validate(); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("validate seed profile: %w", err)
	}
	if err := store.CreateAccount(ctx, uin, seed, accountauth.DefaultPassword); err != nil {
		if errors.Is(err, ErrAccountExists) {
			return store.Load(ctx, uin)
		}
		return game.PlayerProfile{}, err
	}
	return store.Load(ctx, uin)
}

// CreateAccount inserts profile, inventory and credentials together. The
// unique UIN is claimed before saveProfileTx can update any existing rows.
func (store *PlayerStore) CreateAccount(ctx context.Context, uin uint32, profile game.PlayerProfile, password string) error {
	if uin == 0 {
		return fmt.Errorf("player UIN must be non-zero")
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	verifier, err := accountauth.NewVerifier(password)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin account creation for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `INSERT INTO local_players(uin, profile_json, created_utc, updated_utc)
		VALUES(?, '{}', ?, ?) ON CONFLICT(uin) DO NOTHING`, uin, now, now)
	if err != nil {
		return fmt.Errorf("create account UIN %d: %w", uin, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		return ErrAccountExists
	}
	profile.PlayerID, err = availablePlayerIDTx(ctx, tx, uin, profile.PlayerID)
	if err != nil {
		return err
	}
	if err := saveProfileTx(ctx, tx, uin, profile); err != nil {
		return err
	}
	if err := insertCredential(ctx, tx, uin, verifier, false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account creation for UIN %d: %w", uin, err)
	}
	return nil
}

func (store *PlayerStore) Load(ctx context.Context, uin uint32) (game.PlayerProfile, error) {
	if uin == 0 {
		return game.PlayerProfile{}, fmt.Errorf("player UIN must be non-zero")
	}
	var encoded string
	if err := store.db.QueryRowContext(ctx, `SELECT profile_json FROM local_players WHERE uin = ?`, uin).Scan(&encoded); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("load profile for UIN %d: %w", uin, err)
	}
	var profile game.PlayerProfile
	if err := json.Unmarshal([]byte(encoded), &profile); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("decode persisted profile for UIN %d: %w", uin, err)
	}
	// Character selection belongs to the authenticated client session. Ignore
	// legacy profile_json values that persisted it before this boundary was
	// made explicit; REQUEST_LOGIN supplies the current value.
	profile.GameInfo.RoleID = 0
	if err := store.projectKinMembership(ctx, uin, &profile); err != nil {
		return game.PlayerProfile{}, err
	}
	if err := store.projectMarriage(ctx, uin, &profile); err != nil {
		return game.PlayerProfile{}, err
	}
	items, err := store.loadInventory(ctx, uin)
	if err != nil {
		return game.PlayerProfile{}, err
	}
	profile.Inventory = items
	if err := profile.Validate(); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("validate persisted profile for UIN %d: %w", uin, err)
	}
	return profile, nil
}

// ListUINs returns every locally managed account in stable numeric order.
// Profiles are loaded separately so callers choose how much detail to expose.
func (store *PlayerStore) ListUINs(ctx context.Context) ([]uint32, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("player store is nil")
	}
	rows, err := store.db.QueryContext(ctx, `SELECT uin FROM local_players ORDER BY uin`)
	if err != nil {
		return nil, fmt.Errorf("list player UINs: %w", err)
	}
	defer rows.Close()
	var result []uint32
	for rows.Next() {
		var uin uint32
		if err := rows.Scan(&uin); err != nil {
			return nil, fmt.Errorf("scan player UIN: %w", err)
		}
		result = append(result, uin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate player UINs: %w", err)
	}
	return result, nil
}

// Delete removes one local account and all inventory/loadout rows through the
// schema's cascading foreign keys. It never touches editable JSON seeds.
func (store *PlayerStore) Delete(ctx context.Context, uin uint32) (bool, error) {
	if store == nil || store.db == nil {
		return false, fmt.Errorf("player store is nil")
	}
	if uin == 0 {
		return false, fmt.Errorf("player UIN must be non-zero")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin delete player UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	var ownedKin uint32
	if queryErr := tx.QueryRowContext(ctx, `SELECT kin_index FROM kin_families WHERE owner_uin = ?`, uin).Scan(&ownedKin); queryErr == nil {
		rows, listErr := tx.QueryContext(ctx, `SELECT uin FROM kin_members WHERE kin_index = ? AND uin <> ?`, ownedKin, uin)
		if listErr != nil {
			return false, fmt.Errorf("list kin %d members before deleting owner: %w", ownedKin, listErr)
		}
		var members []uint32
		for rows.Next() {
			var memberUIN uint32
			if scanErr := rows.Scan(&memberUIN); scanErr != nil {
				rows.Close()
				return false, fmt.Errorf("scan kin %d member before deleting owner: %w", ownedKin, scanErr)
			}
			members = append(members, memberUIN)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			rows.Close()
			return false, fmt.Errorf("iterate kin %d members before deleting owner: %w", ownedKin, rowsErr)
		}
		rows.Close()
		for _, memberUIN := range members {
			if updateErr := updateProfileKinTx(ctx, tx, memberUIN, 0, "", game.KinFlagID{}); updateErr != nil {
				return false, updateErr
			}
		}
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM kin_families WHERE kin_index = ?`, ownedKin); deleteErr != nil {
			return false, fmt.Errorf("dismiss kin %d before deleting owner: %w", ownedKin, deleteErr)
		}
	} else if !errors.Is(queryErr, sql.ErrNoRows) {
		return false, fmt.Errorf("check owned kin for UIN %d: %w", uin, queryErr)
	}
	var memberKin uint32
	memberQueryErr := tx.QueryRowContext(ctx, `SELECT kin_index FROM kin_members WHERE uin = ?`, uin).Scan(&memberKin)
	result, err := tx.ExecContext(ctx, `DELETE FROM local_players WHERE uin = ?`, uin)
	if err != nil {
		return false, fmt.Errorf("delete player UIN %d: %w", uin, err)
	}
	if memberQueryErr == nil && memberKin != ownedKin {
		if _, err = tx.ExecContext(ctx, `UPDATE kin_families SET list_update = list_update + 1, updated_utc = ? WHERE kin_index = ?`, time.Now().UTC().Format(time.RFC3339Nano), memberKin); err != nil {
			return false, fmt.Errorf("advance kin %d member list after deleting UIN %d: %w", memberKin, uin, err)
		}
	} else if memberQueryErr != nil && !errors.Is(memberQueryErr, sql.ErrNoRows) {
		return false, fmt.Errorf("check kin membership for UIN %d: %w", uin, memberQueryErr)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read deleted player count for UIN %d: %w", uin, err)
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("commit delete player UIN %d: %w", uin, err)
	}
	return count != 0, nil
}

func readProfileRecordTx(ctx context.Context, tx *sql.Tx, uin uint32) (game.PlayerProfile, error) {
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT profile_json FROM local_players WHERE uin = ?`, uin).Scan(&encoded); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("load profile for UIN %d: %w", uin, err)
	}
	var profile game.PlayerProfile
	if err := json.Unmarshal([]byte(encoded), &profile); err != nil {
		return game.PlayerProfile{}, fmt.Errorf("decode profile for UIN %d: %w", uin, err)
	}
	profile.GameInfo.RoleID = 0
	return profile, nil
}

func writeProfileRecordTx(ctx context.Context, tx *sql.Tx, uin uint32, profile game.PlayerProfile) error {
	profile.Inventory = nil
	profile.GameInfo.RoleID = 0
	encoded, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("encode profile for UIN %d: %w", uin, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE local_players SET profile_json = ?, updated_utc = ? WHERE uin = ?`,
		string(encoded), time.Now().UTC().Format(time.RFC3339Nano), uin); err != nil {
		return fmt.Errorf("persist profile for UIN %d: %w", uin, err)
	}
	return nil
}

func (store *PlayerStore) Save(ctx context.Context, uin uint32, profile game.PlayerProfile) error {
	if uin == 0 {
		return fmt.Errorf("player UIN must be non-zero")
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	if err = saveProfileTx(ctx, tx, uin, profile); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit save for UIN %d: %w", uin, err)
	}
	return nil
}

// SaveProfiles persists a room-sized profile set in one SQLite transaction.
// This is intentionally the only multi-account profile write primitive: match
// settlement must never leave earlier participants committed when a later
// participant fails validation or persistence.
func (store *PlayerStore) SaveProfiles(ctx context.Context, profiles map[uint32]game.PlayerProfile) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("player store is nil")
	}
	if len(profiles) == 0 {
		return fmt.Errorf("profile batch is empty")
	}
	uins := make([]uint32, 0, len(profiles))
	for uin, profile := range profiles {
		if uin == 0 {
			return fmt.Errorf("profile batch contains zero UIN")
		}
		if err := profile.Validate(); err != nil {
			return fmt.Errorf("validate profile batch UIN %d: %w", uin, err)
		}
		uins = append(uins, uin)
	}
	sort.Slice(uins, func(i, j int) bool { return uins[i] < uins[j] })
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin profile batch save: %w", err)
	}
	defer tx.Rollback()
	for _, uin := range uins {
		if err = saveProfileTx(ctx, tx, uin, profiles[uin]); err != nil {
			return fmt.Errorf("save profile batch UIN %d: %w", uin, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit profile batch save: %w", err)
	}
	return nil
}

func saveProfileTx(ctx context.Context, tx *sql.Tx, uin uint32, profile game.PlayerProfile) error {
	profileRecord := profile
	profileRecord.Inventory = nil
	profileRecord.GameInfo.RoleID = 0
	encoded, err := json.Marshal(profileRecord)
	if err != nil {
		return fmt.Errorf("encode profile for UIN %d: %w", uin, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO local_players(uin, profile_json, created_utc, updated_utc)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(uin) DO UPDATE SET profile_json = excluded.profile_json, updated_utc = excluded.updated_utc`,
		uin, string(encoded), now, now); err != nil {
		return fmt.Errorf("save profile for UIN %d: %w", uin, err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM player_inventory WHERE uin = ?`, uin); err != nil {
		return fmt.Errorf("replace inventory for UIN %d: %w", uin, err)
	}
	for _, item := range profile.ToLocalLoginConfig().Items {
		if err := insertInventoryItem(ctx, tx, uin, item, false); err != nil {
			return fmt.Errorf("save inventory for UIN %d: %w", uin, err)
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM player_recipes
		WHERE uin = ? AND NOT EXISTS (
			SELECT 1 FROM player_inventory
			WHERE player_inventory.uin = player_recipes.uin
			  AND player_inventory.item_id = player_recipes.book_item_id
			  AND player_inventory.quantity > 0
			  AND player_inventory.item_status = ?
		)`, uin, game.ItemStatusLearnedRecipe); err != nil {
		return fmt.Errorf("reconcile learned recipes for UIN %d: %w", uin, err)
	}
	if err = validatePlayerPetCapacityTx(ctx, tx, uin); err != nil {
		return err
	}
	return nil
}

func (store *PlayerStore) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	var busy, logFrames, checkpointedFrames int
	checkpointErr := store.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointedFrames)
	if checkpointErr == nil && busy != 0 {
		checkpointErr = fmt.Errorf("SQLite WAL checkpoint remained busy (%d of %d frames checkpointed)", checkpointedFrames, logFrames)
	}
	closeErr := store.db.Close()
	if checkpointErr != nil {
		return checkpointErr
	}
	return closeErr
}
