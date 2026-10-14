package truenasrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/katbyte/pandorest/client"
)

// The codes a TrueNAS refuses a call with: JSON-RPC's own, and two of its own for a connection asking too much at once and for a call it could not
// carry out.
const (
	CodeInvalidJSON    = -32700
	CodeInvalidRequest = -32600
	CodeNoSuchMethod   = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeTooManyCalls   = -32000
	CodeCallFailed     = -32001
)

// ErrClosed is a call made on a connection after Close.
var ErrClosed = errors.New("the connection to the TrueNAS is closed")

// Error is a call the NAS refused or could not carry out, in its own words. The Is functions say which kind it is, so a caller need not read codes.
type Error struct {
	// Method is the method that was called.
	Method string
	// Code is the JSON-RPC code, one of the Code constants.
	Code int
	// Message is the NAS's short name for the failure, "Method call error".
	Message string
	// Errno and ErrName are the NAS's number and name for what went wrong, 2 and "ENOENT"; zero and empty when it gave none.
	Errno   int
	ErrName string
	// Reason is the sentence the NAS gave.
	Reason string
	// Extra is whatever more the NAS sent, as it sent it: for refused arguments, one entry per argument (Invalid reads them).
	Extra json.RawMessage
}

// As lets a refusal be read as the generated client's kind of refusal, *client.CallError, which code written against sdk/truenas asks for
// with errors.As: the same fields, Extra decoded from the JSON it came as.
func (e *Error) As(target any) bool {
	into, ok := target.(**client.CallError)
	if !ok {
		return false
	}

	var extra any
	if len(e.Extra) > 0 {
		_ = json.Unmarshal(e.Extra, &extra)
	}
	*into = &client.CallError{Method: e.Method, Code: e.Code, Message: e.Message, Errno: e.Errno, ErrName: e.ErrName, Reason: e.Reason, Extra: extra}

	return true
}

// Invalid is one argument the NAS refused, and why.
type Invalid struct {
	Attribute string
	Message   string
	Errno     int
}

func (e *Error) Error() string {
	switch e.Code {
	case CodeNoSuchMethod:
		return "the TrueNAS has no method " + e.Method
	case CodeTooManyCalls:
		return e.Method + ": the TrueNAS is running too many calls for this connection at once"
	case CodeInvalidParams:
		// a thing that is not there is refused as a bad argument, and is no such thing: the NAS's sentence is the whole of it
		if rows := e.Invalid(); e.missing() {
			return fmt.Sprintf("%s: %s (%s)", e.Method, rows[0].Message, ErrnoName(errnoNotFound))
		}

		said := make([]string, 0, 1)
		for _, i := range e.Invalid() {
			if i.Attribute == "" {
				said = append(said, i.Message)

				continue
			}
			said = append(said, i.Attribute+": "+i.Message)
		}
		if len(said) == 0 {
			said = append(said, e.words())
		}

		return fmt.Sprintf("%s: the TrueNAS refused what it was sent: %s", e.Method, strings.Join(said, "; "))
	default:
		return fmt.Sprintf("%s: %s", e.Method, e.words())
	}
}

// words is what the NAS said went wrong: its sentence with the name of the failure after, or its short name when it gave no sentence.
func (e *Error) words() string {
	switch {
	case e.Reason != "" && e.ErrName != "":
		return fmt.Sprintf("%s (%s)", e.Reason, e.ErrName)
	case e.Reason != "":
		return e.Reason
	case e.Message != "":
		return e.Message
	default:
		return fmt.Sprintf("the TrueNAS answered error %d", e.Code)
	}
}

// errnoNotFound is the number for a thing that is not there, ENOENT.
const errnoNotFound = 2

// missing reports whether the refusal is of something asked for by its id or name that the NAS has not got. A TrueNAS does not say that as a
// failure of the call: it refuses the argument, with no argument named and the number for "not there" beside it.
func (e *Error) missing() bool {
	rows := e.Invalid()

	return len(rows) > 0 && !slices.ContainsFunc(rows, func(i Invalid) bool { return i.Errno != errnoNotFound })
}

// Invalid is each argument the NAS refused, or nothing when the error is not about arguments.
func (e *Error) Invalid() []Invalid {
	var rows [][]json.RawMessage
	if e.Code != CodeInvalidParams || json.Unmarshal(e.Extra, &rows) != nil {
		return nil
	}

	out := make([]Invalid, 0, len(rows))
	for _, row := range rows {
		var i Invalid
		if len(row) > 0 {
			_ = json.Unmarshal(row[0], &i.Attribute)
		}
		if len(row) > 1 {
			_ = json.Unmarshal(row[1], &i.Message)
		}
		if len(row) > 2 {
			_ = json.Unmarshal(row[2], &i.Errno)
		}

		out = append(out, i)
	}

	return out
}

// ConnError is a call that got no answer because the NAS could not be reached, would not let the connection in, or went away part way.
type ConnError struct {
	// Address is the NAS that was dialled.
	Address string
	// Sent says whether the call had left when the connection failed. One that had may still have been carried out, so only a read is safe to make
	// again; one that had not was never seen by the NAS.
	Sent bool
	// Err is what failed.
	Err error
}

