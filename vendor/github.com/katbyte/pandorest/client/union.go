package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Union is a value that is one of several records, held as it came. A generated union type embeds it and says which records
// those are, with a method per member that hands the record over as that member, and Kind where the records name themselves
// by a field. A caller builds one from a record with UnionOf.
type Union struct {
	Raw json.RawMessage
}

// UnionOf is the union holding a record; the record is written as it is, so a member record of the wrong kind is sent as it is.
func UnionOf(record any) (Union, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return Union{}, err
	}

	return Union{Raw: raw}, nil
}

// MarshalJSON writes the record as it came, or null for a union holding nothing.
func (u Union) MarshalJSON() ([]byte, error) {
	if len(u.Raw) == 0 {
		return []byte("null"), nil
	}

	return u.Raw, nil
}

// UnmarshalJSON keeps the record as it came.
func (u *Union) UnmarshalJSON(data []byte) error {
	u.Raw = append(json.RawMessage(nil), data...)

	return nil
}

// Kind is the value of the field the records name themselves by, "" for a union holding nothing or a record without it.
func (u Union) Kind(field string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(u.Raw, &fields); err != nil {
		return ""
	}
	var kind string
	_ = json.Unmarshal(fields[field], &kind)

	return kind
}

// UnionAs hands the record over as the member T. Where the records name themselves by a field, the record is that member only
// when the field holds one of kinds, and another kind is an error naming both; without such a field (field "") the record is
// decoded strictly, so one of another member's shape fails on the key T has no field for.
func UnionAs[T any](u Union, field string, kinds ...string) (T, error) {
	var zero T
	if len(u.Raw) == 0 {
		return zero, errors.New("the union holds nothing")
	}
	if field != "" {
		if got := u.Kind(field); !slices.Contains(kinds, got) {
			return zero, fmt.Errorf("the record is a %q, not a %s", got, strings.Join(quoted(kinds), " or "))
		}

		return Decode[T](u.Raw)
	}

	return DecodeStrict[T](u.Raw)
}

// quoted is each word in quotes.
func quoted(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = fmt.Sprintf("%q", w)
	}

	return out
}
