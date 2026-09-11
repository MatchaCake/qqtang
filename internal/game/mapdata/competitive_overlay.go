package mapdata

import (
	"fmt"
	"slices"

	"qqtang/internal/game/rolecatalog"
)

// CompetitiveOverlayKind identifies gameplay activated for one match in
// addition to the selected map's native rule object. Selecting a map only
// supplies candidates; it does not by itself activate an overlay.
type CompetitiveOverlayKind string

const (
	CompetitiveOverlayNone CompetitiveOverlayKind = ""
	CompetitiveOverlayBoss CompetitiveOverlayKind = "boss"
)

// CompetitiveTeamProjection controls the team colours visible only inside a
// running match. Room colours remain unchanged and are therefore restored
// naturally when the clients return to the room.
type CompetitiveTeamProjection string

const (
	CompetitiveTeamsPreserved CompetitiveTeamProjection = "preserved"
	CompetitiveTeamsUnified   CompetitiveTeamProjection = "unified"
)

// CompetitiveBossTemplate names a statically proven native entity template.
// The protocol-facing encoding remains in the server adapter.
type CompetitiveBossTemplate string

const (
	CompetitiveBossTemplateNone         CompetitiveBossTemplate = ""
	CompetitiveBossTemplateSharedNative CompetitiveBossTemplate = "shared_native"
)

// CompetitiveBossSceneCapability identifies an additional scene event that
// the server may have to author while a particular Boss overlay is active. It
// is not a map rule: the same map without the summon condition remains an
// ordinary competitive round.
type CompetitiveBossSceneCapability string

const (
	// CompetitiveBossSceneAirborneBombs marks an encounter whose original
	// server supplied airborne bubble/item batches. The final client's 0x10E1
	// receiver is proven, but its native producer is installed only by rule 2
	// (kick-bomb). Consequently this capability must never authorize a rule-1
	// client uplink; it is input to a future server-side scheduler.
	CompetitiveBossSceneAirborneBombs CompetitiveBossSceneCapability = "airborne_bombs"
)

// CompetitiveMatchOverlay is the immutable result of activating one
// candidate for a particular match. It must never be stored as an unconditional
// property of a map: the same map can run its ordinary native rule when the
// summon requirements are not met.
type CompetitiveMatchOverlay struct {
	Kind                CompetitiveOverlayKind           `json:"kind"`
	ConclusionAuthority CompetitiveConclusionAuthority   `json:"conclusion_authority"`
	PlayerLifecycle     CompetitivePlayerLifecycle       `json:"player_lifecycle"`
	TeamProjection      CompetitiveTeamProjection        `json:"team_projection"`
	UnifiedTeamID       byte                             `json:"unified_team_id,omitempty"`
	BossTemplate        CompetitiveBossTemplate          `json:"boss_template,omitempty"`
	SceneCapabilities   []CompetitiveBossSceneCapability `json:"scene_capabilities,omitempty"`
}

type CompetitiveBossActivationKind string

const (
	// CompetitiveBossActivationUnconditional is reserved for a specifically
	// verified encounter that starts without a summon item. "Free" is not used
	// here because it already has a different, player-visible meaning: a free
	// room permits unequal team sizes.
	CompetitiveBossActivationUnconditional CompetitiveBossActivationKind = "unconditional"
	// CompetitiveBossActivationAllPlayersOwn requires every active player to
	// satisfy one complete item option. Only its consumable requirements enter
	// the subsequent all-or-nothing inventory transaction.
	CompetitiveBossActivationAllPlayersOwn CompetitiveBossActivationKind = "all_players_own"
)

type CompetitiveBossItemRequirement struct {
	ItemID uint16 `json:"item_id"`
	Count  uint32 `json:"count"`
	// OwnershipOnly keeps qualifying items, such as Nian outfits, out of the debit plan.
	OwnershipOnly bool `json:"ownership_only,omitempty"`
}

// CompetitiveBossItemOption is one AND-set of requirements. Options are ORed
// per player. The shape is needed for encounters such as the Great Nian Beast
// that require several items together; it must not turn recipe ingredients
// into direct summon alternatives.
type CompetitiveBossItemOption struct {
	Requirements []CompetitiveBossItemRequirement `json:"requirements"`
}

