package httpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// DefaultTraceBody is how much of a body a trace prints: enough to see what came back, not a whole listing.
const DefaultTraceBody = 8 << 10

// redacted stands in a trace for a value that is not shown.
const redacted = "REDACTED"

// secretHeaders are the headers a credential travels in whatever the API.
var secretHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"}

// secretWords make a name a credential's wherever they sit in it: primaryNodePassword, passwordConfirmation, clientSecretValue. The price is a
// harmless name with one of them in it, passwordPolicy, hidden too.
var secretWords = []string{"password", "passphrase", "secret"}

// secretEndings are how a credential's name ends, whatever is in front: refresh_token, TmdbApiKey. Only the ending is matched, since maxTokens and
// cookiePolicy hold no credential; the price is a page token hidden too.
var secretEndings = []string{"passkey", "token", "cookie", "apikey", "api_key", "privatekey", "private_key"}

// SecretName reports whether a name holds a credential, by the rule a trace hides one by: a common credential parameter, one of also, a name with
// password, passphrase or secret anywhere in it, or one ending passkey, token, cookie, apikey, api_key, privatekey or private_key, in any case.
func SecretName(name string, also ...string) bool {
	name = strings.ToLower(name)
	if slices.Contains(credentialParams, name) || slices.ContainsFunc(also, func(a string) bool { return strings.EqualFold(a, name) }) {
		return true
	}

	return reads(name)
}

// reads reports whether a lower-case name reads as a credential's, by a word in it or by how it ends.
func reads(name string) bool {
	return slices.ContainsFunc(secretWords, func(word string) bool { return strings.Contains(name, word) }) || slices.ContainsFunc(secretEndings, func(ending string) bool { return strings.HasSuffix(name, ending) })
}

// defaultSecrets is what is hidden when nothing more is named.
var defaultSecrets = newSecrets(Options{})

// RedactJSON blanks every string field in JSON text, whole or cut short, whose name is a credential's (SecretName, plus also). It is the rule a trace
// hides a body's secrets by, for whatever else writes one down.
func RedactJSON(text string, also ...string) string {
	s := defaultSecrets
	if len(also) > 0 {
		s = newSecrets(Options{SecretNames: also})
	}

	return s.fields.ReplaceAllString(text, `${1}"`+redacted+`"`)
}

// lazy is text built only when formatted, so a trace that is off costs nothing.
type lazy func() string

func (l lazy) String() string { return l() }

// secrets is what a trace must not show.
type secrets struct {
	headers []string
	names   []string
	// fields finds a JSON string field by one of the names, even in a body cut off part way
	fields *regexp.Regexp
}

func newSecrets(o Options) secrets {
	s := secrets{headers: append(slices.Clone(secretHeaders), o.SecretHeaders...)}
	for _, n := range append(slices.Clone(credentialParams), o.SecretNames...) {
		s.names = append(s.names, strings.ToLower(n))
	}

	// a field with one of the names, or one that reads as a credential's by a word in it or by how it ends
	named := make([]string, 0, len(s.names)+2)
	for _, n := range s.names {
		named = append(named, regexp.QuoteMeta(n))
	}
	named = append(named, `[^"\\]*(?:`+strings.Join(secretWords, "|")+`)[^"\\]*`, `[^"\\]*(?:`+strings.Join(secretEndings, "|")+`)`)
	s.fields = regexp.MustCompile(`(?i)("(?:` + strings.Join(named, "|") + `)"\s*:\s*)"(?:[^"\\]|\\.)*"?`)

	return s
}

func (s secrets) header(name string) bool {
	return slices.ContainsFunc(s.headers, func(h string) bool { return strings.EqualFold(h, name) })
}

// name reports whether a parameter or form field of this name is a credential.
func (s secrets) name(name string) bool {
	name = strings.ToLower(name)

	return slices.Contains(s.names, name) || reads(name)
}

// query is a query string or form with its credentials blanked.
func (s secrets) query(raw string) string {
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		key, _, hasValue := strings.Cut(p, "=")
		name, err := url.QueryUnescape(key)
		if err != nil {
			name = key
		}
		if hasValue && s.name(name) {
			parts[i] = key + "=" + redacted
		}
	}

	return strings.Join(parts, "&")
}

// requestText is a request as a trace shows it.
func (t *Transport) requestText(req *http.Request) string {
	var b strings.Builder
	path, query, hasQuery := strings.Cut(req.URL.RequestURI(), "?")
	if hasQuery {
		path += "?" + t.secrets.query(query)
	}
	fmt.Fprintf(&b, "%s %s %s\n", req.Method, path, req.Proto)
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	fmt.Fprintf(&b, "Host: %s\n", host)
	t.writeHeaders(&b, req.Header)

	switch {
	case req.Body == nil || req.Body == http.NoBody:
	case t.o.TraceBody < 0:
		b.WriteString("\n(a body, not shown)")
	case req.GetBody == nil:
		// reading it here would take it from the server it is being sent to
		b.WriteString("\n(a body sent as a stream, not shown)")
	default:
		body, err := req.GetBody()
		if err != nil {
			fmt.Fprintf(&b, "\n(a body that could not be read again to show: %v)", err)

			break
		}
		head, _ := readAhead(body, t.traceBody()+1)
		_ = body.Close()
		b.WriteString(t.bodyText(head, req.Header.Get("Content-Type")))
	}

	return b.String()
}

