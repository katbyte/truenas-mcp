// Package described reads the description a TrueNAS gives of its own API (api-defs) for the tests: which methods there are, how each can be
// tested, and whether an answer a real NAS gave is the answer the description says it gives. Everything generated from the description is only
// as right as the description is, so the live tests hold a real NAS's answers to it, and whatever it gets wrong is found here and not in a
// program that could not read an answer.
package described

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Method is one method as the description has it.
type Method struct {
	Name    string            `json:"-"`
	Job     bool              `json:"job"`
	Roles   []string          `json:"roles"`
	Accepts []json.RawMessage `json:"accepts"`
	Returns []json.RawMessage `json:"returns"`
	// Downloadable and Uploadable say the method hands a file back or takes one, by a web request and not as a call
	Downloadable bool `json:"downloadable"`
	Uploadable   bool `json:"uploadable"`
}

// Kind is how a method can be tested, by what it is.
type Kind string

const (
	// Read is a method that changes nothing and needs no argument: it can simply be called.
	Read Kind = "read"
	// ReadWithArguments is a read that has to be told what to read: an id, a path, a name.
	ReadWithArguments Kind = "read with arguments"
	// ReadJob is a read the NAS runs as a job.
	ReadJob Kind = "read job"
	// Write is a method that changes the NAS.
	Write Kind = "write"
	// File is a method that hands a file back or takes one.
	File Kind = "file"
	// Session is a method to do with the connection itself, which the description lists no role for: logging in, subscribing, asking after jobs.
	Session Kind = "session"
)

// readRole is the role that may read everything and change nothing: a method it may call is a read.
const readRole = "READONLY_ADMIN"

// notReads are the methods the read role may call that change something all the same: a user who may only read may still make, change and
// remove keys of their own, and send a ticket to TrueNAS's support. The role says who may call a method, not that it only reads.
var notReads = []string{"api_key.create", "api_key.update", "api_key.delete", "support.new_ticket", "support.attach_ticket"}

// Load reads the description of the release named, as api-defs keeps it, from the directory given.
func Load(dir, release string) (map[string]Method, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "truenas-methods-"+release+".json")) //nolint:gosec // G304: the file is the repository's own
	if err != nil {
		return nil, err
	}

	var methods map[string]Method
	if err := json.Unmarshal(raw, &methods); err != nil {
		return nil, fmt.Errorf("the description of %s cannot be read: %w", release, err)
	}
	for name, m := range methods {
		m.Name = name
		methods[name] = m
	}

	return methods, nil
}

// Kind is how the method can be tested.
func (m Method) Kind() Kind {
	read := slices.Contains(m.Roles, readRole) && !slices.Contains(notReads, m.Name)

	switch {
	case m.Downloadable || m.Uploadable:
		return File
	case len(m.Roles) == 0:
		return Session
	case !read:
		return Write
	case m.Job:
		return ReadJob
	case m.needsArguments():
		return ReadWithArguments
	default:
		return Read
	}
}

// needsArguments reports whether the method has an argument it cannot be called without.
func (m Method) needsArguments() bool {
	return slices.ContainsFunc(m.Accepts, func(raw json.RawMessage) bool {
		var argument struct {
			Required bool `json:"_required_"`
		}

		return json.Unmarshal(raw, &argument) == nil && argument.Required
	})
}

// schema is as much of JSON Schema as the description uses to say what a method answers.
type schema struct {
	Type                 json.RawMessage            `json:"type"`
	AnyOf                []json.RawMessage          `json:"anyOf"`
	OneOf                []json.RawMessage          `json:"oneOf"`
	Properties           map[string]json.RawMessage `json:"properties"`
	PatternProperties    map[string]json.RawMessage `json:"patternProperties"`
	Required             []string                   `json:"required"`
	Items                []json.RawMessage          `json:"items"`
	Enum                 []json.RawMessage          `json:"enum"`
	Const                json.RawMessage            `json:"const"`
	AdditionalProperties json.RawMessage            `json:"additionalProperties"`
}

// Differences says where an answer a NAS gave is not what the method's description says it answers: each one names the place in the answer and
// what is wrong there, once however many rows are wrong the same way. No differences is an answer the description describes.
func (m Method) Differences(answer json.RawMessage) []string {
	if len(m.Returns) != 1 {
		return []string{fmt.Sprintf("the description gives %d shapes for the answer, and one is expected", len(m.Returns))}
	}

	value, err := read(answer)
	if err != nil {
		return []string{"the answer is not JSON: " + err.Error()}
	}

	found := differences(m.Returns[0], value, "$")
	slices.Sort(found)

	return slices.Compact(found)
}

