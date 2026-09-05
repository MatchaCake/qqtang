package probe

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"qqtang/internal/game/mapdata"
	"qqtang/internal/protocol/directory"
	"qqtang/internal/protocol/game"
	gmserver "qqtang/internal/server/gm"
	"qqtang/internal/server/networksetup"
)

type Config struct {
	CaptureRoot            string                                   `json:"capture_root"`
	LogFile                string                                   `json:"log_file"`
	ClientRoot             string                                   `json:"client_root,omitempty"`
	DatabasePath           string                                   `json:"database_path,omitempty"`
	SeedUIN                uint32                                   `json:"seed_uin,omitempty"`
	InitialAccounts        []InitialAccountConfig                   `json:"initial_accounts,omitempty"`
	AdventureRulesPath     string                                   `json:"adventure_rules_path,omitempty"`
	RoleRulesPath          string                                   `json:"role_rules_path,omitempty"`
	EquipmentSeedsPath     string                                   `json:"equipment_seeds_path,omitempty"`
	CombineRecipesPath     string                                   `json:"combine_recipes_path,omitempty"`
	BreakEggRewardsPath    string                                   `json:"break_egg_rewards_path,omitempty"`
	ForgeSuccessPercent    *int                                     `json:"forge_apply_success_percent,omitempty"`
	ItemImageAliasesPath   string                                   `json:"item_image_aliases_path,omitempty"`
	NetworkSettingsPath    string                                   `json:"network_settings_path,omitempty"`
	GMHTTPAddress          string                                   `json:"gm_http_address,omitempty"`
	GMAuthPath             string                                   `json:"gm_auth_path,omitempty"`
	DirectoryHall          *DirectoryHallConfig                     `json:"directory_hall,omitempty"`
	AdventureRewards       AdventureRewardConfig                    `json:"adventure_rewards,omitempty"`
	CompetitiveBossRewards map[string][]CompetitiveBossRewardConfig `json:"competitive_boss_rewards,omitempty"`
	CompetitiveAI          CompetitiveAIConfig                      `json:"competitive_ai,omitempty"`
	MaxPacketSize          int                                      `json:"max_packet_bytes"`
	IdleTimeoutMS          int                                      `json:"idle_timeout_ms"`
	PlayerProfile          *game.PlayerProfile                      `json:"player_profile,omitempty"`
	Listeners              []ListenerConfig                         `json:"listeners"`

	NetworkSettings networksetup.Settings `json:"-"`
	GMCredentials   *gmserver.Credentials `json:"-"`
}

// CompetitiveAIConfig gates the experimental live rule-1 virtual-player
// adapter. Disabled is the zero value and preserves every existing room.
// ModelPath points to the actor artifact selected by Backend. ONNX Runtime
// reads the exported metadata sidecar next to the model and additionally
// requires the shared library; every path is resolved relative to the server
// config file.
type CompetitiveAIConfig struct {
	Enabled           bool   `json:"enabled,omitempty"`
	Backend           string `json:"backend,omitempty"`
	ModelPath         string `json:"model_path,omitempty"`
	MetadataPath      string `json:"metadata_path,omitempty"`
	SharedLibraryPath string `json:"shared_library_path,omitempty"`
	IntraOpThreads    int    `json:"intra_op_threads,omitempty"`
	InterOpThreads    int    `json:"inter_op_threads,omitempty"`
	TickMS            uint32 `json:"tick_ms,omitempty"`
	SearchEnabled     bool   `json:"search_enabled,omitempty"`
}

type DirectoryHallConfig struct {
	ServerIP          string `json:"server_ip"`
	ServerPort        uint16 `json:"server_port"`
	ServerUDPPort     uint16 `json:"server_udp_port"`
	ShopServerID      uint32 `json:"shop_server_id,omitempty"`
	ShopServerIP      string `json:"shop_server_ip,omitempty"`
	ShopServerPort    uint16 `json:"shop_server_port,omitempty"`
	ShopServerUDPPort uint16 `json:"shop_server_udp_port,omitempty"`
	DealServerID      uint32 `json:"deal_server_id,omitempty"`
	DealServerIP      string `json:"deal_server_ip,omitempty"`
	DealServerPort    uint16 `json:"deal_server_port,omitempty"`
	DealServerUDPPort uint16 `json:"deal_server_udp_port,omitempty"`
}

