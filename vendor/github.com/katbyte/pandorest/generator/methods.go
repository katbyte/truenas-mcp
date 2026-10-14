package generator

import (
	"fmt"
	"go/token"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/katbyte/pandorest/definitions"
)

// methodFile renders one operation: its response struct, its options struct
// when it has options, the method, and the Complete pager for a list.
func (g *gen) methodFile(o *definitions.Operation) string {
	responseType := o.Name + "Response"
	optionsType := o.Name + "Options"
	g.declare(responseType, "operation "+o.Name)

	var b strings.Builder
	g.writeResponse(&b, o, responseType)
	if o.Call != nil {
		g.writeCallMethod(&b, o, responseType)

		return g.fileHeader(b.String())
	}
	if len(o.Options) > 0 {
		g.declare(optionsType, "operation "+o.Name)
		g.writeOptions(&b, o, optionsType)
	}
	g.writeMethod(&b, o, responseType, optionsType)
	if o.Pageable != nil {
		g.writeCompleteResult(&b, o)
		g.writeCompleteMethod(&b, o, optionsType)
		g.writeEachMethod(&b, o, optionsType)
	}
	g.writeCommandMethods(&b, o)

	return g.fileHeader(b.String())
}

// operationFile renders, in the types package of the grouped layout, what a
// caller names of one operation: its response struct, its options struct and
// the result type of its pager.
func (g *gen) operationFile(o *definitions.Operation) string {
	responseType := o.Name + "Response"
	optionsType := o.Name + "Options"
	g.declare(responseType, "operation "+o.Name)

	var b strings.Builder
	g.writeResponse(&b, o, responseType)
	if len(o.Options) > 0 {
		g.declare(optionsType, "operation "+o.Name)
		g.writeOptions(&b, o, optionsType)
	}
	if o.Pageable != nil {
		g.writeCompleteResult(&b, o)
	}

	return g.fileHeader(b.String())
}

// groupMethodFile renders, in a tag's package of the grouped layout, one
// operation's method and, for a list, its pager, naming the types as the
// types package's.
func (g *gen) groupMethodFile(o *definitions.Operation) string {
	var b strings.Builder
	if o.Call != nil {
		g.writeCallMethod(&b, o, g.typeName(o.Name+"Response"))

		return g.fileHeader(b.String())
	}
	g.writeMethod(&b, o, g.typeName(o.Name+"Response"), g.typeName(o.Name+"Options"))
	if o.Pageable != nil {
		g.writeCompleteMethod(&b, o, g.typeName(o.Name+"Options"))
		g.writeEachMethod(&b, o, g.typeName(o.Name+"Options"))
	}
	g.writeCommandMethods(&b, o)

	return g.fileHeader(b.String())
}

// embedded is the struct model a response embeds beside Answer, so that a
// caller may read res.Name where it would read res.Answer.Name; "" for an
// answer that is not a struct model (a list, a string, raw JSON, a union).
func (g *gen) embedded(o *definitions.Operation) string {
	if o.Response == nil || o.Response.Type.Type != definitions.Reference || !g.isStruct(o.Response.Type.ReferenceName) {
		return ""
	}

	return g.typeName(o.Response.Type.ReferenceName)
}

// responseModel is how the response struct holds the model: pointers for a
// struct, a constant or a primitive, the value for slices, maps and raw JSON.
func (g *gen) responseModel(o *definitions.Operation) (typ string, pointer bool) {
	if o.Response == nil || o.Response.Type.Type == definitions.RawFile {
		return "", false
	}
	t := o.Response.Type
	switch t.Type {
	case definitions.List, definitions.Dictionary, definitions.RawObject, definitions.Any:
		return g.goType(t), false
	case definitions.Reference:
		if m := g.models[t.ReferenceName]; m != nil && len(m.Union) > 0 && len(m.Members) == 0 {
			return t.ReferenceName, false
		}
	default:
	}

	return "*" + g.goType(t), true
}

func (g *gen) writeResponse(b *strings.Builder, o *definitions.Operation, responseType string) {
	fmt.Fprintf(b, "// %s is the result of %s.\n", responseType, o.Name)
	file := o.Response != nil && o.Response.Type.Type == definitions.RawFile
	stream := file && o.Response.ContentType == eventStream
	switch {
	case stream:
		fmt.Fprintf(b, "//\n// The operation answers %s, left open in HttpResponse.Body: Reader hands it\n// over for the caller to read event by event and close; it has no end to copy to.\n", o.Response.ContentType)
	case file:
		fmt.Fprintf(b, "//\n// The operation answers %s, left unread in HttpResponse.Body. WriteTo copies\n// it out and Bytes reads it whole up to a size, each closing it; Reader hands it over\n// for the caller to read and close. After any of them the body is spent. A failed call\n// has no open body: the base client reads a refusal and closes it.\n", o.Response.ContentType)
	}
	if o.Response.Text() {
		fmt.Fprintf(b, "//\n// The operation answers text (%s), which Answer holds as it was sent.\n", o.Response.ContentType)
	}
	embedded := g.embedded(o)
	if embedded != "" {
		fmt.Fprintf(b, "//\n// Answer is the %s answered, nil when the server answered none (a 204), which\n// Answered tells. The same record is embedded, so res.Answer.Name may be read as\n// res.Name; when nothing was answered the embedded record is a zero one, so that\n// read is empty rather than a panic. Only the method sets the two; a response built\n// by hand sets both or neither.\n", embedded)
		g.checkEmbedded(o)
	}
	typ, _ := g.responseModel(o)
	fmt.Fprintf(b, "type %s struct {\n", responseType)
	if o.Call == nil {
		// a call has no response beyond its answer
		b.WriteString("\tHttpResponse *http.Response\n")
	}
	if typ != "" {
		fmt.Fprintf(b, "\tAnswer %s\n", typ)
	}
	if embedded != "" {
		fmt.Fprintf(b, "\t*%s\n", embedded)
	}
	b.WriteString("}\n\n")
	switch {
	case typ != "" && o.Call != nil:
		fmt.Fprintf(b, "// Answered reports whether the server answered and the answer was decoded:\n// false for a call that failed.\nfunc (r %s) Answered() bool { return r.Answer != nil }\n\n", responseType)
	case typ != "":
		fmt.Fprintf(b, "// Answered reports whether the server answered with a body that was decoded:\n// false for a 204, or a call that failed before one came.\nfunc (r %s) Answered() bool { return r.Answer != nil }\n\n", responseType)
	}
	if embedded != "" {
		fmt.Fprintf(b, "// Record is the %[2]s answered, as a value to change and write back, or an\n// error when the server answered none.\nfunc (r %[1]s) Record() (%[2]s, error) {\n\tif r.Answer == nil {\n\t\treturn %[2]s{}, errors.New(\"%[3]s answered no %[2]s\")\n\t}\n\n\treturn *r.Answer, nil\n}\n\n", responseType, embedded, o.Name)
	}
	if file {
		fmt.Fprintf(b, readerMethods, responseType)
	}
	if file && !stream {
		fmt.Fprintf(b, copyMethods, responseType)
	}
}

