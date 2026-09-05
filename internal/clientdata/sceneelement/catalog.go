// Package sceneelement describes native battlefield objects understood by the
// installed QQTang client. These identifiers belong to the scene factory, not
// to the account inventory namespace and not to a particular wire message.
package sceneelement

// ID is the native battlefield object-factory key used by Client.exe.
type ID uint32

// Native rewards whose identity and value are recoverable from the installed
// 5.2 client. The server still decides which IDs are candidates in a match.
const (
	SugarCoin50      ID = 81
	SugarCoin100     ID = 82
	SugarCoin200     ID = 83
	SugarBag500      ID = 84
	SugarChest1000   ID = 85
	Experience20     ID = 86
	Experience50     ID = 87
	Experience100    ID = 88
	Experience200    ID = 89
	RewardChest91    ID = 91
	RewardChest92    ID = 92
	SugarChest1000B  ID = 93
	SugarChest500    ID = 94
	RewardChest95    ID = 95
	TreasureGemOne   ID = 150
	TreasureGemTwo   ID = 151
	TreasureGemThree ID = 152
	// SculptureFragment12..16 are the five rule-6 scene objects. The
	// battlefield factory constructs IDs 0xA1..0xA5; their pickup callback
	// converts them to the carried material values 12..16 used by the shared
	// bun-action wire family.
	SculptureFragment12 ID = 161
	SculptureFragment13 ID = 162
	SculptureFragment14 ID = 163
	SculptureFragment15 ID = 164
	SculptureFragment16 ID = 165
	// TankRepair, TankArmor and TankShell are the three rule-13-only scene
	// objects constructed by Client.exe's 0xC9/0xCB/0xCC factory branches.
	// Their pickup handlers respectively operate on the base repair path, the
	// armour damage state, and the one-hit player shell state.
	TankRepair     ID = 201
	TankArmor      ID = 203
	TankShell      ID = 204
	RewardChest211 ID = 211
	RewardChest212 ID = 212
	RewardChest213 ID = 213
)

// RewardKind identifies the native accounting namespace affected by a scene
// element. Match sugar is accumulated per player, competitive experience is
// committed by settlement, and treasure score is a match objective only.
type RewardKind uint8

const (
	RewardMatchSugar RewardKind = iota + 1
	RewardCompetitiveExperience
	RewardTreasureScore
)

// RewardDefinition is static client semantics, not a drop-table entry.
type RewardDefinition struct {
	Kind                   RewardKind
	Value                  uint32
	DirectMatchAccumulator bool
}

// NativeReward returns value semantics proven from Client.exe's constructors
// and pickup callbacks. Unresolved chests intentionally return false.
func NativeReward(id ID) (RewardDefinition, bool) {
	switch id {
	case SugarCoin50:
		return RewardDefinition{Kind: RewardMatchSugar, Value: 50, DirectMatchAccumulator: true}, true
	case SugarCoin100:
		return RewardDefinition{Kind: RewardMatchSugar, Value: 100, DirectMatchAccumulator: true}, true
	case SugarCoin200:
		return RewardDefinition{Kind: RewardMatchSugar, Value: 200, DirectMatchAccumulator: true}, true
	case SugarBag500, SugarChest500:
		return RewardDefinition{Kind: RewardMatchSugar, Value: 500, DirectMatchAccumulator: true}, true
	case SugarChest1000, SugarChest1000B:
		return RewardDefinition{Kind: RewardMatchSugar, Value: 1000, DirectMatchAccumulator: true}, true
	case Experience20:
		return RewardDefinition{Kind: RewardCompetitiveExperience, Value: 20}, true
	case Experience50:
		return RewardDefinition{Kind: RewardCompetitiveExperience, Value: 50}, true
	case Experience100:
		return RewardDefinition{Kind: RewardCompetitiveExperience, Value: 100}, true
	case Experience200:
		return RewardDefinition{Kind: RewardCompetitiveExperience, Value: 200}, true
	case TreasureGemOne:
		return RewardDefinition{Kind: RewardTreasureScore, Value: 1}, true
	case TreasureGemTwo:
		return RewardDefinition{Kind: RewardTreasureScore, Value: 2}, true
	case TreasureGemThree:
		return RewardDefinition{Kind: RewardTreasureScore, Value: 3}, true
	default:
		return RewardDefinition{}, false
	}
}

// NativeTransformationDurationMS is assigned by the common avatar constructor
// at Client+0x1F786E. Picking another transformation replaces the current
// avatar object and restarts this duration.
const NativeTransformationDurationMS uint32 = 30_000

// TransformationDefinition is the exact scene-object to avatar-role mapping
// constructed by the installed Client.exe. AvatarRoleID belongs to the
// temporary battlefield avatar namespace rather than the account role catalog.
type TransformationDefinition struct {
	SceneID      ID
	AvatarRoleID uint16
	DurationMS   uint32
	// The fields below project the gameplay-bearing avatar vtable slots used by
	// the rule-1 controller. Rendering-only slots deliberately stay outside the
	// deterministic server/training kernel.
	TraverseStaticTerrain bool
	CanCollectItems       bool
	RecoverOnlyOnOpenCell bool
	ReverseDirection      bool
	CanKickBomb           bool
	CanPushBreakable      bool
	SpeedRate             uint8
	HorizontalSpeedRate   uint8
	VerticalSpeedRate     uint8
	BombCapacity          uint8
	GrantedActionID       uint8
	GrantedActionCount    uint8
}

