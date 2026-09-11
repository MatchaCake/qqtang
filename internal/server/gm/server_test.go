package gm

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"qqtang/internal/game/equipment"
	"qqtang/internal/game/itemcatalog"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/persistence"
)

func TestGMAccountInventorySearchAndNativeIcon(t *testing.T) {
	root := t.TempDir()
	store, err := persistence.OpenPlayerStore(filepath.Join(root, "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries := []itemcatalog.Entry{
		{ID: 22, Index: 4, RegistryCategory: "cap", Name: "印第安族长帽", Kind: "avatar-cosmetic", Categories: []string{"cap"}},
		{ID: 31, Index: 3, RegistryCategory: "frame", Name: "宝石边框", Kind: "profile-decoration", Categories: []string{"frame"}, ClientSceneFactorySupported: true},
	}
	equipmentCatalog, err := equipment.NewCatalog(entries)
	if err != nil {
		t.Fatal(err)
	}
	iconDir := filepath.Join(root, "res", "uiRes", "icon", "item")
	if err := os.MkdirAll(iconDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iconDir, "item22.img"), testDIMG(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iconDir, "item31.img"), testDIMG(), 0o600); err != nil {
		t.Fatal(err)
	}
	categoryDir := filepath.Join(root, "res", "uiRes", "icon", "cap")
	if err := os.MkdirAll(categoryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(categoryDir, "cap4.img"), testDIMG(), 0o600); err != nil {
		t.Fatal(err)
	}
	appearanceDir := filepath.Join(root, "object", "cap")
	if err := os.MkdirAll(appearanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appearanceDir, "cap4_stand.img"), testAnimatedDIMG(), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"DirCfg.ini": "DirIP=127.0.0.1\r\n", "p2psvrInfo.ini": "tcpip=127.0.0.1\r\nudpip=127.0.0.1\r\nstunip=127.0.0.1\r\n",
		"caserver.ini": "ip=127.0.0.1\r\nip2=127.0.0.1\r\n", "webserver.ini": "ip=127.0.0.1\r\nip2=127.0.0.1\r\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server, err := New(store, entries, equipmentCatalog, root, func(uin uint32) game.PlayerProfile {
		profile := game.DefaultPlayerProfile()
		profile.PlayerID = uint16(uin)
		profile.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(99, 500)}
		return profile
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	progression, err := httpServer.Client().Get(httpServer.URL + "/gm/api/progression")
	if err != nil {
		t.Fatal(err)
	}
	var progressionData struct {
		Identities       []identityOption       `json:"identities"`
		CompetitiveRanks []game.CompetitiveRank `json:"competitive_ranks"`
		AdventureRanks   []game.AdventureRank   `json:"adventure_ranks"`
	}
	if err := json.NewDecoder(progression.Body).Decode(&progressionData); err != nil {
		t.Fatal(err)
	}
	progression.Body.Close()
	if len(progressionData.Identities) != 2 || progressionData.Identities[1].Value != uint32(game.IdentityPurpleDiamond) ||
		len(progressionData.CompetitiveRanks) != game.MaxPlayerLevel || len(progressionData.AdventureRanks) != game.MaxAdventureLevel+1 {
		t.Fatalf("progression metadata = identities %v, ranks %d/%d", progressionData.Identities, len(progressionData.CompetitiveRanks), len(progressionData.AdventureRanks))
	}

	missingNickname := requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/gm/api/accounts", `{"uin":3,"nickname":"  ","gender":1,"password":"123456"}`)
	if missingNickname.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty nickname status = %d: %s", missingNickname.StatusCode, readBody(t, missingNickname))
	}
	missingNickname.Body.Close()
	longNickname := requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/gm/api/accounts", `{"uin":3,"nickname":"一二三四五六七八九十","gender":1,"password":"123456"}`)
	if longNickname.StatusCode != http.StatusBadRequest {
		t.Fatalf("long nickname status = %d: %s", longNickname.StatusCode, readBody(t, longNickname))
	}
	if body := readBody(t, longNickname); !strings.Contains(body, "16 个 GBK 字节") {
		t.Fatalf("missing nickname length explanation: %s", body)
	}
	created := requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/gm/api/accounts", `{"uin":3,"nickname":"糖三","gender":1,"password":"123456"}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %s", created.StatusCode, readBody(t, created))
	}
	created.Body.Close()
	passwordReset := requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/gm/api/accounts/3/password", `{"password":"newpass8"}`)
	if passwordReset.StatusCode != http.StatusOK {
		t.Fatalf("password reset status = %d: %s", passwordReset.StatusCode, readBody(t, passwordReset))
	}
	var passwordDetail accountDetail
	if err := json.NewDecoder(passwordReset.Body).Decode(&passwordDetail); err != nil {
		t.Fatal(err)
	}
	passwordReset.Body.Close()
	if !passwordDetail.PasswordConfigured {
		t.Fatal("created account did not report a configured password")
	}
	badRank := requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/gm/api/accounts/3", `{"degree":179,"points":887625000}`)
	if badRank.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-rank points status = %d: %s", badRank.StatusCode, readBody(t, badRank))
	}
	badRank.Body.Close()
	derivedRank := requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/gm/api/accounts/3", `{"points":887625000}`)
	if derivedRank.StatusCode != http.StatusOK {
		t.Fatalf("derived-rank status = %d: %s", derivedRank.StatusCode, readBody(t, derivedRank))
	}
	var ranked accountDetail
	if err := json.NewDecoder(derivedRank.Body).Decode(&ranked); err != nil {
		t.Fatal(err)
	}
	derivedRank.Body.Close()
	if ranked.Profile.GameInfo.Degree != 180 {
		t.Fatalf("derived competitive degree = %d", ranked.Profile.GameInfo.Degree)
	}
	updated := requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/gm/api/accounts/3/inventory/22", `{"quantity":9}`)
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("inventory status = %d: %s", updated.StatusCode, readBody(t, updated))
	}
	var detail accountDetail
	if err := json.NewDecoder(updated.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	updated.Body.Close()
	if detail.UIN != 3 || len(detail.Profile.Inventory) != 2 || detail.Profile.Inventory[0].ItemID != 22 || detail.Profile.Inventory[0].NumOfItem != 9 || detail.Profile.Inventory[1].ItemID != 99 {
		t.Fatalf("updated detail = %+v", detail)
	}
	if len(detail.InventoryItems) != 1 || detail.InventoryItems[0].ID != 22 || detail.InventoryItems[0].Kind != "avatar-cosmetic" {
		t.Fatalf("inventory metadata = %+v", detail.InventoryItems)
	}
	search, err := httpServer.Client().Get(httpServer.URL + "/gm/api/items?q=族长&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	var searchData struct {
		Total int        `json:"total"`
		Items []itemView `json:"items"`
	}
	if err := json.NewDecoder(search.Body).Decode(&searchData); err != nil {
		t.Fatal(err)
	}
	search.Body.Close()
	if searchData.Total != 1 || len(searchData.Items) != 1 || !searchData.Items[0].HasImage || searchData.Items[0].ImageSource != "category-icon" ||
		!searchData.Items[0].HasAppearance || searchData.Items[0].AppearanceSource != "appearance-layer" {
		t.Fatalf("item search = %+v", searchData)
	}
	if searchData.Items[0].ResourceNamespace != "account-inventory" || searchData.Items[0].ResourceKey != "account/cap/4" {
		t.Fatalf("item resource identity = %+v", searchData.Items[0])
	}
	localImages, err := httpServer.Client().Get(httpServer.URL + "/gm/api/items?images=local&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	var localImageData struct {
		Total int        `json:"total"`
		Items []itemView `json:"items"`
	}
	if err := json.NewDecoder(localImages.Body).Decode(&localImageData); err != nil {
		t.Fatal(err)
	}
	localImages.Body.Close()
	if localImageData.Total != 1 || len(localImageData.Items) != 1 || localImageData.Items[0].ID != 22 {
		t.Fatalf("local image filter = %+v", localImageData)
	}
	wrongCollision, err := httpServer.Client().Get(httpServer.URL + "/gm/api/items?q=宝石边框&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	var collisionData struct {
		Items []itemView `json:"items"`
	}
	if err := json.NewDecoder(wrongCollision.Body).Decode(&collisionData); err != nil {
		t.Fatal(err)
	}
	wrongCollision.Body.Close()
	if len(collisionData.Items) != 1 || collisionData.Items[0].HasImage || collisionData.Items[0].ImageSource != "" || collisionData.Items[0].ResourceKey != "account/frame/3" || !collisionData.Items[0].SceneIDCollision {
		t.Fatalf("decoration reused overlapping match-item icon: %+v", collisionData.Items)
	}
	icon, err := httpServer.Client().Get(httpServer.URL + "/gm/api/items/22/image")
	if err != nil {
		t.Fatal(err)
	}
	iconData, err := io.ReadAll(icon.Body)
	icon.Body.Close()
	if err != nil || icon.StatusCode != http.StatusOK || icon.Header.Get("Content-Type") != "image/png" ||
		icon.Header.Get("Cache-Control") != "no-store" || len(iconData) < 8 || string(iconData[1:4]) != "PNG" {
		t.Fatalf("icon status/type/cache/length = %d/%s/%s/%d, err=%v", icon.StatusCode, icon.Header.Get("Content-Type"),
			icon.Header.Get("Cache-Control"), len(iconData), err)
	}
	for _, test := range []struct {
		name        string
		query       string
		contentType string
		magic       string
	}{
		{name: "static appearance", query: "?appearance=1&animate=0", contentType: "image/png", magic: "\x89PNG"},
		{name: "hover animation", query: "?appearance=1&animate=1", contentType: "image/gif", magic: "GIF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, requestErr := httpServer.Client().Get(httpServer.URL + "/gm/api/items/22/image" + test.query)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			data, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != test.contentType ||
				len(data) < len(test.magic) || string(data[:len(test.magic)]) != test.magic {
				t.Fatalf("appearance status/type/magic = %d/%s/%q, err=%v", response.StatusCode, response.Header.Get("Content-Type"), data, readErr)
			}
		})
	}
	fallback, err := httpServer.Client().Get(httpServer.URL + "/gm/api/items/31/image")
	if err != nil {
		t.Fatal(err)
	}
	fallbackData, err := io.ReadAll(fallback.Body)
	fallback.Body.Close()
	if err != nil || fallback.StatusCode != http.StatusOK || !strings.HasPrefix(fallback.Header.Get("Content-Type"), "image/svg+xml") ||
		fallback.Header.Get("Cache-Control") != "no-store" || !bytes.Contains(fallbackData, []byte(">31<")) {
		t.Fatalf("fallback status/type/cache/body = %d/%s/%s/%q, err=%v", fallback.StatusCode,
			fallback.Header.Get("Content-Type"), fallback.Header.Get("Cache-Control"), fallbackData, err)
	}
	static, err := httpServer.Client().Get(httpServer.URL + "/gm/app.js")
	if err != nil {
		t.Fatal(err)
	}
	static.Body.Close()
	if static.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("static cache control = %q", static.Header.Get("Cache-Control"))
	}
	deleted := requestJSON(t, httpServer.Client(), http.MethodDelete, httpServer.URL+"/gm/api/accounts/3", "")
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", deleted.StatusCode, readBody(t, deleted))
	}
	deleted.Body.Close()
	if _, err := store.Load(context.Background(), 3); err == nil {
		t.Fatal("deleted account still loads")
	}
	if response, err := httpServer.Client().Get(httpServer.URL + "/gm/api/network"); err != nil {
		t.Fatal(err)
	} else {
		defer response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("removed GM network endpoint status = %d", response.StatusCode)
		}
	}
}

