package generator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/pandorest/definitions"
)

// The generated tests. Every operation gets a test against a canned server
// that proves the method builds the request its definition describes (method,
// escaped path, every option on the wire, the body and its content type),
// decodes a documented answer, streams a file, and returns an undocumented
// status as an error; a paged list also proves its Complete pager. They test
// the generated code against the definitions, not the server: the
// integration suite's read sweep and bespoke tests do that.

// acceptOption reports whether the operation leaves what it asks for to its
// caller: an Accept option, which a workaround adds and whose value is then
// the whole header.
func acceptOption(o *definitions.Operation) bool {
	return slices.ContainsFunc(o.Options, func(opt definitions.Option) bool {
		return opt.In == definitions.InHeader && strings.EqualFold(opt.Name, "Accept")
	})
}

// sampleText is what the canned server answers an operation that answers
// text: not JSON, and with space a reader must not trim.
const sampleText = "some text, as sent\n"

// undocumentedStatus is a status no operation documents.
const undocumentedStatus = http.StatusTeapot

// testHelpersFile is the generated client_test.go: the canned server the
// operation tests share, the paged one a service with lists has, and New's
// validation.
func (g *gen) testHelpersFile() string {
	if g.calls() {
		return g.callTestHelpersFile()
	}
	helpers := operationServerHelpers
	if g.opts.Grouped {
		// a tag's client is made on a base client, not by New
		helpers = strings.Replace(helpers, "(*Client, *operationServer)", "(Client, *operationServer)", 1)
		helpers = strings.Replace(helpers, "\tc, err := New(s.URL, \"test-token\")\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n", "\tbase, err := client.New(s.URL, client.Bearer(\"test-token\"))\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tc := NewClient(base)\n", 1)
	}
	if g.redirects() {
		// a redirect that is an operation's answer says where it sends its caller, and a client that followed it would ask the canned server twice
		helpers = strings.Replace(helpers, "\t\tw.WriteHeader(status)\n", "\t\tif status >= http.StatusMultipleChoices && status < http.StatusBadRequest {\n\t\t\tw.Header().Set(\"Location\", \"/redirected\")\n\t\t}\n\t\tw.WriteHeader(status)\n", 1)
	}

	if g.opts.Grouped {
		return g.fileHeader(helpers + g.pagedServerHelper())
	}

	return g.fileHeader(helpers + g.pagedServerHelper() + newTest)
}

// clientType is the type the test helpers hand out: a tag's Client by
// value in the grouped layout, *Client in the flat one.
func (g *gen) clientType() string {
	if g.opts.Grouped {
		return "Client"
	}

	return "*Client"
}

// newTestClient makes the client the test helpers hand out, on a canned
// server's URL.
func (g *gen) newTestClient(urlExpr string) string {
	if g.opts.Grouped {
		return "\tbase, err := client.New(" + urlExpr + ", client.Bearer(\"test-token\"))\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tc := NewClient(base)\n"
	}

	return "\tc, err := New(" + urlExpr + ", \"test-token\")\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n"
}

// redirects reports whether any operation of the service answers a redirect:
// one whose first expected status, the one its test serves, is a redirect.
func (g *gen) redirects() bool {
	return slices.ContainsFunc(g.svc.Operations(), func(o *definitions.Operation) bool {
		return len(o.ExpectedStatusCodes) > 0 && o.ExpectedStatusCodes[0] >= http.StatusMultipleChoices && o.ExpectedStatusCodes[0] < http.StatusBadRequest
	})
}

const operationServerHelpers = `// operationServer is a canned server that answers every request with one
// status, content type and body, and records what it was sent. queued, when
// set, is the status a POST gets instead, for a command queued and then polled.
type operationServer struct {
	queued int
	*httptest.Server
	requests []*http.Request
	bodies   []string
}

// newOperationServer starts a canned server for the test and a client for it.
func newOperationServer(t *testing.T, status int, contentType, body string) (*Client, *operationServer) {
	t.Helper()

	s := &operationServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.requests = append(s.requests, r.Clone(r.Context()))
		s.bodies = append(s.bodies, string(b))
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if body == "file bytes" {
			// a file is named, as a server names one it serves
			w.Header().Set("Content-Disposition", "attachment; filename=\"answer.bin\"")
		}
		if s.queued != 0 && r.Method == http.MethodPost {
			// a command queued, answered as the queueing operation does; the polls after it get the status
			w.WriteHeader(s.queued)
			_, _ = io.WriteString(w, body)

			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	c, err := New(s.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	// the server's own client: closing a test server closes the idle
	// connections of http.DefaultTransport, which would break the
	// parallel tests sharing it
	c.Client.HTTPClient = s.Client()

	return c, s
}

// only returns the one request the server received, and its body.
func (s *operationServer) only(t *testing.T) (r *http.Request, body string) {
	t.Helper()

	if len(s.requests) != 1 {
		t.Fatalf("the server received %d requests, want 1", len(s.requests))
	}

	return s.requests[0], s.bodies[0]
}

// expectRequest checks the method and escaped path of a request.
func expectRequest(t *testing.T, r *http.Request, method, path string) {
	t.Helper()

	if r.Method != method || r.URL.EscapedPath() != path {
		t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.EscapedPath(), method, path)
	}
}

// expectQuery checks the values sent for one query parameter.
func expectQuery(t *testing.T, r *http.Request, name string, want ...string) {
	t.Helper()

	if got := r.URL.Query()[name]; !slices.Equal(got, want) {
		t.Errorf("query %s = %q, want %q", name, got, want)
	}
}

`

