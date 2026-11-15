package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
)

// The context window is the smallest --max-model-len of the fleet's own
// variants: another variant's is not the fleet's, a variant with none set
// does not lower it, and one whose argument could not be read makes it
// unknown.
func TestContextOf(t *testing.T) {
	variants := func(prefill, decode int64) []learnreplay.VariantState {
		return []learnreplay.VariantState{
			{Variant: "p", MaxModelLen: prefill}, {Variant: "d", MaxModelLen: decode},
			{Variant: "other", MaxModelLen: 4096}, {Variant: "unset", MaxModelLen: 0},
		}
	}
	for _, c := range []struct {
		name            string
		prefill, decode int64
		want            float64
	}{
		{"prefill smaller", 8192, 16384, 8192},
		{"decode smaller", 16384, 8192, 8192},
		{"prefill unset", 0, 16384, 16384},
	} {
		got := contextOf(variants(c.prefill, c.decode), "p", "d", "unset")
		require.NotNil(t, got.Value, "%s: %+v", c.name, got.Refused)
		assert.InDelta(t, c.want, *got.Value, 0, c.name)
		assert.Equal(t, "tokens", got.Unit)
	}
	none := contextOf(variants(0, 0), "p", "d")
	require.NotNil(t, none.Refused)
	assert.Nil(t, none.Value)
	assert.Equal(t, reasonNotRecorded, none.Refused.Reason)
	assert.Contains(t, none.Refused.Detail, "no readable --max-model-len")

	unread := variants(8192, 16384)
	unread[1].MaxModelLenUnresolved = true
	u := contextOf(unread, "p", "d")
	require.NotNil(t, u.Refused, "a variant whose window is unreadable left the smaller one standing")
	assert.Equal(t, reasonUnreadableArgument, u.Refused.Reason)
	assert.Contains(t, u.Refused.Detail, "variant d")
	unread[1].Variant = "elsewhere"
	assert.NotNil(t, contextOf(unread, "p", "d").Value, "another variant's unreadable argument is not the fleet's")
}
