// Package definitions is pandorest's normalised description of one server's
// API, the contract between the importer and the generator. The importer
// writes it from an OpenAPI document (after the workarounds have patched the
// document); the generator reads only this, never the document. It is checked
// in under api-defs/<service>-<version>/, one JSON file per spec tag plus
// Service.json, so a spec refresh shows up as a readable diff and the differ
// can report what changed in API terms.
//
// Everything the generator needs is decided here rather than in the
// generator: Go names, Go-facing types, expected status codes, which
// operations page, and which tag owns each model.
package definitions

// Service describes one server's API. It is Service.json; the groups are the
// per-tag files next to it.
type Service struct {
	// Name is the service's name in the pandorest config and the
	// prefix of its api-defs directory, e.g. "emby" for emby-4.10.0.40.
	Name string `json:"Name"`
	// Package is the Go package the generator writes, e.g. "emby".
	Package string `json:"Package"`
	// Title and APIVersion are the document's info block.
	Title      string `json:"Title"`
	APIVersion string `json:"ApiVersion"`
	// Source is the document the definitions were imported from.
	Source string `json:"Source"`
	// Auth names how the service authenticates: the base client's
	// authorizer the default client.go wraps New's token in.
	Auth string `json:"Auth"`
	// Workarounds lists the importer workarounds applied, in order.
	Workarounds []string `json:"Workarounds"`
	// Dates and Days are the layouts the service writes a date and time
	// and a day in, first the one it is sent in; empty, dates are strings.
	Dates []string `json:"Dates,omitempty"`
	Days  []string `json:"Days,omitempty"`
	// DatesKeepUnread and DaysKeepUnread read a date no layout reads as
	// zero with its text kept, rather than failing the decode.
	DatesKeepUnread bool `json:"DatesKeepUnread,omitempty"`
	DaysKeepUnread  bool `json:"DaysKeepUnread,omitempty"`
	// IDs are the kinds of id the package declares types for.
	IDs []IDKind `json:"IDs,omitempty"`

	Groups []Group `json:"-"`
}

// CommonGroup owns the models and constants that more than one tag's
// operations use (or none do).
const CommonGroup = "Common"

// Group is one spec tag: its operations, and the models and constants no
// other tag uses.
type Group struct {
	// Name is the Go-facing name used in file names, e.g. "LibraryStructure".
	Name string `json:"Name"`
	// Tag is the tag as the document spells it, e.g. "LibraryStructureService";
	// empty for the common group.
	Tag        string      `json:"Tag,omitempty"`
	Operations []Operation `json:"Operations,omitempty"`
	Models     []Model     `json:"Models,omitempty"`
	Constants  []Constant  `json:"Constants,omitempty"`
}

// Operation is one HTTP method on one path, and one method on the client.
type Operation struct {
	// Name is the Go method name.
	Name        string `json:"Name"`
	OperationID string `json:"OperationId,omitempty"`
	Method      string `json:"Method"`
	// Path is the document's path template, e.g. /Items/{Id}/Similar.
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
	Deprecated  bool   `json:"Deprecated,omitempty"`
	// PathParameters are the template placeholders in path order.
	PathParameters []PathParameter `json:"PathParameters,omitempty"`
	// Options are the query and header parameters, the options struct.
	Options []Option `json:"Options,omitempty"`
	// Request is the request body; nil when the operation takes none.
	Request *Body `json:"Request,omitempty"`
	// Response is the success response body; nil when there is none.
	Response            *Body `json:"Response,omitempty"`
	ExpectedStatusCodes []int `json:"ExpectedStatusCodes"`
	// Pageable is set for the list operations, which also get a Complete
	// method that pages through every result.
	Pageable *Pageable `json:"Pageable,omitempty"`
	// Commands is set on an operation that queues commands named in its body, and says how a queued one is followed; Command is set
	// on each command's own operation, one per name, which sends the same request with its name fixed and also gets an AndWait.
	Commands *CommandSet `json:"Commands,omitempty"`
	Command  string      `json:"Command,omitempty"`
	// Call is set on an operation that is a JSON-RPC method rather than a request to a path (Method is "CALL" and Path the
	// method's name), and Arguments are what it takes, in the order they are sent; it has no path parameters, options, request
	// body or status codes.
	Call      *Call      `json:"Call,omitempty"`
	Arguments []Argument `json:"Arguments,omitempty"`
}

