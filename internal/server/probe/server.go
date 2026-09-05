package probe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"qqtang/internal/accountauth"
	"qqtang/internal/clientdata/sceneelement"
	"qqtang/internal/game/battleai"
	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/craftcatalog"
	"qqtang/internal/game/equipment"
	"qqtang/internal/game/functionitem"
	"qqtang/internal/game/itemeffect"
	"qqtang/internal/game/mapdata"
	"qqtang/internal/game/match"
	"qqtang/internal/game/petcatalog"
	"qqtang/internal/game/roledata"
	"qqtang/internal/game/shopcatalog"
	"qqtang/internal/protocol/capture"
	"qqtang/internal/protocol/game"
	"qqtang/internal/server/application"
	gmserver "qqtang/internal/server/gm"
	"qqtang/internal/server/persistence"
)

const (
	localDirectoryRequestCommand uint16 = 0x0133
	localDirectoryRequestRoute   uint16 = 6
	localDirectoryRequestMarker  uint16 = 0xFFFF
	// Verified captures use 0x00FE on the game/TLS probes and 0x00FD on
	// the HTTP fallback. Directory responses use 0x00FE.
	localDirectoryRequestSectionID      uint16 = 0x00FD
	localDirectoryProbeRequestSectionID uint16 = 0x00FE
)

type Server struct {
	config Config

	serverLifecycle
	authenticationState

	gameDataSeq           atomic.Uint32
	capture               *capture.Writer
	mapCatalog            *mapdata.Catalog
	adventureRules        *mapdata.AdventureRules
	roleRules             *roledata.Rules
	equipmentCatalog      *equipment.Catalog
	forgeCatalog          *craftcatalog.Catalog
	combineCatalog        *craftcatalog.CombineCatalog
	itemKindsByID         map[uint16]string
	itemEffectCatalog     *itemeffect.Catalog
	breakEggCatalog       *functionitem.BreakEggCatalog
	shopCatalog           *shopcatalog.Catalog
	playerStore           *persistence.PlayerStore
	playerService         *application.PlayerService
	world                 *application.World
	petCardsByItem        map[uint16]petcatalog.CardLink
	petCardItemByType     map[uint32]uint16
	petInnateSkillsByType map[uint32][]byte
	petSkillBooksByItem   map[uint16]petcatalog.SkillBookLink
	petSkillItemByID      map[byte]uint16
	petFoodsByItem        map[uint16]petcatalog.FoodLink
	petExperienceTable    []uint32
	logFile               *os.File
	logWriter             io.Writer
	logMu                 sync.Mutex
	applicationMu         sync.Mutex
	roomActors            roomActorSystem
	battleMu              sync.RWMutex
	battles               map[uint32]*match.AdventureBattle
	competitiveBattles    map[uint32]*match.CompetitiveBattle
	competitiveAIMu       sync.Mutex
	competitiveAIPolicy   battleengine.Policy
	competitiveAICloser   io.Closer
	competitiveAIRuntime  map[uint32]*liveCompetitiveAIRuntime
	liveMu                sync.RWMutex
	liveSessions          map[net.Conn]*connectionSession
	roomPeerUDPMu         sync.Mutex
	roomPeerUDPEndpoints  map[roomPeerUDPKey]roomPeerUDPEndpoint
	roomPeerUDPPresence   map[uint32]roomPeerUDPEndpoint
	roomPeerRelayStats    map[roomPeerRelayDiagnosticKey]roomPeerRelayDiagnostic
	virtualRoomPeerSeq    map[uint32]uint32
	virtualRoomPeerSendMu map[uint32]*sync.Mutex
	gmHandler             http.Handler
	gmHTTPServer          *http.Server
	directoryHallPayload  []byte
	gameSectionID         uint16
	sessionPath           string
}

type connectionSession struct {
	mu           sync.Mutex
	projectionMu sync.Mutex
	projections  []sessionProjection
	done         chan struct{}
	auxiliary    bool
	continuation bool
	UIN          uint32
	Profile      game.PlayerProfile
	// SelectedRoleID is supplied by REQUEST_LOGIN and subsequently changed by
	// room role-selection messages. It is session state, never account-save
	// state; Profile mirrors it only because legacy wire structures embed the
	// same byte in GAME_INFO.
	SelectedRoleID     byte
	CurrentMapID       uint32
	CurrentGameID      uint32
	CurrentStageGameID uint32
	RoomID             uint16
	// detachedRoomID records a room membership already removed by an
	// authoritative between-stage elimination. The native client consumes its
	// GAME_OVER first and sends REQUEST_LEAVE_ROOM only when it closes that
	// scene; the matching response must therefore be built from that later
	// request instead of being pushed unsolicited.
	detachedRoomID uint16
	worldUIN       uint32
	liveUIN        atomic.Uint32
	liveRoomID     atomic.Uint32
	// livePlayerID is the immutable room-wire identity used while locating a
	// peer. Keeping it atomic avoids re-locking the request actor's session
	// from inside a dispatcher that already owns actor.mu.
	livePlayerID  atomic.Uint32
	liveSectionID atomic.Uint32
	liveKinIndex  atomic.Uint32
	liveRoleID    atomic.Uint32
	// pendingProfile is a lock-free handoff from an auxiliary connection (for
	// example the native shop socket) to the primary game connection.  A
	// dispatcher must never hold one connection's session.mu while waiting for
	// another connection's session.mu: two simultaneous account operations can
	// otherwise form an ABBA deadlock.  The primary applies the newest durable
	// snapshot at the start of its next serialized request.
	pendingProfile       atomic.Pointer[game.PlayerProfile]
	lastPacket           atomic.Value
	connection           net.Conn
	connectionID         string
	localAddress         string
	remoteAddress        string
	openedAt             time.Time
	sendMu               sync.Mutex
	authUIN              uint32
	authNonce            []byte
	authChallengeExpires time.Time
	authNavigationUIN    atomic.Uint32
}

// sessionProjection is a command sent to one connection actor by the room or
// match authority. Cross-player code appends commands to the recipient's
// mailbox; it never waits for the recipient's session mutex. The recipient
// applies the commands at its next serialized request boundary.
type sessionProjection struct {
	kind        sessionProjectionKind
	roomID      uint16
	gameID      uint32
	stageGameID uint32
	mapID       uint32
}

type sessionProjectionKind byte

const (
	sessionProjectionStartMatch sessionProjectionKind = iota + 1
	sessionProjectionAdvanceStage
	sessionProjectionLeaveRoom
)

func (session *connectionSession) selectedRoleID() byte {
	if session == nil {
		return 0
	}
	if session.SelectedRoleID != 0 {
		return session.SelectedRoleID
	}
	return session.Profile.GameInfo.RoleID
}

func (session *connectionSession) setSelectedRoleID(roleID byte) {
	if session == nil {
		return
	}
	session.SelectedRoleID = roleID
	session.Profile.GameInfo.RoleID = roleID
	session.liveRoleID.Store(uint32(roleID))
}

// replaceProfile applies a freshly loaded durable profile without allowing a
// shop, pet, craft, or settlement refresh to roll the online character back
// to the non-persisted zero/default value.
func (session *connectionSession) replaceProfile(profile game.PlayerProfile) {
	if session == nil {
		return
	}
	roleID := session.selectedRoleID()
	session.Profile = profile
	if roleID != 0 {
		session.Profile.GameInfo.RoleID = roleID
		session.SelectedRoleID = roleID
	}
	session.livePlayerID.Store(uint32(session.Profile.PlayerID))
	session.liveSectionID.Store(uint32(session.Profile.SectionID))
	session.liveKinIndex.Store(session.Profile.KinIndex)
	session.liveRoleID.Store(uint32(session.Profile.GameInfo.RoleID))
}

func (session *connectionSession) applyPendingProfile() {
	if session == nil {
		return
	}
	profile := session.pendingProfile.Swap(nil)
	if profile == nil {
		return
	}
	session.replaceProfile(*profile)
}

func (session *connectionSession) enqueueProjection(projection sessionProjection) {
	if session == nil {
		return
	}
	session.projectionMu.Lock()
	session.projections = append(session.projections, projection)
	session.projectionMu.Unlock()
}

