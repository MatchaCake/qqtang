package probe

import (
	"fmt"

	"qqtang/internal/game/battleengine"
	"qqtang/internal/game/mapdata"
	"qqtang/internal/game/match"
	"qqtang/internal/game/rolecatalog"
	roomstate "qqtang/internal/game/room"
)

// Keep virtual IDs below the signed-int16 boundary used by several legacy UI
// and gameplay consumers, while remaining disjoint from ordinary room IDs and
// the native NPC namespace (30000+).
const competitiveAIFirstPlayerID uint16 = 20_001

var competitiveAIRoleIDs = [...]uint16{
	1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 14, 15, 16, 18, 19, 20, 21, 22,
}

type competitiveAIFillPlan struct {
	Participants []match.CompetitiveParticipant
}

func (plan competitiveAIFillPlan) TeamIDs() []byte {
	result := make([]byte, len(plan.Participants))
	for index, participant := range plan.Participants {
		result[index] = participant.TeamID
	}
	return result
}

// competitiveMapAllowsControlCardAdmission is the exact client/server Start
// gate union: native ordinary gameplay maps plus every map with a registered
// Boss candidate. The only non-ordinary members in the current final-client
// catalog are football Boss maps 701/702. Admission does not itself activate a
// Boss or authorize AI actors; preparation still decides those in that order.
func competitiveMapAllowsControlCardAdmission(selectedMap mapdata.CompetitiveMap) bool {
	return (selectedMap.NativeRule == 1 && selectedMap.Rule == mapdata.CompetitiveRuleOrdinary) || len(selectedMap.BossCandidates) != 0
}

func competitiveMapSupportsAIFill(selectedMap mapdata.CompetitiveMap) bool {
	if selectedMap.RequiredItemField != 0 || selectedMap.NativeRule != 1 || selectedMap.Rule != mapdata.CompetitiveRuleOrdinary || !selectedMap.UsesOrdinaryElimination() {
		return false
	}
	return battleengine.ValidateNativeRuntimeMap(selectedMap) == nil
}

