package battleai

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sync"
)

const (
	nativeActorVersion  = 1
	nativeActorDTypeF32 = 1
	nativeDigestSize    = sha256.Size
)

var nativeTensorOrder = [...]string{
	"spatial_encoder.0.weight", "spatial_encoder.0.bias",
	"spatial_encoder.2.weight", "spatial_encoder.2.bias",
	"spatial_encoder.4.weight", "spatial_encoder.4.bias",
	"agent_encoder.0.weight", "agent_encoder.0.bias",
	"agent_encoder.1.weight", "agent_encoder.1.bias",
	"agent_encoder.3.weight", "agent_encoder.3.bias",
	"actor.weight", "actor.bias",
}

const (
	nativeRoutedExpertCount           = 5
	nativeMultiplayerExpertCount      = 2
	nativeMultiplayerRouteTemperature = float32(0.2)
	nativeRouteFeatureCount           = 8
	nativeBehaviorChannelBase         = 64
)

var nativeRoutedTensorOrder = func() []string {
	result := append([]string(nil), nativeTensorOrder[:]...)
	for expert := 0; expert < nativeRoutedExpertCount; expert++ {
		prefix := fmt.Sprintf("expert_residuals.%d", expert)
		result = append(result, prefix+".weight", prefix+".bias")
	}
	return append(result,
		"behavior_router.0.weight", "behavior_router.0.bias",
		"behavior_router.2.weight", "behavior_router.2.bias",
	)
}()

var nativeRoutedMultiplayerTensorOrder = append(
	append([]string(nil), nativeRoutedTensorOrder...),
	"multiplayer_residual.weight", "multiplayer_residual.bias",
)

var nativeRoutedMultiplayerExpertTensorOrder = func() []string {
	result := append([]string(nil), nativeRoutedMultiplayerTensorOrder...)
	for expert := 0; expert < nativeMultiplayerExpertCount; expert++ {
		prefix := fmt.Sprintf("multiplayer_expert_residuals.%d", expert)
		result = append(result, prefix+".weight", prefix+".bias")
	}
	return append(result,
		"multiplayer_expert_router.weight", "multiplayer_expert_router.bias",
	)
}()

var nativeRoutedMultiplayerContextTensorOrder = append(
	append([]string(nil), nativeRoutedMultiplayerTensorOrder...),
	"multiplayer_context_spatial_encoder.0.weight", "multiplayer_context_spatial_encoder.0.bias",
	"multiplayer_context_spatial_encoder.2.weight", "multiplayer_context_spatial_encoder.2.bias",
	"multiplayer_context_spatial_encoder.4.weight", "multiplayer_context_spatial_encoder.4.bias",
	"multiplayer_context_agent_encoder.0.weight", "multiplayer_context_agent_encoder.0.bias",
	"multiplayer_context_agent_encoder.1.weight", "multiplayer_context_agent_encoder.1.bias",
	"multiplayer_context_agent_encoder.3.weight", "multiplayer_context_agent_encoder.3.bias",
	"multiplayer_context_actor.weight", "multiplayer_context_actor.bias",
)

var nativeMultiplayerRoleTensorOrder = func() []string {
	result := make([]string, 0, nativeMultiplayerExpertCount*2)
	for role := 0; role < nativeMultiplayerExpertCount; role++ {
		prefix := fmt.Sprintf("multiplayer_role_residuals.%d", role)
		result = append(result, prefix+".weight", prefix+".bias")
	}
	return result
}()

var nativeRoutedMultiplayerContextRoleTensorOrder = append(
	append([]string(nil), nativeRoutedMultiplayerContextTensorOrder...),
	nativeMultiplayerRoleTensorOrder...,
)

var nativeRoutedMultiplayerContextTacticalTensorOrder = append(
	append([]string(nil), nativeRoutedMultiplayerContextTensorOrder...),
	"tactical_residual.weight", "tactical_residual.bias",
	"tactical_gate.weight", "tactical_gate.bias",
)

var nativeRoutedMultiplayerContextTacticalStackTensorOrder = append(
	append([]string(nil), nativeRoutedMultiplayerContextTacticalTensorOrder...),
	"finisher_residual.weight", "finisher_residual.bias",
	"finisher_gate.weight", "finisher_gate.bias",
)

var nativeRoutedMultiplayerRoleTensorOrder = append(
	append([]string(nil), nativeRoutedMultiplayerTensorOrder...),
	nativeMultiplayerRoleTensorOrder...,
)

func nativeVariantPreservesTopology(variantID uint16) bool {
	return variantID >= 3 && variantID <= 11
}

func nativeVariantRouted(variantID uint16) bool {
	return variantID >= 4 && variantID <= 11
}

func nativeVariantMultiplayer(variantID uint16) bool {
	return variantID >= 5 && variantID <= 11
}

func nativeVariantContext(variantID uint16) bool {
	return variantID == 7 || variantID == 8 || variantID == 10 || variantID == 11
}

func nativeVariantTactical(variantID uint16) bool {
	return variantID == 10 || variantID == 11
}

func nativeVariantFinisher(variantID uint16) bool {
	return variantID == 11
}

func nativeVariantRoles(variantID uint16) bool {
	return variantID == 8 || variantID == 9
}