// applyPendingProjections runs only while the recipient connection owns
// session.mu. Commands are deliberately small and idempotent: World/Room is
// authoritative, while these fields are merely the native-client routing
// projection of that state.
func (session *connectionSession) applyPendingProjections() {
	if session == nil {
		return
	}
	session.projectionMu.Lock()
	projections := session.projections
	session.projections = nil
	session.projectionMu.Unlock()
	for _, projection := range projections {
		switch projection.kind {
		case sessionProjectionStartMatch:
			if session.liveRoomID.Load() != uint32(projection.roomID) || session.liveUIN.Load() == 0 {
				continue
			}
			session.CurrentGameID = projection.gameID
			session.CurrentStageGameID = projection.stageGameID
			session.CurrentMapID = projection.mapID
		case sessionProjectionAdvanceStage:
			if session.liveRoomID.Load() != uint32(projection.roomID) || session.CurrentGameID != projection.gameID {
				continue
			}
			session.CurrentStageGameID = projection.stageGameID
			session.CurrentMapID = projection.mapID
		case sessionProjectionLeaveRoom:
			if projection.roomID != 0 && session.liveRoomID.Load() != 0 && session.liveRoomID.Load() != uint32(projection.roomID) {
				continue
			}
			session.RoomID = 0
			session.worldUIN = 0
			session.liveRoomID.Store(0)
			session.CurrentGameID = 0
			session.CurrentStageGameID = 0
			session.CurrentMapID = 0
		}
	}
}

// applyOrEnqueueProjection preserves immediate state for an idle connection
// without ever blocking a busy peer. This is the adapter-facing mailbox edge
// of the room actor model.
func (session *connectionSession) applyOrEnqueueProjection(projection sessionProjection) {
	if session == nil {
		return
	}
	if session.mu.TryLock() {
		session.enqueueProjection(projection)
		session.applyPendingProjections()
		session.mu.Unlock()
		return
	}
	session.enqueueProjection(projection)
}

// routingPlayerID returns the lock-free identity maintained by setUIN and
// replaceProfile.  The TryLock fallback exists only for small focused tests
// that construct sessions directly instead of publishing them through the
// production login path; it never waits on another dispatcher.
func (session *connectionSession) routingPlayerID() uint16 {
	if session == nil {
		return 0
	}
	if playerID := uint16(session.livePlayerID.Load()); playerID != 0 {
		return playerID
	}
	if session.mu.TryLock() {
		playerID := session.Profile.PlayerID
		session.mu.Unlock()
		return playerID
	}
	return 0
}

func (session *connectionSession) routingSectionID() uint16 {
	if session == nil {
		return 0
	}
	if sectionID := uint16(session.liveSectionID.Load()); sectionID != 0 {
		return sectionID
	}
	if session.mu.TryLock() {
		sectionID := session.Profile.SectionID
		session.mu.Unlock()
		return sectionID
	}
	return 0
}

func (session *connectionSession) routingKinIndex() uint32 {
	if session == nil {
		return 0
	}
	if kinIndex := session.liveKinIndex.Load(); kinIndex != 0 {
		return kinIndex
	}
	if session.mu.TryLock() {
		kinIndex := session.Profile.KinIndex
		session.mu.Unlock()
		return kinIndex
	}
	return 0
}

// routingRoleID is the lock-free selected-character projection. Role choice
// is intentionally not persisted, but lobby and friend lists need it to
// resolve role-scoped external equipment for other online players.
func (session *connectionSession) routingRoleID() byte {
	if session == nil {
		return 0
	}
	if roleID := byte(session.liveRoleID.Load()); roleID != 0 {
		return roleID
	}
	if session.mu.TryLock() {
		roleID := session.selectedRoleID()
		session.mu.Unlock()
		return roleID
	}
	return 0
}

// gameplayGameID is the per-stage identity embedded in QQT_DATA_PACKAGE.
// Adventure keeps CurrentGameID stable for the whole room match, while the
// native client requires a fresh wire identity after every NEXT_MAP. Older
// leases and competitive matches legitimately have no separate stage value.
func (session *connectionSession) gameplayGameID() uint32 {
	if session != nil && session.CurrentStageGameID != 0 {
		return session.CurrentStageGameID
	}
	if session == nil {
		return 0
	}
	return session.CurrentGameID
}

type BoundAddress struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	Address string `json:"address"`
}

type logEvent struct {
	Time          time.Time `json:"time"`
	Level         string    `json:"level"`
	Event         string    `json:"event"`
	ConnectionID  string    `json:"connection_id,omitempty"`
	AccountID     string    `json:"account_id,omitempty"`
	RoomID        string    `json:"room_id,omitempty"`
	MessageID     string    `json:"message_id,omitempty"`
	MessageLength int       `json:"message_length,omitempty"`
	PayloadLength int       `json:"payload_length,omitempty"`
	OuterSequence uint32    `json:"outer_sequence,omitempty"`
	RouteSequence uint16    `json:"route_sequence,omitempty"`
	InnerSequence uint16    `json:"inner_sequence,omitempty"`
	Route         uint16    `json:"route,omitempty"`
	SectionID     uint16    `json:"section_id,omitempty"`
	Result        string    `json:"result,omitempty"`
	ErrorContext  string    `json:"error_context,omitempty"`
	Network       string    `json:"network,omitempty"`
	LocalAddress  string    `json:"local_address,omitempty"`
	RemoteAddress string    `json:"remote_address,omitempty"`
}

func New(config Config) (*Server, error) {
	var (
		competitiveAIPolicy       battleengine.Policy
		competitiveAICloser       io.Closer
		competitiveAIBackend      string
		competitiveAIModelVariant string
	)
	defer func() {
		if competitiveAICloser != nil {
			_ = competitiveAICloser.Close()
		}
	}()
	if config.CompetitiveAI.Enabled {
		loaded, err := battleai.LoadDeploymentPolicy(battleai.DeploymentPolicyConfig{
			Backend:           config.CompetitiveAI.Backend,
			ModelPath:         config.CompetitiveAI.ModelPath,
			MetadataPath:      config.CompetitiveAI.MetadataPath,
			SharedLibraryPath: config.CompetitiveAI.SharedLibraryPath,
			IntraOpThreads:    config.CompetitiveAI.IntraOpThreads,
			InterOpThreads:    config.CompetitiveAI.InterOpThreads,
			NativePolicyConfig: battleai.NativePolicyConfig{
				DangerHorizonMS: 3_500,
				DecisionMS:      config.CompetitiveAI.TickMS,
				EnableSearch:    config.CompetitiveAI.SearchEnabled,
				Search: battleengine.SearchConfig{
					TopK: 4, HorizonMS: battleengine.NativeBombFuseMS + 200, DangerHorizonMS: 3_500,
					PriorWeight: 0.35, EliminationValue: 100, TrapValue: 12,
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("load competitive AI policy: %w", err)
		}
		competitiveAIPolicy = loaded.Policy
		competitiveAICloser = loaded.Closer
		competitiveAIBackend = loaded.Backend
		competitiveAIModelVariant = loaded.Contract.ModelVariant
	}
	resources, err := loadServerStaticResources(config)
	if err != nil {
		return nil, err
	}
	catalog := resources.mapCatalog
	adventureRules := resources.adventureRules
	roleRules := resources.roleRules
	equipmentCatalog := resources.equipmentCatalog
	forgeCatalog := resources.forgeCatalog
	combineCatalog := resources.combineCatalog
	shopCatalog := resources.shopCatalog
	itemEntries := resources.itemEntries
	directoryHallPayload := resources.directoryHallPayload
	gameSectionID := resources.gameSectionID
	playerRuntime, err := loadServerPlayerRuntime(config, resources)
	if err != nil {
		return nil, err
	}
	playerStore, playerService := playerRuntime.store, playerRuntime.service
	timestamp := time.Now().UTC().Format("20060102-150405.000000000")
	sessionPath := filepath.Join(config.CaptureRoot, "session-"+timestamp)
	if err := os.MkdirAll(sessionPath, 0o755); err != nil {
		if playerStore != nil {
			playerStore.Close()
		}
		return nil, fmt.Errorf("create capture session: %w", err)
	}
	captureWriter, err := capture.Open(filepath.Join(sessionPath, "capture.jsonl"))
	if err != nil {
		if playerStore != nil {
			playerStore.Close()
		}
		return nil, err
	}
	server := &Server{
		config: config, capture: captureWriter, logWriter: os.Stdout, sessionPath: sessionPath,
		serverLifecycle:     newServerLifecycle(),
		authenticationState: newAuthenticationState(),
		mapCatalog:          catalog, adventureRules: adventureRules, roleRules: roleRules,
		equipmentCatalog: equipmentCatalog, forgeCatalog: forgeCatalog, combineCatalog: combineCatalog, itemKindsByID: resources.itemKindsByID, itemEffectCatalog: resources.itemEffectCatalog, breakEggCatalog: resources.breakEggCatalog, shopCatalog: shopCatalog, playerStore: playerStore, playerService: playerService,
		petCardsByItem: resources.petCardsByItem, petSkillBooksByItem: resources.petSkillBooksByItem,
		petCardItemByType: resources.petCardItemByType, petInnateSkillsByType: resources.petInnateSkillsByType,
		petSkillItemByID: resources.petSkillItemByID,
		petFoodsByItem:   resources.petFoodsByItem, petExperienceTable: resources.petExperienceTable,
		battles:               make(map[uint32]*match.AdventureBattle),
		competitiveBattles:    make(map[uint32]*match.CompetitiveBattle),
		competitiveAIPolicy:   competitiveAIPolicy,
		competitiveAIRuntime:  make(map[uint32]*liveCompetitiveAIRuntime),
		world:                 application.NewWorld(),
		liveSessions:          make(map[net.Conn]*connectionSession),
		roomPeerUDPEndpoints:  make(map[roomPeerUDPKey]roomPeerUDPEndpoint),
		roomPeerUDPPresence:   make(map[uint32]roomPeerUDPEndpoint),
		roomPeerRelayStats:    make(map[roomPeerRelayDiagnosticKey]roomPeerRelayDiagnostic),
		virtualRoomPeerSeq:    make(map[uint32]uint32),
		virtualRoomPeerSendMu: make(map[uint32]*sync.Mutex),
		directoryHallPayload:  directoryHallPayload,
		gameSectionID:         gameSectionID,
	}
	if config.GMHTTPAddress != "" {
		gm, gmErr := gmserver.New(playerStore, itemEntries, equipmentCatalog, config.ClientRoot, config.playerProfileForUIN,
			gmserver.WithCredentials(config.GMCredentials), gmserver.WithItemImageAliases(config.ItemImageAliasesPath),
			gmserver.WithAccountOnlineCheck(server.accountOnlineForGM), gmserver.WithPlayerService(playerService))
		if gmErr != nil {
			captureWriter.Close()
			if playerStore != nil {
				playerStore.Close()
			}
			return nil, gmErr
		}
		// GM previews are optional administration data, not a prerequisite for
		// directory/game listeners. A full eager validation walks thousands of
		// small client files and used to consume the launcher's whole readiness
		// window on slow VM disks. Explicit aliases are validated while loading;
		// every remaining image is decoded lazily by the HTTP handler and falls
		// back to a placeholder on failure. Keep that work off the core startup
		// path so server_ready reflects network/database readiness only.
		for _, warning := range gm.ItemImageWarnings() {
			server.log(logEvent{Level: "warn", Event: "gm_item_image_alias_skipped", ErrorContext: warning})
		}
		server.gmHandler = gm.Handler()
	}
	if config.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(config.LogFile), 0o755); err != nil {
			captureWriter.Close()
			if playerStore != nil {
				playerStore.Close()
			}
			return nil, fmt.Errorf("create log directory: %w", err)
		}
		server.logFile, err = os.OpenFile(config.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			captureWriter.Close()
			if playerStore != nil {
				playerStore.Close()
			}
			return nil, fmt.Errorf("open log file: %w", err)
		}
		server.logWriter = io.MultiWriter(os.Stdout, server.logFile)
	}
	server.competitiveAICloser = competitiveAICloser
	competitiveAICloser = nil
	if config.CompetitiveAI.Enabled {
		server.log(logEvent{
			Level:        "info",
			Event:        "competitive_ai_backend_loaded",
			Result:       competitiveAIBackend,
			ErrorContext: competitiveAIModelVariant,
		})
	}
	return server, nil
}

func (server *Server) SessionPath() string { return server.sessionPath }

func (server *Server) Addresses() []BoundAddress {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]BoundAddress(nil), server.addresses...)
}