// Forms says where an answer holds a value a NAS writes in a form of its own, an object with one name that starts with a dollar, as it does a
// moment in time ({"$date": ...}): each is the place and the name, once however many rows have it. The description calls these text, or a
// number, so whatever reads an answer has to know them.
func Forms(answer json.RawMessage) []string {
	value, err := read(answer)
	if err != nil {
		return nil
	}

	var found []string
	var walk func(value any, at string)
	walk = func(value any, at string) {
		switch v := value.(type) {
		case map[string]any:
			if name, ok := form(v); ok {
				found = append(found, at+" is a "+name)

				return
			}
			for _, name := range sorted(v) {
				walk(v[name], at+"."+name)
			}

		case []any:
			for _, item := range v {
				walk(item, at+"[]")
			}
		}
	}
	walk(value, "$")
	slices.Sort(found)

	return slices.Compact(found)
}

// KeptBack is what a NAS answers in place of a secret the caller may not see, whatever kind of value the secret is.
const KeptBack = "********"

// Blanked says where an answer holds a secret the NAS kept back, each place once, written the way api-defs writes where the secrets are.
func Blanked(answer json.RawMessage) []string {
	value, err := read(answer)
	if err != nil {
		return nil
	}

	var found []string
	var walk func(value any, at string)
	walk = func(value any, at string) {
		switch v := value.(type) {
		case string:
			if v == KeptBack {
				found = append(found, at)
			}

		case map[string]any:
			for _, name := range sorted(v) {
				walk(v[name], at+"."+name)
			}

		case []any:
			for _, item := range v {
				walk(item, at+"[]")
			}
		}
	}
	walk(value, "$")
	slices.Sort(found)

	return slices.Compact(found)
}

// Secrets is where the secrets are in each method's answer, as api-defs keeps them for the release named: made from the TrueNAS source, which
// marks them, where the description does not.
func Secrets(dir, release string) (map[string][]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "truenas-secrets-"+release+".json")) //nolint:gosec // G304: the file is the repository's own
	if err != nil {
		return nil, err
	}

	var kept struct {
		Methods map[string]struct {
			Result []string `json:"result"`
		} `json:"methods"`
	}
	if err := json.Unmarshal(raw, &kept); err != nil {
		return nil, fmt.Errorf("the secrets of %s cannot be read: %w", release, err)
	}

	places := map[string][]string{}
	for name, method := range kept.Methods {
		if len(method.Result) > 0 {
			places[name] = method.Result
		}
	}

	return places, nil
}

func read(answer json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(answer))
	dec.UseNumber()

	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}

	return value, nil
}

// form is the name a value written in a form of the NAS's own goes by, if it is one.
func form(object map[string]any) (string, bool) {
	if len(object) != 1 {
		return "", false
	}
	for name := range object {
		if strings.HasPrefix(name, "$") {
			return name, true
		}
	}

	return "", false
}

func differences(described json.RawMessage, value any, at string) []string {
	var s schema
	if err := json.Unmarshal(described, &s); err != nil {
		// a shape that cannot be read as one claims nothing
		return nil
	}

	// a secret the NAS kept back is one piece of text whatever the description says is there, and is told of by KeptBack
	if value == KeptBack {
		return nil
	}

	if options := append(slices.Clone(s.AnyOf), s.OneOf...); len(options) > 0 {
		// what is wrong is said against the shape the answer is nearest to: one of its own kind, a list held to the list and not to the one row
		// or the count a query may also answer, and of those the one with the least wrong
		var closest []string
		var nearest bool
		for i, option := range options {
			found := differences(option, value, at)
			if len(found) == 0 {
				return nil
			}

			var one schema
			var kind string
			near := json.Unmarshal(option, &one) == nil && (json.Unmarshal(one.Type, &kind) != nil || fits(kind, value))
			if i == 0 || (near && !nearest) || (near == nearest && len(found) < len(closest)) {
				closest, nearest = found, near
			}
		}

		return closest
	}

	// a value in a form of the NAS's own is not what the description says, every time, and is told of by Forms
	if object, ok := value.(map[string]any); ok {
		if _, own := form(object); own {
			return nil
		}
	}

	var kind string
	if json.Unmarshal(s.Type, &kind) != nil {
		kind = ""
	}
	if kind != "" && !fits(kind, value) {
		return []string{fmt.Sprintf("%s is %s, and the description says %s", at, is(value), kind)}
	}

	allowed := s.Enum
	if len(s.Const) > 0 {
		allowed = []json.RawMessage{s.Const}
	}
	if len(allowed) > 0 {
		if written, err := json.Marshal(value); err == nil && !slices.ContainsFunc(allowed, func(one json.RawMessage) bool { return same(one, written) }) {
			return []string{fmt.Sprintf("%s is %s, which is not one of the %d values the description allows", at, cut(string(written)), len(allowed))}
		}
	}

	var found []string
	switch v := value.(type) {
	case map[string]any:
		for _, name := range s.Required {
			if _, has := v[name]; !has {
				found = append(found, fmt.Sprintf("%s has no %s, which the description says it always has", at, name))
			}
		}

		// names the description gives no shape for are fine unless it says there are none
		closed, rest := false, json.RawMessage(nil)
		switch more := string(bytes.TrimSpace(s.AdditionalProperties)); more {
		case "false":
			closed = len(s.PatternProperties) == 0
		case "", "true":
		default:
			rest = s.AdditionalProperties
		}

		for _, name := range sorted(v) {
			switch property, named := s.Properties[name]; {
			case named:
				found = append(found, differences(property, v[name], at+"."+name)...)
			case rest != nil:
				found = append(found, differences(rest, v[name], at+".*")...)
			case closed:
				found = append(found, fmt.Sprintf("%s has a %s, which the description does not name", at, name))
			}
		}

	case []any:
		if len(s.Items) != 1 {
			break
		}
		for _, item := range v {
			found = append(found, differences(s.Items[0], item, at+"[]")...)
		}
	}

	return found
}

