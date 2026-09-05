package probe

import (
	"fmt"
	"time"

	"qqtang/internal/game/mapdata"
	"qqtang/internal/protocol/game"
)

const competitiveAirborneInitialDelay = 5 * time.Second

type competitiveAirborneSchedule struct {
	RoomID       uint16
	GameID       uint32
	MapID        uint32
	ArbitratorID uint16
	BossID       string
	Cells        []mapdata.CompetitiveCell
}

func competitiveOverlayHasSceneCapability(overlay mapdata.CompetitiveMatchOverlay, wanted mapdata.CompetitiveBossSceneCapability) bool {
	for _, capability := range overlay.SceneCapabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

// competitiveAirborneInterval is the exact final-client rule-2 formula from
// FUN_005e17c5. It starts at 9000-elapsed/8 milliseconds and bottoms out at
// four seconds. Rule-1 Boss overlays use the same receiver but have no local
// producer, so the server authors those batches with this proven cadence.
func competitiveAirborneInterval(elapsed time.Duration) time.Duration {
	milliseconds := elapsed.Milliseconds()
	interval := int64(9000) - milliseconds/8
	if interval < 4000 {
		interval = 4000
	}
	return time.Duration(interval) * time.Millisecond
}

func competitiveAirborneCounts(elapsed time.Duration, randomValue uint32) (normal, powerful, special int) {
	seconds16 := int(elapsed / (16 * time.Second))
	normal = 5 + seconds16
	if normal > 15 {
		normal = 15
	}
	pop := 1 + seconds16
	if pop > 10 {
		pop = 10
	}
	powerful = pop
	if elapsed < 120*time.Second {
		return normal, powerful, 0
	}
	switch value := randomValue % 1000; {
	case value == 100:
		special = 3
	case value >= 201 && value <= 202:
		special = 2
	case value >= 300 && value <= 304:
		special = 1
	}
	return normal, powerful, special
}

func nextAirborneRandom(state *uint32) uint32 {
	if *state == 0 {
		*state = 0xA341316C
	}
	*state ^= *state << 13
	*state ^= *state >> 17
	*state ^= *state << 5
	return *state
}

func shuffledCompetitiveCells(source []mapdata.CompetitiveCell, seed uint32) []mapdata.CompetitiveCell {
	cells := append([]mapdata.CompetitiveCell(nil), source...)
	state := seed
	for index := len(cells) - 1; index > 0; index-- {
		swap := int(nextAirborneRandom(&state) % uint32(index+1))
		cells[index], cells[swap] = cells[swap], cells[index]
	}
	return cells
}

func buildCompetitiveAirborneBatch(schedule competitiveAirborneSchedule, elapsed time.Duration) (game.DispatchBombData, error) {
	if schedule.ArbitratorID == 0 || len(schedule.Cells) == 0 {
		return game.DispatchBombData{}, fmt.Errorf("competitive airborne schedule has no arbitrator or cells")
	}
	seed := schedule.GameID*0x9E3779B9 ^ schedule.MapID*0x85EBCA6B ^ uint32(elapsed/time.Millisecond)
	randomValue := nextAirborneRandom(&seed)
	normal, powerful, special := competitiveAirborneCounts(elapsed, randomValue)
	cells := shuffledCompetitiveCells(schedule.Cells, nextAirborneRandom(&seed))
	wanted := normal + powerful + special
	if wanted > len(cells) {
		wanted = len(cells)
	}
	bombs := make([]game.DispatchBomb, 0, wanted)
	appendBombs := func(count int, sceneID uint32, property byte) {
		for count > 0 && len(bombs) < wanted {
			cell := cells[len(bombs)]
			bombs = append(bombs, game.DispatchBomb{SceneID: sceneID, Prop: property, Row: cell.Row, Col: cell.Col})
			count--
		}
	}
	// These three native scene/property pairs come directly from
	// FUN_0061988b's NOTIFY_DISPATCH_BOMB producer.
	appendBombs(normal, 1, 2)
	appendBombs(powerful, 2, 5)
	appendBombs(special, 11, 8)
	return game.DispatchBombData{
		PlayerID: schedule.ArbitratorID,
		Time:     uint32(elapsed / time.Millisecond),
		Bombs:    bombs,
	}, nil
}

func (server *Server) scheduleCompetitiveAirborne(schedule competitiveAirborneSchedule) {
	if schedule.RoomID == 0 || schedule.GameID == 0 || schedule.MapID == 0 || schedule.ArbitratorID == 0 || schedule.BossID == "" || len(schedule.Cells) == 0 {
		return
	}
	server.log(logEvent{
		Level: "info", Event: "competitive_airborne_scheduled", RoomID: fmt.Sprint(schedule.RoomID),
		Result: fmt.Sprintf("game_%d_map_%d_boss_%s_cells_%d", schedule.GameID, schedule.MapID, schedule.BossID, len(schedule.Cells)),
	})
	server.runBackground(func() {
		started := time.Now()
		timer := time.NewTimer(competitiveAirborneInitialDelay)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				battle, err := server.competitiveBattle(schedule.GameID)
				if err != nil || battle.IsConcluded() || battle.BossID() != schedule.BossID {
					return
				}
				// Arbitration can move when the original owner disconnects. The
				// periodic server-authored batch must carry the current owner rather
				// than the player cached when the match started.
				currentArbitratorID := battle.ArbitratorPlayerID()
				if currentArbitratorID == 0 {
					return
				}
				elapsed := time.Since(started)
				currentSchedule := schedule
				currentSchedule.ArbitratorID = currentArbitratorID
				batch, buildErr := buildCompetitiveAirborneBatch(currentSchedule, elapsed)
				if buildErr != nil {
					server.log(logEvent{Level: "error", Event: "competitive_airborne_build_failed", RoomID: fmt.Sprint(schedule.RoomID), Result: fmt.Sprintf("game_%d", schedule.GameID), ErrorContext: buildErr.Error()})
					return
				}
				members := server.liveRoomMatchSessions(schedule.RoomID, schedule.GameID)
				if len(members) == 0 {
					return
				}
				for _, member := range members {
					template := member.packetTemplate()
					if len(template) == 0 {
						continue
					}
					packet, packetErr := game.BuildLocalDispatchBombNotify(template, schedule.RoomID, server.nextGameDataSequence(), batch)
					if packetErr != nil {
						server.log(logEvent{Level: "error", Event: "competitive_airborne_notify_failed", ConnectionID: member.connectionID, RoomID: fmt.Sprint(schedule.RoomID), Result: fmt.Sprintf("game_%d", schedule.GameID), ErrorContext: packetErr.Error()})
						continue
					}
					server.writeTCP(member.connection, member.connectionID, member.localAddress, member.remoteAddress, packet, "qqt_competitive_airborne_bomb")
				}
				server.log(logEvent{Level: "info", Event: "competitive_airborne_dispatched", RoomID: fmt.Sprint(schedule.RoomID), Result: fmt.Sprintf("game_%d_bombs_%d_elapsed_ms_%d", schedule.GameID, len(batch.Bombs), batch.Time)})
				timer.Reset(competitiveAirborneInterval(elapsed))
			case <-server.done:
				return
			}
		}
	})
}
