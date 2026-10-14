package acctest

import (
	"slices"
	"testing"
)

// Readers for a decoded answer: each names the field it reads, so a wrong shape fails the test saying which.

// Strs pulls a []string out of a decoded JSON field.
func Strs(t *testing.T, v any, field string) []string {
	t.Helper()

	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T, want a list", field, v)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s contains %T, want strings", field, e)
		}
		out = append(out, s)
	}

	return out
}

// Rows pulls a list of objects out of a decoded JSON field.
func Rows(t *testing.T, v any, field string) []map[string]any {
	t.Helper()

	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T, want a list", field, v)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		row, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("%s contains %T, want objects", field, e)
		}
		out = append(out, row)
	}

	return out
}

// Num pulls a JSON number out of a decoded field.
func Num(t *testing.T, v any, field string) int {
	t.Helper()

	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s is %T, want a number", field, v)
	}

	return int(f)
}

// Decimal reads a number with a fraction, where rounding would pass a check that should fail.
func Decimal(t *testing.T, v any, field string) float64 {
	t.Helper()

	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s is %T (%v), want a number", field, v, v)
	}

	return f
}

// Object reads a nested object.
func Object(t *testing.T, v any, field string) map[string]any {
	t.Helper()

	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T (%v), want an object", field, v, v)
	}

	return m
}

// Str pulls a string out of a decoded field, "" when absent.
func Str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

// Texts is the strings in a decoded list, "" for anything else in it, nothing for a field that is no list.
func Texts(v any) []string {
	raw := RowsOfAny(v)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		out = append(out, Str(e))
	}

	return out
}

// RowsOf is the objects in a decoded list, a missing field or a non-object entry tolerated.
func RowsOf(v any) []map[string]any {
	raw := RowsOfAny(v)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if row, ok := e.(map[string]any); ok {
			out = append(out, row)
		}
	}

	return out
}

// RowsOfAny is a decoded list as it is, nil for a field that is no list.
func RowsOfAny(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}

	return nil
}

// NumOr0 pulls a JSON number out of a decoded field, 0 when it was omitted.
func NumOr0(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}

	return 0
}

// DecimalOr0 reads a number with a fraction, 0 when omitted; NumOr0 would read 1.5 as 1.
func DecimalOr0(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}

	return 0
}

// BoolOf pulls a JSON boolean out of a decoded field, false when it was omitted.
func BoolOf(v any) bool {
	return IsBool(v, true)
}

// IsBool says whether a decoded field is this boolean; an omitted field is neither.
func IsBool(v any, want bool) bool {
	b, ok := v.(bool)

	return ok && b == want
}

// OrEmptyList is a JSON list, or an empty one when the field is absent.
func OrEmptyList(v any) any {
	if v == nil {
		return []any{}
	}

	return v
}

// Sorted is a copy of the strings, sorted.
func Sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)

	return s
}

// Reversed is a copy of the strings, back to front.
func Reversed(s []string) []string {
	out := slices.Clone(s)
	slices.Reverse(out)

	return out
}

// WithoutName is a copy of the names with one taken out, once.
func WithoutName(names []string, name string) []string {
	out := slices.Clone(names)
	if i := slices.Index(out, name); i >= 0 {
		out = slices.Delete(out, i, i+1)
	}

	return out
}

// SortedKeys are a map's keys, sorted.
func SortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)

	return out
}
