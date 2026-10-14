// Package client is the base client every SDK pandorest generates sends its
// requests through, after go-azure-sdk's sdk/client: one copy in place of the
// one each repository used to keep. It is built on go-kt's chttp, so a read
// is asked again for a dropped connection or a gateway's answer and a write
// is sent once, an answer is read whole or not at all, and what is sent is
// traced to a logger when one is handed in, with the credentials hidden.
//
// What is common to every service is here. What is one service's own about
// being talked to - what it is called, how it takes a credential, how long it
// may take to answer, what its refusals mean - is a Service, a value the
// repository keeps in a file of its own beside the generated package and
// makes its clients from:
//
//	var service = client.Service{
//		Name:      "Radarr",
//		UserAgent: "radarr-mcp/" + version.Version,
//		Example:   "http://nas:7878",
//		Messages:  arrMessages,
//		Note:      note,
//	}
//
//	c, err := service.New(baseURL, client.APIKey(key), client.WithLog(clog.Log))
//
// A generated method builds RequestOptions (method, path, the status codes the
// operation is documented to answer, its options object), makes a Request,
// marshals the body into it, executes it and unmarshals the Response into its
// model:
//
//	opts := client.RequestOptions{
//		ContentType:         "application/json",
//		ExpectedStatusCodes: []int{http.StatusOK},
//		HTTPMethod:          http.MethodGet,
//		OptionsObject:       options,
//		Path:                "/api/v3/movie",
//	}
//	req, err := c.Client.NewRequest(ctx, opts)
//	resp, err := req.Execute(ctx)
//	err = resp.Unmarshal(&model)
//
// A status the operation does not document is an error (*StatusError), even
// another 2xx: that is how a document that has drifted from its server shows
// up, and the fix belongs in the importer's workarounds. The response is
// returned alongside the error, its body still readable.
package client

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/katbyte/go-kt/chttp"
)

const (
	// DefaultPageSize is the page a Complete method asks for when the
	// options leave the page size unset.
	DefaultPageSize = 500
	// DefaultTimeout is how long a request whose answer is read whole has,
	// for a Service that sets none.
	DefaultTimeout = 120 * time.Second
	// DefaultMaxResponse is the most of an answer a client holds, for a
	// Service that sets none: a library of ten thousand films is a few tens
	// of megabytes, and a server sending more than this is not answering the
	// question asked. An answer that is a file is not held, and has no
	// limit.
	DefaultMaxResponse = 64 << 20

	jsonMedia = "application/json"

	apiKeyHeader = "X-Api-Key" //nolint:gosec // the name of the header a key travels in, and no key
	// defaultAdvice is what a refused redirect tells its reader to do, for
	// a Service that words none of its own
	defaultAdvice = "set the server url to the address the server itself answers on"
)

// Authorizer adds credentials to a request.
type Authorizer interface {
	Authorize(req *http.Request)
}

// AuthorizerFunc is a function as an Authorizer, for a service that takes
// its credential in a way of its own.
type AuthorizerFunc func(req *http.Request)

// Authorize calls the function.
func (f AuthorizerFunc) Authorize(req *http.Request) { f(req) }

// APIKey authenticates with a key in the X-Api-Key header, which is where
// Sonarr, Radarr and Prowlarr read theirs. A trace hides it.
type APIKey string

// Authorize sends the key in the X-Api-Key header.
func (k APIKey) Authorize(req *http.Request) {
	req.Header.Set(apiKeyHeader, string(k))
}

// Bearer authenticates with a bearer token in the Authorization header. An
// empty one sends nothing, for a server run with its login off.
type Bearer string

// Authorize sends the token, when there is one.
func (t Bearer) Authorize(req *http.Request) {
	if t != "" {
		req.Header.Set("Authorization", "Bearer "+string(t))
	}
}

