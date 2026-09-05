package application

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

func TestProfileUpdateSerializesReadWithInventoryAndSettlement(t *testing.T) {
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const uin = 1000001
	if err := store.Save(t.Context(), uin, game.DefaultPlayerProfile()); err != nil {
		t.Fatal(err)
	}
	service, err := NewPlayerService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		_, updateErr := service.UpdateProfile(t.Context(), uin, func(profile *game.PlayerProfile) error {
			close(entered)
			<-release
			profile.Nickname = "updated nickname"
			return nil
		})
		updated <- updateErr
	}()
	<-entered
	changed := make(chan error, 1)
	go func() {
		if err := service.SetInventoryItem(t.Context(), uin, game.NewPermanentItemInfo(22, 3)); err != nil {
			changed <- err
			return
		}
		_, err := service.ApplyCompetitiveSettlements(t.Context(), []CompetitiveSettlementRequest{{UIN: uin, Settlement: CompetitiveSettlement{Result: game.GameResultWin}}})
		changed <- err
	}()
	select {
	case <-changed:
		close(release)
		<-updated
		t.Fatal("account write overtook the profile read/save interval")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	// Updating another field after settlement also must retain its fresh values.
	_, err = service.UpdateProfile(t.Context(), uin, func(profile *game.PlayerProfile) error { profile.Gender = 1; return nil })
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.Load(t.Context(), uin)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Nickname != "updated nickname" || profile.Gender != 1 || profile.GameInfo.WinNum != 1 || len(profile.Inventory) != 1 || profile.Inventory[0].NumOfItem != 3 {
		t.Fatalf("profile update lost unrelated account state: %+v", profile)
	}
	wantErr := errors.New("invalid edit")
	_, err = service.UpdateProfile(t.Context(), uin, func(profile *game.PlayerProfile) error { profile.Nickname = "must not persist"; return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("failed update = %v", err)
	}
	profile, err = store.Load(t.Context(), uin)
	if err != nil || profile.Nickname != "updated nickname" {
		t.Fatalf("failed update persisted: %+v, %v", profile, err)
	}
}
