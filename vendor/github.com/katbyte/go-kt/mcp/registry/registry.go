// Package registry decides which of an MCP server's tools a session gets, what each tells a client about itself, and what stands around every call.
//
// Each tool is added with a kind: read, write, or delete for what cannot be put back. A read-only session gets the read tools; the delete tools wait
// for the operator to ask; toolsets, an allow list and a deny list narrow the rest. Around every call: a panic becomes an error, empty lists go out
// as [], a line is written for each write with its credentials blanked, and a failure's words and, when asked, an answer's are cleaned of what should
// not reach a model. The tools themselves are the application's (Config).
package registry

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/katbyte/go-kt/chttp"
	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/go-kt/internal/addresses"
	"github.com/katbyte/go-kt/outside"
)

// Kind is what a tool does to the server it works on.
type Kind int

const (
	// Read never changes what the server holds.
	Read Kind = iota
	// Write changes it, and is left out of a read-only session.
	Write
	// Delete removes what cannot be put back, and is registered only when the operator asks for the delete tools.
	Delete
)

func (k Kind) String() string {
	return [...]string{"read", "write", "delete"}[k]
}

// Core is the toolset every other assumes, enough to find things and open them; it is added to whatever a session asks for.
const Core = "core"

// All is the toolset name that asks for every tool.
const All = "all"

// Essential is the name an allow list asks for the application's preset by.
const Essential = "essential"

// Hints are what a tool tells a client about itself beyond its kind, as MCP annotations. The zero value warns: a write tool with no hints is marked
// destructive.
type Hints struct {
	// Additive is a write tool that only ever adds, a new library or playlist, and never changes or removes what was there.
	Additive bool
	// Idempotent is a write tool that changes nothing more when called again with the same arguments.
	Idempotent bool
	// SendsOut is a tool that itself sends something to a person or a service beyond the server: an email, a notification. Making the server fetch
	// from its own providers does not count. MCP calls this "open world".
	SendsOut bool
	// Installs is a tool that has the server bring something in from outside and run it, an app or a plug-in. MCP marks it "open world" too.
	Installs bool
	// WritesHere is a read tool that writes a file on the machine it runs on: still a read tool, kept in a read-only session, but not one that claims
	// to change nothing.
	WritesHere bool
}

// Config is the application's side of a registry.
type Config struct {
	// Toolsets are the curated groups by name, each tool in one of them. The one named Core is added to whatever else is asked for.
	Toolsets map[string][]string
	// Essential is the preset an allow list names as "essential".
	Essential []string
	// Hints are the tools' hints by name.
	Hints map[string]Hints
	// LogError is where a handler's panic and its stack go; nil is the shared log, on stderr, since stdout carries the protocol over stdio.
	LogError func(format string, args ...any)
	// LogWrite is where one line goes for every call of a write or a delete tool: the tool, what it was sent with credentials blanked, and how it
	// ended. nil is the shared log at info, which a tool does not show at its default level: the line is there for whoever sets the log to info.
	LogWrite func(format string, args ...any)
	// SecretArguments are argument names, at any depth, whose values the write line blanks whole, whatever their type: a file's text, a command and
	// its flags, an address whose path is the secret. Blanked unasked are arguments named as a credential (chttp.SecretName), everything under a
	// free-form object such as a settings map, and in any address the sign-in, the query values and the fragment.
	SecretArguments []string
	// ShownArguments are, by tool, the free-form arguments that hold no credential and are written as sent: a server's own settings, checked against
	// a list. They are shown only for a call the tool answered: one it refused or failed was never held to that list, so it is blanked like any
	// other. A credential-named field under one is always blanked.
	ShownArguments map[string][]string
	// CleanAnswers, when above 0, has every piece of text in every answer and refusal cleaned as text from outside is (outside.Text) and cut to this
	// many characters, once, here, rather than by each tool. It stays off until a server has named what to leave as it is.
	CleanAnswers int
	// LeaveAsIs are the answer fields, by JSON name, that a later call hands back: a path, a release's id. They must go back as they came, so they
	// and everything under them are left uncleaned (LeaveAsIsUnused finds a stale name).
	LeaveAsIs []string
	// BlankAddresses are the answer fields, by JSON name, that carry a server's own words for what went wrong: a failed test's failures, a health
	// check's message, a log's lines. A server quotes an address there with its stored key on it, so addresses under these names lose their sign-in,
	// query values and fragment (outside.BlankAddresses). Other answers are left alone: a link a caller follows needs its query. BlankAddressesUnused
	// finds a stale name.
	BlankAddresses []string
}

