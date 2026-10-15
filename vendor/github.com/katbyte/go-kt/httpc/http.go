// Package httpc is the HTTP client every katbyte tool talks to APIs with: a stalled server fails fast, a passing failure is tried again without ever
// re-sending a write whose fate is unknown, and every exchange is traced to whatever logger it is handed, with credentials blanked.
//
// It uses the standard library alone and logs nothing unasked, so an SDK can be built on it. What is worth another try, how long to wait, and what is
// secret are each API's own to set (Options); the defaults are cautious.
package httpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultTries is how many times a request is sent before giving up: enough for a blip, not enough to turn an outage into a long hang.
const DefaultTries = 3

// DefaultHeaderWait is how long a server has to start answering.
const DefaultHeaderWait = 30 * time.Second

// MaxRetryAfter caps the wait a Retry-After header can ask for: longer is "come back later", not "sit there".
const MaxRetryAfter = 60 * time.Second

// Logger is where a client says what it does: a retry at debug, each exchange at trace. A logrus logger is one as it stands. What is traced is built
// only when the logger formats it, so a level that is off costs nothing; a logger must therefore format before Tracef returns.
type Logger interface {
	Debugf(format string, args ...any)
	Tracef(format string, args ...any)
}

// Options are what a client is made with. The zero value is a client that logs nothing and retries with the defaults.
type Options struct {
	// Name says whose traffic this is in a log, so a tool talking to two APIs can tell them apart.
	Name string
	// Log is where retries and the trace of each exchange go. Nil logs nothing.
	Log Logger
	// Retry is when a request is sent again.
	Retry Retry
	// HeaderWait is how long the server has to start answering one try; 0 is DefaultHeaderWait. A search that runs before it writes a byte wants
	// longer. Reading the answer is not bounded by it.
	HeaderWait time.Duration
	// SecretHeaders are headers a trace must not show, beside Authorization, Proxy-Authorization, Cookie and Set-Cookie.
	SecretHeaders []string
	// SecretNames are names a trace must not show the value of wherever they appear, compared without case, beside those hidden unasked (SecretName):
	// an API's "pass" or "pin" needs naming, proxyPassword and passwordConfirmation do not.
	SecretNames []string
	// TraceBody is how much of a body a trace prints: 0 is DefaultTraceBody, negative prints none. No more is ever read for it.
	TraceBody int
	// Base is what sends the requests, under the retries and the trace; nil is NewBaseTransport. A test or a proxy hands in its own.
	Base http.RoundTripper
}

// Retry is when a request is sent again and how long after. The zero value is three tries, a second then two apart, for a 502, 503 or 504 and for a
// dropped connection. A request is sent again only when that cannot do the work twice: a 429 was refused before it was acted on, so anything may
// retry it; otherwise only a GET, HEAD, OPTIONS or a request marked MarkRetrySafe is, since a write that got no answer may still have landed.
type Retry struct {
	// Tries is the most a request is ever sent; 0 is DefaultTries, 1 never retries. Fetch's own re-reads count against the same number.
	Tries int
	// Wait is how long to wait after attempt n (from 0) failed; nil is 1s, 2s, 4s. A 429's Retry-After is used instead, up to MaxRetryAfter.
	Wait func(attempt int) time.Duration
	// Status says whether this status is worth another try; nil is 502, 503 and 504. A plain 500 is left out: most servers answer it for what will
	// never succeed. An API that uses 500 for "busy" adds it.
	Status func(code int) bool
	// Error says whether a request that got no answer is worth sending again; nil is Dropped. A server that cannot be reached at all, or a timeout,
	// is not: the same wait again rarely helps.
	Error func(err error) bool
}

func (r Retry) tries() int {
	if r.Tries == 0 {
		return DefaultTries
	}

	return max(1, r.Tries)
}

func (r Retry) wait(attempt int) time.Duration {
	if r.Wait != nil {
		return r.Wait(attempt)
	}

	return backoff(attempt)
}

func (r Retry) status(code int) bool {
	if r.Status != nil {
		return r.Status(code)
	}

	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func (r Retry) failed(err error) bool {
	if r.Error != nil {
		return r.Error(err)
	}

	return Dropped(err)
}

type ctxKey int

const (
	retrySafeKey ctxKey = iota
	triesKey
)

// MarkRetrySafe says a request may be re-sent though its method says otherwise: a GraphQL query is a read that travels as a POST.
func MarkRetrySafe(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), retrySafeKey, true))
}

// retrySafe reports whether a request may be re-sent when its fate is unknown: a GET, HEAD or OPTIONS, or one marked. A write that got no answer may
// still have landed.
func retrySafe(req *http.Request) bool {
	if v, ok := req.Context().Value(retrySafeKey).(bool); ok && v {
		return true
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}

	return false
}

