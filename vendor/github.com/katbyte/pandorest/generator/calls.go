package generator

import (
	"fmt"
	"go/token"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/katbyte/pandorest/definitions"
)

// The call layout, for a service whose document is a JSON-RPC method description (config.Service.Document): every operation
// is a call, a method name and its arguments in order, and the package sends each through a client.Caller the repository
// writes over its connection, in place of the base client and its requests. The files are the request layout's, flat or
// grouped, with the client.go and the tests of a call service: New takes the Caller, and a test records the call a method
// makes through a Caller of its own.

// dateObjectMillis is the layout of a date written as {"$date": milliseconds}, as the base client names it
// (client.DateObjectMillis), which a test's sample date is written in when it is the service's first layout.
const dateObjectMillis = "$date"

// calls reports whether the service is one of JSON-RPC methods, which every operation then is.
func (g *gen) calls() bool {
	for _, o := range g.svc.Operations() {
		if o.Call != nil {
			return true
		}
	}

	return false
}

// callArguments are a call's arguments as the method takes them, and as it passes them on.
func (g *gen) callArguments(o *definitions.Operation) (params, call []string) {
	for _, a := range o.Arguments {
		params = append(params, a.Argument+" "+g.goType(a.Type))
		call = append(call, a.Argument)
	}

	return params, call
}

// requiredArguments is how many of a call's arguments are sent whatever their value: up to and including the last the server
// insists on, since an argument is sent by position and one left off the end is the only one left out.
func requiredArguments(o *definitions.Operation) int {
	n := 0
	for i, a := range o.Arguments {
		if a.Required {
			n = i + 1
		}
	}

	return n
}

// callMethodName is the name a call's method has on the client it sits on. In a package per namespace the namespace comes off
// the front, so pool.query is Query on the pool package's Client and a caller writes c.Pool.Query; in one package, and where
// nothing comes off, it is the operation's name, PoolQuery, which its response type keeps either way. grp is the group whose
// package the method is written into, nil in the flat layout.
func callMethodName(grp *definitions.Group, o *definitions.Operation) string {
	if grp == nil || o.Call == nil {
		return o.Name
	}
	short := strings.TrimPrefix(o.Name, grp.Name)
	if short == o.Name || !token.IsExported(short) || short == "Caller" {
		return o.Name
	}

	return short
}

