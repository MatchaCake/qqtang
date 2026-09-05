package battleengine

import (
	"fmt"

	"qqtang/internal/game/mapdata"
)

// NativeSpawnMode chooses between the two placement consumers in the final
// client. Free placement consumes the A/B pools in interleaved file order;
// team placement samples the two pools independently with SpawnSeed.
type NativeSpawnMode byte

const (
	NativeSpawnFree NativeSpawnMode = iota
	NativeSpawnTeams
)

// NativeSpawnModeForMap selects the client placement consumer supported by
// the map data. Older ordinary maps store every spawn in group A and leave
// group B empty; the client uses its single-pool/free placement consumer for
// those maps even when the room uses standard team rules. Maps with both
// pools retain the native two-team consumer unless the room is free-rule.
func NativeSpawnModeForMap(entry mapdata.CompetitiveMap, usesFreeRule bool) NativeSpawnMode {
	if usesFreeRule || len(entry.SpawnGroupB) == 0 {
		return NativeSpawnFree
	}
	return NativeSpawnTeams
}

// ValidateNativeSpawnTopology checks one map/team-layout pairing by invoking
// the same native spawn consumer used by ConfigFromCompetitiveMap. It is used
// before room mutation and while selecting training maps, so unsupported
// standard-team layouts never silently fall back to free-room placement.
func ValidateNativeSpawnTopology(entry mapdata.CompetitiveMap, mode NativeSpawnMode, teamIDs []byte) error {
	participants := make([]Participant, len(teamIDs))
	for index, teamID := range teamIDs {
		participants[index] = Participant{PlayerID: uint16(index + 1), TeamID: teamID}
	}
	_, err := PlaceNativeSpawns(0, mode, competitiveCells(entry.SpawnGroupA), competitiveCells(entry.SpawnGroupB), participants)
	if err != nil {
		return fmt.Errorf("competitive map %d: %w", entry.ID, err)
	}
	return nil
}

// PlaceNativeSpawns reproduces Client+0x1C18E2 (free placement) and the
// Client+0x1C19EE/0x1C1A8C pair used by the two-team placement path.
// Participant order is GAME_BEGIN order. The engine later canonicalizes actors
// by PlayerID without changing the spawn assigned here.
func PlaceNativeSpawns(spawnSeed uint32, mode NativeSpawnMode, groupA, groupB []Cell, participants []Participant) ([]Participant, error) {
	if len(participants) == 0 || len(participants) > MaxParticipants {
		return nil, fmt.Errorf("spawn participant count %d is outside 1..%d", len(participants), MaxParticipants)
	}
	if err := validateSpawnPools(groupA, groupB); err != nil {
		return nil, err
	}
	result := append([]Participant(nil), participants...)
	switch mode {
	case NativeSpawnFree:
		cells := interleaveSpawnPools(groupA, groupB)
		if len(cells) < len(result) {
			return nil, fmt.Errorf("free spawn pool contains %d cells for %d participants", len(cells), len(result))
		}
		for index := range result {
			result[index].Spawn = cells[index]
		}
		return result, nil
	case NativeSpawnTeams:
		if len(result)%2 != 0 {
			return nil, fmt.Errorf("native team placement requires an even participant count, got %d", len(result))
		}
		teamA := result[0].TeamID
		if teamA == 0 {
			return nil, fmt.Errorf("native team placement has zero first team ID")
		}
		var teamB byte
		countA, countB := 0, 0
		for _, participant := range result {
			if participant.TeamID == 0 {
				return nil, fmt.Errorf("native team placement participant %d has zero team ID", participant.PlayerID)
			}
			if participant.TeamID == teamA {
				countA++
				continue
			}
			if teamB == 0 {
				teamB = participant.TeamID
			}
			if participant.TeamID == teamB {
				countB++
				continue
			}
			return nil, fmt.Errorf("native team placement supports two teams, found team %d after %d/%d", participant.TeamID, teamA, teamB)
		}
		if teamB == 0 || countA != countB {
			return nil, fmt.Errorf("native team placement requires two equal teams, got %d and %d participants", countA, countB)
		}
		selectedA, err := selectNativeSpawnCells(spawnSeed, groupA, countA)
		if err != nil {
			return nil, fmt.Errorf("select spawn group A: %w", err)
		}
		selectedB, err := selectNativeSpawnCells(spawnSeed, groupB, countB)
		if err != nil {
			return nil, fmt.Errorf("select spawn group B: %w", err)
		}
		indexA, indexB := 0, 0
		for index := range result {
			if result[index].TeamID == teamA {
				result[index].Spawn = selectedA[indexA]
				indexA++
			} else {
				result[index].Spawn = selectedB[indexB]
				indexB++
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unknown native spawn mode %d", mode)
	}
}

func interleaveSpawnPools(groupA, groupB []Cell) []Cell {
	result := make([]Cell, 0, len(groupA)+len(groupB))
	shared := len(groupA)
	if len(groupB) < shared {
		shared = len(groupB)
	}
	for index := 0; index < shared; index++ {
		result = append(result, groupA[index], groupB[index])
	}
	result = append(result, groupA[shared:]...)
	result = append(result, groupB[shared:]...)
	return result
}

func selectNativeSpawnCells(seed uint32, source []Cell, count int) ([]Cell, error) {
	if count < 0 || count > len(source) {
		return nil, fmt.Errorf("requested %d cells from pool of %d", count, len(source))
	}
	pool := append([]Cell(nil), source...)
	rng := nativeMapRNG{state: seed}
	result := make([]Cell, 0, count)
	remaining := len(pool)
	for len(result) < count {
		index := int(rng.next() % uint32(remaining))
		result = append(result, pool[index])
		remaining--
		pool[index] = pool[remaining]
	}
	return result, nil
}

func validateSpawnPools(groups ...[]Cell) error {
	seen := make(map[Cell]struct{})
	for groupIndex, group := range groups {
		for cellIndex, cell := range group {
			if cell.Row < 0 || cell.Col < 0 {
				return fmt.Errorf("spawn group %d cell %d has negative coordinate %d,%d", groupIndex, cellIndex, cell.Row, cell.Col)
			}
			if _, duplicate := seen[cell]; duplicate {
				return fmt.Errorf("spawn coordinate %d,%d is repeated", cell.Row, cell.Col)
			}
			seen[cell] = struct{}{}
		}
	}
	return nil
}