func (server *Server) Start(ctx context.Context) error {
	server.mu.Lock()
	if server.started {
		server.mu.Unlock()
		return fmt.Errorf("server already started")
	}
	if server.closed {
		server.mu.Unlock()
		return fmt.Errorf("server already closed")
	}
	server.started = true
	for _, config := range server.config.Listeners {
		switch config.Network {
		case "tcp":
			listener, err := net.Listen("tcp", config.Address)
			if err != nil {
				server.mu.Unlock()
				_ = server.Close()
				return fmt.Errorf("listen %s/%s: %w", config.Network, config.Address, err)
			}
			server.closers = append(server.closers, listener)
			server.addresses = append(server.addresses, BoundAddress{Name: config.Name, Network: config.Network, Address: listener.Addr().String()})
			server.wg.Add(1)
			go server.serveTCP(ctx, listener, config)
		case "udp":
			address, err := net.ResolveUDPAddr("udp", config.Address)
			if err != nil {
				server.mu.Unlock()
				_ = server.Close()
				return fmt.Errorf("resolve UDP %s: %w", config.Address, err)
			}
			connection, err := net.ListenUDP("udp", address)
			if err != nil {
				server.mu.Unlock()
				_ = server.Close()
				return fmt.Errorf("listen udp/%s: %w", config.Address, err)
			}
			server.closers = append(server.closers, connection)
			server.addresses = append(server.addresses, BoundAddress{Name: config.Name, Network: config.Network, Address: connection.LocalAddr().String()})
			server.wg.Add(1)
			go server.serveUDP(ctx, connection, config)
		}
	}
	if server.gmHandler != nil {
		listener, err := net.Listen("tcp", server.config.GMHTTPAddress)
		if err != nil {
			server.mu.Unlock()
			_ = server.Close()
			return fmt.Errorf("listen GM HTTP/%s: %w", server.config.GMHTTPAddress, err)
		}
		httpServer := &http.Server{
			Handler: server.gmHandler, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		}
		server.gmHTTPServer = httpServer
		server.closers = append(server.closers, listener)
		server.addresses = append(server.addresses, BoundAddress{Name: "gm-http", Network: "http", Address: listener.Addr().String()})
		server.wg.Add(1)
		go func() {
			defer server.wg.Done()
			if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
				server.recordError(fmt.Errorf("serve GM HTTP: %w", err))
			}
		}()
	}
	server.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-server.closeDone:
		}
	}()
	for _, address := range server.addresses {
		server.log(logEvent{Level: "info", Event: "listener_started", Result: address.Name, Network: address.Network, LocalAddress: address.Address})
	}
	return nil
}

func (server *Server) Wait() error {
	server.wg.Wait()
	closeErr := server.Close()
	server.mu.Lock()
	waitErr := server.firstErr
	server.mu.Unlock()
	if waitErr != nil {
		return waitErr
	}
	return closeErr
}

func (server *Server) Close() error {
	server.closeOnce.Do(func() {
		server.mu.Lock()
		server.closed = true
		server.mu.Unlock()
		server.closeErr = server.closeResources()
		close(server.closeDone)
	})
	<-server.closeDone
	return server.closeErr
}

func (server *Server) closeResources() error {
	var first error
	// GM handlers share the player store with match settlement. Stop accepting
	// administrative work and drain active requests before persistence closes.
	if server.gmHTTPServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := server.gmHTTPServer.Shutdown(ctx)
		cancel()
		if err != nil {
			if closeErr := server.gmHTTPServer.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) && first == nil {
				first = closeErr
			}
			if first == nil {
				first = fmt.Errorf("shutdown GM HTTP server: %w", err)
			}
		}
	}
	// Stop accepting room mutations and drain every accepted command before
	// closing the stores and capture writer those commands may still use. This
	// wait deliberately happens without server.mu: command error reporting also
	// uses that mutex, and holding it here would recreate a shutdown deadlock.
	server.roomActors.close()
	if server.done != nil {
		close(server.done)
	}
	for _, closer := range server.closers {
		if err := closer.Close(); err != nil && !errors.Is(err, net.ErrClosed) && first == nil {
			first = err
		}
	}
	// Listener closure prevents new handlers; close the accepted sockets too so
	// blocked reads cannot keep shutdown alive until their normal idle deadline.
	server.liveMu.RLock()
	connections := make([]net.Conn, 0, len(server.liveSessions))
	for connection := range server.liveSessions {
		connections = append(connections, connection)
	}
	server.liveMu.RUnlock()
	for _, connection := range connections {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) && first == nil {
			first = err
		}
	}
	if server.competitiveAICloser != nil {
		if err := server.competitiveAICloser.Close(); err != nil && first == nil {
			first = err
		}
	}
	// Capture, logging and persistence are effects consumed by request and timer
	// goroutines. Keep them open until every producer has observed shutdown and
	// completed its connection-departure cleanup.
	server.wg.Wait()
	if err := server.capture.Close(); err != nil && first == nil {
		first = err
	}
	if server.logFile != nil {
		if err := server.logFile.Close(); err != nil && first == nil {
			first = err
		}
	}
	if server.playerStore != nil {
		if err := server.playerStore.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (server *Server) serveTCP(ctx context.Context, listener net.Listener, config ListenerConfig) {
	defer server.wg.Done()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			server.recordError(fmt.Errorf("accept %s: %w", config.Name, err))
			return
		}
		id := fmt.Sprintf("tcp-%08d", server.connection.Add(1))
		server.wg.Add(1)
		go func() {
			defer server.wg.Done()
			server.handleTCP(ctx, connection, config, id)
		}()
	}
}