// pagedServerHelper is the canned server the Complete tests share, which answers pages in the names each list's answer
// carries, since a service's lists need not all answer alike. A service with no lists has none.
func (g *gen) pagedServerHelper() string {
	if g.paged == nil {
		return ""
	}
	records := "start index"
	if g.paged.Pageable.ByPage {
		records = "page"
	}

	return `// pagedServer answers the first pages requests with one item each and an
// empty list after, reporting total items under the names the list answers
// with, and records the ` + records + ` of each request.
func pagedServer(t *testing.T, startIndex, items, total, item string, count, pages int) (paged ` + g.clientType() + `, asked *[]string) {
	t.Helper()

	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		starts = append(starts, r.URL.Query().Get(startIndex))
		w.Header().Set("Content-Type", "application/json")
		if len(starts) > pages {
			item = ""
		}
		_, _ = fmt.Fprintf(w, "{%q:[%s],%q:%d}", items, item, total, count)
	}))
	t.Cleanup(srv.Close)
` + g.newTestClient("srv.URL") + `	c.Client.HTTPClient = srv.Client()

	return c, &starts
}

`
}

const newTest = `func TestNew(t *testing.T) {
	t.Parallel()

	if _, err := New("http://server", ""); err == nil {
		t.Error("New without a token succeeded")
	}
	if _, err := New("server", "test-token"); err == nil {
		t.Error("New without a scheme succeeded")
	}
	c, err := New("http://server/", "test-token")
	if err != nil || c.Client.BaseURL != "http://server" {
		t.Errorf("New = %+v, %v", c, err)
	}

	// a refusal that echoes the credential does not repeat it
	c, _ = newOperationServer(t, 401, "application/json", ` + "`" + `{"message": "the key test-token is not known"}` + "`" + `)
	req, err := c.Client.NewRequest(t.Context(), client.RequestOptions{HTTPMethod: http.MethodGet, Path: "/x", ExpectedStatusCodes: []int{http.StatusOK}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.Execute(t.Context()); err == nil || strings.Contains(err.Error(), "test-token") || !strings.Contains(err.Error(), "***") {
		t.Errorf("a refusal repeats the credential: %v", err)
	}
}
`

