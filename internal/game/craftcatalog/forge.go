// Package craftcatalog owns the original client-side synthesis and avatar
// forge definitions. Keeping these rules outside the protocol adapter makes
// item validation and persistence independently testable.
package craftcatalog

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"qqtang/internal/protocol/game"
)

// ForgeOperation values come from uiShop.pyc's g_Operator tuple (1, 3, 4).
// Value 2 is intentionally not assigned: it is not one of the three avatar
// forge actions exposed by this client.
type ForgeOperation byte

const (
	ForgeApply  ForgeOperation = 1
	ForgeRevert ForgeOperation = 3
	ForgeSplit  ForgeOperation = 4
)

func (operation ForgeOperation) Valid() bool {
	return operation == ForgeApply || operation == ForgeRevert || operation == ForgeSplit
}

type ForgeClass string

const (
	ForgeCap  ForgeClass = "cap"
	ForgeBody ForgeClass = "body"
	ForgeWing ForgeClass = "wing"
	ForgeBomb ForgeClass = "bomb"
)

// ForgeColor is the index consumed by the client's AvatarForge_wear path.
// The original config uses red for index 3 even though some item descriptions
// call the same visual family orange.
type ForgeColor byte

const (
	ForgeColorWhite ForgeColor = iota
	ForgeColorBlack
	ForgeColorBlue
	ForgeColorRed
	ForgeColorGreen
	ForgeColorPurple
)

type ForgeItem struct {
	ItemID uint16
	Class  ForgeClass
}

type EffectForm struct {
	ID        byte
	Class     ForgeClass
	Level     byte
	Name      string
	Direction byte
}

type MaterialRule struct {
	ItemID         uint16
	SuccessPercent int
	LevelWeights   []LevelWeight
	ColorWeights   []ColorWeight
	BlackPercent   int
}

type LevelWeight struct {
	Level  byte
	Weight uint64
}

type ColorWeight struct {
	Color  ForgeColor
	Weight uint64
}

type MaterialCost struct {
	ItemID   uint16
	Quantity uint32
}

type Catalog struct {
	Version   uint32
	Items     map[uint16]ForgeItem
	Effects   map[byte]EffectForm
	Colors    map[ForgeColor]string
	Materials map[uint16]MaterialRule
	Split     MaterialCost
	Revert    MaterialCost
}

type ForgePlan struct {
	Operation        ForgeOperation
	Succeeded        bool
	TargetItemID     uint16
	MaterialItemID   uint16
	MaterialQuantity uint32
	PreviousEffect   byte
	PreviousColor    byte
	Effect           byte
	Color            byte
}

type forgeRulesFile struct {
	Version   uint32                  `json:"version"`
	Materials []forgeMaterialRuleJSON `json:"materials"`
}

type forgeMaterialRuleJSON struct {
	ItemID         uint16            `json:"item_id"`
	SuccessPercent *int              `json:"success_percent"`
	LevelWeights   map[string]uint64 `json:"level_weights"`
	ColorWeights   map[string]uint64 `json:"color_weights"`
	BlackPercent   *int              `json:"black_percent"`
}

// LoadForge combines stable client-side forge definitions with server-owned
// material probabilities. The probability rules never depend on itemCFG.py.
func LoadForge(clientRoot, rulesPath string) (*Catalog, error) {
	if strings.TrimSpace(clientRoot) == "" {
		return nil, fmt.Errorf("avatar forge client root is empty")
	}
	if strings.TrimSpace(rulesPath) == "" {
		return nil, fmt.Errorf("avatar forge rules path is empty")
	}
	materialRules, err := loadForgeMaterialRules(rulesPath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(clientRoot, "config", "avatarforge.ini"))
	if err != nil {
		return nil, fmt.Errorf("open avatar forge config: %w", err)
	}
	defer file.Close()
	return parseForge(file, materialRules)
}