// CompetitiveBossEntity describes the native model and round-facing scalar
// values sent in BOSS_INFO. RoleID is anchored to the installed player model
// resources. HP values for the tiered encounters are explicit restoration
// values and remain isolated here for later packet/video correction.
type CompetitiveBossEntity struct {
	RoleID uint16 `json:"role_id"`
	HP     uint16 `json:"hp"`
	Rate   uint32 `json:"rate"`
	Bubble uint32 `json:"bubble"`
	Power  uint16 `json:"power"`
	// AIProgram selects one complete native movement/decision controller. It is
	// independent of Skills: the kick-bomb controller is the only AIType that
	// invokes the final client's skill-10..12 interpreter and football-aware
	// pathing, while the general combat controller invokes the 1..9 families.
	AIProgram CompetitiveBossAIProgram `json:"ai_program"`
	// SkillInterpreter records the role-family implementation selected by the
	// final client. It is a compatibility boundary only: its supported numeric
	// range must never be copied wholesale into BOSS_INFO.
	SkillInterpreter CompetitiveBossSkillInterpreter `json:"skill_interpreter"`
	// SkillProgram records the encounter profile selected by the restoration.
	// Skills remains the actual BOSS_INFO wire list. Keeping all three values
	// separate prevents "the interpreter has a case for this ID" from becoming
	// "the original server enabled this skill for this Boss".
	SkillProgram CompetitiveBossSkillProgram `json:"skill_program"`
	// Skills is the native BOSS_INFO skill program. The final client's Boss
	// controller reads this list on its own timers and authors the matching
	// NPC-use-skill events; an empty list leaves only movement and avoidance.
	Skills []uint32 `json:"skills,omitempty"`
}

// CompetitiveBossCombatTuple is one movement/bubble tuple. Power remains a
// WORD because that is its BOSS_INFO wire width; Rate and Bubble are DWORDs.
type CompetitiveBossCombatTuple struct {
	Rate   uint32
	Bubble uint32
	Power  uint16
}

// CompetitiveBossCombatProfile keeps the final client's RoleID row separate
// from the BOSS_INFO tuple. NativeInitial/NativeMaximum are the six protected
// values loaded by FUN_005aa74e. Wire is the server restoration policy for one
// Boss instance; it is not presented as a recovered original-server default.
// Most roles expose the same native caps, but RoleID 33/34 are a proven
// exception: their installed row says 0/0 ordinary bubbles even though
// captured gameplay shows both pirate Bosses placing player-like bubbles. The
// restoration preserves the original client and keeps that discrepancy
// explicit instead of rewriting the RoleID table.
type CompetitiveBossCombatProfile struct {
	NativeInitial CompetitiveBossCombatTuple
	NativeMaximum CompetitiveBossCombatTuple
	Wire          CompetitiveBossCombatTuple
}

// competitiveBossMaximumWireTuple is the requested full-strength BOSS_INFO
// profile. It is deliberately independent of NativeMaximum: pirate and
// kick-bomb RoleID rows do not expose ordinary-bubble capacity, while the
// server-owned Bubble field is still the encounter's recoverable simultaneous
// bubble limit.
var competitiveBossMaximumWireTuple = CompetitiveBossCombatTuple{Rate: 8, Bubble: 8, Power: 9}