// testFile renders the test of one operation.
func (g *gen) testFile(o *definitions.Operation) string {
	if o.Call != nil {
		return g.callTestFile(o)
	}
	var b strings.Builder
	call := g.testCall(o)

	fmt.Fprintf(&b, "func TestOperation%s(t *testing.T) {\n\tt.Parallel()\n\n", o.Name)

	// the documented answer
	status := o.ExpectedStatusCodes[0]
	contentType, body := g.sampleResponse(o)
	fmt.Fprintf(&b, "\tc, s := newOperationServer(t, %d, %q, %q)\n", status, contentType, body)
	fmt.Fprintf(&b, "\tresult, err := %s\n", call.expr)
	b.WriteString("\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n")
	b.WriteString("\tr, body := s.only(t)\n")
	if o.Request == nil {
		b.WriteString("\t_ = body\n")
	}
	fmt.Fprintf(&b, "\texpectRequest(t, r, %s, %q)\n", httpMethods[o.Method], call.path)
	for _, q := range call.query {
		fmt.Fprintf(&b, "\texpectQuery(t, r, %q, %s)\n", q.name, quoteAll(q.values))
	}
	for _, h := range call.headers {
		fmt.Fprintf(&b, "\tif got := r.Header.Get(%q); got != %q {\n\t\tt.Errorf(\"header %s = %%q, want %%q\", got, %q)\n\t}\n", textproto.CanonicalMIMEHeaderKey(h.name), h.value, h.name, h.value)
	}
	if asksFor := o.Response.AsksFor(); asksFor != "" && !acceptOption(o) {
		fmt.Fprintf(&b, "\tif got := r.Header.Get(\"Accept\"); !strings.HasPrefix(got, %q) {\n\t\tt.Errorf(\"Accept = %%q, want %s asked for first\", got)\n\t}\n", asksFor, asksFor)
	}
	if o.Request != nil {
		fmt.Fprintf(&b, "\tif got := r.Header.Get(\"Content-Type\"); got != %q {\n\t\tt.Errorf(\"Content-Type = %%q, want %%q\", got, %q)\n\t}\n", call.bodyType, call.bodyType)
		if o.Request.Type.Type == definitions.RawFile {
			b.WriteString("\tif body != \"raw bytes\" {\n\t\tt.Errorf(\"body = %q, want the raw bytes\", body)\n\t}\n")
		} else {
			b.WriteString("\tif !json.Valid([]byte(body)) {\n\t\tt.Errorf(\"body = %q, want JSON\", body)\n\t}\n")
			if want, ok := g.emptyBodyJSON(o); ok {
				// a record with nothing set sends only what the document requires: a flag never set must not go out as false
				fmt.Fprintf(&b, "\tif body != %q {\n\t\tt.Errorf(\"an empty record sent %%q, want %%q: a field never set went out\", body, %q)\n\t}\n", want, want)
			}
		}
	}
	fmt.Fprintf(&b, "\tif result.HttpResponse == nil || result.HttpResponse.StatusCode != %s {\n\t\tt.Fatalf(\"HttpResponse = %%+v\", result.HttpResponse)\n\t}\n", statusName(status))
	b.WriteString(g.responseChecks(o, call))

	// a null result
	if o.Response != nil && o.Response.Type.Type != definitions.RawFile && status != http.StatusNoContent && slices.Contains(o.ExpectedStatusCodes, http.StatusNoContent) {
		b.WriteString("\n\t// a 204 is a null result, with no model\n")
		b.WriteString("\tc, _ = newOperationServer(t, http.StatusNoContent, \"\", \"\")\n")
		fmt.Fprintf(&b, "\tresult, err = %s\n", call.expr)
		b.WriteString("\tif err != nil || result.Answer != nil || result.Answered() {\n\t\tt.Errorf(\"a 204 = %v, answer %v, answered %t\", err, result.Answer, result.Answered())\n\t}\n")
		if embedded := g.embedded(o); embedded != "" {
			fmt.Fprintf(&b, "\tif result.%s == nil {\n\t\tt.Error(\"a 204 left no zero record behind the promoted fields\")\n\t}\n", embeddedField(embedded))
			b.WriteString("\tif _, err := result.Record(); err == nil {\n\t\tt.Error(\"Record of a 204 succeeded\")\n\t}\n")
		}
	}

	// an answer that is not the JSON documented; text is whatever was sent
	if o.Response != nil && o.Response.Type.Type != definitions.RawFile && !o.Response.Text() && status != http.StatusNoContent {
		b.WriteString("\n\t// an answer that does not decode is an error, with the response\n")
		fmt.Fprintf(&b, "\tc, _ = newOperationServer(t, %d, \"application/json\", \"<html>\")\n", status)
		fmt.Fprintf(&b, "\tresult, err = %s\n", call.expr)
		b.WriteString("\tif err == nil || client.StatusCode(err) != 0 || result.HttpResponse == nil {\n\t\tt.Errorf(\"an answer that does not decode = %v\", err)\n\t}\n")
	}

	// an undocumented status
	b.WriteString("\n\t// a status the operation does not document is an error, with the response\n")
	fmt.Fprintf(&b, "\tc, _ = newOperationServer(t, %d, \"text/plain\", \"no\")\n", undocumentedStatus)
	fmt.Fprintf(&b, "\tresult, err = %s\n", call.expr)
	fmt.Fprintf(&b, "\tif client.StatusCode(err) != %d || result.HttpResponse == nil {\n\t\tt.Errorf(\"an undocumented status = %%v, %%+v\", err, result.HttpResponse)\n\t}\n", undocumentedStatus)

	// options that may all be left out may be: the call goes without them, and more than one is refused unsent
	switch {
	case g.optional(o):
		without := strings.Join(call.args[:len(call.args)-1], ", ")
		b.WriteString("\n\t// the options may be left out, and two are refused before any request\n")
		fmt.Fprintf(&b, "\tc, s = newOperationServer(t, %d, %q, %q)\n", status, contentType, body)
		fmt.Fprintf(&b, "\tif _, err = c.%s(%s); err != nil {\n\t\tt.Errorf(\"without options = %%v\", err)\n\t}\n", o.Name, without)
		fmt.Fprintf(&b, "\tif _, err = c.%s(%s, %s{}, %s{}); err == nil || len(s.requests) != 1 {\n\t\tt.Errorf(\"two options structs = %%v, %%d requests\", err, len(s.requests))\n\t}\n", o.Name, without, g.typeName(o.Name+"Options"), g.typeName(o.Name+"Options"))
	case g.inline(o) && !o.Options[0].Required:
		without := strings.Join(call.args[:len(call.args)-len(call.inlined)], ", ")
		b.WriteString("\n\t// the option may be left out, and then nothing is sent for it\n")
		fmt.Fprintf(&b, "\tc, s = newOperationServer(t, %d, %q, %q)\n", status, contentType, body)
		fmt.Fprintf(&b, "\tif _, err = c.%s(%s); err != nil {\n\t\tt.Errorf(\"without the option = %%v\", err)\n\t}\n", o.Name, without)
		header := textproto.CanonicalMIMEHeaderKey(o.Options[0].Name)
		fmt.Fprintf(&b, "\tif r, _ := s.only(t); r.URL.RawQuery != \"\" || r.Header.Get(%q) != \"\" {\n\t\tt.Errorf(\"without the option, sent %%q and %%q\", r.URL.RawQuery, r.Header.Get(%q))\n\t}\n", header, header)
		if o.Options[0].Type.Type != definitions.List {
			b.WriteString("\t// two values are refused before any request\n")
			fmt.Fprintf(&b, "\tif _, err = c.%s(%s, %s, %s); err == nil || len(s.requests) != 1 {\n\t\tt.Errorf(\"two values = %%v, %%d requests\", err, len(s.requests))\n\t}\n", o.Name, without, call.inlined[0], call.inlined[0])
		}
	}
	b.WriteString("}\n")

	if o.Pageable != nil {
		b.WriteString(g.completeTest(o, call))
	}
	if o.Command != "" {
		b.WriteString(g.andWaitTest(o))
	}

	return g.fileHeader(b.String())
}

// testCallInfo is a call with sample arguments and what it should send.
type testCallInfo struct {
	expr     string // the call expression
	args     []string
	path     string // the escaped path expected
	query    []wireValue
	headers  []wireHeader
	bodyType string
	options  string   // the options literal, or ""
	inlined  []string // the arguments an inlined option went as, in place of the literal
	optNames map[string]string
}

// sampleValue is an inlined option's sample as arguments: the value the
// struct's sample pointed at, an optional list's elements one by one, a
// required list as the slice.
func (g *gen) sampleValue(opt *definitions.Option) []string {
	if opt.Type.Type == definitions.List {
		items, _ := g.sampleList(*opt.Type.NestedItem)
		if opt.Required {
			return []string{g.goType(opt.Type) + "{" + items + "}"}
		}

		return strings.Split(items, ", ")
	}
	expr, _, _ := g.sampleOption(*opt)
	if inner, ok := strings.CutPrefix(expr, "new("); ok {
		expr = strings.TrimSuffix(inner, ")")
	}

	return []string{expr}
}