// writeCallMethod writes the method of a call: it hands the method's name and its arguments, in order and trimmed of those
// left off the end, to the Caller, and decodes the answer as the request layout's method does a response body.
func (g *gen) writeCallMethod(b *strings.Builder, o *definitions.Operation, responseType string) {
	name := callMethodName(g.grp, o)
	g.declare("Client."+name, "operation "+o.Name)
	params, call := g.callArguments(o)

	switch o.Call.Form {
	case definitions.QueryGet:
		fmt.Fprintf(b, "// %s calls %s for one record: the first the filters match, which the server refuses to answer when they match none.", name, o.Call.Method)
	case definitions.QueryCount:
		fmt.Fprintf(b, "// %s calls %s for how many records the filters match.", name, o.Call.Method)
	default:
		fmt.Fprintf(b, "// %s calls %s.", name, o.Call.Method)
	}
	if s := firstSentence(o.Description); s != "" && o.Call.Form == "" {
		b.WriteString(" " + s)
	}
	b.WriteString("\n")
	if o.Call.Form != "" {
		// the form's flag is set in the options by the method, so the caller's options go out with it set
		options := slices.IndexFunc(o.Arguments, func(a definitions.Argument) bool { return a.Name == "options" })
		call[options] = fmt.Sprintf("client.Flagged{Record: %s, Flag: %q}", call[options], o.Call.Form)
	}
	for _, a := range o.Arguments {
		if s := firstSentence(a.Description); s != "" {
			fmt.Fprintf(b, "//\n// %s: %s\n", a.Argument, s)
		}
	}
	switch {
	case o.Call.Job:
		b.WriteString("//\n// The server runs it as a job and answers when the job is done, on a connection that asked for that; one that did not is\n// answered the job's number instead.\n")
	case o.Call.Read:
		b.WriteString("//\n// It changes nothing on the server.\n")
	}
	if o.Call.Uploadable {
		b.WriteString("//\n// It takes files beside its arguments, sent through a connection that can carry them (client.Uploader); called with none,\n// it is a plain call.\n")
		params = append(params, "files ...io.Reader")
	}
	if o.Call.Downloadable {
		fmt.Fprintf(b, "//\n// Its answer is a file: %sTo writes it out.\n", name)
	}
	if o.Deprecated {
		b.WriteString("//\n// Deprecated: the document marks this method deprecated.\n")
	}
	fmt.Fprintf(b, "func (c Client) %s(%s) (result %s, err error) {\n", name, signature(append([]string{"ctx context.Context"}, params...)...), responseType)
	if embedded := g.embedded(o); embedded != "" {
		// a zero record until one is decoded, so a promoted field reads empty rather than panicking
		fmt.Fprintf(b, "\tresult.%s = &%s{}\n", embeddedField(embedded), embedded)
	}
	if len(call) == 0 {
		b.WriteString("\tvar params []any\n")
	} else {
		fmt.Fprintf(b, "\tparams := client.CallParams(%d, %s)\n", requiredArguments(o), strings.Join(call, ", "))
	}
	send := "c.Caller.Call"
	if o.Call.Uploadable {
		send = "send"
		b.WriteString("\tsend := c.Caller.Call\n\tif len(files) > 0 {\n\t\tsend = func(ctx context.Context, method string, params []any, into any) error { return client.UploadWith(ctx, c.Caller, method, params, into, files...) }\n\t}\n")
	}

	typ, pointer := g.responseModel(o)
	switch {
	case typ == "":
		fmt.Fprintf(b, "\terr = %s(ctx, %q, params, nil)\n\n\treturn result, err\n}\n", send, o.Call.Method)
	case pointer:
		fmt.Fprintf(b, "\tvar answer %s\n\tif err := %s(ctx, %q, params, &answer); err != nil {\n\t\treturn result, err\n\t}\n\tresult.Answer = &answer\n", strings.TrimPrefix(typ, "*"), send, o.Call.Method)
		if embedded := g.embedded(o); embedded != "" {
			fmt.Fprintf(b, "\tresult.%s = result.Answer\n", embeddedField(embedded))
		}
		b.WriteString("\n\treturn result, nil\n}\n")
	default:
		fmt.Fprintf(b, "\terr = %s(ctx, %q, params, &result.Answer)\n\n\treturn result, err\n}\n", send, o.Call.Method)
	}

	if o.Call.Job && !o.Call.Uploadable && o.Call.Form == "" {
		// the call itself, for a connection of the repository's own that starts a job and follows it rather than waiting
		g.declare(name+"Call", "operation "+o.Name)
		fmt.Fprintf(b, "\n// %sCall is the call %s makes, the method's name and its arguments as sent, for a connection that starts the job and\n// follows it rather than waiting for it.\nfunc %sCall(%s) (call string, params []any) {\n", name, name, name, signature(params...))
		if len(call) == 0 {
			fmt.Fprintf(b, "\treturn %q, nil\n}\n", o.Call.Method)
		} else {
			fmt.Fprintf(b, "\treturn %q, client.CallParams(%d, %s)\n}\n", o.Call.Method, requiredArguments(o), strings.Join(call, ", "))
		}
	}
	if o.Call.Downloadable {
		// the file the method answers, written out through the connection
		g.declare("Client."+name+"To", "operation "+o.Name)
		fmt.Fprintf(b, "\n// %sTo calls %s for the file it answers and writes it to w, answering how many bytes, through a connection that can carry\n// a file (client.Downloader).\nfunc (c Client) %sTo(%s) (int64, error) {\n", name, o.Call.Method, name, signature(append([]string{"ctx context.Context", "w io.Writer"}, params...)...))
		if len(call) == 0 {
			b.WriteString("\tvar params []any\n")
		} else {
			fmt.Fprintf(b, "\tparams := client.CallParams(%d, %s)\n", requiredArguments(o), strings.Join(call, ", "))
		}
		fmt.Fprintf(b, "\n\treturn client.DownloadWith(ctx, c.Caller, %q, params, w)\n}\n", o.Call.Method)
	}
}

