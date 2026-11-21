package learnreplay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The states StatesFrom reads at earlier cycles, in its one replay, are the
// states StateFrom reads at each of them, field for field: a self-check
// fits on them as the planner would on a recording cut there.
func TestStatesFromEqualsStateAtEachCycle(t *testing.T) {
	for _, fixture := range []string{"ttft-staircase-b-on", "ols-sweep", "pd-restart"} {
		t.Run(fixture, func(t *testing.T) {
			rec := readGoldenRecording(t, fixture)
			src := BytesSource(rec)
			whole, err := StateFrom(context.Background(), src, Options{}, StateQuery{})
			require.NoError(t, err)
			also := []int{whole.Cycles / 2, whole.Cycles * 3 / 4, 0, whole.Cycles + 5}
			last, early, err := StatesFrom(context.Background(), src, Options{}, StateQuery{}, func(c int) []int {
				assert.Equal(t, whole.Cycles, c, "the cycle answered at")
				return also
			})
			require.NoError(t, err)
			assert.Equal(t, whole, last, "the last state")
			require.Len(t, early, 2, "the cycles before the last, once each")
			for _, c := range also[:2] {
				want, err := StateFrom(context.Background(), src, Options{}, StateQuery{Cycle: c})
				require.NoError(t, err)
				assert.Equal(t, want, early[c], "cycle %d", c)
			}
		})
	}
}
