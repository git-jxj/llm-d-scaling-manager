package whatif

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envelopeH200 is a class inside the q32 staircase's bucket B: 2 prefill and
// 2 decode pods on H200, 8000-token prompts, 200 out, Poisson.
func envelopeH200() FigureClass {
	return FigureClass{Topology: TopologyPD, Accelerator: "H200", PrefillReplicas: 2, DecodeReplicas: 2,
		PromptTokens: 8000, OutputTokens: 200, FlowControl: FlowControlNameOff}
}

// The shipped envelope parses, holds the staircase's rows, and validates a
// figure priced where the staircase measured it.
func TestEnvelope_TheShippedFileValidatesTheMeasuredRegion(t *testing.T) {
	e := DefaultEnvelope()
	require.NotEmpty(t, e.Rows)
	for _, q := range []float64{0, 0.9, 0.95} {
		v := e.Judge(Figure{Kind: FigureTTFT, Percentile: q, Class: envelopeH200(), KneeShare: 0.8})
		require.True(t, v.Validated(), "q %g: %s", q, v)
		assert.Regexp(t, `^validated: error [+-]\d+\.\.[+-]\d+% \(q32 staircase\)$`, v.String())
	}
	tpot := e.Judge(Figure{Kind: FigureMeanTPOT, Class: envelopeH200(), KneeShare: 0.1})
	assert.True(t, tpot.Validated(), tpot.String())
}

// Every category no run measured reads OUTSIDE VALIDATED REGION, and says
// which.
func TestEnvelope_UnvalidatedCategoriesSaySo(t *testing.T) {
	e := DefaultEnvelope()
	in := Figure{Kind: FigureTTFT, Percentile: 0.9, Class: envelopeH200(), KneeShare: 0.8}
	cases := []struct {
		name string
		edit func(*Figure)
		want string
	}{
		{"aggregated", func(f *Figure) { f.Class.Topology = TopologyAggregated }, "aggregated variants: never measured"},
		{"prefill's own latency", func(f *Figure) { f.Kind = FigurePrefillLatency }, "prefill's own latency: a client measures TTFT end to end only"},
		{"p99", func(f *Figure) { f.Percentile = 0.99 }, "p99, measured "},
		{"more than 2 prefill pods", func(f *Figure) { f.Class.PrefillReplicas = 4 }, "4 prefill pods, measured 2"},
		{"another accelerator", func(f *Figure) { f.Class.Accelerator = "A100" }, "accelerator A100, measured H200"},
		{"an unknown accelerator", func(f *Figure) { f.Class.Accelerator = "" }, "accelerator unknown, measured H200"},
		{"past the knee share", func(f *Figure) { f.KneeShare = 0.97 }, "at 0.97 of the knee, measured up to 0.90"},
		{"a longer prompt", func(f *Figure) { f.Class.PromptTokens = 32000 }, "prompt 32000 tokens, measured"},
		{"past the onset", func(f *Figure) { f.Flags = []Flag{AboveFlowControlOnset} }, "past the flow-control onset"},
		{"V_P assumed", func(f *Figure) { f.Flags = []Flag{AssumedVP} }, "V_P assumed, not learned"},
		{"a TPOT percentile", func(f *Figure) { f.Kind = FigurePercentileTPOT }, "TPOT percentiles: never scored against a client"},
		{"decode's knee", func(f *Figure) { f.Kind = FigureDecodeKnee }, "decode's knee: no measured run crossed it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := in
			c.edit(&f)
			v := e.Judge(f)
			assert.False(t, v.Validated())
			assert.Contains(t, v.String(), "OUTSIDE VALIDATED REGION ("+c.want)
		})
	}
}

// A kind no row and no Never entry names is outside, by its name.
func TestEnvelope_AnUnknownKindIsOutside(t *testing.T) {
	v := Envelope{}.Judge(Figure{Kind: "made-up", Class: envelopeH200()})
	assert.Equal(t, "OUTSIDE VALIDATED REGION (made-up: never measured)", v.String())
}