// resent is how many times a request has been sent again; it travels with the request, so everything that retries it counts on the one.
type resent struct{ n int }

// counted gives a request a count of retries unless it has one: Fetch starts it, so its re-reads and the transport's retries share one limit. A
// redirect followed on the way is not a retry and costs nothing.
func counted(req *http.Request) (with *http.Request, again *resent) {
	if r, ok := req.Context().Value(triesKey).(*resent); ok && r != nil {
		return req, r
	}
	r := new(resent)

	return req.WithContext(context.WithValue(req.Context(), triesKey, r)), r
}

// Tries is how many times the request behind an answer was sent: 1 for first time, 0 for a request no client of this package sent.
func Tries(resp *http.Response) int {
	if resp == nil || resp.Request == nil {
		return 0
	}
	if r, ok := resp.Request.Context().Value(triesKey).(*resent); ok && r != nil {
		return r.n + 1
	}

	return 0
}

// Client is an http.Client whose requests are retried and traced as its Options say, with Fetch added for an answer read whole.
type Client struct {
	*http.Client

	o Options
}

// New makes a client. It sets no overall timeout and follows redirects as Go does; set Timeout and CheckRedirect for otherwise (RefuseRedirects,
// KeepCredentialsOnHost). A Timeout covers one Do with all its tries and waits; each time Fetch asks again is a Do of its own.
func New(o Options) *Client {
	base := o.Base
	if base == nil {
		base = NewBaseTransport(o)
	}

	return &Client{Client: &http.Client{Transport: NewRetryTransport(o, NewTransport(o, base))}, o: o}
}

// ErrTooLarge is an answer longer than Fetch was told to hold.
var ErrTooLarge = errors.New("the answer is too large")

// Fetch sends a request and reads its whole answer, up to limit bytes; a longer one is ErrTooLarge, not an answer cut to fit, and math.MaxInt64 is no
// limit. The response comes back read and closed whatever its status. An answer that stops part way is asked for again like any dropped connection,
// which a transport cannot do since the body is read after it returns; the usual rules on what may be re-sent, and how often, hold.
func (c *Client) Fetch(req *http.Request, limit int64) (*http.Response, []byte, error) {
	req, again := counted(req)
	tries := c.o.Retry.tries()
	for {
		resp, err := c.Do(req)
		if err != nil {
			return nil, nil, err
		}

		// one byte past the limit is read to know the answer went on; the largest limit there is has no byte past it, and is no limit
		var answer io.Reader = resp.Body
		if limit < math.MaxInt64 {
			answer = io.LimitReader(resp.Body, limit+1)
		}
		body, err := io.ReadAll(answer)
		_ = resp.Body.Close()
		if err == nil {
			if int64(len(body)) > limit {
				return resp, nil, fmt.Errorf("%w: over %s, more than this client reads", ErrTooLarge, size(limit))
			}

			return resp, body, nil
		}

		if again.n >= tries-1 || !retrySafe(req) || !c.o.Retry.failed(err) || req.Context().Err() != nil || !rewind(req) {
			return resp, nil, tried(fmt.Errorf("reading the answer: %w", err), again.n+1)
		}

		wait := c.o.Retry.wait(again.n)
		c.debugf("%s answer stopped part way (try %d of %d), asking again in %s: %v", c.o.Name, again.n+1, tries, wait, err)
		again.n++
		if !sleep(req.Context(), wait) {
			return resp, nil, errors.Join(req.Context().Err(), err)
		}
	}
}

func (c *Client) debugf(format string, args ...any) {
	if c.o.Log != nil {
		c.o.Log.Debugf(format, args...)
	}
}

// size writes a number of bytes the way a person says it.
func size(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + " MiB"
	case n >= 1<<10 && n%(1<<10) == 0:
		return strconv.FormatInt(n>>10, 10) + " KiB"
	}

	return strconv.FormatInt(n, 10) + " bytes"
}

// NewBaseTransport is the default transport with timeouts so a stalled server fails fast: ten seconds to connect, ten for TLS, HeaderWait to start
// answering. Exported for a caller that must build its own client.
func NewBaseTransport(o Options) http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport // unreachable, but degrade gracefully
	}

	c := t.Clone()
	c.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	c.TLSHandshakeTimeout = 10 * time.Second
	c.ResponseHeaderTimeout = o.HeaderWait
	if o.HeaderWait == 0 {
		c.ResponseHeaderTimeout = DefaultHeaderWait
	}

	return c
}

