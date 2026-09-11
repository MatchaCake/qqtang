package gm

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"qqtang/internal/accountauth"
	"qqtang/internal/game/equipment"
	"qqtang/internal/game/itemcatalog"
	"qqtang/internal/game/petcatalog"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/application"
	"qqtang/internal/server/persistence"
)

//go:embed web/*
var webFiles embed.FS

type SeedProfile func(uin uint32) game.PlayerProfile

type Server struct {
	store            *persistence.PlayerStore
	players          *application.PlayerService
	items            []itemcatalog.Entry
	itemByID         map[uint32]itemcatalog.Entry
	petCatalog       petcatalog.Catalog
	petByType        map[uint32]petcatalog.Definition
	petCardByType    map[uint32]petcatalog.CardLink
	equipmentCatalog *equipment.Catalog
	clientRoot       string
	seedProfile      SeedProfile
	static           http.Handler
	objectArchive    *itemcatalog.Archive
	imageAliases     map[uint32]itemImageAlias
	imageWarnings    []string
	imageCache       sync.Map
	credentials      *Credentials
	accountOnline    func(uint32) bool
	authCache        sync.Map
}

// ItemImageWarnings reports optional GM-preview resources which were skipped.
// These images are not part of the game protocol and must never prevent the
// authoritative game server from starting after an interrupted extraction or
// an abrupt machine restart damaged one client-side file.
func (server *Server) ItemImageWarnings() []string {
	return append([]string(nil), server.imageWarnings...)
}

type authCacheEntry struct {
	expires time.Time
}

type Option func(*Server) error

func WithCredentials(credentials *Credentials) Option {
	return func(server *Server) error {
		if credentials == nil {
			return nil
		}
		if err := credentials.Validate(); err != nil {
			return err
		}
		server.credentials = credentials
		return nil
	}
}

// WithAccountOnlineCheck installs the authoritative runtime-presence check
// used by destructive account operations. The GM package does not import the
// game server, so the probe layer supplies this read-only boundary.
func WithAccountOnlineCheck(check func(uint32) bool) Option {
	return func(server *Server) error {
		if check == nil {
			return fmt.Errorf("GM account online check is nil")
		}
		server.accountOnline = check
		return nil
	}
}

// WithPlayerService shares the game server's per-account serialization
// boundary with GM mutations. A separate facade over the same SQLite store
// would use different locks and could still race match settlement.
func WithPlayerService(players *application.PlayerService) Option {
	return func(server *Server) error {
		if players == nil {
			return fmt.Errorf("GM player service is nil")
		}
		server.players = players
		return nil
	}
}

type accountSummary struct {
	UIN             uint32 `json:"uin"`
	Nickname        string `json:"nickname"`
	PlayerID        uint16 `json:"player_id"`
	Gender          byte   `json:"gender"`
	Degree          uint16 `json:"degree"`
	Points          uint32 `json:"points"`
	AdventurePoints uint32 `json:"adventure_points"`
	InventoryCount  int    `json:"inventory_count"`
}

type accountDetail struct {
	UIN                uint32                 `json:"uin"`
	Profile            game.PlayerProfile     `json:"profile"`
	InventoryItems     []itemView             `json:"inventory_items"`
	Loadouts           []equipment.Assignment `json:"loadouts"`
	Pets               []game.PetInfo         `json:"pets"`
	PasswordConfigured bool                   `json:"password_configured"`
}

type createAccountRequest struct {
	UIN      uint32 `json:"uin"`
	Nickname string `json:"nickname"`
	Gender   byte   `json:"gender"`
	Password string `json:"password"`
}

type passwordUpdateRequest struct {
	Password string `json:"password"`
}

type updateAccountRequest struct {
	Nickname          *string `json:"nickname"`
	Gender            *byte   `json:"gender"`
	IconID            *byte   `json:"icon_id"`
	Identity          *uint32 `json:"identity"`
	TutorialCompleted *bool   `json:"tutorial_completed"`
	Points            *uint32 `json:"points"`
	Money             *uint32 `json:"money"`
	Degree            *uint16 `json:"degree"`
	AdventurePoints   *uint32 `json:"adventure_points"`
	Wins              *uint32 `json:"wins"`
	Losses            *uint32 `json:"losses"`
	Draws             *uint32 `json:"draws"`
}

type identityOption struct {
	Value uint32 `json:"value"`
	Label string `json:"label"`
}

type inventoryUpdateRequest struct {
	Quantity        uint32 `json:"quantity"`
	Status          byte   `json:"status,omitempty"`
	RoleID          byte   `json:"role_id,omitempty"`
	Effect          byte   `json:"effect,omitempty"`
	Color           byte   `json:"color,omitempty"`
	AvailablePeriod uint32 `json:"available_period,omitempty"`
}

