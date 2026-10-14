// Package jsonrpc reads a server's own description of its JSON-RPC methods into the document the importer takes, so that a
// server with no OpenAPI document (TrueNAS, whose REST API is going) gets the same generated package as one with.
//
// The description is the answer to TrueNAS's core.get_methods: one object, method name to what the method takes (accepts, one
// JSON Schema per positional parameter in order), what it answers (returns, one schema), the roles that may call it and whether
// it runs as a job. Every schema is written out in full where it is used, so a record shared by many methods appears many times,
// under its class name as the schema's title where it has one and under the parameter's or property's name where it does not.
// Parse folds every record of one shape into one component: named by its class where any occurrence has one, by the one name
// its occurrences share where they have none (the query options of every query are QueryOptions), and otherwise each occurrence
// by the method and parameter or the record and property it sits in. An enum is a type named for the record and property it
// sits in. Each method becomes the Call operation of a path named for it, its parameters In "call" in order, its answer the 200
// response, and its namespace (pool, pool.dataset) the tag it is grouped by.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/katbyte/pandorest/openapi"
)

// The JSON Schema types the description uses, as it spells them.
const (
	typeNull   = "null"
	typeObject = "object"
	typeArray  = "array"
	typeString = "string"
)

// method is one entry of the description, as TrueNAS writes it.
type method struct {
	Description    string    `json:"description"`
	Roles          []string  `json:"roles"`
	Accepts        []*schema `json:"accepts"`
	Returns        []*schema `json:"returns"`
	Job            bool      `json:"job"`
	Filterable     bool      `json:"filterable"`
	ItemMethod     bool      `json:"item_method"`
	NoAuthRequired bool      `json:"no_auth_required"`
	Downloadable   bool      `json:"downloadable"`
	Uploadable     bool      `json:"uploadable"`
}

// schema is a JSON Schema as pydantic writes one, with TrueNAS's own marks on it: _name_ and _required_ on a parameter and a
// property, and a title that is the class name of a record or the name of the parameter or property it sits in.
type schema struct {
	Type                 string             `json:"type"`
	Format               string             `json:"format"`
	Title                string             `json:"title"`
	Description          string             `json:"description"`
	Properties           map[string]*schema `json:"properties"`
	Items                json.RawMessage    `json:"items"`
	PrefixItems          json.RawMessage    `json:"prefixItems"`
	AdditionalProperties json.RawMessage    `json:"additionalProperties"`
	AnyOf                []*schema          `json:"anyOf"`
	OneOf                []*schema          `json:"oneOf"`
	AllOf                []*schema          `json:"allOf"`
	Enum                 []json.RawMessage  `json:"enum"`
	Const                json.RawMessage    `json:"const"`
	Name                 string             `json:"_name_"`
	Required             bool               `json:"_required_"`
	// Discriminator, on a choice of records, names the property the records name themselves by, and maps each value of it
	// to the record ("#/$defs/Class") it names
	Discriminator *discriminator `json:"discriminator"`
}

// discriminator is how a choice of records is told apart, as pydantic writes it.
type discriminator struct {
	PropertyName string            `json:"propertyName"`
	Mapping      map[string]string `json:"mapping"`
}

// Load reads a description from a file; title and version name the server in the document's info block.
func Load(file, title, version string) (*openapi.Spec, []string, error) {
	data, err := os.ReadFile(file) //nolint:gosec // the path the service config names
	if err != nil {
		return nil, nil, err
	}
	spec, notes, err := Parse(data, title, version)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", file, err)
	}

	return spec, notes, nil
}