// eventStream is the content type of an operation that answers events as
// they happen, which has no end to copy to.
const eventStream = "text/event-stream"

// readerMethods are the methods of every response that is a file: the body
// handed over with its content type, and its length.
const readerMethods = `// Reader is the file's body, for the caller to read and close, and the content
// type the server named it; an empty body and "" when the operation gave no
// answer, so a deferred Close is always safe. The body stays readable after
// the result goes out of scope, and has no size or time limit of the client's:
// the caller's context is the one bound on a slow read.
func (r %[1]s) Reader() (io.ReadCloser, string) {
	if r.HttpResponse == nil || r.HttpResponse.Body == nil {
		return http.NoBody, ""
	}

	return r.HttpResponse.Body, r.HttpResponse.Header.Get("Content-Type")
}

// ContentLength is the file's length as the server named it, or -1 when it
// did not, to refuse one too big before reading any of it.
func (r %[1]s) ContentLength() int64 {
	if r.HttpResponse == nil {
		return -1
	}

	return r.HttpResponse.ContentLength
}

// Filename is the name the server gave the file (Content-Disposition), or ""
// when it gave none, for a caller saving it under the server's name.
func (r %[1]s) Filename() string {
	if r.HttpResponse == nil {
		return ""
	}

	return client.Filename(r.HttpResponse.Header)
}

`

// copyMethods are the methods of a response that is a file with an end: a
// copy out and a bounded read, each closing the body.
const copyMethods = `// WriteTo copies the file to w and closes the body whatever happens, which is
// the one way to take the answer that cannot leave it open. A writer that
// failed is a client.WriteError, as against the server's side breaking off;
// it fails when the operation gave no answer, and again once the body is spent.
func (r %[1]s) WriteTo(w io.Writer) (int64, error) {
	if r.HttpResponse == nil || r.HttpResponse.Body == nil {
		return 0, errors.New("no answer to write")
	}

	return client.Copy(w, r.HttpResponse.Body)
}

// Bytes reads the file whole, up to max bytes, and closes the body: a file
// over max is a client.TooLargeError rather than cut short without a word.
func (r %[1]s) Bytes(max int64) ([]byte, error) {
	if r.HttpResponse == nil || r.HttpResponse.Body == nil {
		return nil, errors.New("no answer to read")
	}

	return client.ReadAtMost(r.HttpResponse.Body, max)
}

`

// embeddedField is the name an embedded type is reached by: its own, without
// the package a tag's package qualifies it with.
func embeddedField(typ string) string {
	if _, name, ok := strings.Cut(typ, "."); ok {
		return name
	}

	return typ
}

// checkEmbedded refuses a struct answer whose own field would hide the
// response's, or whose name the response already uses, since Go would pick
// the response's silently.
func (g *gen) checkEmbedded(o *definitions.Operation) {
	name := o.Response.Type.ReferenceName
	if name == "Answer" || name == "HttpResponse" {
		g.clashes = append(g.clashes, fmt.Sprintf("the model %s is answered by %s, whose response has a field by that name: rename it with a workaround", name, o.Name))
	}
	if m := g.models[name]; m != nil {
		for i := range m.Fields {
			if f := m.Fields[i].Name; f == "Answer" || f == "HttpResponse" || f == "Answered" || f == "Record" {
				g.clashes = append(g.clashes, fmt.Sprintf("%s.%s would be hidden by the field of the same name on %s's response, which embeds the model: rename it with a workaround", name, f, o.Name))
			}
		}
	}
}

// optionType is how the options struct holds an option: a pointer for a
// boolean or a number, so false and 0 can be sent (a series' specials are
// season 0, a backdrop is image 0), the plain type for the rest, whose zero
// value is not sent.
func (g *gen) optionType(t definitions.TypeRef) string {
	switch t.Type {
	case definitions.Boolean, definitions.Integer, definitions.Integer64, definitions.Float, definitions.Double:
		return "*" + g.goType(t)
	default:
		return g.goType(t)
	}
}

func (g *gen) writeOptions(b *strings.Builder, o *definitions.Operation, optionsType string) {
	fmt.Fprintf(b, "// %s holds the query and header parameters of %s.\n", optionsType, o.Name)
	fmt.Fprintf(b, "type %s struct {\n", optionsType)
	for i, opt := range o.Options {
		doc := opt.Description
		if opt.Deprecated {
			doc = strings.TrimSpace(doc + "\n\nDeprecated: the document marks this parameter deprecated.")
		}
		if i > 0 && doc != "" {
			b.WriteString("\n")
		}
		b.WriteString(comment("\t", doc))
		fmt.Fprintf(b, "\t%s %s\n", opt.Field, g.optionType(opt.Type))
	}
	b.WriteString("}\n\n")

	for _, in := range []struct{ where, method, typ string }{
		{definitions.InHeader, "ToHeaders", "Headers"},
		{definitions.InQuery, "ToQuery", "QueryParams"},
	} {
		fmt.Fprintf(b, "// %s returns the %s the options set.\n", in.method, strings.ToLower(in.where)+" parameters")
		fmt.Fprintf(b, "func (o %s) %s() *client.%s {\n\tout := client.%s{}\n", optionsType, in.method, in.typ, in.typ)
		for _, opt := range o.Options {
			if opt.In == in.where {
				b.WriteString(g.appendOption(o, opt))
			}
		}
		b.WriteString("\treturn &out\n}\n\n")
	}
}