// operationsFile is the types package's operations.go: a table of every operation a program can go by, where the comments say
// the same to a reader. A request has its HTTP method and path; a call the server's method.
func (g *gen) operationsFile() string {
	g.declare("OperationInfo", "operations.go")
	g.declare("Operations", "operations.go")

	var b strings.Builder
	b.WriteString("// OperationInfo is one operation of the package, as a program goes by it: Name is the operation's name, which its method,\n// response type and test carry; Method and Path its HTTP method and path, or Method the server's method for a call; Group the\n// tag whose client has it; Read whether it changes nothing on the server (a GET, or a call that may be made by a read-only\n// role); Job whether the server runs a call as a job; Form the one-record (\"get\") or count (\"count\") form of a query; and\n// File \"download\" for an operation whose answer is a file or \"upload\" for one that takes one.\ntype OperationInfo struct {\n\tName   string\n\tMethod string\n\tPath   string\n\tGroup  string\n\tRead   bool\n\tJob    bool\n\tForm   string\n\tFile   string\n}\n\n")
	b.WriteString("// Operations is every operation of the package, group by group in the order of their names.\nvar Operations = []OperationInfo{\n")
	for gi := range g.svc.Groups {
		grp := &g.svc.Groups[gi]
		for oi := range grp.Operations {
			o := &grp.Operations[oi]
			row := operationRow{Name: o.Name, Group: grp.Name}
			switch {
			case o.Call != nil:
				row.Method, row.Read, row.Job, row.Form = o.Call.Method, o.Call.Read, o.Call.Job, o.Call.Form
				if o.Call.Downloadable {
					row.File = "download"
				} else if o.Call.Uploadable {
					row.File = "upload"
				}
			default:
				row.Method, row.Path, row.Read = o.Method, o.Path, o.Method == http.MethodGet
				if o.Response != nil && o.Response.Type.Type == definitions.RawFile {
					row.File = "download"
				} else if o.Request != nil && o.Request.Type.Type == definitions.RawFile {
					row.File = "upload"
				}
			}
			// only what the row has: a field left out reads as its zero
			fields := []string{fmt.Sprintf("Name: %q", row.Name), fmt.Sprintf("Method: %q", row.Method)}
			if row.Path != "" {
				fields = append(fields, fmt.Sprintf("Path: %q", row.Path))
			}
			fields = append(fields, fmt.Sprintf("Group: %q", row.Group))
			if row.Read {
				fields = append(fields, "Read: true")
			}
			if row.Job {
				fields = append(fields, "Job: true")
			}
			if row.Form != "" {
				fields = append(fields, fmt.Sprintf("Form: %q", row.Form))
			}
			if row.File != "" {
				fields = append(fields, fmt.Sprintf("File: %q", row.File))
			}
			fmt.Fprintf(&b, "\t{%s},\n", strings.Join(fields, ", "))
		}
	}
	b.WriteString("}\n")

	return g.fileHeader(b.String())
}

// operationRow is a row of the generated Operations table, as the generator builds it before writing it out.
type operationRow struct {
	Name, Method, Path, Group string
	Read, Job                 bool
	Form, File                string
}

// callClient is the client.go of a call service in the flat layout: the Client over a Caller, and New.
const callClient = `// Client is a client for %[1]s; each of the server's methods is a method on it.
// Calls go through Client.Caller, the connection the repository provides.
type Client struct {
	Caller client.Caller
}

// New returns a client for %[1]s over a connection that is dialled and logged in,
// which the repository's own code provides as a client.Caller.
func New(caller client.Caller) (*Client, error) {
	if caller == nil {
		return nil, errors.New("a connection is required")
	}

	return &Client{Caller: caller}, nil
}
`

// callClientFile is the flat layout's client.go for a call service.
func (g *gen) callClientFile() string {
	if g.opts.Client != "" {
		return g.fileHeader(g.apiVersionConsts() + strings.TrimSpace(g.opts.Client) + "\n")
	}

	return g.fileHeader(g.apiVersionConsts() + fmt.Sprintf(callClient, g.svc.Title))
}

