// Package config describes the services pandorest imports and generates, the
// equivalent of Pandora's resource-manager.hcl. A repository that generates
// an SDK lists its services as Service values and hands them to pandorest.Run;
// paths are relative to the repository root, which is where its make targets
// run the command from.
package config

import (
	"cmp"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// DefaultDir is the directory a service's documents and definitions live in
// when its Dir is empty.
const DefaultDir = "api-defs"

// DefaultClient is the base client a service's generated package is built on
// when its ClientImport is empty: pandorest's own.
const DefaultClient = "github.com/katbyte/pandorest/client"

// Naming is how operations get their Go method names.
type Naming string

const (
	// PathNaming builds names from the HTTP method and path, for documents
	// whose operationIds are machine-made and lossy (Emby's
	// getAudiocodecs, and duplicates).
	PathNaming Naming = "path"
	// OperationIDNaming uses the operationId (Jellyfin's are hand-written
	// and unique).
	OperationIDNaming Naming = "operationId"
)

// Document is what kind of document a service vendors.
type Document string

const (
	// OpenAPIDocument is an OpenAPI 3 document, which is the default.
	OpenAPIDocument Document = "openapi"
	// JSONRPCDocument is a server's description of its JSON-RPC methods, as the jsonrpc package reads it.
	JSONRPCDocument Document = "jsonrpc"
)

// ID is one kind of id. Name is the type's (SeriesID). Of names the models
// the kind is the id of (SeriesResource): their Id is the kind, so is every
// bare path {id} whose operation answers or takes one of them, and the base
// type, int or string, is read from that Id. Base names it for a kind with
// no model of its own, a foreign id (TvdbID: "int"). Names are the
// parameters and properties that always mean the kind wherever they sit
// (seriesId, seriesIds, tags), matched without regard to case. In names the
// places the inference cannot settle, in the form a workaround's In takes
// ("GET /api/v3/series/{id} id", "SeriesResource .parentId"), and Except
// the places of a Name that are something else there. Joined names, by
// name and not by place, the query parameters that take the kind as one
// comma-joined string (Emby's Ids), held in Go as a list of the kind; it
// reaches parameters alone, so a field of the name is not claimed by it,
// and an entry that reaches none is refused. A typed id on a path the document types
// as text is formatted by the method, so the caller passes the kind. Shape
// is a regular expression every value of the kind matches, over its text
// (Emby's "^[0-9a-f]{32}$"): a method refuses a path parameter or an option
// that does not, before any request is sent, with an error naming the value,
// where a server might answer a malformed id with a bare 500; Example is a
// value of the shape, which the generated tests send, and is required with
// it.
type ID struct {
	Name    string
	Of      []string
	Base    string
	Names   []string
	In      []string
	Except  []string
	Joined  []string
	Shape   string
	Example string
}

// Dates says how a server writes a date: Send is the layout a date goes out
// in, which is the one the server itself writes (Emby's
// "2006-01-02T15:04:05.0000000Z07:00", TMDB's "2006-01-02"), and Read lists
// any others it is read in as well (time.RFC3339 for a date with fewer
// digits, "2006-01-02T15:04:05" for one with no zone). A date in no layout
// fails the decode naming it and the layouts, so a layout a server adds is
// found by the live tests and not by a zero date; KeepUnread instead reads
// it as zero and keeps its text on the value (Unread), for a caller that
// would rather lose one date than a list of ten thousand items, and the
// sweep still fails an operation whose answer carried one.
type Dates struct {
	Send       string
	Read       []string
	KeepUnread bool
}

// Layouts are every layout a date is read in, Send first.
func (d Dates) Layouts() []string {
	if d.Send == "" {
		return nil
	}

	return append([]string{d.Send}, d.Read...)
}

// Paging says how a service's list operations page, which is how the importer
// tells them from the rest and what the generator builds each one's Complete
// method on: a GET that takes both parameters and answers a model with both
// properties is a list. Names are matched without regard to case.
type Paging struct {
	// Start and Limit are the query parameters: where to start and how many
	// to answer. Start is the index of the first result wanted, from 0
	// (Emby's StartIndex and Limit), unless ByPage says it is a page. Limit
	// may be empty for a service whose page size is the server's to choose
	// (TMDB's page of twenty); the pager then takes the first page's size
	// as the page size, and ByPage must be set.
	Start string
	Limit string
	// Items and Total are the properties of the answer: the page's results
	// and how many there are in all (Emby's Items and TotalRecordCount).
	// Items may be empty for a service that names the list differently on
	// each answer (Dockhand's logs, events, findings): the answer's one
	// list property is then the items, and an answer with two is no list.
	Items string
	Total string
	// ByPage says Start is a page number, counted from 1, rather than an
	// index: Sonarr's page and pageSize, answering records and
	// totalRecords.
	ByPage bool
	// FirstPage is the number of the first page, for a service whose pages count from 0 (Audiobookshelf's page and limit): FirstPage: new(0).
	// Unset is 1. It needs ByPage.
	FirstPage *int
}

// Service is one server API.
//
// Its document and definitions are versioned by the document's own version
// string: <dir>/<name>-openapi-<version>.json is imported into
// <dir>/<name>-<version>/ beside it, and Resolve picks the highest version
// present, which is the one the SDK is generated from. An older document can
// stay beside it for the diff, and a newer one is picked up by being added.
// A service that wants an SDK of every version it vendors, each a package of
// its own that nothing ties to the others, says where each goes in Outputs.
type Service struct {
	// Name is the -service flag and the prefix of the document and
	// definitions names.
	Name string
	// Package is the Go package name of the generated SDK.
	Package string
	// Output is the generated package directory, for a service generated
	// from its highest version alone.
	Output string
	// Outputs, for a service generated from every version it vendors, is
	// where each goes: the document's version to its package directory
	// ("4.10.0.40": "sdk/emby/v1"). Every version listed must be vendored
	// and every document vendored must be listed, or the import fails
	// naming it; Output is then not used. The packages are independent of
	// one another, so a caller can import two side by side.
	Outputs map[string]string
	// Dir is the directory the documents and definitions are in; empty is
	// DefaultDir.
	Dir string
	// Document is what kind of document the service vendors: empty, or OpenAPIDocument, is an OpenAPI document named
	// <name>-openapi-<version>.json; JSONRPCDocument is a server's description of its JSON-RPC methods (TrueNAS's answer to
	// core.get_methods) named <name>-methods-<version>.json, from which each method becomes a call with its arguments in
	// order, grouped by its namespace, and the package takes a client.Caller the repository writes over its connection.
	Document Document
	// ReadRole, for a JSON-RPC service, names the role a method must allow for it to count as a read, which the sweep's read-only
	// run and a caller that changes nothing go by (TrueNAS's READONLY_ADMIN); Reads names the methods that are reads without the
	// role, and NotReads those that carry it and are not, each by the method's name as the server takes it. A name in either
	// that is no method of the document fails the import.
	ReadRole string
	Reads    []string
	NotReads []string
	// TypedUnions makes a value that is one of several records a type of its own, naming its members, which the caller asks
	// for one of (As<Member>) and tells apart by the field the records name themselves by where the document says which;
	// without it such a value is raw JSON for the caller to decode.
	TypedUnions bool
	// Naming picks how method names are built.
	Naming Naming
	// TagSuffix is trimmed from tag names to make group names ("Service"
	// on Emby: LibraryService is the Library group).
	TagSuffix string
	// PathPrefix is trimmed from a path before a method is named after it,
	// so that every Sonarr operation is not named GetApiV3...: with /api/v3,
	// GET /api/v3/series/{id} is GetSeriesById. A path outside the prefix is
	// named in full, and the paths themselves keep it.
	PathPrefix string
	// Words spells the run-together words of the paths, which name methods
	// with one capital otherwise: Sonarr's /api/v3/episodefile would make
	// GetEpisodefile, and with "episodefile": "EpisodeFile" makes
	// GetEpisodeFile. Keys are lower case and each value is its key's
	// letters, or the import fails; a segment not listed is capitalised as
	// it stands.
	Words map[string]string
	// ClientImport is the import path of the base client the generated
	// package sends every request through; empty is DefaultClient. A
	// repository that keeps a base client of its own names it here, and it
	// must have what generated code calls (see the README).
	ClientImport string
	// Auth names how the service authenticates, and is recorded in its
	// definitions. The client.go the generator writes by default wraps the
	// token New is given in the base client's authorizer of this name,
	// client.<Auth>(token): APIKey or Bearer on pandorest's own.
	Auth string
	// ClientService names a client.Service the repository declares by hand
	// in the generated package, in a file of its own beside the generated
	// ones: what is the service's own about being talked to. The client.go
	// the generator writes by default makes its client from it; without one
	// the client is for a server of no particular kind.
	ClientService string
	// Credential is what the service calls the credential New is given, as
	// a caller would read it: "API key". The client.go the generator writes
	// by default names New's parameter after it (apiKey) and refuses an
	// empty one in its words ("API key is required"); empty is a token.
	Credential string
	// NewDoc is the documentation of the New the generator writes by
	// default, in place of its own: what the address is on this service,
	// where its credential is found. It starts "New returns", and its lines
	// are kept as they are broken.
	NewDoc string
	// Client, when set, is the Go source of client.go after its APIVersion
	// constant, in place of the default: the Client type, a struct holding
	// the base client in a field named Client, and New(baseURL, token
	// string) (*Client, error), with whatever else the service's client
	// declares (a hosted API's address, say). It refers to the base client
	// as client and declares no imports: the generator writes them, for
	// the packages generated code uses (errors, fmt, strings, strconv,
	// net/http, net/url, io, encoding/json, context, slices), so a source
	// that needs another does not compile. The generated tests hold New to
	// refusing an empty token and an address with no scheme, to leaving
	// the slash off the end of the address it keeps, and to naming the
	// credential a secret, client.WithSecrets(token) among the options it
	// passes on, so that no error repeats it.
	Client string
	// Grouped lays the SDK out as packages, the way go-azure-sdk lays out a
	// service: the package at Output holds every model, enum, options and
	// response type, which is all a caller names; each tag's operations are
	// a package under it, with a Client of their own; and Output/client
	// holds New and a Client with a field for each tag. Off, the SDK is one
	// package of every file. Packages names a tag's package where its
	// lowercased name will not do (a Go keyword, a standard library
	// package, client, the SDK's own name): "Package": "packages". A tag
	// it names that the document lacks fails the import. Module is the
	// repository's module path, which Output's import path is under and the
	// packages import one another by; left empty it is read from go.mod.
	Grouped  bool
	Packages map[string]string
	Module   string
	// ClientPackage names the grouped layout's client package, the one
	// with New and the Client with a field per tag: client when empty. A
	// repository whose files import pandorest's own client beside it may
	// name it to spare the alias (embyclient); most need not.
	ClientPackage string
	// Notes is a paragraph added to the generated package's documentation,
	// for what is true of the service's every operation and no document
	// says: how it writes its dates, say.
	Notes string
	// Paging says how the service's lists page; nil is a service whose
	// operations get no Complete method. Pagings are further shapes, for a
	// service whose lists do not all page alike (Dockhand's offset and
	// limit on some, page and pageSize on others), tried in order after
	// Paging. The import names each operation that takes a Start one of
	// them names and matches none, and why.
	Paging  *Paging
	Pagings []Paging
	// PreferJSON reads a response that lists JSON beside another media type
	// as JSON: Swashbuckle lists application/json, text/json and text/plain
	// for every action ASP.NET's formatters can answer, and the text/plain
	// there is the same JSON. Without it such a response is a file, which
	// is what text/css listed beside JSON is on Jellyfin.
	PreferJSON bool
	// ExpectSameShape counts a status outside 2xx as one an operation
	// expects when the document gives it the success answer's shape: a
	// failure that still answers in full, as Sonarr's test-all answers 400
	// with every provider's result when one of them fails. Without it only
	// the 2xx statuses an operation declares are expected.
	ExpectSameShape bool
	// SendRequired holds the document to its word on required properties: a
	// property a schema lists as required is always sent, an empty string
	// as "" rather than not at all (Radarr refuses host settings saved
	// without theirs). Without it required is not read.
	SendRequired bool
	// WrittenWhole names the object schemas callers read and write back
	// whole - settings objects - whose empty strings and zero numbers the
	// generated models send rather than leave out: the server reads a field
	// left out as null, which is not the value that was read. A name the
	// document does not have fails the import.
	WrittenWhole []string
	// KeepNull names the model fields, as Schema.property, or as
	// Model.property for a record the document declares inline and the
	// importer names (Dockhand's PostRegistriesTestAnswer), whose zero is
	// not their absence, which the SDK holds as pointers where it holds
	// every other number and string by value. A number: season 0 is the
	// specials and a parental limit of 0 is the strictest, so a season or a
	// limit the server leaves out must not read as either. A string: a
	// server that keeps whatever a write leaves out and clears a field only
	// when it is sent empty needs "" to go out, where a string by value is
	// left out when empty. A boolean: in a model only ever answered it is
	// held by value, false when the server leaves it out, which is what
	// every caller read it as; in a model a request body reaches it keeps
	// the pointer unless the model is written whole, since by value it is
	// always sent. KeepNull keeps the pointer anywhere.
	// Each must be a boolean, a string, or a number the document declares
	// nullable, or the import fails naming it.
	KeepNull []string
	// Dates and Days say how the server writes a date-time and a date,
	// which the document declares but never spells out. Set, every such
	// field, option and path parameter is the package's Time or Day, a
	// time.Time held by value with its zero left out of a body and a query;
	// unset, dates stay the strings the document declares, for the caller
	// to parse.
	Dates Dates
	Days  Dates
	// IDs are the kinds of id the SDK tells apart, each a named type over
	// the plain int or string the document declares (type SeriesID int), so
	// that the compiler refuses a series id where an episode id goes. Every
	// id not in a kind stays as the document has it; Plain names the
	// id-looking ones left so on purpose, by name wherever they sit or by
	// a place as In takes it ("SeasonPassSeriesResource .id"), which quiets
	// the import's log of the ones no kind covers. An id among an
	// operation's options is held by value once typed, so 0 or "" is not
	// sent: a server that mints 0 as an id would need its kind kept plain.
	// Off, nothing changes.
	IDs   []ID
	Plain []string
	// InlineSingleOptions has an operation with one query or header
	// parameter take it as an argument in place of the options struct:
	// GetMovieLookup(ctx, term), or GetMovieLookup(ctx) to send nothing,
	// since an optional one is variadic and a required one plain; a boolean
	// or a number given is sent, false and 0 included, and a list is given
	// as its elements. An operation with more keeps the struct, as does a
	// list that pages, and a document that adds a second parameter changes
	// the operation's shape at the next generation.
	InlineSingleOptions bool

	// Version, Spec and Definitions are set by Resolve: the document's
	// version, the vendored OpenAPI document, and where the importer writes
	// and the generator reads, the last two relative to the repository.
	Version     string
	Spec        string
	Definitions string

	// Root is the repository root the paths above are relative to; empty
	// is the working directory.
	Root string
}

// Select returns the services named in a comma-separated list, or all of them
// for an empty one.
func Select(services []Service, names string) ([]Service, error) {
	if names == "" {
		return services, nil
	}
	var out []Service
	for name := range strings.SplitSeq(names, ",") {
		svc, ok := Find(services, strings.TrimSpace(name))
		if !ok {
			return nil, fmt.Errorf("unknown service %q (have %s)", name, strings.Join(Names(services), ", "))
		}
		out = append(out, svc)
	}

	return out, nil
}

// Find returns a service by name.
func Find(services []Service, name string) (Service, bool) {
	for _, s := range services {
		if s.Name == name {
			return s, true
		}
	}

	return Service{}, false
}

// Names lists the services' names, in order.
func Names(services []Service) []string {
	out := make([]string, 0, len(services))
	for _, s := range services {
		out = append(out, s.Name)
	}

	return out
}

// In returns a copy of the service whose files are under root. The paths
// stay as configured, relative to the repository, because they are recorded
// in the definitions and the generated docs; Path resolves them.
func (s Service) In(root string) Service {
	s.Root = root

	return s
}

// Path resolves one of the service's paths under its root.
func (s Service) Path(p string) string { return filepath.Join(s.Root, p) }

// Resolve finds the service's documents under its directory and settles on
// the highest version, taking its package directory from Outputs when the
// service has them: Spec is that document and Definitions its definitions
// directory. A service with no document is an error.
func (s Service) Resolve() (Service, error) {
	versions, err := s.Versions()
	if err != nil {
		return s, err
	}
	if len(versions) == 0 {
		return s, fmt.Errorf("%s: no document %s under %q", s.Name, filepath.ToSlash(s.SpecPath("<version>")), cmp.Or(s.Root, "."))
	}
	if err := s.checkVersions(versions); err != nil {
		return s, err
	}

	return s.at(versions[len(versions)-1]), nil
}

// ResolveAll resolves the service once per version it generates: every
// version it vendors for one with Outputs, and the highest alone otherwise.
func (s Service) ResolveAll() ([]Service, error) {
	if len(s.Outputs) == 0 {
		resolved, err := s.Resolve()
		if err != nil {
			return nil, err
		}

		return []Service{resolved}, nil
	}

	versions, err := s.Versions()
	if err != nil {
		return nil, err
	}
	if err := s.checkVersions(versions); err != nil {
		return nil, err
	}
	all := make([]Service, 0, len(versions))
	for _, version := range versions {
		all = append(all, s.at(version))
	}

	return all, nil
}

// at is the service resolved to one of its versions.
func (s Service) at(version string) Service {
	s.Version = version
	s.Spec = s.SpecPath(version)
	s.Definitions = s.DefinitionsPath(version)
	if dir, ok := s.Outputs[version]; ok {
		s.Output = dir
	}

	return s
}

// checkVersions holds a service's Outputs to its vendored documents: each
// names the other, and Output is not set beside them.
func (s Service) checkVersions(vendored []string) error {
	if len(s.Outputs) == 0 {
		return nil
	}
	if s.Output != "" {
		return fmt.Errorf("%s: Output and Outputs are both set: a service generated per version says where each goes in Outputs alone", s.Name)
	}
	for _, version := range slices.Sorted(maps.Keys(s.Outputs)) {
		if !slices.Contains(vendored, version) {
			return fmt.Errorf("%s: Outputs names %s, and no document %s is vendored", s.Name, version, filepath.ToSlash(s.SpecPath(version)))
		}
	}
	for _, version := range vendored {
		if _, ok := s.Outputs[version]; !ok {
			return fmt.Errorf("%s: %s is vendored and Outputs does not say where its package goes", s.Name, filepath.ToSlash(s.SpecPath(version)))
		}
	}

	return nil
}

// Versions lists the versions of the service's vendored documents, lowest
// first.
func (s Service) Versions() ([]string, error) {
	matches, err := filepath.Glob(s.Path(s.SpecPath("*")))
	if err != nil {
		return nil, err
	}
	versions := make([]string, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		versions = append(versions, strings.TrimSuffix(strings.TrimPrefix(base, s.Name+"-"+s.documentWord()+"-"), ".json"))
	}
	slices.SortFunc(versions, CompareVersions)

	return versions, nil
}

// SpecPath is where the service's document of one version is vendored.
func (s Service) SpecPath(version string) string {
	return filepath.Join(cmp.Or(s.Dir, DefaultDir), s.Name+"-"+s.documentWord()+"-"+version+".json")
}

// documentWord is the word in a vendored document's name that says what kind it is.
func (s Service) documentWord() string {
	if s.Document == JSONRPCDocument {
		return "methods"
	}

	return "openapi"
}

// DefinitionsPath is where one version's definitions are checked in.
func (s Service) DefinitionsPath(version string) string {
	return filepath.Join(cmp.Or(s.Dir, DefaultDir), s.Name+"-"+version)
}

// CompareVersions orders two version strings by their dot-separated parts,
// numerically where both parts are numbers (4.10 above 4.9), otherwise as
// text; a version with more parts is above its prefix.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		an, aErr := strconv.Atoi(as[i])
		bn, bErr := strconv.Atoi(bs[i])
		var c int
		if aErr == nil && bErr == nil {
			c = cmp.Compare(an, bn)
		} else {
			c = strings.Compare(as[i], bs[i])
		}
		if c != 0 {
			return c
		}
	}

	return cmp.Compare(len(as), len(bs))
}
