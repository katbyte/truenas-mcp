package generator

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/katbyte/pandorest/definitions"
)

// gen holds what every file of one package needs.
type gen struct {
	svc   *definitions.Service
	opts  Options
	kinds map[definitions.ObjectType]bool
	// sent are the models a request body reaches, whose booleans stay pointers
	sent      map[string]bool
	models    map[string]*definitions.Model
	constants map[string]*definitions.Constant
	// paged is the service's first list operation, which says how its lists
	// page; nil for a service with none
	paged *definitions.Operation

	// declared maps each package-level identifier to what declared it, in
	// the package being rendered (pkg); clashes are collected across them
	declared map[string]string
	clashes  []string

	// the grouped layout: pkg is the package being rendered, qual the
	// qualifier its references to the types package carry ("emby." in a
	// group, "" in the types package itself), pkgs each group's package
	// name, and imports what the file being rendered imports beyond what
	// its body shows
	pkg     string
	qual    string
	pkgs    map[string]string
	imports []importSpec
	// grp is the group whose package is being rendered, which a call's method is named within; nil elsewhere
	grp *definitions.Group
	// named are the packages the service's named types come from, by the qualifier a type is written with ("truenasrpc."),
	// gathered once for the files whose bodies use one
	named map[string]importSpec
}

// importSpec is one import a generated file needs: a path, and the name it
// is referred to by when that is not the path's last element.
type importSpec struct{ path, alias string }

func newGen(svc *definitions.Service, opts Options) (*gen, error) {
	if svc.Package == "" || !isIdent(svc.Package) {
		return nil, fmt.Errorf("%s: %q is not a package name", svc.Name, svc.Package)
	}
	if opts.ClientImport == "" {
		return nil, fmt.Errorf("%s: no base client to build the package on: its config names no ClientImport", svc.Name)
	}

	g := &gen{
		svc:       svc,
		opts:      opts,
		models:    svc.Models(),
		constants: svc.Constants(),
		declared:  map[string]string{},
		pkg:       svc.Package,
	}
	if opts.Grouped {
		if err := g.groupPackages(); err != nil {
			return nil, err
		}
	}
	for _, o := range svc.Operations() {
		if o.Pageable != nil {
			g.paged = o
			break
		}
	}
	g.kinds = kindsUsed(svc)
	g.sent = sentModels(svc)

	if opts.Client != "" && (opts.Credential != "" || opts.NewDoc != "") {
		return nil, fmt.Errorf("%s: its config words the default client.go's New (Credential, NewDoc) and gives a Client of its own as well, which has its own words: drop one or the other", svc.Name)
	}
	if opts.Client != "" && opts.Grouped {
		return nil, fmt.Errorf("%s: a Client of the service's own is for the flat layout; grouped, the constructor is generated, and an authorizer declared beside the generated files is named in Auth by its lowercase name", svc.Name)
	}
	if opts.Client == "" && g.calls() {
		// a call service has no credential or authorizer: its connection logs in
		if opts.Credential != "" || opts.NewDoc != "" || svc.Auth != "" {
			return nil, fmt.Errorf("%s: a service of calls takes a connection, so Credential, NewDoc and Auth say nothing: drop them", svc.Name)
		}
		if opts.ClientService != "" && !isIdent(opts.ClientService) {
			return nil, fmt.Errorf("%s: ClientService %q is not the name of a value in the package", svc.Name, opts.ClientService)
		}
		g.declare("Client", clientFileName)
		g.declare("New", clientFileName)
		if opts.ClientService != "" {
			g.declare(opts.ClientService, "the service's own file")
		}

		return g, nil
	}
	if opts.Client == "" {
		// New's parameter must be a name, and not one New already uses
		if argument := credentialArgument(opts.Credential); opts.Credential != "" && (!isIdent(argument) || token.IsKeyword(argument) || slices.Contains([]string{"baseURL", "opts", "c", "err", "client", "errors"}, argument)) {
			return nil, fmt.Errorf("%s: Credential %q makes no name for New's parameter (%q): call it what a caller would, as in \"API key\"", svc.Name, opts.Credential, argument)
		}
		if opts.NewDoc != "" && !strings.HasPrefix(strings.TrimSpace(opts.NewDoc), "New ") {
			return nil, fmt.Errorf("%s: NewDoc must document New, and start with its name: %q", svc.Name, opts.NewDoc)
		}
		if !isIdent(svc.Auth) {
			return nil, fmt.Errorf("%s: Auth %q is not the name of an authorizer of %s, or of one declared beside the generated files, which the default client.go needs; name one, or give the service a Client of its own", svc.Name, svc.Auth, opts.ClientImport)
		}
		if localAuth(svc.Auth) {
			// declared by hand beside the generated files, and not to be declared again
			g.declare(svc.Auth, "the service's own file")
		}
		if opts.ClientService != "" && !isIdent(opts.ClientService) {
			return nil, fmt.Errorf("%s: ClientService %q is not the name of a value in the package", svc.Name, opts.ClientService)
		}
		g.declare("Client", clientFileName)
		g.declare("New", clientFileName)
		if opts.ClientService != "" {
			// declared by hand beside the generated files, and not to be declared again by a model
			g.declare(opts.ClientService, "the service's own file")
		}

		return g, nil
	}

	names, err := declarations(opts.Client)
	if err != nil {
		return nil, fmt.Errorf("%s: the config's Client is not Go source: %w", svc.Name, err)
	}
	for _, want := range []string{"Client", "New"} {
		if !slices.Contains(names, want) {
			return nil, fmt.Errorf("%s: the config's Client does not declare %s; it must declare the Client type and New", svc.Name, want)
		}
	}
	for _, name := range names {
		g.declare(name, clientFileName)
	}

	return g, nil
}