const maxGMInventoryQuantity uint32 = 999

type loadoutUpdateRequest struct {
	RoleID   byte           `json:"role_id"`
	Slot     equipment.Slot `json:"slot"`
	ItemID   uint16         `json:"item_id"`
	Equipped bool           `json:"equipped"`
}

type itemView struct {
	itemcatalog.Entry
	ImageURL          string         `json:"image_url"`
	HasImage          bool           `json:"has_image"`
	ImageSource       string         `json:"image_source"`
	HasAppearance     bool           `json:"has_appearance"`
	AppearanceSource  string         `json:"appearance_source,omitempty"`
	ResourceNamespace string         `json:"resource_namespace"`
	ResourceKey       string         `json:"resource_key"`
	SceneIDCollision  bool           `json:"scene_id_collision"`
	Equipment         bool           `json:"equipment"`
	Slot              equipment.Slot `json:"slot,omitempty"`
	EquipmentID       uint16         `json:"equipment_id,omitempty"`
}

type petCatalogView struct {
	PetTypeID        uint32 `json:"pet_type_id,omitempty"`
	Name             string `json:"name"`
	TypeName         string `json:"type_name,omitempty"`
	Description      string `json:"description,omitempty"`
	CardItemID       uint32 `json:"card_item_id,omitempty"`
	CardResourceID   uint32 `json:"card_resource_id,omitempty"`
	ImageURL         string `json:"image_url,omitempty"`
	HasImage         bool   `json:"has_image"`
	ModelImageID     uint32 `json:"model_image_id,omitempty"`
	Level1ResourceID uint32 `json:"level_1_resource_id,omitempty"`
	Level4ResourceID uint32 `json:"level_4_resource_id,omitempty"`
	Level7ResourceID uint32 `json:"level_7_resource_id,omitempty"`
	Assignable       bool   `json:"assignable"`
	ResourceStatus   string `json:"resource_status"`
	Source           string `json:"source"`
	Reason           string `json:"reason,omitempty"`
}

type cachedItemImage struct {
	data        []byte
	contentType string
}

func New(store *persistence.PlayerStore, items []itemcatalog.Entry, equipmentCatalog *equipment.Catalog, clientRoot string, seedProfile SeedProfile, options ...Option) (*Server, error) {
	if store == nil || seedProfile == nil {
		return nil, fmt.Errorf("GM server requires player store and seed profile factory")
	}
	root, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, fmt.Errorf("open embedded GM web files: %w", err)
	}
	ordered := append([]itemcatalog.Entry(nil), items...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	byID := make(map[uint32]itemcatalog.Entry, len(ordered))
	for _, item := range ordered {
		byID[item.ID] = item
	}
	pets, err := petcatalog.Load(clientRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load client pet catalog: %w", err)
	}
	petByType := make(map[uint32]petcatalog.Definition, len(pets.Definitions))
	for _, definition := range pets.Definitions {
		petByType[definition.PetTypeID] = definition
	}
	petCardByType := make(map[uint32]petcatalog.CardLink, len(pets.CardLinks))
	for _, link := range pets.CardLinks {
		petCardByType[link.PetTypeID] = link
	}
	var objectArchive *itemcatalog.Archive
	objectArchive, archiveErr := itemcatalog.OpenObjectArchive(clientRoot)
	if archiveErr != nil && !errors.Is(archiveErr, os.ErrNotExist) {
		return nil, archiveErr
	}
	players, err := application.NewPlayerService(store, equipmentCatalog)
	if err != nil {
		return nil, fmt.Errorf("create GM player service: %w", err)
	}
	server := &Server{
		store: store, players: players, items: ordered, itemByID: byID, petCatalog: pets, petByType: petByType, petCardByType: petCardByType, equipmentCatalog: equipmentCatalog,
		clientRoot: filepath.Clean(clientRoot), seedProfile: seedProfile,
		static: http.FileServer(http.FS(root)), objectArchive: objectArchive,
	}
	for _, option := range options {
		if option != nil {
			if err := option(server); err != nil {
				return nil, err
			}
		}
	}
	return server, nil
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/gm/api/health", server.handleHealth)
	mux.HandleFunc("/gm/api/progression", server.handleProgression)
	mux.HandleFunc("/gm/api/accounts", server.handleAccounts)
	mux.HandleFunc("/gm/api/accounts/", server.handleAccount)
	mux.HandleFunc("/gm/api/items", server.handleItems)
	mux.HandleFunc("/gm/api/items/", server.handleItemImage)
	mux.HandleFunc("/gm/api/pets", server.handlePets)
	mux.HandleFunc("/gm/api/pet-models/", server.handlePetModelImage)
	mux.HandleFunc("/gm/", server.handleStatic)
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/gm/", http.StatusTemporaryRedirect)
	})
	return server.authentication(server.securityHeaders(mux))
}