// Selection is what one session asks for.
type Selection struct {
	// ReadOnly keeps the read tools alone.
	ReadOnly bool
	// EnableDelete lets the delete tools in.
	EnableDelete bool
	// Toolsets are the sets asked for, each entry a name or several joined by commas: a curated set, All, or a resource family ("library" for
	// library_*). None is every tool.
	Toolsets []string
	// Allow asks for tools by name: exact, a pattern with one "*" at either end, or Essential. With Toolsets it adds to them; alone it is only these,
	// with no Core. It lets nothing past ReadOnly or EnableDelete.
	Allow []string
	// Deny takes tools out, named the same way, from whatever was asked for.
	Deny []string
}

// Info describes a tool a selection would register.
type Info struct {
	Name string
	// Kind is read, write or delete
	Kind string
	// Toolset is the curated set the tool belongs to
	Toolset     string
	Description string
}

type pending struct {
	name        string
	kind        Kind
	description string
	// arguments are the tool's argument names, in declared order
	arguments []string
	// loose are the free-form argument names at any depth, and looseAnswered those less the ones in ShownArguments, for a call the tool answered;
	// looseInput is a tool whose whole input is free-form
	loose         map[string]bool
	looseAnswered map[string]bool
	looseInput    bool
	// answers are the JSON names the tool answers under, however deep
	answers  map[string]bool
	register func(*mcp.Server)
}

// Registry collects an application's tools before any is registered, so a selection is checked against all of them: a pattern that names no tool is
// refused rather than quietly hiding one.
type Registry struct {
	cfg     Config
	pending []pending
	// leave is LeaveAsIs and quoted BlankAddresses, as sets
	leave  map[string]bool
	quoted map[string]bool
}

// New makes an empty registry.
func New(cfg Config) *Registry {
	r := &Registry{cfg: cfg, leave: map[string]bool{}, quoted: map[string]bool{}}
	for _, name := range cfg.LeaveAsIs {
		r.leave[name] = true
	}
	for _, name := range cfg.BlankAddresses {
		r.quoted[name] = true
	}

	return r
}

// Add queues a typed tool with its annotations set from its kind and hints, and wraps it: a panic becomes a tool error, an empty list or map goes out
// as [] or {} rather than null, and the answer is cleaned as the Config asks. A tool whose input is any takes no arguments and now says so in its
// schema, where the SDK's would accept anything.
func Add[In, Out any](r *Registry, kind Kind, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	hints := r.cfg.Hints[t.Name]
	openWorld := hints.SendsOut || hints.Installs
	switch kind {
	case Read:
		t.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: !hints.WritesHere, DestructiveHint: new(false), OpenWorldHint: new(openWorld)}
	case Write:
		t.Annotations = &mcp.ToolAnnotations{DestructiveHint: new(!hints.Additive), IdempotentHint: hints.Idempotent, OpenWorldHint: new(openWorld)}
	case Delete:
		t.Annotations = &mcp.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(openWorld)}
	}

	if reflect.TypeFor[In]() == reflect.TypeFor[any]() && t.InputSchema == nil {
		t.InputSchema = map[string]any{"type": "object", "additionalProperties": false}
	}

	wrapped := func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		res, out, err := recovered(ctx, r, t.Name, h, req, in)
		if err != nil {
			return res, out, r.cleanFailure(err)
		}

		answer := reflect.ValueOf(&out).Elem()
		emptyNils(answer)
		r.cleanAnswer(res, answer)

		return res, out, nil
	}

	loose := looseNames(reflect.TypeFor[In](), map[string]bool{}, map[reflect.Type]bool{})
	looseAnswered := maps.Clone(loose)
	for _, name := range r.cfg.ShownArguments[t.Name] {
		delete(looseAnswered, name)
	}

	r.pending = append(r.pending, pending{
		name:          t.Name,
		kind:          kind,
		description:   t.Description,
		arguments:     argumentNames(reflect.TypeFor[In]()),
		loose:         loose,
		looseAnswered: looseAnswered,
		looseInput:    freeForm(reflect.TypeFor[In]()),
		answers:       answerNames(reflect.TypeFor[Out](), map[string]bool{}, map[reflect.Type]bool{}),
		register:      func(server *mcp.Server) { mcp.AddTool(server, t, wrapped) },
	})
}

