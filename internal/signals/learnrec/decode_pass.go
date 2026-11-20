package learnrec

import (
	"fmt"
	"reflect"
	"sync"
)

// decodeAt reads the value that starts at b[i] into v, and returns the
// index just past it. b is valid JSON (Unmarshal checked it). A list or an
// object decoded into a slice, an array, a map or a struct is read member by
// member as it is passed, so a value is read once however deep it lies;
// every other value, and a list or an object of another type, is the token's
// own (decodeLeaf). It reads what decodeLeaf would: an object with a key
// given twice, which Marshal never writes, or with a member that does not
// decode is read again by decodeLeaf, which reads each key's last value
// alone.
func decodeAt(b []byte, i int, v reflect.Value) (int, error) {
	if i < len(b) && (b[i] == '{' || b[i] == '[') && v.Type() != rawType && v.Type() != timeType && !isRecorded(v) {
		switch k := v.Kind(); {
		case k == reflect.Pointer:
			p := reflect.New(v.Type().Elem())
			end, err := decodeAt(b, i, p.Elem())
			if err != nil {
				return end, err
			}
			v.Set(p)
			return end, nil
		case k == reflect.Slice && b[i] == '[':
			return decodeSlice(b, i, v)
		case k == reflect.Map && b[i] == '{':
			return decodeMap(b, i, v)
		case k == reflect.Struct && b[i] == '{':
			return decodeStruct(b, i, v)
		}
	}
	end := skipValue(b, i)
	return end, decodeLeaf(b[i:end], v)
}

// decodeSlice reads the list at b[i] into the slice v, its length and
// capacity the list's, as decodeLeaf makes it.
func decodeSlice(b []byte, i int, v reflect.Value) (int, error) {
	t := v.Type()
	s := reflect.MakeSlice(t, 0, 4)
	n := 0
	i = skipSpace(b, i+1)
	if i < len(b) && b[i] == ']' {
		v.Set(reflect.MakeSlice(t, 0, 0))
		return i + 1, nil
	}
	for i < len(b) {
		if n == s.Cap() {
			grown := reflect.MakeSlice(t, n, 2*n)
			reflect.Copy(grown, s)
			s = grown
		}
		s = s.Slice(0, n+1)
		end, err := decodeAt(b, i, s.Index(n))
		if err != nil {
			return end, err
		}
		n++
		i = skipSpace(b, end)
		if i >= len(b) || b[i] != ',' {
			break
		}
		i = skipSpace(b, i+1)
	}
	if s.Cap() != n {
		exact := reflect.MakeSlice(t, n, n)
		reflect.Copy(exact, s)
		s = exact
	}
	v.Set(s)
	return i + 1, nil
}

// decodeMap reads the object at b[i] into the map v.
func decodeMap(b []byte, i int, v reflect.Value) (int, error) {
	start := i
	t := v.Type()
	m := reflect.MakeMap(t)
	keys := map[string]bool{}
	i = skipSpace(b, i+1)
	if i < len(b) && b[i] == '}' {
		v.Set(m)
		return i + 1, nil
	}
	for i < len(b) {
		kend := skipString(b, i)
		k := keyOf(b[i:kend])
		if keys[k] {
			end := skipValue(b, start)
			return end, decodeLeaf(b[start:end], v)
		}
		keys[k] = true
		i = skipSpace(b, skipSpace(b, kend)+1)
		key := reflect.New(t.Key()).Elem()
		if err := decodeKey(k, key); err != nil {
			end := skipValue(b, start)
			return end, decodeLeaf(b[start:end], v)
		}
		val := reflect.New(t.Elem()).Elem()
		end, err := decodeAt(b, i, val)
		if err != nil {
			// decodeLeaf's error, as it reads the members in order, unless
			// a later value of a key replaces the one that failed.
			if !repeatsAKey(b, start) {
				return end, err
			}
			end := skipValue(b, start)
			return end, decodeLeaf(b[start:end], v)
		}
		m.SetMapIndex(key, val)
		i = skipSpace(b, end)
		if i >= len(b) || b[i] != ',' {
			break
		}
		i = skipSpace(b, i+1)
	}
	v.Set(m)
	return i + 1, nil
}

