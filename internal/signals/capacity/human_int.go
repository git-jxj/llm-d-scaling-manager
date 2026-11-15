package capacity

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// maxModelLenAuto is what vLLM reads "auto" and "-1" as: the model's own
// context length, chosen at start.
const maxModelLenAuto = -1

// humanIntForm is a number and one suffix, as vLLM's human_readable_int
// matches it (vllm/utils/argparse_utils.py): (\d+(?:\.\d+)?)([kKmMgGtT]).
var humanIntForm = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([kKmMgGtT])$`)

// humanIntMultiplier is each suffix's multiplier, as vLLM's: lower case is
// decimal (k is 1000), upper case binary (K is 1024).
var humanIntMultiplier = map[string]float64{
	"k": 1e3, "m": 1e6, "g": 1e9, "t": 1e12,
	"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40,
}

// errBinaryDecimal is a decimal number with a binary suffix ("1.5K"), which
// vLLM refuses.
var errBinaryDecimal = errors.New("a decimal with a binary suffix")

// parseMaxModelLen reads --max-model-len as vLLM does
// (human_readable_int_or_auto): "auto" or "-1" is maxModelLenAuto; a number
// with a suffix is human_readable_int's, so "16k" is 16000, "16K" is 16384
// and "1m" is 1000000, a lower-case suffix taking a decimal ("25.6k" is
// 25600, truncated as Python's int does) and an upper-case one not; anything
// else is a plain integer.
func parseMaxModelLen(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "-1" || strings.EqualFold(value, "auto") {
		return maxModelLenAuto, nil
	}
	m := humanIntForm.FindStringSubmatch(value)
	if m == nil {
		return strconv.ParseInt(value, 10, 64)
	}
	number, suffix := m[1], m[2]
	mult := humanIntMultiplier[suffix]
	if strings.ToUpper(suffix) == suffix {
		n, err := strconv.ParseInt(number, 10, 64)
		if err != nil {
			return 0, errBinaryDecimal
		}
		if float64(n) > math.MaxInt64/mult {
			return 0, strconv.ErrRange
		}
		return n * int64(mult), nil
	}
	f, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, err
	}
	v := math.Trunc(f * mult)
	if v >= math.MaxInt64 {
		return 0, strconv.ErrRange
	}
	return int64(v), nil
}