// Call is what a JSON-RPC method says of itself: Method is its name as the server takes it, Job that the server runs it as a
// job, Filterable that it is a query taking filters and options and answering a list, Item that it acts on one record by its
// id, NoAuth that it may be called before logging in, Read that it changes nothing (worked out from the roles the service config
// names), and Roles the roles that may call it as the server names them.
type Call struct {
	Method     string `json:"Method"`
	Job        bool   `json:"Job,omitempty"`
	Filterable bool   `json:"Filterable,omitempty"`
	// Form is the one-record (QueryGet) or count (QueryCount) form of a query, which the method asks for by setting that flag in
	// the query's options; the plain form, answering the list, has none.
	Form   string   `json:"Form,omitempty"`
	Item   bool     `json:"Item,omitempty"`
	NoAuth bool     `json:"NoAuth,omitempty"`
	Read   bool     `json:"Read,omitempty"`
	Roles  []string `json:"Roles,omitempty"`
	// Downloadable marks a method whose answer is a file, which the generated <Name>To writes out through a connection that
	// can carry one; Uploadable one that takes files beside its arguments, which the generated method takes as readers.
	Downloadable bool `json:"Downloadable,omitempty"`
	Uploadable   bool `json:"Uploadable,omitempty"`
}

// QueryGet and QueryCount are the forms of a query beside the list: the flag in its options that asks for one record, which
// the server refuses to answer when the filters match none, and the one that asks for how many match.
const (
	QueryGet   = "get"
	QueryCount = "count"
)

// Argument is one value a JSON-RPC method takes, by position: Name as the server names it, Argument the Go argument, and
// Required whether the server insists on it; one not required may be left off the end of the call.
type Argument struct {
	Name        string  `json:"Name"`
	Argument    string  `json:"Argument"`
	Type        TypeRef `json:"Type"`
	Required    bool    `json:"Required,omitempty"`
	Description string  `json:"Description,omitempty"`
}

// CommandSet is how a queued command is followed: Get is the Go name of the operation that answers one by its id, Status the field
// of its answer that says how far it is, Finished the values that mean it is over and Failed those among them that mean it failed.
type CommandSet struct {
	Get      string   `json:"Get"`
	Status   string   `json:"Status"`
	Finished []string `json:"Finished"`
	Failed   []string `json:"Failed,omitempty"`
}

// Key identifies an operation independently of its Go name; a command's
// operation shares its method and path with the one that queues it, and is
// told apart by its name.
func (o *Operation) Key() string {
	if o.Command != "" {
		return o.Method + " " + o.Path + " " + o.Command
	}

	return o.Method + " " + o.Path
}

// PathParameter is a placeholder of the path template.
type PathParameter struct {
	// Name is the placeholder as the template spells it.
	Name string `json:"Name"`
	// Argument is the Go argument name.
	Argument string  `json:"Argument"`
	Type     TypeRef `json:"Type"`
}

// Option locations.
const (
	InQuery  = "Query"
	InHeader = "Header"
)

// Option is one query or header parameter, a field of the options struct.
type Option struct {
	// Name is the parameter as sent.
	Name string `json:"Name"`
	// Field is the Go field name.
	Field       string `json:"Field"`
	In          string `json:"In"`
	Description string `json:"Description,omitempty"`
	Deprecated  bool   `json:"Deprecated,omitempty"`
	// Required is set when the document says the operation needs the option.
	Required bool    `json:"Required,omitempty"`
	Type     TypeRef `json:"Type"`
	// CommaSeparated sends a List as one comma-separated value; otherwise
	// each element is sent under the parameter's name.
	CommaSeparated bool `json:"CommaSeparated,omitempty"`
	// DeepObject sends a Dictionary as one parameter per key, name[key]=value
	// (OpenAPI's deepObject style, which Jellyfin's stream options use);
	// otherwise the dictionary goes as one JSON string.
	DeepObject bool `json:"DeepObject,omitempty"`
}

// Body is a request or response body.
type Body struct {
	// ContentType is the media type sent or expected. A wildcard such as
	// image/* means the caller names the concrete type.
	ContentType string `json:"ContentType"`
	// Type is RawFile for bodies that are not JSON, but for an answer that
	// is text read whole, which is a String of another ContentType than
	// JSON's.
	Type TypeRef `json:"Type"`
}

// JSONContentType is the ContentType of every body that is JSON.
const JSONContentType = "application/json"

// Text reports whether the body is an answer of text read whole into a
// string: not JSON, and not a file left for its caller to read.
func (b *Body) Text() bool {
	return b != nil && b.Type.Type == String && b.ContentType != JSONContentType
}

