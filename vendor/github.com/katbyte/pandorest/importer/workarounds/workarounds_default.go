package workarounds

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/katbyte/pandorest/openapi"
)

// The default workarounds are for the bugs every document seems to have:
// an operation that declares no answer, or the wrong one, or the wrong
// status; a parameter the server reads or a field it sends that the document
// leaves out; a path that is not the API's. A repository says which
// operations and why, and the patching and the check that the bug is still
// there are here, once, in place of a copy in each repository. Each is a
// Workaround like one written by hand, and passes Verify the same way.

// About is what a default workaround is called, the service it is for and
// the bug it fixes: what one written by hand says with its Name, Service and
// Bug methods.
type About struct {
	Name, Service, Bug string
}

type defaultWorkaround struct {
	about About
	apply func(spec *openapi.Spec, note func(string)) error
}

func (r defaultWorkaround) Name() string                   { return r.about.Name }
func (r defaultWorkaround) Service() string                { return r.about.Service }
func (r defaultWorkaround) Bug() string                    { return r.about.Bug }
func (r defaultWorkaround) Apply(spec *openapi.Spec) error { return r.apply(spec, func(string) {}) }

// ApplyNoting applies the workaround and says what it reached, a note per
// name ("createdAt in 60 places"), which the import logs: a name that is a
// date or an enum in fewer places than expected is seen there rather than by
// reading generated files.
func (r defaultWorkaround) ApplyNoting(spec *openapi.Spec, note func(string)) error {
	return r.apply(spec, note)
}

// Answer is what an operation answers, as a default workaround says it:
// one of JSON, JSONString, Model, ListOf, Shape, File and Text.
type Answer struct {
	kind   answerKind
	name   string
	schema *openapi.Schema
}

type answerKind int

const (
	answersJSON answerKind = iota + 1
	answersJSONString
	answersModel
	answersList
	answersShape
	answersFile
	answersText
)

// JSON is JSON of no declared shape, which a method holds raw.
func JSON() Answer { return Answer{kind: answersJSON} }

// JSONString is a JSON string: text in quotes.
func JSONString() Answer { return Answer{kind: answersJSONString} }

// Model is JSON that decodes into one of the document's component schemas.
func Model(schema string) Answer { return Answer{kind: answersModel, name: schema} }

// ListOf is a JSON list of one of the document's component schemas.
func ListOf(schema string) Answer { return Answer{kind: answersList, name: schema} }

// Shape is JSON of a shape the workaround gives, for a document that keeps
// its schemas with its operations and has none to name.
func Shape(schema *openapi.Schema) Answer { return Answer{kind: answersShape, schema: schema} }

// File is an answer of a media type that is not JSON, left for the caller to
// read: an image, a download, a log.
func File(mediaType string) Answer { return Answer{kind: answersFile, name: mediaType} }

// Text is short text that is not JSON, read whole into a string: a path, a
// version, a pong. It is held as it was sent whatever the server calls it,
// since one that sends bare text may label it JSON. A long text is a File.
func Text(mediaType string) Answer { return Answer{kind: answersText, name: mediaType} }

func (a Answer) String() string {
	switch a.kind {
	case answersJSON:
		return "JSON of no declared shape"
	case answersJSONString:
		return "a JSON string"
	case answersModel:
		return "one " + a.name
	case answersList:
		return "a list of " + a.name
	case answersShape:
		return "JSON of a shape of its own"
	case answersFile:
		return "a file (" + a.name + ")"
	case answersText:
		return "text (" + a.name + ")"
	default:
		return "no answer"
	}
}

// content is the answer as a response's content.
func (a Answer) content(spec *openapi.Spec) (map[string]*openapi.MediaType, error) {
	ref := func() (*openapi.Schema, error) {
		if spec.Components.Schemas[a.name] == nil {
			return nil, fmt.Errorf("schema %s is not in the document", a.name)
		}

		return &openapi.Schema{Ref: openapi.SchemaRefPrefix + a.name}, nil
	}
	asJSON := func(schema *openapi.Schema) map[string]*openapi.MediaType {
		return map[string]*openapi.MediaType{jsonMedia: {Schema: schema}}
	}

	switch a.kind {
	case answersJSON:
		return asJSON(nil), nil
	case answersJSONString:
		return asJSON(&openapi.Schema{Type: openapi.TypeString}), nil
	case answersModel:
		schema, err := ref()
		if err != nil {
			return nil, err
		}

		return asJSON(schema), nil
	case answersList:
		schema, err := ref()
		if err != nil {
			return nil, err
		}

		return asJSON(&openapi.Schema{Type: openapi.TypeArray, Items: schema}), nil
	case answersShape:
		if a.schema == nil {
			return nil, errors.New("a shape with no schema")
		}

		return asJSON(a.schema), nil
	case answersFile, answersText:
		if !strings.Contains(a.name, "/") || openapi.JSONMedia(a.name) {
			return nil, fmt.Errorf("%q is not a media type other than JSON's", a.name)
		}

		return map[string]*openapi.MediaType{a.name: {Schema: &openapi.Schema{Type: openapi.TypeString, Format: binaryFormat}, Text: a.kind == answersText}}, nil
	default:
		return nil, errors.New("no answer is given")
	}
}

