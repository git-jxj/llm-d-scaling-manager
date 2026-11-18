package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// q32Class is the q32 staircase's bucket B as the run priced it, on the
// committed recording.
func q32Class() whatif.FigureClass {
	return whatif.FigureClass{Topology: whatif.TopologyPD, Accelerator: "H200", PrefillReplicas: 2, DecodeReplicas: 2,
		PromptTokens: 8000, OutputTokens: 200, PromptCV: 0.0577, ArrivalCV2: 1, FlowControl: whatif.FlowControlNameOff,
		Model: "model-02", TensorParallel: 2, PrefillTensorParallel: 2}
}

// The plan's figures in the measured region read validated, without the
// UnvalidatedTTFT flag beside the label; each of round 2's holes reads
// outside: a knee with flow control on or not known, another tensor
// parallelism, an assumed router hop, a stated batch cap.
func TestValidate_TheQ32RegionAndItsHoles(t *testing.T) {
	ttft := Figure{Value: ptr(5.0), Unit: unitSeconds, Flags: []string{string(whatif.UnvalidatedTTFT)}}
	in := validate(ttft, whatif.FigureTTFT, 0.9, q32Class(), 0.8)
	require.NotNil(t, in.Validation)
	assert.True(t, in.Validation.Validated, in.Validation.Label)
	assert.Empty(t, in.Flags, "a validated figure is not flagged unvalidated")

	knee := Figure{Value: ptr(4.05), Unit: unitRate}
	require.True(t, validate(knee, whatif.FigurePrefillKnee, 0, q32Class(), 0).Validation.Validated)
	for _, c := range []struct {
		name string
		f    Figure
		kind whatif.FigureKind
		q    float64
		edit func(*whatif.FigureClass)
		want string
	}{
		{"knee, flow control on", knee, whatif.FigurePrefillKnee, 0, func(c *whatif.FigureClass) { c.FlowControl = whatif.FlowControlNameOn }, "flow control on, measured off"},
		{"knee, flow control unknown", knee, whatif.FigurePrefillKnee, 0, func(c *whatif.FigureClass) { c.FlowControl = "" }, "flow control unknown, measured off"},
		{"TTFT, TP 4", ttft, whatif.FigureTTFT, 0.9, func(c *whatif.FigureClass) { c.TensorParallel = 4 }, "decode tensor parallelism 4, measured 2"},
		{"TTFT, batch cap stated", ttft, whatif.FigureTTFT, 0.9, func(c *whatif.FigureClass) { c.Stated = []string{"--batch-cap"} }, "--batch-cap stated, which the run did not price"},
	} {
		t.Run(c.name, func(t *testing.T) {
			class := q32Class()
			c.edit(&class)
			got := validate(c.f, c.kind, c.q, class, 0.8)
			require.NotNil(t, got.Validation)
			assert.False(t, got.Validation.Validated)
			assert.Equal(t, c.want, got.Validation.Outside)
		})
	}
	hop := ttft
	hop.Flags = []string{string(whatif.UnvalidatedTTFT), string(whatif.RouterHopAssumed)}
	got := validate(hop, whatif.FigureTTFT, 0.9, q32Class(), 0.8)
	assert.Equal(t, "router hop assumed, not learned", got.Validation.Outside)
	assert.Contains(t, got.Flags, string(whatif.UnvalidatedTTFT), "an outside figure keeps the flag")
}