// Parse reads a description into a document, and says in notes what it could not fold: a name under which two records
// differ, which get numbered names, and a record named as the package names a method's answer.
func Parse(data []byte, title, version string) (*openapi.Spec, []string, error) {
	var methods map[string]*method
	if err := json.Unmarshal(data, &methods); err != nil {
		return nil, nil, fmt.Errorf("the description is not an object of methods: %w", err)
	}
	if len(methods) == 0 {
		return nil, nil, errors.New("the description names no methods")
	}

	spec := &openapi.Spec{OpenAPI: "3.1.0", Paths: map[string]*openapi.PathItem{}}
	spec.Info.Title = title
	spec.Info.Version = version
	spec.Components.Schemas = map[string]*openapi.Schema{}
	l := &loader{spec: spec, reserved: map[string]bool{}, placed: map[string]string{}, classes: map[string]map[string]int{}, owners: map[string][]string{}, enumOwners: map[string][]string{}}
	names := openapi.SortedKeys(methods)
	for _, name := range names {
		if err := l.check(name, methods[name]); err != nil {
			return nil, nil, err
		}
		// the generated package declares this for every method, so a record named the same takes another name
		l.reserved[openapi.Camel(name)+"Response"] = true
		if methods[name].Filterable {
			l.reserved[openapi.Camel(name)+"OneResponse"], l.reserved[openapi.Camel(name)+"CountResponse"] = true, true
		}
	}
	if n := l.queryFlags(methods); n > 0 {
		l.notes = append(l.notes, fmt.Sprintf("the get and count flags of query options are methods of their own, taken out of %d methods' options", n))
	}
	// every record is seen first, so that the name a shape is folded under is settled before any is placed
	for _, name := range names {
		l.survey(name, methods[name])
	}
	l.choose()
	for _, name := range names {
		l.method(name, methods[name])
	}

	return spec, l.notes, nil
}

// loader folds the description's records into the document's components.
type loader struct {
	spec *openapi.Spec
	// reserved is each name the generated package declares for a method (<Method>Response, <Method>Answer), which no record
	// may take
	reserved map[string]bool
	// classes and owners are what the survey found of each record shape: the class titles it appears under, each with how
	// often, and the names of the parameters and properties it sits in where it has no class
	classes map[string]map[string]int
	owners  map[string][]string
	// chosen is the name each shape is folded under, or "" for one each occurrence names itself
	chosen map[string]string
	// enumOwners are the names of the properties and parameters each choice among strings sits in, and chosenEnum the one name
	// a choice met under several is folded under, where their names share a head and a tail
	enumOwners map[string][]string
	chosenEnum map[string]string
	// placed is the shape under each component's name, which is how a record met again is known to be the one there
	placed map[string]string
	notes  []string
}

// check refuses a method the loader cannot take: a name with a space or slash in it, more than one answer, or a parameter
// with no name or a name used twice.
func (*loader) check(name string, m *method) error {
	if strings.Contains(name, " ") || strings.Contains(name, "/") {
		return fmt.Errorf("%s: a method's name has no space or slash", name)
	}
	if len(m.Returns) > 1 {
		return fmt.Errorf("%s: answers %d schemas, where a method answers one", name, len(m.Returns))
	}
	seen := map[string]bool{}
	for i, s := range m.Accepts {
		if s == nil || s.Name == "" {
			return fmt.Errorf("%s: parameter %d has no _name_", name, i)
		}
		if seen[s.Name] {
			return fmt.Errorf("%s: two parameters are named %s", name, s.Name)
		}
		seen[s.Name] = true
	}

	return nil
}

// queryFlags takes the get and count flags out of every query options record, the parameter named options that has both, and
// says how many methods took them: setting either changes what a query answers, so the generated package asks for those forms
// through methods of its own, which set the flag themselves. The lookups of one record take the same options, where the
// flags do nothing, and lose them too so that every method's options fold to one record.
func (*loader) queryFlags(methods map[string]*method) int {
	n := 0
	for _, m := range methods {
		for _, s := range m.Accepts {
			if s == nil || s.Name != "options" || s.Properties["get"] == nil || s.Properties["count"] == nil {
				continue
			}
			delete(s.Properties, "get")
			delete(s.Properties, "count")
			n++
		}
	}

	return n
}

// answerOf is what a method answers: its one returns schema, the list branch for a query, which is declared as the list, one
// record or a count by the options sent; nil for a method answering nothing.
func answerOf(m *method) *schema {
	if len(m.Returns) != 1 || m.Returns[0] == nil || m.Returns[0].Type == typeNull {
		return nil
	}
	returns := m.Returns[0]
	if m.Filterable {
		for _, v := range returns.AnyOf {
			if v.Type == typeArray {
				return v
			}
		}
	}

	return returns
}