// Service is what is one service's own about being talked to. The zero value
// is a server of no particular kind, which is what New makes a client for.
type Service struct {
	// Name is what the server is called in an error and in a trace:
	// "Radarr". Empty is "the server".
	Name string
	// UserAgent is sent with every request; a server that logs it wants the
	// client's name and its running version. Empty sends Go's own.
	UserAgent string
	// Example is an address of the kind the server answers on, for New to
	// show beside a server url it refuses. A host alone, "nas:7878", is
	// shown as http://nas:7878, which is how a server on a home network is
	// reached; a hosted API gives its whole address.
	Example string

	// SendDateAs picks the layout a date goes out in for one query or path parameter, where the server
	// wants something other than the layout its config sends every date in (Dates.Send): "2006-01-02"
	// for a parameter that takes a day, say. Nil, or "", is the usual layout. A generated package asks
	// it with the operation's name and the parameter's name as the document has them.
	SendDateAs func(operation, parameter string) string

	// SecretHeaders are the headers the service takes its credential in,
	// beside Authorization and X-Api-Key: a trace does not show them, and a
	// redirect that leaves the server's host does not carry them.
	SecretHeaders []string
	// SecretNames are names whose values a trace must not show wherever
	// they appear - a query parameter, a form field, a JSON field - beside
	// the ones chttp hides unasked (see chttp.Options).
	SecretNames []string
	// Outcome is where a server that answers 200 to everything says whether
	// the call worked, for a service that does; nil for one whose status
	// says it.
	Outcome *Outcome

	// FollowRedirects follows a redirect no operation documents, without
	// the credentials when it leaves the server's host or goes from https
	// down to http. Without it such a redirect is refused: a server that
	// answers where it is asked redirects only when the server url points
	// at something in front of it, and following one turns a POST into a
	// GET that reports success having done nothing. A redirect an operation
	// does document is not this rule's: it is handed back when it is the
	// operation's whole answer, and followed when the operation documents
	// an answer beyond it.
	FollowRedirects bool
	// RedirectAdvice is what a refused redirect tells its reader to do, in
	// the service's own words: which setting holds the address, that it may
	// lack a URL base.
	RedirectAdvice string
	// WebPageAdvice is what to check when a web page answers where the API
	// was expected, a wrong address or a proxy's login page: empty is
	// "check the server url".
	WebPageAdvice string

	// Timeout is how long a request whose answer is read whole has in all,
	// every try included; 0 is DefaultTimeout. A request whose answer is a
	// file has as long as its context gives it.
	Timeout time.Duration
	// HeaderWait is how long the server has to start answering one attempt;
	// 0 is the whole Timeout, since a server may do all of its work before
	// it writes a byte (a search of every indexer, a lookup it makes of
	// another service).
	HeaderWait time.Duration
	// MaxResponse is the most of an answer the client holds, in bytes; 0 is
	// DefaultMaxResponse.
	MaxResponse int64
	// Retry is when a request is sent again; the zero value is chttp's own
	// rule (a read, three times, for a dropped connection or a gateway's
	// answer).
	Retry chttp.Retry

	// Messages reads what the server said went wrong out of the body of an
	// answer with an undocumented status, each thing it said as one entry,
	// or nothing when the body is not one of its error shapes. They become
	// the StatusError's Messages and, joined, its Body, in place of the
	// start of what was sent.
	Messages func(body []byte) []string
	// Note says what is known on this service of a failure: which key to
	// check for a 401, that a 404 which is not JSON is a path the server
	// does not have rather than a thing that is not there.
	Note func(f Failure) string
}

// Failure is an answer with a status its operation does not document, as a
// Service's Note is asked about it.
type Failure struct {
	StatusCode int
	// Body is what the server sent.
	Body []byte
	// Credentialed says whether the request carried a credential: a 401
	// without one means something else than a 401 with.
	Credentialed bool
}

func (s Service) name() string { return cmp.Or(s.Name, "the server") }

func (s Service) timeout() time.Duration { return cmp.Or(s.Timeout, DefaultTimeout) }

func (s Service) maxResponse() int64 { return cmp.Or(s.MaxResponse, DefaultMaxResponse) }

// example is the service's example address as a clause, or nothing.
func (s Service) example() string {
	switch {
	case s.Example == "":
		return ""
	case strings.Contains(s.Example, "://"):
		return ", e.g. " + s.Example
	default:
		return ", e.g. http://" + s.Example
	}
}

// secretHeaders are the headers a credential travels in beside Authorization.
func (s Service) secretHeaders() []string {
	return append([]string{apiKeyHeader}, s.SecretHeaders...)
}

// Logger is what WithLog takes: chttp's, named here so a generated package
// can offer the option under its own name without importing chttp.
type Logger = chttp.Logger

// Retry is what WithRetry takes: chttp's, named here for the same reason.
type Retry = chttp.Retry

// Option is something a client is made with beside its address, its
// credentials and its Service: what the application using the SDK decides,
// not what the service is.
type Option func(c *settings)

type settings struct {
	http    chttp.Options
	cookies http.CookieJar
	calls   *Calls
	secrets []string
}

// WithLog has the client say what it sends and is sent to a logger, each
// exchange at trace and each request tried again at debug, with what is
// secret in them hidden. A client given none logs nothing.
func WithLog(log chttp.Logger) Option {
	return func(c *settings) { c.http.Log = log }
}