// planCompetitiveAIFill adds only short-lived rule-1 actors. It deliberately
// refuses Boss overlays, item fields and objective-specific maps before any
// room or match state is changed. The caller resolves Boss activation first.
func planCompetitiveAIFill(snapshot roomstate.Snapshot, selectedMap mapdata.CompetitiveMap, humans []match.CompetitiveParticipant, seed uint64) (competitiveAIFillPlan, error) {
	if len(humans) == 0 {
		return competitiveAIFillPlan{}, fmt.Errorf("competitive AI fill requires at least one human")
	}
	humanTeam := humans[0].TeamID
	if humanTeam == 0 || humanTeam > roomstate.RoomSeatCount {
		return competitiveAIFillPlan{}, fmt.Errorf("human team ID %d is outside 1..%d", humanTeam, roomstate.RoomSeatCount)
	}
	usedPlayerIDs := make(map[uint16]struct{}, len(humans))
	for _, human := range humans {
		if human.TeamID != humanTeam {
			return competitiveAIFillPlan{}, nil
		}
		usedPlayerIDs[human.PlayerID] = struct{}{}
	}
	if !competitiveMapSupportsAIFill(selectedMap) {
		// The card is an optional match overlay. Unsupported and objective maps
		// must retain their normal start path instead of becoming unplayable just
		// because the room owner owns the card.
		return competitiveAIFillPlan{}, nil
	}
	capacity := int(snapshot.Capacity())
	if int(selectedMap.PlayerLimit) < capacity {
		capacity = int(selectedMap.PlayerLimit)
	}
	if battleengine.MaxParticipants < capacity {
		capacity = battleengine.MaxParticipants
	}
	if capacity <= len(humans) {
		return competitiveAIFillPlan{}, nil
	}
	random := competitiveAIPRNG{state: seed ^ uint64(snapshot.RoomID)<<32 ^ uint64(selectedMap.ID)}
	if random.state == 0 {
		random.state = 0x9E3779B97F4A7C15
	}
	var teams []byte
	if snapshot.Properties.UsesFreeRule() {
		virtualCount := capacity - len(humans)
		colors := shuffledCompetitiveAIColors(&random, humanTeam)
		// AI teams in a free room may be grouped for variety, but no generated
		// team may be larger than the largest real-player team that authorized
		// the fill. In the current one-colour entry contract this is exactly the
		// number of humans. This keeps a solo owner against solo AI colours rather
		// than silently creating a two- or three-player AI alliance.
		maximumAITeamSize := len(humans)
		if maximumFill := len(colors) * maximumAITeamSize; virtualCount > maximumFill {
			virtualCount = maximumFill
		}
		minimumEnemyTeams := (virtualCount + maximumAITeamSize - 1) / maximumAITeamSize
		maximumEnemyTeams := virtualCount
		if maximumEnemyTeams > len(colors) {
			maximumEnemyTeams = len(colors)
		}
		enemyTeamCount := minimumEnemyTeams
		if maximumEnemyTeams > minimumEnemyTeams {
			enemyTeamCount += int(random.next() % uint64(maximumEnemyTeams-minimumEnemyTeams+1))
		}
		enemyTeams := colors[:enemyTeamCount]
		teams = make([]byte, 0, virtualCount)
		teamSizes := make(map[byte]int, len(enemyTeams))
		// Install every opposing color first, then randomly distribute remaining
		// actors only among teams that are still below the human-team ceiling.
		teams = append(teams, enemyTeams...)
		for _, teamID := range enemyTeams {
			teamSizes[teamID] = 1
		}
		for len(teams) < virtualCount {
			available := make([]byte, 0, len(enemyTeams))
			for _, teamID := range enemyTeams {
				if teamSizes[teamID] < maximumAITeamSize {
					available = append(available, teamID)
				}
			}
			teamID := available[int(random.next()%uint64(len(available)))]
			teams = append(teams, teamID)
			teamSizes[teamID]++
		}
	} else {
		virtualCount := len(humans)
		if len(humans)+virtualCount > capacity {
			return competitiveAIFillPlan{}, nil
		}
		colors := shuffledCompetitiveAIColors(&random, humanTeam)
		teams = make([]byte, virtualCount)
		for index := range teams {
			teams[index] = colors[0]
		}
	}
	plan := competitiveAIFillPlan{Participants: make([]match.CompetitiveParticipant, 0, len(teams))}
	nextPlayerID := competitiveAIFirstPlayerID
	for _, teamID := range teams {
		for {
			if _, used := usedPlayerIDs[nextPlayerID]; !used {
				break
			}
			nextPlayerID++
			if nextPlayerID == 0 {
				return competitiveAIFillPlan{}, fmt.Errorf("competitive AI player ID space is exhausted")
			}
		}
		roleID := competitiveAIRoleIDs[int(random.next()%uint64(len(competitiveAIRoleIDs)))]
		if _, ok := rolecatalog.PlayableCombatProfile(roleID); !ok {
			return competitiveAIFillPlan{}, fmt.Errorf("competitive AI role %d has no native combat profile", roleID)
		}
		plan.Participants = append(plan.Participants, match.CompetitiveParticipant{
			PlayerID: nextPlayerID, RoleID: byte(roleID), TeamID: teamID,
			Source: match.CompetitiveParticipantVirtualAI,
		})
		usedPlayerIDs[nextPlayerID] = struct{}{}
		nextPlayerID++
	}
	spawnMode := battleengine.NativeSpawnModeForMap(selectedMap, snapshot.Properties.UsesFreeRule())
	allTeamIDs := make([]byte, 0, len(humans)+len(plan.Participants))
	for _, human := range humans {
		allTeamIDs = append(allTeamIDs, human.TeamID)
	}
	allTeamIDs = append(allTeamIDs, plan.TeamIDs()...)
	if err := battleengine.ValidateNativeSpawnTopology(selectedMap, spawnMode, allTeamIDs); err != nil {
		return competitiveAIFillPlan{}, fmt.Errorf("competitive AI spawn topology: %w", err)
	}
	return plan, nil
}

type competitiveAIPRNG struct{ state uint64 }

func (random *competitiveAIPRNG) next() uint64 {
	// SplitMix64 is deterministic across Go releases and sufficient for room
	// layout/appearance selection; combat stochasticity remains in the engine.
	random.state += 0x9E3779B97F4A7C15
	value := random.state
	value = (value ^ (value >> 30)) * 0xBF58476D1CE4E5B9
	value = (value ^ (value >> 27)) * 0x94D049BB133111EB
	return value ^ (value >> 31)
}

func shuffledCompetitiveAIColors(random *competitiveAIPRNG, humanTeam byte) []byte {
	colors := make([]byte, 0, roomstate.RoomSeatCount-1)
	for teamID := byte(1); teamID <= roomstate.RoomSeatCount; teamID++ {
		if teamID != humanTeam {
			colors = append(colors, teamID)
		}
	}
	for index := len(colors) - 1; index > 0; index-- {
		other := int(random.next() % uint64(index+1))
		colors[index], colors[other] = colors[other], colors[index]
	}
	return colors
}