func nativeTensorOrderForVariant(variantID uint16) ([]string, bool) {
	switch variantID {
	case 1, 2, 3:
		return nativeTensorOrder[:], true
	case 4:
		return nativeRoutedTensorOrder, true
	case 5:
		return nativeRoutedMultiplayerTensorOrder, true
	case 6:
		return nativeRoutedMultiplayerExpertTensorOrder, true
	case 7:
		return nativeRoutedMultiplayerContextTensorOrder, true
	case 8:
		return nativeRoutedMultiplayerContextRoleTensorOrder, true
	case 9:
		return nativeRoutedMultiplayerRoleTensorOrder, true
	case 10:
		return nativeRoutedMultiplayerContextTacticalTensorOrder, true
	case 11:
		return nativeRoutedMultiplayerContextTacticalStackTensorOrder, true
	default:
		return nil, false
	}
}

type nativeTensor struct {
	shape []int
	data  []float32
}

type nativeWorkspace struct {
	augmented      []float32
	selfMask       []bool
	activeChannels []int
	baseA          []float32
	baseB          []float32
	contextA       []float32
	contextB       []float32
	encoded        []float32
	contextEncoded []float32
}

// NativeRunner is a dependency-free CPU implementation of the compact actor
// graph. It is immutable after loading and safe for concurrent inference.
type NativeRunner struct {
	TensorVersion uint16
	VariantID     uint16
	Channels      int
	Scalars       int
	Actions       int
	Height        int
	Width         int
	tensors       map[string]nativeTensor
	workspacePool sync.Pool
}

func resizeNativeFloatBuffer(buffer []float32, size int, clearValues bool) []float32 {
	if cap(buffer) < size {
		return make([]float32, size)
	}
	buffer = buffer[:size]
	if clearValues {
		clear(buffer)
	}
	return buffer
}

func resizeNativeBoolBuffer(buffer []bool, size int) []bool {
	if cap(buffer) < size {
		return make([]bool, size)
	}
	buffer = buffer[:size]
	clear(buffer)
	return buffer
}

func (runner *NativeRunner) acquireWorkspace() *nativeWorkspace {
	if value := runner.workspacePool.Get(); value != nil {
		return value.(*nativeWorkspace)
	}
	return &nativeWorkspace{}
}

func (runner *NativeRunner) Contract() Contract {
	if runner == nil {
		return Contract{}
	}
	variant := "unknown"
	switch runner.VariantID {
	case 1:
		variant = "standard-v2"
	case 2:
		variant = "compact-v1"
	case 3:
		variant = "topology-v1"
	case 4:
		variant = "routed-experts-v1"
	case 5:
		variant = "routed-multiplayer-v1"
	case 6:
		variant = "routed-multiplayer-experts-v1"
	case 7:
		variant = "routed-multiplayer-context-v1"
	case 8:
		variant = "routed-multiplayer-context-roles-v1"
	case 9:
		variant = "routed-multiplayer-roles-v1"
	case 10:
		variant = "routed-multiplayer-context-tactical-v1"
	case 11:
		variant = "routed-multiplayer-context-tactical-stack-v1"
	}
	return Contract{
		Format: NativeActorFormat, ContractVersion: nativeActorVersion,
		ModelArchitectureVersion: 2, ModelVariant: variant,
		TensorVersion: runner.TensorVersion,
		Channels:      runner.Channels, Scalars: runner.Scalars, Actions: runner.Actions,
		Height: runner.Height, Width: runner.Width,
	}
}

// LoadNativeRunner validates the self-describing tensor layout and its
// trailing SHA-256 before making any weights available to inference.
func LoadNativeRunner(path string) (*NativeRunner, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read native AI model: %w", err)
	}
	return loadNativeRunner(payload)
}

