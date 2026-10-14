package generator

import (
	"encoding/json"
	"fmt"
	"go/token"
	"path"
	"path/filepath"
	"strings"

	"github.com/katbyte/pandorest/definitions"
)

// The grouped layout, after go-azure-sdk's service/version/resource:
//
//	<dir>/                      the types package, named Package: every model
//	                            (model_<model>.go), every enum
//	                            (constants_<tag>.go), every operation's options
//	                            and response types (operation_<operation>.go),
//	                            New for the base client (client.go), the base
//	                            client's options and error helpers under the
//	                            package's own name (helpers.go), and doc.go
//	<dir>/<tag>/                a package per tag with operations: its Client
//	                            and NewClient (client.go), a method per
//	                            operation (method_<operation>.go) with its test,
//	                            the canned server the tests share
//	                            (client_test.go), and doc.go
//	<dir>/client/               New, and Client with a field per tag (client.go)
//	<dir>/groups.json           each operation's tag and each tag's package,
//	                            for a caller moving from the flat layout
//
// A caller imports the types package and client, and never a tag's: nothing
// a caller names is declared in one, so a tag's name is free to be a word
// the caller uses for a variable.
//
// A package's name matches its directory's. So the types package is <dir>
// itself when <dir> is named for the package (sdk/emby), and <dir>/<package>
// when it is not: a version directory (sdk/emby/4.10.0.40/emby), which then
// holds the packages and nothing else.

// reservedPackages are names a tag's package cannot have: the standard
// library's top-level packages, which a file importing both could not tell
// apart, and the layout's own.
var reservedPackages = func() map[string]bool {
	m := map[string]bool{}
	for s := range strings.FieldsSeq("archive bufio bytes cmp compress container context crypto database debug embed encoding errors expvar flag fmt go hash html image index io iter log maps math mime net os path plugin reflect regexp runtime slices sort strconv strings sync syscall testing text time unicode unique unsafe " +
		"client internal main testdata vendor") {
		m[s] = true
	}

	return m
}()

// groupPackages names each tag's package: its lowercased name, or what the
// options' Packages say, refusing a name Go or a caller could not use.
func (g *gen) groupPackages() error {
	if g.opts.ImportPath == "" {
		return fmt.Errorf("%s: the grouped layout needs the import path of %s (ImportPath), for its packages to import one another by", g.svc.Name, g.svc.Package)
	}
	g.pkgs = map[string]string{}
	byPackage := map[string]string{}
	named := map[string]bool{}
	for gi := range g.svc.Groups {
		grp := &g.svc.Groups[gi]
		if len(grp.Operations) == 0 {
			continue
		}
		name := strings.ToLower(grp.Name)
		if given, ok := g.opts.Packages[grp.Name]; ok {
			name, named[grp.Name] = given, true
		}
		switch {
		case !isIdent(name) || strings.ToLower(name) != name:
			return fmt.Errorf("%s: the tag %s would be package %q, which is not a lowercase Go identifier: name it in Packages", g.svc.Name, grp.Name, name)
		case token.IsKeyword(name):
			return fmt.Errorf("%s: the tag %s would be package %q, a Go keyword: name it in Packages", g.svc.Name, grp.Name, name)
		case reservedPackages[name] || name == g.svc.Package || name == g.clientPackage():
			return fmt.Errorf("%s: the tag %s would be package %q, which the standard library or this layout already uses: name it in Packages", g.svc.Name, grp.Name, name)
		case strings.HasSuffix(name, "_test"):
			return fmt.Errorf("%s: the tag %s would be package %q, which go test reads as a test package: name it in Packages", g.svc.Name, grp.Name, name)
		}
		if other, ok := byPackage[name]; ok {
			return fmt.Errorf("%s: the tags %s and %s would both be package %q; give one another name in Packages", g.svc.Name, other, grp.Name, name)
		}
		byPackage[name], g.pkgs[grp.Name] = grp.Name, name
	}
	for _, tag := range sortedKeys(g.opts.Packages) {
		if !named[tag] {
			return fmt.Errorf("%s: Packages names the tag %s, which the document has no operations under: remove the entry", g.svc.Name, tag)
		}
	}

	return nil
}