var competitiveBossCombatProfiles = map[uint16]CompetitiveBossCombatProfile{
	// FUN_005aa74e indexes six protected values in DAT_007f2680 by RoleID.
	// The effective getters prove the pairs are current/cap for ordinary bubble
	// count, bubble power and movement rate respectively.
	rolecatalog.RoleThiefGriffin:     {NativeInitial: CompetitiveBossCombatTuple{7, 3, 4}, NativeMaximum: CompetitiveBossCombatTuple{8, 8, 9}, Wire: competitiveBossMaximumWireTuple},
	rolecatalog.RoleChristmasGriffin: {NativeInitial: CompetitiveBossCombatTuple{7, 5, 6}, NativeMaximum: CompetitiveBossCombatTuple{8, 8, 9}, Wire: competitiveBossMaximumWireTuple},
	rolecatalog.RoleSmallNianBeast:   {NativeInitial: CompetitiveBossCombatTuple{7, 3, 4}, NativeMaximum: CompetitiveBossCombatTuple{8, 8, 9}, Wire: competitiveBossMaximumWireTuple},
	rolecatalog.RoleGhostGriffin:     {NativeInitial: CompetitiveBossCombatTuple{7, 5, 6}, NativeMaximum: CompetitiveBossCombatTuple{8, 8, 9}, Wire: competitiveBossMaximumWireTuple},
	rolecatalog.RoleLargeNianBeast:   {NativeInitial: CompetitiveBossCombatTuple{7, 5, 6}, NativeMaximum: CompetitiveBossCombatTuple{8, 8, 9}, Wire: competitiveBossMaximumWireTuple},
	rolecatalog.RoleWaterSailor:      {NativeInitial: CompetitiveBossCombatTuple{7, 3, 4}, NativeMaximum: CompetitiveBossCombatTuple{8, 8, 9}, Wire: competitiveBossMaximumWireTuple},
	// The pirate wire tuple restores the server-owned ordinary-bubble property.
	// Skill 6 remains the separate falling-bubble program.
	rolecatalog.RoleFirstMate:   {NativeInitial: CompetitiveBossCombatTuple{7, 0, 4}, NativeMaximum: CompetitiveBossCombatTuple{8, 0, 9}, Wire: competitiveBossMaximumWireTuple},
	rolecatalog.RoleCaptainHook: {NativeInitial: CompetitiveBossCombatTuple{7, 0, 4}, NativeMaximum: CompetitiveBossCombatTuple{8, 0, 9}, Wire: competitiveBossMaximumWireTuple},
	// Football actions remain owned by the rule-2 controller. The full wire
	// profile does not authorize general-combat bubble placement in AIType 5.
	rolecatalog.RoleCristiano: {NativeInitial: CompetitiveBossCombatTuple{7, 0, 4}, NativeMaximum: CompetitiveBossCombatTuple{8, 0, 9}, Wire: competitiveBossMaximumWireTuple},
	rolecatalog.RoleRooney:    {NativeInitial: CompetitiveBossCombatTuple{7, 0, 4}, NativeMaximum: CompetitiveBossCombatTuple{8, 0, 9}, Wire: competitiveBossMaximumWireTuple},
}

// NativeCombatProfile returns the final client's immutable profile for this
// model.  It is intentionally RoleID-based rather than encounter-based: map,
// HP, AI and skills are orthogonal to the model's movement/bubble properties.
func (entity CompetitiveBossEntity) NativeCombatProfile() (CompetitiveBossCombatProfile, bool) {
	profile, ok := competitiveBossCombatProfiles[entity.RoleID]
	return profile, ok
}

// CompetitiveBossAIProgram names a proven client controller instead of
// leaking a bare BOSS_INFO integer throughout the restoration catalog.
type CompetitiveBossAIProgram string

const (
	// AIType 3 owns general Boss pursuit, avoidance, ordinary bubble placement
	// and the generic/pirate skill interpreters.
	CompetitiveBossAIGeneralCombat CompetitiveBossAIProgram = "general_combat"
	// AIType 5 owns rule-2 football-aware pathing and the skill-10..12
	// interpreter used by RoleID 35/36. It does not use the general bubble-
	// placement update routine.
	CompetitiveBossAIKickBomb CompetitiveBossAIProgram = "kick_bomb"
)

// NativeAIType resolves the semantic program to the exact BOSS_INFO value
// accepted by the final client's FUN_005df20b factory.
func (entity CompetitiveBossEntity) NativeAIType() (uint32, error) {
	switch entity.AIProgram {
	case CompetitiveBossAIGeneralCombat:
		return 3, nil
	case CompetitiveBossAIKickBomb:
		return 5, nil
	default:
		return 0, fmt.Errorf("unknown Boss AI program %q", entity.AIProgram)
	}
}

// CompetitiveBossSkillInterpreter identifies code in the final client, not a
// server-authored skill list.
type CompetitiveBossSkillInterpreter string

const (
	CompetitiveBossInterpreterGeneric  CompetitiveBossSkillInterpreter = "generic_1_4"
	CompetitiveBossInterpreterPirate   CompetitiveBossSkillInterpreter = "pirate_1_9"
	CompetitiveBossInterpreterKickBomb CompetitiveBossSkillInterpreter = "kick_bomb_10_12"
)