// declaredBy reports whether a response's content declares this answer.
func (a Answer) declaredBy(content map[string]*openapi.MediaType) bool {
	mediaType, hasJSON := openapi.PickJSON(content)
	if a.kind == answersFile || a.kind == answersText {
		media := content[a.name]

		return media != nil && !hasJSON && media.Text == (a.kind == answersText)
	}
	if !hasJSON || content[mediaType] == nil {
		return false
	}

	schema := content[mediaType].Schema
	switch a.kind {
	case answersJSON:
		return schema == nil || reflect.DeepEqual(schema, &openapi.Schema{})
	case answersJSONString:
		return schema != nil && schema.Type == openapi.TypeString && schema.Format != binaryFormat && schema.RefName() == ""
	case answersModel:
		return schema.RefName() == a.name
	case answersList:
		return schema != nil && schema.Type == openapi.TypeArray && schema.Items.RefName() == a.name
	case answersShape:
		return reflect.DeepEqual(schema, a.schema)
	default:
		return false
	}
}

const (
	jsonMedia    = "application/json"
	binaryFormat = "binary"
)

// targeted finds the operation a target names, a method and a path
// ("GET /Items").
func targeted(spec *openapi.Spec, target string) (*openapi.Operation, error) {
	method, path, ok := strings.Cut(target, " ")
	if !ok {
		return nil, fmt.Errorf("%q is not a method and a path", target)
	}

	return Operation(spec, method, path)
}

// success is the response an operation documents for success: its lowest
// 2xx.
func success(op *openapi.Operation) *openapi.Response {
	for _, code := range openapi.SortedKeys(op.Responses) {
		if len(code) == 3 && code[0] == '2' {
			return op.Responses[code]
		}
	}

	return nil
}

// UndeclaredAnswers is for operations that declare a success with no
// content, so that nothing says whether they answer JSON, and in what shape,
// or a file: answers says what each does, by method and path. It fails for
// any that declares its answer now.
func UndeclaredAnswers(about About, answers map[string]Answer) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, _ func(string)) error {
		if len(answers) == 0 {
			return errors.New("it names no operations")
		}

		var declared []string
		for _, target := range openapi.SortedKeys(answers) {
			op, err := targeted(spec, target)
			if err != nil {
				return err
			}
			resp := success(op)
			if resp == nil {
				return fmt.Errorf("%s has no success response", target)
			}
			if len(resp.Content) > 0 {
				declared = append(declared, target)

				continue
			}

			content, err := answers[target].content(spec)
			if err != nil {
				return fmt.Errorf("%s: %w", target, err)
			}
			resp.Content = content
		}
		if len(declared) > 0 {
			return fmt.Errorf("these now declare their answer, so take them out: %s", strings.Join(declared, ", "))
		}

		return nil
	}}
}

// Correction is an answer a document gets wrong: what it declares, and what
// the server answers.
type Correction struct {
	Declared Answer
	Answers  Answer
}

// ListDeclaredAsOne is the commonest correction: the document declares one
// of a schema, and the server answers a list of them (an update of many that
// answers every record it changed).
func ListDeclaredAsOne(schema string) Correction {
	return Correction{Declared: Model(schema), Answers: ListOf(schema)}
}

// WrongAnswers is for operations that declare one answer where the server
// gives another: a JSON string that is sent as bare text, one record where
// the server sends a list of them. corrections says, by method and path,
// what each declares and what it answers. It fails for any that no longer
// declares what its correction says, which is how a document that was fixed,
// or changed again, is noticed.
func WrongAnswers(about About, corrections map[string]Correction) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, _ func(string)) error {
		if len(corrections) == 0 {
			return errors.New("it names no operations")
		}

		for _, target := range openapi.SortedKeys(corrections) {
			op, err := targeted(spec, target)
			if err != nil {
				return err
			}
			resp := success(op)
			if resp == nil {
				return fmt.Errorf("%s has no success response", target)
			}

			c := corrections[target]
			if !c.Declared.declaredBy(resp.Content) {
				return fmt.Errorf("%s no longer declares %s", target, c.Declared)
			}
			content, err := c.Answers.content(spec)
			if err != nil {
				return fmt.Errorf("%s: %w", target, err)
			}
			if c.Declared.declaredBy(content) {
				return fmt.Errorf("%s: the correction changes nothing: both are %s", target, c.Declared)
			}
			resp.Content = content
		}

		return nil
	}}
}