// appendOption renders the statement that adds one option when it is set. A
// list goes comma-separated or as one parameter per element, as the
// document says; a date goes in the layout the service's own file picks for
// the parameter, when it has one to ask.
func (g *gen) appendOption(o *definitions.Operation, opt definitions.Option) string {
	field := "o." + opt.Field
	var cond, value string
	switch t := opt.Type; t.Type {
	case definitions.Boolean:
		cond, value = field+" != nil", "strconv.FormatBool(*"+field+")"
	case definitions.Integer:
		cond, value = field+" != nil", "strconv.Itoa(*"+field+")"
	case definitions.Integer64:
		cond, value = field+" != nil", "strconv.FormatInt(*"+field+", 10)"
	case definitions.Float:
		cond, value = field+" != nil", "strconv.FormatFloat(float64(*"+field+"), 'f', -1, 32)"
	case definitions.Double:
		cond, value = field+" != nil", "strconv.FormatFloat(*"+field+", 'f', -1, 64)"
	case definitions.List:
		if !opt.CommaSeparated {
			item := "v"
			switch t.NestedItem.Type {
			case definitions.Integer:
				item = "strconv.Itoa(v)"
			case definitions.Integer64:
				item = "strconv.FormatInt(v, 10)"
			case definitions.Reference:
				item = "string(v)"
			case definitions.ID:
				item = g.idText(t.NestedItem.ReferenceName, "v")
			default:
			}
			return fmt.Sprintf("\tfor _, v := range %s {\n\t\tout.Append(%q, %s)\n\t}\n", field, opt.Name, item)
		}
		cond, value = "len("+field+") > 0", "client.CSV("+field+")"
		if t.NestedItem.Type == definitions.ID || t.NestedItem.Type == definitions.Reference {
			// a list of a named type, an id or an enum: joined by its text
			value = "client.Joined(" + field + ")"
		}
	case definitions.Dictionary:
		if opt.DeepObject {
			return fmt.Sprintf("\tfor _, kv := range client.DeepObject(%q, %s) {\n\t\tout.Append(kv[0], kv[1])\n\t}\n", opt.Name, field)
		}
		cond, value = "len("+field+") > 0", "client.JSONObject("+field+")"
	case definitions.Reference:
		cond, value = field+` != ""`, "string("+field+")"
	case definitions.DateTime, definitions.Date:
		cond, value = "!"+field+".IsZero()", field+g.dateAs(o, opt.Name)
	case definitions.UnixSeconds, definitions.UnixMillis:
		cond, value = "!"+field+".IsZero()", field+".String()"
	case definitions.ID:
		cond, value = g.idCondition(t.ReferenceName, field), g.idText(t.ReferenceName, field)
	default:
		cond, value = field+` != ""`, field
	}

	return fmt.Sprintf("\tif %s {\n\t\tout.Append(%q, %s)\n\t}\n", cond, opt.Name, value)
}

// pathExpression renders the path with each placeholder filled from its
// escaped argument.
func (g *gen) pathExpression(o *definitions.Operation) string {
	if len(o.PathParameters) == 0 {
		return fmt.Sprintf("%q", o.Path)
	}
	byName := map[string]definitions.PathParameter{}
	for _, p := range o.PathParameters {
		byName[p.Name] = p
	}
	var format strings.Builder
	var args []string
	rest := o.Path
	for rest != "" {
		open := strings.IndexByte(rest, '{')
		closeIdx := strings.IndexByte(rest, '}')
		if open < 0 || closeIdx < open {
			format.WriteString(strings.ReplaceAll(rest, "%", "%%"))
			break
		}
		format.WriteString(strings.ReplaceAll(rest[:open], "%", "%%"))
		p := byName[rest[open+1:closeIdx]]
		switch p.Type.Type {
		case definitions.Integer, definitions.Integer64:
			format.WriteString("%d")
			args = append(args, p.Argument)
		case definitions.Double:
			format.WriteString("%s")
			args = append(args, "strconv.FormatFloat("+p.Argument+", 'f', -1, 64)")
		case definitions.Reference:
			format.WriteString("%s")
			args = append(args, "url.PathEscape(string("+p.Argument+"))")
		case definitions.DateTime, definitions.Date:
			format.WriteString("%s")
			args = append(args, "url.PathEscape("+p.Argument+g.dateAs(o, p.Name)+")")
		case definitions.UnixSeconds, definitions.UnixMillis:
			format.WriteString("%s")
			args = append(args, p.Argument+".String()")
		case definitions.ID:
			format.WriteString("%s")
			args = append(args, "url.PathEscape("+g.idText(p.Type.ReferenceName, p.Argument)+")")
		default:
			format.WriteString("%s")
			args = append(args, "url.PathEscape("+p.Argument+")")
		}
		rest = rest[closeIdx+1:]
	}

	return fmt.Sprintf("fmt.Sprintf(%q, %s)", format.String(), strings.Join(args, ", "))
}

// arguments renders the method's parameters after ctx, and the call that
// passes them on.
func (g *gen) arguments(o *definitions.Operation, optionsType string) (params, call []string) {
	for _, p := range o.PathParameters {
		params = append(params, p.Argument+" "+g.goType(p.Type))
		call = append(call, p.Argument)
	}
	if o.Request != nil {
		if o.Request.Type.Type == definitions.RawFile {
			params = append(params, "input io.Reader", "contentType string")
			call = append(call, "input", "contentType")
		} else {
			params = append(params, "input "+g.goType(o.Request.Type))
			call = append(call, "input")
		}
	}
	switch {
	case g.inline(o):
		opt := &o.Options[0]
		if opt.Required {
			params = append(params, g.inlineArgument(o)+" "+g.goType(opt.Type))
		} else {
			params = append(params, g.inlineArgument(o)+" ..."+g.inlineType(opt))
		}
		call = append(call, "option")
	case g.optional(o):
		params = append(params, "options ..."+optionsType)
		call = append(call, "option")
	case len(o.Options) > 0:
		params = append(params, "options "+optionsType)
		call = append(call, "options")
	}

	return params, call
}

