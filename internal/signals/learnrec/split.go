package learnrec

import (
	"bytes"
	"encoding/json"
)

// The decoder splits an object or a list into its members' raw bytes. The
// data was checked to be valid JSON once, where it entered Unmarshal: the
// split reads it without checking it again at every level, which
// encoding/json's own split into json.RawMessage members would, and without
// copying the members. It is what encoding/json gives, a member for a
// member, the last of a key given twice.

// member is one member of an object: its key and its value, raw.
type member struct {
	key string
	raw []byte
}

// isSpace reports whether c is JSON whitespace.
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// skipSpace is the index of the first byte at or after i that is not
// whitespace.
func skipSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

// skipString is the index just past the string that opens at b[i].
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// skipValue is the index just past the value that starts at b[i].
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for ; i < len(b); i++ {
			switch b[i] {
			case '"':
				i = skipString(b, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
		return i
	}
	for i < len(b) && !isSpace(b[i]) && b[i] != ',' && b[i] != ']' && b[i] != '}' {
		i++
	}
	return i
}

// splitList is the elements of the valid JSON list raw, and false when raw
// is not a list.
func splitList(raw []byte) ([][]byte, bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	out := [][]byte{}
	i := skipSpace(raw, 1)
	if i < len(raw) && raw[i] == ']' {
		return out, true
	}
	for i < len(raw) {
		end := skipValue(raw, i)
		out = append(out, raw[i:end])
		i = skipSpace(raw, end)
		if i >= len(raw) || raw[i] != ',' {
			break
		}
		i = skipSpace(raw, i+1)
	}
	return out, true
}

// splitObject is the members of the valid JSON object raw, in order, a key
// given twice once, at its last value; false when raw is not an object.
func splitObject(raw []byte) ([]member, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var out []member
	i := skipSpace(raw, 1)
	if i < len(raw) && raw[i] == '}' {
		return out, true
	}
	for i < len(raw) {
		kend := skipString(raw, i)
		key := keyOf(raw[i:kend])
		i = skipSpace(raw, kend)
		i = skipSpace(raw, i+1) // the colon
		end := skipValue(raw, i)
		out = append(out, member{key: key, raw: raw[i:end]})
		i = skipSpace(raw, end)
		if i >= len(raw) || raw[i] != ',' {
			break
		}
		i = skipSpace(raw, i+1)
	}
	return dedupe(out), true
}

// keyOf is the key the JSON string s spells: its bytes, or, where it has an
// escape or a byte past ASCII, what encoding/json reads it as.
func keyOf(s []byte) string {
	in := s[1 : len(s)-1]
	plain := bytes.IndexByte(in, '\\') < 0
	for _, c := range in {
		if c >= 0x80 {
			plain = false
			break
		}
	}
	if plain {
		return string(in)
	}
	var k string
	_ = json.Unmarshal(s, &k)
	return k
}

// dedupe keeps the last member of each key, where a key is given twice, in
// the order of their last place: an object decodes as encoding/json decodes
// it into a map.
func dedupe(ms []member) []member {
	var last map[string]int
	if len(ms) > 16 {
		last = make(map[string]int, len(ms))
		for i, m := range ms {
			last[m.key] = i
		}
		if len(last) == len(ms) {
			return ms
		}
	}
	isLast := func(i int) bool {
		if last != nil {
			return last[ms[i].key] == i
		}
		for j := i + 1; j < len(ms); j++ {
			if ms[j].key == ms[i].key {
				return false
			}
		}
		return true
	}
	dup := false
	for i := range ms {
		if !isLast(i) {
			dup = true
			break
		}
	}
	if !dup {
		return ms
	}
	out := make([]member, 0, len(ms))
	for i := range ms {
		if isLast(i) {
			out = append(out, ms[i])
		}
	}
	return out
}

// Member is the raw value of the member name of the object r, the last of a
// key given twice, and whether r is an object that has it. r must be valid
// JSON (json.Valid): a reader that needs one field of a large value finds it
// without decoding the rest.
func (r Raw) Member(name string) (Raw, bool) {
	ms, ok := splitObject(bytes.TrimSpace(r))
	if !ok {
		return nil, false
	}
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].key == name {
			return Raw(ms[i].raw), true
		}
	}
	return nil, false
}