// enter starts rendering a package: the identifiers declared so far are
// another package's.
func (g *gen) enter(pkg, qual string) {
	g.pkg, g.qual = pkg, qual
	g.declared = map[string]string{}
	g.imports = nil
	g.grp = nil
}

// typesDir is the directory of the types package under Dir, with a slash,
// or empty when Dir is the types package.
func (g *gen) typesDir() string {
	if filepath.Base(g.opts.Dir) == g.svc.Package {
		return ""
	}

	return g.svc.Package + "/"
}

// renderGrouped produces every file of the grouped layout.
func (g *gen) renderGrouped(add func(name, src string) error) error {
	// the types package
	root := g.typesDir()
	g.enter(g.svc.Package, "")
	if g.opts.ClientService != "" {
		g.declare(g.opts.ClientService, "the service's own file")
	}
	if localAuth(g.svc.Auth) {
		g.declare(g.svc.Auth, "the service's own file")
	}
	if err := add(root+"doc.go", g.docFile()); err != nil {
		return err
	}
	if err := add(root+"client.go", g.rootClientFile()); err != nil {
		return err
	}
	seen := map[string]string{}
	for gi := range g.svc.Groups {
		grp := &g.svc.Groups[gi]
		if len(grp.Constants) > 0 {
			if err := add(root+kindFile("constants", grp.Name), g.constantsFile(grp)); err != nil {
				return err
			}
		}
		for mi := range grp.Models {
			m := &grp.Models[mi]
			if err := add(root+kindFile("model", m.Name), g.modelFile(m)); err != nil {
				return err
			}
		}
		for oi := range grp.Operations {
			o := &grp.Operations[oi]
			// a method's name is unique across the SDK, as it was in one package, so the sweep and a caller's own checks can go by it alone
			if other, ok := seen[o.Name]; ok {
				g.clashes = append(g.clashes, fmt.Sprintf("%s is an operation of both %s and %s: name one with a workaround", o.Name, other, grp.Name))
			}
			seen[o.Name] = grp.Name
			if err := add(root+kindFile("operation", o.Name), g.operationFile(o)); err != nil {
				return err
			}
		}
	}
	if err := add(root+"helpers.go", g.helpersFile()); err != nil {
		return err
	}
	if err := add(root+"operations.go", g.operationsFile()); err != nil {
		return err
	}
	if g.datesFile() {
		if err := add(root+"time.go", g.timeFile()); err != nil {
			return err
		}
	}
	if len(g.svc.IDs) > 0 {
		if err := add(root+"ids.go", g.idsFile()); err != nil {
			return err
		}
	}

	// a package per tag
	for gi := range g.svc.Groups {
		grp := &g.svc.Groups[gi]
		pkg, ok := g.pkgs[grp.Name]
		if !ok {
			continue
		}
		g.enter(pkg, g.svc.Package+".")
		g.grp = grp
		g.declare("Client", clientFileName)
		g.declare("NewClient", clientFileName)
		if err := add(pkg+"/doc.go", g.groupDocFile(grp)); err != nil {
			return err
		}
		if err := add(pkg+"/client.go", g.groupClientFile(grp)); err != nil {
			return err
		}
		for oi := range grp.Operations {
			o := &grp.Operations[oi]
			name := kindFile("method", o.Name)
			if err := add(pkg+"/"+name, g.groupMethodFile(o)); err != nil {
				return err
			}
			if err := add(pkg+"/"+strings.TrimSuffix(name, ".go")+"_test.go", g.testFile(o)); err != nil {
				return err
			}
		}
		if err := add(pkg+"/client_test.go", g.testHelpersFile()); err != nil {
			return err
		}
	}

	// the client of every tag
	cp := g.clientPackage()
	g.enter(cp, g.svc.Package+".")
	if err := add(cp+"/doc.go", g.aggregateDocFile()); err != nil {
		return err
	}
	if err := add(cp+"/client.go", g.aggregateFile()); err != nil {
		return err
	}
	if err := add(cp+"/client_test.go", g.aggregateTestFile()); err != nil {
		return err
	}

	return add("groups.json", g.groupsJSON())
}