// responseText is an answer as a trace shows it. The start of the body is read and put back in front of the rest, so the reader gets every byte and a
// download stays a stream.
func (t *Transport) responseText(resp *http.Response) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", resp.Proto, resp.Status)
	t.writeHeaders(&b, resp.Header)

	_, handedOver := resp.Body.(io.Writer)
	switch {
	case resp.Body == nil || resp.Body == http.NoBody:
	case handedOver:
		// a connection switched to another protocol is not an answer
	case t.o.TraceBody < 0:
		b.WriteString("\n(a body, not shown)")
	case !textual(resp.Header.Get("Content-Type")):
		fmt.Fprintf(&b, "\n(a body of %s, not shown)", contentOf(resp))
	default:
		head, rest := readAhead(resp.Body, t.traceBody()+1)
		resp.Body = rest
		b.WriteString(t.bodyText(head, resp.Header.Get("Content-Type")))
	}

	return b.String()
}

func (t *Transport) traceBody() int {
	if t.o.TraceBody == 0 {
		return DefaultTraceBody
	}

	return t.o.TraceBody
}

func (t *Transport) writeHeaders(b *strings.Builder, h http.Header) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, v := range h[name] {
			if t.secrets.header(name) {
				v = redacted
			}
			fmt.Fprintf(b, "%s: %s\n", name, v)
		}
	}
}

// bodyText is the start of a body as a trace shows it: secrets blanked, whole JSON laid out, and a note when there was more. head holds one byte past
// the limit when the body went on.
func (t *Transport) bodyText(head []byte, contentType string) string {
	if len(head) == 0 {
		return ""
	}

	limit, more := t.traceBody(), false
	if len(head) > limit {
		head, more = head[:limit], true
		// not half a character
		last := len(head)
		for last > 0 && !utf8.RuneStart(head[last-1]) {
			last--
		}
		if last > 0 && !utf8.FullRune(head[last-1:]) {
			head = head[:last-1]
		}
	}
	if !utf8.Valid(head) {
		return "\n(a body that is not text, not shown)"
	}

	text := string(head)
	if mediaType(contentType) == "application/x-www-form-urlencoded" {
		text = t.secrets.query(text)
	} else {
		text = t.secrets.fields.ReplaceAllString(text, `${1}"`+redacted+`"`)
	}
	if !more && json.Valid([]byte(text)) {
		var out bytes.Buffer
		//nolint:errcheck,gosec // json.Indent only fails on invalid input, which json.Valid just ruled out
		json.Indent(&out, []byte(text), "", " ")
		text = out.String()
	}
	if more {
		text += fmt.Sprintf("\n... (the first %s; the rest is not shown)", size(int64(len(head))))
	}

	return "\n" + text
}

// textual reports whether a body of this type is worth showing as text; one that does not say is tried.
func textual(contentType string) bool {
	mt := mediaType(contentType)
	switch {
	case mt == "", strings.HasPrefix(mt, "text/"):
		// an event stream never ends, and waiting for it would hold its reader up
		return mt != "text/event-stream"
	case strings.HasSuffix(mt, "json"), strings.HasSuffix(mt, "xml"), mt == "application/x-www-form-urlencoded", mt == "application/javascript", mt == "application/graphql":
		return true
	}

	return false
}

func mediaType(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(contentType))
	}

	return mt
}

// contentOf says what a body not shown is: its type and, when given, its length.
func contentOf(resp *http.Response) string {
	what := mediaType(resp.Header.Get("Content-Type"))
	if resp.ContentLength >= 0 {
		what += fmt.Sprintf(", %d bytes", resp.ContentLength)
	}

	return what
}

// readAhead reads up to n bytes of a body and returns them with a body that still gives every byte, those included, and meets the same error where it
// was, so an answer cut short still reads as one.
func readAhead(body io.ReadCloser, n int) ([]byte, io.ReadCloser) {
	head := make([]byte, 0, n)
	var err error
	for len(head) < n && err == nil {
		var m int
		m, err = body.Read(head[len(head):n])
		head = head[:len(head)+m]
	}

	return head, &readAheadBody{head: head, err: err, body: body}
}

type readAheadBody struct {
	head []byte
	err  error
	body io.ReadCloser
}

func (r *readAheadBody) Read(p []byte) (int, error) {
	if len(r.head) > 0 {
		n := copy(p, r.head)
		r.head = r.head[n:]

		return n, nil
	}
	if r.err != nil {
		return 0, r.err
	}

	return r.body.Read(p)
}

func (r *readAheadBody) Close() error { return r.body.Close() }

const logReqMsg = `%s API Request Details:
---[ REQUEST ]---------------------------------------
%s
-----------------------------------------------------`

const logRespMsg = `%s API Response Details:
---[ RESPONSE ]--------------------------------------
%s
-----------------------------------------------------`