// CompetitiveBossSkillProgram names the explicit BOSS_INFO list used by one
// restoration profile. The final client proves the meaning of each included
// ID, but does not contain the original server's per-encounter configuration.
type CompetitiveBossSkillProgram string

const (
	// GenericSlowGlue keeps only the common scene-43 slow-glue wave used by the
	// ordinary Griffin/Nian encounters. Generic skill 2 is intentionally absent:
	// runtime tracing proves that it selects an ordinary non-avatar participant
	// and replaces that player's control state; no restored encounter has source
	// evidence that authorizes this periodic forced-control skill.
	CompetitiveBossSkillsGenericSlowGlue CompetitiveBossSkillProgram = "generic_slow_glue"
	// GenericSlowGlueTransform is the Large Nian program. Client code proves
	// skill 3 is the timed transformation-item grant, while observed encounter
	// behavior identifies Large Nian as the one named Boss that uses it.
	CompetitiveBossSkillsGenericSlowGlueTransform CompetitiveBossSkillProgram = "generic_slow_glue_transform"
	// The pirate controllers do not schedule directly from BOSS_INFO. Role 33
	// and 34 each expand five immutable client tables at DAT_007f2a20 and
	// DAT_007f2ad4. Keep distinct advertised sets so server authority accepts
	// exactly the IDs that the selected native controller can actually emit.
	CompetitiveBossSkillsFirstMateScript CompetitiveBossSkillProgram = "first_mate_script"
	CompetitiveBossSkillsHookScript      CompetitiveBossSkillProgram = "hook_script"
	// KickBombOneScript is the Rooney program on kick-bomb map 01. Its skill 10
	// uses the same slow-glue receiver as generic skill 1.
	CompetitiveBossSkillsKickBombOneScript CompetitiveBossSkillProgram = "kick_bomb_01_script"
	// KickBombTwoScript is the Cristiano program on kick-bomb map 02. Observed
	// encounter behavior excludes slow glue. The native kick-bomb interpreter,
	// however, aligns its three cooldown cells by wire-list position while
	// updating them by skill ID (10->slot 0, 11->slot 1, 12->slot 2). Slot 0
	// therefore has to remain as an ignored zero placeholder; compacting this to
	// {11,12} makes skill 11 test slot 0 but update slot 1 and fire every tick.
	CompetitiveBossSkillsKickBombTwoScript CompetitiveBossSkillProgram = "kick_bomb_02_script"
)

type competitiveBossSkillProgramSpec struct {
	Interpreter CompetitiveBossSkillInterpreter
	Skills      []uint32
}

var competitiveBossSkillPrograms = map[CompetitiveBossSkillProgram]competitiveBossSkillProgramSpec{
	CompetitiveBossSkillsGenericSlowGlue:          {Interpreter: CompetitiveBossInterpreterGeneric, Skills: []uint32{1}},
	CompetitiveBossSkillsGenericSlowGlueTransform: {Interpreter: CompetitiveBossInterpreterGeneric, Skills: []uint32{1, 3}},
	CompetitiveBossSkillsFirstMateScript:          {Interpreter: CompetitiveBossInterpreterPirate, Skills: []uint32{4, 6, 7, 9}},
	CompetitiveBossSkillsHookScript:               {Interpreter: CompetitiveBossInterpreterPirate, Skills: []uint32{4, 5, 6, 7, 8, 9}},
	CompetitiveBossSkillsKickBombOneScript:        {Interpreter: CompetitiveBossInterpreterKickBomb, Skills: []uint32{10, 11, 12}},
	CompetitiveBossSkillsKickBombTwoScript:        {Interpreter: CompetitiveBossInterpreterKickBomb, Skills: []uint32{0, 11, 12}},
}