// AsksFor is the media type a call for this answer asks its server for when
// that is not JSON, and empty when it is: a file's type, or a text's.
func (b *Body) AsksFor() string {
	if b == nil || (b.Type.Type != RawFile && !b.Text()) {
		return ""
	}

	return b.ContentType
}

// Pageable says how a list operation pages.
type Pageable struct {
	// StartIndexOption and LimitOption are the options fields: where a page
	// starts and how many results it holds. LimitOption is empty for a
	// service whose page size is the server's to choose; the pager then
	// takes the first page's size as the page size.
	StartIndexOption string `json:"StartIndexOption"`
	LimitOption      string `json:"LimitOption,omitempty"`
	// ByPage is set when the list pages by page number: StartIndexOption is
	// then the page wanted, counted from 1, rather than the index of its
	// first result.
	ByPage bool `json:"ByPage,omitempty"`
	// ItemsField and TotalField are fields of the response model.
	ItemsField string  `json:"ItemsField"`
	TotalField string  `json:"TotalField"`
	ItemType   TypeRef `json:"ItemType"`
}

// Model is an object schema, or a oneOf/anyOf union kept as raw JSON.
type Model struct {
	Name string `json:"Name"`
	// SchemaName is the component schema; empty for an object declared
	// inline in another model's property.
	SchemaName  string  `json:"SchemaName,omitempty"`
	Description string  `json:"Description,omitempty"`
	Fields      []Field `json:"Fields,omitempty"`
	// Union lists the variants when the schema is a oneOf/anyOf; such a
	// model has no fields and is raw JSON for the caller to decode.
	Union []string `json:"Union,omitempty"`
	// Members, for a service with TypedUnions, are the records the union is one of, each a model, with the value of
	// Discriminator that names it where the document says which field the records name themselves by; the generated type
	// holds the record as it came and hands it over as the member asked for.
	Discriminator string   `json:"Discriminator,omitempty"`
	Members       []Member `json:"Members,omitempty"`
	// WrittenWhole marks a model callers read and write back whole - a
	// settings object - so its empty strings and zero numbers are sent
	// rather than left out: the server reads a field left out as null,
	// which is not the value that was read, and not a value it will save.
	WrittenWhole bool `json:"WrittenWhole,omitempty"`
}

// OwnItem is what a named type's name may say in place of the field's own type, or a list's element, as the package names it
// (truenasrpc.Set[$item] on a list of an enum is truenasrpc.Set[ReplicationDirection]), so a type wrapping the field's own
// survives the package renaming it.
const OwnItem = "$item"

// Member is one record a union is one of: the model, and the values of the union's Discriminator that name it, in order,
// none where the document gives none; several where the document sends several kinds to one record (a mirror, a stripe and
// three raidz levels are all one vdev record).
type Member struct {
	Model string   `json:"Model"`
	Kinds []string `json:"Kinds,omitempty"`
}

// Field is one property of a model.
type Field struct {
	Name        string  `json:"Name"`
	JSONName    string  `json:"JsonName"`
	Description string  `json:"Description,omitempty"`
	Type        TypeRef `json:"Type"`
	Nullable    bool    `json:"Nullable,omitempty"`
	// Required is set when the document says an object must carry the
	// property, for a service whose config holds it to that (SendRequired):
	// the generated field is then always sent, its zero included.
	Required bool `json:"Required,omitempty"`
	// KeepsNull makes the generated field a pointer, so the document's null
	// is told from a number's zero, and a string left out from one sent
	// empty: the fields the service config names in KeepNull. Every other
	// number and string is held by value.
	KeepsNull bool `json:"KeepsNull,omitempty"`
}

// Constant is a string enum.
type Constant struct {
	Name        string          `json:"Name"`
	SchemaName  string          `json:"SchemaName"`
	Description string          `json:"Description,omitempty"`
	Values      []ConstantValue `json:"Values"`
}