func loadNativeRunner(payload []byte) (*NativeRunner, error) {
	if len(payload) < 24+nativeDigestSize {
		return nil, fmt.Errorf("native AI model is truncated")
	}
	body := payload[:len(payload)-nativeDigestSize]
	digest := sha256.Sum256(body)
	if !bytes.Equal(digest[:], payload[len(payload)-nativeDigestSize:]) {
		return nil, fmt.Errorf("native AI model checksum mismatch")
	}
	reader := bytes.NewReader(body)
	var magic [4]byte
	if _, err := reader.Read(magic[:]); err != nil || string(magic[:]) != "QTAI" {
		return nil, fmt.Errorf("native AI model has invalid magic")
	}
	readU16 := func() (uint16, error) {
		var value uint16
		err := binary.Read(reader, binary.LittleEndian, &value)
		return value, err
	}
	version, err := readU16()
	if err != nil || version != nativeActorVersion {
		return nil, fmt.Errorf("unsupported native AI model version %d", version)
	}
	fields := make([]uint16, 9)
	for index := range fields {
		fields[index], err = readU16()
		if err != nil {
			return nil, fmt.Errorf("read native AI header: %w", err)
		}
	}
	runner := &NativeRunner{
		TensorVersion: fields[0], VariantID: fields[1],
		Channels: int(fields[3]), Scalars: int(fields[4]), Actions: int(fields[5]),
		Height: int(fields[6]), Width: int(fields[7]), tensors: make(map[string]nativeTensor),
	}
	tensorCount := int(fields[2])
	tensorOrder, supportedVariant := nativeTensorOrderForVariant(runner.VariantID)
	if !supportedVariant {
		return nil, fmt.Errorf("unsupported native AI variant %d", runner.VariantID)
	}
	if tensorCount != len(tensorOrder) {
		return nil, fmt.Errorf("native AI tensor count %d, want %d", tensorCount, len(tensorOrder))
	}
	if runner.TensorVersion == 0 || runner.Channels <= 0 || runner.Scalars <= 0 || runner.Actions <= 0 || runner.Height <= 0 || runner.Width <= 0 {
		return nil, fmt.Errorf("native AI header contains zero dimensions")
	}
	for tensorIndex := 0; tensorIndex < tensorCount; tensorIndex++ {
		var nameLength uint16
		var dimensions, dataType uint8
		var count uint32
		if err := binary.Read(reader, binary.LittleEndian, &nameLength); err != nil ||
			binary.Read(reader, binary.LittleEndian, &dimensions) != nil ||
			binary.Read(reader, binary.LittleEndian, &dataType) != nil ||
			binary.Read(reader, binary.LittleEndian, &count) != nil {
			return nil, fmt.Errorf("read native AI tensor %d header", tensorIndex)
		}
		if nameLength == 0 || nameLength > 128 || dimensions == 0 || dimensions > 4 || dataType != nativeActorDTypeF32 || count == 0 || count > 10_000_000 {
			return nil, fmt.Errorf("native AI tensor %d has invalid metadata", tensorIndex)
		}
		nameBytes := make([]byte, int(nameLength))
		if _, err := reader.Read(nameBytes); err != nil {
			return nil, fmt.Errorf("read native AI tensor %d name: %w", tensorIndex, err)
		}
		name := string(nameBytes)
		if name != tensorOrder[tensorIndex] {
			return nil, fmt.Errorf("native AI tensor %d is %q, want %q", tensorIndex, name, tensorOrder[tensorIndex])
		}
		shape := make([]int, int(dimensions))
		elements := uint64(1)
		for dimension := range shape {
			var size uint32
			if err := binary.Read(reader, binary.LittleEndian, &size); err != nil || size == 0 {
				return nil, fmt.Errorf("native AI tensor %q has invalid shape", name)
			}
			shape[dimension] = int(size)
			elements *= uint64(size)
		}
		if elements != uint64(count) {
			return nil, fmt.Errorf("native AI tensor %q count %d disagrees with shape %v", name, count, shape)
		}
		data := make([]float32, int(count))
		for index := range data {
			var bits uint32
			if err := binary.Read(reader, binary.LittleEndian, &bits); err != nil {
				return nil, fmt.Errorf("read native AI tensor %q data: %w", name, err)
			}
			data[index] = math.Float32frombits(bits)
		}
		runner.tensors[name] = nativeTensor{shape: shape, data: data}
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("native AI model has %d trailing payload bytes", reader.Len())
	}
	if err := runner.validateShapes(); err != nil {
		return nil, err
	}
	return runner, nil
}

