package chttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// explained is an error with the operating system's part in it said after.
type explained struct {
	err  error
	hint string
}

func (e *explained) Error() string { return e.err.Error() + e.hint }
func (e *explained) Unwrap() error { return e.err }

// Explain adds to a request that got no answer what the operating system may be doing, where known: "no route to host" on a Mac is usually its Local
// Network privacy, which no retry fixes. Transport does this itself; Explain is for a client built on another transport.
func Explain(err error) error {
	if localNetworkHint == "" || err == nil || !errors.Is(err, syscall.EHOSTUNREACH) {
		return err
	}
	if _, done := errors.AsType[*explained](err); done {
		return err
	}

	return &explained{err: err, hint: localNetworkHint}
}

// Dropped reports whether a connection that was working went away: reset, closed by the other end, or cut off mid-answer. That is the one failure
// worth sending the request again for (Retry); a server that could not be reached, a timeout or a cancel would meet the same again.
func Dropped(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return false
	}
	for _, gone := range []error{syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, io.ErrUnexpectedEOF, io.EOF, net.ErrClosed} {
		if errors.Is(err, gone) {
			return true
		}
	}

	// some of these come as words alone
	msg := err.Error()
	if slices.ContainsFunc([]string{"server closed idle connection", "connection reset by peer", "broken pipe", "unexpected EOF", "server sent GOAWAY", "http2: client connection lost"}, func(words string) bool { return strings.Contains(msg, words) }) {
		return true
	}

	// over HTTP/2 an answer cut short arrives as a stream reset; one for a broken protocol would happen again
	return strings.Contains(msg, "stream error: ") && slices.ContainsFunc([]string{"; INTERNAL_ERROR", "; CANCEL", "; REFUSED_STREAM"}, func(code string) bool { return strings.Contains(msg, code) })
}

// Refused reports whether nothing was listening where a request was sent. It is not retried unless a caller whose server restarts under it asks, with
// Dropped(err) || Refused(err) as its Retry's Error.
func Refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// credentialParams are the query parameters a credential commonly travels in.
var credentialParams = []string{"api_key", "apikey", "api_token", "access_token", "token"}

// RedactURL blanks the password and every credential parameter in a URL, the common ones (api_key, apikey, api_token, access_token, token) and those
// in also, and leaves the rest as sent.
func RedactURL(raw string, also ...string) string {
	base, query, found := strings.Cut(raw, "?")
	if u, err := url.Parse(base); err == nil && u.User != nil {
		base = u.Redacted()
	}
	if !found {
		return base
	}
	query, fragment, hasFragment := strings.Cut(query, "#")
	parts := strings.Split(query, "&")
	for i, p := range parts {
		key, _, _ := strings.Cut(p, "=")
		name, err := url.QueryUnescape(key)
		if err != nil {
			name = key
		}
		secret := func(param string) bool { return strings.EqualFold(param, name) }
		if slices.ContainsFunc(credentialParams, secret) || slices.ContainsFunc(also, secret) {
			parts[i] = key + "=REDACTED"
		}
	}
	out := base + "?" + strings.Join(parts, "&")
	if hasFragment {
		out += "#" + fragment
	}

	return out
}

// RedactError blanks the credentials in the URL a failed request prints: a request that got no answer fails with its whole URL, key included. The
// error keeps its type and what it wraps. Use it where the error is first seen; a message already built around it keeps its text.
func RedactError(err error, also ...string) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		urlErr.URL = RedactURL(urlErr.URL, also...)
	}

	return err
}

// KeepCredentialsOnHost is a CheckRedirect that follows a redirect but drops Authorization and the named headers when it leaves the first host or
// steps down from https. Go alone keeps a custom header for any host.
func KeepCredentialsOnHost(headers ...string) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		first := via[0].URL
		if strings.EqualFold(req.URL.Hostname(), first.Hostname()) && (first.Scheme != "https" || req.URL.Scheme == "https") {
			return nil
		}
		req.Header.Del("Authorization")
		for _, h := range headers {
			req.Header.Del(h)
		}

		return nil
	}
}

// RedirectError is a request that was redirected and not followed (RefuseRedirects).
type RedirectError struct {
	Method string
	Path   string
	// To is where it was sent, credentials hidden
	To string
	// Advice is what to do about it on this API: which setting to fix
	Advice string
}

func (e *RedirectError) Error() string {
	msg := fmt.Sprintf("%s %s was redirected to %s", e.Method, e.Path, e.To)
	if e.Advice != "" {
		msg += ": " + e.Advice
	}

	return msg
}

// RefuseRedirects is a CheckRedirect that follows none: the request fails with a RedirectError saying where it was sent, plus the advice. For an API
// that answers where it is asked, a redirect means the address is wrong, and following it is worse than failing: Go turns a POST into a GET on a 302,
// the GET answers 200, and a write reports success having done nothing.
func RefuseRedirects(advice string) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		return &RedirectError{Method: via[0].Method, Path: via[0].URL.Path, To: RedactURL(req.URL.String()), Advice: advice}
	}
}

// IsWebPage reports whether an answer is a web page where an API's answer was expected: a wrong address or a login page answers 200 with one. The
// body decides, since a proxy labels anything anything: a document that starts as HTML is a page, and one called HTML is only if it starts with a
// tag.
func IsWebPage(contentType string, body []byte) bool {
	head := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 512)])))
	if strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html") {
		return true
	}

	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/html") && strings.HasPrefix(head, "<")
}

// StatusError is an answer with a status the request did not ask for.
type StatusError struct {
	Method     string
	Path       string
	StatusCode int
	// Expected are the statuses taken as success, when named
	Expected []int
	// Body is the start of the answer (Preview), printed with its credentials blanked: a refused save echoes the record back, key included
	Body string
	// Tries is how often the request was sent, when more than once
	Tries int
	// Note is what the caller knows of this status on this API: which key to check for a 401, what a 403 wants
	Note string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.StatusCode)
	if len(e.Expected) > 0 {
		want := make([]string, 0, len(e.Expected))
		for _, c := range e.Expected {
			want = append(want, strconv.Itoa(c))
		}
		msg += " (expected " + strings.Join(want, " or ") + ")"
	}
	if e.Body != "" {
		msg += ": " + RedactJSON(e.Body)
	}
	if e.Tries > 1 {
		msg += fmt.Sprintf(" (tried %d times)", e.Tries)
	}
	if e.Note != "" {
		msg += " (" + e.Note + ")"
	}

	return msg
}

// StatusCode is the status of a *StatusError in err's chain, or 0.
func StatusCode(err error) int {
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.StatusCode
	}

	return 0
}

// IsNotFound reports whether err is a 404 from the server.
func IsNotFound(err error) bool { return StatusCode(err) == http.StatusNotFound }

// PreviewLen is how much of a response body a StatusError carries.
const PreviewLen = 300

// Preview is the start of a body for an error to carry: trimmed, cut at PreviewLen on a whole character with "..." for more, and its credentials
// blanked (RedactJSON), a field cut off part way included.
func Preview(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) <= PreviewLen {
		return RedactJSON(s)
	}
	n := PreviewLen
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return RedactJSON(s[:n]) + "..."
}
