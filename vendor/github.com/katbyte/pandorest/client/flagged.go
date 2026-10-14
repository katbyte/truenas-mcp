package client

import (
	"encoding/json"
	"fmt"
)

// Flagged is a record sent with one more flag set in it, for a method whose form is chosen by a flag in its options: a query's
// one-record and count forms are asked for by get or count in the options, which the generated method sets itself rather than
// leaving to the caller, since setting either changes what the method answers. Record is the options as the caller gave them,
// nil or a zero record for none.
type Flagged struct {
	Record any
	Flag   string
}

// MarshalJSON writes the record with the flag set true.
func (f Flagged) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if f.Record != nil {
		raw, err := json.Marshal(f.Record)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("the record is not a JSON object: %w", err)
		}
	}
	fields[f.Flag] = json.RawMessage("true")

	return json.Marshal(fields)
}