// rootClient is the types package's client.go: New makes the base client
// every tag's operations send through. Its arguments are the default
// client's but the first, which is the credential wrapped in the authorizer.
const rootClient = `%[4]sfunc New(baseURL, %[5]s string, opts ...client.Option) (*client.Client, error) {
	if %[5]s == "" {
		return nil, errors.New(%[6]q)
	}

	// the credential is a secret whatever the authorizer makes of it, so no error repeats it
	return %[3]s.New(baseURL, %[2]s, append(opts[:len(opts):len(opts)], client.WithSecrets(%[5]s))...)
}
`

// rootNewDoc documents the types package's New: the document's title and
// the credential's parameter.
const rootNewDoc = `New makes the base client every tag's operations send their requests
through, for %[1]s at baseURL (scheme and host, optionally a path prefix),
authenticating with %[2]s. The options are the base client's: a logger to
trace to, a transport of the caller's own. client.New makes it and a Client
of every tag's operations in one call.`

// credential is New's parameter for the credential, and the refusal of an
// empty one.
func (g *gen) credential() (argument, refusal string) {
	if g.opts.Credential != "" {
		return credentialArgument(g.opts.Credential), g.opts.Credential + " is required"
	}

	return "token", "a token is required"
}

func (g *gen) rootClientFile() string {
	if g.calls() {
		// a call service's New is the client package's alone: there is no base client to make here
		return g.fileHeader(g.apiVersionConsts())
	}
	g.declare("New", clientFileName)
	argument, refusal := g.credential()

	return g.fileHeader(g.apiVersionConsts() + fmt.Sprintf(rootClient, g.svc.Title, authorizerExpr(g.svc.Auth, argument), cmpOr(g.opts.ClientService, "client"), comment("", fmt.Sprintf(rootNewDoc, g.svc.Title, argument)), argument, refusal))
}

// helper is one of the base client's options or error helpers, offered
// under the types package's name so a caller imports one package.
type helper struct {
	name, decl string
}

var helpers = []helper{
	{"Option", "// Option is an option of the base client, which New takes.\ntype Option = client.Option\n"},
	{"Authorizer", "// Authorizer is what puts the credential on a request.\ntype Authorizer = client.Authorizer\n"},
	{"APIKey", "// APIKey is a credential sent in the X-Api-Key header.\ntype APIKey = client.APIKey\n"},
	{"Bearer", "// Bearer is a credential sent as a bearer token.\ntype Bearer = client.Bearer\n"},
	{"Service", "// Service is what is a service's own about being talked to, which its hand-written file declares.\ntype Service = client.Service\n"},
	{"Failure", "// Failure is a refusal, for a Service's Note.\ntype Failure = client.Failure\n"},
	{"Response", "// Response is an answer, as a hand-written request through the base client reads it.\ntype Response = client.Response\n"},
	{"RequestOptions", "// RequestOptions describes a hand-written request through the base client.\ntype RequestOptions = client.RequestOptions\n"},
	{"Headers", "// Headers are the header parameters an options object sets.\ntype Headers = client.Headers\n"},
	{"QueryParams", "// QueryParams are the query parameters an options object sets.\ntype QueryParams = client.QueryParams\n"},
	{"StatusError", "// StatusError is an answer with a status the operation does not document.\ntype StatusError = client.StatusError\n"},
	{"WithLog", "// WithLog has the client trace each exchange to a logger, with the credentials hidden.\nfunc WithLog(log client.Logger) Option { return client.WithLog(log) }\n"},
	{"WithTransport", "// WithTransport has the client send through a transport of the caller's own.\nfunc WithTransport(base http.RoundTripper) Option { return client.WithTransport(base) }\n"},
	{"WithRetry", "// WithRetry sets how a read is asked again.\nfunc WithRetry(retry client.Retry) Option { return client.WithRetry(retry) }\n"},
	{"WithCookies", "// WithCookies has the client keep the cookies a server gives it.\nfunc WithCookies(jar http.CookieJar) Option { return client.WithCookies(jar) }\n"},
	{"StatusCode", "// StatusCode is the status of an error for a status the operation does not document, and 0 for any other error.\nfunc StatusCode(err error) int { return client.StatusCode(err) }\n"},
	{"Messages", "// Messages is what the server said went wrong, one message to an entry.\nfunc Messages(err error) []string { return client.Messages(err) }\n"},
	{"IsNotFound", "// IsNotFound reports whether an error is a 404.\nfunc IsNotFound(err error) bool { return client.IsNotFound(err) }\n"},
	{"IsUnauthorized", "// IsUnauthorized reports whether an error is a 401.\nfunc IsUnauthorized(err error) bool { return client.IsUnauthorized(err) }\n"},
	{"IsForbidden", "// IsForbidden reports whether an error is a 403.\nfunc IsForbidden(err error) bool { return client.IsForbidden(err) }\n"},
	{"IsBadRequest", "// IsBadRequest reports whether an error is a 400 or a 422: what was sent was refused as malformed or not allowed.\nfunc IsBadRequest(err error) bool { return client.IsBadRequest(err) }\n"},
	{"IsConflict", "// IsConflict reports whether an error is a 409: a change that clashes with what the server holds.\nfunc IsConflict(err error) bool { return client.IsConflict(err) }\n"},
	{"IsTooManyRequests", "// IsTooManyRequests reports whether an error is a 429.\nfunc IsTooManyRequests(err error) bool { return client.IsTooManyRequests(err) }\n"},
	{"IsServerError", "// IsServerError reports whether an error is any 5xx: the server failing on its side.\nfunc IsServerError(err error) bool { return client.IsServerError(err) }\n"},
	{"WasNotFound", "// WasNotFound reports whether a response is a 404.\nfunc WasNotFound(resp *http.Response) bool { return client.WasNotFound(resp) }\n"},
}