type wireValue struct {
	name   string
	values []string
}

type wireHeader struct{ name, value string }

// testCall builds the call with a sample value for every path parameter and
// option, and the request those values should produce.
func (g *gen) testCall(o *definitions.Operation) testCallInfo {
	info := testCallInfo{optNames: map[string]string{}}
	args := []string{"t.Context()"}

	path := o.Path
	for _, p := range o.PathParameters {
		expr, wire := g.samplePath(p)
		args = append(args, expr)
		path = strings.Replace(path, "{"+p.Name+"}", wire, 1)
	}
	info.path = path

	if o.Request != nil {
		if o.Request.Type.Type == definitions.RawFile {
			info.bodyType = concreteType(o.Request.ContentType)
			args = append(args, `strings.NewReader("raw bytes")`, strconv.Quote(info.bodyType))
		} else {
			info.bodyType = o.Request.ContentType
			args = append(args, g.sampleBody(o.Request.Type))
		}
	}

	if len(o.Options) > 0 {
		var fields []string
		placed := map[string]bool{}
		for _, opt := range o.Options {
			expr, wire, ok := g.sampleOption(opt)
			if !ok {
				continue
			}
			// a joined list is sent under its string's name (JoinedLists adds UserIdList beside UserId), and setting both sends both, so
			// the test sets the first form alone and expects that one; the list's joining is proven where a list is the only form
			if placed[opt.In+" "+opt.Name] {
				continue
			}
			placed[opt.In+" "+opt.Name] = true
			info.optNames[opt.Field] = opt.Name
			field := opt.Field + ": " + expr + ","
			if opt.Deprecated && g.qual != "" {
				// set from another package, as a group's test sets the types package's options, a deprecated option is a staticcheck
				// finding; in the same package it is not, and a nolint nothing needs is a finding of its own
				field += " //nolint:staticcheck // the test sets every option, deprecated ones too"
			}
			fields = append(fields, field)
			switch {
			case opt.DeepObject:
				info.query = append(info.query, wireValue{opt.Name + "[a]", []string{"b"}}, wireValue{opt.Name + "[k]", []string{"v"}})
			case opt.In == definitions.InHeader:
				info.headers = append(info.headers, wireHeader{opt.Name, wire[0]})
			default:
				info.query = append(info.query, wireValue{opt.Name, wire})
			}
		}
		info.options = g.typeName(o.Name) + "Options{}"
		if len(fields) > 0 {
			info.options = g.typeName(o.Name) + "Options{\n\t\t" + strings.Join(fields, "\n\t\t") + "\n\t}"
		}
		args = append(args, info.options)
	}
	if g.inline(o) {
		// the one option goes as an argument: the sample value itself, a list as its elements
		info.inlined = g.sampleValue(&o.Options[0])
		args = append(args[:len(args)-1], info.inlined...)
	}
	info.args = args
	info.expr = "c." + o.Name + "(" + strings.Join(args, ", ") + ")"

	return info
}

// samplePath is a path argument and how it appears in the escaped path.
func (g *gen) samplePath(p definitions.PathParameter) (expr, wire string) {
	switch p.Type.Type {
	case definitions.Integer, definitions.Integer64:
		return "7", "7"
	case definitions.Float, definitions.Double:
		return "1.5", "1.5"
	case definitions.Reference:
		if c := g.constants[p.Type.ReferenceName]; c != nil && len(c.Values) > 0 {
			return g.typeName(c.Values[0].Name), url.PathEscape(c.Values[0].Value)
		}
	case definitions.DateTime:
		return g.typeName("Time") + "{Time: " + sampleTimeExpr + "}", url.PathEscape(sampleTime.Format(g.svc.Dates[0]))
	case definitions.Date:
		return g.typeName("Day") + "{Time: " + sampleTimeExpr + "}", url.PathEscape(sampleTime.Format(g.svc.Days[0]))
	case definitions.UnixSeconds:
		return g.typeName("Unix") + "{Time: " + sampleTimeExpr + "}", strconv.FormatInt(sampleTime.Unix(), 10)
	case definitions.UnixMillis:
		return g.typeName("UnixMilli") + "{Time: " + sampleTimeExpr + "}", strconv.FormatInt(sampleTime.UnixMilli(), 10)
	case definitions.ID:
		expr, wire := g.sampleID(p.Type.ReferenceName, p.Argument)

		return expr, url.PathEscape(wire)
	default:
	}
	// a slash in the value proves the argument is escaped
	value := "p/" + p.Argument

	return strconv.Quote(value), url.PathEscape(value)
}