// inline reports whether an operation's one option is taken as an argument in
// place of the struct (Options.InlineSingleOptions): GetMovieLookup(ctx, term). A
// list's pager sets the page options itself, so a list keeps its struct.
func (g *gen) inline(o *definitions.Operation) bool {
	return g.opts.InlineSingleOptions && len(o.Options) == 1 && o.Pageable == nil
}

// inlineType is the argument's type: the option's own, without the pointer
// the struct holds a boolean or a number by, since an argument left out is
// what the pointer's nil said; an optional list is taken as its elements,
// a required one as the slice.
func (g *gen) inlineType(opt *definitions.Option) string {
	if opt.Type.Type == definitions.List {
		return g.goType(*opt.Type.NestedItem)
	}

	return g.goType(opt.Type)
}

// localNames are the names a method body uses and the packages it imports,
// which an inlined option's argument keeps clear of.
var localNames = map[string]bool{"ctx": true, "input": true, "contentType": true, "opts": true, "req": true, "resp": true, "result": true, "err": true, "option": true, "options": true, "model": true, "c": true, "client": true, "context": true, "fmt": true, "http": true, "io": true, "url": true, "strconv": true, "strings": true, "json": true, "errors": true}

// inlineArgument names the argument of an inlined option: the field's name
// lower-cased at the front (Term: term), kept clear of the body's own names,
// the packages it uses, Go's keywords and the path parameters by a suffix
// (Type: typeOption, Url: urlOption).
func (g *gen) inlineArgument(o *definitions.Operation) string {
	field := o.Options[0].Field
	first, size := utf8.DecodeRuneInString(field)
	name := string(unicode.ToLower(first)) + field[size:]
	if localNames[name] || name == g.qual || name == g.svc.Package || token.IsKeyword(name) || slices.ContainsFunc(o.PathParameters, func(p definitions.PathParameter) bool { return p.Argument == name }) {
		name += "Option"
	}

	return name
}

// optional reports whether an operation's options may all be left out, so
// that a call can leave the struct out altogether: GetSessions(ctx).
func (g *gen) optional(o *definitions.Operation) bool {
	return len(o.Options) > 0 && !g.inline(o) && !slices.ContainsFunc(o.Options, func(p definitions.Option) bool { return p.Required })
}

// optionsVar is the name the body reads the options by: the one struct a
// variadic call was given, the struct built from an inlined argument, or the
// parameter itself.
func (g *gen) optionsVar(o *definitions.Operation) string {
	if g.optional(o) || g.inline(o) {
		return "option"
	}

	return "options"
}

// writeOptionsPrelude builds the options the body sends: the one struct a
// variadic call was given, or the struct an inlined argument fills, refusing
// more than one before any request is made.
func (g *gen) writeOptionsPrelude(b *strings.Builder, o *definitions.Operation, method, optionsType string) {
	g.writeOptionsPreludeFailing(b, o, method, optionsType, func(expr string) string { return "err = " + expr + "\n\t\treturn" })
}

// writeOptionsPreludeFailing is writeOptionsPrelude with fail saying how a
// refusal is returned: assigned to err in a method, yielded in a pager.
func (g *gen) writeOptionsPreludeFailing(b *strings.Builder, o *definitions.Operation, method, optionsType string, fail func(expr string) string) {
	switch {
	case g.inline(o):
		opt := &o.Options[0]
		arg := g.inlineArgument(o)
		fmt.Fprintf(b, "\tvar option %s\n", optionsType)
		switch {
		case opt.Required:
			fmt.Fprintf(b, "\toption.%s = %s\n\n", opt.Field, held(opt, arg))
		case opt.Type.Type == definitions.List:
			fmt.Fprintf(b, "\tif len(%s) > 0 {\n\t\toption.%s = %s\n\t}\n\n", arg, opt.Field, arg)
		default:
			fmt.Fprintf(b, "\tif len(%s) > 1 {\n\t\t%s\n\t}\n", arg, fail(fmt.Sprintf("fmt.Errorf(\"%s takes one %s at most, not %%d\", len(%s))", method, arg, arg)))
			fmt.Fprintf(b, "\tif len(%s) == 1 {\n\t\toption.%s = %s\n\t}\n\n", arg, opt.Field, held(opt, arg+"[0]"))
		}
	case g.optional(o):
		fmt.Fprintf(b, "\tif len(options) > 1 {\n\t\t%s\n\t}\n", fail(fmt.Sprintf("fmt.Errorf(\"%s takes one options struct at most, not %%d\", len(options))", method)))
		fmt.Fprintf(b, "\tvar option %s\n\tif len(options) == 1 {\n\t\toption = options[0]\n\t}\n\n", optionsType)
	}
}

// writeCommandMethods writes what a command set adds: on the operation that queues, WaitForCommand; on each command's own
// operation, the AndWait form that queues it and waits.
func (g *gen) writeCommandMethods(b *strings.Builder, o *definitions.Operation) {
	switch {
	case o.Commands != nil:
		g.writeWaitForCommand(b, o)
	case o.Command != "":
		g.writeAndWait(b, o)
	}
}

