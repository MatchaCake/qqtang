package probe

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qqtang/internal/protocol/game"
	gmserver "qqtang/internal/server/gm"
)

func TestLoadConfigRejectsNonLoopback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"bad","network":"tcp","address":"0.0.0.0:8000","response":{}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected non-loopback listener to be rejected")
	}
}

func TestLoadConfigResolvesForgeRulesPath(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "server.json")
	data := []byte(`{"database_path":"players.sqlite","forge_rules_path":"forge-rules.json","listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(directory, "forge-rules.json")
	if config.ForgeRulesPath != want {
		t.Fatalf("forge rules path = %q, want %q", config.ForgeRulesPath, want)
	}
}

func TestLoadConfigAppliesLANHostSettings(t *testing.T) {
	directory := t.TempDir()
	networkPath := filepath.Join(directory, "network.json")
	if err := os.WriteFile(networkPath, []byte(`{"schema_version":3,"mode":"lan-host","server_ip":"192.168.50.7","client_server_ip":"192.168.50.7","gm_remote":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "server.json")
	data := []byte(`{"network_settings_path":"network.json","directory_hall":{"server_ip":"127.0.0.1","server_port":18000,"server_udp_port":18000},"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Listeners[0].Address != "0.0.0.0:18000" || config.DirectoryHall.ServerIP != "192.168.50.7" {
		t.Fatalf("LAN settings were not applied: listener=%q hall=%+v", config.Listeners[0].Address, config.DirectoryHall)
	}
}

func TestLoadConfigDerivesONNXMetadataPathFromModel(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "server.json")
	data := []byte(`{"competitive_ai":{"enabled":true,"backend":"onnxruntime","model_path":"models/actor.onnx","shared_library_path":"runtime/onnxruntime.dll"},"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(directory, "models", "actor.onnx.json")
	if config.CompetitiveAI.MetadataPath != want {
		t.Fatalf("derived ONNX metadata path = %q, want %q", config.CompetitiveAI.MetadataPath, want)
	}
}

func TestLoadConfigLocalGMIgnoresStoredCredentials(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "network.json"), []byte(`{"schema_version":3,"mode":"local","server_ip":"127.0.0.1","client_server_ip":"127.0.0.1","gm_remote":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials, err := gmserver.NewCredentials("admin", "stored but unused password")
	if err != nil {
		t.Fatal(err)
	}
	if err := gmserver.SaveCredentials(filepath.Join(directory, "gm-auth.json"), credentials); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "server.json")
	data := []byte(`{"database_path":"players.sqlite","network_settings_path":"network.json","gm_http_address":"127.0.0.1:18100","gm_auth_path":"gm-auth.json","listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.GMHTTPAddress != "127.0.0.1:18100" || config.GMCredentials != nil {
		t.Fatalf("local GM = address %q credentials=%v", config.GMHTTPAddress, config.GMCredentials != nil)
	}
}

func TestLoadConfigRejectsObsoleteNetworkSchema(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "network.json"), []byte(`{"schema_version":2,"mode":"lan-client","server_ip":"192.168.50.7","outbound_isolation":false,"gm_remote":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "server.json")
	if err := os.WriteFile(path, []byte(`{"network_settings_path":"network.json","listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected obsolete network schema to reject server startup")
	}
}

func TestLoadConfigRemoteHostSeparatesBindAdvertiseAndRequiresGMAuth(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "network.json"), []byte(`{"schema_version":3,"mode":"remote-host","server_ip":"203.0.113.8","client_server_ip":"203.0.113.8","gm_remote":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "server.json")
	data := []byte(`{"database_path":"players.sqlite","network_settings_path":"network.json","gm_http_address":"127.0.0.1:18100","gm_auth_path":"gm-auth.json","directory_hall":{"server_ip":"127.0.0.1","server_port":18000,"server_udp_port":18000},"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("remote GM without credentials was accepted")
	}
	credentials, err := gmserver.NewCredentials("admin", "strong local password")
	if err != nil {
		t.Fatal(err)
	}
	if err := gmserver.SaveCredentials(filepath.Join(directory, "gm-auth.json"), credentials); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Listeners[0].Address != "0.0.0.0:18000" || config.DirectoryHall.ServerIP != "203.0.113.8" || config.GMHTTPAddress != "0.0.0.0:18100" || config.GMCredentials == nil {
		t.Fatalf("remote config = listener %q hall %+v gm %q auth=%v", config.Listeners[0].Address, config.DirectoryHall, config.GMHTTPAddress, config.GMCredentials != nil)
	}
}

func TestLoadConfigRemoteHostBindsEveryGameAndShopListener(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "network.json"), []byte(`{"schema_version":3,"mode":"remote-host","server_ip":"203.0.113.8","client_server_ip":"203.0.113.8","gm_remote":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "server.json")
	data := []byte(`{
		"database_path":"players.sqlite",
		"network_settings_path":"network.json",
		"gm_http_address":"127.0.0.1:18100",
		"directory_hall":{"server_ip":"127.0.0.1","server_port":18000,"server_udp_port":18000,"shop_server_id":2,"shop_server_port":18001,"shop_server_udp_port":18001,"deal_server_id":3,"deal_server_port":18001,"deal_server_udp_port":18001},
		"listeners":[
			{"name":"directory","network":"tcp","address":"127.0.0.1:18080","response":{}},
			{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}},
			{"name":"shop","network":"tcp","address":"127.0.0.1:18001","response":{"qqt_login_success":true,"qqt_shop_catalog":true,"qqt_shop_type2_session":true}},
			{"name":"game-fast-path","network":"udp","address":"127.0.0.1:18000","response":{}}
		]
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, listener := range config.Listeners {
		host, _, splitErr := net.SplitHostPort(listener.Address)
		if splitErr != nil || host != "0.0.0.0" {
			t.Fatalf("remote listener %s = %q, split error %v", listener.Name, listener.Address, splitErr)
		}
	}
	if config.DirectoryHall.ServerIP != "203.0.113.8" || config.DirectoryHall.ShopServerIP != "203.0.113.8" || config.DirectoryHall.DealServerIP != "203.0.113.8" {
		t.Fatalf("remote advertised hall = %+v", config.DirectoryHall)
	}
	if config.GMHTTPAddress != "127.0.0.1:18100" {
		t.Fatalf("remote GM default address = %q", config.GMHTTPAddress)
	}
}

func TestLoadConfigResponseHex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"max_packet_bytes":64,"idle_timeout_ms":1000,"listeners":[{"name":"tcp","network":"tcp","address":"127.0.0.1:0","response":{"after_receive_hex":"01 02 ff"}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := config.Listeners[0].response; len(got) != 3 || got[0] != 1 || got[2] != 0xff {
		t.Fatalf("unexpected response %x", got)
	}
}

func TestLoadConfigRejectsMultipleAfterReceiveModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"tcp","network":"tcp","address":"127.0.0.1:0","response":{"after_receive_hex":"01","echo_after_receive":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected multiple response modes to be rejected")
	}
}

func TestLoadConfigQQTLoginSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{"qqt_login_success":true,"qqt_login_success_copies":2}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.Listeners[0].Response.QQTLoginSuccess {
		t.Fatal("qqt_login_success was not retained")
	}
	if config.Listeners[0].Response.QQTLoginSuccessCopies != 2 {
		t.Fatalf("qqt_login_success_copies = %d", config.Listeners[0].Response.QQTLoginSuccessCopies)
	}
}