// decodeStruct reads the object at b[i] into the struct v. A field the
// object names twice, or one that does not decode, sets every field it set
// back to zero, and the object is read again by decodeLeaf, which reads each
// key's last value alone.
func decodeStruct(b []byte, i int, v reflect.Value) (int, error) {
	start := i
	t := v.Type()
	var buf [32]int
	set := buf[:0]
	i = skipSpace(b, i+1)
	if i < len(b) && b[i] == '}' {
		return i + 1, nil
	}
	for i < len(b) {
		kend := skipString(b, i)
		name := keyOf(b[i:kend])
		f := fieldIndex(t, name)
		if f < 0 {
			return i, fieldError(t, name)
		}
		again := func() (int, error) {
			for _, s := range set {
				v.Field(s).SetZero()
			}
			end := skipValue(b, start)
			return end, decodeLeaf(b[start:end], v)
		}
		for _, s := range set {
			if s == f {
				return again()
			}
		}
		set = append(set, f)
		i = skipSpace(b, skipSpace(b, kend)+1)
		end, err := decodeAt(b, i, v.Field(f))
		if err != nil {
			// decodeLeaf's error, as it reads the fields in order, unless a
			// later value of a key replaces the one that failed.
			if !repeatsAKey(b, start) {
				return end, wrapField(t, name, err)
			}
			return again()
		}
		i = skipSpace(b, end)
		if i >= len(b) || b[i] != ',' {
			break
		}
		i = skipSpace(b, i+1)
	}
	return i + 1, nil
}

// repeatsAKey reports whether the object at b[start] gives a key twice. A
// member that fails is read again (decodeLeaf) only then: with every key
// once, decodeLeaf meets the same failure, and a read again at every level a
// failure passes up through doubles at each, 2^depth reads of the deepest.
func repeatsAKey(b []byte, start int) bool {
	ms, ok := members(b[start:skipValue(b, start)])
	if !ok {
		return false
	}
	seen := make(map[string]bool, len(ms))
	for _, m := range ms {
		seen[m.key] = true
	}
	return len(seen) != len(ms)
}

// fieldError is a field the data names and t does not have.
func fieldError(t reflect.Type, name string) error {
	return fmt.Errorf("learnrec: %s has no field %s", t, name)
}

// wrapField is err, met in t's field name.
func wrapField(t reflect.Type, name string, err error) error {
	return fmt.Errorf("%s.%s: %w", t, name, err)
}

// fieldKey is a struct type's field by the name a recording gives it.
type fieldKey struct {
	t    reflect.Type
	name string
}

// fields holds, per fieldKey, the index of an exported field of the type's
// own; a name with none is not kept, so a recording cannot grow it.
var fields sync.Map

// fieldIndex is the index of t's own exported field name, -1 for none.
func fieldIndex(t reflect.Type, name string) int {
	k := fieldKey{t, name}
	if i, ok := fields.Load(k); ok {
		return i.(int)
	}
	f, ok := t.FieldByName(name)
	if !ok || len(f.Index) != 1 || !f.IsExported() {
		return -1
	}
	fields.Store(k, f.Index[0])
	return f.Index[0]
}

// recorded holds, per type, whether it is written through RecordedValue.
var recorded sync.Map

// isRecorded reports whether v's type is one written through RecordedValue
// and read back through SetRecordedValue (decodeRecorded): neither an
// interface nor a pointer.
func isRecorded(v reflect.Value) bool {
	if k := v.Kind(); k == reflect.Interface || k == reflect.Pointer {
		return false
	}
	t := v.Type()
	if r, ok := recorded.Load(t); ok {
		return r.(bool)
	}
	r := t.Implements(recordedValuerType)
	recorded.Store(t, r)
	return r
}

// plainString reports whether raw is a JSON string with no escape and no
// byte that is not printable ASCII: its value is its bytes between the
// quotes.
func plainString(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	for _, c := range raw[1 : len(raw)-1] {
		if c == '\\' || c == '"' || c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}
