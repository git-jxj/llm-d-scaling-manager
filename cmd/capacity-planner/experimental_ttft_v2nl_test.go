package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --experimental-ttft=v2nl sets v2 and the non-linear variant where the
// flag takes it (backtest), and is refused where it does not (answer,
// serve); every other value clears the variant.
func TestExperimentalTTFTValue_V2NL(t *testing.T) {
	var sw, v2, nl bool
	v := experimentalTTFTValue{switches: &sw, v2: &v2, nl: &nl}
	require.NoError(t, v.Set(experimentalTTFTV2NL))
	assert.True(t, v2)
	assert.True(t, nl)
	assert.False(t, sw)
	assert.Equal(t, experimentalTTFTV2NL, v.String())
	require.NoError(t, v.Set(experimentalTTFTV2))
	assert.True(t, v2)
	assert.False(t, nl)
	assert.Equal(t, experimentalTTFTV2, v.String())
	require.NoError(t, v.Set(experimentalTTFTV2NL))
	require.NoError(t, v.Set("false"))
	assert.False(t, v2)
	assert.False(t, nl)
	// Without the variant's field the value is refused, and v2 is left as
	// it was.
	var sw2, v22 bool
	w := experimentalTTFTValue{switches: &sw2, v2: &v22}
	require.Error(t, w.Set(experimentalTTFTV2NL))
	assert.False(t, v22)
}

// =v2nl-line sets the variant and its line where the flag takes it, and is
// refused where it does not.
func TestExperimentalTTFTValue_V2NLLine(t *testing.T) {
	var sw, v2, nl, line bool
	v := experimentalTTFTValue{switches: &sw, v2: &v2, nl: &nl, line: &line}
	require.NoError(t, v.Set(experimentalTTFTV2NLLine))
	assert.True(t, v2)
	assert.True(t, nl)
	assert.True(t, line)
	assert.Equal(t, experimentalTTFTV2NLLine, v.String())
	require.NoError(t, v.Set(experimentalTTFTV2NL))
	assert.True(t, nl)
	assert.False(t, line)
	var sw2, v22, nl2 bool
	w := experimentalTTFTValue{switches: &sw2, v2: &v22, nl: &nl2}
	require.Error(t, w.Set(experimentalTTFTV2NLLine))
}
