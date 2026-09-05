package battleengine

import (
	"reflect"
	"testing"

	"qqtang/internal/game/mapdata"
)

func TestNativeSpawnModeForMapUsesTheMapNativePoolLayout(t *testing.T) {
	singlePool := mapdata.CompetitiveMap{SpawnGroupA: []mapdata.CompetitiveCell{{Row: 1, Col: 1}}}
	if got := NativeSpawnModeForMap(singlePool, false); got != NativeSpawnFree {
		t.Fatalf("single-pool standard map mode = %d, want free placement", got)
	}

	twoPools := mapdata.CompetitiveMap{
		SpawnGroupA: []mapdata.CompetitiveCell{{Row: 1, Col: 1}},
		SpawnGroupB: []mapdata.CompetitiveCell{{Row: 2, Col: 2}},
	}
	if got := NativeSpawnModeForMap(twoPools, false); got != NativeSpawnTeams {
		t.Fatalf("two-pool standard map mode = %d, want team placement", got)
	}
	if got := NativeSpawnModeForMap(twoPools, true); got != NativeSpawnFree {
		t.Fatalf("two-pool free-rule map mode = %d, want free placement", got)
	}
}

func TestNativeSpawnPlacementMatchesFreeAndTeamConsumers(t *testing.T) {
	participants := []Participant{
		{PlayerID: 8, TeamID: 1},
		{PlayerID: 3, TeamID: 2},
		{PlayerID: 7, TeamID: 1},
		{PlayerID: 4, TeamID: 2},
	}
	groupA := []Cell{{Col: 1}, {Col: 2}, {Col: 3}, {Col: 4}}
	groupB := []Cell{{Col: 11}, {Col: 12}, {Col: 13}, {Col: 14}}

	free, err := PlaceNativeSpawns(0x12345678, NativeSpawnFree, groupA, groupB, participants)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := participantSpawns(free), []Cell{{Col: 1}, {Col: 11}, {Col: 2}, {Col: 12}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("free spawns = %v, want %v", got, want)
	}

	teams, err := PlaceNativeSpawns(0x12345678, NativeSpawnTeams, groupA, groupB, participants)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := participantSpawns(teams), []Cell{{Col: 2}, {Col: 12}, {Col: 4}, {Col: 14}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("team spawns = %v, want %v", got, want)
	}
}

func participantSpawns(participants []Participant) []Cell {
	result := make([]Cell, len(participants))
	for index, participant := range participants {
		result[index] = participant.Spawn
	}
	return result
}
