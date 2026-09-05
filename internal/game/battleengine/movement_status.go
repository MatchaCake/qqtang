package battleengine

func (engine *Engine) installMovementStatus(actor *Actor, status MovementStatusKind) {
	actor.MovementStatus = status
	actor.moveRemainder = 0
	switch status {
	case MovementStatusSlow, MovementStatusFast:
		actor.MovementStatusExpiresAt = saturatingAdd(engine.elapsedMS, NativeMovementStatusMS)
	case MovementStatusForcedSlide:
		actor.MovementStatusExpiresAt = 0
	default:
		actor.MovementStatusExpiresAt = 0
	}
}

func (engine *Engine) clearMovementStatus(actor *Actor) Event {
	status := actor.MovementStatus
	actor.MovementStatus = MovementStatusNone
	actor.MovementStatusExpiresAt = 0
	actor.moveRemainder = 0
	return Event{
		Kind: EventMovementStatusEnded, TimeMS: engine.elapsedMS,
		PlayerID: actor.PlayerID, Cell: actor.Position.Cell(), Position: actor.Position,
		MovementStatus: status,
	}
}

func (engine *Engine) expireMovementStatuses() []Event {
	events := make([]Event, 0)
	for index := range engine.actors {
		actor := &engine.actors[index]
		if actor.MovementStatus == MovementStatusNone || actor.MovementStatus == MovementStatusForcedSlide ||
			actor.MovementStatusExpiresAt == 0 || actor.MovementStatusExpiresAt >= engine.elapsedMS {
			continue
		}
		events = append(events, engine.clearMovementStatus(actor))
	}
	return events
}