func (server *Server) handleTCP(ctx context.Context, connection net.Conn, config ListenerConfig, id string) {
	defer connection.Close()
	local, remote := connection.LocalAddr().String(), connection.RemoteAddr().String()
	session := &connectionSession{Profile: server.config.seedPlayerProfile(), done: make(chan struct{})}
	if !server.registerLiveSession(session, connection, id, local, remote) {
		return
	}
	defer server.unregisterLiveSession(connection)
	server.log(logEvent{Level: "info", Event: "connection_opened", ConnectionID: id, Network: "tcp", LocalAddress: local, RemoteAddress: remote, Result: config.Name})
	defer server.log(logEvent{Level: "info", Event: "connection_closed", ConnectionID: id, Network: "tcp", LocalAddress: local, RemoteAddress: remote, Result: config.Name})
	if config.Response.OnConnectHex != "" {
		response, _ := hex.DecodeString(config.Response.OnConnectHex)
		if server.takeResponse(config) && !server.writeTCP(connection, id, local, remote, response, "on_connect") {
			return
		}
	}
	buffer := make([]byte, server.config.MaxPacketSize)
	var pending []byte
	defer func() {
		close(session.done)
		depart := func() error {
			session.mu.Lock()
			defer session.mu.Unlock()
			server.handleConnectionDeparture(session, id)
			return nil
		}
		roomID := uint16(session.liveRoomID.Load())
		var departureErr error
		if roomID != 0 {
			departureErr = runRoomActor(server, roomID, "tcp-connection-departure", depart)
		} else {
			departureErr = depart()
		}
		if errors.Is(departureErr, errRoomActorShutdown) {
			// A rejected command can observe shutdown while an older accepted
			// command is still draining. Wait for all executors before completing
			// local/profile cleanup outside the permanently closed mailbox.
			server.roomActors.wait()
			departureErr = depart()
		}
		if departureErr != nil {
			server.log(logEvent{Level: "error", Event: "room_actor_command_failed", ConnectionID: id, RoomID: fmt.Sprint(roomID), Result: "tcp_connection_departure", ErrorContext: departureErr.Error()})
		}
	}()
	for {
		if err := connection.SetReadDeadline(time.Now().Add(server.config.idleTimeout())); err != nil {
			server.log(logEvent{Level: "error", Event: "set_deadline_failed", ConnectionID: id, ErrorContext: err.Error()})
			return
		}
		n, err := connection.Read(buffer)
		if n > 0 {
			data := append([]byte(nil), buffer[:n]...)
			if config.Response.dynamicQQTSession() {
				pending = append(pending, data...)
				for len(pending) >= 4 {
					declared := int(binary.BigEndian.Uint32(pending[:4]))
					if declared < 4 || declared > server.config.MaxPacketSize {
						server.log(logEvent{Level: "warn", Event: "tcp_frame_rejected", ConnectionID: id, MessageLength: declared, Result: config.Name, ErrorContext: "declared length is outside 4..max_packet_bytes", Network: "tcp", LocalAddress: local, RemoteAddress: remote})
						return
					}
					if len(pending) < declared {
						break
					}
					frame := append([]byte(nil), pending[:declared]...)
					pending = pending[declared:]
					server.prepareSessionContinuation(config, session, id, frame)
					keepConnection := server.handleTCPMessage(connection, config, session, id, local, remote, frame)
					if !keepConnection {
						return
					}
				}
				if len(pending) > server.config.MaxPacketSize {
					server.log(logEvent{Level: "warn", Event: "tcp_frame_rejected", ConnectionID: id, MessageLength: len(pending), Result: config.Name, ErrorContext: "buffered partial frame exceeds max_packet_bytes", Network: "tcp", LocalAddress: local, RemoteAddress: remote})
					return
				}
			} else {
				server.prepareSessionContinuation(config, session, id, data)
				keepConnection := server.handleTCPMessage(connection, config, session, id, local, remote, data)
				if !keepConnection {
					return
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				server.log(logEvent{Level: "info", Event: "connection_idle_timeout", ConnectionID: id, Result: config.Name})
				return
			}
			server.log(logEvent{Level: "error", Event: "tcp_read_failed", ConnectionID: id, ErrorContext: err.Error()})
			return
		}
	}
}

func (server *Server) handleTCPMessage(connection net.Conn, config ListenerConfig, session *connectionSession, id, local, remote string, data []byte) bool {
	if accountauth.IsProtocolFrame(data) {
		session.mu.Lock()
		session.applyPendingProfile()
		session.applyPendingProjections()
		keepConnection := server.handleAccountAuthMessage(connection, session, id, local, remote, data)
		session.mu.Unlock()
		return keepConnection
	}
	defer func() {
		session.mu.Lock()
		server.rememberAuthenticatedSession(session, remote)
		session.mu.Unlock()
	}()
	server.captureRecord(capture.NewRecord(time.Now(), id, "client_to_server", "tcp", local, remote, data))
	received := logEvent{Level: "info", Event: "message_received", ConnectionID: id, MessageLength: len(data), Result: "captured", Network: "tcp", LocalAddress: local, RemoteAddress: remote}
	if inspection, inspectErr := game.InspectLocalPacket(data); inspectErr == nil {
		received.MessageID = fmt.Sprintf("0x%04X", inspection.Command)
	}
	server.log(received)
	if game.LooksLikeRoomFastPacket(data) {
		type fastResult struct {
			keepConnection bool
			afterUnlock    func()
		}
		dispatch := func() (fastResult, error) {
			session.mu.Lock()
			defer session.mu.Unlock()
			session.applyPendingProfile()
			session.applyPendingProjections()
			keepConnection, afterUnlock := server.handleRoomFastPacketLocked(session, id, data)
			return fastResult{keepConnection: keepConnection, afterUnlock: afterUnlock}, nil
		}
		var dispatched fastResult
		var dispatchErr error
		if roomID := uint16(session.liveRoomID.Load()); roomID != 0 {
			dispatched, dispatchErr = callRoomActor(server, roomID, "tcp-room-fast", dispatch)
		} else {
			dispatched, dispatchErr = dispatch()
		}
		if dispatchErr != nil {
			server.log(logEvent{Level: "error", Event: "room_actor_command_failed", ConnectionID: id, RoomID: fmt.Sprint(session.liveRoomID.Load()), Result: "tcp_room_fast", ErrorContext: dispatchErr.Error()})
			return false
		}
		keepConnection, afterUnlock := dispatched.keepConnection, dispatched.afterUnlock
		if afterUnlock != nil {
			afterUnlock()
		}
		return keepConnection
	}
	// Only ordinary 32-byte-ST packets are usable as templates for later
	// server notifications. Saving a compact room event here would corrupt the
	// next role/team/chat notification generated for this client.
	session.notePacket(data)
	dispatch := func() (tcpDispatchOutcome, error) {
		session.mu.Lock()
		defer session.mu.Unlock()
		session.applyPendingProfile()
		session.applyPendingProjections()
		return server.dispatchTCPMessage(config, session, id, local, remote, data), nil
	}
	var outcome tcpDispatchOutcome
	var dispatchErr error
	if roomID, routed := server.roomActorRouteForTCP(session, data); routed {
		outcome, dispatchErr = callRoomActor(server, roomID, "tcp-room-message", dispatch)
	} else {
		outcome, dispatchErr = dispatch()
	}
	if dispatchErr != nil {
		server.log(logEvent{Level: "error", Event: "room_actor_command_failed", ConnectionID: id, RoomID: fmt.Sprint(session.liveRoomID.Load()), Result: "tcp_room_message", ErrorContext: dispatchErr.Error()})
		return false
	}
	if !outcome.keepConnection {
		return false
	}
	if len(outcome.response) > 0 && outcome.result == "qqt_login_success" {
		copies := config.Response.QQTLoginSuccessCopies
		if copies == 0 {
			copies = 1
		}
		for copyIndex := 1; copyIndex <= copies; copyIndex++ {
			if !server.takeResponse(config) {
				break
			}
			copyResult := outcome.result
			if copies > 1 {
				copyResult = fmt.Sprintf("%s_copy_%d_of_%d", outcome.result, copyIndex, copies)
			}
			if !server.writeTCP(connection, id, local, remote, outcome.response, copyResult) {
				return false
			}
		}
		if len(outcome.followUp) != 0 && !server.writeTCP(connection, id, local, remote, outcome.followUp, outcome.followUpResult) {
			return false
		}
		for _, packet := range outcome.additionalFollowUps {
			if !server.writeTCP(connection, id, local, remote, packet.data, packet.result) {
				return false
			}
		}
		if outcome.postResponse != nil {
			outcome.postResponse()
		}
		return true
	}
	if outcome.requiresOrderedDelivery() {
		return server.deliverTCPSequence(tcpSequenceDelivery{
			connection: connection, config: config, session: session, connectionID: id,
			local: local, remote: remote, request: data,
			beforeResponse: outcome.beforeResponse, beforeResponseResult: outcome.beforeResponseResult,
			response: outcome.response, result: outcome.result, followUp: outcome.followUp, followUpResult: outcome.followUpResult,
			additionalFollowUps: outcome.additionalFollowUps,
			startFollowUp:       outcome.startFollowUp, startFollowUpResult: outcome.startFollowUpResult, startFollowUpDelay: outcome.startFollowUpDelay,
			postResponse:  outcome.postResponse,
			afterFollowUp: outcome.afterFollowUp, afterStartFollowUp: outcome.afterStartFollowUp,
			completeAdventure: outcome.completeAdventureAfterSend, completeCompetitive: outcome.completeCompetitiveAfterSend,
			adventureSettlement: outcome.adventureSettlement, adventureRoomSettlement: outcome.adventureRoomSettlement,
			competitiveRoomSettlement: outcome.competitiveRoomSettlement, finalVictory: outcome.finalVictorySchedule,
		})
	}
	if len(outcome.response) == 0 {
		if outcome.postResponse != nil {
			outcome.postResponse()
		}
		return true
	}
	if !server.takeResponse(config) {
		return true
	}
	if !server.writeTCP(connection, id, local, remote, outcome.response, outcome.result) {
		return false
	}
	if outcome.postResponse != nil {
		outcome.postResponse()
	}
	if outcome.closeAfterResponse {
		server.log(logEvent{
			Level: "info", Event: "shop_auxiliary_response_complete", ConnectionID: id,
			Result: outcome.result, Network: "tcp", LocalAddress: local, RemoteAddress: remote,
		})
		return false
	}
	return true
}

const (
	// A challenge and its proof share one short-lived TCP exchange.
	accountAuthChallengeLifetime = 30 * time.Second
	// The legacy client authenticates before showing the district selector and
	// may remain there while the user decides where to enter. This grant is
	// host-bound and consumed by the first game login, so it can safely outlive
	// the challenge exchange without becoming a reusable login token.
	accountAuthGrantLifetime = 5 * time.Minute
	// District selection is an authenticated account state, but it is not an
	// online game-server session. Keep no profile, room or match snapshot here:
	// a player may remain on the selector for a long time and the selected game
	// server initializes fresh section state only when REQUEST_LOGIN arrives.
	accountAuthNavigationLifetime = 12 * time.Hour
	// A validated district-directory socket keeps the one-time login grant
	// alive while the player is choosing a district. When that socket closes,
	// the legacy client immediately opens its game socket; retain only a short
	// handoff window instead of turning the database verifier into a reusable
	// bearer credential.
)

func (server *Server) handleAccountAuthMessage(connection net.Conn, session *connectionSession, id, local, remote string, frame []byte) bool {
	message, err := accountauth.Unmarshal(frame)
	if err != nil {
		server.log(logEvent{Level: "warn", Event: "account_auth_frame_rejected", ConnectionID: id, Result: "invalid", ErrorContext: err.Error(), Network: "tcp", LocalAddress: local, RemoteAddress: remote})
		return false
	}
	server.log(logEvent{Level: "info", Event: "account_auth_message", ConnectionID: id, AccountID: fmt.Sprint(message.UIN), MessageID: fmt.Sprint(message.Type), MessageLength: len(frame), Result: "received", Network: "tcp", LocalAddress: local, RemoteAddress: remote})
	writeResult := func(accepted bool, reason, nickname string, gender byte) bool {
		response, marshalErr := accountauth.Marshal(accountauth.Message{Type: accountauth.MessageResult, Accepted: accepted, Reason: reason, Nickname: nickname, Gender: gender})
		if marshalErr != nil {
			server.log(logEvent{Level: "error", Event: "account_auth_response_failed", ConnectionID: id, ErrorContext: marshalErr.Error()})
			return false
		}
		return server.writeTCP(connection, id, local, remote, response, "account_auth_result")
	}
	if server.playerStore == nil {
		return writeResult(false, "账号或密码错误", "", 0)
	}
	switch message.Type {
	case accountauth.MessageHello:
		server.revokeAccountAuth(remote, message.UIN)
		iterations, salt, parametersErr := server.playerStore.PasswordParameters(context.Background(), message.UIN)
		if parametersErr != nil {
			return writeResult(false, "账号或密码错误", "", 0)
		}
		nonce := make([]byte, accountauth.NonceSize)
		if _, err := rand.Read(nonce); err != nil {
			server.log(logEvent{Level: "error", Event: "account_auth_nonce_failed", ConnectionID: id, AccountID: fmt.Sprint(message.UIN), ErrorContext: err.Error()})
			return false
		}
		session.authUIN = message.UIN
		session.authNonce = nonce
		session.authChallengeExpires = time.Now().Add(accountAuthChallengeLifetime)
		response, marshalErr := accountauth.Marshal(accountauth.Message{
			Type: accountauth.MessageChallenge, UIN: message.UIN, Iterations: iterations, Salt: salt, Nonce: nonce,
		})
		if marshalErr != nil {
			return false
		}
		return server.writeTCP(connection, id, local, remote, response, "account_auth_challenge")
	case accountauth.MessageProof:
		if session.authUIN == 0 || message.UIN != session.authUIN || len(session.authNonce) != accountauth.NonceSize || time.Now().After(session.authChallengeExpires) {
			session.authUIN = 0
			clear(session.authNonce)
			session.authNonce = nil
			return writeResult(false, "账号或密码错误", "", 0)
		}
		accepted, verifyErr := server.playerStore.VerifyPasswordProof(context.Background(), message.UIN, session.authNonce, message.Proof)
		session.authUIN = 0
		clear(session.authNonce)
		session.authNonce = nil
		if verifyErr != nil || !accepted {
			server.revokeAccountAuth(remote, message.UIN)
			return writeResult(false, "账号或密码错误", "", 0)
		}
		profile, loadErr := server.playerStore.Load(context.Background(), message.UIN)
		if loadErr != nil || profile.Nickname == "" {
			server.revokeAccountAuth(remote, message.UIN)
			if loadErr != nil {
				server.log(logEvent{Level: "error", Event: "account_auth_profile_failed", ConnectionID: id, AccountID: fmt.Sprint(message.UIN), ErrorContext: loadErr.Error()})
			}
			return writeResult(false, "账号资料读取失败", "", 0)
		}
		server.grantAccountAuth(remote, message.UIN)
		server.log(logEvent{Level: "info", Event: "account_auth_accepted", ConnectionID: id, AccountID: fmt.Sprint(message.UIN), Result: "challenge_response"})
		return writeResult(true, "ok", profile.Nickname, profile.Gender)
	default:
		return writeResult(false, "账号或密码错误", "", 0)
	}
}

func (server *Server) grantAccountAuth(remote string, uin uint32) {
	server.grantAccountAuthFor(remote, uin, accountAuthGrantLifetime)
}

func (server *Server) grantAccountAuthFor(remote string, uin uint32, lifetime time.Duration) {
	if server.pendingAuth == nil {
		return
	}
	now := time.Now()
	key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin}
	requestedExpiry := now.Add(lifetime)
	server.authMu.Lock()
	defer server.authMu.Unlock()
	for candidate, expiry := range server.pendingAuth {
		if !now.Before(expiry) {
			delete(server.pendingAuth, candidate)
		}
	}
	if currentExpiry, exists := server.pendingAuth[key]; exists && currentExpiry.After(requestedExpiry) {
		return
	}
	server.pendingAuth[key] = requestedExpiry
}