const clientFileName = "client.go"

// declarations lists what a piece of Go source declares at package level, a
// method as Receiver.Method, which is how the generator names the operations
// it declares on Client.
func declarations(src string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), clientFileName, "package p\n\n"+src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				id, ok := recv.(*ast.Ident)
				if !ok {
					return nil, errors.New("method " + name + " has a receiver that is not a plain type")
				}
				name = id.Name + "." + name
			}
			names = append(names, name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, spec.Name.Name)
				case *ast.ValueSpec:
					for _, id := range spec.Names {
						names = append(names, id.Name)
					}
				case *ast.ImportSpec:
					return nil, errors.New("it imports " + spec.Path.Value + "; the generator writes the imports, for the packages it knows")
				}
			}
		}
	}

	return names, nil
}

// declare records a package-level identifier of the package being rendered.
func (g *gen) declare(name, by string) {
	if prev, ok := g.declared[name]; ok {
		where := ""
		if g.opts.Grouped {
			where = "package " + g.pkg + ": "
		}
		g.clashes = append(g.clashes, fmt.Sprintf("%s%s is declared by both %s and %s", where, name, prev, by))
		return
	}
	g.declared[name] = by
}

// localAuth reports whether Auth names an authorizer declared beside the
// generated files rather than one of the base client's: by its lowercase
// name, which an exported one of the base client's cannot have.
func localAuth(auth string) bool {
	first, _ := utf8.DecodeRuneInString(auth)

	return unicode.IsLower(first)
}

// authorizerExpr is the expression that wraps the credential in the
// service's authorizer: the base client's, or the package's own.
func authorizerExpr(auth, credential string) string {
	if localAuth(auth) {
		return auth + "(" + credential + ")"
	}

	return "client." + auth + "(" + credential + ")"
}

// cmpOr is the first of two strings that is not empty.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

// typeName is how a name declared in the types package is written in the
// package being rendered.
func (g *gen) typeName(name string) string { return g.qual + name }

func (g *gen) checkIdentifiers() error {
	if len(g.clashes) == 0 {
		return nil
	}
	slices.Sort(g.clashes)

	return fmt.Errorf("%s: the generated identifiers clash:\n  %s", g.svc.Name, strings.Join(g.clashes, "\n  "))
}

// isStruct reports whether a reference names a struct model (not a union
// alias or a constant).
func (g *gen) isStruct(name string) bool {
	m := g.models[name]
	return m != nil && len(m.Union) == 0
}