// WithTransport sends the client's requests through a transport of the
// caller's own, underneath the retries and the trace: a test's, or an
// application's with a proxy or certificates of its own.
func WithTransport(base http.RoundTripper) Option {
	return func(c *settings) { c.http.Base = base }
}

// WithRetry sets what is asked again, how often and how long after, in place
// of the Service's rule: a test's, so a request tried again does not wait.
func WithRetry(retry chttp.Retry) Option {
	return func(c *settings) { c.http.Retry = retry }
}

// WithCookies keeps the cookies the server sets and sends them back, for a
// server that also takes a session it gives on login.
func WithCookies(jar http.CookieJar) Option {
	return func(c *settings) { c.cookies = jar }
}

// Client sends requests to one server.
type Client struct {
	// BaseURL is the server address without a trailing slash.
	BaseURL string
	// HTTPClient is what requests are sent through. It can be replaced, as
	// a generated test does with its canned server's own: the Service's
	// redirect rule, its time limit and its size limit still apply to one
	// that sets none of its own, but the retries and the trace are the
	// transport's and go with it. A caller who wants to see or shape the
	// traffic passes WithTransport instead, and keeps both.
	HTTPClient *http.Client
	Authorizer Authorizer

	service Service
	// calls records what is sent, when the application asks; secrets are
	// the values no error may repeat
	calls   *Calls
	secrets []string
	// retries is how HTTPClient was made, kept so an answer read whole is
	// asked again for the same failures a request is
	retries *chttp.Client
	// redirects is the service's redirect rule, for an HTTPClient put in
	// its place that has none, and follow the rule for an operation that
	// documents a redirect on the way to its answer
	redirects func(req *http.Request, via []*http.Request) error
	follow    func(req *http.Request, via []*http.Request) error
}

// New returns a client for a server of no particular kind at baseURL (see
// Service.New).
func New(baseURL string, authorizer Authorizer, opts ...Option) (*Client, error) {
	return Service{}.New(baseURL, authorizer, opts...)
}

// New returns a client for the service at baseURL: scheme and host, and a
// path when the server is set up under one, such as http://nas:7878/radarr.
func (s Service) New(baseURL string, authorizer Authorizer, opts ...Option) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("a server url is required")
	}
	if authorizer == nil {
		return nil, errors.New("an authorizer is required")
	}

	// a url that cannot be read is not repeated: what is in it is unknown, a password included
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("the server url cannot be read as one: it must be the address %s answers on%s", s.name(), s.example())
	}
	if u.User != nil {
		return nil, errors.New("the server url must not contain credentials: they are passed separately, not as a user and password in the address")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("server url %q must be the address %s answers on, with its scheme and host%s", chttp.RedactURL(baseURL), s.name(), s.example())
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("server url %q must not carry a query or a fragment", chttp.RedactURL(baseURL))
	}

	cfg := settings{http: chttp.Options{Name: s.Name, Retry: s.Retry, HeaderWait: cmp.Or(s.HeaderWait, s.timeout()), SecretHeaders: s.secretHeaders(), SecretNames: s.SecretNames}}
	for _, opt := range opts {
		opt(&cfg)
	}

	retries := chttp.New(cfg.http)
	retries.Timeout = s.timeout()
	retries.Jar = cfg.cookies
	follow := chttp.KeepCredentialsOnHost(s.secretHeaders()...)
	retries.CheckRedirect = chttp.RefuseRedirects(cmp.Or(s.RedirectAdvice, defaultAdvice))
	if s.FollowRedirects {
		retries.CheckRedirect = follow
	}

	secrets := cfg.secrets
	if holder, ok := authorizer.(secretHolder); ok {
		secrets = append(secrets, holder.Secrets()...)
	}

	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTPClient: retries.Client, Authorizer: authorizer, service: s, calls: cfg.calls, secrets: secrets, retries: retries, redirects: retries.CheckRedirect, follow: follow}, nil
}

// Call is one request a client sent: its method, the path it went to, and
// the Go name of the operation it was for when a generated method sent it,
// empty for one written by hand.
type Call struct {
	Method, Path string
	Operation    string
}

// Calls records every request a client sends, for a live suite to ask at
// its end which operations it never reached (sweep.NeverCalled). Safe to
// share between clients and goroutines.
type Calls struct {
	mu   sync.Mutex
	seen []Call
}

// Seen is every call so far, in order.
func (c *Calls) Seen() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.seen)
}

