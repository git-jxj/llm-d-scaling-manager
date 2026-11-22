package main

import (
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
)

// --experimental-ttft turns on, for one answer, the TTFT model's replay
// switches that are not in learnreplay.PlannerSwitches (experimentalTTFT):
// the combination that read closest to a production P/D fleet's measured
// TTFT on one holdout, and failed a second. It is read where
// --prefill-vp-plateau is (answerFlags.replayOptions): every command that
// replays a recording reads its state there, the server and day-plan,
// backtest and predict through whatifapi.InputsRequest.ExperimentalTTFT.
// Every prefill and TTFT figure of such an answer is flagged
// whatif.ExperimentalTTFT, which the envelope names first, so it reads
// OUTSIDE VALIDATED REGION.

// experimentalTTFTFlag is the flag's name.
const experimentalTTFTFlag = "experimental-ttft"

// experimentalTTFTUsage is the flag's help, on every command that takes it.
const experimentalTTFTUsage = "experimental, for demonstration, not validated: price TTFT with the TTFT model's switches that are off by default " +
	"(processor sharing by the engine's flags, the router hop from window means, V_P raised to the served work, " +
	"the whole window's V_P, the computed-token hit, the cache tier's load and KV wait). " +
	"On one production P/D fleet's holdout its TTFT read about 1.1x measured at the mean, 1.3x at p90 and 1.6x at p95, " +
	"and it failed a second holdout split. Every prefill and TTFT figure is flagged ExperimentalTTFT and reads OUTSIDE VALIDATED REGION. Off by default"

// experimentalTTFT is the replay switches --experimental-ttft turns on, by
// package, in the order the assumed line names them.
func experimentalTTFT() []struct{ pkg, name string } {
	return []struct{ pkg, name string }{
		{learnreplay.PackageSaturation, saturation.SwitchPrefillVPWindowBest},
		{learnreplay.PackageSaturation, saturation.SwitchPrefillVPAtServedWork},
		{learnreplay.PackageSLO, slo.SwitchTTFTComputedHit},
		{learnreplay.PackageSLO, slo.SwitchTTFTTierLoad},
		{learnreplay.PackageSLO, slo.SwitchTTFTTierKVWait},
		{learnreplay.PackageSLO, slo.SwitchTTFTPSByEngine},
		{learnreplay.PackageSLO, slo.SwitchTTFTHopWindowMeans},
	}
}

// experimentalTTFTNote is the assumed line of an answer read with
// --experimental-ttft: what the model is, and every switch it turned on.
func experimentalTTFTNote() string {
	names := make([]string, 0, len(experimentalTTFT()))
	for _, s := range experimentalTTFT() {
		names = append(names, s.name)
	}
	return "experimental TTFT model (--" + experimentalTTFTFlag + ", not validated): replay switches on: " + strings.Join(names, ", ")
}