func (config DirectoryHallConfig) NativeConfig() (directory.LocalHallNativeConfig, error) {
	native := directory.DefaultLocalHallNativeConfig()
	address, err := netip.ParseAddr(strings.TrimSpace(config.ServerIP))
	if err != nil || !address.Is4() {
		return native, fmt.Errorf("directory_hall.server_ip %q is not IPv4", config.ServerIP)
	}
	if !address.IsLoopback() && !address.IsPrivate() && !address.IsGlobalUnicast() {
		return native, fmt.Errorf("directory_hall.server_ip %q must be a unicast IPv4 address", address)
	}
	if config.ServerPort == 0 || config.ServerUDPPort == 0 {
		return native, fmt.Errorf("directory_hall server ports must be non-zero")
	}
	native.ServerIP = address
	native.ServerPort = config.ServerPort
	native.ServerUDPPort = config.ServerUDPPort
	shopConfigured := config.ShopServerID != 0 || strings.TrimSpace(config.ShopServerIP) != "" || config.ShopServerPort != 0 || config.ShopServerUDPPort != 0
	if shopConfigured {
		if config.ShopServerID == 0 || config.ShopServerPort == 0 || config.ShopServerUDPPort == 0 {
			return native, fmt.Errorf("directory_hall shop server ID and ports must all be non-zero")
		}
		shopAddress := address
		if value := strings.TrimSpace(config.ShopServerIP); value != "" {
			shopAddress, err = netip.ParseAddr(value)
			if err != nil || !shopAddress.Is4() {
				return native, fmt.Errorf("directory_hall.shop_server_ip %q is not IPv4", config.ShopServerIP)
			}
			if !shopAddress.IsLoopback() && !shopAddress.IsPrivate() && !shopAddress.IsGlobalUnicast() {
				return native, fmt.Errorf("directory_hall.shop_server_ip %q must be a unicast IPv4 address", shopAddress)
			}
		}
		native.AdditionalServers = append(native.AdditionalServers, directory.LocalHallServerConfig{
			ServerID: config.ShopServerID, ServerIP: shopAddress,
			ServerPort: config.ShopServerPort, ServerUDPPort: config.ShopServerUDPPort,
		})
	}
	dealConfigured := config.DealServerID != 0 || strings.TrimSpace(config.DealServerIP) != "" || config.DealServerPort != 0 || config.DealServerUDPPort != 0
	if dealConfigured {
		if config.DealServerID == 0 || config.DealServerPort == 0 || config.DealServerUDPPort == 0 {
			return native, fmt.Errorf("directory_hall deal server ID and ports must all be non-zero")
		}
		dealAddress := address
		if value := strings.TrimSpace(config.DealServerIP); value != "" {
			dealAddress, err = netip.ParseAddr(value)
			if err != nil || !dealAddress.Is4() {
				return native, fmt.Errorf("directory_hall.deal_server_ip %q is not IPv4", config.DealServerIP)
			}
			if !dealAddress.IsLoopback() && !dealAddress.IsPrivate() && !dealAddress.IsGlobalUnicast() {
				return native, fmt.Errorf("directory_hall.deal_server_ip %q must be a unicast IPv4 address", dealAddress)
			}
		}
		native.AdditionalServers = append(native.AdditionalServers, directory.LocalHallServerConfig{
			ServerID: config.DealServerID, ServerIP: dealAddress,
			ServerPort: config.DealServerPort, ServerUDPPort: config.DealServerUDPPort,
		})
	}
	return native, nil
}

// AdventureRewardConfig is the explicit local-server score policy for the
// result extension fields. The client adds every FieldValue2 score to its
// adventure growth value; QQT_GAME_RESULT_DATA.Point is a separate field and
// must not be mislabeled as experience.
type AdventureRewardConfig struct {
	NPCKillScore      uint32 `json:"npc_kill_score"`
	PlayerRescueScore uint32 `json:"player_rescue_score"`
	ItemPickupScore   uint32 `json:"item_pickup_score"`
}

// CompetitiveBossRewardConfig is a reconstructed server-side BOSS_INFO
// inventory entry. ItemID belongs to the battlefield scene-element namespace,
// not the account inventory namespace. Slice order is significant to the
// native rule-1 random picker and therefore remains explicit in JSON.
// ChancePercent is a deterministic server-side eligibility roll. Zero keeps
// older configurations source-compatible and means 100 percent. Known scene
// rewards use BOSS_INFO.NormalItems; permanent inventory pickups use
// OutfitItems and therefore the lethal path.
type CompetitiveBossRewardConfig struct {
	ItemID        uint32 `json:"item_id"`
	ItemCount     uint16 `json:"item_count"`
	DropTime      uint16 `json:"drop_time,omitempty"`
	ChancePercent uint8  `json:"chance_percent,omitempty"`
}