// rows is the record a method's answer is a list of, with the name it is folded under when its shape has no class and no other
// name (<Method>ResultItem, as the importer names a list's inline record), or nil for an answer that is not a list of records.
func rows(name string, returns *schema) (item *schema, itemName string) {
	inner, _ := unwrapNull(returns)
	if inner == nil || inner.Type != typeArray || len(inner.PrefixItems) > 0 {
		return nil, ""
	}
	item = itemSchema(inner)
	if record, _ := unwrapNull(item); record == nil || record.Type != typeObject || len(record.Properties) == 0 {
		return nil, ""
	}

	return item, openapi.Camel(name) + "ResultItem"
}

// answer is a method's answer as the document carries it: a list of records is a list of the record, named as its rows, and
// anything else a record or a value named for the method.
func (l *loader) answer(name string, returns *schema) *openapi.Schema {
	if item, itemName := rows(name, returns); item != nil {
		inner, nullable := unwrapNull(returns)

		return &openapi.Schema{Type: openapi.TypeArray, Items: l.record(item, itemName), Description: strings.TrimSpace(inner.Description), Nullable: nullable}
	}

	return l.record(returns, openapi.Camel(name)+"Answer")
}

// parameterName is the name the record a parameter takes is folded under when its shape has no class and no other name: the
// parameter's where it reads as a class (dns_authenticator_create, login_data), and the method's and the parameter's otherwise
// (options, data).
func parameterName(method string, s *schema) string {
	if strings.Contains(s.Name, "_") {
		return openapi.Camel(s.Name)
	}

	return openapi.Camel(method) + openapi.Camel(s.Name)
}

// survey walks a method's records noting each shape's class titles and names, before anything is placed.
func (l *loader) survey(name string, m *method) {
	for _, s := range m.Accepts {
		l.surveyRecord(s, parameterName(name, s))
	}
	if answer := answerOf(m); answer != nil {
		if item, itemName := rows(name, answer); item != nil {
			l.surveyRecord(item, itemName)
		} else {
			l.surveyRecord(answer, openapi.Camel(name)+"Answer")
		}
	}
}

// surveyRecord notes a record's shape under its class title or, without one, the name it sits in, and walks what it holds.
func (l *loader) surveyRecord(s *schema, name string) {
	s, _ = unwrapNull(s)
	switch {
	case s == nil:
		return
	case isStringEnum(s):
		if name != "" {
			l.enumOwners[shape(s)] = append(l.enumOwners[shape(s)], name)
		}
	case len(s.AnyOf) > 0 || len(s.OneOf) > 0 || len(s.AllOf) > 0:
		for _, v := range slices.Concat(s.AnyOf, s.OneOf, s.AllOf) {
			l.surveyRecord(v, "")
		}
	case s.Type == typeArray:
		if item, _ := unwrapNull(itemSchema(s)); isStringEnum(item) {
			// a list of a choice is named as the choice is, for the property it sits in
			l.surveyRecord(item, name)

			return
		}
		l.surveyRecord(itemSchema(s), "")
	case s.Type == typeObject && len(s.Properties) > 0:
		key := shape(s)
		own := name
		if classLike(s.Title) {
			if l.classes[key] == nil {
				l.classes[key] = map[string]int{}
			}
			l.classes[key][s.Title]++
			own = s.Title
		} else if name != "" {
			l.owners[key] = append(l.owners[key], name)
		}
		for _, prop := range openapi.SortedKeys(s.Properties) {
			child := ""
			if own != "" {
				child = own + openapi.Camel(prop)
			}
			l.surveyRecord(s.Properties[prop], child)
		}
	}
}

