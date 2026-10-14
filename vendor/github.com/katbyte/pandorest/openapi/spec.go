// Package openapi decodes the subset of an OpenAPI 3 document the importer
// reads. Anything not listed here (servers, security, examples, xml hints) is
// ignored. The types are mutable on purpose: workarounds patch a loaded
// document before it is normalised into definitions.
package openapi

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode"
)

// Spec is an OpenAPI 3 document.
type Spec struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]*PathItem `json:"paths"`
	Components struct {
		Schemas map[string]*Schema `json:"schemas"`
	} `json:"components"`
}

// PathItem holds the operations of one path. HEAD and OPTIONS are not
// imported: the servers use them only for cache probes and CORS.
type PathItem struct {
	Get    *Operation `json:"get"`
	Post   *Operation `json:"post"`
	Put    *Operation `json:"put"`
	Delete *Operation `json:"delete"`
	Patch  *Operation `json:"patch"`
	// Call is the one operation of a path that is a JSON-RPC method rather than a URL ("pool.query"), which a method description
	// loaded by the jsonrpc package makes; no OpenAPI document carries it.
	Call *Operation `json:"x-call,omitempty"`
}

// Operation is one HTTP method on one path.
type Operation struct {
	OperationID string               `json:"operationId"`
	Tags        []string             `json:"tags"`
	Summary     string               `json:"summary"`
	Description string               `json:"description"`
	Deprecated  bool                 `json:"deprecated"`
	Parameters  []*Parameter         `json:"parameters"`
	RequestBody *RequestBody         `json:"requestBody"`
	Responses   map[string]*Response `json:"responses"`

	// Name is the Go method a workaround gives the operation, in place of
	// the one its path or its operationId would make: how two operations
	// that make the same name are told apart. No document carries it.
	Name string `json:"-"`
	// Commands is set by a workaround on an operation that queues a command named in its body, and says which commands there
	// are and how a queued one is followed. No document carries it.
	Commands *CommandSet `json:"-"`
	// Call is what a JSON-RPC method says of itself beyond its parameters and answer, set by the jsonrpc package on an operation
	// under a path's Call; its parameters are In "call", in the order the method takes them.
	Call *CallInfo `json:"x-call,omitempty"`
}

// CallInfo is what a JSON-RPC method description says of a method beside what it takes and answers: Method is its name as the
// server takes it, Job that it runs as a job, Filterable that it is a query taking filters and options, Item that it acts on one
// record by its id, NoAuth that it may be called before logging in, Downloadable and Uploadable that its bytes move outside the
// call, and Roles the roles that may call it, as the server names them.
type CallInfo struct {
	Method       string   `json:"method"`
	Job          bool     `json:"job,omitempty"`
	Filterable   bool     `json:"filterable,omitempty"`
	Item         bool     `json:"item,omitempty"`
	NoAuth       bool     `json:"noAuth,omitempty"`
	Downloadable bool     `json:"downloadable,omitempty"`
	Uploadable   bool     `json:"uploadable,omitempty"`
	Roles        []string `json:"roles,omitempty"`
}

// CommandSet is how a server that queues commands by name through one operation is followed: Get is the operation that answers a
// queued command by its id ("GET /api/v3/command/{id}"), Status the field of its answer that says how far it is, Finished the
// values that mean it is over and Failed those among them that mean it failed, and Commands the schema each command's body is, by
// the command's name as the server takes it.
type CommandSet struct {
	Get      string
	Status   string
	Finished []string
	Failed   []string
	Commands map[string]string
}

// Parameter is a path, query or header parameter.
type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Description string  `json:"description"`
	Required    bool    `json:"required"`
	Deprecated  bool    `json:"deprecated"`
	Style       string  `json:"style"`
	Explode     *bool   `json:"explode"`
	Schema      *Schema `json:"schema"`
	// Added is set for a parameter a workaround added, which no document
	// can say of its own: the importer keeps an added header named Accept,
	// where it ignores a document's as OpenAPI says to.
	Added bool `json:"-"`
	// Field is the Go field a workaround chose for the parameter, when the
	// name's own would clash with another parameter of the same name
	// (a list added beside the string it joins); "" is the name's own.
	Field string `json:"-"`
}