func (runner *NativeRunner) validateShapes() error {
	shape := func(name string) []int { return runner.tensors[name].shape }
	equal := func(actual []int, expected ...int) bool {
		if len(actual) != len(expected) {
			return false
		}
		for index := range actual {
			if actual[index] != expected[index] {
				return false
			}
		}
		return true
	}
	conv0 := shape("spatial_encoder.0.weight")
	conv1 := shape("spatial_encoder.2.weight")
	conv2 := shape("spatial_encoder.4.weight")
	if len(conv0) != 4 || len(conv1) != 4 || len(conv2) != 4 ||
		!equal(conv0[1:], runner.Channels+2, 3, 3) ||
		!equal(conv1[1:], conv0[0], 3, 3) || !equal(conv2[1:], conv1[0], 3, 3) {
		return fmt.Errorf("native AI convolution shapes are inconsistent")
	}
	for _, pair := range [][2]string{{"spatial_encoder.0.weight", "spatial_encoder.0.bias"}, {"spatial_encoder.2.weight", "spatial_encoder.2.bias"}, {"spatial_encoder.4.weight", "spatial_encoder.4.bias"}} {
		if !equal(shape(pair[1]), shape(pair[0])[0]) {
			return fmt.Errorf("native AI bias %q has invalid shape", pair[1])
		}
	}
	hidden := shape("agent_encoder.0.weight")[0]
	spatialFeatures := conv2[0] * 7
	if nativeVariantPreservesTopology(runner.VariantID) {
		spatialFeatures = conv2[0] * runner.Height * runner.Width
	}
	if !equal(shape("agent_encoder.0.weight"), hidden, spatialFeatures+runner.Scalars) ||
		!equal(shape("agent_encoder.0.bias"), hidden) ||
		!equal(shape("agent_encoder.1.weight"), hidden) ||
		!equal(shape("agent_encoder.1.bias"), hidden) ||
		!equal(shape("agent_encoder.3.weight"), hidden, hidden) ||
		!equal(shape("agent_encoder.3.bias"), hidden) ||
		!equal(shape("actor.weight"), runner.Actions, hidden) ||
		!equal(shape("actor.bias"), runner.Actions) {
		return fmt.Errorf("native AI dense layer shapes are inconsistent")
	}
	if nativeVariantRouted(runner.VariantID) {
		if runner.Channels < nativeBehaviorChannelBase+nativeRouteFeatureCount {
			return fmt.Errorf("routed native AI misses public behavior channels")
		}
		for expert := 0; expert < nativeRoutedExpertCount; expert++ {
			prefix := fmt.Sprintf("expert_residuals.%d", expert)
			if !equal(shape(prefix+".weight"), runner.Actions, hidden) ||
				!equal(shape(prefix+".bias"), runner.Actions) {
				return fmt.Errorf("native AI routed expert %d shapes are inconsistent", expert)
			}
		}
		routerHiddenShape := shape("behavior_router.0.weight")
		if len(routerHiddenShape) != 2 ||
			routerHiddenShape[1] != nativeRouteFeatureCount ||
			!equal(shape("behavior_router.0.bias"), routerHiddenShape[0]) ||
			!equal(shape("behavior_router.2.weight"), nativeRoutedExpertCount, routerHiddenShape[0]) ||
			!equal(shape("behavior_router.2.bias"), nativeRoutedExpertCount) {
			return fmt.Errorf("native AI behavior router shapes are inconsistent")
		}
		if nativeVariantMultiplayer(runner.VariantID) &&
			(!equal(shape("multiplayer_residual.weight"), runner.Actions, hidden) ||
				!equal(shape("multiplayer_residual.bias"), runner.Actions)) {
			return fmt.Errorf("native AI multiplayer residual shapes are inconsistent")
		}
		if runner.VariantID == 6 {
			for expert := 0; expert < nativeMultiplayerExpertCount; expert++ {
				prefix := fmt.Sprintf("multiplayer_expert_residuals.%d", expert)
				if !equal(shape(prefix+".weight"), runner.Actions, hidden) ||
					!equal(shape(prefix+".bias"), runner.Actions) {
					return fmt.Errorf("native AI multiplayer expert %d shapes are inconsistent", expert)
				}
			}
			if !equal(shape("multiplayer_expert_router.weight"), nativeMultiplayerExpertCount, hidden) ||
				!equal(shape("multiplayer_expert_router.bias"), nativeMultiplayerExpertCount) {
				return fmt.Errorf("native AI multiplayer expert router shapes are inconsistent")
			}
		}
		if nativeVariantContext(runner.VariantID) {
			contextConv0 := shape("multiplayer_context_spatial_encoder.0.weight")
			contextConv1 := shape("multiplayer_context_spatial_encoder.2.weight")
			contextConv2 := shape("multiplayer_context_spatial_encoder.4.weight")
			if len(contextConv0) != 4 || len(contextConv1) != 4 || len(contextConv2) != 4 ||
				!equal(contextConv0[1:], runner.Channels+2, 3, 3) ||
				!equal(contextConv1[1:], contextConv0[0], 3, 3) ||
				!equal(contextConv2[1:], contextConv1[0], 3, 3) {
				return fmt.Errorf("native AI context convolution shapes are inconsistent")
			}
			for _, pair := range [][2]string{
				{"multiplayer_context_spatial_encoder.0.weight", "multiplayer_context_spatial_encoder.0.bias"},
				{"multiplayer_context_spatial_encoder.2.weight", "multiplayer_context_spatial_encoder.2.bias"},
				{"multiplayer_context_spatial_encoder.4.weight", "multiplayer_context_spatial_encoder.4.bias"},
			} {
				if !equal(shape(pair[1]), shape(pair[0])[0]) {
					return fmt.Errorf("native AI context bias %q has invalid shape", pair[1])
				}
			}
			contextHidden := shape("multiplayer_context_agent_encoder.0.weight")[0]
			contextSpatialFeatures := contextConv2[0] * runner.Height * runner.Width
			if !equal(shape("multiplayer_context_agent_encoder.0.weight"), contextHidden, contextSpatialFeatures+runner.Scalars) ||
				!equal(shape("multiplayer_context_agent_encoder.0.bias"), contextHidden) ||
				!equal(shape("multiplayer_context_agent_encoder.1.weight"), contextHidden) ||
				!equal(shape("multiplayer_context_agent_encoder.1.bias"), contextHidden) ||
				!equal(shape("multiplayer_context_agent_encoder.3.weight"), contextHidden, contextHidden) ||
				!equal(shape("multiplayer_context_agent_encoder.3.bias"), contextHidden) ||
				!equal(shape("multiplayer_context_actor.weight"), runner.Actions, contextHidden) ||
				!equal(shape("multiplayer_context_actor.bias"), runner.Actions) {
				return fmt.Errorf("native AI context dense layer shapes are inconsistent")
			}
		}
		if nativeVariantRoles(runner.VariantID) {
			if runner.Channels < 82 {
				return fmt.Errorf("native AI roles require schema-10 actor-local role channels")
			}
			for role := 0; role < nativeMultiplayerExpertCount; role++ {
				prefix := fmt.Sprintf("multiplayer_role_residuals.%d", role)
				if !equal(shape(prefix+".weight"), runner.Actions, hidden) ||
					!equal(shape(prefix+".bias"), runner.Actions) {
					return fmt.Errorf("native AI multiplayer role %d shapes are inconsistent", role)
				}
			}
		}
		if nativeVariantTactical(runner.VariantID) {
			if !equal(shape("tactical_residual.weight"), runner.Actions, hidden) ||
				!equal(shape("tactical_residual.bias"), runner.Actions) ||
				!equal(shape("tactical_gate.weight"), 1, hidden) ||
				!equal(shape("tactical_gate.bias"), 1) {
				return fmt.Errorf("native AI tactical adapter shapes are inconsistent")
			}
		}
		if nativeVariantFinisher(runner.VariantID) {
			if !equal(shape("finisher_residual.weight"), runner.Actions, hidden) ||
				!equal(shape("finisher_residual.bias"), runner.Actions) ||
				!equal(shape("finisher_gate.weight"), 1, hidden) ||
				!equal(shape("finisher_gate.bias"), 1) {
				return fmt.Errorf("native AI finisher adapter shapes are inconsistent")
			}
		}
	}
	return nil
}