// choose settles the name each shape is folded under: its one class title where it has one, nothing where it has several
// (two classes that happen to hold the same fields are two, and each occurrence names itself), the one name its occurrences
// share where it has no class (the words every name ends in: QueryOptions, Schedule), and nothing where the names share none.
func (l *loader) choose() {
	l.chosen = map[string]string{}
	for key, titles := range l.classes {
		if len(titles) == 1 {
			l.chosen[key] = openapi.SortedKeys(titles)[0]
		}
	}
	for key, names := range l.owners {
		if l.chosen[key] != "" || len(l.classes[key]) > 0 {
			continue
		}
		unique := slices.Compact(slices.Sorted(slices.Values(names)))
		if len(unique) == 1 {
			l.chosen[key] = unique[0]

			continue
		}
		l.chosen[key] = commonSuffix(unique)
	}

	// a choice among the same strings met under several names is one type, named by the words the names share at both ends
	// (ReplicationCreateDirection and ReplicationQueryResultItemDirection are ReplicationDirection), so a value read from one
	// record passes to another without a conversion
	l.chosenEnum = map[string]string{}
	for key, names := range l.enumOwners {
		// within the names that start alike, so a direction shared by replication, cloud sync and rsync tasks is three types,
		// each namespace's own
		byHead := map[string][]string{}
		for _, name := range slices.Compact(slices.Sorted(slices.Values(names))) {
			head := words(name)[0]
			byHead[head] = append(byHead[head], name)
		}
		for head, unique := range byHead {
			if len(unique) > 1 {
				l.chosenEnum[key+"\x00"+head] = enumName(unique)
			}
		}
	}
}

// enumName is the one name a choice met under several is folded under: the words the names share at the head and at the tail
// (ReplicationDirection), or the tail alone where they share no head and it is more than one word; "" where they share no
// tail, or only a lone word (every Type is not one Type).
func enumName(names []string) string {
	head, tail := commonHead(names), commonTail(names)
	switch {
	case len(tail) == 0, len(head) == 0 && len(tail) == 1:
		return ""
	default:
		return strings.Join(head, "") + strings.Join(tail, "")
	}
}

// commonHead is the words every name starts with, short of the words at the tail they share, so a head and a tail never
// overlap.
func commonHead(names []string) []string {
	var common []string
	tail := len(commonTail(names))
	for i, name := range names {
		w := words(name)
		w = w[:len(w)-tail]
		if i == 0 {
			common = w

			continue
		}
		n := 0
		for n < len(common) && n < len(w) && common[n] == w[n] {
			n++
		}
		common = common[:n]
		if n == 0 {
			return nil
		}
	}

	return common
}

// tailWords are the words a name ends in by construction, which say nothing of what a record is: a shape whose names share
// only one of them (every method's Answer) is not folded under it.
var tailWords = map[string]bool{"Answer": true, "Options": true, "Result": true, "Item": true, "Items": true, "Data": true, "Entry": true, "Model": true, "Create": true, "Update": true, "Params": true, "Config": true, "Info": true}

// commonSuffix is the words every name ends in, as one name, or "": AlertQueryOptions and PoolQueryOptions share QueryOptions,
// and CronjobCreateSchedule and ReplicationCreateSchedule share Schedule; a lone tail word is no name.
func commonSuffix(names []string) string {
	common := commonTail(names)
	if len(common) == 0 || (len(common) == 1 && tailWords[common[0]]) {
		return ""
	}

	return strings.Join(common, "")
}

// commonTail is the words every name ends in.
func commonTail(names []string) []string {
	var common []string
	for i, name := range names {
		w := words(name)
		if i == 0 {
			common = w

			continue
		}
		n := 0
		for n < len(common) && n < len(w) && common[len(common)-1-n] == w[len(w)-1-n] {
			n++
		}
		common = common[len(common)-n:]
		if n == 0 {
			return nil
		}
	}

	return common
}

// words splits a name at its capitals: PoolQueryOptions is Pool, Query, Options, and NFS4ACE is one word.
func words(name string) []string {
	var out []string
	runes := []rune(name)
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		if unicode.IsUpper(cur) && (unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && unicode.IsLower(next))) {
			out = append(out, string(runes[start:i]))
			start = i
		}
	}

	return append(out, string(runes[start:]))
}