// callGroupClientFile is a tag's client.go for a call service.
func (g *gen) callGroupClientFile(grp *definitions.Group) string {
	return g.fileHeader(fmt.Sprintf(`// Client is the %[1]s methods of %[2]s; each is a method. Calls go through
// Client.Caller, the connection shared with every other tag's.
type Client struct {
	Caller client.Caller
}

// NewClient makes the client of these methods over a connection, which
// client.New does for every tag at once.
func NewClient(caller client.Caller) Client { return Client{Caller: caller} }
`, grp.Name, g.svc.Title))
}

// callAggregateFile is the client package's client.go for a call service: New over a Caller, and a Client with a field per tag.
func (g *gen) callAggregateFile() string {
	g.declare("Client", clientFileName)
	g.declare("New", clientFileName)
	g.imports = []importSpec{{g.opts.ClientImport, "base"}}
	groups := g.groupsWithOperations()
	for _, grp := range groups {
		g.imports = append(g.imports, importSpec{path.Join(g.opts.ImportPath, g.pkgs[grp.Name]), g.pkgs[grp.Name]})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "// Client is a client for %s: the methods of each tag on a field of its own,\n// every one calling through Caller, the shared connection.\ntype Client struct {\n\tCaller base.Caller\n", g.svc.Title)
	for _, grp := range groups {
		fmt.Fprintf(&b, "\t%s %s.Client\n", grp.Name, g.pkgs[grp.Name])
	}
	b.WriteString("}\n\n")
	b.WriteString("// New makes a Client of every tag's methods over a connection that is dialled and\n// logged in, which the repository's own code provides as a Caller.\nfunc New(caller base.Caller) (*Client, error) {\n\tif caller == nil {\n\t\treturn nil, errors.New(\"a connection is required\")\n\t}\n\n\treturn &Client{\n\t\tCaller: caller,\n")
	for _, grp := range groups {
		fmt.Fprintf(&b, "\t\t%s: %s.NewClient(caller),\n", grp.Name, g.pkgs[grp.Name])
	}
	b.WriteString("\t}, nil\n}\n")

	return g.fileHeader(b.String())
}

// callAggregateTestFile proves New refuses no connection and hands every tag the one it is given.
func (g *gen) callAggregateTestFile() string {
	g.imports = []importSpec{{g.opts.ClientImport, "base"}}
	groups := g.groupsWithOperations()
	shared := make([]string, 0, len(groups))
	for _, grp := range groups {
		shared = append(shared, "c."+grp.Name+".Caller != caller")
	}

	return g.fileHeader(fmt.Sprintf(`// connection is a Caller a test can compare, which a function cannot be.
type connection struct{ base.Caller }

func TestNew(t *testing.T) {
	t.Parallel()

	if _, err := New(nil); err == nil {
		t.Error("New without a connection succeeded")
	}
	caller := &connection{}
	c, err := New(caller)
	if err != nil {
		t.Fatal(err)
	}
	// every tag's client calls through the one connection
	if %s {
		t.Error("a tag's client does not share the connection")
	}
}
`, strings.Join(shared, " || ")))
}