// same reports whether two values written as JSON are the same value, however each was spaced.
func same(a, b json.RawMessage) bool {
	var one, other bytes.Buffer
	if json.Compact(&one, a) != nil || json.Compact(&other, b) != nil {
		return false
	}

	return bytes.Equal(one.Bytes(), other.Bytes())
}

// fits reports whether a value is of the kind the description names.
func fits(kind string, value any) bool {
	switch kind {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)

		return ok
	case "boolean":
		_, ok := value.(bool)

		return ok
	case "object":
		_, ok := value.(map[string]any)

		return ok
	case "array":
		_, ok := value.([]any)

		return ok
	case "number", "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		if kind == "number" {
			return true
		}

		whole, err := number.Float64()

		return err == nil && whole == math.Trunc(whole)
	default:
		return true
	}
}

// is names the kind of a value, in the description's words.
func is(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number (" + v.String() + ")"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func sorted(object map[string]any) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

func cut(text string) string {
	if len(text) > 60 {
		return text[:60] + "..."
	}

	return text
}

// known are the words of a shape the checking acts on, and unheld those it knows of and holds no answer to: they say how a value is written or
// what it is for, not what it may be, or bound a length or a size that only an argument is held to.
var (
	known  = []string{"type", "anyOf", "oneOf", "properties", "patternProperties", "required", "items", "enum", "const", "additionalProperties"}
	unheld = []string{"title", "description", "default", "examples", "format", "discriminator", "prefixItems", "minLength", "maxLength", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minItems", "maxItems", "uniqueItems", "pattern", "writeOnly", "readOnly", "deprecated", "_name_", "_required_", "_attrs_order_"}
)

// Unknown says which words of a shape the checking does not know, each with where it is: a description that starts to use one is making a claim
// no answer is held to until it is taught.
func Unknown(described json.RawMessage) []string {
	var found []string

	var walk func(raw json.RawMessage, at string)
	walk = func(raw json.RawMessage, at string) {
		var shape map[string]json.RawMessage
		if json.Unmarshal(raw, &shape) != nil {
			return
		}

		for word, value := range shape {
			switch {
			case slices.Contains([]string{"properties", "patternProperties"}, word):
				var named map[string]json.RawMessage
				if json.Unmarshal(value, &named) != nil {
					found = append(found, fmt.Sprintf("%s: %s is not a set of named shapes", at, word))

					continue
				}
				for name, one := range named {
					walk(one, at+"."+name)
				}

			case slices.Contains([]string{"anyOf", "oneOf", "items", "prefixItems"}, word):
				var several []json.RawMessage
				if json.Unmarshal(value, &several) != nil {
					found = append(found, fmt.Sprintf("%s: %s is not a list of shapes", at, word))

					continue
				}
				for _, one := range several {
					walk(one, at+"[]")
				}

			case word == "additionalProperties":
				walk(value, at+".*")

			case word == "type":
				var kind string
				if json.Unmarshal(value, &kind) != nil {
					found = append(found, fmt.Sprintf("%s: the type is %s, and the checking reads one name", at, cut(string(value))))
				}

			case !slices.Contains(known, word) && !slices.Contains(unheld, word):
				found = append(found, fmt.Sprintf("%s: the word %q is one the checking does not know", at, word))
			}
		}
	}
	walk(described, "$")
	slices.Sort(found)

	return slices.Compact(found)
}