// InitialAccountConfig describes one account created only while initializing a
// brand-new local database. It is deliberately separate from PlayerProfile:
// these values are never projected over a persisted account at login or while
// entering a room.
type InitialAccountConfig struct {
	UIN      uint32  `json:"uin"`
	Nickname *string `json:"nickname,omitempty"`
	Gender   *byte   `json:"gender,omitempty"`
	IconID   *byte   `json:"icon_id,omitempty"`
	Identity *uint32 `json:"identity,omitempty"`
}

type ListenerConfig struct {
	Name     string         `json:"name"`
	Network  string         `json:"network"`
	Address  string         `json:"address"`
	Response ResponseConfig `json:"response"`

	response []byte
}

type ResponseConfig struct {
	OnConnectHex          string `json:"on_connect_hex,omitempty"`
	AfterReceiveHex       string `json:"after_receive_hex,omitempty"`
	AfterReceiveFile      string `json:"after_receive_file,omitempty"`
	EchoAfterReceive      bool   `json:"echo_after_receive,omitempty"`
	QQTDirectoryResponse  bool   `json:"qqt_directory_response,omitempty"`
	QQTLoginSuccess       bool   `json:"qqt_login_success,omitempty"`
	QQTLoginSuccessCopies int    `json:"qqt_login_success_copies,omitempty"`
	QQTRoomList           bool   `json:"qqt_room_list,omitempty"`
	QQTPlayerList         bool   `json:"qqt_player_list,omitempty"`
	QQTCreateRoomSuccess  bool   `json:"qqt_create_room_success,omitempty"`
	QQTStartGameSuccess   bool   `json:"qqt_start_game_success,omitempty"`
	QQTGameBegin          bool   `json:"qqt_game_begin,omitempty"`
	// QQTAdventureGame is the legacy configuration name retained so older
	// release bundles still load. The handler itself is category-neutral.
	QQTAdventureGame    bool `json:"qqt_adventure_game,omitempty"`
	QQTGameEventRelay   bool `json:"qqt_game_event_relay,omitempty"`
	QQTShopCatalog      bool `json:"qqt_shop_catalog,omitempty"`
	QQTShopType2Session bool `json:"qqt_shop_type2_session,omitempty"`
	MaxSends            int  `json:"max_sends,omitempty"`
}

func (response ResponseConfig) shopEnabled() bool {
	return response.QQTShopCatalog || response.QQTShopType2Session
}

func (response ResponseConfig) gameBeginEnabled() bool {
	return response.QQTGameBegin || response.QQTAdventureGame
}