// callRecorderHelpers are the test helpers of a call service: a Caller that records the one call it is sent and answers
// canned JSON, or fails it the way a server does.
const callRecorderHelpers = `// callRecorder is a connection for a test: it records the call a method makes and
// answers it with canned JSON, or fails it with what a server would say.
type callRecorder struct {
	method   string
	params   []any
	answer   string
	fail     error
	uploaded string
}

// Call records the call and answers it.
func (r *callRecorder) Call(_ context.Context, method string, params []any, into any) error {
	r.method, r.params = method, params
	if r.fail != nil {
		return r.fail
	}
	if into == nil || r.answer == "" {
		return nil
	}

	return json.Unmarshal([]byte(r.answer), into)
}

// Upload records the call and the files sent beside it, and answers it.
func (r *callRecorder) Upload(ctx context.Context, method string, params []any, into any, files ...io.Reader) error {
	var b strings.Builder
	for _, f := range files {
		_, _ = io.Copy(&b, f)
	}
	r.uploaded = b.String()

	return r.Call(ctx, method, params, into)
}

// Download records the call and writes a file for it.
func (r *callRecorder) Download(_ context.Context, method string, params []any, w io.Writer) (int64, error) {
	r.method, r.params = method, params
	if r.fail != nil {
		return 0, r.fail
	}
	n, err := io.WriteString(w, "file bytes")

	return int64(n), err
}

// newCallRecorder makes a client over a recording connection that answers the JSON given, or fails every call with fail.
func newCallRecorder(t *testing.T, answer string, fail error) (*Client, *callRecorder) {
	t.Helper()

	r := &callRecorder{answer: answer, fail: fail}
	c, err := New(r)
	if err != nil {
		t.Fatal(err)
	}

	return c, r
}

`

// callNewTest proves the flat layout's New refuses no connection.
const callNewTest = `func TestNew(t *testing.T) {
	t.Parallel()

	if _, err := New(nil); err == nil {
		t.Error("New without a connection succeeded")
	}
	c, r := newCallRecorder(t, "", nil)
	if c.Caller != r {
		t.Error("New did not keep the connection it was given")
	}
}
`

// callTestHelpersFile is the client_test.go of a call service.
func (g *gen) callTestHelpersFile() string {
	helpers := callRecorderHelpers
	if g.opts.Grouped {
		// a tag's client is made on the connection, not by New
		helpers = strings.Replace(helpers, "(*Client, *callRecorder)", "(Client, *callRecorder)", 1)
		helpers = strings.Replace(helpers, "\tc, err := New(r)\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n", "\tc := NewClient(r)\n", 1)

		return g.fileHeader(helpers)
	}

	return g.fileHeader(helpers + callNewTest)
}