func (server *Server) handleProgression(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"identities": []identityOption{
			{Value: uint32(game.IdentityOrdinary), Label: "普通玩家"},
			{Value: uint32(game.IdentityPurpleDiamond), Label: "紫钻贵族"},
		},
		"competitive_ranks": game.CompetitiveRanks(),
		"adventure_ranks":   game.AdventureRanks(),
	})
}

func (server *Server) authentication(next http.Handler) http.Handler {
	if server.credentials == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization := request.Header.Get("Authorization")
		authorizationKey := sha256.Sum256([]byte(authorization))
		if cached, ok := server.authCache.Load(authorizationKey); ok && time.Now().Before(cached.(authCacheEntry).expires) {
			next.ServeHTTP(writer, request)
			return
		}
		username, password, ok := request.BasicAuth()
		if !ok || !server.credentials.Verify(username, password) {
			writer.Header().Set("WWW-Authenticate", `Basic realm="QQTang GM", charset="UTF-8"`)
			writeError(writer, http.StatusUnauthorized, "需要 GM 管理员账号和密码")
			return
		}
		server.authCache.Store(authorizationKey, authCacheEntry{expires: time.Now().Add(10 * time.Minute)})
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		if isMutation(request.Method) && !sameOrigin(request) {
			writeError(writer, http.StatusForbidden, "拒绝跨站修改请求")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func isMutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func sameOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, request.Host)
}

func (server *Server) handleStatic(writer http.ResponseWriter, request *http.Request) {
	// The GM UI is local and small. Revalidate its shell on every navigation so
	// a rebuilt embedded frontend cannot keep referring to an obsolete image
	// revision after resource restoration.
	writer.Header().Set("Cache-Control", "no-cache")
	if request.URL.Path == "/gm/" {
		clone := request.Clone(request.Context())
		clone.URL.Path = "/"
		server.static.ServeHTTP(writer, clone)
		return
	}
	clone := request.Clone(request.Context())
	clone.URL.Path = strings.TrimPrefix(request.URL.Path, "/gm")
	server.static.ServeHTTP(writer, clone)
}

func (server *Server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"ok": true, "service": "QQ堂本地 GM 工具", "item_count": len(server.items),
		"pet_type_count": len(server.petCatalog.Definitions), "unmapped_pet_model_count": len(server.petCatalog.UnmappedModels),
		"time_utc": time.Now().UTC().Format(time.RFC3339),
	})
}

func (server *Server) handleAccounts(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		server.listAccounts(writer, request)
	case http.MethodPost:
		server.createAccount(writer, request)
	default:
		writeMethodNotAllowed(writer, http.MethodGet, http.MethodPost)
	}
}

func (server *Server) listAccounts(writer http.ResponseWriter, request *http.Request) {
	uins, err := server.store.ListUINs(request.Context())
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	accounts := make([]accountSummary, 0, len(uins))
	for _, uin := range uins {
		profile, loadErr := server.store.Load(request.Context(), uin)
		if loadErr != nil {
			writeInternalError(writer, loadErr)
			return
		}
		accounts = append(accounts, summarizeAccount(uin, profile))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"accounts": accounts})
}

func summarizeAccount(uin uint32, profile game.PlayerProfile) accountSummary {
	derivedRank := game.CompetitiveRankForPoints(profile.GameInfo.Point)
	return accountSummary{
		UIN: uin, Nickname: profile.Nickname, PlayerID: profile.PlayerID,
		Gender: profile.Gender,
		Degree: derivedRank.Degree, Points: profile.GameInfo.Point,
		AdventurePoints: profile.GameInfo.ExtPoint, InventoryCount: len(profile.Inventory),
	}
}