// freeForm reports whether a type carries names of someone else's choosing: a map, an untyped value, or a list of either.
func freeForm(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}

	return t.Kind() == reflect.Map || t.Kind() == reflect.Interface
}

// looseNames adds to names the JSON names of an input's free-form fields, however deep; seen stops a type that holds itself from looping.
func looseNames(t reflect.Type, names map[string]bool, seen map[reflect.Type]bool) map[string]bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return names
	}
	seen[t] = true

	for f := range t.Fields() {
		name := jsonName(f)
		switch {
		case name == "-", !f.IsExported() && !f.Anonymous:
		case freeForm(f.Type):
			names[name] = true
		default:
			looseNames(f.Type, names, seen)
		}
	}

	return names
}

// jsonName is the name a field is answered under: its tag's, its own when untagged, "" for an untagged embedded struct, "-" for one kept out.
func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" && !f.Anonymous {
		return f.Name
	}

	return name
}

// answerNames adds to names the JSON names a type answers under, however deep; seen stops a type that holds itself from looping.
func answerNames(t reflect.Type, names map[string]bool, seen map[reflect.Type]bool) map[string]bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		answerNames(t.Elem(), names, seen)
	case reflect.Struct:
		if seen[t] {
			return names
		}
		seen[t] = true
		for f := range t.Fields() {
			name := jsonName(f)
			if name == "-" || !f.IsExported() && !f.Anonymous {
				continue
			}
			if name != "" {
				names[name] = true
			}
			answerNames(f.Type, names, seen)
		}
	default:
	}

	return names
}

// LeaveAsIsUnused is the names in Config.LeaveAsIs that no tool answers a field under, for a server's test to hold to none: a name left behind by a
// rename protects nothing. Only fields count; a name used as a map key is reported too.
func (r *Registry) LeaveAsIsUnused() []string { return r.unused(r.cfg.LeaveAsIs) }

// BlankAddressesUnused is the same check for Config.BlankAddresses.
func (r *Registry) BlankAddressesUnused() []string { return r.unused(r.cfg.BlankAddresses) }

// unused is the names among these that no tool answers a field under.
func (r *Registry) unused(names []string) []string {
	var unused []string
	for _, name := range names {
		if !slices.ContainsFunc(r.pending, func(p pending) bool { return p.answers[name] }) {
			unused = append(unused, name)
		}
	}

	return unused
}

// cleanAnswer cleans a tool's answer in place as the Config asks: every piece of text (CleanAnswers), and the addresses under the named fields
// (BlankAddresses).
func (r *Registry) cleanAnswer(res *mcp.CallToolResult, answer reflect.Value) {
	if r.cfg.CleanAnswers <= 0 && len(r.quoted) == 0 {
		return
	}

	if res != nil {
		for _, c := range res.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				text.Text = r.cleaned(text.Text)
			}
		}
	}
	r.clean(answer, r.cleaned)
}

