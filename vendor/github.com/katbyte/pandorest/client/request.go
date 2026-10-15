package client

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/katbyte/go-kt/httpc"
)

// Options is what a generated options struct implements: the query and
// header parameters it holds.
type Options interface {
	ToHeaders() *Headers
	ToQuery() *QueryParams
}

// Headers are request headers from an options object.
type Headers struct {
	values http.Header
}

// Append adds a header.
func (h *Headers) Append(key, value string) {
	if h.values == nil {
		h.values = http.Header{}
	}
	h.values.Add(key, value)
}

// Values returns the headers.
func (h *Headers) Values() http.Header { return h.values }

// QueryParams are query parameters from an options object.
type QueryParams struct {
	values url.Values
}

// Append adds a query parameter.
func (q *QueryParams) Append(key, value string) {
	if q.values == nil {
		q.values = url.Values{}
	}
	q.values.Add(key, value)
}

// Values returns the parameters.
func (q *QueryParams) Values() url.Values { return q.values }

// RequestOptions describes one request.
type RequestOptions struct {
	// Accept is the media type the operation is documented to answer when
	// that is not JSON: a file's, or an event stream's. The request asks for
	// it first, since a server may choose its answer by what it is asked
	// for, and for JSON after it, which is how a refusal reads. Empty asks
	// for JSON alone.
	Accept string
	// ContentType is the media type of the request body, when there is one.
	ContentType string
	// ExpectedStatusCodes are the statuses the operation documents; any
	// other is a *StatusError.
	ExpectedStatusCodes []int
	HTTPMethod          string
	// Operation is the Go name of the operation the request is for, which a
	// generated method sets so that a record of calls (WithCalls) names it
	// without matching paths; empty for a request written by hand.
	Operation string
	// OptionsObject supplies query parameters and headers; nil for none.
	OptionsObject Options
	// Path is the escaped path below BaseURL.
	Path string
	// StreamResponse leaves a successful response's body unread for the
	// caller, for operations that answer a file.
	StreamResponse bool
}

// Request is a request being built.
type Request struct {
	*http.Request

	ExpectedStatusCodes []int
	StreamResponse      bool

	client      *Client
	contentType string
	operation   string
	// credentialed is whether the authorizer put anything on the request
	credentialed bool
}

// NewRequest builds a request with authentication, the fixed client headers
// and the options object applied.
func (c *Client) NewRequest(ctx context.Context, input RequestOptions) (*Request, error) {
	u := c.BaseURL + input.Path
	var headers http.Header
	if input.OptionsObject != nil {
		if q := input.OptionsObject.ToQuery(); q != nil && len(q.Values()) > 0 {
			u += "?" + q.Values().Encode()
		}
		if h := input.OptionsObject.ToHeaders(); h != nil {
			headers = h.Values()
		}
	}

	req, err := http.NewRequestWithContext(ctx, input.HTTPMethod, u, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", input.HTTPMethod, input.Path, err)
	}
	req.Header.Set("Accept", accept(input.Accept))
	if c.service.UserAgent != "" {
		req.Header.Set("User-Agent", c.service.UserAgent)
	}

	// a credential is a header or a query parameter the authorizer adds, or a cookie the server gave a session
	plain, query := len(req.Header), req.URL.RawQuery
	c.Authorizer.Authorize(req)
	credentialed := len(req.Header) != plain || req.URL.RawQuery != query || (c.HTTPClient.Jar != nil && len(c.HTTPClient.Jar.Cookies(req.URL)) > 0)
	maps.Copy(req.Header, headers)

	return &Request{Request: req, ExpectedStatusCodes: input.ExpectedStatusCodes, StreamResponse: input.StreamResponse, client: c, contentType: input.ContentType, operation: input.Operation, credentialed: credentialed}, nil
}

// accept is the Accept header of an operation documented to answer
// mediaType.
func accept(mediaType string) string {
	if mediaType == "" || mediaType == jsonMedia {
		return jsonMedia
	}

	return mediaType + ", " + jsonMedia + ";q=0.9"
}

