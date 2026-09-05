package persistence

import (
	"path/filepath"
	"testing"

	"qqtang/internal/protocol/game"
)

func TestAccountPlayerIDsDoNotCollideAndRemainStable(t *testing.T) {
	store, err := OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Config.playerProfileForUIN can propose the same uint16 ID for UINs
	// separated by 65535. Persistence must resolve that preference atomically.
	seed := game.DefaultPlayerProfile()
	seed.PlayerID = 2
	first, err := store.LoadOrCreate(t.Context(), 1000002, seed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.LoadOrCreate(t.Context(), 1065537, seed)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlayerID == second.PlayerID {
		t.Fatalf("distinct accounts share PlayerID %d", first.PlayerID)
	}
	// Repair an existing save produced by the earlier modulo mapping at login.
	if err := store.Save(t.Context(), 1131072, seed); err != nil {
		t.Fatal(err)
	}
	third, err := store.LoadOrCreate(t.Context(), 1131072, seed)
	if err != nil {
		t.Fatal(err)
	}
	if third.PlayerID == first.PlayerID || third.PlayerID == second.PlayerID {
		t.Fatalf("legacy collision survived login: %d, %d, %d", first.PlayerID, second.PlayerID, third.PlayerID)
	}
	for uin, want := range map[uint32]uint16{1000002: first.PlayerID, 1065537: second.PlayerID, 1131072: third.PlayerID} {
		profile, err := store.LoadOrCreate(t.Context(), uin, seed)
		if err != nil || profile.PlayerID != want {
			t.Fatalf("UIN %d changed PlayerID: got %d want %d err=%v", uin, profile.PlayerID, want, err)
		}
	}
}
