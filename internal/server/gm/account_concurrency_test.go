package gm

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qqtang/internal/protocol/game"
	"qqtang/internal/server/application"
	"qqtang/internal/server/persistence"
)

func TestConcurrentAccountCreationDoesNotReplaceExistingAccount(t *testing.T) {
	store, err := persistence.OpenPlayerStore(filepath.Join(t.TempDir(), "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := &Server{store: store, seedProfile: func(uint32) game.PlayerProfile {
		entered <- struct{}{}
		<-release
		return game.DefaultPlayerProfile()
	}}
	// Both requests reach their seed after any optimistic existence check.
	// The persistence operation still has to choose exactly one winner.
	server.players, err = application.NewPlayerService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan int, 2)
	for range 2 {
		go func() {
			request := httptest.NewRequest(http.MethodPost, "/gm/api/accounts", strings.NewReader(`{"uin":1000001,"nickname":"new account","gender":0,"password":"abc12345"}`))
			response := httptest.NewRecorder()
			server.createAccount(response, request)
			results <- response.Code
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("creation did not reach seed")
		}
	}
	close(release)
	counts := map[int]int{}
	for range 2 {
		counts[<-results]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent account responses = %v, want one creation and one conflict", counts)
	}
}