// Marshal sets the request body to payload as JSON.
func (r *Request) Marshal(payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s %s: writing the request body: %w", r.Method, r.URL.Path, err)
	}

	// held as bytes, so a request turned away before it was acted on can be sent again with its body
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	r.ContentLength = int64(len(b))
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.Header.Set("Content-Type", cmp.Or(r.contentType, jsonMedia))

	return nil
}

// SetBody sets the request body to raw bytes of the given media type, or the
// operation's own when contentType is empty. A nil body sends none. An
// operation that takes a range of types (image/*) needs the concrete one.
func (r *Request) SetBody(body io.Reader, contentType string) error {
	if body == nil {
		return nil
	}
	contentType = cmp.Or(contentType, r.contentType)
	if strings.Contains(contentType, "*") {
		return fmt.Errorf("%s %s: name the body's content type (the operation takes %s)", r.Method, r.URL.Path, contentType)
	}

	// a reader that knows its length says so, so the upload is not sent chunked
	r.Body = io.NopCloser(body)
	switch b := body.(type) {
	case *bytes.Reader:
		r.ContentLength = int64(b.Len())
	case *bytes.Buffer:
		r.ContentLength = int64(b.Len())
	case *strings.Reader:
		r.ContentLength = int64(b.Len())
	default:
		r.ContentLength = -1
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}

	return nil
}

// Execute sends the request. The response is returned whenever the server
// answered, including with a *StatusError, so the caller can read its status
// and body. Its body is held (and readable again) unless the request streams
// a successful response, which is then the caller's to read and close.
func (r *Request) Execute(ctx context.Context) (*Response, error) {
	req := r.WithContext(ctx)
	if r.StreamResponse {
		return r.stream(req)
	}
	service := r.client.service
	r.client.calls.record(r.Method, r.URL.Path, r.operation)

	// the whole answer, asked for again if it stops part way, through whatever HTTPClient is now: a test puts its own there
	whole := *r.client.retries
	whole.Client = r.sender(cmp.Or(r.client.HTTPClient.Timeout, service.timeout()))
	httpResp, body, err := whole.Fetch(req, service.maxResponse()) //nolint:bodyclose // Fetch hands the response back with its body read and closed
	if httpResp == nil {
		return nil, r.client.redactErr(fmt.Errorf("%s %s: %w", r.Method, r.URL.Path, httpc.RedactError(err)))
	}
	resp := held(httpResp, body, r.client)

	// a status the operation does not document keeps its status whether or not its body could be read
	if !slices.Contains(r.ExpectedStatusCodes, httpResp.StatusCode) {
		return resp, r.statusError(httpResp, body)
	}

	switch {
	case errors.Is(err, httpc.ErrTooLarge):
		return resp, fmt.Errorf("%s %s: the answer is over %d MiB, more than this client holds: ask for less at a time", r.Method, r.URL.Path, service.maxResponse()>>20)
	case err != nil:
		return resp, r.client.redactErr(fmt.Errorf("%s %s: %w", r.Method, r.URL.Path, httpc.RedactError(err)))
	case !redirect(httpResp.StatusCode) && httpc.IsWebPage(httpResp.Header.Get("Content-Type"), body):
		return resp, fmt.Errorf("%s %s: answered with a web page, not %s's API: %s", r.Method, r.URL.Path, service.name(), cmp.Or(service.WebPageAdvice, "check the server url"))
	default:
		// a service that says in the body whether the call worked is read there
		return resp, r.outcomeError(body)
	}
}

// stream sends a request whose answer is a file: a successful one is handed
// back unread, and has as long as its context gives it rather than the time
// one answer read whole gets.
func (r *Request) stream(req *http.Request) (*Response, error) {
	r.client.calls.record(r.Method, r.URL.Path, r.operation)
	httpResp, err := r.sender(0).Do(req)
	if err != nil {
		return nil, r.client.redactErr(fmt.Errorf("%s %s: %w", r.Method, r.URL.Path, httpc.RedactError(err)))
	}
	if slices.Contains(r.ExpectedStatusCodes, httpResp.StatusCode) {
		return &Response{Response: httpResp, client: r.client}, nil
	}

	// a refusal is small whatever the call would have streamed
	body, _ := io.ReadAll(io.LimitReader(httpResp.Body, r.client.service.maxResponse()))
	_ = httpResp.Body.Close()

	return held(httpResp, body, r.client), r.statusError(httpResp, body)
}