func (server *Server) createAccount(writer http.ResponseWriter, request *http.Request) {
	var input createAccountRequest
	if !decodeJSONBody(writer, request, &input) {
		return
	}
	if input.UIN == 0 || input.Gender > 1 {
		writeError(writer, http.StatusBadRequest, "账号必须大于 0，性别只能是 0 或 1")
		return
	}
	nickname := strings.TrimSpace(input.Nickname)
	if nickname == "" {
		writeError(writer, http.StatusBadRequest, "昵称为必填项")
		return
	}
	if err := accountauth.ValidatePassword(input.Password); err != nil {
		writeError(writer, http.StatusBadRequest, "登录密码必须为 6–16 位可打印 ASCII 字符")
		return
	}
	if _, err := server.store.Load(request.Context(), input.UIN); err == nil {
		writeError(writer, http.StatusConflict, "该账号已存在")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeInternalError(writer, err)
		return
	}
	profile := server.seedProfile(input.UIN)
	profile.Nickname = nickname
	profile.Gender = input.Gender
	if err := server.store.CreateAccount(request.Context(), input.UIN, profile, input.Password); err != nil {
		if errors.Is(err, persistence.ErrAccountExists) {
			writeError(writer, http.StatusConflict, "该账号已存在")
			return
		}
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.writeAccount(writer, request.Context(), http.StatusCreated, input.UIN)
}

func (server *Server) handleAccount(writer http.ResponseWriter, request *http.Request) {
	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/gm/api/accounts/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(writer, http.StatusNotFound, "账号路径无效")
		return
	}
	uin64, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || uin64 == 0 {
		writeError(writer, http.StatusBadRequest, "账号 ID 无效")
		return
	}
	uin := uint32(uin64)
	if len(parts) == 1 {
		server.handleAccountProfile(writer, request, uin)
		return
	}
	if len(parts) == 3 && parts[1] == "inventory" {
		itemID64, parseErr := strconv.ParseUint(parts[2], 10, 16)
		if parseErr != nil || itemID64 == 0 {
			writeError(writer, http.StatusBadRequest, "物品 ID 无效")
			return
		}
		server.handleInventoryItem(writer, request, uin, uint16(itemID64))
		return
	}
	if len(parts) == 3 && parts[1] == "pets" {
		petTypeID64, parseErr := strconv.ParseUint(parts[2], 10, 32)
		if parseErr != nil || petTypeID64 == 0 {
			writeError(writer, http.StatusBadRequest, "宠物类型 ID 无效")
			return
		}
		server.handlePetGrant(writer, request, uin, uint32(petTypeID64))
		return
	}
	if len(parts) == 3 && parts[1] == "pet-instances" {
		petID64, parseErr := strconv.ParseUint(parts[2], 10, 32)
		if parseErr != nil || petID64 == 0 {
			writeError(writer, http.StatusBadRequest, "宠物实例 ID 无效")
			return
		}
		server.handlePetInstance(writer, request, uin, uint32(petID64))
		return
	}
	if len(parts) == 2 && parts[1] == "loadouts" {
		server.handleLoadout(writer, request, uin)
		return
	}
	if len(parts) == 2 && parts[1] == "password" {
		server.handlePassword(writer, request, uin)
		return
	}
	writeError(writer, http.StatusNotFound, "未找到 GM 接口")
}

func (server *Server) handlePassword(writer http.ResponseWriter, request *http.Request, uin uint32) {
	if request.Method != http.MethodPut {
		writeMethodNotAllowed(writer, http.MethodPut)
		return
	}
	if _, err := server.store.Load(request.Context(), uin); err != nil {
		writeStoreError(writer, err)
		return
	}
	var input passwordUpdateRequest
	if !decodeJSONBody(writer, request, &input) {
		return
	}
	if err := server.store.SetPassword(request.Context(), uin, input.Password); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.writeAccount(writer, request.Context(), http.StatusOK, uin)
}

