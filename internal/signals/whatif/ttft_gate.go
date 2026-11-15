package whatif

// This file prices the EPP flow-control gate past the prefill onset
// (TTFTFleet.PriceRouterWait; docs/proposals/slo-capacity-planner-ttft.md,
// "As built: the router stage"). The mechanism is llm-d-router v0.11.0's:
//
//   - The gate (pkg/epp/flowcontrol/controller/internal/processor.go,
//     dispatchCycle) runs every millisecond and on each enqueue, and
//     dispatches one queued request a cycle unless the pool's saturation is
//     at least 1. Saturation is the higher of the prefill and decode stages',
//     each the mean over the stage's pods of max(waiting/queueThreshold,
//     kvUsage/kvThreshold) (saturationdetector/utilization/detector.go,
//     Saturation). While the score is under 1 the whole router queue goes out
//     within milliseconds.
//   - The score is read from the last metrics poll, every 50 ms by default
//     (pkg/epp/datalayer/runtime.go, defaultRefreshInterval), but the engine's
//     waiting count it polls moves once an engine step: vLLM publishes its
//     scheduler stats when a step ends, and a prompt that arrives during a
//     step is not counted until the next one. Under load a step holds the
//     running prompts' prefill, so the score is a step stale.
//   - The prefill scorers (queue, KV utilization; the prefix-cache scorer
//     ties on unshared prompts) rank the pods on that same stale poll, so
//     every request released within one step goes to the same pod
//     (scorer/queuedepth/queue.go, Score; the max-score picker breaks only
//     exact ties).
//
// So past the onset the gate does not trickle requests to the pods as they
// free: it lets a step's worth of arrivals through to one pod, closes when
// that pod's count is published, and stays closed while one pod drains the
// burst, with the other pods short of work. It adds time; it does not only
// move it from the pod to the router. A replay of the Qwen3-32B staircase's
// arrivals through this mechanism holds nothing at the steps where the router
// held nothing and holds at the steps where it held, and holds nothing with
// the bursts spread round-robin instead.
//
// The stage, at rate lambda past the onset:
//
//   - Delta, the score's staleness: the prefill pod's mean sojourn at the
//     rate (its own work and its stretch), or the EPP's poll interval
//     (EPPRefreshSeconds) where that is longer. Assumed: the engine's step
//     holds a running prompt's whole prefill (max_num_batched_tokens at least
//     the running prompts' tokens); a smaller budget steps faster and
//     publishes fresher, so this errs long.
//   - b = lambda Delta, the burst: one staleness period's arrivals, to one pod.
//   - m = b E[S], the time one pod takes to drain it at V_P (margined).
//   - The gate cycles open for Delta and closed for m: a request meets it
//     closed with probability pi = m/(m + Delta) and then waits the closed
//     period's residual, priced exponential at mean m/2 (W_router). Every
//     request is in a burst, and waits behind its burst-mates on the pod,
//     priced exponential at mean m/2 as well (W_burst). The closed period
//     ignores the threshold's slack (queueThreshold per pod, and the running
//     cap) and the burst wait is added to the processor-sharing stretch, not
//     put in its place: both err long.
//
// Both waits are folded into the processor-sharing mixture as one wait in
// front of every part (stageMix.withWait), independent of it.

// EPPRefreshSeconds is the EPP's metrics poll interval, in seconds: its
// --refresh-metrics-interval default (llm-d-router v0.11.0,
// pkg/epp/server/options.go, MinRefreshMetricsInterval). Not read from the
// cluster: the gate's staleness is the larger of it and the engine step, and
// at any prompt the planner prices the step is the larger.
const EPPRefreshSeconds = 0.05

// gateStage is the flow-control gate past the prefill onset at one rate.
type gateStage struct {
	// staleness is Delta, burst b, drain m (seconds), closed pi.
	staleness, burst, drain, closed float64
}

// newGateStage is the gate at rate on the prefill plan pf, whose mean
// prefill stretch over the requests (psParts.prefill) is stretch.
func newGateStage(pf prefillPlan, rate, stretch float64) gateStage {
	delta := max(EPPRefreshSeconds, pf.work+stretch)
	b := rate * delta
	m := b * pf.work
	return gateStage{staleness: delta, burst: b, drain: m, closed: m / (m + delta)}
}

// routerWait is W_router: 0 with probability 1 - pi, else exponential at
// mean m/2.
func (g gateStage) routerWait() waitTail {
	if !(g.drain > 0) {
		return waitTail{}
	}
	return expWait(g.closed, 2/g.drain)
}

// burstWait is W_burst: exponential at mean m/2, the wait behind the burst's
// other requests on its pod.
func (g gateStage) burstWait() waitTail {
	if !(g.drain > 0) {
		return waitTail{}
	}
	return expWait(1, 2/g.drain)
}

// wait is W_router + W_burst, taken independent.
func (g gateStage) wait() waitTail {
	// Both are single unshifted pieces (or empty), which sum always takes.
	w, _ := g.routerWait().sum(g.burstWait())
	return w
}