func (response ResponseConfig) dynamicQQTSession() bool {
	return response.QQTLoginSuccess || response.QQTRoomList || response.QQTPlayerList ||
		response.QQTCreateRoomSuccess || response.QQTStartGameSuccess ||
		response.QQTGameEventRelay || response.gameBeginEnabled() || response.shopEnabled()
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var config Config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if config.CaptureRoot == "" {
		config.CaptureRoot = "runtime/captures"
	}
	if config.MaxPacketSize == 0 {
		config.MaxPacketSize = 64 * 1024
	}
	if config.IdleTimeoutMS == 0 {
		config.IdleTimeoutMS = int((5 * time.Minute) / time.Millisecond)
	}
	base := filepath.Dir(path)
	if config.ClientRoot == "" {
		config.ClientRoot = filepath.Join(base, "..", "runtime", "client-patched")
	} else if !filepath.IsAbs(config.ClientRoot) {
		config.ClientRoot = filepath.Join(base, config.ClientRoot)
	}
	config.ClientRoot = filepath.Clean(config.ClientRoot)
	if config.CompetitiveAI.Enabled {
		config.CompetitiveAI.Backend = strings.ToLower(strings.TrimSpace(config.CompetitiveAI.Backend))
		if config.CompetitiveAI.Backend == "" {
			config.CompetitiveAI.Backend = "native"
		}
		if config.CompetitiveAI.Backend != "native" && config.CompetitiveAI.Backend != "onnxruntime" {
			return Config{}, fmt.Errorf("competitive_ai.backend %q is unsupported", config.CompetitiveAI.Backend)
		}
		if strings.TrimSpace(config.CompetitiveAI.ModelPath) == "" {
			return Config{}, fmt.Errorf("competitive_ai.model_path is required when live AI is enabled")
		}
		resolveAIPath := func(value string) string {
			if !filepath.IsAbs(value) {
				value = filepath.Join(base, value)
			}
			return filepath.Clean(value)
		}
		config.CompetitiveAI.ModelPath = resolveAIPath(config.CompetitiveAI.ModelPath)
		if config.CompetitiveAI.Backend == "onnxruntime" {
			if strings.TrimSpace(config.CompetitiveAI.MetadataPath) == "" {
				config.CompetitiveAI.MetadataPath = config.CompetitiveAI.ModelPath + ".json"
			} else {
				config.CompetitiveAI.MetadataPath = resolveAIPath(config.CompetitiveAI.MetadataPath)
			}
			if strings.TrimSpace(config.CompetitiveAI.SharedLibraryPath) == "" {
				return Config{}, fmt.Errorf("competitive_ai.shared_library_path is required for ONNX Runtime")
			}
			config.CompetitiveAI.SharedLibraryPath = resolveAIPath(config.CompetitiveAI.SharedLibraryPath)
		}
		if config.CompetitiveAI.IntraOpThreads < 0 || config.CompetitiveAI.InterOpThreads < 0 {
			return Config{}, fmt.Errorf("competitive_ai thread counts must be non-negative")
		}
		if config.CompetitiveAI.TickMS == 0 {
			config.CompetitiveAI.TickMS = 100
		}
		if config.CompetitiveAI.TickMS < 20 || config.CompetitiveAI.TickMS > 250 {
			return Config{}, fmt.Errorf("competitive_ai.tick_ms %d is outside 20..250", config.CompetitiveAI.TickMS)
		}
	}
	if err := config.resolveClientDealServerID(); err != nil {
		return Config{}, err
	}
	if config.DatabasePath != "" && !filepath.IsAbs(config.DatabasePath) {
		config.DatabasePath = filepath.Clean(filepath.Join(base, config.DatabasePath))
	}
	if config.AdventureRulesPath != "" && !filepath.IsAbs(config.AdventureRulesPath) {
		config.AdventureRulesPath = filepath.Clean(filepath.Join(base, config.AdventureRulesPath))
	}
	if config.RoleRulesPath != "" && !filepath.IsAbs(config.RoleRulesPath) {
		config.RoleRulesPath = filepath.Clean(filepath.Join(base, config.RoleRulesPath))
	}
	if config.EquipmentSeedsPath != "" && !filepath.IsAbs(config.EquipmentSeedsPath) {
		config.EquipmentSeedsPath = filepath.Clean(filepath.Join(base, config.EquipmentSeedsPath))
	}
	if config.CombineRecipesPath != "" && !filepath.IsAbs(config.CombineRecipesPath) {
		config.CombineRecipesPath = filepath.Clean(filepath.Join(base, config.CombineRecipesPath))
	}
	if config.BreakEggRewardsPath != "" && !filepath.IsAbs(config.BreakEggRewardsPath) {
		config.BreakEggRewardsPath = filepath.Clean(filepath.Join(base, config.BreakEggRewardsPath))
	}
	if config.ItemImageAliasesPath != "" && !filepath.IsAbs(config.ItemImageAliasesPath) {
		config.ItemImageAliasesPath = filepath.Clean(filepath.Join(base, config.ItemImageAliasesPath))
	}
	if config.GMAuthPath != "" {
		if !filepath.IsAbs(config.GMAuthPath) {
			config.GMAuthPath = filepath.Clean(filepath.Join(base, config.GMAuthPath))
		}
	}
	config.NetworkSettings = networksetup.Default()
	if config.NetworkSettingsPath != "" {
		if !filepath.IsAbs(config.NetworkSettingsPath) {
			config.NetworkSettingsPath = filepath.Clean(filepath.Join(base, config.NetworkSettingsPath))
		}
		settings, loadErr := networksetup.Load(config.NetworkSettingsPath)
		if loadErr != nil {
			return Config{}, loadErr
		}
		config.NetworkSettings = settings
		if !settings.RunsServer() {
			return Config{}, fmt.Errorf("network mode %s does not run a local server; use the client launcher", settings.Mode)
		}
		if err := config.applyNetworkSettings(); err != nil {
			return Config{}, err
		}
	}
	// Loopback GM deliberately needs no credentials. A password becomes
	// mandatory only when the operator explicitly exposes GM to LAN/Internet.
	if config.NetworkSettings.GMRemote && config.GMAuthPath != "" {
		credentials, loadErr := gmserver.LoadCredentials(config.GMAuthPath)
		if loadErr == nil {
			config.GMCredentials = credentials
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return Config{}, loadErr
		}
	}
	if err := config.validate(base); err != nil {
		return Config{}, err
	}
	return config, nil
}