func (server *Server) handleAccountProfile(writer http.ResponseWriter, request *http.Request, uin uint32) {
	switch request.Method {
	case http.MethodGet:
		server.writeAccount(writer, request.Context(), http.StatusOK, uin)
	case http.MethodPut:
		var input updateAccountRequest
		if !decodeJSONBody(writer, request, &input) {
			return
		}
		_, err := server.players.UpdateProfile(request.Context(), uin, func(profile *game.PlayerProfile) error {
			return applyAccountUpdate(profile, input)
		})
		if errors.Is(err, sql.ErrNoRows) {
			writeStoreError(writer, err)
			return
		}
		if err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		server.writeAccount(writer, request.Context(), http.StatusOK, uin)
	case http.MethodDelete:
		if server.rejectOnlineDestructiveMutation(writer, uin) {
			return
		}
		deleted, err := server.store.Delete(request.Context(), uin)
		if err != nil {
			writeInternalError(writer, err)
			return
		}
		if !deleted {
			writeError(writer, http.StatusNotFound, "账号不存在")
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	default:
		writeMethodNotAllowed(writer, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func applyAccountUpdate(profile *game.PlayerProfile, input updateAccountRequest) error {
	if input.Nickname != nil {
		profile.Nickname = strings.TrimSpace(*input.Nickname)
	}
	if input.Gender != nil {
		profile.Gender = *input.Gender
	}
	if input.IconID != nil {
		profile.IconID = *input.IconID
	}
	if input.Identity != nil {
		profile.Identity = *input.Identity
	}
	if input.TutorialCompleted != nil {
		profile.TutorialCompleted = *input.TutorialCompleted
	}
	if input.Points != nil {
		profile.GameInfo.Point = *input.Points
	}
	if input.Money != nil {
		profile.GameInfo.Money = *input.Money
	}
	if input.Degree != nil {
		rank, err := game.CompetitiveRankByDegree(*input.Degree)
		if err != nil {
			return err
		}
		if input.Points == nil {
			profile.GameInfo.Point = rank.MinPoints
		} else if profile.GameInfo.Point < rank.MinPoints || profile.GameInfo.Point > rank.MaxPoints {
			return fmt.Errorf("竞技等级 %d 的积分范围是 %d..%d", rank.Degree, rank.MinPoints, rank.MaxPoints)
		}
		profile.GameInfo.Degree = rank.Degree
	} else if input.Points != nil {
		profile.GameInfo = profile.GameInfo.WithDegreeDerivedFromPoints()
	}
	if input.AdventurePoints != nil {
		profile.GameInfo.ExtPoint = *input.AdventurePoints
	}
	if input.Wins != nil {
		profile.GameInfo.WinNum = *input.Wins
	}
	if input.Losses != nil {
		profile.GameInfo.LossNum = *input.Losses
	}
	if input.Draws != nil {
		profile.GameInfo.EqualNum = *input.Draws
	}
	return nil
}

func (server *Server) writeAccount(writer http.ResponseWriter, ctx context.Context, status int, uin uint32) {
	profile, err := server.store.Load(ctx, uin)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	loadouts, err := server.store.LoadEquipment(ctx, uin)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	passwordConfigured, err := server.store.HasPassword(ctx, uin)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	pets, err := server.store.ListPets(ctx, uin)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	inventoryItems := make([]itemView, 0, len(profile.Inventory))
	for _, owned := range profile.Inventory {
		if item, known := server.itemByID[uint32(owned.ItemID)]; known {
			inventoryItems = append(inventoryItems, server.itemView(item, profile.GameInfo.RoleID))
		}
	}
	writeJSON(writer, status, accountDetail{
		UIN: uin, Profile: profile, InventoryItems: inventoryItems,
		Loadouts: loadouts, Pets: pets, PasswordConfigured: passwordConfigured,
	})
}

func (server *Server) handlePetGrant(writer http.ResponseWriter, request *http.Request, uin, petTypeID uint32) {
	if request.Method != http.MethodPut {
		writeMethodNotAllowed(writer, http.MethodPut)
		return
	}
	definition, known := server.petByType[petTypeID]
	if !known || !definition.Assignable {
		writeError(writer, http.StatusBadRequest, "当前客户端 PetCfg.ini 没有该宠物类型，不能安全写入账号")
		return
	}
	if _, err := server.players.GrantPet(request.Context(), uin, petTypeID, definition.Name); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.writeAccount(writer, request.Context(), http.StatusOK, uin)
}

func (server *Server) handlePetInstance(writer http.ResponseWriter, request *http.Request, uin, petID uint32) {
	if request.Method != http.MethodDelete {
		writeMethodNotAllowed(writer, http.MethodDelete)
		return
	}
	if server.rejectOnlineDestructiveMutation(writer, uin) {
		return
	}
	removed, err := server.players.DeletePet(request.Context(), uin, petID)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	if !removed {
		writeError(writer, http.StatusNotFound, "宠物实例不存在")
		return
	}
	server.writeAccount(writer, request.Context(), http.StatusOK, uin)
}

func (server *Server) handleInventoryItem(writer http.ResponseWriter, request *http.Request, uin uint32, itemID uint16) {
	if _, known := server.itemByID[uint32(itemID)]; !known {
		writeError(writer, http.StatusBadRequest, "客户端物品目录中没有该 ID")
		return
	}
	switch request.Method {
	case http.MethodPut:
		var input inventoryUpdateRequest
		if !decodeJSONBody(writer, request, &input) {
			return
		}
		if input.Quantity < 1 || input.Quantity > maxGMInventoryQuantity {
			writeError(writer, http.StatusBadRequest, "道具数量必须在 1 到 999 之间；移除道具请使用移除操作")
			return
		}
		item := game.ItemInfo{
			ItemID: itemID, NumOfItem: input.Quantity, ItemStatus: input.Status,
			ItemRoleID: input.RoleID, ItemEffect: input.Effect, ItemColor: input.Color,
			AvailPeriod: input.AvailablePeriod,
		}
		if err := server.players.SetInventoryItem(request.Context(), uin, item); err != nil {
			writeStoreError(writer, err)
			return
		}
		server.writeAccount(writer, request.Context(), http.StatusOK, uin)
	case http.MethodDelete:
		if server.rejectOnlineDestructiveMutation(writer, uin) {
			return
		}
		if err := server.players.SetInventoryItem(request.Context(), uin, game.ItemInfo{ItemID: itemID}); err != nil {
			writeStoreError(writer, err)
			return
		}
		server.writeAccount(writer, request.Context(), http.StatusOK, uin)
	default:
		writeMethodNotAllowed(writer, http.MethodPut, http.MethodDelete)
	}
}

func (server *Server) rejectOnlineDestructiveMutation(writer http.ResponseWriter, uin uint32) bool {
	if server.accountOnline == nil || !server.accountOnline(uin) {
		return false
	}
	writeError(writer, http.StatusConflict, "账号当前在线，请先让对应客户端正常退出")
	return true
}

func (server *Server) handleLoadout(writer http.ResponseWriter, request *http.Request, uin uint32) {
	if request.Method != http.MethodPut {
		writeMethodNotAllowed(writer, http.MethodPut)
		return
	}
	var input loadoutUpdateRequest
	if !decodeJSONBody(writer, request, &input) {
		return
	}
	definition, known := server.equipmentCatalog.Lookup(input.ItemID)
	if !known || definition.Slot != input.Slot || input.RoleID == 0 {
		writeError(writer, http.StatusBadRequest, "角色、装备槽或装扽物品不匹配")
		return
	}
	if err := server.players.ApplyEquipmentChanges(request.Context(), uin, []equipment.Change{{
		RoleID: input.RoleID, Slot: input.Slot, ItemID: input.ItemID, Equipped: input.Equipped,
	}}); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.writeAccount(writer, request.Context(), http.StatusOK, uin)
}

func (server *Server) handleItems(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	query := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("q")))
	kind := strings.TrimSpace(request.URL.Query().Get("kind"))
	images := strings.TrimSpace(request.URL.Query().Get("images"))
	if images != "" && images != "local" {
		writeError(writer, http.StatusBadRequest, "图片筛选值无效")
		return
	}
	limit := queryInt(request.URL.Query().Get("limit"), 60, 1, 100)
	offset := queryInt(request.URL.Query().Get("offset"), 0, 0, len(server.items))
	preferredRole := byte(queryInt(request.URL.Query().Get("role"), 7, 1, 22))
	filtered := make([]itemView, 0, limit)
	total := 0
	for _, item := range server.items {
		if kind != "" && item.Kind != kind {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(fmt.Sprintf("%d %s %s %s", item.ID, item.Name, item.Description, strings.Join(item.Categories, " "))), query) {
			continue
		}
		if images == "local" {
			if _, hasImage := server.resolveItemImage(item, preferredRole); !hasImage {
				continue
			}
		}
		if total >= offset && len(filtered) < limit {
			filtered = append(filtered, server.itemView(item, preferredRole))
		}
		total++
	}
	kinds := make([]string, 0)
	seenKinds := make(map[string]struct{})
	for _, item := range server.items {
		if _, seen := seenKinds[item.Kind]; seen {
			continue
		}
		seenKinds[item.Kind] = struct{}{}
		kinds = append(kinds, item.Kind)
	}
	sort.Strings(kinds)
	writeJSON(writer, http.StatusOK, map[string]any{"items": filtered, "total": total, "offset": offset, "limit": limit, "kinds": kinds})
}

func (server *Server) handlePets(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	query := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("q")))
	limit := queryInt(request.URL.Query().Get("limit"), 80, 1, 100)
	preferredRole := byte(queryInt(request.URL.Query().Get("role"), 7, 1, 255))
	all := make([]petCatalogView, 0, len(server.petCatalog.Definitions)+len(server.petCatalog.UnmappedModels))
	for _, definition := range server.petCatalog.Definitions {
		view := petCatalogView{
			PetTypeID: definition.PetTypeID, Name: definition.Name, TypeName: definition.Name,
			Level1ResourceID: definition.Level1ResourceID, Level4ResourceID: definition.Level4ResourceID,
			Level7ResourceID: definition.Level7ResourceID, Assignable: definition.Assignable,
			ResourceStatus: definition.ResourceStatus, Source: definition.Source,
		}
		if link, linked := server.petCardByType[definition.PetTypeID]; linked {
			view.Name = link.Name
			view.Description = link.Description
			view.CardItemID = link.ItemID
			view.CardResourceID = link.ResourceID
			view.ImageURL = fmt.Sprintf("/gm/api/items/%d/image", link.ItemID)
			if item, ok := server.itemByID[link.ItemID]; ok {
				_, view.HasImage = server.resolveItemImage(item, preferredRole)
			}
		}
		if !view.HasImage {
			view.ModelImageID = preferredPetModelImage(definition.Level1ResourceID, definition.Level4ResourceID, definition.Level7ResourceID)
			if view.ModelImageID != 0 {
				_, view.HasImage = server.resolvePetModelImage(view.ModelImageID)
				if view.HasImage {
					view.ImageURL = fmt.Sprintf("/gm/api/pet-models/%d/image", view.ModelImageID)
				}
			}
		}
		searchable := fmt.Sprintf("%d %s %s %s %d %d %d %d %d", view.PetTypeID, view.Name, view.TypeName, view.Description,
			view.CardItemID, view.CardResourceID, view.Level1ResourceID, view.Level4ResourceID, view.Level7ResourceID)
		if query == "" || strings.Contains(strings.ToLower(searchable), query) {
			all = append(all, view)
		}
	}
	for _, model := range server.petCatalog.UnmappedModels {
		view := petCatalogView{
			Name: model.Name, Level4ResourceID: model.JuvenileResourceID, Level7ResourceID: model.AdultResourceID,
			Assignable: false, ResourceStatus: model.ResourceStatus, Source: model.Source, Reason: model.Reason,
		}
		view.ModelImageID = preferredPetModelImage(0, model.JuvenileResourceID, model.AdultResourceID)
		if view.ModelImageID != 0 {
			_, view.HasImage = server.resolvePetModelImage(view.ModelImageID)
			if view.HasImage {
				view.ImageURL = fmt.Sprintf("/gm/api/pet-models/%d/image", view.ModelImageID)
			}
		}
		if query == "" || strings.Contains(strings.ToLower(fmt.Sprintf("%s %d %d", view.Name, view.Level4ResourceID, view.Level7ResourceID)), query) {
			all = append(all, view)
		}
	}
	total := len(all)
	if len(all) > limit {
		all = all[:limit]
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"pets": all, "total": total, "limit": limit,
		"linked_card_count": len(server.petCatalog.CardLinks),
		"note":              "宠物卡 ItemID 与宠物实体 PetTypeID 是两套编号；已按原客户端配置关联，没有 PetTypeID 映射的模型只读展示。",
	})
}

