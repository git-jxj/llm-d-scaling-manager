package main

import (
	"reflect"
)

// mapEntryOverhead is what a map holds per entry beyond its key and value:
// the bucket's share of hashes and pointers, about.
const mapEntryOverhead = 16

// heapEstimate is the heap n bytes of sizeOf keep alive, about: sizeOf counts
// what is reached, not the allocator's rounding to its size classes nor a
// map's spare slots. Measured in a warm process on the three committed
// recordings (ols-sweep, ttft-three-pods, ttft-saturated), the heap a
// resolved entry kept was 1.03 to 1.19 times sizeOf; a quarter more keeps
// the estimate above each. It is an estimate, not an account.
func heapEstimate(n int) int { return n + n/4 }

// sizeOf is about how many bytes v reaches: its own size and what its
// pointers, slices, maps, strings and interfaces hold, each pointer followed
// once. It weighs a resolved source in the inputs cache (heapEstimate): an
// estimate of the memory an entry keeps alive, not an account.
func sizeOf(v any) int {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return 0
	}
	return int(rv.Type().Size()) + held(rv, map[uintptr]bool{})
}

// held is what v reaches beyond its own size.
func held(v reflect.Value, seen map[uintptr]bool) int {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return 0
		}
		seen[v.Pointer()] = true
		return int(v.Type().Elem().Size()) + held(v.Elem(), seen)
	case reflect.Interface:
		if v.IsNil() {
			return 0
		}
		e := v.Elem()
		return int(e.Type().Size()) + held(e, seen)
	case reflect.String:
		return v.Len()
	case reflect.Slice:
		if v.IsNil() || v.Cap() == 0 || seen[v.Pointer()] {
			return 0
		}
		seen[v.Pointer()] = true
		n := v.Cap() * int(v.Type().Elem().Size())
		if hasPointers(v.Type().Elem()) {
			for i := range v.Len() {
				n += held(v.Index(i), seen)
			}
		}
		return n
	case reflect.Array:
		n := 0
		if hasPointers(v.Type().Elem()) {
			for i := range v.Len() {
				n += held(v.Index(i), seen)
			}
		}
		return n
	case reflect.Map:
		if v.IsNil() || seen[v.Pointer()] {
			return 0
		}
		seen[v.Pointer()] = true
		entry := int(v.Type().Key().Size()+v.Type().Elem().Size()) + mapEntryOverhead
		n := v.Len() * entry
		for it := v.MapRange(); it.Next(); {
			n += held(it.Key(), seen) + held(it.Value(), seen)
		}
		return n
	case reflect.Struct:
		n := 0
		for i := range v.NumField() {
			n += held(v.Field(i), seen)
		}
		return n
	}
	return 0
}

// hasPointers reports whether a value of t may reach memory beyond its own
// size.
func hasPointers(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.String, reflect.Slice, reflect.Map:
		return true
	case reflect.Array:
		return hasPointers(t.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			if hasPointers(t.Field(i).Type) {
				return true
			}
		}
	}
	return false
}