func TestDirectoryHallConfigAddsDistinctShopAndDealServers(t *testing.T) {
	config := DirectoryHallConfig{
		ServerIP: "127.0.0.1", ServerPort: 18000, ServerUDPPort: 18000,
		ShopServerID: 2, ShopServerPort: 18001, ShopServerUDPPort: 18001,
		DealServerID: 3, DealServerPort: 18001, DealServerUDPPort: 18001,
	}
	native, err := config.NativeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(native.AdditionalServers) != 2 ||
		native.AdditionalServers[0].ServerID != 2 || native.AdditionalServers[0].ServerPort != 18001 ||
		native.AdditionalServers[1].ServerID != 3 || native.AdditionalServers[1].ServerPort != 18001 {
		t.Fatalf("shop/deal directory servers = %+v", native.AdditionalServers)
	}
}

func TestParseClientDealServerID(t *testing.T) {
	serverID, err := parseClientDealServerID([]byte("[public]\r\nvs=723\r\n[deal]\r\ndealsvrdid = 3\r\n[shop]\r\nlastShopVertion=5\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if serverID != 3 {
		t.Fatalf("deal server ID = %d", serverID)
	}
}

func TestLoadConfigDerivesDealServerIDFromClient(t *testing.T) {
	directory := t.TempDir()
	clientRoot := filepath.Join(directory, "client")
	if err := os.MkdirAll(filepath.Join(clientRoot, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientRoot, "config", "DLVersion.ini"), []byte("[deal]\ndealsvrdid=3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "server.json")
	data := []byte(`{
		"client_root":"client",
		"directory_hall":{"server_ip":"127.0.0.1","server_port":18000,"server_udp_port":18000,"shop_server_id":2,"shop_server_port":18001,"shop_server_udp_port":18001,"deal_server_port":18001,"deal_server_udp_port":18001},
		"listeners":[{"name":"shop","network":"tcp","address":"127.0.0.1:18001","response":{"qqt_login_success":true,"qqt_shop_catalog":true,"qqt_shop_type2_session":true}}]
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.DirectoryHall.ShopServerID != 2 || config.DirectoryHall.DealServerID != 3 {
		t.Fatalf("shop/deal server IDs = %d/%d", config.DirectoryHall.ShopServerID, config.DirectoryHall.DealServerID)
	}
}

func TestLoadConfigRejectsDealServerIDThatConflictsWithClient(t *testing.T) {
	directory := t.TempDir()
	clientRoot := filepath.Join(directory, "client")
	if err := os.MkdirAll(filepath.Join(clientRoot, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientRoot, "config", "DLVersion.ini"), []byte("[deal]\ndealsvrdid=3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "server.json")
	data := []byte(`{
		"client_root":"client",
		"directory_hall":{"server_ip":"127.0.0.1","server_port":18000,"server_udp_port":18000,"shop_server_id":2,"shop_server_port":18001,"shop_server_udp_port":18001,"deal_server_id":2,"deal_server_port":18001,"deal_server_udp_port":18001},
		"listeners":[{"name":"shop","network":"tcp","address":"127.0.0.1:18001","response":{"qqt_login_success":true,"qqt_shop_catalog":true,"qqt_shop_type2_session":true}}]
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "conflicts with client dealsvrdid 3") {
		t.Fatalf("conflicting deal server ID error = %v", err)
	}
}

func TestLoadConfigRejectsType2ShopSessionWithoutLoginProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"shop-type2-session","network":"tcp","address":"127.0.0.1:18001","response":{"qqt_shop_type2_session":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected shop session without login protocol to be rejected")
	}
}

func TestLoadConfigAcceptsCombinedNativeShopTransports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"directory_hall":{"server_ip":"127.0.0.1","server_port":18000,"server_udp_port":18000,"shop_server_id":2,"shop_server_port":18001,"shop_server_udp_port":18001},"listeners":[{"name":"shop","network":"tcp","address":"127.0.0.1:18001","response":{"qqt_login_success":true,"qqt_shop_catalog":true,"qqt_shop_type2_session":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Listeners) != 1 || !config.Listeners[0].Response.QQTShopCatalog || !config.Listeners[0].Response.QQTShopType2Session {
		t.Fatalf("combined shop listener = %+v", config.Listeners)
	}
}

func TestLoadConfigRejectsShopSessionThatDoesNotMatchDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{
		"directory_hall":{"server_ip":"127.0.0.1","server_port":18000,"server_udp_port":18000,"shop_server_id":2,"shop_server_port":18003,"shop_server_udp_port":18003},
		"listeners":[{"name":"shop","network":"tcp","address":"127.0.0.1:18001","response":{"qqt_login_success":true,"qqt_shop_catalog":true,"qqt_shop_type2_session":true}}]
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "must match native shop") {
		t.Fatalf("mismatched directory shop endpoint error = %v", err)
	}
}

func TestLoadConfigRejectsIncompleteNativeShopPair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{
		"directory_hall":{"server_ip":"127.0.0.1","server_port":18000,"server_udp_port":18000,"shop_server_id":2,"shop_server_port":18002,"shop_server_udp_port":18002},
		"listeners":[{"name":"shop-deal","network":"tcp","address":"127.0.0.1:18001","response":{"qqt_shop_catalog":true}}]
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "combining") {
		t.Fatalf("incomplete native shop pair error = %v", err)
	}
}

func TestLoadConfigRejectsQQTLoginCopiesWithoutLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{"qqt_login_success_copies":2}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected login copies without qqt_login_success to be rejected")
	}
}