// WrongStatus is for operations documented as answering one status that
// answer another with the same body: a create documented as 200 that answers
// 201 Created. answers lists, under each status the server really gives, the
// operations that give it, by method and path. It fails for any that no
// longer documents the declared status, or documents the other already.
func WrongStatus(about About, declared int, answers map[int][]string) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, _ func(string)) error {
		from, moved := strconv.Itoa(declared), 0
		for _, status := range slices.Sorted(maps.Keys(answers)) {
			to := strconv.Itoa(status)
			for _, target := range answers[status] {
				op, err := targeted(spec, target)
				if err != nil {
					return err
				}
				resp := op.Responses[from]
				if resp == nil || op.Responses[to] != nil {
					return fmt.Errorf("%s no longer documents a %s and no %s", target, from, to)
				}

				delete(op.Responses, from)
				op.Responses[to] = resp
				moved++
			}
		}
		if moved == 0 {
			return errors.New("it names no operations")
		}

		return nil
	}}
}

// UndeclaredParameters is for operations that read a parameter their
// document does not declare: targets are the operations, by method and path,
// and params what each of them is given. It fails when any already declares
// one of them. A header named Accept gives the caller the say over what one
// call asks for (see Param).
func UndeclaredParameters(about About, targets []string, params ...Param) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, _ func(string)) error {
		if len(targets) == 0 || len(params) == 0 {
			return errors.New("it names no operations or no parameters")
		}

		return AddParameters(spec, targets, params...)
	}}
}

// UndeclaredProperties is for the fields a server sends, and reads back,
// that its document does not declare, so that a model drops them without a
// word: properties says, by component schema, what to add. It fails for any
// the schema declares now.
func UndeclaredProperties(about About, properties map[string]map[string]*openapi.Schema) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, _ func(string)) error {
		added := 0
		for _, name := range openapi.SortedKeys(properties) {
			schema := spec.Components.Schemas[name]
			if schema == nil {
				return fmt.Errorf("no schema %s", name)
			}
			for _, property := range openapi.SortedKeys(properties[name]) {
				if schema.Properties[property] != nil {
					return fmt.Errorf("%s declares %s now", name, property)
				}
				if schema.Properties == nil {
					schema.Properties = map[string]*openapi.Schema{}
				}

				schema.Properties[property] = properties[name][property]
				added++
			}
		}
		if added == 0 {
			return errors.New("it names no properties")
		}

		return nil
	}}
}

// NamedTypes is for a field the server writes in a form of its own that the document cannot describe: a secret it hides from
// some callers behind a placeholder whatever the field's type, a list it writes two ways. Each field, named <schema>.<property>,
// is held as the Go type given, written as a package uses it (truenasrpc.Secret[int]) and imported from importPath; the type
// reads and writes its own JSON. client.Secret[T] is the one for a hidden value. $item in the type stands for the field's own
// type, or a list's element, as the package names it (truenasrpc.Set[$item]), so the workaround survives the package renaming
// an enum. It fails for a schema or property that is not there.
func NamedTypes(about About, importPath string, fields map[string]string) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(fields) == 0 {
			return errors.New("it names no fields")
		}

		for _, field := range openapi.SortedKeys(fields) {
			schema, property, ok := strings.Cut(field, ".")
			if !ok || schema == "" || property == "" {
				return fmt.Errorf("%q is not <schema>.<property>", field)
			}
			if fields[field] == "" {
				return fmt.Errorf("%s names no Go type", field)
			}
			s, err := Property(spec, schema, property)
			if err != nil {
				return err
			}
			if s.GoType != nil {
				return fmt.Errorf("%s is held as %s already", field, s.GoType.Name)
			}

			s.GoType = &openapi.GoType{Name: fields[field], Import: importPath}
		}
		note(fmt.Sprintf("%d fields held as named types", len(fields)))

		return nil
	}}
}

// DeclaredShapes is for a field the document leaves open, a record of no declared fields or a value of no type, that the
// server writes in a stable shape a program reads (a scrub's state, a task's last run): each field, named <schema>.<property>,
// takes the schema given, a record declared in place, which becomes a model named for the place, or a reference to one
// DeclaredSchemas added. It fails for a schema or property that is not there, and for one the document gives a shape now.
func DeclaredShapes(about About, fields map[string]*openapi.Schema) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(fields) == 0 {
			return errors.New("it names no fields")
		}

		for _, field := range openapi.SortedKeys(fields) {
			schema, property, ok := strings.Cut(field, ".")
			if !ok || schema == "" || property == "" {
				return fmt.Errorf("%q is not <schema>.<property>", field)
			}
			if fields[field] == nil {
				return fmt.Errorf("%s has no shape", field)
			}
			p, err := Property(spec, schema, property)
			if err != nil {
				return err
			}
			if p.RefName() != "" || len(p.Properties) > 0 || p.IsUnion() || p.IsEnum() || p.Items != nil || p.GoType != nil || (p.Type != "" && p.Type != openapi.TypeObject) {
				return fmt.Errorf("%s has a shape now", field)
			}

			shape := *fields[field]
			if shape.Description == "" {
				shape.Description = p.Description
			}
			*p = shape
		}
		note(fmt.Sprintf("%d fields given a shape", len(fields)))

		return nil
	}}
}