// goType renders a type as it is written in a list, a map, an argument or an
// option: a struct model by its name.
func (g *gen) goType(t definitions.TypeRef) string {
	switch t.Type {
	case definitions.Boolean:
		return "bool"
	case definitions.Integer:
		return "int"
	case definitions.Integer64:
		return "int64"
	case definitions.Float:
		return "float32"
	case definitions.Double:
		return "float64"
	case definitions.String:
		return "string"
	case definitions.DateTime:
		return g.typeName("Time")
	case definitions.Date:
		return g.typeName("Day")
	case definitions.UnixSeconds:
		return g.typeName("Unix")
	case definitions.UnixMillis:
		return g.typeName("UnixMilli")
	case definitions.Any:
		return "any"
	case definitions.RawObject:
		return "json.RawMessage"
	case definitions.RawFile:
		return "io.Reader"
	case definitions.List:
		return "[]" + g.goType(*t.NestedItem)
	case definitions.Dictionary:
		return "map[string]" + g.goType(*t.NestedItem)
	case definitions.Reference, definitions.ID:
		return g.typeName(t.ReferenceName)
	case definitions.Named:
		return t.GoType
	}

	return "any"
}

// namedImports are the packages the service's named types come from, by the qualifier a type is written with: a type written
// truenasrpc.Secret[int] from github.com/katbyte/truenas-mcp/sdk/truenasrpc is imported plain, and one whose qualifier is not
// its path's last element under that name.
func (g *gen) namedImports() map[string]importSpec {
	if g.named != nil {
		return g.named
	}
	g.named = map[string]importSpec{}
	var walk func(t definitions.TypeRef)
	walk = func(t definitions.TypeRef) {
		switch t.Type {
		case definitions.Named:
			if qual, _, ok := strings.Cut(t.GoType, "."); ok && t.Import != "" {
				g.named[qual+"."] = importSpec{t.Import, qual}
			}
		case definitions.List, definitions.Dictionary:
			if t.NestedItem != nil {
				walk(*t.NestedItem)
			}
		default:
		}
	}
	for _, m := range g.svc.Models() {
		for _, f := range m.Fields {
			walk(f.Type)
		}
	}
	for _, o := range g.svc.Operations() {
		for _, t := range o.Types() {
			walk(t)
		}
	}

	return g.named
}

// heldType renders a type as a model's field holds it: a struct model by
// pointer, anything else as goType has it.
func (g *gen) heldType(t definitions.TypeRef) string {
	if t.Type == definitions.Reference && g.isStruct(t.ReferenceName) {
		return "*" + g.typeName(t.ReferenceName)
	}

	return g.goType(t)
}

// fieldType is how a model holds a field: a struct model by pointer, a
// number or string whose null is kept (KeepsNull) by pointer, a boolean by
// pointer where boolPointer says, the rest by value.
func (g *gen) fieldType(m *definitions.Model, f *definitions.Field) string {
	if f.KeepsNull || g.boolPointer(m, f) {
		return "*" + g.heldType(f.Type)
	}

	return g.heldType(f.Type)
}

// boolPointer says when a boolean is held by pointer: when the document
// declares it nullable or KeepNull names it; and in a model a request body
// reaches, unless the model is written back whole or the field is required,
// since a boolean by value is always sent and a server that keeps what it is
// not sent would read a flag never set as switched off (an edit of a user's
// email removing an administrator). A model only ever answered holds its
// booleans by value, false when the server left them out, which is what
// every caller read them as through a helper.
func (g *gen) boolPointer(m *definitions.Model, f *definitions.Field) bool {
	if f.Type.Type != definitions.Boolean {
		return false
	}

	return f.KeepsNull || f.Nullable || (g.sent[m.Name] && !m.WrittenWhole && !f.Required)
}

