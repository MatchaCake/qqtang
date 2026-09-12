package craftcatalog

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qqtang/internal/protocol/game"
)

func TestForgeSplitClearsColorAndKeepsEffectWhileRevertClearsBothAxes(t *testing.T) {
	catalog := &Catalog{
		Split:  MaterialCost{ItemID: 30001, Quantity: 1},
		Revert: MaterialCost{ItemID: 30002, Quantity: 1},
	}
	forged := game.NewPermanentItemInfo(300, 1)
	forged.ItemEffect = 5
	forged.ItemColor = byte(ForgeColorPurple)

	split, err := catalog.Plan(ForgeSplit, forged, catalog.Split.ItemID, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !split.Succeeded || split.Effect != forged.ItemEffect || split.Color != 0 {
		t.Fatalf("split plan = %+v", split)
	}

	effectOnly := forged
	effectOnly.ItemEffect = split.Effect
	effectOnly.ItemColor = split.Color
	revert, err := catalog.Plan(ForgeRevert, effectOnly, catalog.Revert.ItemID, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !revert.Succeeded || revert.Effect != 0 || revert.Color != 0 {
		t.Fatalf("revert plan = %+v", revert)
	}
}

func TestForgeMaterialRulesLoadIndependentWeightedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge-rules.json")
	contents := []byte(`{
  "version": 1,
  "materials": [{
    "item_id": 20051,
    "success_percent": 75,
    "level_weights": {"1": 70, "2": 30},
    "color_weights": {"red": 60, "blue": 40},
    "black_percent": 5
  }]
}`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := loadForgeMaterialRules(path)
	if err != nil {
		t.Fatal(err)
	}
	rule := rules[20051]
	if rule.SuccessPercent != 75 || rule.BlackPercent != 5 {
		t.Fatalf("material percentages = success %d, black %d", rule.SuccessPercent, rule.BlackPercent)
	}
	if len(rule.LevelWeights) != 2 || rule.LevelWeights[0] != (LevelWeight{Level: 1, Weight: 70}) || rule.LevelWeights[1] != (LevelWeight{Level: 2, Weight: 30}) {
		t.Fatalf("level weights = %#v", rule.LevelWeights)
	}
	if len(rule.ColorWeights) != 2 || rule.ColorWeights[0] != (ColorWeight{Color: ForgeColorBlue, Weight: 40}) || rule.ColorWeights[1] != (ColorWeight{Color: ForgeColorRed, Weight: 60}) {
		t.Fatalf("color weights = %#v", rule.ColorWeights)
	}
}

func TestLoadForgeUsesServerRulesWithoutItemRegistry(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	catalog, err := LoadForge(filepath.Join("testdata", "forge-client"), filepath.Join(root, "data", "qqt_forge_rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Materials) != 7 {
		t.Fatalf("material count = %d, want 7", len(catalog.Materials))
	}
	if rule := catalog.Materials[20051]; len(rule.LevelWeights) != 2 || len(rule.ColorWeights) != 2 {
		t.Fatalf("material 20051 = %+v", rule)
	}
}

func TestForgeMaterialRulesRejectInvalidProbabilityConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		rule    string
		message string
	}{
		{name: "missing success", rule: `"level_weights":{"1":1},"color_weights":{"red":1},"black_percent":0`, message: "no success_percent"},
		{name: "invalid black", rule: `"success_percent":50,"level_weights":{"1":1},"color_weights":{"red":1},"black_percent":101`, message: "black_percent is outside"},
		{name: "empty levels", rule: `"success_percent":50,"level_weights":{"1":0},"color_weights":{"red":1},"black_percent":0`, message: "level_weights has no positive"},
		{name: "black ordinary color", rule: `"success_percent":50,"level_weights":{"1":1},"color_weights":{"black":1},"black_percent":0`, message: "invalid ordinary color"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "forge-rules.json")
			contents := []byte(`{"version":1,"materials":[{"item_id":20051,` + test.rule + `}]}`)
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadForgeMaterialRules(path)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want containing %q", err, test.message)
			}
		})
	}
}

