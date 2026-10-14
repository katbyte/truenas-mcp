package main

// secrets.go writes api-defs/truenas-secrets-<release>.json: where the secrets are in each method of the TrueNAS API. The description a NAS gives
// of its API does not say which values are secrets; the TrueNAS source does, as a field typed Secret[...]. The source is read without being run
// (python.go), every method is followed to the models of what it takes and answers, and each secret is written as a place in the method's
// arguments ("$[0].password") or its answer ("$[].provider", or "$" for an answer that is a secret whole). Every place is then looked up in the
// NAS's own description, so a place this reading got wrong is reported, not written.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

const secretsUsage = `secrets writes api-defs/truenas-secrets-<release>.json from a checkout of the TrueNAS source at the release. The source's API version (api/v25_10_5) is the newest one in the checkout unless a third argument names another. From the repository root:

    git clone --depth 1 --branch TS-25.10.7 --filter=blob:none --sparse https://github.com/truenas/middleware.git ../middleware
    git -C ../middleware sparse-checkout set src/middlewared/middlewared/api src/middlewared/middlewared/plugins src/middlewared/middlewared/service
    go run ./sdk/pandorest secrets ../middleware 25.10.7`

var (
	// listTypes, dictTypes and emptyTypes are the types of the source that hold others: a list holds its secrets in each item, a dictionary in
	// each value, and a literal, a class or a function none
	listTypes  = []string{"list", "List", "set", "Set", "frozenset", "tuple", "Tuple", "UniqueList", "Sequence", "Iterable"}
	dictTypes  = []string{"dict", "Dict", "Mapping"}
	emptyTypes = []string{"Literal", "type", "Type", "Callable"}

	// madeTypes are what the source makes a method's answer with from the model of one entry
	madeTypes = []string{"query_result", "query_result_item", "get_instance_result", "config_result", "single_argument_result"}

	// shapeKeys are what a schema in the description says a value's shape with; one with none of them is described as anything at all
	shapeKeys = []string{"type", "properties", "items", "anyOf", "oneOf", "allOf", "additionalProperties", "enum", "const", "$ref"}

	fieldName = regexp.MustCompile(`^\.([A-Za-z0-9_]+)`)
	digits    = regexp.MustCompile(`\d+`)
)

// unmarkedSecrets are what the source does not mark and is a secret all the same, by the start of a method's name and the field's name. Each
// was found by reading what a NAS describes for names that sound like a secret, then the source for what the field holds. They are written
// under "unmarked", apart from what the source marks, because a NAS does not hide them from anyone: api-defs/README.md has the account of each.
var unmarkedSecrets = []struct {
	prefix string
	names  []string
}{
	{"ssh.", []string{"privatekey", "host_key", "host_dsa_key", "host_ecdsa_key", "host_ed25519_key", "host_rsa_key"}},
	{"replication.", []string{"encryption_key"}},
	{"keychaincredential.generate_ssh_key_pair", []string{"private_key"}},
}

