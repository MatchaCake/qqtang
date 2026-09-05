package battleenv

import (
	"fmt"
	"testing"

	"qqtang/internal/game/battleengine"
)

// This is a control-interface audit, not a new curriculum or a change to the
// native 500 < charge < 600 ms rule. Each trial uses the real Batch.Step path,
// including its legality check, held movement and first-tick placement pulse.
func TestNativePassDecisionCadenceEvidence(t *testing.T) {
	for _, decisionMS := range []uint32{100, 20} {
		t.Run(fmt.Sprintf("decision_%dms", decisionMS), func(t *testing.T) {
			phases := make(map[uint32][2]int)
			for startX := int32(41); startX <= 60; startX++ {
				config := testBatchConfig(1)
				config.DecisionMS = decisionMS
				batch, err := NewBatch(config)
				if err != nil {
					t.Fatal(err)
				}
				engine := batch.episodes[0].engine
				// Only the initial fixture is installed. All contact, charging,
				// activation and traversal below arise from ordinary actions.
				if err := engine.ApplyVerifiedMovementCheckpoint(1, battleengine.Position{X: startX, Y: 60}); err != nil {
					t.Fatal(err)
				}
				if _, err := engine.ApplyVerifiedBombPlacement(2, battleengine.Cell{Row: 1, Col: 2}, 1, 0); err != nil {
					t.Fatal(err)
				}
				origin := engine.ElapsedMS()
				var contact, placedAt uint32
				contactSeen, placed, activated, crossed := false, false, false, false
				for engine.ElapsedMS()-origin < 1_800 {
					self := engine.Actors()[0]
					action := battleengine.ActionMoveRight
					if self.NativePassCollisionValid {
						if !contactSeen {
							contact = self.NativePassCollisionStartedAt
							contactSeen = true
						}
						if !placed && engine.ElapsedMS()-contact > battleengine.NativePassChargeMinMS {
							// If the native window was skipped, still attempt the
							// legal placement-only action; do not bypass the mask.
							action = battleengine.ActionPlaceBomb
							mask, err := engine.LegalActionMask(1)
							if err != nil {
								t.Fatal(err)
							}
							if mask[battleengine.ActionMoveRightAndPlaceBomb] {
								action = battleengine.ActionMoveRightAndPlaceBomb
							}
							placedAt, placed = engine.ElapsedMS(), true
						} else if placed && !activated {
							break
						}
					}
					result, err := batch.Step([][]battleengine.ActionID{{action, battleengine.ActionWait}})
					if err != nil {
						t.Fatalf("start_x=%d time=%d: %v", startX, engine.ElapsedMS(), err)
					}
					for _, event := range result.Events[0] {
						if event.Kind == battleengine.EventNativePassStarted && event.PlayerID == 1 {
							activated = true
						}
					}
					if engine.Actors()[0].Position.Cell().Col > 2 {
						crossed = true
						break
					}
				}
				if !placed {
					t.Fatalf("start_x=%d never reached a placement attempt", startX)
				}
				charge := placedAt - contact
				want := charge > battleengine.NativePassChargeMinMS && charge < battleengine.NativePassChargeMaxMS
				if activated != want || crossed != want {
					t.Fatalf("start_x=%d contact=%d placement=%d charge=%d activation=%v crossing=%v want=%v", startX, contact, placedAt, charge, activated, crossed, want)
				}
				phase := (contact - origin) % 100
				counts := phases[phase]
				counts[0]++
				if crossed {
					counts[1]++
				}
				phases[phase] = counts
			}
			for _, phase := range []uint32{0, 20, 40, 60, 80} {
				counts := phases[phase]
				t.Logf("contact_phase_ms=%d cases=%d crossed=%d", phase, counts[0], counts[1])
				if counts[0] == 0 {
					t.Fatalf("fixture did not cover contact phase %d", phase)
				}
				want := counts[0]
				if decisionMS == 100 && phase == 0 {
					want = 0
				}
				if counts[1] != want {
					t.Fatalf("phase %d crossed %d of %d, want %d", phase, counts[1], counts[0], want)
				}
			}
		})
	}
}

// The half-body/after-impact contrast uses the same 100 ms action interface.
// It shows an executable distinction, not just manually assigned hit states.
func TestHalfBodyThenEnterAfterImpactAtNormalDecisionCadence(t *testing.T) {
	for _, enterAtMS := range []uint32{2_900, 3_100} {
		t.Run(fmt.Sprintf("enter_at_%dms", enterAtMS), func(t *testing.T) {
			batch, err := NewBatch(testBatchConfig(1))
			if err != nil {
				t.Fatal(err)
			}
			engine := batch.episodes[0].engine
			origin := engine.ElapsedMS()
			if err := engine.ApplyVerifiedMovementCheckpoint(1, battleengine.Position{X: 100, Y: 100}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.ApplyVerifiedBombPlacement(2, battleengine.Cell{Row: 1, Col: 1}, 1, 0); err != nil {
				t.Fatal(err)
			}
			step := func(action battleengine.ActionID) {
				t.Helper()
				if _, err := batch.Step([][]battleengine.ActionID{{action, battleengine.ActionWait}}); err != nil {
					t.Fatal(err)
				}
			}
			step(battleengine.ActionMoveUp)
			half := engine.Actors()[0]
			flameCell := battleengine.Cell{Row: 1, Col: 2}
			if half.Position.Cell().Row != 2 || !engine.PositionOverlapsCell(half.Position, flameCell) {
				t.Fatalf("normal input did not reach half-body boundary: %+v", half.Position)
			}
			for engine.ElapsedMS()-origin < enterAtMS {
				step(battleengine.ActionWait)
			}
			if self := engine.Actors()[0]; self.State != battleengine.ActorActive || self.Position != half.Position {
				t.Fatalf("half-body hold failed: %+v", self)
			}
			step(battleengine.ActionMoveUp)
			step(battleengine.ActionWait) // include the pre-movement explosion callback
			self := engine.Actors()[0]
			want := battleengine.ActorActive
			if enterAtMS == 2_900 {
				want = battleengine.ActorTrapped
			}
			if self.Position.Cell() != flameCell || self.State != want {
				t.Fatalf("entry at %d: position=%+v state=%d want=%d", enterAtMS, self.Position, self.State, want)
			}
			visible := false
			for _, flame := range engine.Flames() {
				if flame.Cell == flameCell && flame.ExpiresAtMS > engine.ElapsedMS() {
					visible = true
				}
			}
			if !visible {
				t.Fatal("contrast did not enter a still-visible flame")
			}
			observation, err := batch.Observe()
			if err != nil {
				t.Fatal(err)
			}
			at := func(channel int) float32 {
				return observation.Spatial[(channel*observation.Height+int(flameCell.Row))*observation.Width+int(flameCell.Col)]
			}
			if at(15) != 1 || at(16) != 0 || at(35) != 0 || at(36) != 0 || at(37) != 0 {
				t.Fatalf("spent flame input: visible=%v first=%v last=%v clear=%v waves=%v", at(15), at(16), at(35), at(36), at(37))
			}
			t.Logf("half_body_position=%+v entered_position=%+v entry_at_ms=%d active=%v visible_flame=%v", half.Position, self.Position, enterAtMS, self.State == battleengine.ActorActive, visible)
		})
	}
}