// attachAccountAuthNavigation binds a directory-selection TCP connection to
// the already verified account grant. The connection itself becomes the
// authenticated navigation capability, so the fixed five-minute grant may
// expire without breaking a user who is still choosing a district.
func (server *Server) attachAccountAuthNavigation(session *connectionSession, remote string, uin uint32) bool {
	if session == nil || uin == 0 {
		return false
	}
	if current := session.authNavigationUIN.Load(); current != 0 {
		if current != uin {
			return false
		}
		server.grantAccountNavigation(remote, uin)
		return true
	}
	if server.pendingAuth != nil {
		key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin}
		now := time.Now()
		server.authMu.Lock()
		expires, ok := server.pendingAuth[key]
		if ok && now.Before(expires) {
			delete(server.pendingAuth, key)
		}
		server.authMu.Unlock()
		if !ok || !now.Before(expires) {
			return false
		}
	}
	if !session.authNavigationUIN.CompareAndSwap(0, uin) && session.authNavigationUIN.Load() != uin {
		return false
	}
	server.grantAccountNavigation(remote, uin)
	return true
}

func (server *Server) handoffAccountAuthNavigation(session *connectionSession) bool {
	if session == nil {
		return false
	}
	uin := session.authNavigationUIN.Swap(0)
	if uin == 0 {
		return false
	}
	server.grantAccountNavigation(session.remoteAddress, uin)
	server.log(logEvent{
		Level: "info", Event: "directory_auth_handoff", ConnectionID: session.connectionID,
		AccountID: fmt.Sprint(uin), Result: "game_login_window_opened",
	})
	return true
}