func (server *Server) itemView(item itemcatalog.Entry, preferredRole byte) itemView {
	image, hasImage := server.resolveItemImage(item, preferredRole)
	appearance, hasAppearance := server.resolveItemAppearance(item, preferredRole)
	view := itemView{
		Entry: item, ImageURL: fmt.Sprintf("/gm/api/items/%d/image", item.ID), HasImage: hasImage, ImageSource: image.source,
		HasAppearance: hasAppearance, AppearanceSource: appearance.source,
		ResourceNamespace: "account-inventory", ResourceKey: accountResourceKey(item), SceneIDCollision: item.ClientSceneFactorySupported,
	}
	if item.ID <= 0xFFFF {
		if definition, ok := server.equipmentCatalog.Lookup(uint16(item.ID)); ok {
			view.Equipment = true
			view.Slot = definition.Slot
			view.EquipmentID = definition.ID
		}
	}
	return view
}

func (server *Server) handleItemImage(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/gm/api/items/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "image" {
		writeError(writer, http.StatusNotFound, "物品图片路径无效")
		return
	}
	id64, err := strconv.ParseUint(parts[0], 10, 32)
	item, known := server.itemByID[uint32(id64)]
	if err != nil || !known {
		writeError(writer, http.StatusNotFound, "物品不存在")
		return
	}
	preferredRole := byte(queryInt(request.URL.Query().Get("role"), 7, 1, 22))
	appearanceValue := strings.TrimSpace(request.URL.Query().Get("appearance"))
	if appearanceValue != "" && appearanceValue != "0" && appearanceValue != "1" {
		writeError(writer, http.StatusBadRequest, "物品图片展示模式无效")
		return
	}
	appearance := appearanceValue == "1"
	animateValue := strings.TrimSpace(request.URL.Query().Get("animate"))
	if animateValue != "" && animateValue != "0" && animateValue != "1" {
		writeError(writer, http.StatusBadRequest, "物品图片动画模式无效")
		return
	}
	animate := animateValue == "1"
	var imageRef itemImageRef
	var hasImage bool
	if appearance {
		imageRef, hasImage = server.resolveItemAppearance(item, preferredRole)
	}
	if !hasImage {
		imageRef, hasImage = server.resolveItemImage(item, preferredRole)
	}
	server.writeDIMGResponse(writer, fmt.Sprintf("item/%d/%d/%t/%t", item.ID, preferredRole, appearance, animate), imageRef, hasImage, animate, fallbackSVG(item))
}