// Exploded reports whether an array parameter is sent as one key per value
// rather than comma-separated: OpenAPI's default for a query parameter
// (style form, explode true), but not for a header (style simple, explode
// false).
func (p *Parameter) Exploded() bool {
	if p.Explode != nil {
		return *p.Explode
	}
	if p.In == InHeader {
		return p.Style == "form"
	}

	return p.Style == "" || p.Style == "form"
}

// DeepObject reports whether an object parameter is sent as one key per
// field, name[field]=value, rather than as one JSON string.
func (p *Parameter) DeepObject() bool { return p.Style == "deepObject" }

// Parameter locations.
const (
	InPath   = "path"
	InQuery  = "query"
	InHeader = "header"
	// InCall is a JSON-RPC method's parameter, one of the values sent in order; no OpenAPI document declares one.
	InCall = "call"
)

// RequestBody maps media types to their schema.
type RequestBody struct {
	Required bool                  `json:"required"`
	Content  map[string]*MediaType `json:"content"`
}

// MediaType is the schema of one media type of a body or response.
type MediaType struct {
	Schema   *Schema             `json:"schema"`
	Examples map[string]*Example `json:"examples"`

	// Text is set by a workaround for an answer that is short text and not
	// JSON, to be read whole into a string where an answer of another type
	// than JSON is otherwise a file left for its caller to read. No document
	// carries it: a log file is text too, and is not to be held in memory.
	Text bool `json:"-"`
}

// JSONMedia reports whether a media type is JSON: application/json, text/json
// or one that ends +json, whatever its parameters.
func JSONMedia(mediaType string) bool {
	base := strings.TrimSpace(strings.SplitN(strings.ToLower(mediaType), ";", 2)[0])

	return base == "application/json" || base == "text/json" || strings.HasSuffix(base, "+json")
}

// PickJSON returns the JSON media type of a body or response's content: plain
// application/json when listed, else the first JSON type.
func PickJSON(content map[string]*MediaType) (string, bool) {
	if _, ok := content["application/json"]; ok {
		return "application/json", true
	}
	for _, mediaType := range SortedKeys(content) {
		if JSONMedia(mediaType) {
			return mediaType, true
		}
	}

	return "", false
}

// Example is one named example of a media type's content. TMDB's document
// keeps the only true shape of its request bodies here.
type Example struct {
	Value json.RawMessage `json:"value"`
}

// Response is one status code of an operation. A response that is a `$ref`
// into components/responses (Emby's error codes) carries no content, and
// decodes to one with none.
type Response struct {
	Description string                `json:"description"`
	Content     map[string]*MediaType `json:"content"`
}

