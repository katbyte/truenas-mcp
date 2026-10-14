package client

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Caller is a connection to a server that takes JSON-RPC calls, which a generated package for such a server sends every
// method through: the repository writes it over its websocket, logs in, numbers the calls and matches the answers, and the
// generated methods only name a method and hand it the arguments in order. Call decodes the server's answer into what into
// points at, or leaves it alone for nil, and answers a *CallError for a call the server refused, so a caller tells one of those
// from a connection that failed with errors.As.
type Caller interface {
	Call(ctx context.Context, method string, params []any, into any) error
}

// CallerFunc is a Caller made of one function, for a test or a fake.
type CallerFunc func(ctx context.Context, method string, params []any, into any) error

// Call calls the function.
func (f CallerFunc) Call(ctx context.Context, method string, params []any, into any) error {
	return f(ctx, method, params, into)
}

// CallError is a call the server refused, as a JSON-RPC error carries it: Code and Message are the protocol's (-32601 for a
// method that is not there, -32602 for arguments it refuses, -32001 for a method that failed), and what the server said of why
// sits under them: Errno and ErrName its error number and name (2 and ENOENT for a record that is not there, 22 and EINVAL for
// a value it will not take), Reason its words, and Extra whatever more it attached, a list of fields and what is wrong with
// each for arguments it refuses. A connection that answers one should fill what the server sent and leave the rest zero.
type CallError struct {
	Method  string
	Code    int
	Message string
	Errno   int
	ErrName string
	Reason  string
	Extra   any
}

// Error names the method and says why, in the server's words where it gave them.
func (e *CallError) Error() string {
	var b strings.Builder
	if e.Method != "" {
		b.WriteString(e.Method + ": ")
	}
	switch {
	case e.Reason != "":
		b.WriteString(strings.TrimSpace(e.Reason))
	case e.Message != "":
		b.WriteString(e.Message)
	default:
		fmt.Fprintf(&b, "the server refused the call (code %d)", e.Code)
	}
	if e.ErrName != "" {
		b.WriteString(" (" + e.ErrName + ")")
	}

	return b.String()
}

// CallErrorOf is the CallError in err's chain, or nil: what the server said of a call it refused.
func CallErrorOf(err error) *CallError {
	if ce, ok := errors.AsType[*CallError](err); ok {
		return ce
	}

	return nil
}

// IsRefusedCall reports whether err is a call the server refused by name: ENOENT for what is not there, EINVAL or EPERM for
// what it will not do, which are the names a server gives where an HTTP one gives a status. It is false for a connection that
// failed, so a caller that retries can tell the two apart.
func IsRefusedCall(err error, errNames ...string) bool {
	ce := CallErrorOf(err)
	if ce == nil {
		return false
	}
	if len(errNames) == 0 {
		return true
	}
	for _, name := range errNames {
		if strings.EqualFold(ce.ErrName, name) {
			return true
		}
	}

	return false
}

// CallParams is the arguments a method is sent: every one of the first required, and after them only up to the last one that
// is set, since a server reads an argument left off the end as its default where it would read a zero sent as a value. A
// struct with nothing set, an empty list or map, a zero number, "" or false is not set. A list or map that is sent unset goes
// as an empty one, since a server that asks for a list reads null as no list at all.
func CallParams(required int, args ...any) []any {
	n := len(args)
	for n > required && isZero(args[n-1]) {
		n--
	}
	if n == 0 {
		return nil
	}
	sent := make([]any, n)
	for i, a := range args[:n] {
		sent[i] = emptied(a)
	}

	return sent
}

// emptied is a nil list or map as an empty one, and any other value as it is.
func emptied(v any) any {
	rv := reflect.ValueOf(v)
	switch rv.Kind() { //nolint:exhaustive // only a nil list or map changes
	case reflect.Slice:
		if rv.IsNil() {
			return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
		}
	case reflect.Map:
		if rv.IsNil() {
			return reflect.MakeMap(rv.Type()).Interface()
		}
	}

	return v
}

// isZero reports whether a value is its type's zero: nil, or what reflect calls zero, with an empty list or map counted too.
func isZero(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() { //nolint:exhaustive // every other kind is settled by IsZero
	case reflect.Slice, reflect.Map:
		return rv.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	}

	return rv.IsZero()
}