// method adds one method as the Call operation of a path named for it.
func (l *loader) method(name string, m *method) {
	namespace := name
	if before, _, ok := strings.CutLast(name, "."); ok {
		namespace = before
	}
	op := &openapi.Operation{
		OperationID: name,
		Tags:        []string{namespace},
		Description: strings.TrimSpace(m.Description),
		Responses:   map[string]*openapi.Response{},
		Call:        &openapi.CallInfo{Method: name, Job: m.Job, Filterable: m.Filterable, Item: m.ItemMethod, NoAuth: m.NoAuthRequired, Downloadable: m.Downloadable, Uploadable: m.Uploadable, Roles: m.Roles},
	}
	for _, s := range m.Accepts {
		op.Parameters = append(op.Parameters, &openapi.Parameter{Name: s.Name, In: openapi.InCall, Required: s.Required, Description: strings.TrimSpace(s.Description), Schema: l.parameter(name, s)})
	}
	answer := &openapi.Response{Description: "the method's answer"}
	if returns := answerOf(m); returns != nil {
		answer.Content = map[string]*openapi.MediaType{"application/json": {Schema: l.answer(name, returns)}}
	}
	op.Responses["200"] = answer
	l.spec.Paths[name] = &openapi.PathItem{Call: op}
}

// parameter is a parameter's schema: an enum taken bare is a type named for the method and the parameter (auth.login_ex's
// mechanism), and a record is folded under its shape's name.
func (l *loader) parameter(method string, s *schema) *openapi.Schema {
	inner, nullable := unwrapNull(s)
	if isStringEnum(inner) {
		ref := l.enum(openapi.Camel(method)+openapi.Camel(s.Name), inner)
		ref.Nullable = nullable
		ref.Description = strings.TrimSpace(s.Description)

		return ref
	}

	return l.record(s, parameterName(method, s))
}

// record is a schema as the document carries it, with a record of properties folded into a component under the name its
// shape was given, or under name where its shape has none; anything else is converted as it stands.
func (l *loader) record(s *schema, name string) *openapi.Schema {
	inner, nullable := unwrapNull(s)
	if inner == nil || inner.Type != typeObject || len(inner.Properties) == 0 {
		return l.convert(s)
	}
	key := shape(inner)
	chosen := l.chosen[key]
	if chosen == "" && classLike(inner.Title) {
		// a class among several of one shape keeps its own name
		chosen = inner.Title
	}
	if chosen == "" {
		chosen = name
	}
	if chosen == "" {
		// a record nothing names, among a list's items: left as it is, for the importer to name
		out := l.plain(inner, "")
		out.Nullable = out.Nullable || nullable

		return out
	}
	ref := l.place(chosen, key, func(under string) *openapi.Schema { return l.plain(inner, under) })
	ref.Nullable = nullable
	ref.Description = strings.TrimSpace(s.Description)

	return ref
}

// convert turns one schema into the document's, folding a record into a component where its shape has a name.
func (l *loader) convert(s *schema) *openapi.Schema {
	if s == nil {
		return &openapi.Schema{}
	}
	if s.Type == typeObject && len(s.Properties) > 0 {
		return l.record(s, "")
	}

	return l.plain(s, "")
}

// place puts a schema among the components under a name, or finds it there already, and answers a reference to it; one of
// another shape under the same name gets the name numbered, and a note says so. body makes the schema under the name it gets,
// which is what nested names are built from.
func (l *loader) place(name, key string, body func(under string) *openapi.Schema) *openapi.Schema {
	if l.reserved[name] {
		l.note(name + " is a name the package gives a method's answer, so the record is " + name + "Model")
		name += "Model"
	}
	for i := 1; ; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s%d", name, i)
		}
		prev, ok := l.placed[candidate]
		switch {
		case !ok:
			l.placed[candidate] = key
			l.spec.Components.Schemas[candidate] = body(candidate)
			if i > 1 {
				l.note(fmt.Sprintf("two records are named %s and differ, so the second is %s", name, candidate))
			}

			return &openapi.Schema{Ref: openapi.SchemaRefPrefix + candidate}
		case prev == key:
			return &openapi.Schema{Ref: openapi.SchemaRefPrefix + candidate}
		}
	}
}