func (c *Calls) record(method, path, operation string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, Call{Method: method, Path: path, Operation: operation})
}

// WithCalls has the client record every request it sends in calls, whether
// or not the server answers it.
func WithCalls(calls *Calls) Option {
	return func(c *settings) { c.calls = calls }
}

// WithSecrets names values the client must not repeat: a second token, a
// password sent in a body. The credential the authorizer holds is one
// already. Wherever one of them would appear in an error, a status error's
// body or message included, it reads *** instead. A value shorter than six
// characters is left as it is, since hiding it would hide pieces of
// ordinary words.
func WithSecrets(values ...string) Option {
	return func(c *settings) { c.secrets = append(c.secrets, values...) }
}

// secretHolder is an authorizer that can say what it holds, so the client
// keeps it out of errors.
type secretHolder interface{ Secrets() []string }

// Secrets is the key.
func (k APIKey) Secrets() []string { return nonEmpty(string(k)) }

// Secrets is the token.
func (t Bearer) Secrets() []string { return nonEmpty(string(t)) }

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}

	return []string{s}
}

// minSecret is how long a value must be to be hidden: a shorter one would hide pieces of ordinary words.
const minSecret = 6

// redact replaces every secret in s with ***.
func (c *Client) redact(s string) string {
	if c == nil {
		return s
	}
	for _, secret := range c.secrets {
		if len(secret) >= minSecret {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}

	return s
}

// redactErr is err with every secret in its text replaced, still unwrapping
// to err; err itself when its text carries none.
func (c *Client) redactErr(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	if redacted := c.redact(text); redacted != text {
		return &redactedError{text: redacted, err: err}
	}

	return err
}

type redactedError struct {
	text string
	err  error
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.err }

// Outcome is where a service that answers 200 to everything says whether a
// call worked: Field is the JSON field at the top of the answer that holds
// it, Failures the values that mean it did not, each with what a reader
// should know of it, and Message the field that carries the server's own
// words, if any. A call whose answer carries one of the values returns an
// *OutcomeError under its documented status, with the answer still decoded.
type Outcome struct {
	Field    string
	Failures map[string]string
	Message  string
}

// OutcomeError is a call the server refused in the body of a 200: the
// outcome value it answered, what the service says of it, and the server's
// own message when the service names a field for one.
type OutcomeError struct {
	Method, Path string
	Value        string
	Meaning      string
	Message      string
}

func (e *OutcomeError) Error() string {
	msg := fmt.Sprintf("%s %s: the server answered %s %q", e.Method, e.Path, e.Value, e.Meaning)
	if e.Message != "" {
		msg += ": " + e.Message
	}

	return msg
}

// OutcomeValue is the outcome value a call was refused with, or "" for any
// other error, so a caller can branch on the values a service declares.
func OutcomeValue(err error) string {
	if oe, ok := errors.AsType[*OutcomeError](err); ok {
		return oe.Value
	}

	return ""
}

// outcomeError reads the outcome of a JSON answer, when the service declares
// one, and is nil when the call worked or the answer carries no outcome.
func (r *Request) outcomeError(body []byte) error {
	o := r.client.service.Outcome
	if o == nil || o.Field == "" {
		return nil
	}
	top := topLevel(body)
	value, ok := textAt(top, o.Field)
	if !ok {
		return nil
	}
	meaning, failed := o.Failures[value]
	if !failed {
		return nil
	}
	e := &OutcomeError{Method: r.Method, Path: r.URL.Path, Value: value, Meaning: meaning}
	if msg, ok := textAt(top, o.Message); ok {
		e.Message = r.client.redact(msg)
	}

	return e
}

// topLevel is the fields at the top of a JSON object, or none for anything else.
func topLevel(body []byte) map[string]json.RawMessage {
	var top map[string]json.RawMessage
	_ = json.Unmarshal(body, &top)

	return top
}

// textAt is the string a top-level field holds, when it holds one.
func textAt(top map[string]json.RawMessage, field string) (string, bool) {
	raw, ok := top[field]
	if !ok || field == "" {
		return "", false
	}
	var text string
	ok = json.Unmarshal(raw, &text) == nil

	return text, ok
}

// ErrCommandFailed and ErrCommandRunning are what a generated WaitForCommand
// wraps its two refusals in, for errors.Is: the server said the command
// failed, or it was still running when the wait ran out, which a caller may
// treat as an answer rather than a failure.
var (
	ErrCommandFailed  = errors.New("the command failed")
	ErrCommandRunning = errors.New("the command is still running")
)