// fieldTag is the JSON tag of a model's field. A boolean by value is always
// sent, so a record written back whole carries its false flags and a server
// that defaults a flag to true reads the false; one held by pointer
// (boolPointer) is omitempty, so nil leaves it unset. Lists and
// maps are omitzero, so a nil one is left out but an empty one is sent,
// which is how a body clears a list, as are the dates and a named type, since omitempty never leaves a struct out. The rest are
// omitempty, except the strings and numbers of a model written back whole (a settings object), which are always sent: read "",
// write "", rather than a null the server does not save. A field the document requires is always sent whatever it is, so an
// empty string goes as "" rather than not at all.
func (g *gen) fieldTag(m *definitions.Model, f *definitions.Field) string {
	if f.Required || (f.Type.Type == definitions.Boolean && !g.boolPointer(m, f)) {
		return f.JSONName
	}
	if union := g.models[f.Type.ReferenceName]; f.Type.Type == definitions.Reference && union != nil && len(union.Members) > 0 {
		// a union holding nothing is left out, since omitempty never leaves a struct out
		return f.JSONName + ",omitzero"
	}
	switch f.Type.Type {
	case definitions.List, definitions.Dictionary, definitions.DateTime, definitions.Date, definitions.UnixSeconds, definitions.UnixMillis, definitions.Named:
		return f.JSONName + ",omitzero"
	case definitions.String, definitions.Integer, definitions.Integer64, definitions.Float, definitions.Double:
		// a number or string whose null is kept is a pointer, and its nil is left out
		if m.WrittenWhole && !f.KeepsNull {
			return f.JSONName
		}
	default:
	}

	return f.JSONName + ",omitempty"
}

// fileHeader starts a file: the generated marker, the package clause and
// the imports the body uses.
func (g *gen) fileHeader(body string) string {
	var b strings.Builder
	b.WriteString(Header)
	b.WriteString("\n")
	fmt.Fprintf(&b, "package %s\n\n", g.pkg)

	var std, ext []string
	for _, imp := range []struct{ path, use string }{
		{"bytes", "bytes."},
		{"context", "context."},
		{"encoding/json", "json."},
		{"errors", "errors."},
		{"fmt", "fmt."},
		{"io", "io."},
		{"iter", "iter."},
		{"net/http", "http."},
		{"net/http/httptest", "httptest."},
		{"net/url", "url."},
		{"reflect", "reflect."},
		{"regexp", "regexp."},
		{"slices", "slices."},
		{"strconv", "strconv."},
		{"strings", "strings."},
		{"testing", "testing."},
		{"time", "time."},
	} {
		if usesPackage(body, imp.use) {
			std = append(std, imp.path)
		}
	}
	if usesPackage(body, "client.") {
		ext = append(ext, strconv.Quote(g.opts.ClientImport))
	}
	if g.qual != "" && usesPackage(body, g.qual) {
		ext = append(ext, g.rootImport().String())
	}
	for _, imp := range g.imports {
		ext = append(ext, imp.String())
	}
	named := g.namedImports()
	for _, use := range sortedKeys(named) {
		// the base client's own package is imported already wherever the body says client.
		if imp := named[use]; usesPackage(body, use) && !slices.Contains(ext, imp.String()) && (use != "client." || imp.path != g.opts.ClientImport) {
			ext = append(ext, imp.String())
		}
	}
	switch len(std) + len(ext) {
	case 0:
	case 1:
		if len(std) == 1 {
			fmt.Fprintf(&b, "import %q\n\n", std[0])
		} else {
			fmt.Fprintf(&b, "import %s\n\n", ext[0])
		}
	default:
		b.WriteString("import (\n")
		for _, p := range std {
			fmt.Fprintf(&b, "\t%q\n", p)
		}
		if len(std) > 0 && len(ext) > 0 {
			b.WriteString("\n")
		}
		for _, p := range ext {
			fmt.Fprintf(&b, "\t%s\n", p)
		}
		b.WriteString(")\n\n")
	}
	b.WriteString(body)

	return b.String()
}

// String renders the import as it is written in an import block.
func (i importSpec) String() string {
	if i.alias != "" && i.alias != path.Base(i.path) {
		return i.alias + " " + strconv.Quote(i.path)
	}

	return strconv.Quote(i.path)
}

// rootImport is how a group imports the types package: Dir's import path,
// or the package under it when Dir is not named for it (see grouped.go).
func (g *gen) rootImport() importSpec {
	if filepath.Base(g.opts.Dir) == g.svc.Package {
		return importSpec{g.opts.ImportPath, g.svc.Package}
	}

	return importSpec{path.Join(g.opts.ImportPath, g.svc.Package), g.svc.Package}
}