func (server *Server) consumeAccountAuth(remote string, uin uint32) bool {
	// A nil map is retained only for narrow unit-test Server literals. Every
	// production Server created by New has enforcement enabled.
	if server.pendingAuth == nil {
		return true
	}
	key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin}
	server.authMu.Lock()
	expires, ok := server.pendingAuth[key]
	delete(server.pendingAuth, key)
	server.authMu.Unlock()
	if ok && time.Now().Before(expires) {
		return true
	}
	return server.consumeAccountAuthNavigation(remote, uin)
}

func (server *Server) grantAccountNavigation(remote string, uin uint32) {
	if server.navigationAuth == nil || uin == 0 {
		return
	}
	now := time.Now()
	key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin}
	server.authMu.Lock()
	defer server.authMu.Unlock()
	for candidate, expiry := range server.navigationAuth {
		if !now.Before(expiry) {
			delete(server.navigationAuth, candidate)
		}
	}
	server.navigationAuth[key] = now.Add(accountAuthNavigationLifetime)
}

// consumeAccountAuthNavigation bridges a password-verified district selector
// to the legacy game socket even when the original fixed-duration grant has
// expired. The capability belongs to one live directory connection and is
// cleared atomically on first use, preserving one-time authentication.
func (server *Server) consumeAccountAuthNavigation(remote string, uin uint32) bool {
	if uin == 0 {
		return false
	}
	host := remoteHost(remote)
	key := accountAuthGrant{RemoteHost: host, UIN: uin}
	if server.navigationAuth != nil {
		now := time.Now()
		server.authMu.Lock()
		expires, ok := server.navigationAuth[key]
		delete(server.navigationAuth, key)
		server.authMu.Unlock()
		if ok && now.Before(expires) {
			server.clearLiveAccountNavigation(host, uin)
			return true
		}
	}
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, 1)
	for _, candidate := range server.liveSessions {
		if candidate != nil && remoteHost(candidate.remoteAddress) == host {
			candidates = append(candidates, candidate)
		}
	}
	server.liveMu.RUnlock()
	for _, candidate := range candidates {
		if candidate.authNavigationUIN.CompareAndSwap(uin, 0) {
			return true
		}
	}
	return false
}

func (server *Server) clearLiveAccountNavigation(host string, uin uint32) {
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, 1)
	for _, candidate := range server.liveSessions {
		if candidate != nil && remoteHost(candidate.remoteAddress) == host {
			candidates = append(candidates, candidate)
		}
	}
	server.liveMu.RUnlock()
	for _, candidate := range candidates {
		candidate.authNavigationUIN.CompareAndSwap(uin, 0)
	}
}

func (server *Server) revokeAccountAuth(remote string, uin uint32) {
	if server.pendingAuth == nil {
		return
	}
	server.authMu.Lock()
	key := accountAuthGrant{RemoteHost: remoteHost(remote), UIN: uin}
	delete(server.pendingAuth, key)
	delete(server.navigationAuth, key)
	server.authMu.Unlock()
	server.clearLiveAccountNavigation(key.RemoteHost, uin)
}

func remoteHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

// Adventure is one cooperative side in the native battle scene. Room TermID
// colours remain session-scoped UI choices, but PLAYER_GAME_INFO.TeamID must
// be red/team 1 for every adventure participant: the 5.2 client chooses the
// save (0x0FAA) versus kill (0x0FA8) interaction path from this field before
// the server receives anything.
const adventureCooperativeTeamID byte = 1

func (server *Server) localAdventureParticipants(session *connectionSession) ([]match.AdventureParticipant, error) {
	state, err := server.sessionRoom(session)
	if err != nil {
		return nil, err
	}
	members := state.Snapshot().Members
	extPointsByPlayer := make(map[uint16]uint32, len(members))
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, len(server.liveSessions))
	for _, live := range server.liveSessions {
		if live.liveRoomID.Load() == uint32(session.RoomID) && live.liveUIN.Load() != 0 {
			candidates = append(candidates, live)
		}
	}
	server.liveMu.RUnlock()
	for _, live := range candidates {
		if live == session {
			extPointsByPlayer[live.Profile.PlayerID] = live.Profile.GameInfo.ExtPoint
			continue
		}
		playerID := live.routingPlayerID()
		extPoint := uint32(0)
		matchesRoom := live.liveRoomID.Load() == uint32(session.RoomID) && live.liveUIN.Load() != 0
		if matchesRoom && live.mu.TryLock() {
			extPoint = live.Profile.GameInfo.ExtPoint
			live.mu.Unlock()
		} else if matchesRoom && server.playerStore != nil {
			profile, loadErr := server.playerStore.Load(context.Background(), live.liveUIN.Load())
			if loadErr != nil {
				return nil, fmt.Errorf("load adventure participant %d: %w", playerID, loadErr)
			}
			extPoint = profile.GameInfo.ExtPoint
		} else if matchesRoom {
			return nil, fmt.Errorf("adventure participant %d profile is busy and no durable store is available", playerID)
		}
		if matchesRoom && playerID != 0 {
			extPointsByPlayer[playerID] = extPoint
		}
	}
	participants := make([]match.AdventureParticipant, 0, len(members))
	localPlayerPresent := false
	for _, member := range members {
		roleID := member.RoleID
		if server.roleRules != nil && server.roleRules.IsRandomPlaceholder(roleID) {
			roleID, err = server.roleRules.ResolveForMode(roleID, 0, roledata.RandomModeAdventure)
			if err != nil {
				return nil, fmt.Errorf("resolve random role for player %d: %w", member.PlayerID, err)
			}
		}
		participant := match.AdventureParticipant{
			PlayerID: member.PlayerID,
			RoleID:   roleID,
			TeamID:   adventureCooperativeTeamID,
			ExtPoint: extPointsByPlayer[member.PlayerID],
		}
		if member.PlayerID == session.Profile.PlayerID {
			localPlayerPresent = true
			// The actor is already protected by the caller's session lock and
			// some focused state-machine tests intentionally omit liveSessions.
			// Prefer the authoritative current profile for that one member.
			participant.ExtPoint = session.Profile.GameInfo.ExtPoint
		}
		participants = append(participants, participant)
	}
	if !localPlayerPresent {
		return nil, fmt.Errorf("session player %d is not present in room %d", session.Profile.PlayerID, session.RoomID)
	}
	return participants, nil
}

func (server *Server) localCompetitiveParticipants(session *connectionSession) ([]match.CompetitiveParticipant, error) {
	state, err := server.sessionRoom(session)
	if err != nil {
		return nil, err
	}
	members := state.Snapshot().Members
	pointsByPlayer := make(map[uint16]uint32, len(members))
	bunCardByPlayer := make(map[uint16]bool, len(members))
	server.liveMu.RLock()
	candidates := make([]*connectionSession, 0, len(server.liveSessions))
	for _, live := range server.liveSessions {
		if live.liveRoomID.Load() == uint32(session.RoomID) && live.liveUIN.Load() != 0 {
			candidates = append(candidates, live)
		}
	}
	server.liveMu.RUnlock()
	for _, live := range candidates {
		if live == session {
			pointsByPlayer[live.Profile.PlayerID] = live.Profile.GameInfo.Point
			bunCardByPlayer[live.Profile.PlayerID] = inventoryQuantity(live.Profile.Inventory, 204) > 0
			continue
		}
		playerID := live.routingPlayerID()
		points := uint32(0)
		ownsBunCard := false
		matchesRoom := live.liveRoomID.Load() == uint32(session.RoomID) && live.liveUIN.Load() != 0
		if matchesRoom && live.mu.TryLock() {
			points = live.Profile.GameInfo.Point
			ownsBunCard = inventoryQuantity(live.Profile.Inventory, 204) > 0
			live.mu.Unlock()
		} else if matchesRoom && server.playerStore != nil {
			profile, loadErr := server.playerStore.Load(context.Background(), live.liveUIN.Load())
			if loadErr != nil {
				return nil, fmt.Errorf("load competitive participant %d: %w", playerID, loadErr)
			}
			points = profile.GameInfo.Point
			ownsBunCard = inventoryQuantity(profile.Inventory, 204) > 0
		} else if matchesRoom {
			return nil, fmt.Errorf("competitive participant %d profile is busy and no durable store is available", playerID)
		}
		if matchesRoom && playerID != 0 {
			pointsByPlayer[playerID] = points
			bunCardByPlayer[playerID] = ownsBunCard
		}
	}
	participants := make([]match.CompetitiveParticipant, 0, len(members))
	localPlayerPresent := false
	for _, member := range members {
		roleID := member.RoleID
		if server.roleRules != nil && server.roleRules.IsRandomPlaceholder(roleID) {
			roleID, err = server.roleRules.ResolveCompetitiveWithBunCard(roleID, 0, bunCardByPlayer[member.PlayerID])
			if err != nil {
				return nil, fmt.Errorf("resolve random competitive role for player %d: %w", member.PlayerID, err)
			}
		}
		if member.PlayerID == session.Profile.PlayerID {
			localPlayerPresent = true
		}
		participants = append(participants, match.CompetitiveParticipant{
			PlayerID: member.PlayerID, RoleID: roleID, TeamID: member.TeamID, Point: pointsByPlayer[member.PlayerID],
			Source: match.CompetitiveParticipantHuman,
		})
	}
	if !localPlayerPresent {
		return nil, fmt.Errorf("session player %d is not present in room %d", session.Profile.PlayerID, session.RoomID)
	}
	return participants, nil
}