// writeWaitForCommand writes the poll of a queued command until the server says it finished.
func (g *gen) writeWaitForCommand(b *strings.Builder, o *definitions.Operation) {
	set := o.Commands
	get := g.operationNamed(set.Get)
	if get == nil || get.Response == nil || len(get.PathParameters) != 1 {
		return
	}
	g.declare("Client.WaitForCommand", "operation "+o.Name)
	answer := get.Response.Type.ReferenceName
	message := g.fieldFor(answer, "message")
	b.WriteString("\n// CommandPoll is how often WaitForCommand asks after a command.\nvar CommandPoll = 2 * time.Second\n\n")
	fmt.Fprintf(b, "// WaitForCommand asks after the command every CommandPoll until the server says it finished (%s), or ctx ends, or wait\n", strings.Join(set.Finished, ", "))
	b.WriteString("// runs out, and answers it as it stands: a command that failed is an error carrying what the server said, wrapping\n// client.ErrCommandFailed, and one still running when wait ran out wraps client.ErrCommandRunning, for a caller that takes\n// that as an answer. For a command queued by its own method, <Name>AndWait does both at once.\n")
	fmt.Fprintf(b, "func (c Client) WaitForCommand(ctx context.Context, id %s, wait time.Duration) (result %s, err error) {\n", g.goType(get.PathParameters[0].Type), g.typeName(get.Name+"Response"))
	b.WriteString("\tdeadline := time.Now().Add(wait)\n\tfor {\n")
	fmt.Fprintf(b, "\t\tresult, err = c.%s(ctx, id)\n\t\tif err != nil || result.Answer == nil {\n\t\t\treturn\n\t\t}\n", get.Name)
	fmt.Fprintf(b, "\t\tstatus := string(result.Answer.%s)\n", g.fieldFor(answer, set.Status))
	if len(set.Failed) > 0 {
		// what the server said of it: its message, and its exception where the answer has one, since a server may put the useful part there
		said := `""`
		if message != "" {
			said = "string(result.Answer." + message + ")"
		}
		if exception := g.fieldFor(answer, "exception"); exception != "" {
			said = "strings.TrimSpace(strings.Join([]string{" + said + ", string(result.Answer." + exception + ")}, \" \"))"
		}
		format, args := "the command %v %s: %s: %w", "id, status, "+said
		if name := g.fieldFor(answer, "name"); name != "" {
			format, args = "the command %v %s %s: %s: %w", "id, string(result.Answer."+name+"), status, "+said
		}
		fmt.Fprintf(b, "\t\tif slices.Contains(%s, status) {\n\t\t\terr = fmt.Errorf(%q, %s, client.ErrCommandFailed)\n\t\t\treturn\n\t\t}\n", goStrings(set.Failed), format, args)
	}
	fmt.Fprintf(b, "\t\tif slices.Contains(%s, status) {\n\t\t\treturn\n\t\t}\n", goStrings(set.Finished))
	b.WriteString("\t\tif time.Now().After(deadline) {\n\t\t\terr = fmt.Errorf(\"the command %v is still %s after %s: %w\", id, status, wait, client.ErrCommandRunning)\n\t\t\treturn\n\t\t}\n")
	b.WriteString("\t\tselect {\n\t\tcase <-ctx.Done():\n\t\t\terr = ctx.Err()\n\t\t\treturn\n\t\tcase <-time.After(CommandPoll):\n\t\t}\n\t}\n}\n")
}

// writeAndWait writes a command's AndWait: queued by its own method, then waited for.
func (g *gen) writeAndWait(b *strings.Builder, o *definitions.Operation) {
	queue := g.queueing(o)
	if queue == nil {
		return
	}
	get := g.operationNamed(queue.Commands.Get)
	if get == nil || len(get.PathParameters) != 1 || o.Response == nil {
		return
	}
	method := o.Name + "AndWait"
	g.declare("Client."+method, "operation "+o.Name)
	fmt.Fprintf(b, "\n// %s queues the %s command and waits for it as WaitForCommand does, answering it as it stands.\n", method, o.Command)
	fmt.Fprintf(b, "func (c Client) %s(ctx context.Context, input %s, wait time.Duration) (result %s, err error) {\n", method, g.goType(o.Request.Type), g.typeName(get.Name+"Response"))
	fmt.Fprintf(b, "\tqueued, err := c.%s(ctx, input)\n\tif err != nil {\n\t\treturn\n\t}\n", o.Name)
	fmt.Fprintf(b, "\tif queued.Answer == nil {\n\t\terr = errors.New(\"%s answered no command to wait for\")\n\t\treturn\n\t}\n\n", o.Name)
	fmt.Fprintf(b, "\treturn c.WaitForCommand(ctx, %s(queued.Answer.%s), wait)\n}\n", g.goType(get.PathParameters[0].Type), g.fieldFor(o.Response.Type.ReferenceName, "id"))
}

// queueing is the operation that queues a command's operation: the same method and path, holding the command set.
func (g *gen) queueing(o *definitions.Operation) *definitions.Operation {
	for _, q := range g.svc.Operations() {
		if q.Commands != nil && q.Method == o.Method && q.Path == o.Path {
			return q
		}
	}

	return nil
}

// operationNamed is the operation with the Go name, or nil.
func (g *gen) operationNamed(name string) *definitions.Operation {
	for _, o := range g.svc.Operations() {
		if o.Name == name {
			return o
		}
	}

	return nil
}

// fieldFor is the Go field a model holds a JSON name in, or "" for none.
func (g *gen) fieldFor(model, jsonName string) string {
	m := g.models[model]
	if m == nil {
		return ""
	}
	for i := range m.Fields {
		if strings.EqualFold(m.Fields[i].JSONName, jsonName) {
			return m.Fields[i].Name
		}
	}

	return ""
}

// goStrings is a []string literal.
func goStrings(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, strconv.Quote(v))
	}

	return "[]string{" + strings.Join(quoted, ", ") + "}"
}

// writeShapeChecks refuses a path parameter or an option that is an id of a kind with a shape and has not got it, before any request.
func (g *gen) writeShapeChecks(b *strings.Builder, o *definitions.Operation) {
	wrote := false
	for i := range o.PathParameters {
		p := &o.PathParameters[i]
		if p.Type.Type == definitions.ID && g.idShape(p.Type.ReferenceName) != "" {
			fmt.Fprintf(b, "\tif err = %s.Check(); err != nil {\n\t\treturn\n\t}\n", p.Argument)
			wrote = true
		}
	}
	ov := g.optionsVar(o)
	for i := range o.Options {
		opt := &o.Options[i]
		field := ov + "." + opt.Field
		switch {
		case opt.Type.Type == definitions.ID && g.idShape(opt.Type.ReferenceName) != "":
			fmt.Fprintf(b, "\tif %s {\n\t\tif err = %s.Check(); err != nil {\n\t\t\treturn\n\t\t}\n\t}\n", g.idCondition(opt.Type.ReferenceName, field), field)
			wrote = true
		case opt.Type.Type == definitions.List && opt.Type.NestedItem != nil && opt.Type.NestedItem.Type == definitions.ID && g.idShape(opt.Type.NestedItem.ReferenceName) != "":
			fmt.Fprintf(b, "\tfor _, id := range %s {\n\t\tif err = id.Check(); err != nil {\n\t\t\treturn\n\t\t}\n\t}\n", field)
			wrote = true
		}
	}
	if wrote {
		b.WriteString("\n")
	}
}