// cleaned is a piece of text cleaned when CleanAnswers asks, else as it was.
func (r *Registry) cleaned(text string) string {
	if r.cfg.CleanAnswers <= 0 {
		return text
	}

	return outside.Text(text, r.cfg.CleanAnswers)
}

// unquoted is a server's own words cleaned and then with their addresses blanked; cleaned first, so a hidden character cannot keep an address from
// being seen as one.
func (r *Registry) unquoted(text string) string { return outside.BlankAddresses(r.cleaned(text)) }

// clean applies text to every piece of text a value holds, however deep, skipping what is under a LeaveAsIs name and switching to unquoted under a
// BlankAddresses name. Raw JSON a tool passes on is bytes and is left.
func (r *Registry) clean(v reflect.Value, text func(string) string) {
	switch v.Kind() {
	case reflect.String:
		if s := text(v.String()); v.CanSet() && s != v.String() {
			v.SetString(s)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			r.clean(v.Elem(), text)
		}
	case reflect.Interface:
		// what an interface holds cannot be changed where it is, so a copy is cleaned and put in its place
		if v.IsNil() || !v.CanSet() {
			return
		}
		held := reflect.New(v.Elem().Type()).Elem()
		held.Set(v.Elem())
		r.clean(held, text)
		v.Set(held)
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if name := jsonName(f); name != "-" && (f.IsExported() || f.Anonymous) && !r.leave[name] {
				r.clean(v.Field(i), r.under(name, text))
			}
		}
	case reflect.Slice, reflect.Array:
		if holdsText(v.Type().Elem()) {
			for i := range v.Len() {
				r.clean(v.Index(i), text)
			}
		}
	case reflect.Map:
		r.cleanMap(v, text)
	default:
		// a number, a flag: nothing to clean
	}
}

// under is what to do to the text under a name: unquoted for a BlankAddresses name, otherwise whatever was being done above.
func (r *Registry) under(name string, text func(string) string) func(string) string {
	if r.quoted[name] {
		return r.unquoted
	}

	return text
}

// holdsText reports whether a list of these could hold text, so a list of bytes or numbers is not walked for nothing.
func holdsText(elem reflect.Type) bool {
	switch elem.Kind() {
	case reflect.String, reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Slice, reflect.Array, reflect.Map:
		return true
	default:
		return false
	}
}

// cleanMap cleans a map's values, and its keys when they are text (a count by title has the title for a key). A key named in LeaveAsIs or
// BlankAddresses counts as a field name. Keys are taken in order so two that clean to one key keep the same one each run.
func (r *Registry) cleanMap(v reflect.Value, text func(string) string) {
	if v.IsNil() {
		return
	}

	named := v.Type().Key().Kind() == reflect.String
	keys := v.MapKeys()
	if named {
		slices.SortFunc(keys, func(a, b reflect.Value) int { return cmp.Compare(a.String(), b.String()) })
	}
	for _, k := range keys {
		if named && r.leave[k.String()] {
			continue
		}

		below := text
		if named {
			below = r.under(k.String(), text)
		}
		held := reflect.New(v.Type().Elem()).Elem()
		held.Set(v.MapIndex(k))
		r.clean(held, below)

		if named {
			if cleaned := reflect.ValueOf(r.cleaned(k.String())).Convert(k.Type()); cleaned.String() != k.String() && !v.MapIndex(cleaned).IsValid() {
				v.SetMapIndex(k, reflect.Value{})
				k = cleaned
			}
		}
		v.SetMapIndex(k, held)
	}
}

// cleanedError is a tool's error with its words cleaned: an error quotes what the server said, and is read like an answer.
type cleanedError struct {
	err   error
	limit int
}

func (e cleanedError) Error() string { return outside.Text(e.err.Error(), e.limit) }
func (e cleanedError) Unwrap() error { return e.err }