func TestLoadConfigRejectsQQTLoginSuccessOnUDP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"game","network":"udp","address":"127.0.0.1:18000","response":{"qqt_login_success":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected UDP dynamic login response to be rejected")
	}
}

func TestLoadConfigAllowsQQTLocalSessionResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{"qqt_login_success":true,"qqt_room_list":true,"qqt_player_list":true,"qqt_create_room_success":true,"qqt_start_game_success":true,"qqt_adventure_game":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	response := config.Listeners[0].Response
	if !response.QQTLoginSuccess || !response.QQTRoomList || !response.QQTPlayerList || !response.QQTCreateRoomSuccess || !response.QQTStartGameSuccess || !response.QQTAdventureGame {
		t.Fatalf("dynamic flags were not retained: %+v", response)
	}
}

func TestLoadConfigRejectsAdventureGameWithoutStartGame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{"qqt_adventure_game":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected qqt_adventure_game without qqt_start_game_success to be rejected")
	}
}

func TestLoadConfigRejectsObsoleteHeaderOnlyGameStartNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{"qqt_start_game_success":true,"qqt_game_start_notify":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected obsolete qqt_game_start_notify field to be rejected")
	}
}

func TestLoadConfigRejectsQQTPlayerListOnUDP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"game","network":"udp","address":"127.0.0.1:18000","response":{"qqt_player_list":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected UDP dynamic player-list response to be rejected")
	}
}