// A row judges a figure by each bound it sets, and by none it leaves unset.
func TestEnvelope_RegionBounds(t *testing.T) {
	lo, hi := -0.05, 0.15
	row := EnvelopeRow{Figure: FigureTTFT, Shape: "s", Source: "a run", Readings: 3, ErrorLo: &lo, ErrorHi: &hi,
		Region: Region{Topology: TopologyPD, ArrivalCV2: &Span{Lo: 0.8, Hi: 1.25}, FlowControl: []string{FlowControlNameOn}}}
	e := Envelope{Rows: []EnvelopeRow{row}}
	f := Figure{Kind: FigureTTFT, Class: FigureClass{Topology: TopologyPD, FlowControl: FlowControlNameOn}}
	v := e.Judge(f)
	require.True(t, v.Validated(), "Ca^2 0 reads as Poisson, and nothing else is bounded")
	assert.Equal(t, "validated: error -5..+15% (a run)", v.String())
	f.Class.ArrivalCV2 = 4
	assert.Equal(t, "OUTSIDE VALIDATED REGION (arrivals Ca^2 4, measured 0.8 to 1.25)", e.Judge(f).String())
	f.Class.ArrivalCV2, f.Class.FlowControl = 1, FlowControlNameOff
	assert.Equal(t, "OUTSIDE VALIDATED REGION (flow control off, measured on)", e.Judge(f).String())
}

// The error prints both sides, or the one a bounded reading gives, and
// never -0.
func TestEnvelope_ErrorText(t *testing.T) {
	x := func(v float64) *float64 { return &v }
	assert.Equal(t, "-5..+15%", ErrorText(x(-0.05), x(0.15)))
	assert.Equal(t, "+0..+14%", ErrorText(x(-0.004), x(0.14)))
	assert.Equal(t, "at most -6%", ErrorText(nil, x(-0.0555)))
	assert.Equal(t, "at least +5%", ErrorText(x(0.05), nil))
	assert.Equal(t, "unbounded", ErrorText(nil, nil))
}

// A row that judges nothing is refused when the file is read.
func TestEnvelope_ParseRefusesAnEmptyRow(t *testing.T) {
	for _, raw := range []string{
		`{"rows":[{"figure":"ttft","shape":"s","source":"r","region":{"topology":"pd"},"errorLo":0,"readings":0}]}`,
		`{"rows":[{"figure":"ttft","shape":"s","source":"r","region":{"topology":"pd"},"readings":2}]}`,
		`{"rows":[{"figure":"ttft","shape":"s","source":"r","region":{},"errorLo":0,"readings":2}]}`,
		`{"rows":[{"shape":"s","source":"r","region":{"topology":"pd"},"errorLo":0,"readings":2}]}`,
		`not json`,
	} {
		_, err := ParseEnvelope([]byte(raw))
		assert.Error(t, err, raw)
	}
}

// FleetClass reads the fleet's pods, shape, arrivals and flow control.
func TestEnvelope_FleetClass(t *testing.T) {
	fl := TTFTFleet{Prefill: Prefill{Replicas: 2, PromptTokens: 8000, ArrivalCV2: 4},
		Decode: &Decode{Replicas: 3, OutputTokens: 200}, FlowControl: FlowControl{State: FlowControlOn}}
	assert.Equal(t, FigureClass{Topology: TopologyPD, PrefillReplicas: 2, DecodeReplicas: 3, PromptTokens: 8000,
		OutputTokens: 200, ArrivalCV2: 4, FlowControl: FlowControlNameOn}, FleetClass(fl))
	fl.FlowControl.State, fl.Decode = FlowControlNoSeries, nil
	c := FleetClass(fl)
	assert.Equal(t, FlowControlNameOff, c.FlowControl, "no queue series is priced as off")
	assert.Zero(t, c.DecodeReplicas)
}