// NotAPI is for the paths a document lists that are not the API's: the web
// interface's page, its static files, its login form, which answer HTML to a
// browser and nothing a client reads. They are taken out of the document. It
// fails for any that is not there.
func NotAPI(about About, paths ...string) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, _ func(string)) error {
		if len(paths) == 0 {
			return errors.New("it names no paths")
		}

		for _, path := range paths {
			if spec.Paths[path] == nil {
				return fmt.Errorf("%s is not in the document", path)
			}
			delete(spec.Paths, path)
		}

		return nil
	}}
}

// UndeclaredDates is for the dates a document declares as plain strings
// (Dockhand's createdAt, startedAt, expiresAt), so that nothing marks them
// for the service's Dates: every string property with one of the names,
// wherever it sits in the document, is declared a date-time. It fails for a
// name no string property has, which is how a field the server renames is
// noticed, and for one every property of which is declared a date now.
func UndeclaredDates(about About, names ...string) Workaround {
	return declareDates(about, "date-time", nil, names)
}

// UndeclaredDays is UndeclaredDates for a day without a time (TMDB's
// release_date, written 2006-01-02), declared a date.
func UndeclaredDays(about About, names ...string) Workaround {
	return declareDates(about, "date", nil, names)
}

// UndeclaredDatesExcept is UndeclaredDates for a name that is a date
// everywhere but in some places, where the same name is a string of
// something else: except says, by operation ("GET /api/audit") or component
// schema ("Note"), which of the names to leave alone there. It fails for an
// exception that excludes nothing, which is how a document that changed is
// noticed.
func UndeclaredDatesExcept(about About, except map[string][]string, names ...string) Workaround {
	return declareDates(about, "date-time", except, names)
}

// UndeclaredDaysExcept is UndeclaredDatesExcept for days.
func UndeclaredDaysExcept(about About, except map[string][]string, names ...string) Workaround {
	return declareDates(about, "date", except, names)
}

// UndeclaredUnixDates is for the dates a document declares as integers, which
// are seconds or milliseconds since 1970 (unit "seconds" or "milliseconds"):
// every integer property with one of the names is declared one, for the
// generated Unix or UnixMilli, which reads a number and writes one back. A
// document never declares an epoch, so this is the only way a field becomes
// one. It fails as UndeclaredDates does.
func UndeclaredUnixDates(about About, unit string, names ...string) Workaround {
	format, ok := map[string]string{"seconds": openapi.FormatUnixSeconds, "milliseconds": openapi.FormatUnixMillis}[unit]
	if !ok {
		return defaultWorkaround{about, func(*openapi.Spec, func(string)) error {
			return fmt.Errorf("unit %q is not seconds or milliseconds", unit)
		}}
	}

	return declare(about, openapi.TypeInteger, format, nil, names)
}

// declareDates marks every string property of the names a date of the format given.
func declareDates(about About, format string, except map[string][]string, names []string) Workaround {
	return declare(about, openapi.TypeString, format, except, names)
}

// declare marks every property of the names that has the type given with the format, except where
// except says not to, and fails for a name that marks nothing or an exception that excludes nothing.
func declare(about About, typ, format string, except map[string][]string, names []string) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(names) == 0 {
			return errors.New("it names no properties")
		}
		marked, declared, excluded := map[string]int{}, map[string]int{}, map[string]int{}
		spec.EachSchema(func(where, path string, s *openapi.Schema) {
			property := openapi.PathName(path)
			if property == "" || s.Type != typ || !slices.Contains(names, property) {
				return
			}
			if slices.Contains(except[where], property) {
				excluded[where+" "+property]++

				return
			}
			if s.Format != "" {
				declared[property]++

				return
			}
			s.Format = format
			marked[property]++
		})
		for _, name := range names {
			switch {
			case marked[name] == 0 && declared[name] > 0:
				return fmt.Errorf("every %s is declared a %s now", name, format)
			case marked[name] == 0:
				return fmt.Errorf("no %s property or parameter is called %s", typ, name)
			}
			note(fmt.Sprintf("%s in %s", name, placesWord(marked[name])))
		}
		for _, where := range openapi.SortedKeys(except) {
			for _, name := range except[where] {
				if excluded[where+" "+name] == 0 {
					return fmt.Errorf("%s has no %s called %s to leave alone", where, typ, name)
				}
			}
		}

		return nil
	}}
}