// RunActor evaluates exactly one visibility-bounded actor tensor.
func (runner *NativeRunner) RunActor(spatial []float32, scalars []float32, legal []uint8) ([]float32, error) {
	if runner == nil {
		return nil, fmt.Errorf("native AI runner is nil")
	}
	plane := runner.Height * runner.Width
	if len(spatial) != runner.Channels*plane || len(scalars) != runner.Scalars || len(legal) != runner.Actions {
		return nil, fmt.Errorf("native AI input dimensions do not match model")
	}
	workspace := runner.acquireWorkspace()
	defer runner.workspacePool.Put(workspace)
	var routeFeatures []float32
	var routeFeatureValues [nativeRouteFeatureCount]float32
	multiplayerGate := float32(0)
	teamRole := 0
	if nativeVariantRouted(runner.VariantID) {
		routeFeatures = routeFeatureValues[:]
		opponentCount := float32(0)
		for index := 0; index < plane; index++ {
			if spatial[11*plane+index]+spatial[12*plane+index] <= 0 {
				continue
			}
			opponentCount++
			for feature := range routeFeatures {
				routeFeatures[feature] += spatial[(nativeBehaviorChannelBase+feature)*plane+index]
			}
		}
		if opponentCount < 1 {
			opponentCount = 1
		}
		for feature := range routeFeatures {
			routeFeatures[feature] /= opponentCount
		}
		if nativeVariantMultiplayer(runner.VariantID) {
			for index := 0; index < plane; index++ {
				if spatial[9*plane+index]+spatial[10*plane+index] > 0 {
					multiplayerGate = 1
					break
				}
			}
			if multiplayerGate == 0 {
				rawOpponentCount := float32(0)
				for index := 0; index < plane; index++ {
					if spatial[11*plane+index]+spatial[12*plane+index] > 0 {
						rawOpponentCount++
					}
				}
				if rawOpponentCount > 1 {
					multiplayerGate = 1
				}
			}
		}
		if nativeVariantRoles(runner.VariantID) {
			roleZero := float32(0)
			roleOne := float32(0)
			for index := 0; index < plane; index++ {
				roleZero += spatial[80*plane+index]
				roleOne += spatial[81*plane+index]
			}
			if roleOne > roleZero {
				teamRole = 1
			}
		}
	}
	workspace.augmented = resizeNativeFloatBuffer(
		workspace.augmented, (runner.Channels+2)*plane, false,
	)
	augmented := workspace.augmented
	copy(augmented, spatial)
	workspace.selfMask = resizeNativeBoolBuffer(workspace.selfMask, plane)
	selfMask := workspace.selfMask
	selfCount := float32(0)
	selfRow := float32(0)
	selfCol := float32(0)
	for row := 0; row < runner.Height; row++ {
		rowValue := linearCoordinate(row, runner.Height)
		for col := 0; col < runner.Width; col++ {
			index := row*runner.Width + col
			if spatial[7*plane+index]+spatial[8*plane+index] <= 0 {
				continue
			}
			selfMask[index] = true
			selfCount++
			selfRow += rowValue
			selfCol += linearCoordinate(col, runner.Width)
		}
	}
	if selfCount < 1 {
		selfCount = 1
	}
	selfRow /= selfCount
	selfCol /= selfCount
	for row := 0; row < runner.Height; row++ {
		for col := 0; col < runner.Width; col++ {
			index := row*runner.Width + col
			augmented[runner.Channels*plane+index] = linearCoordinate(row, runner.Height) - selfRow
			augmented[(runner.Channels+1)*plane+index] = linearCoordinate(col, runner.Width) - selfCol
		}
	}
	// The public observation is intentionally sparse: many semantic planes are
	// completely zero for one actor/frame. Skip those planes only in the first
	// convolution. Scan after writing the two actor-relative coordinate planes;
	// pooled workspaces retain capacity and must never expose values from the
	// previous inference.
	workspace.activeChannels = workspace.activeChannels[:0]
	for channel := 0; channel < runner.Channels+2; channel++ {
		channelPlane := augmented[channel*plane : (channel+1)*plane]
		for _, value := range channelPlane {
			if value != 0 {
				workspace.activeChannels = append(workspace.activeChannels, channel)
				break
			}
		}
	}
	activeInputChannels := workspace.activeChannels
	var contextDone chan []float32
	if nativeVariantContext(runner.VariantID) && multiplayerGate > 0 {
		// The public multiplayer context tower and the base tower consume the
		// same immutable actor input and share no scratch state. Live rooms already
		// evaluate actors concurrently; one additional tower task per actor still
		// stays below the supported room's CPU parallelism on the deployment host.
		contextDone = make(chan []float32, 1)
		go func() {
			contextChannels0 := runner.tensors["multiplayer_context_spatial_encoder.0.weight"].shape[0]
			workspace.contextA = resizeNativeFloatBuffer(workspace.contextA, contextChannels0*plane, false)
			contextFeatures := runner.convInto(workspace.contextA, augmented, runner.Channels+2, activeInputChannels, "multiplayer_context_spatial_encoder.0")
			siluInPlace(contextFeatures)
			contextChannels1 := runner.tensors["multiplayer_context_spatial_encoder.2.weight"].shape[0]
			workspace.contextB = resizeNativeFloatBuffer(workspace.contextB, contextChannels1*plane, false)
			contextFeatures = runner.convInto(
				workspace.contextB, contextFeatures, contextChannels0, nil,
				"multiplayer_context_spatial_encoder.2",
			)
			siluInPlace(contextFeatures)
			contextChannels2 := runner.tensors["multiplayer_context_spatial_encoder.4.weight"].shape[0]
			workspace.contextA = resizeNativeFloatBuffer(workspace.contextA, contextChannels2*plane, false)
			contextFeatures = runner.convInto(
				workspace.contextA, contextFeatures, contextChannels1, nil,
				"multiplayer_context_spatial_encoder.4",
			)
			siluInPlace(contextFeatures)
			workspace.contextEncoded = resizeNativeFloatBuffer(
				workspace.contextEncoded, len(contextFeatures)+runner.Scalars, false,
			)
			copy(workspace.contextEncoded, contextFeatures)
			copy(workspace.contextEncoded[len(contextFeatures):], scalars)
			contextDone <- workspace.contextEncoded
		}()
	}
	baseChannels0 := runner.tensors["spatial_encoder.0.weight"].shape[0]
	workspace.baseA = resizeNativeFloatBuffer(workspace.baseA, baseChannels0*plane, false)
	features := runner.convInto(workspace.baseA, augmented, runner.Channels+2, activeInputChannels, "spatial_encoder.0")
	siluInPlace(features)
	baseChannels1 := runner.tensors["spatial_encoder.2.weight"].shape[0]
	workspace.baseB = resizeNativeFloatBuffer(workspace.baseB, baseChannels1*plane, false)
	features = runner.convInto(workspace.baseB, features, baseChannels0, nil, "spatial_encoder.2")
	siluInPlace(features)
	baseChannels2 := runner.tensors["spatial_encoder.4.weight"].shape[0]
	workspace.baseA = resizeNativeFloatBuffer(workspace.baseA, baseChannels2*plane, false)
	features = runner.convInto(workspace.baseA, features, baseChannels1, nil, "spatial_encoder.4")
	siluInPlace(features)
	channels := baseChannels2
	if nativeVariantPreservesTopology(runner.VariantID) {
		workspace.encoded = resizeNativeFloatBuffer(workspace.encoded, len(features)+runner.Scalars, false)
		encoded := workspace.encoded
		copy(encoded, features)
		copy(encoded[len(features):], scalars)
		var contextEncoded []float32
		if contextDone != nil {
			contextEncoded = <-contextDone
		}
		return runner.runDenseActor(
			encoded, legal, routeFeatures, multiplayerGate, contextEncoded, teamRole,
		), nil
	}
	workspace.encoded = resizeNativeFloatBuffer(workspace.encoded, channels*7+runner.Scalars, true)
	pooled := workspace.encoded
	validCount := float32(0)
	directionCounts := [4]float32{}
	for index := 0; index < plane; index++ {
		valid := spatial[index] > 0
		if valid {
			validCount++
		}
		row := index / runner.Width
		col := index % runner.Width
		relativeRow := linearCoordinate(row, runner.Height) - selfRow
		relativeCol := linearCoordinate(col, runner.Width) - selfCol
		direction := -1
		vertical := abs32(relativeRow) >= abs32(relativeCol)
		switch {
		case valid && vertical && relativeRow < 0:
			direction = 0
		case valid && !vertical && relativeCol > 0:
			direction = 1
		case valid && vertical && relativeRow > 0:
			direction = 2
		case valid && !vertical && relativeCol < 0:
			direction = 3
		}
		if direction >= 0 {
			directionCounts[direction]++
		}
		for channel := 0; channel < channels; channel++ {
			value := features[channel*plane+index]
			if selfMask[index] {
				pooled[channel] += value
			}
			if valid {
				pooled[channels+channel] += value
				maxIndex := channels*2 + channel
				if validCount == 1 || value > pooled[maxIndex] {
					pooled[maxIndex] = value
				}
			}
			if direction >= 0 {
				pooled[channels*(3+direction)+channel] += value
			}
		}
	}
	if validCount < 1 {
		validCount = 1
	}
	for channel := 0; channel < channels; channel++ {
		pooled[channel] /= selfCount
		pooled[channels+channel] /= validCount
		for direction := 0; direction < 4; direction++ {
			count := directionCounts[direction]
			if count < 1 {
				count = 1
			}
			pooled[channels*(3+direction)+channel] /= count
		}
	}
	copy(pooled[channels*7:], scalars)
	return runner.runDenseActor(
		pooled, legal, routeFeatures, multiplayerGate, nil, teamRole,
	), nil
}