// resolveClientDealServerID keeps the deal-server advertisement aligned with
// the original client's DLVersion.ini. Shop and deal servers are distinct
// QQTDir candidate classes even when both services share one TCP endpoint:
// GetRandShopServerID reads the shop table, while [deal] dealsvrdid identifies
// the separate deal table. Treating dealsvrdid as ShopServerID leaves the
// native shop table empty and makes the selector return zero.
func (config *Config) resolveClientDealServerID() error {
	if config.DirectoryHall == nil {
		return nil
	}
	hall := config.DirectoryHall
	dealConfigured := hall.DealServerID != 0 || strings.TrimSpace(hall.DealServerIP) != "" ||
		hall.DealServerPort != 0 || hall.DealServerUDPPort != 0
	if !dealConfigured {
		return nil
	}
	path := filepath.Join(config.ClientRoot, "config", "DLVersion.ini")
	contents, err := os.ReadFile(path)
	if err != nil {
		// Explicit IDs remain supported for isolated protocol tests and custom
		// client trees that intentionally omit the original configuration file.
		if hall.DealServerID != 0 && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read client deal-server configuration %s: %w", path, err)
	}
	serverID, err := parseClientDealServerID(contents)
	if err != nil {
		return fmt.Errorf("parse client deal-server configuration %s: %w", path, err)
	}
	if hall.DealServerID != 0 && hall.DealServerID != serverID {
		return fmt.Errorf("directory_hall.deal_server_id %d conflicts with client dealsvrdid %d", hall.DealServerID, serverID)
	}
	hall.DealServerID = serverID
	return nil
}

func parseClientDealServerID(contents []byte) (uint32, error) {
	inDealSection := false
	for _, rawLine := range strings.Split(string(contents), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if comment := strings.IndexByte(line, ';'); comment >= 0 {
			line = strings.TrimSpace(line[:comment])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inDealSection = strings.EqualFold(strings.TrimSpace(line[1:len(line)-1]), "deal")
			continue
		}
		if !inDealSection {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "dealsvrdid") {
			continue
		}
		parsed, parseErr := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
		if parseErr != nil || parsed == 0 {
			return 0, fmt.Errorf("dealsvrdid %q is not a non-zero uint32", strings.TrimSpace(value))
		}
		return uint32(parsed), nil
	}
	return 0, fmt.Errorf("[deal] dealsvrdid is absent")
}

func (config *Config) applyNetworkSettings() error {
	serverAddress, err := config.NetworkSettings.ResolveServerIPv4()
	if err != nil {
		return err
	}
	serverIP := serverAddress.String()
	bindIP := config.NetworkSettings.BindIP()
	for index := range config.Listeners {
		_, port, err := net.SplitHostPort(config.Listeners[index].Address)
		if err != nil {
			return fmt.Errorf("listener %q address: %w", config.Listeners[index].Name, err)
		}
		config.Listeners[index].Address = net.JoinHostPort(bindIP, port)
	}
	if config.DirectoryHall != nil {
		config.DirectoryHall.ServerIP = serverIP
		if config.DirectoryHall.ShopServerID != 0 {
			config.DirectoryHall.ShopServerIP = serverIP
		}
		if config.DirectoryHall.DealServerID != 0 {
			config.DirectoryHall.DealServerIP = serverIP
		}
	}
	if config.GMHTTPAddress != "" {
		_, port, err := net.SplitHostPort(config.GMHTTPAddress)
		if err != nil {
			return fmt.Errorf("gm_http_address: %w", err)
		}
		gmHost := "127.0.0.1"
		if config.NetworkSettings.GMRemote {
			gmHost = bindIP
		}
		config.GMHTTPAddress = net.JoinHostPort(gmHost, port)
	}
	return nil
}