func (server *Server) adventureStageRule(mapID uint32) (mapdata.AdventureStageRule, bool) {
	if server == nil || server.adventureRules == nil {
		return mapdata.AdventureStageRule{}, false
	}
	return server.adventureRules.Stage(mapID)
}

func adventurePVEBossData(stage mapdata.AdventureStageRule, itemSeed uint32) game.CreatePVENPCBoss {
	bosses := make([]game.PVEBossInfo, 0, len(stage.NPCDropGroups))
	for _, group := range stage.NPCDropGroups {
		selected := stage.SelectNPCDropInventory(group, itemSeed)
		items := make([]game.BossItemInfo, 0, len(selected))
		for _, item := range selected {
			items = append(items, game.BossItemInfo{
				ItemID: item.ItemID, ItemCount: item.Quantity, DropTime: item.DropTime,
			})
		}
		bosses = append(bosses, game.PVEBossInfo{
			BossID: group.BossID, BossCount: group.BossCount, NormalItems: items,
		})
	}
	return game.CreatePVENPCBoss{Bosses: bosses}
}

func adventureWallItemsToWire(stage mapdata.AdventureStageRule, itemSeed uint32) ([]game.GameItemType, error) {
	selected, err := stage.SelectWallItemInventory(itemSeed)
	if err != nil {
		return nil, err
	}
	items := make([]game.GameItemType, 0, len(selected))
	for _, item := range selected {
		if !sceneelement.IsClientID(item.ItemID) {
			return nil, fmt.Errorf("map %d wall material ID %d is not a client scene element", stage.MapID, item.ItemID)
		}
		if item.Quantity > uint32(^uint16(0)>>1) {
			return nil, fmt.Errorf("map %d wall material ID %d quantity %d exceeds GAME_ITEM_TYPE", stage.MapID, item.ItemID, item.Quantity)
		}
		items = append(items, game.GameItemType{ItemID: item.ItemID, Quantity: int16(item.Quantity)})
	}
	return items, nil
}

func adventureNextMapTargetMatches(wireTarget, currentMapID, nextMapID uint32, hasNext bool) bool {
	if wireTarget == game.UnspecifiedNextMapIDWire {
		return true
	}
	if !hasNext {
		return false
	}
	// Some native battlefield files emit the nominal sequential ID even when
	// Continue.ini skips an uninstalled stage. 守望山麓 is the observed case:
	// map 1646 requests 1647, while the authoritative installed route continues
	// at 1648. The outgoing notification still carries nextMapID from the
	// catalog; accept only this immediate placeholder, not arbitrary targets.
	return wireTarget == nextMapID || wireTarget == currentMapID+1
}

func (server *Server) newAdventureGameData(session *connectionSession, stageGameID uint32, selectedMap mapdata.AdventureMap, arbitratorID uint16, participants []match.AdventureParticipant) (game.GameBeginData, error) {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return game.GameBeginData{}, fmt.Errorf("generate adventure seeds: %w", err)
	}
	spawnSeed := binary.BigEndian.Uint32(entropy[0:4])
	itemSeed := binary.BigEndian.Uint32(entropy[4:8])
	if spawnSeed == 0 {
		spawnSeed = 1
	}
	if itemSeed == 0 {
		itemSeed = 1
	}
	var wallItems []game.GameItemType
	if stage, exists := server.adventureStageRule(selectedMap.ID); exists {
		var err error
		wallItems, err = adventureWallItemsToWire(stage, itemSeed)
		if err != nil {
			return game.GameBeginData{}, err
		}
	}
	players := make([]game.PlayerGameInfo, 0, len(participants))
	for _, participant := range participants {
		profile, err := server.adventureParticipantProfile(session, participant.PlayerID)
		if err != nil {
			return game.GameBeginData{}, err
		}
		preparedItems, err := server.preparedAdventureItems(profile, participant.RoleID)
		if err != nil {
			return game.GameBeginData{}, err
		}
		players = append(players, game.PlayerGameInfo{
			PlayerID:  participant.PlayerID,
			RoleID:    participant.RoleID,
			TeamID:    adventureCooperativeTeamID,
			DelayTime: participant.DelayTime,
			ExtPoint:  participant.ExtPoint,
			NewItems:  preparedItems,
		})
	}
	return game.NewAdventureGameBeginData(game.GameBeginOptions{
		GameID: stageGameID, MapID: selectedMap.ID,
		SpawnSeed: spawnSeed, ItemSeed: itemSeed, ArbitratorPlayerID: arbitratorID,
		// The route is already installed in Continue.ini. A positive value and
		// a battlefield MD5 here make QQTSection call CheckContinueFiles and
		// display the obsolete official map-download prompt.
		ContinueID: game.NoRemoteContinueFileID,
		MapHash:    selectedMap.MapHash,
		GameTimeMS: game.DefaultAdventureGameTimeMS,
		Players:    players,
		// GAME_BEGIN.Items drives the ordinary-map timed delivery NPC (the
		// flying duck), so it remains empty in adventure. NewItems contains only
		// the selected route's verified material scene elements; the original
		// client distributes these instances into destructible walls.
		NewItems: wallItems,
	})
}

func (server *Server) adventureParticipantProfile(actor *connectionSession, playerID uint16) (game.PlayerProfile, error) {
	if actor != nil && actor.Profile.PlayerID == playerID {
		return clonePlayerProfile(actor.Profile), nil
	}
	peer := server.liveRoomSessionForPlayer(actor.RoomID, playerID)
	if peer == nil {
		return game.PlayerProfile{}, fmt.Errorf("active adventure participant %d has no live session", playerID)
	}
	if server.playerStore != nil {
		if peer.mu.TryLock() {
			profile := clonePlayerProfile(peer.Profile)
			peer.mu.Unlock()
			return profile, nil
		}
		profile, err := server.playerStore.Load(context.Background(), peer.liveUIN.Load())
		if err != nil {
			return game.PlayerProfile{}, fmt.Errorf("load active adventure participant %d: %w", playerID, err)
		}
		return profile, nil
	}
	if peer.mu.TryLock() {
		defer peer.mu.Unlock()
		return clonePlayerProfile(peer.Profile), nil
	}
	return game.PlayerProfile{}, fmt.Errorf("active adventure participant %d profile is busy", playerID)
}

type adventureDepartureTransition struct {
	ArbitratorPlayerID uint16
	Battle             *match.AdventureBattle
	GameOver           *game.GameOverData
}

// handleAdventureDeparture updates only the battle model.  The room layer
// must remove and project the departing member before finishAdventureDeparture
// sends a GAME_OVER caused by that departure.  Keeping those two phases
// separate prevents a surviving client from settling a scene whose native
// room model still contains the player that just left.
func (server *Server) handleAdventureDeparture(session *connectionSession, connectionID string, reason match.AdventureDepartureReason) adventureDepartureTransition {
	if session == nil || session.CurrentGameID == 0 || session.Profile.PlayerID == 0 {
		return adventureDepartureTransition{}
	}
	battle, err := server.adventureBattle(session.CurrentGameID)
	if err != nil {
		return adventureDepartureTransition{}
	}
	departure, err := battle.RemoveParticipant(session.Profile.PlayerID, reason)
	if err != nil {
		return adventureDepartureTransition{}
	}
	transition := adventureDepartureTransition{ArbitratorPlayerID: departure.ArbitratorPlayerID}
	result := fmt.Sprintf("adventure_player_%d_departed_reason_%d", departure.Participant.PlayerID, reason)
	if departure.NewlyConcluded {
		gameOver, gameOverErr := adventureDepartureGameOverData(battle, departure)
		if gameOverErr != nil {
			server.log(logEvent{Level: "error", Event: "adventure_departure_result_failed", ConnectionID: connectionID, ErrorContext: gameOverErr.Error()})
			return transition
		}
		transition.Battle = battle
		transition.GameOver = &gameOver
		result += "_all_players_eliminated_pending_room_departure"
	}
	if departure.ArbitratorPlayerID != 0 {
		result += fmt.Sprintf("_arbitrator_%d", departure.ArbitratorPlayerID)
	}
	if len(departure.Remaining) == 0 && !departure.NewlyConcluded {
		server.removeAdventureBattle(session.CurrentGameID)
		result += "_battle_removed"
	}
	server.log(logEvent{Level: "info", Event: "adventure_participant_departed", ConnectionID: connectionID, Result: result})
	return transition
}