// note records a note once.
func (l *loader) note(text string) {
	if !slices.Contains(l.notes, text) {
		l.notes = append(l.notes, text)
	}
}

// enum adds a choice among strings to the components under a name, the one chosen for the choice where it was met under
// several, numbered when another choice has it, and answers a reference to it.
func (l *loader) enum(name string, s *schema) *openapi.Schema {
	if chosen := l.chosenEnum[shape(s)+"\x00"+words(name)[0]]; chosen != "" {
		name = chosen
	}
	values := s.Enum
	if len(values) == 0 {
		values = []json.RawMessage{s.Const}
	}
	body := &openapi.Schema{Type: openapi.TypeString, Description: strings.TrimSpace(s.Description)}
	body.Enum, body.Nullable = stringEnum(values)

	return l.place(name, shape(s), func(string) *openapi.Schema { return body })
}

// plain turns one schema into the document's without folding it, whatever its title; owner is the name a choice among strings
// or a record inside it is named from, or "" where it has none and stays a string or is left as it is.
func (l *loader) plain(s *schema, owner string) *openapi.Schema {
	out := &openapi.Schema{Description: strings.TrimSpace(s.Description), Format: s.Format}
	switch {
	case len(s.AnyOf) > 0:
		l.anyOf(out, s.AnyOf, s.Discriminator)

		return out
	case len(s.OneOf) > 0:
		l.members(out, s.OneOf, s.Discriminator)

		return out
	case len(s.AllOf) == 1:
		return l.convert(s.AllOf[0])
	}
	if len(s.Enum) > 0 {
		out.Enum, out.Nullable = stringEnum(s.Enum)
	} else if len(s.Const) > 0 {
		out.Enum, _ = stringEnum([]json.RawMessage{s.Const})
	}

	switch s.Type {
	case typeNull:
		// a value that is always null: anything, and absent
		out.Nullable = true
	case typeString, "integer", "number", "boolean":
		out.Type = s.Type
	case typeArray:
		out.Type = openapi.TypeArray
		out.Items = l.items(s)
	case typeObject:
		out.Type = openapi.TypeObject
		for _, name := range openapi.SortedKeys(s.Properties) {
			p := s.Properties[name]
			if out.Properties == nil {
				out.Properties = map[string]*openapi.Schema{}
			}
			out.Properties[name] = l.property(owner, name, p)
			if p != nil && p.Required {
				out.Required = append(out.Required, name)
			}
		}
		out.AdditionalProperties = l.additional(s.AdditionalProperties)
	default:
		// no type: anything
	}

	return out
}

// property is one property of a record as the document carries it: a choice among strings becomes a type named for the record
// and the property (PoolEntryStatus), a list of them a list of that type, and a record is folded under its shape's name or
// the record's and the property's.
func (l *loader) property(owner, prop string, p *schema) *openapi.Schema {
	inner, nullable := unwrapNull(p)
	name := ""
	if owner != "" {
		name = owner + openapi.Camel(prop)
	}
	var out *openapi.Schema
	switch {
	case inner == nil:
		return l.convert(p)
	case name != "" && isStringEnum(inner):
		out = l.enum(name, inner)
	case name != "" && inner.Type == typeArray && len(inner.PrefixItems) == 0 && isStringEnum(itemSchema(inner)):
		out = &openapi.Schema{Type: openapi.TypeArray, Items: l.enum(name, itemSchema(inner))}
	case inner.Type == typeObject && len(inner.Properties) > 0:
		return l.record(p, name)
	default:
		return l.convert(p)
	}
	out.Nullable = out.Nullable || nullable
	if out.Description == "" {
		out.Description = strings.TrimSpace(p.Description)
	}

	return out
}