func (config *Config) validate(configBase string) error {
	if config.MaxPacketSize < 1 || config.MaxPacketSize > 1024*1024 {
		return fmt.Errorf("max_packet_bytes must be between 1 and 1048576")
	}
	if config.IdleTimeoutMS < 1 || config.IdleTimeoutMS > int((24*time.Hour)/time.Millisecond) {
		return fmt.Errorf("idle_timeout_ms is outside the safe range")
	}
	if len(config.Listeners) == 0 {
		return fmt.Errorf("at least one listener is required")
	}
	if config.PlayerProfile != nil {
		if err := config.PlayerProfile.Validate(); err != nil {
			return err
		}
	}
	if config.SeedUIN != 0 && (config.DatabasePath == "" || config.PlayerProfile == nil) {
		return fmt.Errorf("seed_uin requires database_path and player_profile")
	}
	if config.EquipmentSeedsPath != "" && config.DatabasePath == "" {
		return fmt.Errorf("equipment_seeds_path requires database_path")
	}
	if config.CombineRecipesPath != "" && config.DatabasePath == "" {
		return fmt.Errorf("combine_recipes_path requires database_path")
	}
	if config.BreakEggRewardsPath != "" && config.DatabasePath == "" {
		return fmt.Errorf("break_egg_rewards_path requires database_path")
	}
	if config.ForgeSuccessPercent != nil && (*config.ForgeSuccessPercent < 0 || *config.ForgeSuccessPercent > 100) {
		return fmt.Errorf("forge_apply_success_percent is outside 0..100")
	}
	for candidateID, rewards := range config.CompetitiveBossRewards {
		if strings.TrimSpace(candidateID) == "" {
			return fmt.Errorf("competitive_boss_rewards contains an empty candidate ID")
		}
		if _, ok := mapdata.LookupCompetitiveBossCandidate(candidateID); !ok {
			return fmt.Errorf("competitive_boss_rewards contains unknown candidate %q", candidateID)
		}
		if _, err := buildCompetitiveBossRewardItems(candidateID, rewards); err != nil {
			return err
		}
	}
	if config.GMHTTPAddress != "" {
		if config.DatabasePath == "" {
			return fmt.Errorf("gm_http_address requires database_path")
		}
		host, _, err := net.SplitHostPort(config.GMHTTPAddress)
		if err != nil {
			return fmt.Errorf("gm_http_address: %w", err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("gm_http_address must bind an explicit IPv4 address")
		}
		if !ip.IsLoopback() && config.GMCredentials == nil {
			return fmt.Errorf("non-loopback gm_http_address requires gm_auth_path with valid credentials")
		}
	}
	if config.DirectoryHall != nil {
		if _, err := config.DirectoryHall.NativeConfig(); err != nil {
			return err
		}
	}
	seenInitialAccounts := make(map[uint32]struct{}, len(config.InitialAccounts))
	for index, account := range config.InitialAccounts {
		if config.DatabasePath == "" || config.PlayerProfile == nil {
			return fmt.Errorf("initial_accounts requires database_path and player_profile")
		}
		if account.UIN == 0 {
			return fmt.Errorf("initial_accounts[%d].uin must be non-zero", index)
		}
		if _, duplicate := seenInitialAccounts[account.UIN]; duplicate {
			return fmt.Errorf("initial_accounts contains duplicate UIN %d", account.UIN)
		}
		seenInitialAccounts[account.UIN] = struct{}{}
		if account.Nickname != nil {
			profile := game.DefaultPlayerProfile()
			profile.Nickname = *account.Nickname
			if err := profile.Validate(); err != nil {
				return fmt.Errorf("initial_accounts[%d].nickname: %w", index, err)
			}
		}
		if account.Gender != nil && *account.Gender > 1 {
			return fmt.Errorf("initial_accounts[%d].gender must be 0 or 1", index)
		}
	}
	seen := make(map[string]struct{})
	var shopListeners []*ListenerConfig
	for index := range config.Listeners {
		listener := &config.Listeners[index]
		listener.Network = strings.ToLower(listener.Network)
		if listener.Network != "tcp" && listener.Network != "udp" {
			return fmt.Errorf("listener %q network must be tcp or udp", listener.Name)
		}
		host, _, err := net.SplitHostPort(listener.Address)
		if err != nil {
			return fmt.Errorf("listener %q address: %w", listener.Name, err)
		}
		ip, parseErr := netip.ParseAddr(host)
		allowUnspecified := config.NetworkSettings.Mode == networksetup.ModeLANHost || config.NetworkSettings.Mode == networksetup.ModeRemoteHost
		if parseErr != nil || !ip.Is4() || (!ip.IsLoopback() && !ip.IsPrivate() && !(allowUnspecified && ip.IsUnspecified())) {
			return fmt.Errorf("listener %q has an address not permitted by network mode %s", listener.Name, config.NetworkSettings.Mode)
		}
		key := listener.Network + "/" + listener.Address
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate listener %s", key)
		}
		seen[key] = struct{}{}
		responseModes := 0
		if listener.Response.AfterReceiveHex != "" {
			responseModes++
		}
		if listener.Response.AfterReceiveFile != "" {
			responseModes++
		}
		if listener.Response.EchoAfterReceive {
			responseModes++
		}
		if listener.Response.QQTLoginSuccess || listener.Response.QQTRoomList || listener.Response.QQTPlayerList || listener.Response.QQTCreateRoomSuccess || listener.Response.QQTStartGameSuccess {
			responseModes++
		}
		if responseModes > 1 {
			return fmt.Errorf("listener %q sets multiple after-receive response modes", listener.Name)
		}
		if listener.Response.QQTLoginSuccess && listener.Network != "tcp" {
			return fmt.Errorf("listener %q qqt_login_success is TCP-only", listener.Name)
		}
		if listener.Response.QQTDirectoryResponse {
			if listener.Network != "tcp" {
				return fmt.Errorf("listener %q qqt_directory_response is TCP-only", listener.Name)
			}
			if config.DirectoryHall == nil {
				return fmt.Errorf("listener %q qqt_directory_response requires directory_hall", listener.Name)
			}
			if listener.Response.MaxSends != 0 {
				return fmt.Errorf("listener %q qqt_directory_response is request-gated and must not set probe-only max_sends", listener.Name)
			}
		}
		if listener.Response.QQTLoginSuccessCopies < 0 || listener.Response.QQTLoginSuccessCopies > 4 {
			return fmt.Errorf("listener %q qqt_login_success_copies must be between 0 and 4", listener.Name)
		}
		if listener.Response.QQTLoginSuccessCopies != 0 && !listener.Response.QQTLoginSuccess {
			return fmt.Errorf("listener %q qqt_login_success_copies requires qqt_login_success", listener.Name)
		}
		if listener.Response.QQTRoomList && listener.Network != "tcp" {
			return fmt.Errorf("listener %q qqt_room_list is TCP-only", listener.Name)
		}
		if listener.Response.QQTPlayerList && listener.Network != "tcp" {
			return fmt.Errorf("listener %q qqt_player_list is TCP-only", listener.Name)
		}
		if listener.Response.QQTCreateRoomSuccess && listener.Network != "tcp" {
			return fmt.Errorf("listener %q qqt_create_room_success is TCP-only", listener.Name)
		}
		if listener.Response.QQTStartGameSuccess && listener.Network != "tcp" {
			return fmt.Errorf("listener %q qqt_start_game_success is TCP-only", listener.Name)
		}
		if listener.Response.gameBeginEnabled() && !listener.Response.QQTStartGameSuccess {
			return fmt.Errorf("listener %q qqt_game_begin requires qqt_start_game_success", listener.Name)
		}
		if listener.Response.QQTGameEventRelay && !listener.Response.gameBeginEnabled() {
			return fmt.Errorf("listener %q qqt_game_event_relay requires qqt_game_begin", listener.Name)
		}
		if listener.Response.QQTShopCatalog && listener.Network != "tcp" {
			return fmt.Errorf("listener %q qqt_shop_catalog is TCP-only", listener.Name)
		}
		if listener.Response.QQTShopType2Session && (!listener.Response.QQTLoginSuccess || listener.Network != "tcp") {
			return fmt.Errorf("listener %q qqt_shop_type2_session requires TCP qqt_login_success", listener.Name)
		}
		if listener.Response.QQTShopCatalog || listener.Response.QQTShopType2Session {
			shopListeners = append(shopListeners, listener)
		}
		if listener.Response.MaxSends < 0 || listener.Response.MaxSends > 1000 {
			return fmt.Errorf("listener %q response max_sends must be between 0 and 1000", listener.Name)
		}
		if listener.Response.AfterReceiveHex != "" {
			response, err := hex.DecodeString(strings.Map(func(r rune) rune {
				if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
					return -1
				}
				return r
			}, listener.Response.AfterReceiveHex))
			if err != nil {
				return fmt.Errorf("listener %q response hex: %w", listener.Name, err)
			}
			listener.response = response
		}
		if listener.Response.AfterReceiveFile != "" {
			responsePath := listener.Response.AfterReceiveFile
			if !filepath.IsAbs(responsePath) {
				responsePath = filepath.Join(configBase, responsePath)
			}
			response, err := os.ReadFile(responsePath)
			if err != nil {
				return fmt.Errorf("listener %q response file: %w", listener.Name, err)
			}
			if len(response) > config.MaxPacketSize {
				return fmt.Errorf("listener %q response file exceeds max_packet_bytes", listener.Name)
			}
			listener.response = response
		}
		if listener.Response.OnConnectHex != "" && listener.Network != "tcp" {
			return fmt.Errorf("listener %q on_connect_hex is TCP-only", listener.Name)
		}
		if listener.Response.OnConnectHex != "" {
			response, err := hex.DecodeString(strings.ReplaceAll(listener.Response.OnConnectHex, " ", ""))
			if err != nil {
				return fmt.Errorf("listener %q on-connect response hex: %w", listener.Name, err)
			}
			if len(response) > config.MaxPacketSize {
				return fmt.Errorf("listener %q on-connect response exceeds max_packet_bytes", listener.Name)
			}
			listener.Response.OnConnectHex = hex.EncodeToString(response)
		}
	}
	if len(shopListeners) != 0 {
		if len(shopListeners) != 1 || !shopListeners[0].Response.QQTShopCatalog || !shopListeners[0].Response.QQTShopType2Session {
			return fmt.Errorf("native shop requires one TCP listener combining type-3 catalog and type-2 session traffic")
		}
		if config.DirectoryHall == nil || config.DirectoryHall.ShopServerID == 0 {
			return fmt.Errorf("native shop listener requires directory_hall shop server configuration")
		}
		_, shopPort, _ := net.SplitHostPort(shopListeners[0].Address)
		if shopPort != fmt.Sprint(config.DirectoryHall.ShopServerPort) {
			return fmt.Errorf("directory_hall shop port %d must match native shop listener port %s", config.DirectoryHall.ShopServerPort, shopPort)
		}
	}
	return nil
}