// callTestFile is the test of one call: the method names the right server method, sends the arguments in order with those
// left off the end trimmed, decodes the answer, and passes a refusal on with what the server said.
func (g *gen) callTestFile(o *definitions.Operation) string {
	var b strings.Builder
	args, zeros := make([]string, 0, len(o.Arguments)), make([]string, 0, len(o.Arguments))
	for _, a := range o.Arguments {
		args = append(args, g.sampleArgument(a))
		zeros = append(zeros, g.zeroArgument(a))
	}
	required := requiredArguments(o)
	// a sample record is sent empty, and an empty record is not set, so it is trimmed when nothing set follows it
	sent := len(args)
	for sent > required && g.isZeroSample(o.Arguments[sent-1]) {
		sent--
	}
	// what the Caller is handed: the arguments as given, the options flagged for a query's form
	want := slices.Clone(args)
	if o.Call.Form != "" {
		options := slices.IndexFunc(o.Arguments, func(a definitions.Argument) bool { return a.Name == "options" })
		want[options] = fmt.Sprintf("client.Flagged{Record: %s, Flag: %q}", args[options], o.Call.Form)
	}
	expr := func(with []string) string {
		return "c." + callMethodName(g.grp, o) + "(" + strings.Join(append([]string{"t.Context()"}, with...), ", ") + ")"
	}
	call := testCallInfo{expr: expr(args)}
	_, body := g.sampleResponse(o)

	fmt.Fprintf(&b, "func TestOperation%s(t *testing.T) {\n\tt.Parallel()\n\n", o.Name)
	fmt.Fprintf(&b, "\tc, r := newCallRecorder(t, %q, nil)\n", body)
	if typ, _ := g.responseModel(o); typ == "" {
		// a method that answers nothing has nothing to read off the result
		fmt.Fprintf(&b, "\t_, err := %s\n", call.expr)
	} else {
		fmt.Fprintf(&b, "\tresult, err := %s\n", call.expr)
	}
	b.WriteString("\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n")
	fmt.Fprintf(&b, "\tif r.method != %q {\n\t\tt.Errorf(\"method = %%q, want %%q\", r.method, %q)\n\t}\n", o.Call.Method, o.Call.Method)
	fmt.Fprintf(&b, "\tif want := %s; !reflect.DeepEqual(r.params, want) {\n\t\tt.Errorf(\"params = %%#v, want %%#v\", r.params, want)\n\t}\n", anyList(want[:sent]))
	b.WriteString(g.responseChecks(o, call))

	if o.Response != nil {
		b.WriteString("\n\t// an answer that does not decode is an error\n")
		b.WriteString("\tc, _ = newCallRecorder(t, \"<html>\", nil)\n")
		fmt.Fprintf(&b, "\tif _, err = %s; err == nil {\n\t\tt.Error(\"an answer that does not decode succeeded\")\n\t}\n", call.expr)
	}

	b.WriteString("\n\t// a call the server refuses is an error carrying what it said\n")
	fmt.Fprintf(&b, "\tc, _ = newCallRecorder(t, \"\", &client.CallError{Method: %q, Code: -32001, Errno: 2, ErrName: \"ENOENT\", Reason: \"not there\"})\n", o.Call.Method)
	if typ, _ := g.responseModel(o); typ == "" {
		fmt.Fprintf(&b, "\tif _, err = %s; !client.IsNotFound(err) || client.CallErrorOf(err) == nil {\n\t\tt.Errorf(\"a refused call = %%v\", err)\n\t}\n", call.expr)
	} else {
		fmt.Fprintf(&b, "\tresult, err = %s\n", call.expr)
		b.WriteString("\tif !client.IsNotFound(err) || client.CallErrorOf(err) == nil || result.Answered() {\n\t\tt.Errorf(\"a refused call = %v, answered %t\", err, result.Answered())\n\t}\n")
	}

	if sent > required || required < len(args) {
		// an argument the server does not insist on is left off the end when it is not set
		with := slices.Concat(args[:required], zeros[required:])
		b.WriteString("\n\t// an argument the server does not insist on is left off the end when it is not set\n")
		fmt.Fprintf(&b, "\tc, r = newCallRecorder(t, %q, nil)\n", body)
		fmt.Fprintf(&b, "\tif _, err = %s; err != nil {\n\t\tt.Fatal(err)\n\t}\n", expr(with))
		fmt.Fprintf(&b, "\tif want := %s; !reflect.DeepEqual(r.params, want) {\n\t\tt.Errorf(\"params = %%#v, want %%#v\", r.params, want)\n\t}\n", anyList(want[:required]))
	}
	if o.Call.Uploadable {
		b.WriteString("\n\t// files sent beside the call go through the connection's Upload, with the call\n")
		fmt.Fprintf(&b, "\tc, r = newCallRecorder(t, %q, nil)\n", body)
		fmt.Fprintf(&b, "\tif _, err = %s; err != nil || r.uploaded != \"file bytes\" || r.method != %q {\n\t\tt.Errorf(\"with a file = %%v, uploaded %%q to %%q\", err, r.uploaded, r.method)\n\t}\n", expr(append(slices.Clone(args), `strings.NewReader("file bytes")`)), o.Call.Method)
	}
	if o.Call.Job && !o.Call.Uploadable && o.Call.Form == "" {
		b.WriteString("\n\t// the call itself, for a connection that starts the job and follows it\n")
		fmt.Fprintf(&b, "\tif method, params := %sCall(%s); method != %q || !reflect.DeepEqual(params, %s) {\n\t\tt.Errorf(\"Call = %%q, %%#v\", method, params)\n\t}\n", callMethodName(g.grp, o), strings.Join(args, ", "), o.Call.Method, anyList(want[:sent]))
	}
	if o.Call.Downloadable {
		b.WriteString("\n\t// the file it answers is written out through the connection's Download\n")
		b.WriteString("\tc, r = newCallRecorder(t, \"\", nil)\n\tvar file bytes.Buffer\n")
		fmt.Fprintf(&b, "\tif n, err := c.%sTo(%s); err != nil || n != 10 || file.String() != \"file bytes\" || r.method != %q {\n\t\tt.Errorf(\"To = %%d, %%v, %%q from %%q\", n, err, file.String(), r.method)\n\t}\n", callMethodName(g.grp, o), strings.Join(append([]string{"t.Context()", "&file"}, args...), ", "), o.Call.Method)
	}
	b.WriteString("}\n")

	return g.fileHeader(b.String())
}