// ValidateSkillProgram rejects a candidate whose descriptive profile and wire
// list drift apart, or whose client role would select a different interpreter.
func (entity CompetitiveBossEntity) ValidateSkillProgram() error {
	spec, ok := competitiveBossSkillPrograms[entity.SkillProgram]
	if !ok {
		return fmt.Errorf("unknown skill program %q", entity.SkillProgram)
	}
	if entity.SkillInterpreter != spec.Interpreter {
		return fmt.Errorf("skill program %q uses interpreter %q, want %q", entity.SkillProgram, entity.SkillInterpreter, spec.Interpreter)
	}
	if !slices.Equal(entity.Skills, spec.Skills) {
		return fmt.Errorf("skill program %q has wire skills %v, want %v", entity.SkillProgram, entity.Skills, spec.Skills)
	}
	if _, err := entity.NativeAIType(); err != nil {
		return err
	}
	profile, ok := entity.NativeCombatProfile()
	if !ok {
		return fmt.Errorf("role %d has no native Boss combat profile", entity.RoleID)
	}
	if entity.Rate != profile.Wire.Rate || entity.Bubble != profile.Wire.Bubble || entity.Power != profile.Wire.Power {
		return fmt.Errorf("role %d combat tuple is %d/%d/%d, want Boss wire %d/%d/%d",
			entity.RoleID, entity.Rate, entity.Bubble, entity.Power,
			profile.Wire.Rate, profile.Wire.Bubble, profile.Wire.Power)
	}
	wantAIProgram := CompetitiveBossAIGeneralCombat
	if spec.Interpreter == CompetitiveBossInterpreterKickBomb {
		wantAIProgram = CompetitiveBossAIKickBomb
	}
	if entity.AIProgram != wantAIProgram {
		return fmt.Errorf("skill program %q uses AI program %q, want %q", entity.SkillProgram, entity.AIProgram, wantAIProgram)
	}
	var compatible bool
	switch entity.SkillProgram {
	case CompetitiveBossSkillsGenericSlowGlue:
		switch entity.RoleID {
		case rolecatalog.RoleThiefGriffin, rolecatalog.RoleChristmasGriffin,
			rolecatalog.RoleGhostGriffin, rolecatalog.RoleSmallNianBeast,
			rolecatalog.RoleWaterSailor:
			compatible = true
		}
	case CompetitiveBossSkillsGenericSlowGlueTransform:
		compatible = entity.RoleID == rolecatalog.RoleLargeNianBeast
	case CompetitiveBossSkillsFirstMateScript:
		compatible = entity.RoleID == rolecatalog.RoleFirstMate
	case CompetitiveBossSkillsHookScript:
		compatible = entity.RoleID == rolecatalog.RoleCaptainHook
	case CompetitiveBossSkillsKickBombOneScript:
		compatible = entity.RoleID == rolecatalog.RoleRooney
	case CompetitiveBossSkillsKickBombTwoScript:
		compatible = entity.RoleID == rolecatalog.RoleCristiano
	}
	if !compatible {
		return fmt.Errorf("role %d is incompatible with skill program %q", entity.RoleID, entity.SkillProgram)
	}
	return nil
}

// CompetitiveBossCandidate describes an encounter that a map is capable of
// hosting. Runtime code evaluates Activation immediately before GAME_BEGIN and
// uses Overlay only when the condition succeeds.
type CompetitiveBossCandidate struct {
	ID          string                        `json:"id"`
	Name        string                        `json:"name"`
	Activation  CompetitiveBossActivationKind `json:"activation"`
	ItemOptions []CompetitiveBossItemOption   `json:"item_options,omitempty"`
	Entity      CompetitiveBossEntity         `json:"entity"`
	Overlay     CompetitiveMatchOverlay       `json:"overlay"`
}

// CompetitiveMaximumActiveBosses is an observed gameplay invariant, not the
// wire array capacity. Every currently verified competitive Boss round has one
// active named Boss/entity. A map may have several satisfied summon candidates;
// the server chooses one from that pool before constructing this single entity.
const CompetitiveMaximumActiveBosses = 1

func bossOverlay(capabilities ...CompetitiveBossSceneCapability) CompetitiveMatchOverlay {
	return CompetitiveMatchOverlay{
		Kind:                CompetitiveOverlayBoss,
		ConclusionAuthority: CompetitiveConclusionNativeArbitrator,
		PlayerLifecycle:     CompetitiveLifecyclePermanentElimination,
		TeamProjection:      CompetitiveTeamsUnified,
		UnifiedTeamID:       1,
		BossTemplate:        CompetitiveBossTemplateSharedNative,
		SceneCapabilities:   append([]CompetitiveBossSceneCapability(nil), capabilities...),
	}
}

