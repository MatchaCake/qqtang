package probe

import (
	"context"
	"fmt"
	"strings"

	"qqtang/internal/protocol/game"
)

type loginServiceResult struct {
	beforeResponse       []byte
	beforeResponseResult string
	response             []byte
	result               string
	followUp             []byte
	followUpResult       string
	followUps            []tcpFollowUpPacket
	handled              bool
	keepConnection       bool
	postResponse         func()
}

// handleLoginRequest owns authentication, single-session claiming, profile
// hydration, and lobby presence. It deliberately reports whether the TCP
// connection may remain open because authentication failures close it before
// any of the normal message handlers can run.
func (server *Server) handleLoginRequest(session *connectionSession, connectionID, remote string, data []byte) loginServiceResult {
	loginRequest, loginErr := game.DecodeLocalLoginRequest(data)
	if loginErr != nil {
		return loginServiceResult{keepConnection: true}
	}
	result := loginServiceResult{handled: true, keepConnection: false}
	expectedSectionID := server.gameSectionID
	if expectedSectionID == 0 {
		expectedSectionID = server.config.seedPlayerProfile().SectionID
	}
	if loginRequest.SectionID == 0 || expectedSectionID != 0 && loginRequest.SectionID != expectedSectionID {
		server.log(logEvent{
			Level: "warn", Event: "login_section_rejected", ConnectionID: connectionID,
			AccountID: fmt.Sprint(loginRequest.UIN), Result: fmt.Sprintf("section_%d", loginRequest.SectionID),
			ErrorContext: fmt.Sprintf("game server owns section %d", expectedSectionID),
		})
		return result
	}
	authenticatedByChallenge := server.consumeAccountAuth(remote, loginRequest.UIN)
	resumedFromLease := false
	if authenticatedByChallenge {
		// A fresh password proof supersedes a detached shop-transition lease,
		// but never an actually live TCP owner.
		server.forgetAuthenticatedSession(remote, loginRequest.UIN)
	} else {
		resumedFromLease = server.claimLiveUINFromLease(session, remote, loginRequest.UIN)
	}
	if !authenticatedByChallenge && !resumedFromLease {
		server.log(logEvent{
			Level: "warn", Event: "login_password_rejected", ConnectionID: connectionID,
			AccountID: fmt.Sprint(loginRequest.UIN), Result: "challenge_required",
			ErrorContext: "no password challenge grant or resumable authenticated lease exists for this account and remote host",
		})
		return result
	}
	if !resumedFromLease && !server.claimLiveUIN(session, loginRequest.UIN) {
		server.log(logEvent{
			Level: "warn", Event: "duplicate_login_rejected", ConnectionID: connectionID,
			AccountID: fmt.Sprint(loginRequest.UIN), Result: "single_session_per_uin",
			ErrorContext: "another live game connection already owns this UIN",
		})
		return result
	}
	if resumedFromLease {
		server.log(logEvent{
			Level: "info", Event: "login_authenticated_lease_resumed", ConnectionID: connectionID,
			AccountID: fmt.Sprint(loginRequest.UIN), Result: "shop_return_same_host",
		})
	}

	seedProfile := server.config.playerProfileForUIN(loginRequest.UIN)
	profile := seedProfile
	if server.playerStore != nil {
		players, serviceErr := server.players()
		if serviceErr != nil {
			loginErr = serviceErr
		} else {
			profile, loginErr = players.LoadOrCreate(context.Background(), loginRequest.UIN, profile)
		}
		profileChanged := false
		// Profiles persisted before lobby-player serialization did not have a
		// nickname. Fill only the absent field from the configured seed;
		// existing user-edited names remain authoritative.
		if loginErr == nil && profile.Nickname == "" {
			profile.Nickname = seedProfile.Nickname
			profileChanged = true
		}
		// Room-pool topology belongs to the selected local section, not to a
		// durable character save. Project configured values over old saves.
		if loginErr == nil && (profile.RoomCount != seedProfile.RoomCount || profile.MinimumRoomID != seedProfile.MinimumRoomID) {
			profile.RoomCount = seedProfile.RoomCount
			profile.MinimumRoomID = seedProfile.MinimumRoomID
			profileChanged = true
		}
		if loginErr == nil && profileChanged {
			profile, loginErr = players.UpdateProfile(context.Background(), loginRequest.UIN, func(current *game.PlayerProfile) error {
				if current.Nickname == "" {
					current.Nickname = seedProfile.Nickname
				}
				current.RoomCount = seedProfile.RoomCount
				current.MinimumRoomID = seedProfile.MinimumRoomID
				return nil
			})
		}
	}
	if loginErr != nil {
		server.log(logEvent{Level: "error", Event: "player_profile_load_failed", ConnectionID: connectionID, AccountID: fmt.Sprint(loginRequest.UIN), Result: "sqlite", ErrorContext: loginErr.Error()})
		return result
	}
	// RequestLearnScroll is the canonical ITEM_STATUS_CHANGE (0x0085) path.
	// Repair the normalized server recipe index from status-4 books at login so
	// profiles learned before that mapping was recovered remain usable.
	if server.playerStore != nil && server.combineCatalog != nil {
		players, serviceErr := server.players()
		if serviceErr != nil {
			loginErr = serviceErr
		} else {
			loginErr = players.ReconcileLearnedRecipes(context.Background(), loginRequest.UIN, server.combineCatalog)
		}
		if loginErr != nil {
			server.log(logEvent{Level: "error", Event: "player_recipe_reconcile_failed", ConnectionID: connectionID, AccountID: fmt.Sprint(loginRequest.UIN), Result: "sqlite", ErrorContext: loginErr.Error()})
			return result
		}
	}
	// Section topology belongs to the selected game-server endpoint. Never
	// trust a stale section value loaded from an account save; REQUEST_LOGIN is
	// the client's explicit district selection and was validated above.
	profile.SectionID = loginRequest.SectionID
	if loginRequest.RoleID == 0 {
		loginErr = fmt.Errorf("login role ID must be non-zero")
	} else if server.roleRules != nil {
		loginErr = server.roleRules.ValidateRoomSelection(loginRequest.RoleID, profile.Identity)
	}
	if loginErr != nil {
		server.log(logEvent{Level: "warn", Event: "login_role_rejected", ConnectionID: connectionID, AccountID: fmt.Sprint(loginRequest.UIN), Result: fmt.Sprintf("role_%d", loginRequest.RoleID), ErrorContext: loginErr.Error()})
		return result
	}
	session.replaceProfile(profile)
	session.setSelectedRoleID(loginRequest.RoleID)
	if loginErr = session.Profile.Validate(); loginErr != nil {
		server.log(logEvent{Level: "error", Event: "login_profile_projection_failed", ConnectionID: connectionID, AccountID: fmt.Sprint(loginRequest.UIN), Result: fmt.Sprintf("role_%d", loginRequest.RoleID), ErrorContext: loginErr.Error()})
		return result
	}
	presenceErr := server.worldState().EnterLobby(loginRequest.UIN, session.Profile)
	if presenceErr != nil {
		server.log(logEvent{Level: "error", Event: "lobby_presence_failed", ConnectionID: connectionID, AccountID: fmt.Sprint(loginRequest.UIN), Result: "lobby_state", ErrorContext: presenceErr.Error()})
		return result
	}
	loginConfig := session.Profile.ToClientLoginConfig()
	result.response, _ = game.BuildLocalLoginSuccess(data, loginConfig)
	result.result = "preset"
	if len(result.response) > 0 {
		result.result = "qqt_login_success"
	}
	if session.Profile.KinIndex != 0 {
		kinRestore, restoreErr := game.BuildLocalKinSessionRestore(data, loginRequest.UIN, session.Profile.KinIndex)
		if restoreErr != nil {
			server.log(logEvent{Level: "error", Event: "kin_session_restore_build_failed", ConnectionID: connectionID, AccountID: fmt.Sprint(loginRequest.UIN), Result: fmt.Sprintf("kin_%d", session.Profile.KinIndex), ErrorContext: restoreErr.Error()})
		} else {
			result.followUps = append(result.followUps, tcpFollowUpPacket{data: kinRestore, result: fmt.Sprintf("qqt_kin_session_restore_%d", session.Profile.KinIndex)})
		}
	}
	// The legacy client does not reliably ask for REQUEST_FRIENDS after every
	// process login.  Rehydrate every durable entry here as native 0x00A5
	// notifications so an already-online friend is not mistaken for a deleted
	// relationship merely because no later presence edge occurs.
	if server.playerStore != nil {
		result.followUps = append(result.followUps, server.friendSessionRestorePackets(data, loginRequest.UIN, session, connectionID)...)
	}
	result.keepConnection = true
	result.postResponse = func() {
		server.notifyFriendOwnersStatus(loginRequest.UIN, true, session)
	}
	return result
}