func redirect(status int) bool {
	return status >= http.StatusMultipleChoices && status < http.StatusBadRequest
}

// unfollowed is a redirect rule that follows none and hands the redirect back.
func unfollowed(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// sender is what the request is sent through: HTTPClient as it is now, with
// the time the whole request has (none, for a file) and the redirect rule
// its operation's document makes for it.
//
// An operation that documents a redirect and nothing else as an answer (a
// login that sends its caller on) is handed the redirect back as it came, to
// read where it points and what cookie it set. One that documents a redirect
// beside a 2xx has it followed to that answer, the credentials staying on
// the server's host, whatever the service does with the rest. A redirect no
// operation documents is the service's to refuse or follow, which is also
// the rule for a client put in HTTPClient's place that has none of its own.
func (r *Request) sender(timeout time.Duration) *http.Client {
	sender := *r.client.HTTPClient
	sender.Timeout = timeout

	documented := slices.ContainsFunc(r.ExpectedStatusCodes, redirect)
	beyond := slices.ContainsFunc(r.ExpectedStatusCodes, func(status int) bool { return !redirect(status) })
	switch {
	case documented && beyond:
		sender.CheckRedirect = r.client.follow
	case documented:
		sender.CheckRedirect = unfollowed
	case sender.CheckRedirect == nil:
		sender.CheckRedirect = r.client.redirects
	}

	return &sender
}

// statusError is an answer with a status the operation does not document,
// with what the server said went wrong where the service can read its body,
// and what the service knows of that status.
func (r *Request) statusError(resp *http.Response, body []byte) *StatusError {
	service := r.client.service
	e := &StatusError{StatusError: &httpc.StatusError{Method: r.Method, Path: r.client.redact(r.URL.Path), StatusCode: resp.StatusCode, Expected: r.ExpectedStatusCodes, Body: r.client.redact(httpc.Preview(body))}}

	// a request sent once says nothing of how often it was sent
	if tries := httpc.Tries(resp); tries > 1 {
		e.Tries = tries
	}
	if service.Messages != nil {
		e.Messages = service.Messages(body)
		for i, m := range e.Messages {
			e.Messages[i] = r.client.redact(m)
		}
	}
	if len(e.Messages) > 0 {
		e.Body = strings.Join(e.Messages, "; ")
	}
	if service.Note != nil {
		e.Note = service.Note(Failure{StatusCode: resp.StatusCode, Body: body, Credentialed: r.credentialed})
	}

	return e
}

// Response is a server's answer.
type Response struct {
	*http.Response

	// body is the answer when it was read whole; a stream has none
	body []byte
	held bool
	// client is the one that asked, whose secrets an error must not repeat
	client *Client
}

// held is a response whose body has been read, and can be read again.
func held(resp *http.Response, body []byte, c *Client) *Response {
	resp.Body = io.NopCloser(bytes.NewReader(body))

	return &Response{Response: resp, body: body, held: true, client: c}
}

// Unmarshal decodes the JSON body into model, leaving the body readable
// again. An empty body leaves model as it is.
func (r *Response) Unmarshal(model any) error {
	if err := r.hold(); err != nil {
		return err
	}

	if len(bytes.TrimSpace(r.body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.body, model); err != nil {
		return fmt.Errorf("%s %s: the answer is not what this client reads there (%w): %s", r.Request.Method, r.Request.URL.Path, err, r.client.redact(httpc.Preview(r.body)))
	}

	return nil
}

// Text sets into to the body as it was sent, for an operation that answers
// short text and not JSON, leaving the body readable again. What the server
// calls the answer is not looked at: one that sends bare text may label it
// JSON.
func (r *Response) Text(into *string) error {
	if err := r.hold(); err != nil {
		return err
	}
	*into = string(r.body)

	return nil
}

// hold reads the body whole, if it has not been, and leaves it readable
// again.
func (r *Response) hold() error {
	if !r.held {
		body, err := io.ReadAll(io.LimitReader(r.Body, DefaultMaxResponse))
		_ = r.Body.Close()
		if err != nil {
			return fmt.Errorf("%s %s: reading the answer: %w", r.Request.Method, r.Request.URL.Path, err)
		}
		r.body, r.held = body, true
	}
	r.Body = io.NopCloser(bytes.NewReader(r.body))

	return nil
}

// StatusError is an answer with a status the operation does not document. Its
// Body is what the server said went wrong, or the start of what it sent when
// that is not one of the service's error shapes, and its Note what the
// service knows of that status. It is httpc's status error, which is what it
// unwraps to, with the server's words kept apart as well as joined.
type StatusError struct {
	*httpc.StatusError

	// Messages are what the server said went wrong, each thing it said as
	// one entry, when the service could read them out of the body: a failed
	// validation's fields one by one.
	Messages []string
}

// Unwrap is the httpc status error this one is.
func (e *StatusError) Unwrap() error { return e.StatusError }

// StatusCode returns the status of a status error in err's chain, or 0: the
// status of an answer the operation did not document, and 0 for any other
// error, one from an answer that did not decode included.
func StatusCode(err error) int { return httpc.StatusCode(err) }

// Messages returns what the server said went wrong, each thing as one entry,
// for a status error in err's chain whose body the service could read; nil
// for any other error.
func Messages(err error) []string {
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Messages
	}

	return nil
}

// IsNotFound reports whether err is the server answering that what was asked
// for is not there: a 404, or a call refused as ENOENT.
func IsNotFound(err error) bool {
	return StatusCode(err) == http.StatusNotFound || IsRefusedCall(err, "ENOENT")
}

// IsUnauthorized reports whether err is the server refusing the credentials,
// or the lack of them.
func IsUnauthorized(err error) bool { return StatusCode(err) == http.StatusUnauthorized }

// IsForbidden reports whether err is the server refusing what was asked to
// the one who asked.
func IsForbidden(err error) bool { return StatusCode(err) == http.StatusForbidden }

// IsBadRequest reports whether err is the server refusing what was sent as
// malformed or not allowed: a 400 or a 422.
func IsBadRequest(err error) bool {
	code := StatusCode(err)

	return code == http.StatusBadRequest || code == http.StatusUnprocessableEntity
}

// IsConflict reports whether err is the server refusing a change that clashes
// with what it holds: a record that is already there, or one changed since it
// was read.
func IsConflict(err error) bool { return StatusCode(err) == http.StatusConflict }

// IsTooManyRequests reports whether err is the server asking for fewer
// requests, which a retry after a wait may get past.
func IsTooManyRequests(err error) bool { return StatusCode(err) == http.StatusTooManyRequests }

// IsServerError reports whether err is the server failing on its side, any
// 5xx, which what was asked for had no part in.
func IsServerError(err error) bool {
	code := StatusCode(err)

	return code >= http.StatusInternalServerError && code < 600
}

// WasNotFound reports whether a response is a 404.
func WasNotFound(resp *http.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}

// CSV joins a list parameter comma-separated, for the query parameters the
// document says take one value.
func CSV[T ~string | ~int | ~int64](values []T) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprint(v)
	}

	return strings.Join(parts, ",")
}

// DeepObject renders an object query parameter the deepObject way, one
// parameter per key as name[key]=value, in key order so a request is the
// same from one call to the next.
func DeepObject[K ~string](name string, value map[K]string) [][2]string {
	out := make([][2]string, 0, len(value))
	for _, k := range slices.Sorted(maps.Keys(value)) {
		out = append(out, [2]string{fmt.Sprintf("%s[%v]", name, k), value[k]})
	}

	return out
}

// JSONObject renders an object query parameter as the JSON string a server
// binds it from.
func JSONObject[T any](value map[string]T) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}

	return string(b)
}