// Enum is what UndeclaredEnums declares a property to be: its values, as the
// server writes them, and the name of the type the package holds it as
// (ContainerState), the property's own name capitalised when empty. In
// narrows it to some places, for a name whose values differ by place; empty
// is everywhere. A place is an operation ("GET /api/stacks") or a component
// schema ("Container"), and may name a path inside it: "GET /api/stacks
// .status" is the property at the top of the answer alone, "GET /api/stacks
// containerDetails.status" the one at that path, with lists adding nothing
// to a path, which is the form the sweep prints a value's place in.
type Enum struct {
	Name   string
	Values []string
	In     []string
}

// UndeclaredEnums is for the strings a document declares plain whose values
// the server draws from a fixed set (Dockhand's state, protocol, mode), so
// that the package holds them as a typed enum with its values as constants,
// and the sweep fails an answer carrying a value the set does not list. It
// adds a component schema for each enum and points every string property or
// parameter of the name at it. It fails for a name nothing has, for an enum
// with no values, and for a type name a different schema already has.
func UndeclaredEnums(about About, enums map[string]Enum) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(enums) == 0 {
			return errors.New("it names no properties")
		}
		for _, property := range openapi.SortedKeys(enums) {
			e := enums[property]
			if len(e.Values) == 0 {
				return fmt.Errorf("%s has no values", property)
			}
			name := e.Name
			if name == "" {
				name = strings.ToUpper(property[:1]) + property[1:]
			}
			values := make([]json.RawMessage, 0, len(e.Values))
			for _, v := range e.Values {
				values = append(values, json.RawMessage(strconv.Quote(v)))
			}
			if existing := spec.Components.Schemas[name]; existing != nil && !slices.Equal(existing.EnumValues(), e.Values) {
				return fmt.Errorf("schema %s exists and is not this enum", name)
			}
			if spec.Components.Schemas == nil {
				// a document that declares no schemas of its own (Dockhand's)
				spec.Components.Schemas = map[string]*openapi.Schema{}
			}
			spec.Components.Schemas[name] = &openapi.Schema{Type: openapi.TypeString, Enum: values}

			places := make([]openapi.Place, 0, len(e.In))
			for _, entry := range e.In {
				places = append(places, openapi.PlaceOf(entry))
			}
			marked, markedIn := 0, make([]int, len(places))
			spec.EachSchema(func(where, path string, s *openapi.Schema) {
				if openapi.PathName(path) != property || s.Type != openapi.TypeString || s.Format != "" || s.IsEnum() {
					return
				}
				if len(places) > 0 {
					i := slices.IndexFunc(places, func(p openapi.Place) bool { return p.Matches(where, path) })
					if i < 0 {
						return
					}
					markedIn[i]++
				}
				*s = openapi.Schema{Ref: openapi.SchemaRefPrefix + name, Description: s.Description, Nullable: s.Nullable}
				marked++
			})
			for i, n := range markedIn {
				if n == 0 {
					return fmt.Errorf("%s has no plain string called %s", e.In[i], property)
				}
			}
			if marked == 0 {
				return fmt.Errorf("no plain string property or parameter is called %s", property)
			}
			note(fmt.Sprintf("%s as %s in %s", property, name, placesWord(marked)))
		}

		return nil
	}}
}

// placesWord words a count of places.
func placesWord(n int) string {
	if n == 1 {
		return "1 place"
	}

	return fmt.Sprintf("%d places", n)
}

// Joined is what JoinedLists declares a string parameter to carry: a list of
// Of, an enum schema's name or "string", sent as one comma-joined value.
// In narrows it to some operations ("GET /Items"); empty is every
// operation that takes the parameter.
type Joined struct {
	Of string
	In []string
}