// members settles a choice of records into the document's: each converted, and where the choice says which field the records
// name themselves by (disc), the value naming each member, from the mapping by the member's class or from the one value the
// field is fixed to in the record.
func (l *loader) members(out *openapi.Schema, records []*schema, disc *discriminator) {
	if disc != nil {
		out.Discriminator = &openapi.Discriminator{PropertyName: disc.PropertyName, Mapping: map[string]string{}}
	}
	// the document's record under each class title, once converted, so every kind the mapping sends to a class is kept
	byTitle := map[string]string{}
	for _, v := range records {
		member := l.convert(v)
		out.OneOf = append(out.OneOf, member)
		if out.Discriminator == nil || member.RefName() == "" {
			continue
		}
		byTitle[v.Title] = member.RefName()
		// a record the mapping does not name is known by the one value its field is fixed to
		var kind string
		if p := v.Properties[disc.PropertyName]; p != nil && len(p.Const) > 0 {
			_ = json.Unmarshal(p.Const, &kind)
		}
		if kind != "" {
			out.Discriminator.Mapping[kind] = openapi.SchemaRefPrefix + member.RefName()
		}
	}
	if disc != nil {
		for kind, ref := range disc.Mapping {
			if name := byTitle[path.Base(ref)]; name != "" {
				out.Discriminator.Mapping[kind] = openapi.SchemaRefPrefix + name
			}
		}
	}
}

// anyOf settles a choice of schemas into one: null among them makes the rest nullable, one left is itself, and several are what
// they have in common, a string, a number, a list or a record of one of several shapes (told apart by disc, where the choice
// gives one), and otherwise anything.
func (l *loader) anyOf(out *openapi.Schema, choices []*schema, disc *discriminator) {
	var rest []*schema
	for _, v := range choices {
		if v != nil && v.Type == typeNull {
			out.Nullable = true

			continue
		}
		rest = append(rest, v)
	}
	switch {
	case len(rest) == 0:
		return
	case len(rest) == 1:
		one := l.convert(rest[0])
		one.Nullable = one.Nullable || out.Nullable
		if one.Description == "" {
			one.Description = out.Description
		}
		*out = *one

		return
	}
	kinds := map[string]bool{}
	for _, v := range rest {
		kinds[v.Type] = true
	}
	switch {
	case len(kinds) == 1 && kinds[typeString]:
		// two strings, one an enum, say: the string, since the values of one do not bound the other
		out.Type = openapi.TypeString
	case len(kinds) == 1 && kinds["integer"]:
		out.Type = openapi.TypeInteger
	case (len(kinds) == 1 || len(kinds) == 2 && kinds["integer"]) && kinds["number"]:
		out.Type = openapi.TypeNumber
	case len(kinds) == 1 && kinds[typeArray]:
		out.Type = openapi.TypeArray
		out.Items = &openapi.Schema{}
	case len(kinds) == 1 && kinds[typeObject]:
		l.members(out, rest, disc)
	default:
		// a boolean or an integer or a string: anything
	}
}

// items is a list's item schema: pydantic writes items as a list of one schema, JSON Schema as one; a tuple (prefixItems) is a
// list of anything.
func (l *loader) items(s *schema) *openapi.Schema {
	raw := bytes.TrimSpace(s.Items)
	switch {
	case len(raw) == 0 || len(s.PrefixItems) > 0:
		return &openapi.Schema{}
	case raw[0] == '[':
		item := itemSchema(s)
		if item == nil {
			return &openapi.Schema{}
		}

		return l.convert(item)
	default:
		var one schema
		if err := json.Unmarshal(raw, &one); err != nil {
			return &openapi.Schema{}
		}

		return l.convert(&one)
	}
}

// itemSchema is a list's item schema as pydantic writes it, a list of one, or nil.
func itemSchema(s *schema) *schema {
	if s == nil {
		return nil
	}
	raw := bytes.TrimSpace(s.Items)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var list []*schema
	if err := json.Unmarshal(raw, &list); err != nil || len(list) != 1 {
		return nil
	}

	return list[0]
}

// additional is a record's additionalProperties as the document carries it: a schema converted, true as itself, and false or
// absent as nothing.
func (l *loader) additional(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0, bytes.Equal(raw, []byte("false")):
		return nil
	case bytes.Equal(raw, []byte("true")):
		return raw
	}
	var s schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	out, _ := json.Marshal(l.convert(&s))

	return out
}

