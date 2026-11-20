package learnrec

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Raw.Member of data that is not valid JSON is false, never a panic: every
// prefix of an object, cut short, and objects missing a key, colon, value or
// comma. Of the whole object it is the member's bytes.
func TestMemberOfInvalidJSONIsFalse(t *testing.T) {
	whole := `{"Input": {"Namespace": "ns", "ModelID": "m\"x"}, "List": [1, {"y": "}]"}], "N": 3}`
	in, ok := Raw(whole).Member("Input")
	require.True(t, ok)
	assert.Equal(t, `{"Namespace": "ns", "ModelID": "m\"x"}`, string(in))
	n, ok := Raw(whole).Member("N")
	require.True(t, ok)
	assert.Equal(t, "3", string(n))
	for i := range len(whole) {
		for _, name := range []string{"Input", "List", "N"} {
			got, ok := Raw(whole[:i]).Member(name)
			assert.False(t, ok, "%q of %q: %q", name, whole[:i], got)
		}
	}
	for _, bad := range []string{`{"a"`, `{"a":`, `{"a":1`, `{"a" 1}`, `{"a\`, `{"`, `{"\"`, `{1:2}`, `{"a":1 "b":2}`, `{"a":}`, `{"a":1,}`, `{,}`} {
		got, ok := Raw(bad).Member("a")
		assert.False(t, ok, "%q: %q", bad, got)
	}
}

// nested is a struct that holds itself, to nest a value deep.
type nested struct {
	N *nested
	X int
}

// A value that does not decode deep in a nest is read once, not again at
// every level it fails up through: 2^depth reads, minutes at depth 26. Its
// error is decodeLeaf's, each level's field named. A key given twice is still
// read again, for its last value.
func TestDecodeNestedFailureReadsOnce(t *testing.T) {
	const depth = 26
	data := strings.Repeat(`{"X": 1, "N": `, depth) + `{"X": "one"}` + strings.Repeat("}", depth)
	done := make(chan error, 1)
	go func() {
		var v nested
		done <- Unmarshal([]byte(data), &v)
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.Equal(t, depth, strings.Count(err.Error(), "learnrec.nested.N: "), err.Error())
		assert.True(t, strings.HasSuffix(err.Error(), `learnrec.nested.X: learnrec: want an integer for int: strconv.ParseInt: parsing "\"one\"": invalid syntax`), err.Error())
	case <-time.After(10 * time.Second):
		t.Fatalf("a failure %d deep took over 10 s: read again at every level", depth)
	}

	var v nested
	require.NoError(t, Unmarshal([]byte(`{"N": {"X": "one", "X": 2}, "X": 1}`), &v), "a key given twice decodes its last value")
	require.NotNil(t, v.N)
	assert.Equal(t, 2, v.N.X)
}