func parseForge(reader io.Reader, materialRules map[uint16]MaterialRule) (*Catalog, error) {
	catalog := &Catalog{
		Items: make(map[uint16]ForgeItem), Effects: make(map[byte]EffectForm),
		Colors: make(map[ForgeColor]string), Materials: make(map[uint16]MaterialRule),
	}
	sections := make(map[string]map[string]string)
	section := ""
	scanner := bufio.NewScanner(reader)
	// The shipped 2008 INI uses bare carriage returns rather than CRLF.
	// bufio.ScanLines does not split that legacy form.
	scanner.Split(scanLegacyLines)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			if sections[section] == nil {
				sections[section] = make(map[string]string)
			}
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || section == "" {
			continue
		}
		sections[section][strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read avatar forge config: %w", err)
	}
	version, err := parseUint(sections["public"]["version"], 32, "public.version")
	if err != nil {
		return nil, err
	}
	catalog.Version = uint32(version)

	forgeTypeCount, err := parseUint(sections["forgetype"]["count"], 8, "forgetype.count")
	if err != nil {
		return nil, err
	}
	for index := uint64(1); index <= forgeTypeCount; index++ {
		name := fmt.Sprintf("forgetype.%d", index)
		values := sections[name]
		class := ForgeClass(strings.ToLower(values["typename"]))
		if class != ForgeCap && class != ForgeBody && class != ForgeWing && class != ForgeBomb {
			return nil, fmt.Errorf("%s has unsupported typename %q", name, class)
		}
		lineCount, parseErr := parseUint(values["count"], 16, name+".count")
		if parseErr != nil {
			return nil, parseErr
		}
		for lineIndex := uint64(1); lineIndex <= lineCount; lineIndex++ {
			for _, token := range strings.Fields(values[fmt.Sprintf("item.%d", lineIndex)]) {
				itemID, parseErr := parseUint(token, 16, name+" item")
				if parseErr != nil {
					return nil, parseErr
				}
				id := uint16(itemID)
				if existing, found := catalog.Items[id]; found && existing.Class != class {
					return nil, fmt.Errorf("forge item %d belongs to both %s and %s", id, existing.Class, class)
				}
				catalog.Items[id] = ForgeItem{ItemID: id, Class: class}
			}
		}
	}

	formCount, err := parseUint(sections["effectform"]["formcount"], 8, "effectform.formcount")
	if err != nil {
		return nil, err
	}
	for index := uint64(1); index <= formCount; index++ {
		key := fmt.Sprintf("form.%d", index)
		name := strings.ToLower(sections["effectform"][key])
		class, level, parseErr := parseEffectName(name)
		if parseErr != nil {
			return nil, parseErr
		}
		direction, _ := strconv.ParseUint(sections["effectform"][key+".direction"], 10, 8)
		catalog.Effects[byte(index)] = EffectForm{ID: byte(index), Class: class, Level: level, Name: name, Direction: byte(direction)}
	}

	colorCount, err := parseUint(sections["colorlist.1"]["colorcount"], 8, "colorlist.1.colorcount")
	if err != nil {
		return nil, err
	}
	for index := uint64(0); index < colorCount; index++ {
		catalog.Colors[ForgeColor(index)] = strings.ToLower(sections["colorlist.1"][fmt.Sprintf("color.%d", index)])
	}

	materialIDs := strings.Fields(sections["material"]["item.1"])
	for _, token := range materialIDs {
		itemID, parseErr := parseUint(token, 16, "material item")
		if parseErr != nil {
			return nil, parseErr
		}
		rule, found := materialRules[uint16(itemID)]
		if !found {
			return nil, fmt.Errorf("avatar forge material %d has no configured probability rule", itemID)
		}
		catalog.Materials[rule.ItemID] = rule
	}
	if len(catalog.Materials) != len(materialRules) {
		return nil, fmt.Errorf("avatar forge rules contain %d materials, client config contains %d", len(materialRules), len(catalog.Materials))
	}
	for _, rule := range catalog.Materials {
		for _, candidate := range rule.LevelWeights {
			for _, class := range []ForgeClass{ForgeCap, ForgeBody, ForgeWing, ForgeBomb} {
				if _, found := catalog.effectFor(class, candidate.Level); !found {
					return nil, fmt.Errorf("avatar forge material %d refers to unavailable level %d", rule.ItemID, candidate.Level)
				}
			}
		}
		for _, candidate := range rule.ColorWeights {
			if _, found := catalog.Colors[candidate.Color]; !found {
				return nil, fmt.Errorf("avatar forge material %d refers to unavailable color %d", rule.ItemID, candidate.Color)
			}
		}
	}
	if catalog.Split, err = parseCost(sections["split"], "split"); err != nil {
		return nil, err
	}
	if catalog.Revert, err = parseCost(sections["revert"], "revert"); err != nil {
		return nil, err
	}
	if len(catalog.Items) == 0 || len(catalog.Effects) != 20 || len(catalog.Colors) != 6 || len(catalog.Materials) != len(materialIDs) {
		return nil, fmt.Errorf("avatar forge config is incomplete: items=%d effects=%d colors=%d materials=%d", len(catalog.Items), len(catalog.Effects), len(catalog.Colors), len(catalog.Materials))
	}
	return catalog, nil
}

func scanLegacyLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for index, value := range data {
		if value != '\r' && value != '\n' {
			continue
		}
		advance = index + 1
		if value == '\r' && advance < len(data) && data[advance] == '\n' {
			advance++
		}
		return advance, data[:index], nil
	}
	if atEOF && len(data) != 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func loadForgeMaterialRules(path string) (map[uint16]MaterialRule, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open avatar forge rules: %w", err)
	}
	defer file.Close()

	var configured forgeRulesFile
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configured); err != nil {
		return nil, fmt.Errorf("decode avatar forge rules: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode avatar forge rules: multiple JSON values")
		}
		return nil, fmt.Errorf("decode avatar forge rules: %w", err)
	}
	if configured.Version != 1 {
		return nil, fmt.Errorf("avatar forge rules version %d, want 1", configured.Version)
	}

	rules := make(map[uint16]MaterialRule, len(configured.Materials))
	for index, source := range configured.Materials {
		field := fmt.Sprintf("materials[%d]", index)
		if source.ItemID == 0 {
			return nil, fmt.Errorf("avatar forge %s.item_id is zero", field)
		}
		if _, found := rules[source.ItemID]; found {
			return nil, fmt.Errorf("avatar forge material %d is duplicated", source.ItemID)
		}
		if source.SuccessPercent == nil {
			return nil, fmt.Errorf("avatar forge material %d has no success_percent", source.ItemID)
		}
		if *source.SuccessPercent < 0 || *source.SuccessPercent > 100 {
			return nil, fmt.Errorf("avatar forge material %d success_percent is outside 0..100", source.ItemID)
		}
		if source.BlackPercent == nil {
			return nil, fmt.Errorf("avatar forge material %d has no black_percent", source.ItemID)
		}
		if *source.BlackPercent < 0 || *source.BlackPercent > 100 {
			return nil, fmt.Errorf("avatar forge material %d black_percent is outside 0..100", source.ItemID)
		}

		rule := MaterialRule{ItemID: source.ItemID, SuccessPercent: *source.SuccessPercent, BlackPercent: *source.BlackPercent}
		levelTotal := uint64(0)
		seenLevels := make(map[byte]bool, len(source.LevelWeights))
		for value, weight := range source.LevelWeights {
			level, parseErr := strconv.ParseUint(value, 10, 8)
			if parseErr != nil || level < 1 || level > 5 {
				return nil, fmt.Errorf("avatar forge material %d has invalid level %q", source.ItemID, value)
			}
			if seenLevels[byte(level)] {
				return nil, fmt.Errorf("avatar forge material %d has duplicate level %d", source.ItemID, level)
			}
			seenLevels[byte(level)] = true
			if addErr := addForgeWeight(&levelTotal, weight); addErr != nil {
				return nil, fmt.Errorf("avatar forge material %d level_weights: %w", source.ItemID, addErr)
			}
			rule.LevelWeights = append(rule.LevelWeights, LevelWeight{Level: byte(level), Weight: weight})
		}
		if levelTotal == 0 {
			return nil, fmt.Errorf("avatar forge material %d level_weights has no positive weight", source.ItemID)
		}
		sortLevelWeights(rule.LevelWeights)

		colorTotal := uint64(0)
		seenColors := make(map[ForgeColor]bool, len(source.ColorWeights))
		for value, weight := range source.ColorWeights {
			color, found := forgeColorByName(strings.ToLower(strings.TrimSpace(value)))
			if !found || color == ForgeColorWhite || color == ForgeColorBlack {
				return nil, fmt.Errorf("avatar forge material %d has invalid ordinary color %q", source.ItemID, value)
			}
			if seenColors[color] {
				return nil, fmt.Errorf("avatar forge material %d has duplicate color %q", source.ItemID, value)
			}
			seenColors[color] = true
			if addErr := addForgeWeight(&colorTotal, weight); addErr != nil {
				return nil, fmt.Errorf("avatar forge material %d color_weights: %w", source.ItemID, addErr)
			}
			rule.ColorWeights = append(rule.ColorWeights, ColorWeight{Color: color, Weight: weight})
		}
		if colorTotal == 0 {
			return nil, fmt.Errorf("avatar forge material %d color_weights has no positive weight", source.ItemID)
		}
		sortColorWeights(rule.ColorWeights)
		rules[rule.ItemID] = rule
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("avatar forge rules contain no materials")
	}
	return rules, nil
}