// cleanFailure is a tool's error cleaned when CleanAnswers asks, or the error itself when there is nothing to clean.
func (r *Registry) cleanFailure(err error) error {
	limit := r.cfg.CleanAnswers
	if limit <= 0 || outside.Text(err.Error(), limit) == err.Error() {
		return err
	}

	return cleanedError{err: err, limit: limit}
}

// argumentNames is the JSON names of an input's fields in declared order, an embedded struct's among them; a non-struct input names none.
func argumentNames(t reflect.Type) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	var names []string
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-", !f.IsExported() && !f.Anonymous:
		case f.Anonymous && name == "":
			names = append(names, argumentNames(f.Type)...)
		case name == "":
			names = append(names, f.Name)
		default:
			names = append(names, name)
		}
	}

	return names
}

// recovered calls a handler and turns a panic into a tool error naming the tool, with the stack logged: nothing above recovers one, so one nil
// dereference would otherwise end the session.
func recovered[In, Out any](ctx context.Context, r *Registry, name string, h mcp.ToolHandlerFor[In, Out], req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
	defer func() {
		if p := recover(); p != nil {
			r.logError("internal error in %s: %v\n%s", name, p, debug.Stack())
			var zero Out
			res, out, err = nil, zero, fmt.Errorf("internal error in %s: %v", name, p)
		}
	}()

	return h(ctx, req, in)
}

func (r *Registry) logError(format string, args ...any) {
	if r.cfg.LogError != nil {
		r.cfg.LogError(format, args...)

		return
	}
	clog.Log.Errorf(format, args...)
}

// emptyNils replaces every nil slice and map in v with an empty one: null reads as "not fetched" to a client, and the SDK refuses a null where the
// schema says object. What a map holds is not walked.
func emptyNils(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			emptyNils(v.Elem())
		}
	case reflect.Struct:
		for _, f := range v.Fields() {
			emptyNils(f)
		}
	case reflect.Slice:
		if v.IsNil() {
			if v.CanSet() {
				v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			}

			return
		}
		for i := range v.Len() {
			emptyNils(v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() && v.CanSet() {
			v.Set(reflect.MakeMap(v.Type()))
		}
	default:
	}
}

// Register adds the tools the selection permits and returns their names, sorted. A toolset or pattern that reaches no tool is an error, so a typo
// cannot quietly hide one.
func (r *Registry) Register(server *mcp.Server, sel Selection) ([]string, error) {
	keep, err := r.selected(sel)
	if err != nil {
		return nil, err
	}

	var registered []string
	tools := map[string]pending{}
	for _, p := range r.pending {
		if !keep[p.name] {
			continue
		}
		p.register(server)
		registered = append(registered, p.name)
		tools[p.name] = p
	}
	slices.Sort(registered)
	server.AddReceivingMiddleware(r.calls(tools))

	return registered, nil
}

// How a write or delete call ended, as the write line says it. Whether an answered call changed anything or only previewed is the tool's own argument
// (confirm, dry_run), which the line carries.
const (
	// outcomeRefused: the arguments were turned away before the tool ran
	outcomeRefused = "refused"
	// outcomeFailed: the tool ran and answered with an error
	outcomeFailed = "failed"
	// outcomeAnswered: the tool ran and answered
	outcomeAnswered = "answered"
)

// The most of a call's arguments, and of an error, a write line carries.
const (
	loggedArguments = 2000
	loggedError     = 300
)

