package main

import (
	"flag"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// --experimental-ttft=v2 prices with the stratified service moments by
// default, on backtest and on serve; --rank-mixture-stratified=false reads
// each rank's moments from its own draws.
func TestRankMixtureStratified_OnByDefault(t *testing.T) {
	v2 := append(slices.Clone(backtestQ32), "--"+experimentalTTFTFlag+"="+experimentalTTFTV2, "-o", "json")
	cli := func(args ...string) *whatifapi.RankMixtureReport {
		out, _ := runCmd(t, append(slices.Clone(v2), args...)...)
		var bt whatifapi.BacktestResponse
		require.NoError(t, jsonStrict([]byte(out), &bt), out)
		require.NotNil(t, bt.RankMixture)
		return bt.RankMixture
	}
	assert.True(t, cli().Stratified, "backtest: on by default")
	assert.True(t, cli("--"+rankMixtureStratifiedFlag).Stratified)
	assert.False(t, cli("--"+rankMixtureStratifiedFlag+"=false").Stratified, "backtest: =false turns it off")

	served := func(f serveFlags) *whatifapi.RankMixtureReport {
		require.NoError(t, experimentalTTFTValue{switches: &f.experimentalTTFT, v2: &f.rankMixtureTTFT}.Set(experimentalTTFTV2))
		bt := servedV2Backtest(t, f, "", "")
		require.NotNil(t, bt.RankMixture)
		return bt.RankMixture
	}
	off := false
	assert.True(t, served(serveFlags{}).Stratified, "serve: on by default")
	assert.False(t, served(serveFlags{rankMixtureStratified: &off}).Stratified, "serve: =false turns it off")
}

// The switch parses the same on every command that takes it: absent or
// alone it is on, =false off.
func TestRankMixtureStratified_Flag(t *testing.T) {
	for _, c := range []struct {
		args []string
		off  bool
	}{{nil, false}, {[]string{"--" + rankMixtureStratifiedFlag}, false}, {[]string{"--" + rankMixtureStratifiedFlag + "=true"}, false},
		{[]string{"--" + rankMixtureStratifiedFlag + "=false"}, true}} {
		var bf backtestFlags
		fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		bf.registerAssume(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, !c.off, stratifiedOr(bf.rankMixtureStratified), "backtest %v", c.args)

		var af answerFlags
		fs = flag.NewFlagSet("at-replicas", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		af.register(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, !c.off, stratifiedOr(af.rankMixtureStratified), "at-replicas %v", c.args)
	}
	var given *bool
	assert.Error(t, optionalBool{&given, true}.Set("maybe"))
	assert.Nil(t, given)
}
