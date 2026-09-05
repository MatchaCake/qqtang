package persistence

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"qqtang/internal/accountauth"
	"qqtang/internal/protocol/game"
)

func TestAccountCreationConflictPreservesProfileInventoryAndPassword(t *testing.T) {
	store, err := OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const uin = 1000001
	profile := game.DefaultPlayerProfile()
	profile.Nickname = "original"
	profile.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(22, 3)}
	if err := store.CreateAccount(t.Context(), uin, profile, "first123"); err != nil {
		t.Fatal(err)
	}
	replacement := game.DefaultPlayerProfile()
	replacement.Nickname = "replacement"
	if err := store.CreateAccount(t.Context(), uin, replacement, "second123"); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("duplicate create = %v", err)
	}
	if err := store.EnsureDefaultPassword(t.Context(), uin); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Load(t.Context(), uin)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Nickname != "original" || len(stored.Inventory) != 1 || stored.Inventory[0].NumOfItem != 3 {
		t.Fatalf("duplicate create replaced account: %+v", stored)
	}
	iterations, salt, err := store.PasswordParameters(t.Context(), uin)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, accountauth.NonceSize)
	proof, err := accountauth.ClientProof([]byte("first123"), salt, iterations, nonce, uin)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := store.VerifyPasswordProof(t.Context(), uin, nonce, proof)
	if err != nil || !accepted {
		t.Fatalf("original password was replaced: accepted=%v, err=%v", accepted, err)
	}
}

func TestAccountCreationRollsBackWhenCredentialsCannotBeWritten(t *testing.T) {
	store, err := OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(t.Context(), `CREATE TRIGGER fail_credentials BEFORE INSERT ON player_credentials BEGIN SELECT RAISE(ABORT, 'injected credential failure'); END`); err != nil {
		t.Fatal(err)
	}
	profile := game.DefaultPlayerProfile()
	profile.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(22, 3)}
	if err := store.CreateAccount(t.Context(), 1000001, profile, "first123"); err == nil {
		t.Fatal("creation ignored credential failure")
	}
	if _, err := store.Load(t.Context(), 1000001); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial account survived failed creation: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM player_inventory WHERE uin = 1000001`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial inventory survived: count=%d, err=%v", count, err)
	}
}