func forgeColorByName(name string) (ForgeColor, bool) {
	for color, candidate := range map[ForgeColor]string{
		ForgeColorWhite: "white", ForgeColorBlack: "black", ForgeColorBlue: "blue",
		ForgeColorRed: "red", ForgeColorGreen: "green", ForgeColorPurple: "purple",
	} {
		if name == candidate {
			return color, true
		}
	}
	return 0, false
}

func addForgeWeight(total *uint64, weight uint64) error {
	if ^uint64(0)-*total < weight {
		return fmt.Errorf("weight total overflows uint64")
	}
	*total += weight
	return nil
}

func sortLevelWeights(values []LevelWeight) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor].Level < values[cursor-1].Level; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}

func sortColorWeights(values []ColorWeight) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor].Color < values[cursor-1].Color; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}

func (catalog *Catalog) Plan(operation ForgeOperation, item game.ItemInfo, materialID uint16, entropy io.Reader) (ForgePlan, error) {
	if catalog == nil || !operation.Valid() || item.ItemID == 0 || !item.Active() {
		return ForgePlan{}, fmt.Errorf("invalid avatar forge request")
	}
	plan := ForgePlan{
		Operation: operation, TargetItemID: item.ItemID, MaterialItemID: materialID,
		PreviousEffect: item.ItemEffect, PreviousColor: item.ItemColor,
	}
	switch operation {
	case ForgeApply:
		definition, found := catalog.Items[item.ItemID]
		if !found {
			return ForgePlan{}, fmt.Errorf("item %d is not listed by avatarforge.ini", item.ItemID)
		}
		material, found := catalog.Materials[materialID]
		if !found {
			return ForgePlan{}, fmt.Errorf("item %d is not an avatar forge crystal", materialID)
		}
		plan.MaterialQuantity = 1
		plan.Effect = item.ItemEffect
		plan.Color = item.ItemColor
		succeeded, err := rollPercent(entropy, material.SuccessPercent)
		if err != nil {
			return ForgePlan{}, err
		}
		if !succeeded {
			return plan, nil
		}
		currentLevel := byte(0)
		if item.ItemEffect != 0 {
			current, ok := catalog.Effects[item.ItemEffect]
			if !ok || current.Class != definition.Class {
				return ForgePlan{}, fmt.Errorf("item %d has incompatible forge effect %d", item.ItemID, item.ItemEffect)
			}
			currentLevel = current.Level
		}
		levelIndex, err := chooseLevelIndex(entropy, material.LevelWeights)
		if err != nil {
			return ForgePlan{}, err
		}
		colorIndex, err := chooseColorIndex(entropy, material.ColorWeights)
		if err != nil {
			return ForgePlan{}, err
		}
		selectedLevel := material.LevelWeights[levelIndex].Level
		if currentLevel == 0 || selectedLevel > currentLevel {
			form, found := catalog.effectFor(definition.Class, selectedLevel)
			if !found {
				return ForgePlan{}, fmt.Errorf("no %s level %d effect form", definition.Class, selectedLevel)
			}
			plan.Effect = form.ID
		}
		plan.Succeeded = true
		plan.Color = byte(material.ColorWeights[colorIndex].Color)
		black, err := rollPercent(entropy, material.BlackPercent)
		if err != nil {
			return ForgePlan{}, err
		}
		if black {
			plan.Color = byte(ForgeColorBlack)
		}
	case ForgeSplit:
		plan.Succeeded = true
		if materialID != catalog.Split.ItemID {
			return ForgePlan{}, fmt.Errorf("split material %d, want %d", materialID, catalog.Split.ItemID)
		}
		if item.ItemColor == 0 {
			return ForgePlan{}, fmt.Errorf("item %d has no forge color to split", item.ItemID)
		}
		plan.MaterialQuantity = catalog.Split.Quantity
		plan.Effect = item.ItemEffect
		plan.Color = 0
	case ForgeRevert:
		plan.Succeeded = true
		if materialID != catalog.Revert.ItemID {
			return ForgePlan{}, fmt.Errorf("revert material %d, want %d", materialID, catalog.Revert.ItemID)
		}
		if item.ItemEffect == 0 && item.ItemColor == 0 {
			return ForgePlan{}, fmt.Errorf("item %d has no forge color or effect to revert", item.ItemID)
		}
		plan.MaterialQuantity = catalog.Revert.Quantity
		plan.Effect = 0
		plan.Color = 0
	}
	return plan, nil
}