func (server *Server) handlePetModelImage(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeMethodNotAllowed(writer, http.MethodGet)
		return
	}
	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/gm/api/pet-models/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "image" {
		writeError(writer, http.StatusNotFound, "宠物模型图片路径无效")
		return
	}
	id64, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || id64 == 0 {
		writeError(writer, http.StatusNotFound, "宠物模型不存在")
		return
	}
	modelID := uint32(id64)
	imageRef, hasImage := server.resolvePetModelImage(modelID)
	server.writeDIMGResponse(writer, fmt.Sprintf("pet-model/%d", modelID), imageRef, hasImage, false, fallbackSVG(itemcatalog.Entry{ID: modelID, Kind: "pet-model"}))
}

func (server *Server) writeDIMGResponse(writer http.ResponseWriter, cacheKey string, imageRef itemImageRef, hasImage, animate bool, fallback string) {
	if cached, ok := server.imageCache.Load(cacheKey); ok {
		image := cached.(cachedItemImage)
		writer.Header().Set("Content-Type", image.contentType)
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write(image.data)
		return
	}
	if hasImage {
		data, readErr := server.readItemImage(imageRef)
		if readErr == nil {
			var imageData []byte
			contentType := "image/png"
			var decodeErr error
			if imageRef.tightFrame && animate {
				preview, previewErr := itemcatalog.DIMGToPreview(data)
				decodeErr = previewErr
				if previewErr == nil {
					imageData, contentType = preview.Data, preview.ContentType
				}
			} else if imageRef.tightFrame {
				imageData, decodeErr = itemcatalog.DIMGFrameToPNG(data)
			} else {
				imageData, decodeErr = itemcatalog.DIMGToPNG(data)
			}
			if decodeErr == nil {
				server.imageCache.Store(cacheKey, cachedItemImage{data: imageData, contentType: contentType})
				writer.Header().Set("Content-Type", contentType)
				writer.Header().Set("Cache-Control", "no-store")
				_, _ = writer.Write(imageData)
				return
			}
		}
	}
	writer.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	// A missing resource can be restored while the GM service is running. Never
	// let the browser pin this placeholder at the same image URL.
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(writer, fallback)
}