func (e *ConnError) Error() string { return fmt.Sprintf("the TrueNAS at %s: %v", e.Address, e.Err) }
func (e *ConnError) Unwrap() error { return e.Err }

// ErrNothingVouches is in a login given up before a key or a password was sent: the NAS wants it as it is, and nothing vouches for the NAS's
// certificate.
var ErrNothingVouches = errors.New("nothing vouches for its certificate")

// LoginError is a NAS that was reached and not logged into: it did not accept the login, or this side would not finish it.
type LoginError struct {
	// Address is the NAS that was dialled.
	Address string
	// Response is the NAS's answer to the login: AUTH_ERR, EXPIRED, DENIED, OTP_REQUIRED or REDIRECT.
	Response string
	// URLs are where a NAS that is one of a pair says to log in instead, when its answer is REDIRECT.
	URLs []string
	// Err is why this side gave the login up, where it was not the NAS that refused: a key that may not be sent to it, or a NAS that could not
	// prove it is the one the key was made on.
	Err error
	// Tied says the login the NAS refused was one tied to the certificate it showed, which is refused for a wrong key and for a certificate that
	// is not the NAS's own alike.
	Tied bool
}

func (e *LoginError) Unwrap() error { return e.Err }

func (e *LoginError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("the TrueNAS at %s was not logged into: %v", e.Address, e.Err)
	}

	switch e.Response {
	case "AUTH_ERR":
		if e.Tied {
			return fmt.Sprintf("the TrueNAS at %s refused the API key or password: check it, and the user name it goes with (a NAS reached through something that shows a certificate of its own, such as a reverse proxy, refuses a right key too: its login cannot be tied to the certificate, and has to be told not to be)", e.Address)
		}

		return fmt.Sprintf("the TrueNAS at %s refused the API key or password: check it, and the user name it goes with", e.Address)
	case "EXPIRED":
		return fmt.Sprintf("the TrueNAS at %s says the API key or password has expired or been revoked (it revokes a key that was ever sent without TLS): make a new one", e.Address)
	case "DENIED":
		return fmt.Sprintf("the TrueNAS at %s says this account may not use the API", e.Address)
	case "OTP_REQUIRED":
		return fmt.Sprintf("the TrueNAS at %s wants a one-time password as well, and was given nowhere to get one", e.Address)
	case "REDIRECT":
		return fmt.Sprintf("the TrueNAS at %s is not the one of its pair that takes logins: use the address of the active one (%s)", e.Address, strings.Join(e.URLs, ", "))
	default:
		return fmt.Sprintf("the TrueNAS at %s answered the login with %s", e.Address, e.Response)
	}
}

// IsNoSuchMethod reports whether the NAS has no method of the name called: a NAS older or newer than the program expects.
func IsNoSuchMethod(err error) bool {
	e, ok := errors.AsType[*Error](err)

	return ok && e.Code == CodeNoSuchMethod
}

// IsNotLoggedIn reports whether the NAS refused a call because nobody is logged in on the connection, or the login it had has run out.
func IsNotLoggedIn(err error) bool {
	if _, ok := errors.AsType[*LoginError](err); ok {
		return true
	}
	e, ok := errors.AsType[*Error](err)

	return ok && (e.ErrName == "ENOTAUTHENTICATED" || e.ErrName == "EACCES" && strings.Contains(strings.ToLower(e.Reason), "session is expired"))
}

// IsNotAllowed reports whether the NAS refused a call because the key's user may not make it.
func IsNotAllowed(err error) bool {
	e, ok := errors.AsType[*Error](err)

	return ok && (e.ErrName == "EACCES" || e.ErrName == "EPERM") && !IsNotLoggedIn(err)
}

// IsInvalid reports whether the NAS refused the arguments it was sent, for what they were. A NAS refuses an id it has nothing for the same way,
// which is IsNotFound and not this.
func IsInvalid(err error) bool {
	e, ok := errors.AsType[*Error](err)

	return ok && e.Code == CodeInvalidParams && !e.missing()
}

// IsNotFound reports whether the NAS has nothing of the id or name a call asked for. That includes a query told to answer the one row it
// matches when no row does, which a NAS refuses with nothing but the name of what went wrong inside it.
func IsNotFound(err error) bool {
	e, ok := errors.AsType[*Error](err)

	return ok && (e.ErrName == "ENOENT" || e.missing() || e.ErrName == "EINVAL" && e.Reason == noMatch)
}

// noMatch is all a NAS says when a query told to answer its one row matches none: seen on a real 25.10.7.
const noMatch = "MatchNotFound()"

// IsTooManyCalls reports whether the NAS refused a call because the connection, or the address it comes from, is asking too much at once.
func IsTooManyCalls(err error) bool {
	e, ok := errors.AsType[*Error](err)

	return ok && (e.Code == CodeTooManyCalls || e.ErrName == "EBUSY" && strings.Contains(strings.ToLower(e.Reason), "rate limit"))
}

// IsDisconnected reports whether a call got no answer because the NAS could not be reached or the connection went away.
func IsDisconnected(err error) bool {
	_, ok := errors.AsType[*ConnError](err)

	return ok
}