// handleLoginUtilityMessage groups the small login-service requests that are
// valid only when no earlier dispatcher produced a response.
func (server *Server) handleLoginUtilityMessage(session *connectionSession, connectionID string, data []byte) loginServiceResult {
	if logoutRequest, logoutErr := game.DecodeLocalLogoutRequest(data); logoutErr == nil {
		response, logoutErr := game.BuildLocalLogoutSuccess(data)
		if logoutErr == nil {
			logoutErr = server.logoutSession(session, connectionID)
		}
		if logoutErr != nil {
			server.log(logEvent{Level: "warn", Event: "logout_rejected", ConnectionID: connectionID, AccountID: fmt.Sprint(logoutRequest.UIN), MessageID: fmt.Sprintf("0x%04X", game.LogoutCommand), Result: "rejected", ErrorContext: logoutErr.Error()})
			return loginServiceResult{handled: true, keepConnection: true}
		}
		return loginServiceResult{response: response, result: "qqt_logout_success", handled: true, keepConnection: true}
	}

	if configRequest, configErr := game.DecodeLocalConfigFileRequest(data); configErr == nil {
		response, configErr := game.BuildLocalConfigFilesCurrent(data)
		if configErr != nil {
			server.log(logEvent{Level: "warn", Event: "config_file_request_rejected", ConnectionID: connectionID, AccountID: fmt.Sprint(configRequest.UIN), MessageID: fmt.Sprintf("0x%04X", game.GetConfigFileCommand), Result: "rejected", ErrorContext: configErr.Error()})
			return loginServiceResult{handled: true, keepConnection: true}
		}
		fileIDs := make([]string, 0, len(configRequest.Files))
		for _, file := range configRequest.Files {
			fileIDs = append(fileIDs, fmt.Sprintf("%d:%d", file.FileID, file.FileVersion))
		}
		server.log(logEvent{
			Level: "info", Event: "config_files_requested", ConnectionID: connectionID,
			AccountID: fmt.Sprint(configRequest.UIN), MessageID: fmt.Sprintf("0x%04X", game.GetConfigFileCommand),
			Result: strings.Join(fileIDs, ","),
		})
		return loginServiceResult{
			response: response, result: fmt.Sprintf("qqt_config_files_current_%s", strings.Join(fileIDs, "_")),
			handled: true, keepConnection: true,
		}
	}

	if result := server.handlePetOperationMessage(session, connectionID, data); result.handled {
		return result
	}
	if result := server.handleFunctionalItemMessage(session, connectionID, data); result.handled {
		return result
	}
	if result := server.handleCraftMessage(session, connectionID, data); result.handled {
		return result
	}
	if result := server.handleRoomInventoryItemMessage(session, connectionID, data); result.handled {
		return result
	}

	if petsRequest, petsErr := game.DecodeLocalPlayerPetsRequest(data); petsErr == nil {
		if session.UIN == 0 || petsRequest.UIN != session.UIN {
			petsErr = fmt.Errorf("pet request UIN %d does not match session UIN %d", petsRequest.UIN, session.UIN)
		} else if server.playerStore == nil {
			petsErr = fmt.Errorf("player store is unavailable")
		}
		var pets []game.PetInfo
		if petsErr == nil {
			players, serviceErr := server.players()
			if serviceErr != nil {
				petsErr = serviceErr
			} else {
				pets, petsErr = players.ListPets(context.Background(), session.UIN)
			}
		}
		var response []byte
		if petsErr == nil {
			pets = server.projectPetsForClient(pets)
			response, petsErr = game.BuildLocalPlayerPetsResponse(data, pets)
		}
		if petsErr != nil {
			server.log(logEvent{Level: "warn", Event: "player_pets_request_rejected", ConnectionID: connectionID, AccountID: fmt.Sprint(petsRequest.UIN), MessageID: fmt.Sprintf("0x%04X", game.PlayerPetsCommand), Result: "rejected", ErrorContext: petsErr.Error()})
			return loginServiceResult{handled: true, keepConnection: true}
		}
		return loginServiceResult{
			response: response, result: fmt.Sprintf("qqt_player_pets_%d", len(pets)),
			handled: true, keepConnection: true,
		}
	}
	return loginServiceResult{keepConnection: true}
}
