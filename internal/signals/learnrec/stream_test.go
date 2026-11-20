package learnrec

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Scanner reads what Read reads, a byte at a time and past its buffer: the
// events, the torn tail, and the errors.
func TestScanner_ReadsAsReadDoes(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, 0)
	r.Record("k", t0, strings.Repeat("x", 200<<10))
	for i := range 3 {
		r.Record("k", t0, i)
	}
	whole := buf.String()
	lines := strings.SplitAfter(whole, "\n")
	for name, data := range map[string]string{
		"whole":     whole,
		"torn":      whole + `{"Seq":5,"Now":"2026-01`,
		"blank":     "\n  \n" + whole + "\n\n",
		"no ending": strings.TrimSuffix(whole, "\n"),
		"gap":       lines[0] + lines[2],
		"bad line":  lines[0] + "{broken\n" + lines[1],
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			want, wantErr := Read([]byte(data))
			s := NewScanner(iotest.OneByteReader(strings.NewReader(data)))
			var got []RawEvent
			for s.Scan() {
				got = append(got, s.Event())
			}
			if wantErr != nil {
				require.EqualError(t, s.Err(), wantErr.Error())
				return
			}
			require.NoError(t, s.Err())
			assert.Equal(t, want.Events, got)
			assert.Equal(t, want.TornTail, s.TornTail())
		})
	}
}

// A reader's error stops the scan, as it is.
func TestScanner_StopsAtTheReadersError(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, 0).Record("k", t0, 1)
	boom := errors.New("boom")
	s := NewScanner(io.MultiReader(&buf, iotest.ErrReader(boom)))
	assert.True(t, s.Scan(), "the whole line before the error")
	assert.False(t, s.Scan())
	assert.ErrorIs(t, s.Err(), boom)
}

// A line Marshal writes is read without encoding/json's decoder, and every
// other line by it: either way the event is encoding/json's.
func TestReadLine_IsEncodingJSONs(t *testing.T) {
	for _, line := range []string{
		`{"Seq":1,"Now":"2026-01-02T03:04:05Z","Kind":"k","Data":{"A":[1,"}",{"B":"\""}]}}`,
		`{"Data":2,"Kind":"k","Now":"2026-01-02T03:04:05Z","Seq":7}`,
		`{"Seq":1,"Now":"2026-01-02T03:04:05Z","Kind":"k","Data":null}`,
		`{"Seq":1,"Now":"2026-01-02T03:04:05Z","Kind":"k\u00e9","Data":1}`,
		`{"Seq":1,"Now":"2026-01-02T03:04:05Z","Kind":"k","Data":1,"Extra":true}`,
		`{"seq":1,"Now":"2026-01-02T03:04:05Z","Kind":"k","Data":1}`,
		`{"Seq":1,"Seq":2,"Now":"2026-01-02T03:04:05Z","Kind":"k","Data":1}`,
		` { "Seq" : 3 , "Now" : "2026-01-02T03:04:05Z" , "Kind" : "k" , "Data" : [ ] } `,
		`{"Seq":-1,"Now":"2026-01-02T03:04:05Z","Kind":"k","Data":1}`,
		`{"Seq":1e3,"Now":"2026-01-02T03:04:05Z","Kind":"k","Data":1}`,
		`{"Seq":1,"Now":"2026-01-02T03:04:05Z","Kind":"k","Data":`,
	} {
		t.Run(line, func(t *testing.T) {
			var want envelope
			wantErr := json.Unmarshal([]byte(line), &want)
			got, err := readLine([]byte(line))
			if wantErr != nil {
				require.EqualError(t, err, wantErr.Error())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, want.Seq, got.Seq)
			assert.Equal(t, want.Kind, got.Kind)
			assert.Equal(t, string(want.Data), string(got.Data))
			assert.Equal(t, line, got.Line)
		})
	}
}

type passInner struct {
	S   string
	N   int
	Raw Raw
}

type passOuter struct {
	List  []passInner
	Ptrs  []*passInner
	Map   map[string]passInner
	Inner passInner
	Ptr   *passInner
	Arr   [2]float64
	Any   any
	At    time.Time
	Empty []int
	Flag  bool
}