// helpersFile offers the base client's options and error helpers under the
// types package's name. One whose name a model or enum of the document
// takes is left out, and said so.
func (g *gen) helpersFile() string {
	var b strings.Builder
	var taken []string
	for _, h := range helpers {
		if _, ok := g.declared[h.name]; ok {
			taken = append(taken, h.name)

			continue
		}
		g.declare(h.name, "helpers.go")
		b.WriteString(h.decl)
		b.WriteString("\n")
	}
	if len(taken) > 0 {
		b.WriteString("// The document declares " + strings.Join(taken, ", ") + ", so the base client's are not offered under those names here: use the base client's own.\n")
	}

	return g.fileHeader(b.String())
}

func (g *gen) groupDocFile(grp *definitions.Group) string {
	return fmt.Sprintf(`// Package %[1]s is a typed client for the %[2]s operations of %[3]s %[4]s, generated by pandorest
// from the definitions in %[5]s. What it takes and answers is declared in
// %[6]s; a caller reaches these operations through client.Client.%[2]s and
// names nothing of this package.
package %[1]s
`, g.pkg, grp.Name, g.svc.Title, g.svc.APIVersion, g.opts.Definitions, g.svc.Package)
}

func (g *gen) groupClientFile(grp *definitions.Group) string {
	if g.calls() {
		return g.callGroupClientFile(grp)
	}

	return g.fileHeader(fmt.Sprintf(`// Client is the %[1]s operations of %[2]s; each is a method. Requests go
// through Client.Client, the base client shared with every other tag's.
type Client struct {
	Client *client.Client
}

// NewClient makes the client of these operations on a base client, which
// %[3]s.New makes and client.New wraps for every tag at once.
func NewClient(base *client.Client) Client { return Client{Client: base} }
`, grp.Name, g.svc.Title, g.svc.Package))
}

func (g *gen) aggregateDocFile() string {
	return fmt.Sprintf(`// Package %[5]s is a typed client for %[1]s %[2]s as a whole, generated by pandorest
// from the definitions in %[3]s: New, and a Client with the operations of
// each tag on a field of its own. The types are %[4]s's.
package %[5]s
`, g.svc.Title, g.svc.APIVersion, g.opts.Definitions, g.svc.Package, g.clientPackage())
}

// groupsWithOperations lists the groups that have a package, in order.
func (g *gen) groupsWithOperations() []*definitions.Group {
	var out []*definitions.Group
	for gi := range g.svc.Groups {
		if _, ok := g.pkgs[g.svc.Groups[gi].Name]; ok {
			out = append(out, &g.svc.Groups[gi])
		}
	}

	return out
}