func (runner *NativeRunner) runEncoder(encoded []float32, prefix string) []float32 {
	hidden := runner.linear(encoded, prefix+".0")
	runner.layerNorm(hidden, prefix+".1")
	siluInPlace(hidden)
	hidden = runner.linear(hidden, prefix+".3")
	siluInPlace(hidden)
	return hidden
}

func (runner *NativeRunner) runDenseActor(
	encoded []float32,
	legal []uint8,
	routeFeatures []float32,
	multiplayerGate float32,
	contextEncoded []float32,
	teamRole int,
) []float32 {
	hidden := runner.runEncoder(encoded, "agent_encoder")
	logits := runner.linear(hidden, "actor")
	if nativeVariantRouted(runner.VariantID) {
		routerHidden := runner.linear(routeFeatures, "behavior_router.0")
		siluInPlace(routerHidden)
		routeLogits := runner.linear(routerHidden, "behavior_router.2")
		routeProbabilities := softmax(routeLogits)
		for expert := 0; expert < nativeRoutedExpertCount; expert++ {
			residual := runner.linear(hidden, fmt.Sprintf("expert_residuals.%d", expert))
			for action := range logits {
				logits[action] += routeProbabilities[expert] * residual[action]
			}
		}
	}
	if nativeVariantMultiplayer(runner.VariantID) && multiplayerGate > 0 {
		residual := runner.linear(hidden, "multiplayer_residual")
		for action := range logits {
			logits[action] += multiplayerGate * residual[action]
		}
	}
	if runner.VariantID == 6 && multiplayerGate > 0 {
		routeLogits := runner.linear(hidden, "multiplayer_expert_router")
		for index := range routeLogits {
			routeLogits[index] /= nativeMultiplayerRouteTemperature
		}
		routeProbabilities := softmax(routeLogits)
		for expert := 0; expert < nativeMultiplayerExpertCount; expert++ {
			residual := runner.linear(hidden, fmt.Sprintf("multiplayer_expert_residuals.%d", expert))
			for action := range logits {
				logits[action] += multiplayerGate * routeProbabilities[expert] * residual[action]
			}
		}
	}
	if nativeVariantContext(runner.VariantID) && multiplayerGate > 0 {
		contextHidden := runner.runEncoder(
			contextEncoded, "multiplayer_context_agent_encoder",
		)
		residual := runner.linear(contextHidden, "multiplayer_context_actor")
		for action := range logits {
			logits[action] += multiplayerGate * residual[action]
		}
	}
	if nativeVariantRoles(runner.VariantID) && multiplayerGate > 0 {
		residual := runner.linear(
			hidden, fmt.Sprintf("multiplayer_role_residuals.%d", teamRole),
		)
		for action := range logits {
			logits[action] += multiplayerGate * residual[action]
		}
	}
	if nativeVariantTactical(runner.VariantID) {
		gateLogit := runner.linear(hidden, "tactical_gate")[0]
		gate := float32(1 / (1 + math.Exp(float64(-gateLogit))))
		residual := runner.linear(hidden, "tactical_residual")
		for action := range logits {
			logits[action] += gate * residual[action]
		}
	}
	if nativeVariantFinisher(runner.VariantID) {
		gateLogit := runner.linear(hidden, "finisher_gate")[0]
		gate := float32(1 / (1 + math.Exp(float64(-gateLogit))))
		residual := runner.linear(hidden, "finisher_residual")
		for action := range logits {
			logits[action] += gate * residual[action]
		}
	}
	for index := range logits {
		if legal[index] == 0 {
			logits[index] = -math.MaxFloat32
		}
	}
	return logits
}