func rollPercent(entropy io.Reader, percent int) (bool, error) {
	if entropy == nil {
		return false, fmt.Errorf("avatar forge entropy reader is nil")
	}
	if percent < 0 || percent > 100 {
		return false, fmt.Errorf("avatar forge success percent %d is outside 0..100", percent)
	}
	if percent == 0 || percent == 100 {
		return percent == 100, nil
	}
	// Discard the high tail so mapping a random byte to 0..99 is unbiased.
	const unbiasedLimit = 200
	for {
		var value [1]byte
		if _, err := io.ReadFull(entropy, value[:]); err != nil {
			return false, fmt.Errorf("read avatar forge probability entropy: %w", err)
		}
		if int(value[0]) >= unbiasedLimit {
			continue
		}
		return int(value[0])%100 < percent, nil
	}
}

func (catalog *Catalog) effectFor(class ForgeClass, level byte) (EffectForm, bool) {
	for _, form := range catalog.Effects {
		if form.Class == class && form.Level == level {
			return form, true
		}
	}
	return EffectForm{}, false
}

func chooseLevelIndex(entropy io.Reader, candidates []LevelWeight) (int, error) {
	weights := make([]uint64, len(candidates))
	for index, candidate := range candidates {
		weights[index] = candidate.Weight
	}
	return chooseWeightedIndex(entropy, weights)
}

func chooseColorIndex(entropy io.Reader, candidates []ColorWeight) (int, error) {
	weights := make([]uint64, len(candidates))
	for index, candidate := range candidates {
		weights[index] = candidate.Weight
	}
	return chooseWeightedIndex(entropy, weights)
}

func chooseWeightedIndex(entropy io.Reader, weights []uint64) (int, error) {
	if entropy == nil {
		return 0, fmt.Errorf("avatar forge entropy reader is nil")
	}
	total := uint64(0)
	onlyPositive := -1
	for index, weight := range weights {
		if err := addForgeWeight(&total, weight); err != nil {
			return 0, fmt.Errorf("avatar forge candidate weights: %w", err)
		}
		if weight > 0 {
			if onlyPositive == -1 {
				onlyPositive = index
			} else {
				onlyPositive = -2
			}
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("avatar forge candidate weights have no positive value")
	}
	if onlyPositive >= 0 {
		return onlyPositive, nil
	}
	// Reject the short leading range so modulo maps every accepted uint64 to
	// each weight bucket equally often.
	threshold := -total % total
	for {
		var raw [8]byte
		if _, err := io.ReadFull(entropy, raw[:]); err != nil {
			return 0, fmt.Errorf("read avatar forge entropy: %w", err)
		}
		value := binary.LittleEndian.Uint64(raw[:])
		if value < threshold {
			continue
		}
		selected := value % total
		for index, weight := range weights {
			if selected < weight {
				return index, nil
			}
			selected -= weight
		}
	}
}

func parseEffectName(name string) (ForgeClass, byte, error) {
	for _, class := range []ForgeClass{ForgeCap, ForgeBody, ForgeWing, ForgeBomb} {
		prefix := string(class)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		level, err := strconv.ParseUint(strings.TrimPrefix(name, prefix), 10, 8)
		if err != nil || level < 1 || level > 5 {
			return "", 0, fmt.Errorf("invalid avatar forge effect form %q", name)
		}
		return class, byte(level), nil
	}
	return "", 0, fmt.Errorf("invalid avatar forge effect form %q", name)
}

func parseCost(values map[string]string, name string) (MaterialCost, error) {
	itemID, err := parseUint(values["itemid"], 16, name+".itemid")
	if err != nil {
		return MaterialCost{}, err
	}
	quantity, err := parseUint(values["itemnum"], 32, name+".itemnum")
	if err != nil {
		return MaterialCost{}, err
	}
	return MaterialCost{ItemID: uint16(itemID), Quantity: uint32(quantity)}, nil
}

func parseUint(value string, bits int, field string) (uint64, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, bits)
	if err != nil {
		return 0, fmt.Errorf("parse avatar forge %s=%q: %w", field, value, err)
	}
	return parsed, nil
}