// secrets writes truenas-secrets-<release>.json into defs, beside the release's description, from a checkout of the TrueNAS source. It fails
// when the source names a place the description has nothing at, has a method it could not follow, or has a file it could not read.
func secrets(ctx context.Context, defs string, args []string, stdout io.Writer) error {
	if len(args) < 2 || len(args) > 3 {
		return errors.New(secretsUsage)
	}
	checkout, release := args[0], args[1]
	root := filepath.Join(checkout, "src", "middlewared", "middlewared")

	var version string
	if len(args) == 3 {
		version = args[2]
	} else {
		var err error
		if version, err = newestAPI(root); err != nil {
			return err
		}
	}

	raw, err := os.ReadFile(filepath.Join(defs, "truenas-methods-"+release+".json")) //nolint:gosec // G304: the repository's own description of the release named
	if err != nil {
		return err
	}
	var document map[string]struct {
		Accepts []any `json:"accepts"`
		Returns []any `json:"returns"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}

	m, err := readModels(root, version)
	if err != nil {
		return err
	}
	declared, unreadable, err := m.services(root)
	if err != nil {
		return err
	}

	methods := slices.Sorted(maps.Keys(document))
	marked, kept := &object{}, map[string]map[string][]string{}
	var unread, wrong []string
	for _, method := range methods {
		d, ok := declared[method]
		if !ok {
			// a method added to a service from elsewhere, as each kind of cloud storage adds its own: its models are still named for it
			takes, answers, found := m.namedFor(method)
			if !found {
				unread = append(unread, method)

				continue
			}
			d = declaration{params: m.arguments(takes), result: places{}}
			if answers != "" {
				d.result = m.answer(answers)
			}
		}

		sides := map[string][]string{}
		for _, path := range d.params.sorted() {
			number, rest, _ := strings.Cut(strings.TrimPrefix(path, "$["), "]")
			index, err := strconv.Atoi(number)
			if accepts := document[method].Accepts; err == nil && index < len(accepts) && described(accepts[index], rest) {
				sides["params"] = append(sides["params"], path)
			} else {
				wrong = append(wrong, method+" params "+path)
			}
		}
		for _, path := range d.result.sorted() {
			if returns := document[method].Returns; len(returns) > 0 && described(returns[0], path[1:]) {
				sides["result"] = append(sides["result"], path)
			} else {
				wrong = append(wrong, method+" result "+path)
			}
		}

		if len(sides) > 0 {
			kept[method] = sides
			entry := &object{}
			for _, side := range []string{"params", "result"} {
				if len(sides[side]) > 0 {
					entry.set(side, sides[side])
				}
			}
			marked.set(method, entry)
		}
	}

	// what the source leaves unmarked is looked for in the description itself, so that a field of the same name on a method added later is found
	unmarked := &object{}
	for _, u := range unmarkedSecrets {
		found := 0
		for _, method := range methods {
			if !strings.HasPrefix(method, u.prefix) {
				continue
			}

			params, result := places{}, places{}
			for i, accepts := range document[method].Accepts {
				params.add(named(accepts, fmt.Sprintf("$[%d]", i), u.names))
			}
			if returns := document[method].Returns; len(returns) > 0 {
				result.add(named(returns[0], "$", u.names))
			}

			for _, side := range []struct {
				name  string
				found places
			}{{"params", params}, {"result", result}} {
				var more []string
				for _, path := range side.found.sorted() {
					if !under(path, kept[method][side.name]) {
						more = append(more, path)
					}
				}
				if len(more) == 0 {
					continue
				}

				entry := unmarked.child(method)
				entry.set(side.name, slices.Compact(slices.Sorted(slices.Values(slices.Concat(entry.list(side.name), more)))))
				found += len(more)
			}
		}

		if found == 0 {
			wrong = append(wrong, u.prefix+"* unmarked "+strings.Join(u.names, ", "))
		}
	}

	// the commit the checkout is at, which the file records; nothing when the checkout is not one git knows
	commit, _ := exec.CommandContext(ctx, "git", "-C", checkout, "rev-parse", "HEAD").Output() //nolint:gosec // G204: git, asked where the checkout named is

	source := &object{}
	source.set("repository", "https://github.com/truenas/middleware")
	source.set("tag", "TS-"+release)
	source.set("commit", strings.TrimSpace(string(commit)))
	source.set("api", version)
	result := &object{}
	result.set("source", source)
	result.set("methods", marked)
	result.set("unmarked", unmarked)
	result.set("not_read", unread)

	var out strings.Builder
	writeJSON(&out, result, "")
	out.WriteString("\n")
	target := filepath.Join(defs, "truenas-secrets-"+release+".json")
	if err := os.WriteFile(target, []byte(out.String()), 0o644); err != nil { //nolint:gosec // G306: the file is committed, for anyone to read
		return err
	}

	_, _ = fmt.Fprintf(stdout, "%s: %d secret places in %d of the %d methods the NAS describes (source %s)\n", filepath.Base(target), count(marked), len(marked.keys), len(document), version)
	_, _ = fmt.Fprintf(stdout, "secrets the source does not mark: %d places in %d methods\n", count(unmarked), len(unmarked.keys))
	_, _ = fmt.Fprintf(stdout, "methods the description has that this could not follow to their models: %d\n", len(unread))
	for _, method := range unread {
		_, _ = fmt.Fprintln(stdout, "  "+method)
	}
	_, _ = fmt.Fprintf(stdout, "places the source marks that the description has nothing at: %d\n", len(wrong))
	for _, place := range wrong {
		_, _ = fmt.Fprintln(stdout, "  "+place)
	}
	_, _ = fmt.Fprintf(stdout, "files of the source this could not read: %d\n", len(unreadable))
	for _, file := range unreadable {
		_, _ = fmt.Fprintln(stdout, "  "+file)
	}

	if len(unread) > 0 || len(wrong) > 0 || len(unreadable) > 0 {
		return fmt.Errorf("%d methods not followed, %d places not described and %d files not read", len(unread), len(wrong), len(unreadable))
	}

	return nil
}

// newestAPI is the newest version of the API the source has, by its numbers: v25_10_5 is newer than v25_10_4 and v25_04_2.
func newestAPI(root string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "api"))
	if err != nil {
		return "", err
	}

	var versions []string
	for _, e := range entries {
		if matched, _ := filepath.Match("v*_*", e.Name()); matched && e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return "", fmt.Errorf("%s has no version of the API", filepath.Join(root, "api"))
	}

	numbers := func(v string) []int {
		found := digits.FindAllString(v, -1)
		n := make([]int, 0, len(found))
		for _, d := range found {
			i, _ := strconv.Atoi(d)
			n = append(n, i)
		}

		return n
	}

	return slices.MaxFunc(versions, func(a, b string) int { return slices.Compare(numbers(a), numbers(b)) }), nil
}

// pythonFiles lists the Python files in a directory, and in those under it when deep is set, sorted as Python sorts paths: by their parts, so a
// directory's files come before those of another whose name only starts with the first's.
func pythonFiles(dir string, deep bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { //nolint:gosec // G703: a folder of the checkout named
		switch {
		case err != nil:
			return err
		case d.IsDir() && path != dir && !deep:
			return filepath.SkipDir
		case !d.IsDir() && strings.HasSuffix(d.Name(), ".py"):
			files = append(files, path)
		}

		return nil
	})
	slices.SortFunc(files, func(a, b string) int {
		return slices.Compare(strings.Split(a, string(filepath.Separator)), strings.Split(b, string(filepath.Separator)))
	})

	return files, err
}

// places are where secrets are in a value, as paths from it.
type places map[string]bool

func (p places) add(more places) {
	for path := range more {
		p[path] = true
	}
}

// prefixed is the places with something put before each.
func (p places) prefixed(prefix string) places {
	out := places{}
	for path := range p {
		out[prefix+path] = true
	}

	return out
}

func (p places) sorted() []string {
	return slices.Sorted(maps.Keys(p))
}

// declaration is where the secrets are in one method's arguments and in its answer.
type declaration struct {
	params places
	result places
}

// models are the model classes and the names given to types of one version of the API, as the source writes them.
type models struct {
	classes map[string]*model
	order   []string // the classes by when each was first met, which is the order Python keeps them in
	aliases map[string]*expr
	wrapped map[string]string // "args" for a model whose fields are a method's one argument, "result" for one whose fields are its answer
}

// model is one model class: the classes it is made from and its own fields.
type model struct {
	bases  []string
	fields fields
}

// fields are a model's fields by the name each is sent under, in the order they are written, a base's before the model's own.
type fields struct {
	names []string
	types map[string]*expr
}

// set adds a field, or gives one already there a new type and leaves it where it was.
func (f *fields) set(name string, t *expr) {
	if f.types == nil {
		f.types = map[string]*expr{}
	}
	if _, ok := f.types[name]; !ok {
		f.names = append(f.names, name)
	}
	f.types[name] = t
}

// readModels reads the models of one version of the API: those all versions share, then the version's own, which win.
func readModels(root, version string) (*models, error) {
	shared, err := pythonFiles(filepath.Join(root, "api", "base"), true)
	if err != nil {
		return nil, err
	}
	own, err := pythonFiles(filepath.Join(root, "api", version), false)
	if err != nil {
		return nil, err
	}

	m := &models{classes: map[string]*model{}, aliases: map[string]*expr{}, wrapped: map[string]string{}}
	for _, path := range slices.Concat(shared, own) {
		src, err := os.ReadFile(path) //nolint:gosec // G304: a file of the checkout named
		if err != nil {
			return nil, err
		}
		module, err := parseModule(string(src))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		m.read(module)
	}

	return m, nil
}

// read takes in the model classes and names given to types of one file: only those the file makes itself, not any inside a function or block.
func (m *models) read(module []*stmt) {
	for _, s := range module {
		switch {
		case s.kind == stmtClass:
			var own fields
			for _, item := range s.body {
				if item.kind != stmtAnnotated {
					continue
				}
				// a field is sent under its alias where it has one: pass_ in the source is "pass" to a caller
				name := item.name
				if alias := aliasOf(item); alias != "" {
					name = alias
				}
				own.set(name, item.annotation)
			}

			var bases []string
			for _, b := range s.bases {
				if name := nameOf(b); name != "" {
					bases = append(bases, name)
				}
			}
			if _, ok := m.classes[s.name]; !ok {
				m.order = append(m.order, s.name)
			}
			m.classes[s.name] = &model{bases: bases, fields: own}

			for _, d := range s.decorators {
				switch {
				case d.kind == exprCall && nameOf(d.of) == "single_argument_args":
					m.wrapped[s.name] = "args"
				case nameOf(d) == "single_argument_result" || d.kind == exprCall && nameOf(d.of) == "single_argument_result":
					m.wrapped[s.name] = "result"
				}
			}

		case s.kind == stmtAssign && s.targets == 1 && s.name != "", s.kind == stmtAnnotated && s.value != nil:
			m.aliases[s.name] = s.value
		}
	}
}

// aliasOf is the name a field is sent under when that is not its own, Field(alias="pass") as its value or inside its type, or "".
func aliasOf(field *stmt) string {
	var calls []*expr
	if field.value != nil && field.value.kind == exprCall {
		calls = append(calls, field.value)
	}
	for _, e := range walk(field.annotation) {
		if e.kind == exprCall {
			calls = append(calls, e)
		}
	}

	for _, call := range calls {
		if nameOf(call.of) != "Field" {
			continue
		}
		for _, k := range call.keywords {
			if k.name == "alias" && (k.value.kind == exprText || k.value.kind == exprConstant) {
				return k.value.text
			}
		}
	}

	return ""
}

// fieldsOf is a model's fields with those of the models it is made from, its own winning. seen is the models already counted, so one met twice
// is counted once.
func (m *models) fieldsOf(name string, seen map[string]bool) fields {
	c, ok := m.classes[name]
	if !ok || seen[name] {
		return fields{}
	}
	seen[name] = true

	var out fields
	for _, base := range c.bases {
		inherited := m.fieldsOf(base, seen)
		for _, field := range inherited.names {
			out.set(field, inherited.types[field])
		}
	}
	for _, field := range c.fields.names {
		out.set(field, c.fields.types[field])
	}

	return out
}

// secretsOf is where the secrets are in a value of a type: paths from the value, "" for the value itself. visiting is the types already being
// worked out, so that a type that holds itself ends.
func (m *models) secretsOf(e *expr, visiting []string) places {
	out := places{}
	if e == nil {
		return out
	}

	switch e.kind {
	case exprText:
		// a type named before it is made, written as text
		if inner, err := parseText(e.text); err == nil {
			return m.secretsOf(inner, visiting)
		}

	case exprOr:
		out.add(m.secretsOf(e.parts[0], visiting))
		out.add(m.secretsOf(e.parts[1], visiting))

	case exprSubscript:
		return m.subscripted(e, visiting)

	case exprCall:
		made := nameOf(e.of)
		if !slices.Contains(madeTypes, made) || len(e.parts) == 0 {
			return out
		}
		inside := m.secretsOf(e.parts[0], visiting)
		if made == "query_result" {
			inside.add(inside.prefixed("[]"))
		}

		return inside

	case exprName, exprAttribute:
		name := e.name
		if slices.Contains(visiting, name) {
			return out
		}
		deeper := append(slices.Clip(visiting), name)

		if _, ok := m.classes[name]; ok {
			f := m.fieldsOf(name, map[string]bool{})
			for _, field := range f.names {
				out.add(m.secretsOf(f.types[field], deeper).prefixed("." + field))
			}

			return out
		}
		if alias, ok := m.aliases[name]; ok {
			return m.secretsOf(alias, deeper)
		}

	default:
		// a constant, a tuple, a list, a sum: none of them is a type that holds a secret
	}

	return out
}

// subscripted is where the secrets are in a value of a type made from others: Secret[str], list[Model], Annotated[Model, ...] and the like.
func (m *models) subscripted(e *expr, visiting []string) places {
	out := places{}
	inner := []*expr{e.parts[0]}
	if e.parts[0].kind == exprTuple {
		inner = e.parts[0].parts
	}

	switch outer := nameOf(e.of); {
	case outer == "Secret":
		out[""] = true
	case slices.Contains(emptyTypes, outer):
	case outer == "Annotated" && len(inner) > 0:
		return m.secretsOf(inner[0], visiting)
	case slices.Contains(listTypes, outer):
		for _, part := range inner {
			out.add(m.secretsOf(part, visiting).prefixed("[]"))
		}
	case slices.Contains(dictTypes, outer) && len(inner) > 0:
		out.add(m.secretsOf(inner[len(inner)-1], visiting).prefixed(".*"))
	default:
		for _, part := range inner {
			out.add(m.secretsOf(part, visiting))
		}
	}

	return out
}

// namedFor is the models of a method's arguments and answer found by their names alone, for a method no service declares the usual way. The
// answer is "" when there is no model of it, and found is false when there is none of the arguments either.
func (m *models) namedFor(method string) (takes, answers string, found bool) {
	key := strings.ToLower(strings.NewReplacer(".", "", "_", "").Replace(method))
	for _, name := range m.order {
		if base, ok := strings.CutSuffix(name, "Args"); ok && strings.EqualFold(base, key) {
			_, isClass := m.classes[base+"Result"]
			_, isAlias := m.aliases[base+"Result"]
			if isClass || isAlias {
				return name, base + "Result", true
			}

			return name, "", true
		}
	}

	return "", "", false
}

// arguments is where the secrets are in a method's arguments, from the model of them: a field for each argument in order, or one argument
// that holds them all.
func (m *models) arguments(name string) places {
	out := places{}
	f := m.fieldsOf(name, map[string]bool{})
	for i, field := range f.names {
		found := m.secretsOf(f.types[field], []string{name})
		if m.wrapped[name] == "args" {
			out.add(found.prefixed("$[0]." + field))
		} else {
			out.add(found.prefixed(fmt.Sprintf("$[%d]", i)))
		}
	}

	return out
}

// answer is where the secrets are in a method's answer, from the model of it: its result field, or the model itself when it is the answer.
func (m *models) answer(name string) places {
	if m.wrapped[name] == "result" {
		out := places{}
		f := m.fieldsOf(name, map[string]bool{})
		for _, field := range f.names {
			out.add(m.secretsOf(f.types[field], []string{name}).prefixed("$." + field))
		}

		return out
	}
	if alias, ok := m.aliases[name]; ok {
		return m.secretsOf(alias, []string{name}).prefixed("$")
	}

	return m.secretsOf(m.fieldsOf(name, map[string]bool{}).types["result"], []string{name}).prefixed("$")
}

// entries is where the secrets are in what a query answers: a list of a model, one of it, or a count of it.
func (m *models) entries(name string) places {
	one := m.secretsOf(&expr{kind: exprName, name: name}, nil).prefixed("$")
	out := maps.Clone(one)
	for path := range one {
		out["$[]"+path[1:]] = true
	}

	return out
}

// services is every method the source's services declare, with where the secrets are in its arguments and answer, and the files that could
// not be read.
func (m *models) services(root string) (methods map[string]declaration, unreadable []string, err error) {
	deep, err := pythonFiles(filepath.Join(root, "plugins"), true)
	if err != nil {
		return nil, nil, err
	}
	flat, err := pythonFiles(filepath.Join(root, "service"), false)
	if err != nil {
		return nil, nil, err
	}

	methods = map[string]declaration{}
	for _, path := range slices.Concat(deep, flat) {
		src, err := os.ReadFile(path) //nolint:gosec // G304: a file of the checkout named
		if err != nil {
			return nil, nil, err
		}
		module, err := parseModule(string(src))
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", path, err))

			continue
		}

		for _, class := range classesOf(module) {
			m.service(class, methods)
		}
	}

	return methods, unreadable, nil
}

// service adds the methods one class of the source declares: those it decorates as methods of the API, and those a service of entries
// answers without declaring them.
func (m *models) service(class *stmt, methods map[string]declaration) {
	namespace := strings.ToLower(strings.TrimSuffix(class.name, "Service"))
	entry := ""
	for _, item := range class.body {
		if item.kind != stmtClass || item.name != "Config" {
			continue
		}
		for _, setting := range item.body {
			switch {
			case setting.kind != stmtAssign:
			case setting.name == "namespace" && setting.value.kind == exprText:
				namespace = setting.value.text
			case setting.name == "entry":
				entry = nameOf(setting.value)
			}
		}
	}

	if entry != "" {
		// a list of them, one of them, or its one record of settings; a method declared as well says for itself
		one := m.secretsOf(&expr{kind: exprName, name: entry}, nil).prefixed("$")
		for method, result := range map[string]places{"query": m.entries(entry), "get_instance": one, "config": one} {
			if _, ok := methods[namespace+"."+method]; !ok {
				methods[namespace+"."+method] = declaration{params: places{}, result: result}
			}
		}
	}

	for _, fn := range class.body {
		if fn.kind != stmtDef {
			continue
		}
		for _, d := range fn.decorators {
			if d.kind != exprCall || truthy(keywordOf(d, "private")) {
				continue
			}

			method := namespace + "." + strings.TrimPrefix(fn.name, "do_")
			switch nameOf(d.of) {
			case "api_method":
				if len(d.parts) >= 2 {
					methods[method] = declaration{params: m.arguments(nameOf(d.parts[0])), result: m.answer(nameOf(d.parts[1]))}
				}
			case "filterable_api_method":
				result := places{}
				if item := nameOf(keywordOf(d, "item")); item != "" {
					result = m.entries(item)
				}
				methods[method] = declaration{params: places{}, result: result}
			}
		}
	}
}

// keywordOf is the value a call gives a keyword argument, or nil when it gives none.
func keywordOf(call *expr, name string) *expr {
	for _, k := range call.keywords {
		if k.name == name {
			return k.value
		}
	}

	return nil
}

// truthy is whether an expression is a constant Python counts as true.
func truthy(e *expr) bool {
	return e != nil && (e.kind == exprText && e.text != "" || e.kind == exprConstant && e.truthy)
}

// described is whether the NAS's description of a value has anything at a path: the check that a place read from the source is a real one.
func described(schema any, path string) bool {
	if path == "" {
		return true
	}
	s, isObject := schema.(map[string]any)
	if !isObject {
		return false
	}
	// a value described as anything at all, a title and no shape: how the description leaves a model inside the single row a query can answer
	if !slices.ContainsFunc(shapeKeys, func(k string) bool { _, has := s[k]; return has }) {
		return true
	}

	if slices.ContainsFunc(alternatives(s), func(alternative any) bool { return described(alternative, path) }) {
		return true
	}

	if rest, ok := strings.CutPrefix(path, "[]"); ok {
		items := s["items"]
		if list, isList := items.([]any); isList {
			return slices.ContainsFunc(list, func(item any) bool { return described(item, rest) })
		}

		return items != nil && described(items, rest)
	}

	if rest, ok := strings.CutPrefix(path, ".*"); ok {
		// a dictionary of anything is described as an object and no more
		more := s["additionalProperties"]
		anything, isBool := more.(bool)
		inner, isSchema := more.(map[string]any)
		_, hasProperties := s["properties"]

		return isBool && anything || isSchema && described(inner, rest) || s["type"] == "object" && !hasProperties
	}

	found := fieldName.FindStringSubmatchIndex(path)
	if found == nil {
		return false
	}
	properties, hasProperties := s["properties"].(map[string]any)
	inside, has := properties[path[found[2]:found[3]]]

	return hasProperties && has && described(inside, path[found[1]:])
}

// alternatives are the schemas a value is described as one or all of.
func alternatives(s map[string]any) []any {
	var all []any
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if list, ok := s[key].([]any); ok {
			all = append(all, list...)
		}
	}

	return all
}

// named is the place of every field of one of these names in the NAS's description of a value, as paths.
func named(schema any, path string, names []string) places {
	out := places{}
	s, ok := schema.(map[string]any)
	if !ok {
		return out
	}

	for _, alternative := range alternatives(s) {
		out.add(named(alternative, path, names))
	}

	if properties, hasProperties := s["properties"].(map[string]any); hasProperties {
		for name, inside := range properties {
			if slices.Contains(names, name) {
				out[path+"."+name] = true
			} else {
				out.add(named(inside, path+"."+name, names))
			}
		}
	}

	items, ok := s["items"].([]any)
	if !ok {
		items = []any{s["items"]}
	}
	for _, item := range items {
		out.add(named(item, path+"[]", names))
	}

	return out
}

// under is whether a path is one of the marked places or inside one.
func under(path string, marked []string) bool {
	return slices.ContainsFunc(marked, func(m string) bool {
		return path == m || strings.HasPrefix(path, m+".") || strings.HasPrefix(path, m+"[")
	})
}

// count is how many places an object of methods holds.
func count(methods *object) int {
	n := 0
	for _, method := range methods.keys {
		sides := methods.child(method)
		for _, side := range sides.keys {
			n += len(sides.list(side))
		}
	}

	return n
}

// object is a JSON object whose members are written in the order they were first set, as Python writes a dictionary.
type object struct {
	keys   []string
	values map[string]any
}

func (o *object) set(key string, value any) {
	if o.values == nil {
		o.values = map[string]any{}
	}
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// child is the object at key, set to a new one first when there is none.
func (o *object) child(key string) *object {
	if inner, ok := o.values[key].(*object); ok {
		return inner
	}
	inner := &object{}
	o.set(key, inner)

	return inner
}

// list is the list of text at key, or nothing when there is none.
func (o *object) list(key string) []string {
	if list, ok := o.values[key].([]string); ok {
		return list
	}

	return nil
}

// writeJSON writes a value as Python's json.dumps(indent=2) does, so the file reads the same whichever wrote it: text, a list of text, or an
// object of either.
func writeJSON(out *strings.Builder, value any, indent string) {
	switch v := value.(type) {
	case string:
		out.WriteString(quoted(v))

	case []string:
		if len(v) == 0 {
			out.WriteString("[]")

			return
		}
		out.WriteString("[")
		for i, s := range v {
			if i > 0 {
				out.WriteString(",")
			}
			out.WriteString("\n" + indent + "  " + quoted(s))
		}
		out.WriteString("\n" + indent + "]")

	case *object:
		if len(v.keys) == 0 {
			out.WriteString("{}")

			return
		}
		out.WriteString("{")
		for i, key := range v.keys {
			if i > 0 {
				out.WriteString(",")
			}
			out.WriteString("\n" + indent + "  " + quoted(key) + ": ")
			writeJSON(out, v.values[key], indent+"  ")
		}
		out.WriteString("\n" + indent + "}")
	}
}

// quoted is text as a JSON string, with everything but printable ASCII written as an escape, as Python writes it.
func quoted(s string) string {
	var out strings.Builder
	out.WriteString(`"`)
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			out.WriteString(`\` + string(r))
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\t':
			out.WriteString(`\t`)
		case r == '\b':
			out.WriteString(`\b`)
		case r == '\f':
			out.WriteString(`\f`)
		case r < 0x20 || r > 0x7e:
			for _, unit := range utf16.Encode([]rune{r}) {
				_, _ = fmt.Fprintf(&out, `\u%04x`, unit)
			}
		default:
			out.WriteRune(r)
		}
	}
	out.WriteString(`"`)

	return out.String()
}