func TestLoadConfigRejectsQQTRoomListOnUDP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"game","network":"udp","address":"127.0.0.1:18000","response":{"qqt_room_list":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected UDP dynamic room-list response to be rejected")
	}
}

func TestLoadConfigRejectsInvalidResponseLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"listeners":[{"name":"tcp","network":"tcp","address":"127.0.0.1:0","response":{"after_receive_hex":"01","max_sends":-1}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected invalid max_sends to fail")
	}
}

func TestLoadConfigPlayerProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"player_profile":{"player_id":1,"section_id":1,"minimum_room_id":1,"tutorial_completed":true,"game_info":{"wins":5200,"points":1631150000,"degree":180},"inventory":[{"id":99,"quantity":1}]},"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{"qqt_login_success":true}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	login := config.localLoginConfig()
	if login.GameInfo.Point != game.MaxPlayerExperience || login.GameInfo.Degree != game.MaxPlayerLevel {
		t.Fatalf("unexpected configured level: %+v", login.GameInfo)
	}
	if len(login.Items) != 1 || login.Items[0].ItemID != game.SinglePlayerAdventureCardItemID || login.Items[0].NumOfItem != 1 || login.Items[0].AvailPeriod != game.LocalPermanentAvailablePeriod {
		t.Fatalf("unexpected configured inventory: %+v", login.Items)
	}
}

func TestPlayerProfileForUINAssignsDistinctLocalPlayerIDs(t *testing.T) {
	config := Config{SeedUIN: 1_000_001, PlayerProfile: &game.PlayerProfile{
		PlayerID: 1, SectionID: 1, MinimumRoomID: 1, GameInfo: game.GameInfo{RoleID: 1},
	}}
	primary := config.playerProfileForUIN(1_000_001)
	secondary := config.playerProfileForUIN(1_000_002)
	third := config.playerProfileForUIN(1_000_003)
	if primary.PlayerID != 1 || secondary.PlayerID != 2 || third.PlayerID != 3 {
		t.Fatalf("local PlayerIDs = %d/%d/%d, want 1/2/3", primary.PlayerID, secondary.PlayerID, third.PlayerID)
	}
	if secondary.Inventory == nil && config.PlayerProfile.Inventory != nil {
		t.Fatal("secondary local profile did not clone seed inventory")
	}
}

func TestLoadConfigRejectsPlayerAboveClientMaximum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"player_profile":{"player_id":1,"section_id":1,"minimum_room_id":1,"game_info":{"points":1631150001,"degree":181}},"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected an above-maximum local player profile to be rejected")
	}
}

func TestLoadConfigRejectsUnknownCompetitiveBossRewardKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	data := []byte(`{"competitive_boss_rewards":{"guessed_boss":[{"item_id":81,"item_count":1}]},"listeners":[{"name":"game","network":"tcp","address":"127.0.0.1:18000","response":{}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), `unknown candidate "guessed_boss"`) {
		t.Fatalf("unknown competitive Boss reward key error = %v", err)
	}
}

// This test covers static resources and persistence, not the optional live
// AI deployment selected by the project development config.