// unwrapNull is a schema with a null choice taken off it: the one other choice and true, or the schema itself and false.
func unwrapNull(s *schema) (inner *schema, nullable bool) {
	if s == nil || len(s.AnyOf) != 2 {
		return s, false
	}
	for i, v := range s.AnyOf {
		if v != nil && v.Type == typeNull {
			other := s.AnyOf[1-i]
			if other == nil {
				return s, false
			}

			return other, true
		}
	}

	return s, false
}

// isStringEnum reports whether a schema is a choice among strings, or one string (a const), which the package declares a type
// for when it has a name to give it.
func isStringEnum(s *schema) bool {
	if s == nil || (s.Type != typeString && s.Type != "") || len(s.AnyOf) > 0 || len(s.OneOf) > 0 {
		return false
	}
	if len(s.Const) > 0 {
		values, _ := stringEnum([]json.RawMessage{s.Const})

		return len(values) == 1
	}
	values, _ := stringEnum(s.Enum)

	return len(values) > 0
}

// stringEnum is an enum's string values, and whether null was among them; an enum of numbers or booleans is no enum, the type
// alone says what the value is.
func stringEnum(values []json.RawMessage) (strs []json.RawMessage, nullable bool) {
	for _, raw := range values {
		trimmed := bytes.TrimSpace(raw)
		switch {
		case bytes.Equal(trimmed, []byte(typeNull)):
			nullable = true
		case len(trimmed) > 0 && trimmed[0] == '"':
			strs = append(strs, trimmed)
		default:
			return nil, nullable
		}
	}

	return strs, nullable
}

// shape is a schema's structure as text, the same for two records of one shape whatever they are titled or described: the
// type, format, values, properties, items and choices, down to the last.
func shape(s *schema) string {
	var b strings.Builder
	var walk func(s *schema)
	walk = func(s *schema) {
		if s == nil {
			b.WriteString("_")

			return
		}
		b.WriteString(s.Type)
		b.WriteByte(':')
		b.WriteString(s.Format)
		if len(s.Enum) > 0 || len(s.Const) > 0 {
			b.WriteString("e[")
			for _, v := range s.Enum {
				b.Write(bytes.TrimSpace(v))
				b.WriteByte(',')
			}
			b.Write(bytes.TrimSpace(s.Const))
			b.WriteByte(']')
		}
		if len(s.Properties) > 0 {
			b.WriteString("p{")
			// whether a property is required does not reach the type a caller holds, so a record that insists on a field where
			// another does not is one shape: a query's row and get_instance's answer
			for _, name := range openapi.SortedKeys(s.Properties) {
				b.WriteString(name)
				b.WriteByte(':')
				walk(s.Properties[name])
				b.WriteByte(';')
			}
			b.WriteByte('}')
		}
		if len(s.Items) > 0 {
			b.WriteString("i[")
			if item := itemSchema(s); item != nil {
				walk(item)
			} else {
				b.Write(bytes.TrimSpace(s.Items))
			}
			b.WriteByte(']')
		}
		if len(s.PrefixItems) > 0 {
			b.WriteString("t")
		}
		// additionalProperties false says nothing of the type either; a schema there, or true, does
		if raw := bytes.TrimSpace(s.AdditionalProperties); len(raw) > 0 && !bytes.Equal(raw, []byte("false")) {
			b.WriteString("a[")
			b.Write(raw)
			b.WriteByte(']')
		}
		for _, choice := range []struct {
			word string
			list []*schema
		}{{"y", s.AnyOf}, {"o", s.OneOf}, {"l", s.AllOf}} {
			if len(choice.list) == 0 {
				continue
			}
			b.WriteString(choice.word)
			b.WriteByte('(')
			for _, v := range choice.list {
				walk(v)
				b.WriteByte('|')
			}
			b.WriteByte(')')
		}
	}
	walk(s)

	return b.String()
}

// classLike reports whether a title is a class name (PoolEntry, NFS4ACE_AdvancedFlags) rather than the name of the parameter
// or property the record sits in (result, options, data).
func classLike(title string) bool {
	if title == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(title)
	if !unicode.IsUpper(first) {
		return false
	}

	return !slices.ContainsFunc([]rune(title), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })
}