func TestChooseWeightedIndexUsesConfiguredBoundaries(t *testing.T) {
	weights := []uint64{70, 20, 10}
	for value, want := range map[uint64]int{100: 0, 169: 0, 170: 1, 189: 1, 190: 2, 199: 2} {
		got, err := chooseWeightedIndex(weightedEntropy(value), weights)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value %d selected %d, want %d", value, got, want)
		}
	}
}

func TestForgeApplyAfterSplitUpgradesButNeverDegradesEffectAndRerollsColor(t *testing.T) {
	catalog := &Catalog{
		Items: map[uint16]ForgeItem{300: {ItemID: 300, Class: ForgeCap}},
		Effects: map[byte]EffectForm{
			1: {ID: 1, Class: ForgeCap, Level: 1},
			2: {ID: 2, Class: ForgeCap, Level: 2},
			5: {ID: 5, Class: ForgeCap, Level: 5},
		},
		Materials: map[uint16]MaterialRule{
			20051: {
				ItemID: 20051, SuccessPercent: 100,
				LevelWeights: []LevelWeight{{Level: 1, Weight: 1}, {Level: 2, Weight: 1}},
				ColorWeights: []ColorWeight{{Color: ForgeColorRed, Weight: 1}, {Color: ForgeColorBlue, Weight: 1}},
			},
		},
	}
	splitItem := game.NewPermanentItemInfo(300, 1)
	splitItem.ItemEffect = 5
	splitItem.ItemColor = 0

	first, err := catalog.Plan(ForgeApply, splitItem, 20051, weightedEntropy(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first.Effect != 5 || first.Color != byte(ForgeColorRed) {
		t.Fatalf("first non-degrading reroll = %+v", first)
	}
	second, err := catalog.Plan(ForgeApply, splitItem, 20051, weightedEntropy(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if second.Effect != 5 || second.Color != byte(ForgeColorBlue) {
		t.Fatalf("second non-degrading reroll = %+v", second)
	}

	levelOne := splitItem
	levelOne.ItemEffect = 1
	reforged, err := catalog.Plan(ForgeApply, levelOne, 20051, weightedEntropy(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if reforged.Effect != 1 || reforged.Color != byte(ForgeColorRed) {
		t.Fatalf("existing-effect reroll = %+v", reforged)
	}
	upgraded, err := catalog.Plan(ForgeApply, levelOne, 20051, weightedEntropy(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Effect != 2 || upgraded.Color != byte(ForgeColorRed) {
		t.Fatalf("upgrading reroll = %+v", upgraded)
	}

	unforged := splitItem
	unforged.ItemEffect = 0
	initial, err := catalog.Plan(ForgeApply, unforged, 20051, weightedEntropy(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if initial.Effect != 2 || initial.Color != byte(ForgeColorBlue) {
		t.Fatalf("initial forge = %+v", initial)
	}
}

func TestForgeBlackRollOverridesWeightedOrdinaryColor(t *testing.T) {
	catalog := &Catalog{
		Items:   map[uint16]ForgeItem{300: {ItemID: 300, Class: ForgeCap}},
		Effects: map[byte]EffectForm{1: {ID: 1, Class: ForgeCap, Level: 1}},
		Materials: map[uint16]MaterialRule{20051: {
			ItemID: 20051, SuccessPercent: 100, BlackPercent: 25,
			LevelWeights: []LevelWeight{{Level: 1, Weight: 1}},
			ColorWeights: []ColorWeight{{Color: ForgeColorRed, Weight: 1}},
		}},
	}
	item := game.NewPermanentItemInfo(300, 1)

	black, err := catalog.Plan(ForgeApply, item, 20051, bytes.NewReader([]byte{0}))
	if err != nil {
		t.Fatal(err)
	}
	if black.Color != byte(ForgeColorBlack) {
		t.Fatalf("black roll color = %d", black.Color)
	}

	ordinary, err := catalog.Plan(ForgeApply, item, 20051, bytes.NewReader([]byte{25}))
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Color != byte(ForgeColorRed) {
		t.Fatalf("ordinary roll color = %d", ordinary.Color)
	}
}

func weightedEntropy(values ...uint64) *bytes.Reader {
	data := make([]byte, 8*len(values))
	for index, value := range values {
		binary.LittleEndian.PutUint64(data[index*8:], value)
	}
	return bytes.NewReader(data)
}