// held is the expression the options struct holds an argument's value by: its
// address for a boolean or a number, the value for the rest (see optionType).
func held(opt *definitions.Option, expr string) string {
	switch opt.Type.Type {
	case definitions.Boolean, definitions.Integer, definitions.Integer64, definitions.Float, definitions.Double:
		return "&" + expr
	default:
		return expr
	}
}

var httpMethods = map[string]string{
	"GET": "http.MethodGet", "POST": "http.MethodPost", "PUT": "http.MethodPut", "DELETE": "http.MethodDelete", "PATCH": "http.MethodPatch",
}

var statusNames = map[int]string{
	http.StatusOK: "http.StatusOK", http.StatusCreated: "http.StatusCreated", http.StatusAccepted: "http.StatusAccepted",
	http.StatusNoContent: "http.StatusNoContent", http.StatusPartialContent: "http.StatusPartialContent",
	http.StatusBadRequest:       "http.StatusBadRequest",
	http.StatusMovedPermanently: "http.StatusMovedPermanently", http.StatusFound: "http.StatusFound", http.StatusSeeOther: "http.StatusSeeOther",
	http.StatusNotModified: "http.StatusNotModified", http.StatusTemporaryRedirect: "http.StatusTemporaryRedirect", http.StatusPermanentRedirect: "http.StatusPermanentRedirect",
}

func (g *gen) writeMethod(b *strings.Builder, o *definitions.Operation, responseType, optionsType string) {
	g.declare("Client."+o.Name, "operation "+o.Name)
	params, _ := g.arguments(o, optionsType)

	fmt.Fprintf(b, "// %s calls %s %s.", o.Name, o.Method, o.Path)
	if s := firstSentence(o.Description); s != "" {
		b.WriteString(" " + s)
	}
	b.WriteString("\n")
	if o.Request != nil && o.Request.Type.Type == definitions.RawFile {
		if strings.Contains(o.Request.ContentType, "*") {
			fmt.Fprintf(b, "//\n// input is sent as contentType, which must name a concrete type: the\n// operation takes %s.\n", o.Request.ContentType)
		} else {
			fmt.Fprintf(b, "//\n// input is sent as contentType, or as %s when that is empty.\n", o.Request.ContentType)
		}
	}
	if o.Deprecated {
		b.WriteString("//\n// Deprecated: the document marks this operation deprecated.\n")
	}
	fmt.Fprintf(b, "func (c Client) %s(%s) (result %s, err error) {\n",
		o.Name, strings.Join(append([]string{"ctx context.Context"}, params...), ", "), responseType)
	g.writeOptionsPrelude(b, o, o.Name, optionsType)
	g.writeShapeChecks(b, o)
	if o.Command != "" {
		// the command's name is the method's to set, so a caller cannot send another command through it
		fmt.Fprintf(b, "\tinput.%s = %q\n\n", g.fieldFor(o.Request.Type.ReferenceName, "name"), o.Command)
	}
	if embedded := g.embedded(o); embedded != "" {
		// a zero record until one is decoded, so a promoted field reads empty rather than panicking
		fmt.Fprintf(b, "\tresult.%s = &%s{}\n", embeddedField(embedded), embedded)
	}

	b.WriteString("\topts := client.RequestOptions{\n")
	if asksFor := o.Response.AsksFor(); asksFor != "" {
		// a server may choose its answer by what it is asked for, so a call asks for what its document says it answers
		fmt.Fprintf(b, "\t\tAccept: %q,\n", asksFor)
	}
	if o.Request != nil {
		fmt.Fprintf(b, "\t\tContentType: %q,\n", o.Request.ContentType)
	}
	b.WriteString("\t\tExpectedStatusCodes: []int{\n")
	for _, code := range o.ExpectedStatusCodes {
		name, ok := statusNames[code]
		if !ok {
			name = strconv.Itoa(code)
		}
		fmt.Fprintf(b, "\t\t\t%s,\n", name)
	}
	b.WriteString("\t\t},\n")
	fmt.Fprintf(b, "\t\tHTTPMethod: %s,\n", httpMethods[o.Method])
	fmt.Fprintf(b, "\t\tOperation: %q,\n", o.Name)
	if len(o.Options) > 0 {
		fmt.Fprintf(b, "\t\tOptionsObject: %s,\n", g.optionsVar(o))
	}
	fmt.Fprintf(b, "\t\tPath: %s,\n", g.pathExpression(o))
	if o.Response != nil && o.Response.Type.Type == definitions.RawFile {
		b.WriteString("\t\tStreamResponse: true,\n")
	}
	b.WriteString("\t}\n\n")

	b.WriteString("\treq, err := c.Client.NewRequest(ctx, opts)\n\tif err != nil {\n\t\treturn\n\t}\n\n")
	if o.Request != nil {
		if o.Request.Type.Type == definitions.RawFile {
			b.WriteString("\tif err = req.SetBody(input, contentType); err != nil {\n\t\treturn\n\t}\n\n")
		} else {
			b.WriteString("\tif err = req.Marshal(input); err != nil {\n\t\treturn\n\t}\n\n")
		}
	}

	b.WriteString("\tvar resp *client.Response\n\tresp, err = req.Execute(ctx)\n")
	b.WriteString("\tif resp != nil {\n\t\tresult.HttpResponse = resp.Response\n\t}\n")
	b.WriteString("\tif err != nil {\n\t\treturn\n\t}\n\n")

	if typ, pointer := g.responseModel(o); typ != "" {
		if slices.Contains(o.ExpectedStatusCodes, http.StatusNoContent) {
			// a 204 is a null result: no model
			b.WriteString("\tif resp.StatusCode == http.StatusNoContent {\n\t\treturn\n\t}\n\n")
		}
		switch {
		case o.Response.Text():
			// text is held as it was sent, whatever the server calls it
			b.WriteString("\tvar answer string\n\tresult.Answer = &answer\n")
			b.WriteString("\tif err = resp.Text(result.Answer); err != nil {\n\t\treturn\n\t}\n\n")
		case pointer:
			fmt.Fprintf(b, "\tvar answer %s\n\tresult.Answer = &answer\n", strings.TrimPrefix(typ, "*"))
			b.WriteString("\tif err = resp.Unmarshal(result.Answer); err != nil {\n\t\treturn\n\t}\n")
			if embedded := g.embedded(o); embedded != "" {
				fmt.Fprintf(b, "\tresult.%s = result.Answer\n", embeddedField(embedded))
			}
			b.WriteString("\n")
		default:
			b.WriteString("\tif err = resp.Unmarshal(&result.Answer); err != nil {\n\t\treturn\n\t}\n\n")
		}
	}
	b.WriteString("\treturn\n}\n")
}