// Unmarshal reads a value in one pass as the split into raw members read it:
// escapes and keys past ASCII, white space, lists whose lengths are exact, a
// key given twice read at its last value alone, and errors that name the
// field.
func TestUnmarshal_OnePassReadsAsTheSplit(t *testing.T) {
	in := ` {
	  "List" : [ {"S":"a","N":1,"Raw": { "k" : [1, 2] } }, {"S":"b\"\\c\u00e9","N":-2} ],
	  "Ptrs": [null, {"S":"p"}],
	  "Map": {"x\u00e9": {"N": 3}, "y": {"S": "}]"}},
	  "Inner": {"S": "first", "N": 9},
	  "Inner": {"S": "last"},
	  "Ptr": {"N": 4},
	  "Arr": [0.5, "NaN"],
	  "Any": {"k": [1.5, "s"]},
	  "At": "2026-01-02T03:04:05+01:00",
	  "Empty": [],
	  "Flag": true
	} `
	var got passOuter
	require.NoError(t, Unmarshal([]byte(in), &got))
	assert.Equal(t, []passInner{{S: "a", N: 1, Raw: Raw(`{ "k" : [1, 2] }`)}, {S: "b\"\\c\u00e9", N: -2}}, got.List)
	assert.Equal(t, 2, cap(got.List), "the list's capacity is its length")
	require.Len(t, got.Ptrs, 2)
	assert.Nil(t, got.Ptrs[0])
	assert.Equal(t, "p", got.Ptrs[1].S)
	assert.Equal(t, map[string]passInner{"x\u00e9": {N: 3}, "y": {S: "}]"}}, got.Map)
	assert.Equal(t, passInner{S: "last"}, got.Inner, "a key given twice is read at its last value alone")
	assert.Equal(t, 4, got.Ptr.N)
	assert.Equal(t, 0.5, got.Arr[0])
	assert.Equal(t, map[string]any{"k": []any{1.5, "s"}}, got.Any)
	assert.True(t, got.At.Equal(time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC)))
	assert.NotNil(t, got.Empty)
	assert.Empty(t, got.Empty)
	assert.True(t, got.Flag)

	var m map[string]int
	require.NoError(t, Unmarshal([]byte(`{"a":"x","a":1}`), &m), "a map's key given twice is read at its last value alone")
	assert.Equal(t, map[string]int{"a": 1}, m)

	for data, want := range map[string]string{
		`{"List":[{"Nope":1}]}`:  "learnrec.passOuter.List: learnrec: learnrec.passInner has no field Nope",
		`{"Inner":{"N":"x"}}`:    `learnrec.passOuter.Inner: learnrec.passInner.N: learnrec: want an integer for int: strconv.ParseInt: parsing "\"x\"": invalid syntax`,
		`{"List":{"S":"a"}}`:     "learnrec.passOuter.List: learnrec: want a list for []learnrec.passInner: json: cannot unmarshal object into Go value of type []json.RawMessage",
		`{"Map":[1]}`:            "learnrec.passOuter.Map: learnrec: want an object for map[string]learnrec.passInner: json: cannot unmarshal array into Go value of type map[string]json.RawMessage",
		`{"Arr":[1]}`:            "learnrec.passOuter.Arr: learnrec: want a list of 2 for [2]float64",
		`{"Inner":{"S":"a"},"x"`: "learnrec: not valid JSON",
	} {
		var v passOuter
		err := Unmarshal([]byte(data), &v)
		require.Error(t, err, data)
		assert.Equal(t, want, err.Error(), data)
	}
}

// Member finds one member of an object without decoding the rest: the last
// of a key given twice, and nothing in a value that is no object.
func TestRaw_Member(t *testing.T) {
	r := Raw(` {"A": {"B": "}"}, "C": [1, {"D": 2}], "A": 3} `)
	got, ok := r.Member("A")
	assert.True(t, ok)
	assert.Equal(t, "3", string(got))
	got, ok = r.Member("C")
	assert.True(t, ok)
	assert.Equal(t, `[1, {"D": 2}]`, string(got))
	_, ok = r.Member("D")
	assert.False(t, ok, "a member of a member")
	_, ok = Raw(`[1]`).Member("A")
	assert.False(t, ok)
	_, ok = Raw(`{}`).Member("A")
	assert.False(t, ok)
	assert.Equal(t, `"x"`, string(must(Raw(`{"k\u0041":"x"}`).Member("kA"))), "a key spelled with an escape")
}

func must(r Raw, ok bool) Raw {
	if !ok {
		return nil
	}
	return r
}
