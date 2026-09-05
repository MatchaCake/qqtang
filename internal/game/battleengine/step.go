package battleengine

import "fmt"

// Step advances exactly one configured tick. Input order is irrelevant: at
// most one action is accepted per player and actors are always processed by
// ascending PlayerID.
func (engine *Engine) Step(actions []Action) ([]Event, error) {
	if engine == nil {
		return nil, fmt.Errorf("battle engine is nil")
	}
	if engine.outcome.Ended {
		return nil, nil
	}
	engine.activatePendingPickupDispatches()
	actionByPlayer := make(map[uint16]Action, len(actions))
	for _, action := range actions {
		if engine.actorIndex(action.PlayerID) < 0 {
			return nil, fmt.Errorf("battle action player %d is not a participant", action.PlayerID)
		}
		if _, duplicate := actionByPlayer[action.PlayerID]; duplicate {
			return nil, fmt.Errorf("battle action repeats player %d", action.PlayerID)
		}
		if _, _, ok := action.Move.delta(); !ok {
			return nil, fmt.Errorf("battle action player %d has invalid direction %d", action.PlayerID, action.Move)
		}
		if action.UseActionID != 0 {
			if _, ok := nativeActionDiscreteID(action.UseActionID); !ok {
				return nil, fmt.Errorf("battle action player %d uses unsupported native item %d", action.PlayerID, action.UseActionID)
			}
		}
		actionByPlayer[action.PlayerID] = action
	}
	for index := range engine.actors {
		playerID := engine.actors[index].PlayerID
		action, ok := actionByPlayer[playerID]
		if !ok {
			continue
		}
		if action.UseActionID == 0 {
			continue
		}
		actor := &engine.actors[index]
		if !canUseHeldAction(actor, action.UseActionID) {
			return nil, fmt.Errorf("battle action player %d cannot use native item %d in state %d", playerID, action.UseActionID, actor.State)
		}
	}
	// Most simulated movement ticks emit nothing. Allocate only on an event.
	events := []Event{}
	for index := range engine.actors {
		engine.expireNativePassState(&engine.actors[index])
	}
	// 005f43b1 runs DiffGrid handlers (pickup/contact) before generic input
	// handlers (items/bombs); 005c87b0 then calls 005f3810 for movement.
	// All sample the current scene clock, advanced only by 005b8009 later.
	engine.releaseFieldObjectPassThrough()
	events = append(events, engine.resolvePickupContacts()...)
	fieldEvents := engine.resolveFieldObjectContacts()
	events = append(events, fieldEvents...)
	events = append(events, engine.resolveActorContacts()...)
	// A live hit is committed only on its reliable echo. Suppress this frame's
	// input in the meantime, as the missing native client already consumed FA5.
	for _, event := range fieldEvents {
		if event.Kind == EventActorHitRequested {
			delete(actionByPlayer, event.PlayerID)
		}
	}
	// 006112af -> 005c07fe -> 005ac982 consumes direction before the item
	// producer reads facing. Keep this world direction across avatar recovery;
	// neither recovery nor a later hit reruns the input handler in this frame.
	var directionByActor [MaxParticipants]Direction
	for index := range engine.actors {
		actor := &engine.actors[index]
		if actor.State != ActorActive {
			continue
		}
		direction := actor.Facing
		if actor.MovementStatus != MovementStatusForcedSlide {
			direction = engine.transformInput(actor, actionByPlayer[actor.PlayerID].Move)
		}
		if direction != DirectionNone {
			actor.Facing = direction
			directionByActor[index] = direction
		}
	}

	// Placement snapshots the actor's pre-movement cell while the held
	// direction continues to advance the actor in the same input frame. Local
	// clients each check occupancy before their own 0xFA3; the remote consumer
	// does not recheck it. Preserve that native simultaneous-input behavior by
	// evaluating every request against the bomb cells present at phase start.
	var occupiedBeforePlacement map[Cell]struct{}
	for index := range engine.actors {
		action, ok := actionByPlayer[engine.actors[index].PlayerID]
		if !ok || !action.PlaceBomb || engine.actors[index].State != ActorActive {
			continue
		}
		if occupiedBeforePlacement == nil {
			occupiedBeforePlacement = make(map[Cell]struct{}, len(engine.bombs))
			for _, bomb := range engine.bombs {
				occupiedBeforePlacement[bomb.Cell] = struct{}{}
			}
		}
		if _, occupied := occupiedBeforePlacement[engine.actors[index].Position.Cell()]; occupied {
			continue
		}
		if event, placed := engine.placeBombOnSnapshotEmptyCell(index); placed {
			events = append(events, event)
			// FUN_005b08e0 invokes the actor vtable slot +0x1c only after
			// successfully creating the local bubble. For normal actors that
			// slot reaches FUN_005d33ab and is the sole activation gate for the
			// charged native pass state. Collision updates merely charge the
			// state; they never activate it on a frame by themselves.
			actor := &engine.actors[index]
			if engine.activateNativePassState(actor) {
				events = append(events, Event{
					Kind: EventNativePassStarted, TimeMS: engine.elapsedMS,
					PlayerID: actor.PlayerID, Cell: actor.Position.Cell(), Position: actor.Position,
				})
			}
		}
	}
	for index := range engine.actors {
		action, ok := actionByPlayer[engine.actors[index].PlayerID]
		if !ok || action.UseActionID == 0 {
			continue
		}
		usedEvents, _ := engine.useHeldAction(index, action.UseActionID)
		events = append(events, usedEvents...)
	}

	// Ordinary generic callbacks run after item/bomb input and before world
	// movement: 0060e720 emits explosions, 0060dd20 recovers avatars, and
	// 0060c63a samples actor hits. Their scene clock is still T, not T+dt.
	engine.expireFlames()
	if !engine.rules.NativeOutcomeAuthority {
		events = append(events, engine.explodeDueBombs()...)
		events = append(events, engine.dispatchRecycledPickups()...)
	}
	events = append(events, engine.expireTransformations()...)
	var interactions [MaxParticipants]struct{ attempted, blocked bool }
worldInteractions:
	for index := range engine.actors {
		actor := &engine.actors[index]
		direction := directionByActor[index]
		if actor.State != ActorActive || direction == DirectionNone {
			continue
		}
		for _, event := range fieldEvents {
			if event.Kind == EventActorHitRequested && event.PlayerID == actor.PlayerID {
				continue worldInteractions
			}
		}
		// Generic 0060cf49/0060cac9 run before 0060c63a actor hits.
		if event, attempted := engine.tryActorWorldInteraction(index, direction); attempted {
			interactions[index].attempted = true
			interactions[index].blocked = event.Kind == 0
			if event.Kind != 0 {
				events = append(events, event)
			}
		}
	}
	hitEvents := engine.applyFlameHazards()
	events = append(events, hitEvents...)
	for _, event := range hitEvents {
		if event.Kind == EventActorTransformationEnded {
			if index := engine.actorIndex(event.PlayerID); index >= 0 {
				directionByActor[index] = DirectionNone
			}
		}
	}
movement:
	for index := range engine.actors {
		if engine.actors[index].State != ActorActive {
			continue
		}
		for _, event := range fieldEvents {
			if event.Kind == EventActorHitRequested && event.PlayerID == engine.actors[index].PlayerID {
				// Live state waits for the native echo, but the missing local
				// client would already have stopped at its hit checkpoint.
				// The runtime suspends subsequent steps when publishing FA5.
				continue movement
			}
		}
		direction := directionByActor[index]
		forcedSlide := engine.actors[index].MovementStatus == MovementStatusForcedSlide
		if direction == DirectionNone {
			continue
		}
		actor := &engine.actors[index]
		actor.nativePreviousPosition = actor.Position
		distance := engine.consumeNativeMovementDistance(actor, direction, engine.rules.TickMS)
		var moveEvents []Event
		var moved bool
		if !interactions[index].blocked && distance != 0 {
			moveEvents, moved = engine.moveActorDisplacement(index, direction, actor.Position, distance, !interactions[index].attempted)
		}
		events = append(events, moveEvents...)
		if moved {
		} else if forcedSlide {
			events = append(events, engine.clearMovementStatus(&engine.actors[index]))
		}
	}
	engine.releaseFieldObjectPassThrough()

	engine.elapsedMS = saturatingAdd(engine.elapsedMS, engine.rules.TickMS)
	engine.activatePendingPickupDispatches()
	engine.resolveActionProjectiles()
	events = append(events, engine.expireMovementStatuses()...)
	events = append(events, engine.expireTraps()...)
	if event, ended := engine.evaluateTerminal(); ended {
		events = append(events, event)
	}
	engine.recordPublicBehavior(events)
	return events, nil
}

func saturatingAdd(value, delta uint32) uint32 {
	if ^uint32(0)-value < delta {
		return ^uint32(0)
	}
	return value + delta
}