// writeCompleteResult declares the result of a list's pager.
func (g *gen) writeCompleteResult(b *strings.Builder, o *definitions.Operation) {
	resultType := o.Name + "CompleteResult"
	g.declare(resultType, "operation "+o.Name)

	fmt.Fprintf(b, "\n// %s is every result %sComplete loaded.\n", resultType, o.Name)
	fmt.Fprintf(b, "type %s struct {\n\tLatestHttpResponse *http.Response\n\tItems []%s\n}\n\n", resultType, g.goType(o.Pageable.ItemType))
}

// writeCompleteMethod writes a list's pager.
func (g *gen) writeCompleteMethod(b *strings.Builder, o *definitions.Operation, optionsType string) {
	p := o.Pageable
	method := o.Name + "Complete"
	resultType := g.typeName(o.Name + "CompleteResult")
	g.declare("Client."+method, "operation "+o.Name)
	params, call := g.arguments(o, optionsType)

	switch {
	case p.ByPage && p.LimitOption == "":
		fmt.Fprintf(b, "// %s calls %s page by page, from the\n", method, o.Name)
		fmt.Fprintf(b, "// page options.%s names (the first when unset), until every result is loaded.\n", p.StartIndexOption)
		b.WriteString("// The page size is the server's: a page shorter than the first is the last.\n")
	case p.ByPage:
		fmt.Fprintf(b, "// %s calls %s page by page, from the\n", method, o.Name)
		fmt.Fprintf(b, "// page options.%s names (the first when unset), until every result is loaded.\n", p.StartIndexOption)
		fmt.Fprintf(b, "// options.%s is the page size, client.DefaultPageSize when unset.\n", p.LimitOption)
	default:
		fmt.Fprintf(b, "// %s calls %s page by page from options.%s until every\n", method, o.Name, p.StartIndexOption)
		fmt.Fprintf(b, "// result is loaded. options.%s is the page size, client.DefaultPageSize when\n// unset.\n", p.LimitOption)
	}
	fmt.Fprintf(b, "func (c Client) %s(%s) (result %s, err error) {\n", method, strings.Join(append([]string{"ctx context.Context"}, params...), ", "), resultType)
	g.writeOptionsPrelude(b, o, method, optionsType)
	ov := g.optionsVar(o)
	writePageSetup(b, p, ov, "\t")
	b.WriteString("\tfor {\n")
	fmt.Fprintf(b, "\t\tvar page %s\n", g.typeName(o.Name+"Response"))
	fmt.Fprintf(b, "\t\tpage, err = c.%s(%s)\n", o.Name, strings.Join(append([]string{"ctx"}, call...), ", "))
	b.WriteString("\t\tresult.LatestHttpResponse = page.HttpResponse\n")
	b.WriteString("\t\tif err != nil {\n\t\t\terr = fmt.Errorf(\"loading results: %w\", err)\n\t\t\treturn\n\t\t}\n")
	fmt.Fprintf(b, "\t\tif page.Answer == nil || len(page.Answer.%s) == 0 {\n\t\t\treturn\n\t\t}\n", p.ItemsField)
	fmt.Fprintf(b, "\t\tresult.Items = append(result.Items, page.Answer.%s...)\n", p.ItemsField)
	writePageEnd(b, p, ov, "\t\t", "len(result.Items)")
	b.WriteString("\t}\n}\n")
}

// writeEachMethod writes a list's pager that hands out one result at a time
// as a range-over-func, so that a loop which stops ends the paging.
func (g *gen) writeEachMethod(b *strings.Builder, o *definitions.Operation, optionsType string) {
	p := o.Pageable
	method := o.Name + "Each"
	itemType := g.goType(p.ItemType)
	g.declare("Client."+method, "operation "+o.Name)
	params, call := g.arguments(o, optionsType)

	fmt.Fprintf(b, "\n// %s calls %s page by page as its results are ranged over, so a\n", method, o.Name)
	b.WriteString("// loop that stops ends the paging:\n//\n")
	fmt.Fprintf(b, "//\tfor item, err := range c.%s(ctx) {\n//\t\tif err != nil {\n//\t\t\treturn err\n//\t\t}\n//\t\tif found(item) {\n//\t\t\tbreak\n//\t\t}\n//\t}\n//\n", method)
	fmt.Fprintf(b, "// An error is the last pair handed out, with a zero %s. The pages are asked for as %sComplete asks for them.\n", strings.TrimPrefix(itemType, "*"), o.Name)
	fmt.Fprintf(b, "func (c Client) %s(%s) iter.Seq2[%s, error] {\n", method, strings.Join(append([]string{"ctx context.Context"}, params...), ", "), itemType)
	fmt.Fprintf(b, "\treturn func(yield func(%s, error) bool) {\n", itemType)
	fmt.Fprintf(b, "\t\tvar zero %s\n", itemType)
	g.writeOptionsPreludeFailing(b, o, method, optionsType, func(expr string) string { return "yield(zero, " + expr + ")\n\t\t\treturn" })
	ov := g.optionsVar(o)
	writePageSetup(b, p, ov, "\t\t")
	if p.ByPage && p.LimitOption == "" {
		b.WriteString("\t\tloaded := 0\n")
	}
	b.WriteString("\t\tfor {\n")
	fmt.Fprintf(b, "\t\t\tpage, err := c.%s(%s)\n", o.Name, strings.Join(append([]string{"ctx"}, call...), ", "))
	b.WriteString("\t\t\tif err != nil {\n\t\t\t\tyield(zero, fmt.Errorf(\"loading results: %w\", err))\n\t\t\t\treturn\n\t\t\t}\n")
	fmt.Fprintf(b, "\t\t\tif page.Answer == nil || len(page.Answer.%s) == 0 {\n\t\t\t\treturn\n\t\t\t}\n", p.ItemsField)
	fmt.Fprintf(b, "\t\t\tfor _, item := range page.Answer.%s {\n\t\t\t\tif !yield(item, nil) {\n\t\t\t\t\treturn\n\t\t\t\t}\n\t\t\t}\n", p.ItemsField)
	if p.ByPage && p.LimitOption == "" {
		fmt.Fprintf(b, "\t\t\tloaded += len(page.Answer.%s)\n", p.ItemsField)
	}
	writePageEnd(b, p, ov, "\t\t\t", "loaded")
	b.WriteString("\t\t}\n\t}\n}\n")
}

