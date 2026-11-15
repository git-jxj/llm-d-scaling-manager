package main

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// guideYAML is the capacity planner guide's machine-readable half, whose
// sample steps a reader runs to get a first plan without Prometheus.
const guideYAML = "../../docs/guides/capacity-planner/guide.yaml"

var (
	// sampleURL is the path, in the release's source tree, the guide's
	// sample.get step downloads.
	sampleURL = regexp.MustCompile(`/\$\{V\}/(\S+)`)
	// sampleOpen is the address the guide's sample.open step opens.
	sampleOpen = regexp.MustCompile(`'(http://127\.0\.0\.1:8080/#[^']+)'`)
)

// guideSample reads the guide's sample steps: the recording's path in the
// tree, and the address's fragment.
func guideSample(t *testing.T) (string, url.Values) {
	t.Helper()
	b, err := os.ReadFile(guideYAML)
	require.NoError(t, err)
	var g struct {
		Sample struct {
			Get  struct{ Run string } `yaml:"get"`
			Open struct{ Run string } `yaml:"open"`
		} `yaml:"sample"`
	}
	require.NoError(t, yaml.Unmarshal(b, &g))
	m := sampleURL.FindStringSubmatch(g.Sample.Get.Run)
	require.NotNil(t, m, "sample.get downloads no ${V}/... path:\n%s", g.Sample.Get.Run)
	o := sampleOpen.FindStringSubmatch(g.Sample.Open.Run)
	require.NotNil(t, o, "sample.open opens no address:\n%s", g.Sample.Open.Run)
	u, err := url.Parse(o[1])
	require.NoError(t, err)
	q, err := url.ParseQuery(u.Fragment)
	require.NoError(t, err)
	return m[1], q
}

// TestGuideSampleIsInTheTree holds the guide's sample download to a file the
// release's source tree has: the URL names a path at the release's tag.
func TestGuideSampleIsInTheTree(t *testing.T) {
	path, _ := guideSample(t)
	info, err := os.Stat(filepath.Join("../..", filepath.FromSlash(path)))
	require.NoError(t, err, "the guide downloads %s, which the tree does not have", path)
	require.True(t, info.Mode().IsRegular(), "%s is not a file", path)
}

// TestGuideSampleMeetsItsTargets prices the sample as the page does on the
// guide's address: no model named, the address's assumed V_P, prompt bucket,
// output length and targets, the rest from the inputs. The guide says the
// address sets targets the sample can meet: both are met at the rate the page
// opens on.
func TestGuideSampleMeetsItsTargets(t *testing.T) {
	path, q := guideSample(t)
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("../..", filepath.FromSlash(path)))
	require.NoError(t, err)
	name := q.Get("recording")
	require.NotEmpty(t, name, "the address names no recording")
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o600))
	h := whatifapi.New(whatifapi.Config{Resolver: &servedSources{recordings: dir}, WhatifSchema: "schema-v3"}).Handler()

	in, _ := apiInputs(t, h, name, "")
	num := func(k string) float64 {
		v, err := strconv.ParseFloat(q.Get(k), 64)
		require.NoError(t, err, "the address's %s", k)
		return v
	}

	// The page's planRequest: the policy's statistics, else its own (p90 TTFT,
	// mean TPOT); the policy's headroom, else 0.85; a prompt at the bucket's
	// upper edge.
	ttftStat, tpotStat, headroom := "p90", statMean, 0.85
	if in.Policy.TTFT != nil {
		ttftStat = in.Policy.TTFT.Statistic
	}
	if in.Policy.TPOT != nil {
		tpotStat = in.Policy.TPOT.Statistic
	}
	if in.Policy.Headroom > 0 {
		headroom = in.Policy.Headroom
	}
	var prompt int
	names := make([]string, 0, len(in.Observed.Buckets))
	for _, bk := range in.Observed.Buckets {
		names = append(names, bk.Name)
		if bk.Name == q.Get("priceAs") {
			prompt = bk.UpperTokens
		}
	}
	require.NotZero(t, prompt, "the address prices at bucket %q; the inputs have %s", q.Get("priceAs"), strings.Join(names, ", "))

	ttft, tpot := num("ttft"), num("tpot")
	plan, body := apiPlan(t, h, whatifapi.PlanRequest{
		InputsID: in.InputsID,
		Headroom: &headroom,
		Traffic:  &whatifapi.Traffic{Mix: &whatifapi.Mix{PromptTokens: float64(prompt), OutputTokens: num("out")}},
		SLO: &whatifapi.SLORequest{
			TTFT: &whatifapi.Objective{Statistic: ttftStat, Target: &ttft},
			TPOT: &whatifapi.Objective{Statistic: tpotStat, Target: &tpot},
		},
		Assume: &whatifapi.Assume{VP: fp(num("vp"))},
	})

	at := plan.TTFT.Line.AtRate.Figure
	if at.Value != nil && plan.TPOT != nil && plan.TPOT.AtRate.Figure.Value != nil {
		t.Logf("at %.2f r/s: TTFT %s %.3f s (target %.3f), TPOT %s %.4f s (target %.4f)", plan.TTFT.Line.AtRate.Rate,
			ttftStat, *at.Value, ttft, tpotStat, *plan.TPOT.AtRate.Figure.Value, tpot)
	}
	require.NotNil(t, at.Value, "TTFT at the rate is refused: %s", body)
	require.LessOrEqual(t, *at.Value, ttft, "TTFT %s at %.2f r/s misses the guide's target", ttftStat, plan.TTFT.Line.AtRate.Rate)
	require.NotNil(t, plan.TPOT, "no TPOT line")
	at = plan.TPOT.AtRate.Figure
	require.NotNil(t, at.Value, "TPOT at the rate is refused: %s", body)
	require.LessOrEqual(t, *at.Value, tpot, "TPOT %s at %.2f r/s misses the guide's target", tpotStat, plan.TPOT.AtRate.Rate)
}