func (g *gen) aggregateFile() string {
	if g.calls() {
		return g.callAggregateFile()
	}
	g.declare("Client", clientFileName)
	g.declare("New", clientFileName)
	g.declare("NewClient", clientFileName)
	g.imports = []importSpec{{g.opts.ClientImport, "base"}}
	groups := g.groupsWithOperations()
	for _, grp := range groups {
		g.imports = append(g.imports, importSpec{path.Join(g.opts.ImportPath, g.pkgs[grp.Name]), g.pkgs[grp.Name]})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "// Client is a client for %s: the operations of each tag on a field of its\n// own, every one sending through Client, the shared base client.\ntype Client struct {\n\tClient *base.Client\n", g.svc.Title)
	for _, grp := range groups {
		fmt.Fprintf(&b, "\t%s %s.Client\n", grp.Name, g.pkgs[grp.Name])
	}
	b.WriteString("}\n\n")

	argument, _ := g.credential()
	doc := g.opts.NewDoc
	if doc == "" {
		doc = fmt.Sprintf(defaultNewDoc, g.svc.Title, argument)
	}
	b.WriteString(comment("", doc))
	fmt.Fprintf(&b, "func New(baseURL, %[1]s string, opts ...base.Option) (*Client, error) {\n\tc, err := %[2]s.New(baseURL, %[1]s, opts...)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\n\treturn NewClient(c), nil\n}\n\n", argument, g.svc.Package)

	fmt.Fprintf(&b, "// NewClient makes a Client of every tag's operations on a base client already\n// made, which %s.New makes.\nfunc NewClient(c *base.Client) *Client {\n\treturn &Client{\n\t\tClient: c,\n", g.svc.Package)
	for _, grp := range groups {
		fmt.Fprintf(&b, "\t\t%s: %s.NewClient(c),\n", grp.Name, g.pkgs[grp.Name])
	}
	b.WriteString("\t}\n}\n")

	return g.fileHeader(b.String())
}

func (g *gen) aggregateTestFile() string {
	if g.calls() {
		return g.callAggregateTestFile()
	}
	g.imports = nil
	groups := g.groupsWithOperations()
	shared := make([]string, 0, len(groups))
	for _, grp := range groups {
		shared = append(shared, "c."+grp.Name+".Client != c.Client")
	}

	return g.fileHeader(fmt.Sprintf(`func TestNew(t *testing.T) {
	t.Parallel()

	if _, err := New("http://server", ""); err == nil {
		t.Error("New without a credential succeeded")
	}
	if _, err := New("server", "test-token"); err == nil {
		t.Error("New with a server url that has no scheme succeeded")
	}
	c, err := New("http://server", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	// every tag's client sends through the one base client
	if %s {
		t.Error("a tag's client does not share the base client")
	}
}
`, strings.Join(shared, " || ")))
}

// groupsJSON is the map a caller moving from the flat layout rewrites by:
// each method's tag, the pagers included, and each tag's package and field.
func (g *gen) groupsJSON() string {
	type group struct {
		Package string `json:"Package"`
		Field   string `json:"Field"`
	}
	out := struct {
		Package    string            `json:"Package"`
		ImportPath string            `json:"ImportPath"`
		Client     string            `json:"Client"`
		Groups     map[string]group  `json:"Groups"`
		Methods    map[string]string `json:"Methods"`
	}{g.svc.Package, g.rootImport().path, path.Join(g.opts.ImportPath, g.clientPackage()), map[string]group{}, map[string]string{}}
	for _, grp := range g.groupsWithOperations() {
		out.Groups[grp.Name] = group{g.pkgs[grp.Name], grp.Name}
		for oi := range grp.Operations {
			out.Methods[grp.Operations[oi].Name] = grp.Name
			if grp.Operations[oi].Pageable != nil {
				out.Methods[grp.Operations[oi].Name+"Complete"] = grp.Name
			}
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")

	return string(b) + "\n"
}

// clientPackage is the name, and the directory, of the package holding New
// and the Client with a field per tag: client, or what the config says for a
// repository whose files import pandorest's own client beside it.
func (g *gen) clientPackage() string {
	return cmpOr(g.opts.ClientPackage, "client")
}