// writePageSetup writes the page size and the first page a walk starts
// from, into the options the walk sends.
func writePageSetup(b *strings.Builder, p *definitions.Pageable, ov, in string) {
	if p.LimitOption == "" {
		// the server's page size, learnt from the first page
		fmt.Fprintf(b, "%slimit := 0\n", in)
	} else {
		fmt.Fprintf(b, "%[3]slimit := client.DefaultPageSize\n%[3]sif %[2]s.%[1]s != nil && *%[2]s.%[1]s > 0 {\n%[3]s\tlimit = *%[2]s.%[1]s\n%[3]s}\n", p.LimitOption, ov, in)
		fmt.Fprintf(b, "%s%s.%s = &limit\n", in, ov, p.LimitOption)
	}
	if p.ByPage {
		fmt.Fprintf(b, "%[3]snumber := 1\n%[3]sif %[2]s.%[1]s != nil && *%[2]s.%[1]s > 0 {\n%[3]s\tnumber = *%[2]s.%[1]s\n%[3]s}\n", p.StartIndexOption, ov, in)
		fmt.Fprintf(b, "%s%s.%s = &number\n", in, ov, p.StartIndexOption)
	} else {
		fmt.Fprintf(b, "%[3]sstart := 0\n%[3]sif %[2]s.%[1]s != nil {\n%[3]s\tstart = *%[2]s.%[1]s\n%[3]s}\n", p.StartIndexOption, ov, in)
	}
}

// writePageEnd writes how a walk ends after a page, and moves to the next:
// a short page is the last, and so is reaching the total when the server
// reports one. loaded is the expression for how many results the walk has
// handed out, which the walk by page number without a page size compares.
func writePageEnd(b *strings.Builder, p *definitions.Pageable, ov, in, loaded string) {
	switch {
	case p.ByPage && p.LimitOption == "":
		fmt.Fprintf(b, "%s// the first page says how long a page is; a shorter one is the last, and\n", in)
		fmt.Fprintf(b, "%s// so is the page that reaches the total, when the server reports one\n", in)
		fmt.Fprintf(b, "%[2]sif limit == 0 {\n%[2]s\tlimit = len(page.Answer.%[1]s)\n%[2]s}\n", p.ItemsField, in)
		fmt.Fprintf(b, "%[3]sif len(page.Answer.%[1]s) < limit || page.Answer.%[2]s > 0 && %[4]s >= page.Answer.%[2]s {\n%[3]s\treturn\n%[3]s}\n", p.ItemsField, p.TotalField, in, loaded)
		fmt.Fprintf(b, "%snumber++\n", in)
	case p.ByPage:
		fmt.Fprintf(b, "%s// a short page is the last; so is the page that reaches the total, when\n", in)
		fmt.Fprintf(b, "%s// the server reports one\n", in)
		fmt.Fprintf(b, "%[3]sif len(page.Answer.%[1]s) < limit || page.Answer.%[2]s > 0 && number*limit >= page.Answer.%[2]s {\n%[3]s\treturn\n%[3]s}\n", p.ItemsField, p.TotalField, in)
		// options holds a pointer to number, so the next call asks for the next page
		fmt.Fprintf(b, "%snumber++\n", in)
	default:
		fmt.Fprintf(b, "%[4]sstart += len(page.Answer.%[1]s)\n%[4]s%[2]s.%[3]s = &start\n", p.ItemsField, ov, p.StartIndexOption, in)
		fmt.Fprintf(b, "%s// a short page is the last; so is reaching the total, when the server\n", in)
		fmt.Fprintf(b, "%s// reports one (a list may report 0 whatever it holds)\n", in)
		fmt.Fprintf(b, "%[3]sif len(page.Answer.%[1]s) < limit || page.Answer.%[2]s > 0 && start >= page.Answer.%[2]s {\n%[3]s\treturn\n%[3]s}\n", p.ItemsField, p.TotalField, in)
	}
}

// dateAs is the call that renders a date for one parameter: in the layout the
// service's own file picks for it when there is one to ask, else the usual.
func (g *gen) dateAs(o *definitions.Operation, parameter string) string {
	if g.opts.ClientService == "" {
		return ".String()"
	}

	return fmt.Sprintf(".As(sendDateAs(%q, %q))", o.Name, parameter)
}

// idCondition is when an id option is sent: when it is not the zero of its
// base, which no server mints as an id.
func (g *gen) idCondition(kind, field string) string {
	if g.idBase(kind) == definitions.String {
		return field + ` != ""`
	}

	return field + " != 0"
}

// idText is an id as a query or a path carries it.
func (g *gen) idText(kind, expr string) string {
	switch g.idBase(kind) {
	case definitions.String:
		return "string(" + expr + ")"
	case definitions.Integer64:
		return "strconv.FormatInt(int64(" + expr + "), 10)"
	default:
		return "strconv.Itoa(int(" + expr + "))"
	}
}