// sampleOption is an option value and what it sends; ok is false for a type
// an option cannot hold.
func (g *gen) sampleOption(opt definitions.Option) (expr string, wire []string, ok bool) {
	switch opt.Type.Type {
	case definitions.Boolean:
		return "new(true)", []string{"true"}, true
	case definitions.Integer:
		return "new(7)", []string{"7"}, true
	case definitions.Integer64:
		return "new(int64(7))", []string{"7"}, true
	case definitions.Float:
		return "new(float32(1.5))", []string{"1.5"}, true
	case definitions.Double:
		return "new(1.5)", []string{"1.5"}, true
	case definitions.String:
		value := "v-" + opt.Field
		return strconv.Quote(value), []string{value}, true
	case definitions.DateTime:
		return g.typeName("Time") + "{Time: " + sampleTimeExpr + "}", []string{sampleTime.Format(g.svc.Dates[0])}, true
	case definitions.Date:
		return g.typeName("Day") + "{Time: " + sampleTimeExpr + "}", []string{sampleTime.Format(g.svc.Days[0])}, true
	case definitions.UnixSeconds:
		return g.typeName("Unix") + "{Time: " + sampleTimeExpr + "}", []string{strconv.FormatInt(sampleTime.Unix(), 10)}, true
	case definitions.UnixMillis:
		return g.typeName("UnixMilli") + "{Time: " + sampleTimeExpr + "}", []string{strconv.FormatInt(sampleTime.UnixMilli(), 10)}, true
	case definitions.ID:
		expr, wire := g.sampleID(opt.Type.ReferenceName, opt.Field)

		return expr, []string{wire}, true
	case definitions.Reference:
		if c := g.constants[opt.Type.ReferenceName]; c != nil && len(c.Values) > 0 {
			return g.typeName(c.Values[0].Name), []string{c.Values[0].Value}, true
		}
		return "", nil, false
	case definitions.Dictionary:
		if opt.DeepObject {
			// two keys, so the order on the wire is proven sorted
			return `map[string]string{"k": "v", "a": "b"}`, nil, true
		}
		return `map[string]string{"k": "v"}`, []string{`{"k":"v"}`}, true
	case definitions.List:
		items, values := g.sampleList(*opt.Type.NestedItem)
		if items == "" {
			return "", nil, false
		}
		if opt.CommaSeparated {
			values = []string{strings.Join(values, ",")}
		}
		return g.goType(opt.Type) + "{" + items + "}", values, true
	default:
		return "", nil, false
	}
}

// sampleList is two list elements and what each sends.
func (g *gen) sampleList(item definitions.TypeRef) (items string, values []string) {
	switch item.Type {
	case definitions.ID:
		kind := g.typeName(item.ReferenceName)
		// a kind with a shape refuses "a" or 1 before the call is sent, so a list of it is its example twice over
		if k := g.svc.IDKind(item.ReferenceName); k != nil && k.Example != "" {
			expr, wire := g.sampleID(item.ReferenceName, "")

			return expr + ", " + expr, []string{wire, wire}
		}
		if g.idBase(item.ReferenceName) == definitions.String {
			return kind + `("a"), ` + kind + `("b")`, []string{"a", "b"}
		}

		return kind + "(1), " + kind + "(2)", []string{"1", "2"}
	case definitions.String:
		return `"a", "b"`, []string{"a", "b"}
	case definitions.Integer, definitions.Integer64:
		return "1, 2", []string{"1", "2"}
	case definitions.Reference:
		c := g.constants[item.ReferenceName]
		if c == nil || len(c.Values) == 0 {
			return "", nil
		}
		last := c.Values[len(c.Values)-1]
		return g.typeName(c.Values[0].Name) + ", " + g.typeName(last.Name), []string{c.Values[0].Value, last.Value}
	default:
		return "", nil
	}
}

// sampleBody is a request body value.
func (g *gen) sampleBody(t definitions.TypeRef) string {
	switch t.Type {
	case definitions.List, definitions.Dictionary:
		return g.goType(t) + "{}"
	case definitions.Reference:
		if c := g.constants[t.ReferenceName]; c != nil && len(c.Values) > 0 {
			return g.typeName(c.Values[0].Name)
		}
		if m := g.models[t.ReferenceName]; m != nil && len(m.Union) > 0 && len(m.Members) == 0 {
			return g.typeName(t.ReferenceName) + `("{}")`
		}
		return g.typeName(t.ReferenceName) + "{}"
	default:
		return `json.RawMessage("{}")`
	}
}

