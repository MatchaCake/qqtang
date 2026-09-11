package battleengine

// NeedsImmediatePolicyDecision requests another learned action on the next
// world frame. It never chooses an action or widens a native collision window.
// Both live control and the training scheduler use this same predicate.
func (engine *Engine) NeedsImmediatePolicyDecision(playerID uint16) bool {
	if engine == nil || engine.outcome.Ended {
		return false
	}
	index := engine.actorIndex(playerID)
	if index < 0 || engine.actors[index].State == ActorEliminated {
		return false
	}
	actor := &engine.actors[index]
	if actor.State == ActorActive && actor.NativePassActive &&
		engine.elapsedMS-actor.NativePassStartedAt < actor.NativePassDurationMS {
		// A traversal can require a turn after the 500..600ms activation
		// window has closed. Let the policy act on native frames throughout
		// the actual pass; this neither selects inputs nor extends its timer.
		return true
	}
	if actor.State == ActorActive && actor.NativePassCollisionValid {
		charge := engine.elapsedMS - actor.NativePassCollisionStartedAt
		fresh := engine.elapsedMS - actor.NativePassCollisionLastAt
		if charge > NativePassChargeMinMS && charge < NativePassChargeMaxMS && fresh < NativePassCollisionFreshMS {
			// Activation still requires the model to select a successful local
			// placement. An occupied cell or exhausted capacity cannot trigger it.
			legal, err := engine.LegalActionMask(playerID)
			if err == nil && legal[ActionPlaceBomb] {
				return true
			}
		}
	}
	// A chain cannot begin before a root fuse or a projectile callback. Reject
	// quiet frames cheaply before cloning the world for the exact forecast.
	deadline := saturatingAdd(engine.elapsedMS, 400)
	possibleImpact := false
	for _, bomb := range engine.bombs {
		if bomb.EffectiveExplodeAtMS() <= deadline {
			possibleImpact = true
			break
		}
	}
	if !possibleImpact {
		for _, projectile := range engine.projectiles {
			if projectile.ResolveAtMS <= deadline {
				possibleImpact = true
				break
			}
		}
	}
	if !possibleImpact {
		return false
	}
	// Preserve the existing live imminent-impact threshold and forecast rules.
	timeline, err := engine.DangerTimeline(600)
	if err != nil {
		return false
	}
	impact, threatened := timeline.ImpactAt(actor.Position.Cell())
	return threatened && impact <= deadline
}