func fallbackSVG(item itemcatalog.Entry) string {
	label := item.Kind
	if label == "" {
		label = "item"
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="72" height="72" viewBox="0 0 72 72"><rect width="72" height="72" rx="16" fill="#eaf5ff"/><path d="M18 21h36v30H18z" fill="#fff" stroke="#78add5" stroke-width="2"/><text x="36" y="39" text-anchor="middle" font-family="sans-serif" font-size="12" fill="#32678d">%d</text><text x="36" y="63" text-anchor="middle" font-family="sans-serif" font-size="7" fill="#6c8da3">%s</text></svg>`, item.ID, html.EscapeString(label))
}

func queryInt(value string, fallback, minimum, maximum int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	if parsed < minimum {
		return minimum
	}
	if parsed > maximum {
		return maximum
	}
	return parsed
}

func decodeJSONBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, 64*1024)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "请求 JSON 无效："+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func writeStoreError(writer http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(writer, http.StatusNotFound, "账号不存在")
		return
	}
	if errors.Is(err, persistence.ErrPetCapacityReduction) {
		writeError(writer, http.StatusConflict, "现有宠物数量超过移除后的宠物栏上限，请先移除多余宠物")
		return
	}
	if errors.Is(err, persistence.ErrInventoryKindLimitReached) {
		writeError(writer, http.StatusConflict, "背包种类已满，客户端最多容纳 500 种物品")
		return
	}
	writeInternalError(writer, err)
}

func writeInternalError(writer http.ResponseWriter, err error) {
	writeError(writer, http.StatusInternalServerError, "GM 操作失败："+err.Error())
}

func writeMethodNotAllowed(writer http.ResponseWriter, methods ...string) {
	writer.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(writer, http.StatusMethodNotAllowed, "不支持该请求方法")
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