// concreteType names a concrete media type for a range such as image/*.
func concreteType(contentType string) string {
	switch {
	case !strings.Contains(contentType, "*"):
		return contentType
	case strings.HasPrefix(contentType, "image/"):
		return "image/png"
	case strings.HasPrefix(contentType, "text/"):
		return "text/plain"
	case strings.HasPrefix(contentType, "audio/"):
		return "audio/mpeg"
	case strings.HasPrefix(contentType, "video/"):
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

// sampleResponse is the answer the canned server gives for the operation's
// documented response.
func (g *gen) sampleResponse(o *definitions.Operation) (contentType, body string) {
	switch {
	case o.Response == nil:
		return "", ""
	case o.Response.Type.Type == definitions.RawFile:
		return concreteType(o.Response.ContentType), "file bytes"
	case o.Response.Text():
		// labelled JSON, as a server that sends text may label it: the method reads it as text all the same
		return definitions.JSONContentType, sampleText
	default:
		return "application/json", g.sampleJSON(o.Response.Type)
	}
}

// sampleJSON is a JSON document of a type.
func (g *gen) sampleJSON(t definitions.TypeRef) string {
	switch t.Type {
	case definitions.List:
		return "[" + g.sampleJSON(*t.NestedItem) + "]"
	case definitions.String:
		return `"s"`
	case definitions.DateTime:
		if g.svc.Dates[0] == dateObjectMillis {
			return fmt.Sprintf(`{"$date":%d}`, sampleTime.UnixMilli())
		}

		return strconv.Quote(sampleTime.Format(g.svc.Dates[0]))
	case definitions.Date:
		return strconv.Quote(sampleTime.Format(g.svc.Days[0]))
	case definitions.UnixSeconds:
		return strconv.FormatInt(sampleTime.Unix(), 10)
	case definitions.UnixMillis:
		return strconv.FormatInt(sampleTime.UnixMilli(), 10)
	case definitions.ID:
		if g.idBase(t.ReferenceName) == definitions.String {
			return `"s"`
		}

		return "7"
	case definitions.Integer, definitions.Integer64:
		return "7"
	case definitions.Float, definitions.Double:
		return "1.5"
	case definitions.Boolean:
		return "true"
	case definitions.Reference:
		if c := g.constants[t.ReferenceName]; c != nil && len(c.Values) > 0 {
			b, _ := json.Marshal(c.Values[0].Value)
			return string(b)
		}
		return "{}"
	default:
		return "{}"
	}
}

// responseChecks checks what the operation decoded or streamed.
func (g *gen) responseChecks(o *definitions.Operation, call testCallInfo) string {
	switch {
	case o.Response == nil:
		return ""
	case o.Response.Type.Type == definitions.RawFile && o.Response.ContentType == eventStream:
		return "\tstream, contentType := result.Reader()\n\tif contentType == \"\" || result.ContentLength() == 0 {\n\t\tt.Errorf(\"Reader = %q, length %d\", contentType, result.ContentLength())\n\t}\n" +
			"\tstreamed, _ := io.ReadAll(stream)\n\t_ = stream.Close()\n\tif string(streamed) != \"file bytes\" {\n\t\tt.Errorf(\"streamed body = %q\", streamed)\n\t}\n"
	case o.Response.Type.Type == definitions.RawFile:
		return "\tif _, contentType := result.Reader(); contentType == \"\" || result.ContentLength() != 10 || result.Filename() != \"answer.bin\" {\n\t\tt.Errorf(\"Reader = %q, length %d, filename %q\", contentType, result.ContentLength(), result.Filename())\n\t}\n" +
			"\tif data, err := result.Bytes(9); err == nil || len(data) != 0 {\n\t\tt.Errorf(\"Bytes over the size = %q, %v\", data, err)\n\t}\n" +
			"\tc, _ = newOperationServer(t, " + strconv.Itoa(o.ExpectedStatusCodes[0]) + ", " + strconv.Quote(concreteType(o.Response.ContentType)) + ", \"file bytes\")\n\tresult, err = " + call.expr + "\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n" +
			"\tif data, err := result.Bytes(10); err != nil || string(data) != \"file bytes\" {\n\t\tt.Errorf(\"Bytes = %q, %v\", data, err)\n\t}\n" +
			"\tc, _ = newOperationServer(t, " + strconv.Itoa(o.ExpectedStatusCodes[0]) + ", " + strconv.Quote(concreteType(o.Response.ContentType)) + ", \"file bytes\")\n\tresult, err = " + call.expr + "\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n" +
			"\tvar streamed bytes.Buffer\n\tif _, err := result.WriteTo(&streamed); err != nil || streamed.String() != \"file bytes\" {\n\t\tt.Errorf(\"WriteTo = %v, %q\", err, streamed.String())\n\t}\n" +
			"\tif _, err := result.WriteTo(&streamed); err == nil {\n\t\tt.Error(\"WriteTo on a spent body succeeded\")\n\t}\n" +
			"\tif _, err := (" + g.typeName(o.Name+"Response") + "{}).WriteTo(&streamed); err == nil {\n\t\tt.Error(\"WriteTo with no answer succeeded\")\n\t}\n" +
			"\tif body, contentType := (" + g.typeName(o.Name+"Response") + "{}).Reader(); body == nil || contentType != \"\" || body.Close() != nil {\n\t\tt.Error(\"Reader with no answer is not safely empty\")\n\t}\n"
	}
	typ, pointer := g.responseModel(o)
	switch {
	case o.Response.Text():
		return fmt.Sprintf("\tif result.Answer == nil || *result.Answer != %q {\n\t\tt.Errorf(\"the text was not held as it was sent: %%v\", result.Answer)\n\t}\n", sampleText)
	case pointer:
		check := "\tif result.Answer == nil || !result.Answered() {\n\t\tt.Error(\"the answer was not decoded\")\n\t}\n"
		if embedded := g.embedded(o); embedded != "" {
			check += "\tif result." + embeddedField(embedded) + " != result.Answer {\n\t\tt.Error(\"the embedded answer is not the answer\")\n\t}\n"
			check += "\tif record, err := result.Record(); err != nil || !reflect.DeepEqual(&record, result.Answer) {\n\t\tt.Errorf(\"Record = %+v, %v\", record, err)\n\t}\n"
		}

		return check
	case typ == "json.RawMessage" || strings.HasPrefix(typ, "[]") || strings.HasPrefix(typ, "map["):
		return "\tif result.Answer == nil || !result.Answered() {\n\t\tt.Error(\"the answer was not decoded\")\n\t}\n"
	default:
		// a union alias or any: decoded as raw JSON
		return "\t_ = result.Answer\n"
	}
}

// pagedNames is what a list's answer calls its items and its total on the wire.
func (g *gen) pagedNames(o *definitions.Operation) (items, total string) {
	items, total = o.Pageable.ItemsField, o.Pageable.TotalField
	if m := g.models[o.Response.Type.ReferenceName]; m != nil {
		for i := range m.Fields {
			switch m.Fields[i].Name {
			case o.Pageable.ItemsField:
				items = m.Fields[i].JSONName
			case o.Pageable.TotalField:
				total = m.Fields[i].JSONName
			}
		}
	}

	return items, total
}

// completeTest proves a paged list's Complete method walks the pages.
func (g *gen) completeTest(o *definitions.Operation, call testCallInfo) string {
	p := o.Pageable
	startName := call.optNames[p.StartIndexOption]
	items, total := g.pagedNames(o)
	args := strings.Join(slicesWithout(call.args, call.options), ", ")
	if args != "" {
		args += ", "
	}
	options := func(limit int) string {
		if p.LimitOption == "" {
			return g.typeName(o.Name) + "Options{}"
		}

		return fmt.Sprintf("%sOptions{%s: new(%d)}", g.typeName(o.Name), p.LimitOption, limit)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nfunc TestOperation%sComplete(t *testing.T) {\n\tt.Parallel()\n\n", o.Name)
	b.WriteString("\t// the three ways a walk ends: the total is reached, an empty page when the\n")
	b.WriteString("\t// server reports no total, and a page shorter than the limit\n")
	b.WriteString("\tfor _, tc := range []struct {\n\t\tname          string\n\t\ttotal, pages  int\n\t\toptions       ")
	fmt.Fprintf(&b, "%sOptions\n\t\titems         int\n\t\tstarts        []string\n\t}{\n", g.typeName(o.Name))
	// a walk by index starts with no index and goes on from the count it
	// has; one by page asks for the first page and then the next
	starts := []string{`"", "1"`, `"", "1", "2"`, `""`}
	if p.ByPage {
		first := p.First()
		page := func(n int) string { return strconv.Itoa(first + n) }
		starts = []string{`"` + page(0) + `", "` + page(1) + `"`, `"` + page(0) + `", "` + page(1) + `", "` + page(2) + `"`, `"` + page(0) + `"`}
	}
	fmt.Fprintf(&b, "\t\t{\"total reached\", 2, 9, %s, 2, []string{%s}},\n", options(1), starts[0])
	fmt.Fprintf(&b, "\t\t{\"empty page\", 0, 2, %s, 2, []string{%s}},\n", options(1), starts[1])
	if p.LimitOption != "" {
		// with the server's page size learnt from the first page, every page it answers is full
		fmt.Fprintf(&b, "\t\t{\"short page\", 0, 9, %s, 1, []string{%s}},\n", options(2), starts[2])
	}
	b.WriteString("\t} {\n\t\tt.Run(tc.name, func(t *testing.T) {\n\t\t\tt.Parallel()\n\n")
	fmt.Fprintf(&b, "\t\t\tc, starts := pagedServer(t, %q, %q, %q, %q, tc.total, tc.pages)\n", startName, items, total, g.sampleJSON(p.ItemType))
	fmt.Fprintf(&b, "\t\t\tresult, err := c.%sComplete(%stc.options)\n", o.Name, args)
	b.WriteString("\t\t\tif err != nil {\n\t\t\t\tt.Fatal(err)\n\t\t\t}\n")
	b.WriteString("\t\t\tif len(result.Items) != tc.items || !slices.Equal(*starts, tc.starts) || result.LatestHttpResponse == nil {\n")
	b.WriteString("\t\t\t\tt.Errorf(\"Complete = %d items over pages starting %q, want %d over %q\", len(result.Items), *starts, tc.items, tc.starts)\n")
	b.WriteString("\t\t\t}\n\t\t})\n\t}\n}\n")

	// Each hands out the same results one at a time, and a loop that stops asks for no page more
	fmt.Fprintf(&b, "\nfunc TestOperation%sEach(t *testing.T) {\n\tt.Parallel()\n\n", o.Name)
	fmt.Fprintf(&b, "\tc, starts := pagedServer(t, %q, %q, %q, %q, 0, 2)\n", startName, items, total, g.sampleJSON(p.ItemType))
	b.WriteString("\tn := 0\n")
	fmt.Fprintf(&b, "\tfor _, err := range c.%sEach(%s%s) {\n\t\tif err != nil {\n\t\t\tt.Fatal(err)\n\t\t}\n\t\tn++\n\t}\n", o.Name, args, options(1))
	b.WriteString("\tif n != 2 || len(*starts) != 3 {\n\t\tt.Errorf(\"Each handed out %d results over %d pages, want 2 over 3\", n, len(*starts))\n\t}\n\n")
	b.WriteString("\t// stopping after the first result asks for no page more\n")
	fmt.Fprintf(&b, "\tc, starts = pagedServer(t, %q, %q, %q, %q, 0, 2)\n", startName, items, total, g.sampleJSON(p.ItemType))
	fmt.Fprintf(&b, "\tfor range c.%sEach(%s%s) {\n\t\tbreak\n\t}\n", o.Name, args, options(1))
	b.WriteString("\tif len(*starts) != 1 {\n\t\tt.Errorf(\"a stopped Each asked for %d pages, want 1\", len(*starts))\n\t}\n}\n")

	return b.String()
}

func slicesWithout(items []string, drop string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if drop == "" || it != drop {
			out = append(out, it)
		}
	}

	return out
}

func quoteAll(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, strconv.Quote(v))
	}

	return strings.Join(quoted, ", ")
}

// sampleTime is the date the generated tests send and answer, and
// sampleTimeExpr the same as Go source.
var sampleTime = time.Date(2024, time.March, 4, 5, 6, 7, 0, time.UTC)

const sampleTimeExpr = "time.Date(2024, time.March, 4, 5, 6, 7, 0, time.UTC)"

// sampleID is a sample of a kind of id as Go source and as it is sent.
func (g *gen) sampleID(kind, name string) (expr, wire string) {
	example := ""
	if k := g.svc.IDKind(kind); k != nil {
		example = k.Example
	}
	if g.idBase(kind) == definitions.String {
		if example != "" {
			return g.typeName(kind) + "(" + strconv.Quote(example) + ")", example
		}

		return g.typeName(kind) + `("v-` + name + `")`, "v-" + name
	}
	if example != "" {
		return g.typeName(kind) + "(" + example + ")", example
	}

	return g.typeName(kind) + "(7)", "7"
}

// emptyBodyJSON is what an operation's request body must go out as when
// nothing is set: {} for a struct model with no required field, else the
// required fields alone at their zeros, in the model's order; nothing else
// may appear. Not for a model written back whole, which sends everything.
func (g *gen) emptyBodyJSON(o *definitions.Operation) (string, bool) {
	if o.Request == nil || o.Request.Type.Type != definitions.Reference {
		return "", false
	}
	m := g.models[o.Request.Type.ReferenceName]
	if m == nil || m.WrittenWhole || len(m.Union) > 0 {
		return "", false
	}
	if o.Command != "" {
		// a command's name is set by the method whatever the caller set
		return fmt.Sprintf(`{"name":%q}`, o.Command), true
	}
	var parts []string
	for i := range m.Fields {
		f := &m.Fields[i]
		if !f.Required {
			continue
		}
		zero, ok := g.zeroJSON(m, f)
		if !ok {
			return "", false
		}
		parts = append(parts, strconv.Quote(f.JSONName)+":"+zero)
	}

	return "{" + strings.Join(parts, ",") + "}", true
}

// zeroJSON is the JSON a required field of a model sends when nothing set
// it, or false for a kind whose zero this cannot say.
func (g *gen) zeroJSON(m *definitions.Model, f *definitions.Field) (string, bool) {
	if g.fieldType(m, f)[0] == '*' || f.Type.Type == definitions.List || f.Type.Type == definitions.Dictionary {
		return "null", true
	}
	switch f.Type.Type {
	case definitions.Boolean:
		return "false", true
	case definitions.Integer, definitions.Integer64, definitions.Float, definitions.Double:
		return "0", true
	case definitions.String:
		return `""`, true
	case definitions.DateTime, definitions.Date, definitions.UnixSeconds, definitions.UnixMillis:
		return "null", true
	case definitions.ID:
		if g.idBase(f.Type.ReferenceName) == definitions.String {
			return `""`, true
		}

		return "0", true
	case definitions.Reference:
		if g.constants[f.Type.ReferenceName] != nil {
			return `""`, true
		}

		return "", false
	default:
		return "", false
	}
}

// andWaitTest proves a command's AndWait queues it and waits: a server that answers the queueing and then says the command
// finished, one that says it failed, and the poll that gives up.
func (g *gen) andWaitTest(o *definitions.Operation) string {
	queue := g.queueing(o)
	if queue == nil {
		return ""
	}
	set := queue.Commands
	get := g.operationNamed(set.Get)
	if get == nil || get.Response == nil {
		return ""
	}
	answer := get.Response.Type.ReferenceName
	idJSON, statusJSON := g.jsonFor(answer, "id"), g.jsonFor(answer, set.Status)
	body := func(status string) string { return fmt.Sprintf(`{%q: 7, %q: %q}`, idJSON, statusJSON, status) }
	polled := get.ExpectedStatusCodes[0]
	server := func(status string) string {
		return fmt.Sprintf("\tc, s := newOperationServer(t, %d, \"application/json\", %q)\n\ts.queued = %d\n", polled, body(status), o.ExpectedStatusCodes[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nfunc TestOperation%sAndWait(t *testing.T) {\n\tt.Parallel()\n\n", o.Name)
	b.WriteString(server(set.Finished[0]))
	fmt.Fprintf(&b, "\tresult, err := c.%sAndWait(t.Context(), %s{}, time.Second)\n", o.Name, g.goType(o.Request.Type))
	b.WriteString("\tif err != nil || !result.Answered() || len(s.requests) != 2 {\n\t\tt.Fatalf(\"AndWait = %v, %v after %d requests, want the command finished after queueing it and asking once\", result.Answer, err, len(s.requests))\n\t}\n")
	fmt.Fprintf(&b, "\tif got := s.requests[0]; got.Method != http.MethodPost || !strings.Contains(s.bodies[0], %q) {\n\t\tt.Errorf(\"the command went as %%s %%s\", got.Method, s.bodies[0])\n\t}\n", `"name":"`+o.Command+`"`)
	if len(set.Failed) > 0 {
		b.WriteString("\n" + strings.Replace(server(set.Failed[0]), "c, s :=", "c, s =", 1))
		fmt.Fprintf(&b, "\tif _, err := c.%sAndWait(t.Context(), %s{}, time.Second); !errors.Is(err, client.ErrCommandFailed) || !strings.Contains(err.Error(), %q) {\n\t\tt.Errorf(\"a failed command = %%v\", err)\n\t}\n", o.Name, g.goType(o.Request.Type), set.Failed[0])
	}
	b.WriteString("\n\t// a command still running when the wait ends is an error saying so\n")
	b.WriteString(strings.Replace(server("running"), "c, s :=", "c, s =", 1))
	fmt.Fprintf(&b, "\tif _, err := c.%sAndWait(t.Context(), %s{}, 0); !errors.Is(err, client.ErrCommandRunning) || !strings.Contains(err.Error(), \"still running\") {\n\t\tt.Errorf(\"a command the wait gave up on = %%v\", err)\n\t}\n}\n", o.Name, g.goType(o.Request.Type))

	return b.String()
}

// jsonFor is the JSON name a model holds a field as, by its JSON name ignoring case, or the name given.
func (g *gen) jsonFor(model, jsonName string) string {
	m := g.models[model]
	if m == nil {
		return jsonName
	}
	for i := range m.Fields {
		if strings.EqualFold(m.Fields[i].JSONName, jsonName) {
			return m.Fields[i].JSONName
		}
	}

	return jsonName
}