// JoinedLists is for the parameters a document declares as one string that
// the server reads as several values joined by commas (Emby's
// IncludeItemTypes), so that the package also takes a list, of an enum or of
// strings, and a caller hands two servers the same list where one takes it
// joined and the other repeated. The string stays as it is; the list is
// added beside it as <Field>List, sent under the same name joined, so no
// caller moves. It fails for a name no plain string parameter has, an Of
// that is not an enum schema of the document or "string", and an In that
// matches nothing.
func JoinedLists(about About, lists map[string]Joined) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(lists) == 0 {
			return errors.New("it names no parameters")
		}
		for _, name := range openapi.SortedKeys(lists) {
			j := lists[name]
			item := &openapi.Schema{Type: openapi.TypeString}
			if j.Of != "string" {
				e := spec.Components.Schemas[j.Of]
				if e == nil || !e.IsEnum() {
					return fmt.Errorf("%s: Of %q is not an enum schema of the document, or \"string\"", name, j.Of)
				}
				item = &openapi.Schema{Ref: openapi.SchemaRefPrefix + j.Of}
			}
			marked, already, inUsed := 0, 0, make([]bool, len(j.In))
			for _, path := range openapi.SortedKeys(spec.Paths) {
				p := spec.Paths[path]
				for method, op := range map[string]*openapi.Operation{"GET": p.Get, "POST": p.Post, "PUT": p.Put, "DELETE": p.Delete, "PATCH": p.Patch} {
					if op == nil {
						continue
					}
					where := method + " " + path
					in := -1
					if len(j.In) > 0 {
						in = slices.IndexFunc(j.In, func(entry string) bool { return openapi.PlaceOf(entry).Matches(where, name) })
						if in < 0 {
							continue
						}
					}
					if slices.ContainsFunc(op.Parameters, func(prm *openapi.Parameter) bool {
						return strings.EqualFold(prm.Name, name) && prm.Schema != nil && prm.Schema.Type == openapi.TypeArray
					}) {
						// the list is there already, declared by the document or by an earlier run of this
						already++

						continue
					}
					for _, prm := range op.Parameters {
						if !strings.EqualFold(prm.Name, name) || prm.In != openapi.InQuery || prm.Schema == nil || prm.Schema.Type != openapi.TypeString || prm.Schema.IsEnum() {
							continue
						}
						op.Parameters = append(op.Parameters, &openapi.Parameter{Name: prm.Name, In: openapi.InQuery, Description: strings.TrimSpace(prm.Description + "\n\nAs a list, sent joined by commas under the same name; set this or the string, not both."), Schema: &openapi.Schema{Type: openapi.TypeArray, Items: item}, Explode: new(false), Added: true, Field: openapi.Camel(prm.Name) + "List"})
						marked++
						if in >= 0 {
							inUsed[in] = true
						}
					}
				}
			}
			for i, used := range inUsed {
				if !used {
					return fmt.Errorf("%s: In %q matches no operation", name, j.In[i])
				}
			}
			switch {
			case marked == 0 && already > 0:
				return fmt.Errorf("every %s has its list already", name)
			case marked == 0:
				return fmt.Errorf("no plain string parameter is called %s", name)
			}
			note(fmt.Sprintf("%s as a list of %s in %s", name, j.Of, placesWord(marked)))
		}

		return nil
	}}
}

// NumberedPathIDs is for a path parameter a document types as text on one
// operation and as a number on another of the same path (Sonarr's PUT
// /tag/{id} takes a string where its GET and DELETE take an integer), which
// leaves a caller formatting a number it holds. The text one becomes the
// number the other declares, format kept. A parameter only ever typed as
// text, a log file's name, is left alone. It fails when it reaches nothing,
// which is how a document that was fixed is noticed.
func NumberedPathIDs(about About) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		marked := 0
		for _, path := range openapi.SortedKeys(spec.Paths) {
			methods := spec.Paths[path].Methods()
			for _, m := range methods {
				for _, prm := range m.Operation.Parameters {
					if prm.In != openapi.InPath || prm.Schema == nil || prm.Schema.Type != openapi.TypeString {
						continue
					}
					if number := numberedAs(methods, prm.Name); number != nil {
						prm.Schema.Type, prm.Schema.Format = number.Type, number.Format
						marked++
					}
				}
			}
		}
		if marked == 0 {
			return errors.New("no text path parameter has a number of the same name beside it")
		}
		note(fmt.Sprintf("the text id of %s is the number another operation of the path declares", placesWord(marked)))

		return nil
	}}
}

// numberedAs is the integer schema an operation of the path gives a path parameter of the name, or nil.
func numberedAs(methods []openapi.Method, name string) *openapi.Schema {
	for _, m := range methods {
		for _, prm := range m.Operation.Parameters {
			if prm.In == openapi.InPath && prm.Name == name && prm.Schema != nil && prm.Schema.Type == openapi.TypeInteger {
				return prm.Schema
			}
		}
	}

	return nil
}

// EnumValues is for an enum the document declares whose server answers a value it does not list (Sonarr's series status answering
// upcoming): each named component schema gains the values given, so the generated constant exists and Known accepts it and the
// sweep stops failing the answer. It fails for a schema that is not there or not an enum of strings, and for a value the document
// lists already, which is how a document that caught up retires the line.
func EnumValues(about About, values map[string][]string) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(values) == 0 {
			return errors.New("it names no enums")
		}
		for _, name := range openapi.SortedKeys(values) {
			s := spec.Components.Schemas[name]
			switch {
			case s == nil:
				return fmt.Errorf("no component schema is called %s", name)
			case !s.IsEnum():
				return fmt.Errorf("%s is not an enum of strings", name)
			case len(values[name]) == 0:
				return fmt.Errorf("%s gets no values", name)
			}
			listed := map[string]bool{}
			for _, raw := range s.Enum {
				var v string
				if json.Unmarshal(raw, &v) == nil {
					listed[v] = true
				}
			}
			for _, v := range values[name] {
				if listed[v] {
					return fmt.Errorf("%s lists %s already", name, v)
				}
				raw, _ := json.Marshal(v)
				s.Enum = append(s.Enum, raw)
			}
			note(fmt.Sprintf("%s added to %s", strings.Join(values[name], ", "), name))
		}

		return nil
	}}
}