// usesPackage reports whether code refers to a package selector such as
// "json." outside a comment or string, well enough for generated code: the
// selector must follow a character that cannot end an identifier.
func usesPackage(code, selector string) bool {
	for line := range strings.SplitSeq(code, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for i := 0; ; {
			j := strings.Index(line[i:], selector)
			if j < 0 {
				break
			}
			at := i + j
			// a selector after a dot is a field of something else, except after the dots of a variadic (files ...io.Reader)
			if at == 0 || !isIdentRune(rune(line[at-1])) && (line[at-1] != '.' || strings.HasSuffix(line[:at], "...")) && line[at-1] != '"' {
				return true
			}
			i = at + len(selector)
		}
	}

	return false
}

func isIdentRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func isIdent(s string) bool {
	for i, r := range s {
		if !isIdentRune(r) || i == 0 && unicode.IsDigit(r) {
			return false
		}
	}

	return s != ""
}

// comment renders text as Go line comments with the given indent, or nothing
// when the text is blank.
func comment(indent, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var b strings.Builder
	for line := range strings.SplitSeq(text, "\n") {
		b.WriteString(indent)
		b.WriteString("//")
		if line = strings.TrimRight(line, " \t"); line != "" {
			b.WriteString(" ")
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// firstSentence trims text to its first sentence, for one-line doc comments.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	if s != "" && !strings.HasSuffix(s, ".") {
		s += "."
	}

	return s
}

// apiVersionConsts names the document a package was generated from, so a
// caller can read it at run time and compare it with the server's own.
func (g *gen) apiVersionConsts() string {
	g.declare("APIVersion", clientFileName)
	consts := fmt.Sprintf(`// APIVersion is the version of the API document this package was generated
// from (%s, as the document names itself); a server of another version may
// answer differently.
const APIVersion = %q

`, g.svc.Title, g.svc.APIVersion)
	if g.opts.Version == "" {
		return consts
	}
	g.declare("DocumentVersion", clientFileName)

	return consts + fmt.Sprintf(`// DocumentVersion is the version the document's file is named by: the
// release of the server it was taken from, which is the one to compare with a
// running server's where the document's own version names the API and does not
// move from release to release.
const DocumentVersion = %q

`, g.opts.Version)
}

// defaultClient is the client.go of a service that gives none of its own: the
// Client type, and a New that wraps its credential in the base client's
// authorizer the service's Auth names and makes the client the service's own
// way where it says what that is. Its arguments: the document's title, the
// credential wrapped in the authorizer, what makes the client (the package's client.Service, or the
// base client itself), New's documentation, its parameter for the credential
// and its refusal of an empty one.
const defaultClient = `// Client is a client for %[1]s; each of its operations is a method.
// Requests go through Client.Client, the shared base client.
type Client struct {
	Client *client.Client
}

%[4]sfunc New(baseURL, %[5]s string, opts ...client.Option) (*Client, error) {
	if %[5]s == "" {
		return nil, errors.New(%[6]q)
	}
	// the credential is a secret whatever the authorizer makes of it, so no error repeats it
	c, err := %[3]s.New(baseURL, %[2]s, append(opts[:len(opts):len(opts)], client.WithSecrets(%[5]s))...)
	if err != nil {
		return nil, err
	}

	return &Client{Client: c}, nil
}
`

// defaultNewDoc is the documentation of the default New, for a service that
// words none of its own: the document's title, and the parameter the
// credential is passed in.
const defaultNewDoc = `New returns a client for %[1]s at baseURL (scheme and host, optionally a
path prefix) that authenticates with %[2]s. The options are the base
client's: a logger to trace to, a transport of the caller's own.`

func (g *gen) clientFile() string {
	if g.calls() {
		return g.callClientFile()
	}
	if g.opts.Client != "" {
		return g.fileHeader(g.apiVersionConsts() + strings.TrimSpace(g.opts.Client) + "\n")
	}

	argument, refusal := "token", "a token is required"
	if g.opts.Credential != "" {
		argument, refusal = credentialArgument(g.opts.Credential), g.opts.Credential+" is required"
	}
	doc := cmpOr(g.opts.NewDoc, fmt.Sprintf(defaultNewDoc, g.svc.Title, argument))

	return g.fileHeader(g.apiVersionConsts() + fmt.Sprintf(defaultClient, g.svc.Title, authorizerExpr(g.svc.Auth, argument), cmpOr(g.opts.ClientService, "client"), comment("", doc), argument, refusal))
}

// credentialArgument is the name of New's parameter for a credential the
// service calls by a name of its own: "API key" is apiKey.
func credentialArgument(credential string) string {
	words := strings.FieldsFunc(credential, func(r rune) bool { return !isIdentRune(r) || r == '_' })
	if len(words) > 2 {
		// a phrase ("TMDB read access token or API key") is for the doc, not for an argument name
		return "credential"
	}

	var b strings.Builder
	for i, word := range words {
		if i == 0 {
			b.WriteString(strings.ToLower(word))
			continue
		}
		first, size := utf8.DecodeRuneInString(word)
		b.WriteRune(unicode.ToUpper(first))
		b.WriteString(word[size:])
	}

	return b.String()
}

// docFile is the package's documentation. It is the one generated file
// without the generated header: linters pass over a file that has it, and a
// package with a file written by hand beside the generated ones (the
// service's own, a test) would then be told it has no package comment. Its
// first line says what wrote it instead, which is how Generated knows it.
func (g *gen) docFile() string {
	// no generated header here: a linter skips a file marked generated and then misses the package comment, and flags the
	// hand-written file beside it as lacking one; the comment's first line says what wrote it instead, and Generated knows it
	var b strings.Builder
	fmt.Fprintf(&b, "// Package %s is a typed client for %s %s, generated by pandorest\n", g.svc.Package, g.svc.Title, g.svc.APIVersion)
	fmt.Fprintf(&b, "// from the definitions in %s, which were imported from %s.\n", filepath.ToSlash(g.opts.Definitions), g.svc.Source)
	b.WriteString("//\n// # Layout\n//\n")
	if g.opts.Grouped {
		fmt.Fprintf(&b, `// This package holds every model (model_<model>.go), every enum
// (constants_<tag>.go) and every operation's options and response types
// (operation_<operation>.go), so that a caller names them all as %[1]s.X. The
// operations are methods on the Client of their tag's package, one package
// per tag beside this one, and %[2]s/client holds New and a Client with a
// field for each. A caller imports this package and client, and never a
// tag's. The requests go through the hand-written base client,
// %[3]s.
//
`, g.svc.Package, path.Base(g.opts.ImportPath), g.opts.ClientImport)
	} else {
		fmt.Fprintf(&b, `// Every operation of the API is a method on Client, in a file of its own named
// <tag>_method_<operation>.go. Each model is in <tag>_model_<model>.go and each
// tag's enums in <tag>_constants.go; a model or enum more than one tag uses is
// under common_. The requests go through the hand-written base client,
// %s.
//
`, g.opts.ClientImport)
	}
	if g.calls() {
		b.WriteString(`// # Calls
//
// Each method of the server is a method here, named for it (pool.query is
// PoolQuery) and taking a context and then the method's arguments in the
// order the server takes them, each typed from the server's description; an
// argument the server does not insist on may be left as its zero, and those
// left so at the end are not sent. Every call goes through the client.Caller
// New is given, which the repository writes over its connection: it is what
// dials, logs in, numbers the calls and matches the answers.
//
// It returns a <Name>Response holding, when the method answers something,
// Answer: a pointer for a struct, enum or primitive and the value for a list,
// map or raw JSON, nil when the call failed. A struct answer is also embedded,
// so res.Answer.Name may be read as res.Name, a zero record when nothing was
// answered, and Record() hands it back by value with an error when none came.
// A call the server refuses is an error carrying a client.CallError with what
// the server said: its code, error name and reason; client.CallErrorOf reads
// it, IsNotFound says the server has no such record, and client.IsRefusedCall
// tells a refusal from a connection that failed.
`)
	} else {
		g.writeRequestDoc(&b)
	}
	if g.paged != nil {
		pages := g.paged.Pageable.StartIndexOption
		if g.paged.Pageable.LimitOption != "" {
			pages += " and " + g.paged.Pageable.LimitOption
		}
		fmt.Fprintf(&b, "//\n// A list operation, one whose options page with %s, also has a\n// <Name>Complete method that loads every result, and a <Name>Each that\n// hands them out one at a time as a range, so a loop that stops asks for no\n// page more.\n", pages)
	}
	if notes := comment("", g.opts.Notes); notes != "" {
		// before the Ids heading, or the service's own words read as part of it
		b.WriteString("//\n")
		b.WriteString(notes)
	}
	if len(g.svc.IDs) > 0 {
		names := make([]string, 0, len(g.svc.IDs))
		for _, k := range g.svc.IDs {
			names = append(names, k.Name)
		}
		fmt.Fprintf(&b, "//\n// # Ids\n//\n// %s are kinds of id the package tells apart, each a bare named type over\n// the int or string the document declares, so that one kind where another goes\n// does not compile. Where an id sits among the options it is held by value and\n// sent when not zero. int(id) and SeriesID(n) convert one value, client.IDs and\n// client.StringIDs a list.\n", strings.Join(names, ", "))
	}
	if len(g.svc.Workarounds) > 0 {
		b.WriteString("//\n// # Workarounds\n//\n// The importer fixed these bugs in the document before generating from it:\n//\n")
		for _, w := range g.svc.Workarounds {
			fmt.Fprintf(&b, "//   - %s\n", w)
		}
	}
	fmt.Fprintf(&b, "package %s\n", g.svc.Package)

	return b.String()
}

// writeRequestDoc is the Calls section of a service of requests: what a method takes and answers.
func (g *gen) writeRequestDoc(b *strings.Builder) {
	b.WriteString(`// # Calls
//
// A method takes a context, then the path parameters in path order, then the
// request body (input: the model for a JSON body, or an io.Reader and its
// content type for raw bytes), then, when the operation has query or header
// parameters, a <Name>Options, which may be left out when every one of them
// is optional`)
	if g.opts.InlineSingleOptions {
		b.WriteString(`, and which an operation with one parameter takes as the
// parameter itself, variadic when optional`)
	}
	b.WriteString(`.
// Unset options are not sent: a boolean or a number is a pointer, sent when
// set (so 0 and false can be asked for), an empty string or list is skipped,
// and a list is sent comma-joined or as one parameter for each element, as
// the document says.
//
// It returns a <Name>Response holding HttpResponse and, when the operation
// answers JSON, Answer: a pointer for a struct, enum or primitive, nil when
// the server answered with no body, and the value for a list, map or raw
// JSON. Answered() says whether a body came and was decoded. A struct answer
// is also embedded, so res.Answer.Name may be read as res.Name, a zero record
// when nothing was answered, and Record() hands it back by value with an
// error when none came. HttpResponse is set whenever the server answered,
// including alongside an error. An operation that answers a file has no
// Answer: WriteTo(w) copies the file out and closes the body, Bytes(max)
// reads it whole up to a size, Reader() hands the body over with its content
// type, ContentLength() and Filename() say what the server said of it, and
// the body is also still in HttpResponse.Body. A status the operation does
// not document is an error: client.StatusCode reads the status from it, and
// IsNotFound and the other Is helpers say which it is.
`)
}

// kindsUsed is every type a service's fields, options, path parameters and bodies are of, so that a
// type the package declares only when something is of it (Unix, UnixMilli) is declared then.
func kindsUsed(svc *definitions.Service) map[definitions.ObjectType]bool {
	kinds := map[definitions.ObjectType]bool{}
	var note func(t definitions.TypeRef)
	note = func(t definitions.TypeRef) {
		kinds[t.Type] = true
		if t.NestedItem != nil {
			note(*t.NestedItem)
		}
	}
	for _, m := range svc.Models() {
		for i := range m.Fields {
			note(m.Fields[i].Type)
		}
	}
	for _, o := range svc.Operations() {
		for i := range o.Options {
			note(o.Options[i].Type)
		}
		for i := range o.PathParameters {
			note(o.PathParameters[i].Type)
		}
		if o.Request != nil {
			note(o.Request.Type)
		}
		if o.Response != nil {
			note(o.Response.Type)
		}
	}

	return kinds
}

// datesFile reports whether the package declares a date type: Time or Day from the service's layouts,
// Unix or UnixMilli from a field a workaround declared an epoch.
func (g *gen) datesFile() bool {
	return len(g.svc.Dates) > 0 || len(g.svc.Days) > 0 || g.kinds[definitions.UnixSeconds] || g.kinds[definitions.UnixMillis]
}

// idBase is the base type of a kind of id, Integer when the service does not
// declare the kind, which Validate has already refused.
func (g *gen) idBase(kind string) definitions.ObjectType {
	if base := g.svc.IDBase(kind); base != "" {
		return base
	}

	return definitions.Integer
}

// idsFile declares a named type for each kind of id the service tells apart.
func (g *gen) idsFile() string {
	var b strings.Builder
	for i, k := range g.svc.IDs {
		g.declare(k.Name, "ids.go")
		if i > 0 {
			b.WriteString("\n")
		}
		base := g.goType(definitions.TypeRef{Type: k.Base})
		list := "client.IDs"
		if k.Base == definitions.String {
			list = "client.StringIDs"
		}
		fmt.Fprintf(&b, "// %s is a kind of id the service tells apart, which the compiler keeps from every other kind;\n// %s(id) and %s(n) convert one value, %s a list.\n", k.Name, base, k.Name, list)
		fmt.Fprintf(&b, "type %s %s\n", k.Name, base)
		if k.Shape != "" {
			shape := shapeVar(k.Name)
			g.declare(shape, "ids.go")
			fmt.Fprintf(&b, "\n// %s is the shape every %s has.\nvar %s = regexp.MustCompile(%s)\n", shape, k.Name, shape, strconv.Quote(k.Shape))
			fmt.Fprintf(&b, "\n// Check refuses a value that has not the shape every %s has, which a method does before sending a request with it; the error names the value.\n", k.Name)
			fmt.Fprintf(&b, "func (id %s) Check() error {\n\tif !%s.MatchString(fmt.Sprint(%s(id))) {\n\t\treturn fmt.Errorf(\"%%v is not %s %s (the shape is %%s)\", %s(id), %s)\n\t}\n\n\treturn nil\n}\n", k.Name, shape, base, article(k.Name), k.Name, base, strconv.Quote(k.Shape))
		}
	}

	return g.fileHeader(b.String())
}

// article is "an" before a name that starts with a vowel, else "a".
func article(name string) string {
	if strings.ContainsRune("AEIOUaeiou", rune(name[0])) {
		return "an"
	}

	return "a"
}

// shapeVar names the variable holding a kind's shape: seriesIDShape.
func shapeVar(kind string) string {
	return strings.ToLower(kind[:1]) + kind[1:] + "Shape"
}

// idShape is the shape of a kind of id, or "" for one with none.
func (g *gen) idShape(kind string) string {
	if k := g.svc.IDKind(kind); k != nil {
		return k.Shape
	}

	return ""
}

// sentModels is every model a request body or a call's argument reaches, itself or through a field, a list or a map of it.
func sentModels(svc *definitions.Service) map[string]bool {
	models := svc.Models()
	sent := map[string]bool{}
	var reach func(t definitions.TypeRef)
	reach = func(t definitions.TypeRef) {
		switch {
		case t.NestedItem != nil:
			reach(*t.NestedItem)
		case t.Type == definitions.Reference:
			m := models[t.ReferenceName]
			if m == nil || sent[m.Name] {
				return
			}
			sent[m.Name] = true
			for i := range m.Fields {
				reach(m.Fields[i].Type)
			}
			// a union that is sent sends one of its members
			for _, member := range m.Members {
				reach(definitions.TypeRef{Type: definitions.Reference, ReferenceName: member.Model})
			}
		}
	}
	for _, o := range svc.Operations() {
		if o.Request != nil {
			reach(o.Request.Type)
		}
		for _, a := range o.Arguments {
			reach(a.Type)
		}
	}

	return sent
}
