package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Decode reads a raw answer, one whose document declares no shape, into a
// type of the caller's own: a local struct with the generated package's Time,
// enum and id types reads with the layouts and checks the generated models
// have. It fails for no answer, as Record does, and for JSON the type cannot
// hold.
func Decode[T any](raw json.RawMessage) (T, error) {
	return readRaw[T](raw, func(*json.Decoder) {})
}

// DecodeStrict is Decode failing for a key the type has no field for, naming
// it, which keeps a local struct whole against what the server sends.
func DecodeStrict[T any](raw json.RawMessage) (T, error) {
	return readRaw[T](raw, (*json.Decoder).DisallowUnknownFields)
}

// readRaw decodes the raw answer with a decoder set up as the caller asks.
func readRaw[T any](raw json.RawMessage, setUp func(*json.Decoder)) (T, error) {
	var v T
	if len(bytes.TrimSpace(raw)) == 0 {
		return v, errors.New("no answer to decode")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	setUp(d)
	if err := d.Decode(&v); err != nil {
		return v, fmt.Errorf("decoding the answer as %T: %w", v, err)
	}

	return v, nil
}