// anyList is a []any literal of expressions, nil for none, as CallParams answers it.
func anyList(exprs []string) string {
	if len(exprs) == 0 {
		return "[]any(nil)"
	}

	return "[]any{" + strings.Join(exprs, ", ") + "}"
}

// sampleArgument is a value of an argument's type for a test to send.
func (g *gen) sampleArgument(a definitions.Argument) string {
	return g.sampleOf(a.Type)
}

// sampleElement is a value of a type inside a list or a map literal, where a record, a list or a map of its own goes without its type,
// as gofmt -s would have it.
func (g *gen) sampleElement(t definitions.TypeRef) string {
	sample := g.sampleOf(t)
	if typ := g.goType(t); strings.HasPrefix(sample, typ+"{") {
		return strings.TrimPrefix(sample, typ)
	} else if strings.HasPrefix(typ, "*") && strings.HasPrefix(sample, "&"+typ[1:]+"{") {
		return strings.TrimPrefix(sample, "&"+typ[1:])
	}

	return sample
}

// sampleOf is a value of a type: a record empty, a list of one, a map of one, and a primitive the test's usual sample.
func (g *gen) sampleOf(t definitions.TypeRef) string {
	switch t.Type {
	case definitions.List:
		return g.goType(t) + "{" + g.sampleElement(*t.NestedItem) + "}"
	case definitions.Dictionary:
		return g.goType(t) + `{"k": ` + g.sampleElement(*t.NestedItem) + "}"
	case definitions.Reference:
		if c := g.constants[t.ReferenceName]; c != nil && len(c.Values) > 0 {
			return g.typeName(c.Values[0].Name)
		}
		if m := g.models[t.ReferenceName]; m != nil && len(m.Union) > 0 && len(m.Members) == 0 {
			return g.typeName(t.ReferenceName) + `("{}")`
		}

		return g.typeName(t.ReferenceName) + "{}"
	case definitions.RawObject:
		return `json.RawMessage("{}")`
	case definitions.Any:
		return `"s"`
	case definitions.ID:
		expr, _ := g.sampleID(t.ReferenceName, "")

		return expr
	case definitions.Named:
		// the zero of a type the generator knows nothing of
		return "*new(" + t.GoType + ")"
	default:
		expr, _, ok := g.sampleOption(definitions.Option{Type: t, Field: "Arg"})
		if !ok {
			return "nil"
		}
		// an option's sample is what its pointer points at
		return strings.TrimSuffix(strings.TrimPrefix(expr, "new("), ")")
	}
}

// zeroArgument is the zero of an argument's type, for a test to leave it off the end with.
func (g *gen) zeroArgument(a definitions.Argument) string {
	switch a.Type.Type {
	case definitions.List, definitions.Dictionary, definitions.RawObject, definitions.Any:
		return "nil"
	case definitions.Named:
		return "*new(" + a.Type.GoType + ")"
	case definitions.String:
		return `""`
	case definitions.Boolean:
		return "false"
	case definitions.Integer, definitions.Integer64, definitions.Float, definitions.Double:
		return "0"
	case definitions.ID:
		if g.idBase(a.Type.ReferenceName) == definitions.String {
			return `""`
		}

		return "0"
	case definitions.Reference:
		if m := g.models[a.Type.ReferenceName]; m != nil && (len(m.Union) == 0 || len(m.Members) > 0) {
			return g.goType(a.Type) + "{}"
		}
		// an enum or a raw union is a string
		return `""`
	default:
		return g.goType(a.Type) + "{}"
	}
}

// isZeroSample reports whether an argument's sample is its type's zero, which CallParams trims off the end: an empty record is.
func (g *gen) isZeroSample(a definitions.Argument) bool {
	if a.Type.Type != definitions.Reference {
		return false
	}
	m := g.models[a.Type.ReferenceName]

	return m != nil && (len(m.Union) == 0 || len(m.Members) > 0)
}
