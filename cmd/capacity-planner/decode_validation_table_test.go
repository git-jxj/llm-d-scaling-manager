package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The proposal's "Measured on the goldens" table quotes every decode knee
// figure that testdata/decode-validation.golden freezes, and no other: a
// golden that gains a knee (or loses one) moves the table in the same PR.
// The figures are compared per golden, as "<golden> <knee error>", so a
// knee that moves from one golden to another is caught too.
func TestDecodeValidationTableMatchesTheGolden(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "decode-validation.golden"))
	require.NoError(t, err)
	goldenRe := regexp.MustCompile(`(?m)^(\S+) .*; knee ([+-][0-9.]+%) over \d+ rows`)
	ms := goldenRe.FindAllStringSubmatch(string(golden), -1)
	fromGolden := make([]string, 0, len(ms))
	for _, m := range ms {
		fromGolden = append(fromGolden, m[1]+" "+m[2])
	}
	require.NotEmpty(t, fromGolden, "no decode knee figure in the golden")

	page, err := os.ReadFile("../../docs/proposals/slo-capacity-planner.md")
	require.NoError(t, err)
	fromTable := measuredKneeFigures(t, strings.ReplaceAll(string(page), "\r\n", "\n"))
	require.NotEmpty(t, fromTable, "no decode knee figure in the proposal's table")

	slices.Sort(fromGolden)
	slices.Sort(fromTable)
	assert.Equal(t, fromGolden, fromTable,
		"the knee figures in docs/proposals/slo-capacity-planner.md (Measured on the goldens) and testdata/decode-validation.golden disagree")
}

// measuredKneeFigures reads "<golden> <knee error>" from every row of the
// first table under "### Measured on the goldens" whose "Knee error" cell
// is a figure rather than a SKIPPED reason.
func measuredKneeFigures(t *testing.T, page string) []string {
	t.Helper()
	_, section, ok := strings.Cut(page, "### Measured on the goldens\n")
	require.True(t, ok, "no Measured on the goldens section")
	nameRe := regexp.MustCompile("^`([^`]+)`")
	figureRe := regexp.MustCompile(`^[+-][0-9.]+%$`)
	kneeCol := -1
	var out []string
	inTable := false
	for line := range strings.SplitSeq(section, "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		inTable = true
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if kneeCol < 0 {
			kneeCol = slices.Index(cells, "Knee error")
			require.GreaterOrEqual(t, kneeCol, 0, "the first table has no Knee error column: %q", line)
			continue
		}
		if kneeCol >= len(cells) || !figureRe.MatchString(cells[kneeCol]) {
			continue
		}
		m := nameRe.FindStringSubmatch(cells[0])
		require.NotNil(t, m, "a table row names no golden: %q", line)
		out = append(out, m[1]+" "+cells[kneeCol])
	}
	return out
}