func softmax(values []float32) []float32 {
	result := make([]float32, len(values))
	if len(values) == 0 {
		return result
	}
	maximum := values[0]
	for _, value := range values[1:] {
		if value > maximum {
			maximum = value
		}
	}
	total := float32(0)
	for index, value := range values {
		result[index] = float32(math.Exp(float64(value - maximum)))
		total += result[index]
	}
	if total == 0 {
		return result
	}
	for index := range result {
		result[index] /= total
	}
	return result
}

func (runner *NativeRunner) convInto(
	output, input []float32,
	inputChannels int,
	activeInputChannels []int,
	prefix string,
) []float32 {
	weight := runner.tensors[prefix+".weight"]
	bias := runner.tensors[prefix+".bias"].data
	outputChannels := weight.shape[0]
	height := runner.Height
	width := runner.Width
	plane := height * width
	output = output[:outputChannels*plane]
	for outChannel := 0; outChannel < outputChannels; outChannel++ {
		outputBase := outChannel * plane
		weightOutputBase := outChannel * inputChannels * 9
		for row := 0; row < height; row++ {
			rowBase := row * width
			firstInteriorCol := 1
			lastInteriorCol := width - 1
			if row == 0 || row+1 == height {
				firstInteriorCol = width
				lastInteriorCol = width
			}
			for col := 0; col < firstInteriorCol; col++ {
				cell := rowBase + col
				sum := bias[outChannel]
				if activeInputChannels == nil {
					for inChannel := 0; inChannel < inputChannels; inChannel++ {
						inputPlane := input[inChannel*plane : (inChannel+1)*plane]
						weightBase := weightOutputBase + inChannel*9
						kernel := weight.data[weightBase : weightBase+9]
						for kernelRow := 0; kernelRow < 3; kernelRow++ {
							inputRow := row + kernelRow - 1
							if inputRow < 0 || inputRow >= height {
								continue
							}
							for kernelCol := 0; kernelCol < 3; kernelCol++ {
								inputCol := col + kernelCol - 1
								if inputCol < 0 || inputCol >= width {
									continue
								}
								sum += inputPlane[inputRow*width+inputCol] * kernel[kernelRow*3+kernelCol]
							}
						}
					}
				} else {
					for _, inChannel := range activeInputChannels {
						inputPlane := input[inChannel*plane : (inChannel+1)*plane]
						weightBase := weightOutputBase + inChannel*9
						kernel := weight.data[weightBase : weightBase+9]
						for kernelRow := 0; kernelRow < 3; kernelRow++ {
							inputRow := row + kernelRow - 1
							if inputRow < 0 || inputRow >= height {
								continue
							}
							for kernelCol := 0; kernelCol < 3; kernelCol++ {
								inputCol := col + kernelCol - 1
								if inputCol < 0 || inputCol >= width {
									continue
								}
								sum += inputPlane[inputRow*width+inputCol] * kernel[kernelRow*3+kernelCol]
							}
						}
					}
				}
				output[outputBase+cell] = sum
			}
			for col := firstInteriorCol; col < lastInteriorCol; col++ {
				cell := rowBase + col
				upper := cell - width
				lower := cell + width
				sum := bias[outChannel]
				if activeInputChannels == nil {
					for inChannel := 0; inChannel < inputChannels; inChannel++ {
						inputPlane := input[inChannel*plane : (inChannel+1)*plane]
						weightBase := weightOutputBase + inChannel*9
						kernel := weight.data[weightBase : weightBase+9]
						sum += inputPlane[upper-1] * kernel[0]
						sum += inputPlane[upper] * kernel[1]
						sum += inputPlane[upper+1] * kernel[2]
						sum += inputPlane[cell-1] * kernel[3]
						sum += inputPlane[cell] * kernel[4]
						sum += inputPlane[cell+1] * kernel[5]
						sum += inputPlane[lower-1] * kernel[6]
						sum += inputPlane[lower] * kernel[7]
						sum += inputPlane[lower+1] * kernel[8]
					}
				} else {
					for _, inChannel := range activeInputChannels {
						inputPlane := input[inChannel*plane : (inChannel+1)*plane]
						weightBase := weightOutputBase + inChannel*9
						kernel := weight.data[weightBase : weightBase+9]
						sum += inputPlane[upper-1] * kernel[0]
						sum += inputPlane[upper] * kernel[1]
						sum += inputPlane[upper+1] * kernel[2]
						sum += inputPlane[cell-1] * kernel[3]
						sum += inputPlane[cell] * kernel[4]
						sum += inputPlane[cell+1] * kernel[5]
						sum += inputPlane[lower-1] * kernel[6]
						sum += inputPlane[lower] * kernel[7]
						sum += inputPlane[lower+1] * kernel[8]
					}
				}
				output[outputBase+cell] = sum
			}
			for col := lastInteriorCol; col < width; col++ {
				cell := rowBase + col
				sum := bias[outChannel]
				if activeInputChannels == nil {
					for inChannel := 0; inChannel < inputChannels; inChannel++ {
						inputPlane := input[inChannel*plane : (inChannel+1)*plane]
						weightBase := weightOutputBase + inChannel*9
						kernel := weight.data[weightBase : weightBase+9]
						for kernelRow := 0; kernelRow < 3; kernelRow++ {
							inputRow := row + kernelRow - 1
							if inputRow < 0 || inputRow >= height {
								continue
							}
							for kernelCol := 0; kernelCol < 3; kernelCol++ {
								inputCol := col + kernelCol - 1
								if inputCol < 0 || inputCol >= width {
									continue
								}
								sum += inputPlane[inputRow*width+inputCol] * kernel[kernelRow*3+kernelCol]
							}
						}
					}
				} else {
					for _, inChannel := range activeInputChannels {
						inputPlane := input[inChannel*plane : (inChannel+1)*plane]
						weightBase := weightOutputBase + inChannel*9
						kernel := weight.data[weightBase : weightBase+9]
						for kernelRow := 0; kernelRow < 3; kernelRow++ {
							inputRow := row + kernelRow - 1
							if inputRow < 0 || inputRow >= height {
								continue
							}
							for kernelCol := 0; kernelCol < 3; kernelCol++ {
								inputCol := col + kernelCol - 1
								if inputCol < 0 || inputCol >= width {
									continue
								}
								sum += inputPlane[inputRow*width+inputCol] * kernel[kernelRow*3+kernelCol]
							}
						}
					}
				}
				output[outputBase+cell] = sum
			}
		}
	}
	return output
}