// calls stands between a client and every call, the one place that sees a call refused before the tool ran and every failure whoever worded it. It
// names the arguments a tool takes to a caller who sent a wrong one, blanks addresses in a failure (hideAddresses), and writes the line for a write.
func (r *Registry) calls(tools map[string]pending) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if method != "tools/call" || !ok || call.Params == nil {
				return next(ctx, method, req)
			}
			p, known := tools[call.Params.Name]
			if !known {
				return next(ctx, method, req)
			}

			result, err := next(ctx, method, req)
			outcome, failure := outcomeAnswered, err
			if err != nil {
				outcome = outcomeFailed
			}
			if res, answered := result.(*mcp.CallToolResult); answered && err == nil && res != nil && res.IsError {
				outcome, failure = outcomeFailed, res.GetError()
				if failure != nil && strings.HasPrefix(failure.Error(), `validating "arguments":`) {
					outcome = outcomeRefused
					nameArguments(res, p)
				}
				hideAddresses(res)
			}
			if p.kind != Read {
				r.logWrite(p, call.Params.Arguments, outcome, failure)
			}

			return result, err
		}
	}
}

// hideAddresses blanks what each address in a failure's words may carry a credential in (outside.BlankAddresses): a server quotes the address it
// called back with its stored key on it, which was never the caller's to see. Answers are left alone unless Config.BlankAddresses names the field.
func hideAddresses(res *mcp.CallToolResult) {
	for i, c := range res.Content {
		text, ok := c.(*mcp.TextContent)
		if !ok || text == nil {
			continue
		}

		if shown := outside.BlankAddresses(text.Text); shown != text.Text {
			rewritten := *text
			rewritten.Text = shown
			res.Content[i] = &rewritten
		}
	}
}

// blanked stands for a value the write line does not show.
const blanked = addresses.Blanked

// notShown stands for arguments that could not be read, and so could not be blanked, and are not written at all.
const notShown = "(arguments that do not read as JSON, not shown)"

// repeated is the shortest blanked value a failure's text is searched for: anything shorter is a word or a number the failure may have had anyway.
const repeated = 6

// hider blanks what the write line may not show of one call, and remembers what it blanked.
type hider struct {
	// loose are the names everything under which is blanked; secret the names a server adds to those that read as a credential's
	loose  map[string]bool
	secret []string
	// all is a tool whose whole input is free-form
	all bool
	// hidden is what was blanked, to blank again where the failure repeats it
	hidden []string
}

// arguments is what a call sent as the write line shows it: credentials by name, everything under a free-form object, and what an address carries
// blanked; the names kept so the line still says what was sent.
func (h *hider) arguments(sent json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(sent))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return notShown
	}

	if h.all {
		v = h.blank(v)
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h.hide(v)); err != nil {
		return notShown
	}

	return strings.TrimSuffix(out.String(), "\n")
}

// hide blanks what is under a name that hides it, and what an address in any text carries.
func (h *hider) hide(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for name, held := range v {
			if h.loose[name] || chttp.SecretName(name, h.secret...) {
				v[name] = h.blank(held)

				continue
			}
			v[name] = h.hide(held)
		}

		return v
	case []any:
		for i, held := range v {
			v[i] = h.hide(held)
		}

		return v
	case string:
		return addresses.Blank(v, h.keep)
	default:
		return v
	}
}

// blank blanks everything in a value and keeps its names; null stays null.
func (h *hider) blank(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for name, held := range v {
			v[name] = h.blank(held)
		}

		return v
	case []any:
		for i, held := range v {
			v[i] = h.blank(held)
		}

		return v
	case string:
		h.keep(v)

		return blanked
	case nil:
		return nil
	default:
		return blanked
	}
}

// keep remembers a blanked piece of text for failure.
func (h *hider) keep(piece string) { h.hidden = append(h.hidden, piece) }

