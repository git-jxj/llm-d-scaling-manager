package main

import (
	"strings"
	"testing"
)

// A percentile TPOT target is warned about, naming the percentile; a mean,
// none, or no target is not.
func TestWarnTPOTPercentile(t *testing.T) {
	for _, target := range []string{"30ms@p99", "4ms@p90"} {
		var b strings.Builder
		warnTPOTPercentile(&b, target)
		got := b.String()
		if !strings.Contains(got, "warning: --tpot "+target+" is a percentile") ||
			!strings.Contains(got, "slo.tpot is the mean") || !strings.Contains(got, "latency-definitions.md") {
			t.Errorf("%s: %q", target, got)
		}
		if _, pct, _ := strings.Cut(target, "@"); !strings.Contains(got, "the "+pct+" runs above it") {
			t.Errorf("%s: the percentile is not named: %q", target, got)
		}
	}
	for _, target := range []string{"", "30ms", "none", "30ms@mean", "30ms@"} {
		var b strings.Builder
		warnTPOTPercentile(&b, target)
		if b.Len() != 0 {
			t.Errorf("%q warned: %q", target, b.String())
		}
	}
	warnTPOTPercentile(nil, "30ms@p99")
}

// Each command that takes a TPOT target warns on a percentile one, before it
// reads its source: the warning does not depend on the answer.
func TestTPOTPercentileWarnedByEachCommand(t *testing.T) {
	for _, args := range [][]string{
		{"slo-to-rate", "--tpot", "30ms@p99", "--recording", "/nonexistent.jsonl.gz", "--model", "m"},
		{"at-replicas", "--tpot", "30ms@p99", "--recording", "/nonexistent.jsonl.gz", "--model", "m", "--prefill", "1", "--decode", "1"},
	} {
		var out, warn strings.Builder
		_ = run(t.Context(), args, &out, &warn)
		if !strings.Contains(warn.String(), "warning: --tpot 30ms@p99 is a percentile") {
			t.Errorf("%s: no warning; stderr %q", args[0], warn.String())
		}
	}
}