func (config Config) localLoginConfig() game.LocalLoginConfig {
	if config.PlayerProfile == nil {
		return game.DefaultLocalLoginConfig()
	}
	return config.PlayerProfile.ToClientLoginConfig()
}

func (config Config) seedPlayerProfile() game.PlayerProfile {
	if config.PlayerProfile == nil {
		return game.DefaultPlayerProfile()
	}
	profile := *config.PlayerProfile
	profile.Inventory = append([]game.ItemInfo(nil), profile.Inventory...)
	return profile
}

// playerProfileForUIN keeps the configured seed account byte-for-byte stable
// while proposing protocol PlayerIDs for newly created local accounts.
// The legacy room and battle messages address players by uint16 PlayerID, so
// two SQLite rows that both inherit PlayerID 1 cannot participate in one room.
// Persistence resolves collisions in this modulo preference transactionally.
func (config Config) playerProfileForUIN(uin uint32) game.PlayerProfile {
	profile := config.seedPlayerProfile()
	if uin != 0 && config.SeedUIN != 0 && uin != config.SeedUIN {
		const playerIDSpace = uint64(1<<16 - 1)
		var delta uint64
		if uin >= config.SeedUIN {
			delta = uint64(uin-config.SeedUIN) % playerIDSpace
		} else {
			backward := uint64(config.SeedUIN-uin) % playerIDSpace
			delta = (playerIDSpace - backward) % playerIDSpace
		}
		if delta == 0 {
			delta = 1
		}
		profile.PlayerID = uint16((uint64(profile.PlayerID)-1+delta)%playerIDSpace + 1)
		if profile.PlayerID == config.seedPlayerProfile().PlayerID {
			profile.PlayerID = uint16(uint64(profile.PlayerID)%playerIDSpace + 1)
		}
	}
	return profile
}

func (config Config) initialAccountProfile(account InitialAccountConfig) game.PlayerProfile {
	profile := config.playerProfileForUIN(account.UIN)
	if account.Nickname != nil {
		profile.Nickname = *account.Nickname
	}
	if account.Gender != nil {
		profile.Gender = *account.Gender
	}
	if account.IconID != nil {
		profile.IconID = *account.IconID
	}
	if account.Identity != nil {
		profile.Identity = *account.Identity
	}
	return profile
}

func (config Config) idleTimeout() time.Duration {
	return time.Duration(config.IdleTimeoutMS) * time.Millisecond
}