func bossEntity(roleID uint16, hp uint16, program CompetitiveBossSkillProgram) CompetitiveBossEntity {
	spec := competitiveBossSkillPrograms[program]
	profile, ok := competitiveBossCombatProfiles[roleID]
	if !ok {
		panic(fmt.Sprintf("missing native Boss combat profile for role %d", roleID))
	}
	aiProgram := CompetitiveBossAIGeneralCombat
	if spec.Interpreter == CompetitiveBossInterpreterKickBomb {
		aiProgram = CompetitiveBossAIKickBomb
	}
	return CompetitiveBossEntity{
		RoleID: roleID, HP: hp, Rate: profile.Wire.Rate, Bubble: profile.Wire.Bubble, Power: profile.Wire.Power,
		AIProgram: aiProgram, SkillInterpreter: spec.Interpreter, SkillProgram: program,
		Skills: append([]uint32(nil), spec.Skills...),
	}
}

func itemOption(requirements ...CompetitiveBossItemRequirement) CompetitiveBossItemOption {
	return CompetitiveBossItemOption{Requirements: append([]CompetitiveBossItemRequirement(nil), requirements...)}
}

func itemBoss(id, name string, entity CompetitiveBossEntity, overlay CompetitiveMatchOverlay, options ...CompetitiveBossItemOption) CompetitiveBossCandidate {
	return CompetitiveBossCandidate{
		ID: id, Name: name, Activation: CompetitiveBossActivationAllPlayersOwn,
		ItemOptions: append([]CompetitiveBossItemOption(nil), options...), Entity: entity, Overlay: overlay,
	}
}

var (
	sailorBoss = CompetitiveBossCandidate{
		ID: "sailor", Name: "水手", Activation: CompetitiveBossActivationUnconditional,
		Entity: bossEntity(rolecatalog.RoleWaterSailor, 5, CompetitiveBossSkillsGenericSlowGlue), Overlay: bossOverlay(),
	}
	firstMateBoss = itemBoss("first_mate", "大副", bossEntity(rolecatalog.RoleFirstMate, 10, CompetitiveBossSkillsFirstMateScript), bossOverlay(),
		itemOption(CompetitiveBossItemRequirement{ItemID: 460, Count: 1}),
	)
	hookBoss = itemBoss("hook", "虎克船长", bossEntity(rolecatalog.RoleCaptainHook, 10, CompetitiveBossSkillsHookScript), bossOverlay(),
		itemOption(CompetitiveBossItemRequirement{ItemID: 453, Count: 1}),
	)
	thiefGriffinBoss = itemBoss("thief_griffin", "大盗格里芬", bossEntity(rolecatalog.RoleThiefGriffin, 5, CompetitiveBossSkillsGenericSlowGlue), bossOverlay(),
		itemOption(CompetitiveBossItemRequirement{ItemID: 402, Count: 1}),
	)
	christmasGriffinBoss = itemBoss("christmas_griffin", "圣诞格里芬", bossEntity(rolecatalog.RoleChristmasGriffin, 10, CompetitiveBossSkillsGenericSlowGlue), bossOverlay(),
		itemOption(CompetitiveBossItemRequirement{ItemID: 403, Count: 1}),
	)
	ghostGriffinBoss = itemBoss("ghost_griffin", "鬼仆格里芬", bossEntity(rolecatalog.RoleGhostGriffin, 10, CompetitiveBossSkillsGenericSlowGlue), bossOverlay(),
		itemOption(CompetitiveBossItemRequirement{ItemID: 435, Count: 1}),
	)
	littleNianBoss = itemBoss("little_nian", "小年兽", bossEntity(rolecatalog.RoleSmallNianBeast, 5, CompetitiveBossSkillsGenericSlowGlue), bossOverlay(),
		itemOption(CompetitiveBossItemRequirement{ItemID: 436, Count: 1}),
	)
	greatNianBoss = itemBoss("great_nian", "大年兽", bossEntity(rolecatalog.RoleLargeNianBeast, 10, CompetitiveBossSkillsGenericSlowGlueTransform), bossOverlay(),
		itemOption(
			CompetitiveBossItemRequirement{ItemID: 437, Count: 1},
			CompetitiveBossItemRequirement{ItemID: 433, Count: 1, OwnershipOnly: true},
			CompetitiveBossItemRequirement{ItemID: 431, Count: 1, OwnershipOnly: true},
		),
	)
	cristianoBoss = itemBoss("cristiano", "小小罗", bossEntity(rolecatalog.RoleCristiano, 5, CompetitiveBossSkillsKickBombTwoScript), bossOverlay(),
		itemOption(CompetitiveBossItemRequirement{ItemID: 464, Count: 1}),
	)
	rooneyBoss = itemBoss("rooney", "鲁尼", bossEntity(rolecatalog.RoleRooney, 10, CompetitiveBossSkillsKickBombOneScript), bossOverlay(),
		itemOption(CompetitiveBossItemRequirement{ItemID: 469, Count: 1}),
	)
)

