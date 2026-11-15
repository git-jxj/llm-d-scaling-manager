package capacity

import (
	"slices"
	"testing"
)

// --max-model-len is read as vLLM reads it (human_readable_int_or_auto in
// vllm/utils/argparse_utils.py): a lower-case suffix is decimal, an
// upper-case one binary, "auto" and "-1" the model's own length.
func TestParseMaxModelLen(t *testing.T) {
	for in, want := range map[string]int64{
		"16k": 16000, "16K": 16384, "1m": 1000000, "1M": 1 << 20, "2g": 2e9, "1G": 1 << 30,
		"25.6k": 25600, "0.5m": 500000, "8192": 8192,
		"auto": maxModelLenAuto, "AUTO": maxModelLenAuto, "-1": maxModelLenAuto,
	} {
		got, err := parseMaxModelLen(in)
		if err != nil || got != want {
			t.Errorf("parseMaxModelLen(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	// vLLM strips the value first.
	if got, err := parseMaxModelLen(" 4096 "); err != nil || got != 4096 {
		t.Errorf("parseMaxModelLen(%q) = %d, %v; want 4096", " 4096 ", got, err)
	}
	for _, in := range []string{"1.5K", "16x", "k", "", "1e3", "16kk", "16 k", "-16k", "$VLLM_MAX_MODEL_LEN", "9999999999T"} {
		if got, err := parseMaxModelLen(in); err == nil {
			t.Errorf("parseMaxModelLen(%q) = %d, want an error", in, got)
		}
	}
}

// The parser takes the suffixed forms, and one vLLM refuses stays
// Unresolved, never a silent 0.
func TestMaxModelLenSuffixesInArgs(t *testing.T) {
	for _, c := range []struct {
		args       []string
		want       int64
		unresolved bool
	}{
		{[]string{"--max-model-len", "16k"}, 16000, false},
		{[]string{"--max-model-len=16K"}, 16384, false},
		{[]string{"--max-model-len", "1m"}, 1000000, false},
		{[]string{"--max_model_len", "32768"}, 32768, false},
		{[]string{"--max-model-len", "1.5K"}, 0, true},
		{[]string{"--max-model-len", "lots"}, 0, true},
	} {
		var p EngineParams
		parseArgs(c.args, &p, nil)
		if p.MaxModelLen != c.want || slices.Contains(p.Unresolved, "max_model_len") != c.unresolved {
			t.Errorf("%v: MaxModelLen %d, Unresolved %v; want %d, unresolved %v", c.args, p.MaxModelLen, p.Unresolved, c.want, c.unresolved)
		}
	}
}