// Schema is a JSON schema object, as far as the two specs use it.
type Schema struct {
	Ref         string             `json:"$ref,omitempty"`
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	Nullable    bool               `json:"nullable,omitempty"`
	Enum        []json.RawMessage  `json:"enum,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	// Required names the properties an object must carry.
	Required             []string        `json:"required,omitempty"`
	Items                *Schema         `json:"items,omitempty"`
	AdditionalProperties json.RawMessage `json:"additionalProperties,omitempty"`
	AllOf                []*Schema       `json:"allOf,omitempty"`
	OneOf                []*Schema       `json:"oneOf,omitempty"`
	AnyOf                []*Schema       `json:"anyOf,omitempty"`
	// GoType is a Go type a workaround holds the value as in place of what the schema describes, for a value the server writes
	// in a form of its own: a secret hidden behind a placeholder, a list written two ways.
	GoType *GoType `json:"x-go-type,omitempty"`
	// Discriminator says, for a choice of records (oneOf, anyOf), which field the records name themselves by and which value
	// of it names each member schema.
	Discriminator *Discriminator `json:"discriminator,omitempty"`
}

// Discriminator is how a choice of records is told apart: the property every member carries, and each value of it with the
// reference ("#/components/schemas/Name") of the member it names.
type Discriminator struct {
	PropertyName string            `json:"propertyName"`
	Mapping      map[string]string `json:"mapping,omitempty"`
}

// GoType names a Go type as a package would write it (truenasrpc.Secret[int]) and the import path it comes from.
type GoType struct {
	Name   string `json:"name"`
	Import string `json:"import,omitempty"`
}

// SchemaRefPrefix is how a `$ref` names a component schema.
const SchemaRefPrefix = "#/components/schemas/"

// FormatUnixSeconds and FormatUnixMillis are the formats a workaround gives an integer that is a date
// (seconds or milliseconds since 1970), which no document declares.
const (
	FormatUnixSeconds = "unix-seconds"
	FormatUnixMillis  = "unix-milliseconds"
)

// JSON schema type names.
const (
	TypeString  = "string"
	TypeInteger = "integer"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeArray   = "array"
	TypeObject  = "object"
)

// Load reads and decodes an OpenAPI 3 JSON document.
func Load(path string) (*Spec, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the spec path comes from the service config
	if err != nil {
		return nil, err
	}

	return Parse(raw)
}

// Parse decodes an OpenAPI 3 JSON document.
func Parse(raw []byte) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decoding spec: %w", err)
	}
	if !strings.HasPrefix(s.OpenAPI, "3.") {
		return nil, fmt.Errorf("unsupported openapi version %q (want 3.x)", s.OpenAPI)
	}

	return &s, nil
}

// RefName returns the schema name a `$ref` points at, or "" when the schema
// is not a reference. A single-element allOf wrapping a $ref (how Jellyfin
// attaches descriptions and nullable to references) counts as a reference.
func (s *Schema) RefName() string {
	if s == nil {
		return ""
	}
	if s.Ref != "" {
		return strings.TrimPrefix(s.Ref, SchemaRefPrefix)
	}
	if len(s.AllOf) == 1 && s.AllOf[0].Ref != "" {
		return strings.TrimPrefix(s.AllOf[0].Ref, SchemaRefPrefix)
	}

	return ""
}

// Additional returns the additionalProperties schema. ok is false when the
// schema does not allow additional properties; a bare `true` or `{}` yields an
// empty schema.
func (s *Schema) Additional() (sch *Schema, ok bool) {
	raw := strings.TrimSpace(string(s.AdditionalProperties))
	switch raw {
	case "", "false":
		return nil, false
	case "true", "{}":
		return &Schema{}, true
	}
	var a Schema
	if err := json.Unmarshal(s.AdditionalProperties, &a); err != nil {
		// not a schema at all: an untyped map, which is what a strict
		// import would otherwise silently make of it as an empty struct
		return &Schema{}, true
	}

	return &a, true
}

// EnumValues returns the enum members as strings. Both specs only use string
// enums; anything else is rendered through its JSON form.
func (s *Schema) EnumValues() []string {
	out := make([]string, 0, len(s.Enum))
	for _, raw := range s.Enum {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			v = string(raw)
		}
		out = append(out, v)
	}

	return out
}

// IsEnum reports whether a schema defines a string enum type.
func (s *Schema) IsEnum() bool {
	return s != nil && len(s.Enum) > 0 && (s.Type == TypeString || s.Type == "")
}

// IsUnion reports whether a schema is a oneOf/anyOf union, which cannot be
// represented as a struct.
func (s *Schema) IsUnion() bool {
	return s != nil && (len(s.OneOf) > 0 || len(s.AnyOf) > 0)
}

// Method is one operation of a path together with its HTTP method.
type Method struct {
	Method    string // GET, POST, ...
	Operation *Operation
}

// MethodCall is the "method" of a JSON-RPC call among a path's operations, beside GET, POST and the rest: a place names one as
// "CALL pool.query".
const MethodCall = "CALL"

// Methods returns the imported operations of a path in a fixed method order.
func (p *PathItem) Methods() []Method {
	all := []Method{{"GET", p.Get}, {"POST", p.Post}, {"PUT", p.Put}, {"DELETE", p.Delete}, {"PATCH", p.Patch}, {MethodCall, p.Call}}

	return slices.DeleteFunc(all, func(m Method) bool { return m.Operation == nil })
}

// Operation returns the operation for an HTTP method on a path, or nil.
func (s *Spec) Operation(method, path string) *Operation {
	item := s.Paths[path]
	if item == nil {
		return nil
	}
	for _, m := range item.Methods() {
		if strings.EqualFold(m.Method, method) {
			return m.Operation
		}
	}

	return nil
}

// Parameter returns the operation's parameter of that location and name
// (case-insensitive, as both servers bind them), or nil.
func (o *Operation) Parameter(in, name string) *Parameter {
	for _, p := range o.Parameters {
		if p.In == in && strings.EqualFold(p.Name, name) {
			return p
		}
	}

	return nil
}

// SortedKeys returns the keys of a map in sorted order, for deterministic output.
func SortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// EachSchema calls fn for every schema in the document: where is the operation it belongs to, as
// "GET /path", or the component schema's name, and path where it sits, the property names down to
// it joined by dots ("containerDetails.status"), a parameter's name, or "" for one that is not named.
// A list adds nothing to the path: its items sit where it does. It covers the component schemas,
// each operation's parameters, request body and answers, and everything nested in them.
func (s *Spec) EachSchema(fn func(where, path string, schema *Schema)) {
	var walk func(where, path string, schema *Schema)
	walk = func(where, path string, schema *Schema) {
		if schema == nil {
			return
		}
		fn(where, path, schema)
		for _, name := range SortedKeys(schema.Properties) {
			walk(where, PathJoin(path, name), schema.Properties[name])
		}
		walk(where, path, schema.Items)
		for _, list := range [][]*Schema{schema.AllOf, schema.OneOf, schema.AnyOf} {
			for _, nested := range list {
				walk(where, path, nested)
			}
		}
		if additional, ok := schema.Additional(); ok {
			// the value schema is decoded fresh each time, so what fn changed is written back
			walk(where, path, additional)
			if raw, err := json.Marshal(additional); err == nil && string(raw) != "{}" {
				schema.AdditionalProperties = raw
			}
		}
	}
	for _, name := range SortedKeys(s.Components.Schemas) {
		walk(name, "", s.Components.Schemas[name])
	}
	for _, path := range SortedKeys(s.Paths) {
		item := s.Paths[path]
		for method, op := range map[string]*Operation{"GET": item.Get, "POST": item.Post, "PUT": item.Put, "DELETE": item.Delete, "PATCH": item.Patch} {
			if op == nil {
				continue
			}
			where := method + " " + path
			for _, p := range op.Parameters {
				walk(where, p.Name, p.Schema)
			}
			if op.RequestBody != nil {
				for _, mediaType := range SortedKeys(op.RequestBody.Content) {
					walk(where, "", op.RequestBody.Content[mediaType].Schema)
				}
			}
			for _, status := range SortedKeys(op.Responses) {
				for _, mediaType := range SortedKeys(op.Responses[status].Content) {
					walk(where, "", op.Responses[status].Content[mediaType].Schema)
				}
			}
		}
	}
}

// PathJoin is path with name under it: the name alone at the top.
func PathJoin(path, name string) string {
	if path == "" {
		return name
	}

	return path + "." + name
}

// PathName is the last name of a path: the property's own.
func PathName(path string) string {
	if _, name, ok := strings.CutLast(path, "."); ok {
		return name
	}

	return path
}

// Place is one place in a document as a workaround's In or Except names it: an
// operation ("GET /api/stacks") or a component schema ("Container"), and an
// optional path inside it, ".status" for the property at the top alone,
// "containerDetails.status" for the one at that path.
type Place struct {
	Where, Path string
	Top         bool
}

// PlaceOf reads a place as written: the place, then an optional path, a
// leading dot marking the top.
func PlaceOf(entry string) Place {
	entry = strings.TrimSpace(entry)
	i := strings.LastIndexByte(entry, ' ')
	if i < 0 || strings.HasPrefix(entry[i+1:], "/") {
		return Place{Where: entry}
	}
	p := Place{Where: entry[:i], Path: entry[i+1:]}
	if strings.HasPrefix(p.Path, ".") {
		p.Path, p.Top = p.Path[1:], true
	}

	return p
}

// Matches reports whether a schema at where and path is in the place.
func (p Place) Matches(where, path string) bool {
	switch {
	case p.Where != where:
		return false
	case p.Path == "":
		return true
	default:
		return path == p.Path
	}
}

// Camel joins the alphanumeric pieces of s, upper-casing the first letter of
// each piece and keeping the rest as spelled: "QueryResult_BaseItemDto" →
// "QueryResultBaseItemDto", "master.m3u8" → "MasterM3u8", "user_usage_stats"
// → "UserUsageStats", "GetFirstUser_2" → "GetFirstUser2". A leading digit gets
// an "N" prefix so the result is always a valid identifier.
func Camel(s string) string {
	var b strings.Builder
	upNext := true
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upNext = true
			continue
		}
		if upNext {
			r = unicode.ToUpper(r)
			upNext = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if out != "" && unicode.IsDigit(rune(out[0])) {
		out = "N" + out
	}

	return out
}