// OneRecord is for a document that declares the same record inline wherever it appears (Dockhand's notification config, declared six
// times over as many operations), so that the generated package has a model per place and a caller converts between them. Each
// name is a model to make, and its places are where the record is declared, as a workaround's In names them: an operation and a path
// inside its answer or its body ("GET /api/notifications config", "POST /api/notifications config", the request and the answer both
// where an operation has the path on each). Every place becomes a reference to one component schema of the name, so the operations
// share one model. It fails for a name the document has a schema of already, a place that matches nothing, fewer than two places,
// and a place whose record differs from the first's, naming the property, which is the check a conversion never made.
func OneRecord(about About, records map[string][]string) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(records) == 0 {
			return errors.New("it names no records")
		}
		for _, name := range openapi.SortedKeys(records) {
			if spec.Components.Schemas[name] != nil {
				return fmt.Errorf("the document has a schema called %s already", name)
			}
			places := records[name]
			if len(places) < 2 {
				return fmt.Errorf("%s has %d place, and one record needs two or more", name, len(places))
			}
			var first *openapi.Schema
			firstAt := ""
			matched := make([]int, len(places))
			var differs error
			spec.EachSchema(func(where, path string, s *openapi.Schema) {
				if differs != nil || s.Ref != "" || s.Type != openapi.TypeObject {
					return
				}
				for i, entry := range places {
					if !openapi.PlaceOf(entry).Matches(where, path) {
						continue
					}
					matched[i]++
					if first == nil {
						// a copy, since the place itself becomes the reference
						first, firstAt = new(openapi.Schema), entry
						*first = *s
					} else if why := differ(first, s); why != "" {
						differs = fmt.Errorf("%s: the record at %s differs from the one at %s: %s", name, entry, firstAt, why)

						return
					}
					*s = openapi.Schema{Ref: openapi.SchemaRefPrefix + name}
				}
			})
			if differs != nil {
				return differs
			}
			for i, n := range matched {
				if n == 0 {
					return fmt.Errorf("%s: %s matches no inline object", name, places[i])
				}
			}
			if spec.Components.Schemas == nil {
				spec.Components.Schemas = map[string]*openapi.Schema{}
			}
			spec.Components.Schemas[name] = first
			note(fmt.Sprintf("%s declared once for %s", name, placesWord(len(places))))
		}

		return nil
	}}
}

// differ says how two inline records differ, by the first property that does, or "" when they are one shape: the same properties of
// the same types, descriptions aside.
func differ(a, b *openapi.Schema) string {
	for _, name := range openapi.SortedKeys(a.Properties) {
		if b.Properties[name] == nil {
			return "no " + name
		}
	}
	for _, name := range openapi.SortedKeys(b.Properties) {
		if a.Properties[name] == nil {
			return "an extra " + name
		}
	}
	for _, name := range openapi.SortedKeys(a.Properties) {
		if shapeOf(a.Properties[name]) != shapeOf(b.Properties[name]) {
			return name + " is " + shapeOf(b.Properties[name]) + ", not " + shapeOf(a.Properties[name])
		}
	}

	return ""
}

// shapeOf is a property's shape as text, descriptions left out.
func shapeOf(s *openapi.Schema) string {
	if s == nil {
		return "nothing"
	}
	if s.Ref != "" {
		return s.RefName()
	}
	var out strings.Builder
	out.WriteString(s.Type)
	if s.Format != "" {
		out.WriteString(" " + s.Format)
	}
	if s.Nullable {
		out.WriteString(" nullable")
	}
	if s.Items != nil {
		out.WriteString(" of " + shapeOf(s.Items))
	}
	for _, name := range openapi.SortedKeys(s.Properties) {
		out.WriteString(" " + name + ":" + shapeOf(s.Properties[name]))
	}

	return out.String()
}

// DeclaredSchemas is for a record a document leaves out altogether (the *arr's result of testing a provider), which a workaround
// then refers to by name: each schema is added to the document's components. It fails for a name the document has, which is how
// a document that caught up retires the line.
func DeclaredSchemas(about About, schemas map[string]*openapi.Schema) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(schemas) == 0 {
			return errors.New("it names no schemas")
		}
		for _, name := range openapi.SortedKeys(schemas) {
			if spec.Components.Schemas[name] != nil {
				return fmt.Errorf("the document declares %s", name)
			}
		}
		if spec.Components.Schemas == nil {
			spec.Components.Schemas = map[string]*openapi.Schema{}
		}
		for _, name := range openapi.SortedKeys(schemas) {
			if schemas[name] == nil {
				return fmt.Errorf("%s has no schema", name)
			}
			spec.Components.Schemas[name] = schemas[name]
		}
		note(strings.Join(openapi.SortedKeys(schemas), ", ") + " declared")

		return nil
	}}
}