// failure is a call's error as the write line shows it: addresses blanked, and anything blanked in the arguments blanked here too, since an error
// tends to repeat the address it could not reach or the value it refused.
func (h *hider) failure(text string) string {
	text = addresses.Blank(text, h.keep)

	slices.SortFunc(h.hidden, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, secret := range h.hidden {
		if len(secret) >= repeated {
			text = blankWhole(text, secret)
		}
	}

	return text
}

// blankWhole blanks secret where it stands as a whole word: "require" in "sslmode=require", not inside "required".
func blankWhole(text, secret string) string {
	first, _ := utf8.DecodeRuneInString(secret)
	last, _ := utf8.DecodeLastRuneInString(secret)

	var out strings.Builder
	for {
		at := strings.Index(text, secret)
		if at < 0 {
			break
		}
		end := at + len(secret)

		before, _ := utf8.DecodeLastRuneInString(text[:at])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (wordy(first) && wordy(before)) || (wordy(last) && wordy(after)) {
			out.WriteString(text[:end])
		} else {
			out.WriteString(text[:at] + blanked)
		}
		text = text[end:]
	}
	out.WriteString(text)

	return out.String()
}

// wordy reports whether a character is part of a word: a letter or a digit.
func wordy(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// nameArguments adds the arguments a tool takes to its refusal of one it does not: the SDK names the wrong one, which alone does not help a caller
// who mistyped it.
func nameArguments(res *mcp.CallToolResult, p pending) {
	refusal := res.GetError().Error()
	if !strings.Contains(refusal, "unexpected additional properties") {
		return
	}

	takes := "no arguments"
	switch n := len(p.arguments); {
	case n == 1:
		takes = p.arguments[0]
	case n > 1:
		takes = strings.Join(p.arguments[:n-1], ", ") + " and " + p.arguments[n-1]
	}
	res.Content = []mcp.Content{&mcp.TextContent{Text: refusal + ": " + p.name + " takes " + takes}}
}

// logWrite writes the line for one write or delete call, with credentials blanked (hider) and hidden characters removed so a line stays one line.
// Blanking comes before cutting to length, or a name could lose its value.
func (r *Registry) logWrite(p pending, arguments json.RawMessage, outcome string, failure error) {
	log := r.cfg.LogWrite
	if log == nil {
		log = clog.Log.Infof
	}

	// what a server shows is shown for a call the tool answered alone: one it refused or failed was never held to the tool's own list of names
	loose := p.loose
	if outcome == outcomeAnswered {
		loose = p.looseAnswered
	}

	h := hider{loose: loose, secret: r.cfg.SecretArguments, all: p.looseInput}
	sent := "{}"
	if len(arguments) > 0 {
		sent = outside.Text(h.arguments(arguments), loggedArguments)
	}
	if failure != nil {
		log("%s %s %s %s: %s", p.kind, p.name, outcome, sent, outside.Text(h.failure(failure.Error()), loggedError))

		return
	}
	log("%s %s %s %s", p.kind, p.name, outcome, sent)
}

// Describe lists what a selection would register without registering it, by the same choice Register makes, so help cannot drift from service.
func (r *Registry) Describe(sel Selection) ([]Info, error) {
	keep, err := r.selected(sel)
	if err != nil {
		return nil, err
	}

	set := map[string]string{}
	for name, members := range r.cfg.Toolsets {
		for _, m := range members {
			// core wins: it is the set a tool is reached through most often
			if set[m] == "" || name == Core {
				set[m] = name
			}
		}
	}

	out := make([]Info, 0, len(r.pending))
	for _, p := range r.pending {
		if !keep[p.name] {
			continue
		}
		out = append(out, Info{Name: p.name, Kind: p.kind.String(), Toolset: set[p.name], Description: p.description})
	}
	slices.SortFunc(out, func(a, b Info) int { return cmp.Compare(a.Name, b.Name) })

	return out, nil
}

// Names is every tool added, in the order added.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.pending))
	for _, p := range r.pending {
		out = append(out, p.name)
	}

	return out
}

// ToolsetNames lists the curated toolsets, sorted, for help output.
func (r *Registry) ToolsetNames() []string {
	out := make([]string, 0, len(r.cfg.Toolsets))
	for k := range r.cfg.Toolsets {
		out = append(out, k)
	}
	slices.Sort(out)

	return out
}

// FamilyNames lists the resource families, the part of each tool's name before its first underscore, sorted and read off the tools themselves.
func (r *Registry) FamilyNames() []string { return families(r.Names()) }

// selected applies the kind gates, then the toolsets and allow list together, then the deny list; with no toolsets and no allow list a session gets
// every tool.
func (r *Registry) selected(sel Selection) (map[string]bool, error) {
	names := r.Names()
	sets, err := r.compileToolsets(sel.Toolsets, names)
	if err != nil {
		return nil, err
	}
	allow, err := r.compilePatterns(sel.Allow, names, "allow")
	if err != nil {
		return nil, err
	}
	deny, err := r.compilePatterns(sel.Deny, names, "deny")
	if err != nil {
		return nil, err
	}
	// what was asked for is read off the lists as they were given: an allow list of a preset the application left empty asks for nothing, which is
	// not the same as no allow list
	everything := len(entries(sel.Toolsets)) == 0 && len(entries(sel.Allow)) == 0

	keep := make(map[string]bool, len(r.pending))
	for _, p := range r.pending {
		switch {
		case p.kind == Delete && !sel.EnableDelete:
		case p.kind != Read && sel.ReadOnly:
		case !everything && !sets[p.name] && !matchesAny(allow, p.name):
		case matchesAny(deny, p.name):
		default:
			keep[p.name] = true
		}
	}

	return keep, nil
}

// entries splits entries that may each hold several names joined by commas, as a repeated flag and an environment variable both arrive.
func entries(raw []string) []string {
	var out []string
	for _, entry := range raw {
		for name := range strings.SplitSeq(entry, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
	}

	return out
}

// compileToolsets turns the set names asked for into their tools, Core always among them; an unknown name is an error naming the valid ones.
func (r *Registry) compileToolsets(raw, known []string) (map[string]bool, error) {
	asked := entries(raw)
	if len(asked) == 0 {
		return nil, nil
	}

	out := map[string]bool{}
	for _, name := range asked {
		if name == All {
			for _, t := range known {
				out[t] = true
			}

			continue
		}
		if tools, ok := r.cfg.Toolsets[name]; ok {
			for _, t := range tools {
				out[t] = true
			}

			continue
		}
		// not a named set: a resource family, every tool with that prefix
		found := false
		for _, t := range known {
			if strings.HasPrefix(t, name+"_") {
				out[t], found = true, true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown toolset %q (sets: %s, %s; or a resource family: %s)", name, All, strings.Join(r.ToolsetNames(), ", "), strings.Join(families(known), ", "))
		}
	}
	// core is what every other set assumes: without it there is no way to find anything or open it
	for _, t := range r.cfg.Toolsets[Core] {
		out[t] = true
	}

	return out, nil
}

// families lists the resource prefixes in use, sorted.
func families(known []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range known {
		if i := strings.Index(t, "_"); i > 0 && !seen[t[:i]] {
			seen[t[:i]] = true
			out = append(out, t[:i])
		}
	}
	slices.Sort(out)

	return out
}

// compilePatterns expands Essential and checks every other pattern matches at least one tool.
func (r *Registry) compilePatterns(raw, known []string, which string) ([]string, error) {
	var out []string
	for _, pat := range entries(raw) {
		if pat == Essential {
			out = append(out, r.cfg.Essential...)

			continue
		}
		if !slices.ContainsFunc(known, func(n string) bool { return matchPattern(pat, n) }) {
			return nil, fmt.Errorf("%s-tools pattern %q matches no tool (have: %s)", which, pat, strings.Join(known, ", "))
		}
		out = append(out, pat)
	}

	return out, nil
}

func matchesAny(patterns []string, name string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return matchPattern(p, name) })
}

// matchPattern matches an exact name, or one with a single "*" at either end.
func matchPattern(pattern, name string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	case strings.HasPrefix(pattern, "*"):
		return strings.HasSuffix(name, strings.TrimPrefix(pattern, "*"))
	default:
		return pattern == name
	}
}