// finishAdventureDeparture runs only after NOTIFY_LEAVE_ROOM has been
// projected.  The departed account is persisted separately because it is no
// longer a room member; the retained room transaction then covers the peers
// that still have a native room/scene to settle.
func (server *Server) finishAdventureDeparture(session *connectionSession, connectionID string, roomID uint16, gameID uint32, transition adventureDepartureTransition) {
	if session == nil || transition.Battle == nil || transition.GameOver == nil {
		return
	}
	settlement, err := adventureSettlementForPlayer(*transition.GameOver, session.Profile.PlayerID)
	if err == nil {
		err = attachAdventureCollectedItems(transition.Battle, session.Profile.PlayerID, &settlement)
	}
	if err == nil {
		err = server.commitAdventureSettlement(session, settlement, connectionID)
	}
	if err != nil {
		server.log(logEvent{Level: "error", Event: "adventure_departure_actor_settlement_failed", ConnectionID: connectionID, ErrorContext: err.Error()})
	}
	server.broadcastRoomNotification(roomID, session.UIN, "qqt_adventure_game_over_departure_all_players_eliminated", func(recipientPacket []byte) ([]byte, error) {
		return game.BuildLocalGameOverPushForRecipient(recipientPacket, roomID, server.nextGameDataSequence(), *transition.GameOver)
	})
	members := server.liveRoomMatchSessions(roomID, gameID)
	if len(members) == 0 {
		server.removeAdventureBattle(gameID)
		return
	}
	if completeErr := server.completeAdventureRoomMatchOnRoomActor(members[0], connectionID, adventureRoomSettlementCommit{GameOver: *transition.GameOver, Battle: transition.Battle}); completeErr != nil {
		server.log(logEvent{Level: "error", Event: "adventure_departure_completion_failed", ConnectionID: connectionID, ErrorContext: completeErr.Error()})
	}
}

// adventureDepartureGameOverData builds the same loss table used when the
// last avatar dies normally.  A player who explicitly leaves or disconnects
// is still part of this final transaction, just as competitive departure
// settlement records the forfeiting participant before returning the room to
// preparing.
func adventureDepartureGameOverData(battle *match.AdventureBattle, departure match.AdventureDeparture) (game.GameOverData, error) {
	participants := make([]match.AdventureParticipant, 0, len(departure.Remaining)+1)
	participants = append(participants, departure.Participant)
	participants = append(participants, departure.Remaining...)
	results := make([]game.GameResultData, 0, len(participants))
	for _, participant := range participants {
		statistics, err := battle.PlayerStatistics(participant.PlayerID)
		if err != nil {
			return game.GameOverData{}, err
		}
		results = append(results, game.GameResultData{
			PlayerID: participant.PlayerID,
			Result:   game.GameResultLoss,
			Fields: (game.AdventureResultStatistics{
				KillCount: statistics.KillCount, KillScore: statistics.KillScore,
				RescueCount: statistics.RescueCount, RescueScore: statistics.RescueScore,
				RewardCount: statistics.RewardCount, RewardScore: statistics.RewardScore,
			}).GameResultFields(),
		})
	}
	data := game.GameOverData{Results: results, GameMode: game.SettlementGameModeAdventure}
	applyAdventureOutcomeCourageMultiplier(&data)
	return data, nil
}

func (server *Server) writeTCP(connection net.Conn, id, local, remote string, data []byte, result string) bool {
	if session := server.liveSessionForConnection(connection); session != nil {
		session.sendMu.Lock()
		defer session.sendMu.Unlock()
	}
	if err := connection.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		server.log(logEvent{Level: "error", Event: "set_write_deadline_failed", ConnectionID: id, ErrorContext: err.Error()})
		return false
	}
	if _, err := connection.Write(data); err != nil {
		server.log(logEvent{Level: "error", Event: "tcp_write_failed", ConnectionID: id, MessageLength: len(data), ErrorContext: err.Error()})
		return false
	}
	server.captureRecord(capture.NewRecord(time.Now(), id, "server_to_client", "tcp", local, remote, data))
	server.log(logEvent{Level: "info", Event: "message_sent", ConnectionID: id, MessageLength: len(data), Result: result, Network: "tcp", LocalAddress: local, RemoteAddress: remote})
	return true
}

func (server *Server) serveUDP(ctx context.Context, connection *net.UDPConn, config ListenerConfig) {
	defer server.wg.Done()
	buffer := make([]byte, server.config.MaxPacketSize+1)
	for {
		if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			server.recordError(err)
			return
		}
		n, remote, err := connection.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			server.recordError(fmt.Errorf("UDP read %s: %w", config.Name, err))
			return
		}
		id := fmt.Sprintf("udp-%08d", server.connection.Add(1))
		local := connection.LocalAddr().String()
		if n > server.config.MaxPacketSize {
			server.log(logEvent{Level: "warn", Event: "datagram_rejected", ConnectionID: id, MessageLength: n, Result: "over_limit", Network: "udp", LocalAddress: local, RemoteAddress: remote.String()})
			continue
		}
		data := append([]byte(nil), buffer[:n]...)
		// High-FPS room movement is forwarded unchanged but omitted from per-packet
		// diagnostics. Avoid synchronously serializing every per-destination clone
		// on this single receive goroutine; action and unknown datagrams keep their
		// full inbound/outbound evidence.
		if !roomPeerMulticastDatagram(data) {
			server.captureRecord(capture.NewRecord(time.Now(), id, "client_to_server", "udp", local, remote.String(), data))
			server.log(logEvent{Level: "info", Event: "message_received", ConnectionID: id, MessageLength: n, Result: "captured", Network: "udp", LocalAddress: local, RemoteAddress: remote.String()})
		}
		if server.handleLegacyUDPControl(connection, config.Name, id, local, remote, data) {
			continue
		}
		response, result := config.response, "preset"
		if config.Response.EchoAfterReceive {
			response, result = data, "echo"
		}
		if len(response) > 0 && server.takeResponse(config) {
			if _, err := connection.WriteToUDP(response, remote); err != nil {
				server.log(logEvent{Level: "error", Event: "udp_write_failed", ConnectionID: id, ErrorContext: err.Error()})
				continue
			}
			server.captureRecord(capture.NewRecord(time.Now(), id, "server_to_client", "udp", local, remote.String(), response))
			server.log(logEvent{Level: "info", Event: "message_sent", ConnectionID: id, MessageLength: len(response), Result: result, Network: "udp", LocalAddress: local, RemoteAddress: remote.String()})
		}
	}
}

func (server *Server) takeResponse(config ListenerConfig) bool {
	if config.Response.MaxSends == 0 {
		return true
	}
	server.responseMu.Lock()
	defer server.responseMu.Unlock()
	if server.responses[config.Name] >= config.Response.MaxSends {
		return false
	}
	server.responses[config.Name]++
	return true
}

func (server *Server) nextGameDataSequence() uint32 {
	sequence := server.gameDataSeq.Add(1)
	if sequence == 0 {
		sequence = server.gameDataSeq.Add(1)
	}
	return sequence
}

func (server *Server) captureRecord(record capture.Record) {
	if err := server.capture.Write(record); err != nil {
		server.recordError(err)
	}
}

func (server *Server) recordError(err error) {
	server.mu.Lock()
	if server.firstErr == nil {
		server.firstErr = err
	}
	server.mu.Unlock()
	server.log(logEvent{Level: "error", Event: "server_error", ErrorContext: err.Error()})
}

func (server *Server) log(event logEvent) {
	event.Time = time.Now().UTC()
	server.logMu.Lock()
	defer server.logMu.Unlock()
	// Encoder already writes one complete JSON record. A new 4 KiB buffered
	// writer flushed on every line adds an allocation without batching any IO.
	_ = json.NewEncoder(server.logWriter).Encode(event)
}