// AlsoAnswers is for an operation that answers a body at a status its document gives none, success or not (the *arr's test of every
// provider: a list of results, documented as nothing, as a 200 when all pass and a 400 when any fails): answers says, by method and
// path, what each status answers, and a status the document does not list is added. With ExpectSameShape on the service, a failure
// status that answers the success's shape is decoded as an answer. It fails for a status that declares a body already.
func AlsoAnswers(about About, answers map[string]map[int]Answer) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		if len(answers) == 0 {
			return errors.New("it names no operations")
		}
		added := 0
		for _, target := range openapi.SortedKeys(answers) {
			op, err := targeted(spec, target)
			if err != nil {
				return err
			}
			if len(answers[target]) == 0 {
				return fmt.Errorf("%s gets no answers", target)
			}
			for _, status := range slices.Sorted(maps.Keys(answers[target])) {
				code := strconv.Itoa(status)
				resp := op.Responses[code]
				if resp != nil && len(resp.Content) > 0 {
					return fmt.Errorf("%s declares what a %d answers now, so take it out", target, status)
				}
				content, err := answers[target][status].content(spec)
				if err != nil {
					return fmt.Errorf("%s %d: %w", target, status, err)
				}
				if resp == nil {
					resp = &openapi.Response{Description: http.StatusText(status)}
					if op.Responses == nil {
						op.Responses = map[string]*openapi.Response{}
					}
					op.Responses[code] = resp
				}
				resp.Content = content
				added++
			}
		}
		note(answersWord(added) + " declared")

		return nil
	}}
}

// answersWord counts answers.
func answersWord(n int) string {
	if n == 1 {
		return "1 answer"
	}

	return fmt.Sprintf("%d answers", n)
}

// CommandSet is what Commands declares: Post is the operation that queues a command named in its body ("POST /api/v3/command"), Get
// the one that answers a queued command by its id, Status the field of that answer that says how far the command is, Finished the
// values that mean it is over and Failed those among them that mean it failed, and Commands each command's name as the server
// takes it with the fields its body carries beside the name.
type CommandSet struct {
	Post     string
	Get      string
	Status   string
	Finished []string
	Failed   []string
	Commands map[string]map[string]*openapi.Schema
}

// Commands is for a server that queues work by name through one operation whose document declares the body as an open record
// (the *arr's POST /api/v3/command, "{"name": "RefreshSeries", "seriesId": 12}"), so that a caller writes the name and the fields
// into a map nobody checks. Each command becomes a schema of its own, <Name>Command, with the fields given and the name, and the
// generated package has a method per command that fixes the name, an AndWait form that polls the queued command until the server
// says it finished, and WaitForCommand for one queued the plain way. It fails for an operation that is not there, a Post that takes
// no JSON body, a status with no finished values, a command with no name, a schema the document has, and a Post declared twice.
func Commands(about About, set CommandSet) Workaround {
	return defaultWorkaround{about, func(spec *openapi.Spec, note func(string)) error {
		post, err := targeted(spec, set.Post)
		if err != nil {
			return err
		}
		if _, err := targeted(spec, set.Get); err != nil {
			return err
		}
		switch {
		case post.Commands != nil:
			return fmt.Errorf("%s has its commands declared already", set.Post)
		case post.RequestBody == nil || post.RequestBody.Content[jsonMedia] == nil:
			return fmt.Errorf("%s takes no JSON body", set.Post)
		case set.Status == "" || len(set.Finished) == 0:
			return errors.New("a command set needs the Status field and the Finished values")
		case len(set.Commands) == 0:
			return errors.New("it names no commands")
		}
		for _, name := range openapi.SortedKeys(set.Commands) {
			if !token.IsIdentifier(name) || !token.IsExported(name) {
				return fmt.Errorf("the command %q is not an exported Go name", name)
			}
			if spec.Components.Schemas[name+"Command"] != nil {
				return fmt.Errorf("the document has a schema called %sCommand", name)
			}
		}
		if spec.Components.Schemas == nil {
			spec.Components.Schemas = map[string]*openapi.Schema{}
		}
		schemas := map[string]string{}
		for _, name := range openapi.SortedKeys(set.Commands) {
			properties := map[string]*openapi.Schema{"name": {Type: openapi.TypeString, Description: "The command's name, which the method sets."}}
			for field, s := range set.Commands[name] {
				if field == "name" {
					return fmt.Errorf("the command %s names a field name, which is the command's own", name)
				}
				properties[field] = s
			}
			spec.Components.Schemas[name+"Command"] = &openapi.Schema{Type: openapi.TypeObject, Description: "The " + name + " command, as " + set.Post + " queues it.", Properties: properties}
			schemas[name] = name + "Command"
		}
		post.Commands = &openapi.CommandSet{Get: set.Get, Status: set.Status, Finished: set.Finished, Failed: set.Failed, Commands: schemas}
		note(fmt.Sprintf("%d commands declared", len(schemas)))

		return nil
	}}
}