func TestGMMutationRejectsForeignOrigin(t *testing.T) {
	root := t.TempDir()
	store, err := persistence.OpenPlayerStore(filepath.Join(root, "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog, _ := equipment.NewCatalog(nil)
	server, err := New(store, nil, catalog, root, func(uint32) game.PlayerProfile { return game.DefaultPlayerProfile() })
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/gm/api/accounts", bytes.NewBufferString(`{"uin":3}`))
	request.Header.Set("Origin", "https://example.com")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("foreign-origin mutation status = %d", recorder.Code)
	}
}

func TestGMRejectsOnlineAccountDeletion(t *testing.T) {
	root := t.TempDir()
	store, err := persistence.OpenPlayerStore(filepath.Join(root, "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	profile := game.DefaultPlayerProfile()
	profile.PlayerID = 3
	profile.Nickname = "糖三"
	if err := store.Save(context.Background(), 3, profile); err != nil {
		t.Fatal(err)
	}
	catalog, _ := equipment.NewCatalog(nil)
	online := true
	server, err := New(store, nil, catalog, root, func(uint32) game.PlayerProfile { return game.DefaultPlayerProfile() },
		WithAccountOnlineCheck(func(uin uint32) bool { return online && uin == 3 }))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response := requestJSON(t, httpServer.Client(), http.MethodDelete, httpServer.URL+"/gm/api/accounts/3", "")
	if response.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, response), "在线") {
		t.Fatalf("online delete status = %d", response.StatusCode)
	}
	if _, err := store.Load(context.Background(), 3); err != nil {
		t.Fatalf("online account was deleted: %v", err)
	}

	online = false
	response = requestJSON(t, httpServer.Client(), http.MethodDelete, httpServer.URL+"/gm/api/accounts/3", "")
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("offline delete status = %d: %s", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()
	if _, err := store.Load(context.Background(), 3); err == nil {
		t.Fatal("offline account still loads after deletion")
	}
}

func TestGMRejectsOnlineInventoryAndPetRemoval(t *testing.T) {
	root := t.TempDir()
	store, err := persistence.OpenPlayerStore(filepath.Join(root, "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const uin uint32 = 3
	profile := game.DefaultPlayerProfile()
	profile.PlayerID = uint16(uin)
	profile.Nickname = "糖三"
	profile.Inventory = []game.ItemInfo{game.NewPermanentItemInfo(22, 1)}
	if err := store.Save(context.Background(), uin, profile); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEquipmentChanges(context.Background(), uin, []equipment.Change{{
		RoleID: 1, Slot: equipment.SlotHeadFront, ItemID: 22, Equipped: true,
	}}); err != nil {
		t.Fatal(err)
	}
	pet, err := store.GrantPet(context.Background(), uin, 25_001, "普通的酷比")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPetActive(context.Background(), uin, pet.PetID, true); err != nil {
		t.Fatal(err)
	}
	entries := []itemcatalog.Entry{{
		ID: 22, Index: 4, RegistryCategory: "cap", Name: "印第安族长帽", Kind: "avatar-cosmetic", Categories: []string{"cap"},
	}}
	equipmentCatalog, err := equipment.NewCatalog(entries)
	if err != nil {
		t.Fatal(err)
	}
	online := true
	server, err := New(store, entries, equipmentCatalog, root, func(uint32) game.PlayerProfile { return game.DefaultPlayerProfile() },
		WithAccountOnlineCheck(func(candidate uint32) bool { return online && candidate == uin }))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	for _, mutation := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "inventory delete", method: http.MethodDelete, path: "/gm/api/accounts/3/inventory/22"},
		{name: "pet delete", method: http.MethodDelete, path: "/gm/api/accounts/3/pet-instances/" + strconv.FormatUint(uint64(pet.PetID), 10)},
	} {
		t.Run("online "+mutation.name, func(t *testing.T) {
			response := requestJSON(t, httpServer.Client(), mutation.method, httpServer.URL+mutation.path, mutation.body)
			if response.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, response), "在线") {
				t.Fatalf("online mutation status = %d", response.StatusCode)
			}
		})
	}
	if _, found, err := store.InventoryItem(context.Background(), uin, 22); err != nil || !found {
		t.Fatalf("online removal changed inventory: found=%v, err=%v", found, err)
	}
	if loadouts, err := store.LoadEquipment(context.Background(), uin); err != nil || len(loadouts) != 1 {
		t.Fatalf("online removal changed loadouts: %+v, %v", loadouts, err)
	}
	if pets, err := store.ListPets(context.Background(), uin); err != nil || len(pets) != 1 {
		t.Fatalf("online removal changed pets: %+v, %v", pets, err)
	}
	for _, quantity := range []uint32{0, 1_000} {
		response := requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/gm/api/accounts/3/inventory/22",
			fmt.Sprintf(`{"quantity":%d}`, quantity))
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(readBody(t, response), "1 到 999") {
			t.Fatalf("quantity %d status = %d", quantity, response.StatusCode)
		}
	}

	online = false
	response := requestJSON(t, httpServer.Client(), http.MethodDelete, httpServer.URL+"/gm/api/accounts/3/inventory/22", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("offline inventory delete status = %d: %s", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()
	if _, found, err := store.InventoryItem(context.Background(), uin, 22); err != nil || found {
		t.Fatalf("offline inventory removal found=%v, err=%v", found, err)
	}
	if loadouts, err := store.LoadEquipment(context.Background(), uin); err != nil || len(loadouts) != 0 {
		t.Fatalf("offline inventory removal left loadouts: %+v, %v", loadouts, err)
	}

	response = requestJSON(t, httpServer.Client(), http.MethodDelete,
		httpServer.URL+"/gm/api/accounts/3/pet-instances/"+strconv.FormatUint(uint64(pet.PetID), 10), "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("offline pet delete status = %d: %s", response.StatusCode, readBody(t, response))
	}
	response.Body.Close()
	profile, err = store.Load(context.Background(), uin)
	if err != nil || profile.GameInfo.PetID != 0 {
		t.Fatalf("offline pet removal profile PetID=%d, err=%v", profile.GameInfo.PetID, err)
	}
	if pets, err := store.ListPets(context.Background(), uin); err != nil || len(pets) != 0 {
		t.Fatalf("offline pet removal left pets: %+v, %v", pets, err)
	}
}

func TestGMWebCancelButtonsAndInventoryOnlyGrant(t *testing.T) {
	root := t.TempDir()
	store, err := persistence.OpenPlayerStore(filepath.Join(root, "players.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog, _ := equipment.NewCatalog(nil)
	server, err := New(store, nil, catalog, root, func(uint32) game.PlayerProfile { return game.DefaultPlayerProfile() })
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	read := func(path string) string {
		t.Helper()
		response, requestErr := httpServer.Client().Get(httpServer.URL + path)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return readBody(t, response)
	}
	html := read("/gm/")
	script := read("/gm/app.js")
	if !strings.Contains(html, `id="cancel-create" type="button"`) || !strings.Contains(script, `$("cancel-create").addEventListener("click"`) {
		t.Fatal("create-account cancel button is not wired as a non-submit close action")
	}
	if !strings.Contains(html, `id="create-nickname" maxlength="16" placeholder="请输入昵称" required`) {
		t.Fatal("create-account nickname is not required")
	}
	if !strings.Contains(html, `QQ糖币<input id="profile-money"`) || strings.Contains(html, `>游戏币<input id="profile-money"`) {
		t.Fatal("profile currency label is not QQ糖币")
	}
	if !strings.Contains(html, `id="profile-adventure-level"`) || !strings.Contains(html, `id="profile-identity" required`) ||
		!strings.Contains(script, `api("/gm/api/progression")`) {
		t.Fatal("profile progression selectors are missing")
	}
	if strings.Contains(html+script, "添加并穿戴") || strings.Contains(script, "data-equip") {
		t.Fatal("removed add-and-equip action is still present")
	}
}

func requestJSON(t *testing.T, client *http.Client, method, url, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return string(data)
}

func testDIMG() []byte {
	data := make([]byte, 68+3)
	copy(data, []byte{'Q', 'Q', 'F', 0x1A, 'D', 'I', 'M', 'G'})
	binary.LittleEndian.PutUint32(data[8:12], 65536)
	binary.LittleEndian.PutUint32(data[12:16], 24)
	binary.LittleEndian.PutUint32(data[16:20], 1)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	binary.LittleEndian.PutUint32(data[32:36], 1)
	binary.LittleEndian.PutUint32(data[36:40], 1)
	binary.LittleEndian.PutUint32(data[52:56], 3)
	binary.LittleEndian.PutUint32(data[56:60], 1)
	binary.LittleEndian.PutUint32(data[60:64], 1)
	binary.LittleEndian.PutUint16(data[68:70], 0xF800)
	data[70] = 32
	return data
}

func testAnimatedDIMG() []byte {
	data := make([]byte, 40+2*(16+12+3))
	copy(data, []byte{'Q', 'Q', 'F', 0x1A, 'D', 'I', 'M', 'G'})
	binary.LittleEndian.PutUint32(data[8:12], 65536)
	binary.LittleEndian.PutUint32(data[12:16], 24)
	binary.LittleEndian.PutUint32(data[16:20], 2)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	binary.LittleEndian.PutUint32(data[32:36], 1)
	binary.LittleEndian.PutUint32(data[36:40], 1)
	for index, pixel := range []uint16{0xF800, 0x001F} {
		offset := 40 + index*(16+12+3)
		binary.LittleEndian.PutUint32(data[offset+12:offset+16], 3)
		binary.LittleEndian.PutUint32(data[offset+16:offset+20], 1)
		binary.LittleEndian.PutUint32(data[offset+20:offset+24], 1)
		binary.LittleEndian.PutUint16(data[offset+28:offset+30], pixel)
		data[offset+30] = 32
	}
	return data
}