var competitiveBossesByID = map[string]CompetitiveBossCandidate{
	sailorBoss.ID:           sailorBoss,
	firstMateBoss.ID:        firstMateBoss,
	hookBoss.ID:             hookBoss,
	thiefGriffinBoss.ID:     thiefGriffinBoss,
	christmasGriffinBoss.ID: christmasGriffinBoss,
	ghostGriffinBoss.ID:     ghostGriffinBoss,
	littleNianBoss.ID:       littleNianBoss,
	greatNianBoss.ID:        greatNianBoss,
	cristianoBoss.ID:        cristianoBoss,
	rooneyBoss.ID:           rooneyBoss,
}

// competitiveBossCandidates is the restoration catalog joined to final-client
// map IDs. Entity models, native rules and summon-item descriptions are
// client-anchored; several map associations come from historical references
// and remain explicitly documented as such. Entries that share a map remain
// separate candidates; activation is decided from frozen participant
// inventories immediately before GAME_BEGIN.
var competitiveBossCandidates = map[uint32][]CompetitiveBossCandidate{
	11:  {sailorBoss},
	124: {sailorBoss},
	411: {firstMateBoss},
	104: {firstMateBoss},
	12:  {hookBoss},
	16:  {hookBoss},
	905: {thiefGriffinBoss, christmasGriffinBoss},
	906: {thiefGriffinBoss, christmasGriffinBoss},
	910: {thiefGriffinBoss, christmasGriffinBoss},
	211: {thiefGriffinBoss, christmasGriffinBoss, ghostGriffinBoss, littleNianBoss, greatNianBoss},
	212: {thiefGriffinBoss, christmasGriffinBoss, ghostGriffinBoss, littleNianBoss, greatNianBoss},
	102: {thiefGriffinBoss},
	510: {christmasGriffinBoss},
	511: {ghostGriffinBoss},
	512: {littleNianBoss},
	513: {greatNianBoss},
	702: {cristianoBoss},
	701: {rooneyBoss},
}

// LookupCompetitiveBossCandidates returns deep defensive copies of the
// encounter candidates associated with a map. An empty result means the map
// always uses its native ordinary/special rule without a Boss overlay.
func LookupCompetitiveBossCandidates(mapID uint32) []CompetitiveBossCandidate {
	source := competitiveBossCandidates[mapID]
	result := make([]CompetitiveBossCandidate, len(source))
	for index := range source {
		result[index] = cloneCompetitiveBossCandidate(source[index])
	}
	return result
}

// LookupCompetitiveBossCandidate resolves a stable runtime candidate ID. It
// is used to validate external reward configuration without exposing the
// mutable package registry.
func LookupCompetitiveBossCandidate(candidateID string) (CompetitiveBossCandidate, bool) {
	candidate, ok := competitiveBossesByID[candidateID]
	if !ok {
		return CompetitiveBossCandidate{}, false
	}
	return cloneCompetitiveBossCandidate(candidate), true
}

func cloneCompetitiveBossCandidate(source CompetitiveBossCandidate) CompetitiveBossCandidate {
	result := source
	result.Entity.Skills = append([]uint32(nil), source.Entity.Skills...)
	result.ItemOptions = make([]CompetitiveBossItemOption, len(source.ItemOptions))
	for index := range source.ItemOptions {
		result.ItemOptions[index].Requirements = append([]CompetitiveBossItemRequirement(nil), source.ItemOptions[index].Requirements...)
	}
	result.Overlay.SceneCapabilities = append([]CompetitiveBossSceneCapability(nil), source.Overlay.SceneCapabilities...)
	return result
}