// BattleActionPickupDefinition describes an ordinary-map scene pickup whose
// native collision callback adds an item to one of the player's seven in-match
// action slots. ActionID belongs to the battlefield action namespace, not to
// the account inventory catalog.
type BattleActionPickupDefinition struct {
	SceneID  ID
	ActionID uint8
	Count    uint8
}

// NativeBattleActionPickup returns the exact scene-to-action mappings emitted
// by Client.exe's ordinary scene pickup callbacks. The uneven counts are
// intentional client rules rather than server balance choices.
func NativeBattleActionPickup(id ID) (BattleActionPickupDefinition, bool) {
	definition := BattleActionPickupDefinition{SceneID: id}
	switch id {
	case 21:
		definition.ActionID, definition.Count = 41, 1
	case 23:
		definition.ActionID, definition.Count = 42, 1
	case 24:
		definition.ActionID, definition.Count = 63, 1
	case 25:
		definition.ActionID, definition.Count = 43, 3
	case 27:
		definition.ActionID, definition.Count = 44, 2
	case 28:
		definition.ActionID, definition.Count = 64, 1
	default:
		return BattleActionPickupDefinition{}, false
	}
	return definition, true
}

// NativeTransformation returns every transformation reachable from native
// ordinary rule-1 wall tables. Their pickup callbacks all share the same
// replacement path (Client+0x1F767D -> Client+0x1ADF5A).
func NativeTransformation(id ID) (TransformationDefinition, bool) {
	var avatarRoleID uint16
	switch id {
	case 101:
		avatarRoleID = 43
	case 104:
		avatarRoleID = 41
	case 107:
		avatarRoleID = 45
	case 108:
		avatarRoleID = 46
	case 109:
		avatarRoleID = 44
	case 110:
		avatarRoleID = 42
	case 114:
		avatarRoleID = 54
	case 115:
		avatarRoleID = 55
	default:
		return TransformationDefinition{}, false
	}
	definition := TransformationDefinition{
		SceneID: id, AvatarRoleID: avatarRoleID, DurationMS: NativeTransformationDurationMS,
		CanCollectItems: true,
	}
	switch avatarRoleID {
	case 41: // duck: vtable slots 16/18/19
		definition.TraverseStaticTerrain = true
		definition.CanCollectItems = false
		definition.RecoverOnlyOnOpenCell = true
	case 42: // demon: vtable slots 20/21/23
		definition.SpeedRate = 8
		definition.BombCapacity = 8
		definition.ReverseDirection = true
	case 44: // panda: vtable slots 12/13
		definition.CanKickBomb = true
		definition.CanPushBreakable = true
	case 45: // fast ghost: vtable slot 20
		definition.SpeedRate = 8
	case 46: // candy king: vtable slot 21
		definition.BombCapacity = 8
	case 54: // crab: vtable slot 20 depends on movement axis
		definition.HorizontalSpeedRate = 5
		definition.VerticalSpeedRate = 2
	case 55: // axe gang: install/uninstall grants/removes action 46 x9
		definition.GrantedActionID = 46
		definition.GrantedActionCount = 9
	}
	return definition, true
}

// IsTransformationPickup reports whether id uses the native avatar replacement
// path. The server uses the same catalog for map recovery and simulation.
func IsTransformationPickup(id ID) bool {
	_, ok := NativeTransformation(id)
	return ok
}

// IsClientID reports whether the installed client scene-element factory has a
// constructor path for id. It is deliberately narrower than inventory item
// validity; callers must also verify the selected map or mode.
func IsClientID(id uint32) bool {
	if id >= 1 && id <= 8 ||
		id >= 0x15 && id <= 0x1c ||
		id >= 0x29 && id <= 0x2c ||
		id >= 0x2e && id <= 0x31 ||
		id >= 0x3d && id <= 0x40 ||
		id >= 0x50 && id <= 0x59 ||
		id >= 0x5b && id <= 0x62 ||
		id >= 0x6b && id <= 0x6e ||
		id >= 0x72 && id <= 0x78 ||
		id >= 0x96 && id <= 0x98 ||
		id >= 0xa1 && id <= 0xa5 ||
		id >= 0xd3 && id <= 0xd5 ||
		id >= 0x192 && id <= 0x3e8 ||
		id >= 9001 && id <= 9010 ||
		id >= 24001 && id <= 24499 ||
		id >= 25001 && id <= 29000 ||
		id >= 30001 && id <= 31999 {
		return true
	}
	switch id {
	case 0x42, 0x65, 0x68, 0xc9, 0xcb, 0xcc:
		return true
	default:
		return false
	}
}

// IsPermanentInventoryPickup reports whether the common scene factory treats
// id as an account-backed item that can be picked up from the battlefield.
// Client.exe has dedicated constructors for 402/403 and routes 404..1000
// through the generic item-resource constructor. These objects use the normal
// ITEM_INFO inventory namespace after pickup; they are not temporary battle
// actions, transformations, currency counters, or rule-owned objectives.
func IsPermanentInventoryPickup(id uint32) bool {
	return id >= 0x192 && id <= 0x3e8
}