// Transport traces each request and answer to the Log with secrets blanked and JSON laid out, and explains a request that got no answer where the
// operating system is the cause (Explain). With no Log it only explains. While tracing, the start of a text answer is read before it is handed on.
type Transport struct {
	o         Options
	transport http.RoundTripper
	secrets   secrets
}

// NewTransport wraps next with the trace the options ask for.
func NewTransport(o Options, next http.RoundTripper) *Transport {
	return &Transport{o: o, transport: next, secrets: newSecrets(o)}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.o.Log != nil {
		t.o.Log.Tracef(logReqMsg, t.o.Name, lazy(func() string { return t.requestText(req) }))
	}

	resp, err := t.transport.RoundTrip(req)
	if err != nil {
		return resp, Explain(err)
	}

	if t.o.Log != nil {
		t.o.Log.Tracef(logRespMsg, t.o.Name, lazy(func() string { return t.responseText(resp) }))
	}

	return resp, nil
}

// RetryTransport retries as the Options say (Retry): a 429 for any request, a dropped connection or a listed status for one safe to repeat. A
// cancelled context ends the wait.
type RetryTransport struct {
	o         Options
	transport http.RoundTripper
}

// NewRetryTransport wraps next with the retries the options ask for.
func NewRetryTransport(o Options, next http.RoundTripper) *RetryTransport {
	return &RetryTransport{o: o, transport: next}
}

// RoundTrip implements http.RoundTripper.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// the count of times sent again is the request's, not this call's: a request Fetch is asking for again arrives with some of its tries used
	req, again := counted(req)
	safe := retrySafe(req)
	tries := t.o.Retry.tries()
	for {
		resp, err := t.transport.RoundTrip(req)
		left := again.n < tries-1
		if err != nil {
			// the write may have landed and only the answer been lost, so only a request safe to repeat goes again
			if left && safe && t.o.Retry.failed(err) && req.Context().Err() == nil && rewind(req) {
				wait := t.o.Retry.wait(again.n)
				t.debugf("%s request failed (try %d of %d), retrying in %s: %v", t.o.Name, again.n+1, tries, wait, err)
				again.n++
				if !sleep(req.Context(), wait) {
					return nil, errors.Join(req.Context().Err(), err)
				}

				continue
			}

			return nil, tried(err, again.n+1)
		}

		// a 429 was refused before it was acted on, so anything may retry it; a server error leaves a write's fate unknown
		if resp.StatusCode == http.StatusTooManyRequests || (safe && t.o.Retry.status(resp.StatusCode)) {
			if left && rewind(req) {
				wait := t.o.Retry.wait(again.n)
				if resp.StatusCode == http.StatusTooManyRequests {
					if ra, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
						wait = ra
					}
				}
				t.debugf("%s got status %d (try %d of %d), retrying in %s", t.o.Name, resp.StatusCode, again.n+1, tries, wait)
				again.n++
				_ = resp.Body.Close()
				if !sleep(req.Context(), wait) {
					return nil, req.Context().Err()
				}

				continue
			}
		}

		return resp, nil
	}
}

func (t *RetryTransport) debugf(format string, args ...any) {
	if t.o.Log != nil {
		t.o.Log.Debugf(format, args...)
	}
}

// rewind puts a request's body back to its start for another try, and says whether it could: a body with no GetBody cannot be sent twice.
func rewind(req *http.Request) bool {
	if req.Body == nil || req.Body == http.NoBody {
		return true
	}
	if req.GetBody == nil {
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	req.Body = body

	return true
}

// triedError is a request that failed however often it was sent, saying how often.
type triedError struct {
	err   error
	tries int
}

func (e *triedError) Error() string { return fmt.Sprintf("%v (tried %d times)", e.err, e.tries) }
func (e *triedError) Unwrap() error { return e.err }

// tried adds to a failure how many times it was tried, when more than once.
func tried(err error, tries int) error {
	if tries < 2 {
		return err
	}
	if _, done := errors.AsType[*triedError](err); done {
		return err
	}

	return &triedError{err: err, tries: tries}
}

// backoff is the wait after attempt n failed: 1s, 2s, 4s.
func backoff(attempt int) time.Duration {
	return time.Duration(1<<attempt) * time.Second
}

// sleep waits for d unless ctx ends first, and says whether it waited it out.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// retryAfter reads a Retry-After header, seconds or a date, as a wait from now, capped at MaxRetryAfter; false for none or one that does not read.
func retryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	var wait time.Duration
	if secs, err := strconv.Atoi(value); err == nil {
		wait = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		wait = at.Sub(now)
	} else {
		return 0, false
	}

	return max(0, min(wait, MaxRetryAfter)), true
}