// ConstantValue is one member of an enum.
type ConstantValue struct {
	// Name is the Go constant name.
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// ObjectType is the kind of a TypeRef.
type ObjectType string

// The object types. Integer is a 32-bit integer (Go int), Integer64 a 64-bit
// one; Float is float32 and Double float64. RawObject is JSON the caller
// decodes (untyped objects, unions, binary strings); Any is a value of no
// declared type; RawFile is a body that is not JSON.
const (
	Boolean    ObjectType = "Boolean"
	Integer    ObjectType = "Integer"
	Integer64  ObjectType = "Integer64"
	Float      ObjectType = "Float"
	Double     ObjectType = "Double"
	String     ObjectType = "String"
	List       ObjectType = "List"
	Dictionary ObjectType = "Dictionary"
	Reference  ObjectType = "Reference"
	RawObject  ObjectType = "RawObject"
	Any        ObjectType = "Any"
	RawFile    ObjectType = "RawFile"
	// DateTime and Date are strings the document declares as a date and
	// time or a day, held as the package's Time and Day when the service
	// gives the layouts it writes them in (Service.Dates, Days).
	DateTime ObjectType = "DateTime"
	Date     ObjectType = "Date"
	// UnixSeconds and UnixMillis are integers a workaround declares a date
	// (seconds or milliseconds since 1970), held as the package's Unix and
	// UnixMilli, which read a number and write one back.
	UnixSeconds ObjectType = "UnixSeconds"
	UnixMillis  ObjectType = "UnixMillis"
	// ID is a kind of id the service tells apart (Service.IDs), named in
	// ReferenceName, held as a named type over its base.
	ID ObjectType = "ID"
	// Named is a value held as a Go type a workaround names (GoType, Import), for a value the server writes in a form of its
	// own that the document cannot describe; the type reads and writes its own JSON.
	Named ObjectType = "Named"
)

// IDKind is one kind of id, a named type over an int, an int64 or a string.
type IDKind struct {
	Name string     `json:"Name"`
	Base ObjectType `json:"Base"`
	// Shape is the regular expression every value of the kind matches, over its text, and Example a value of it; empty for a kind with no shape.
	Shape   string `json:"Shape,omitempty"`
	Example string `json:"Example,omitempty"`
}

// TypeRef is the type of a field, parameter or body.
type TypeRef struct {
	Type ObjectType `json:"Type"`
	// ReferenceName names the model or constant of a Reference.
	ReferenceName string `json:"ReferenceName,omitempty"`
	// NestedItem is the element type of a List or Dictionary.
	NestedItem *TypeRef `json:"NestedItem,omitempty"`
	// GoType is the Go type a Named value is held as, written as a package uses it (truenasrpc.Secret[int]), and Import the
	// path it comes from; the type reads and writes its own JSON.
	GoType string `json:"GoType,omitempty"`
	Import string `json:"Import,omitempty"`
}

// String renders a type for logs and diffs: List[Reference(BaseItemDto)].
func (t TypeRef) String() string {
	switch t.Type {
	case Reference:
		return "Reference(" + t.ReferenceName + ")"
	case Named:
		return "Named(" + t.GoType + ")"
	case List, Dictionary:
		if t.NestedItem == nil {
			return string(t.Type) + "[?]"
		}
		return string(t.Type) + "[" + t.NestedItem.String() + "]"
	default:
		return string(t.Type)
	}
}

// Equal reports whether two types are the same.
func (t TypeRef) Equal(o TypeRef) bool { return t.String() == o.String() }

// Numeric reports whether the type is a number: an integer or a float of
// either size.
func (t TypeRef) Numeric() bool {
	return t.Type == Integer || t.Type == Integer64 || t.Type == Float || t.Type == Double
}

// Operations returns every operation of the service.
func (s *Service) Operations() []*Operation {
	var out []*Operation
	for gi := range s.Groups {
		for oi := range s.Groups[gi].Operations {
			out = append(out, &s.Groups[gi].Operations[oi])
		}
	}

	return out
}

// Models returns every model of the service by name.
func (s *Service) Models() map[string]*Model {
	out := map[string]*Model{}
	for gi := range s.Groups {
		for mi := range s.Groups[gi].Models {
			m := &s.Groups[gi].Models[mi]
			out[m.Name] = m
		}
	}

	return out
}

// Constants returns every constant of the service by name.
func (s *Service) Constants() map[string]*Constant {
	out := map[string]*Constant{}
	for gi := range s.Groups {
		for ci := range s.Groups[gi].Constants {
			c := &s.Groups[gi].Constants[ci]
			out[c.Name] = c
		}
	}

	return out
}

// IDKind is a kind of id by name, or nil for one the service does not declare.
func (s *Service) IDKind(kind string) *IDKind {
	for i := range s.IDs {
		if s.IDs[i].Name == kind {
			return &s.IDs[i]
		}
	}

	return nil
}

// IDBase is the base type of a kind of id, or "" for a kind the service does
// not declare.
func (s *Service) IDBase(kind string) ObjectType {
	for _, k := range s.IDs {
		if k.Name == kind {
			return k.Base
		}
	}

	return ""
}
