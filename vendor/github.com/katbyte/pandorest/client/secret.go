package client

import "encoding/json"

// SecretPlaceholder is what a server writes in place of a value it hides from the caller.
const SecretPlaceholder = "********"

// Secret is a value a server shows only to some callers and hides from the rest by sending a placeholder in its place, whatever
// the value's type, so a number or a record arrives as text. Hidden says the placeholder came; Value is the value, or the zero
// when it was hidden. The NamedTypes workaround puts it on the fields a server hides.
type Secret[T any] struct {
	Value  T
	Hidden bool
}

// UnmarshalJSON reads the value, or the placeholder as hidden.
func (s *Secret[T]) UnmarshalJSON(data []byte) error {
	var text string
	if json.Unmarshal(data, &text) == nil && text == SecretPlaceholder {
		*s = Secret[T]{Hidden: true}

		return nil
	}

	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*s = Secret[T]{Value: v}

	return nil
}

// MarshalJSON writes the value, or the placeholder for one that was hidden, so a record read back is written as it came.
func (s Secret[T]) MarshalJSON() ([]byte, error) {
	if s.Hidden {
		return json.Marshal(SecretPlaceholder)
	}

	return json.Marshal(s.Value)
}

// String prints the secret as ***, however a record holding it is printed, so one logged by mistake shows nothing; Value is
// read on purpose.
func (Secret[T]) String() string { return "***" }

// GoString prints the secret as *** under %#v too.
func (Secret[T]) GoString() string { return "***" }