func (runner *NativeRunner) linear(input []float32, prefix string) []float32 {
	weight := runner.tensors[prefix+".weight"]
	bias := runner.tensors[prefix+".bias"].data
	output := make([]float32, weight.shape[0])
	for row := range output {
		sum := bias[row]
		base := row * weight.shape[1]
		for col, value := range input {
			sum += value * weight.data[base+col]
		}
		output[row] = sum
	}
	return output
}

func (runner *NativeRunner) layerNorm(values []float32, prefix string) {
	weight := runner.tensors[prefix+".weight"].data
	bias := runner.tensors[prefix+".bias"].data
	mean := float32(0)
	for _, value := range values {
		mean += value
	}
	mean /= float32(len(values))
	variance := float32(0)
	for _, value := range values {
		difference := value - mean
		variance += difference * difference
	}
	variance /= float32(len(values))
	inverse := float32(1 / math.Sqrt(float64(variance+1e-5)))
	for index := range values {
		values[index] = (values[index]-mean)*inverse*weight[index] + bias[index]
	}
}

func siluInPlace(values []float32) {
	for index, value := range values {
		values[index] = value / (1 + float32(math.Exp(float64(-value))))
	}
}

func linearCoordinate(index, size int) float32 {
	if size <= 1 {
		return -1
	}
	return -1 + 2*float32(index)/float32(size-1)
}

func abs32(value float32) float32 {
	if value < 0 {
		return -value
	}
	return value
}
