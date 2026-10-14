// Package differ reports what changed between two sets of definitions in API
// terms, after Pandora's data-api-differ: operations, options, bodies, models,
// fields and enum values added, removed or changed, with the changes that
// would break a caller of the generated SDK marked. make pandorest-diff runs
// it on a refreshed spec against the checked-in definitions before they are
// regenerated.
package differ

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/katbyte/pandorest/definitions"
)

// Kind is what happened to a subject.
type Kind string

// The kinds, each the mark it has in a report.
const (
	Added   Kind = "+"
	Removed Kind = "-"
	Changed Kind = "~"
)

// Change is one operation, model or constant that differs.
type Change struct {
	Kind Kind
	// Subject names it: "operation GetItems (GET /Items)".
	Subject string
	// Details are the differences within a changed subject.
	Details []Detail
	// Breaking is set when the change (or any detail) breaks callers.
	Breaking bool
}

// Detail is one difference inside a changed subject.
type Detail struct {
	Kind     Kind
	Text     string
	Breaking bool
}

// Report is every change between two versions of one service.
type Report struct {
	Service string
	// Service-level differences: document version, workarounds applied.
	Notes   []string
	Changes []Change
}

// Empty reports whether nothing differs.
func (r *Report) Empty() bool { return len(r.Notes) == 0 && len(r.Changes) == 0 }

// Breaking reports whether any change breaks callers.
func (r *Report) Breaking() bool {
	return slices.ContainsFunc(r.Changes, func(c Change) bool { return c.Breaking })
}

// String renders the report for a terminal.
func (r *Report) String() string {
	var b strings.Builder
	if r.Empty() {
		fmt.Fprintf(&b, "%s: no changes\n", r.Service)
		return b.String()
	}
	counts := map[Kind]int{}
	breaking := 0
	for _, c := range r.Changes {
		counts[c.Kind]++
		if c.Breaking {
			breaking++
		}
	}
	fmt.Fprintf(&b, "%s: %d added, %d removed, %d changed (%d breaking)\n", r.Service, counts[Added], counts[Removed], counts[Changed], breaking)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "%s %s%s\n", c.Kind, c.Subject, breakingMark[c.Kind != Changed && c.Breaking])
		for _, d := range c.Details {
			fmt.Fprintf(&b, "    %s %s%s\n", d.Kind, d.Text, breakingMark[d.Breaking])
		}
	}

	return b.String()
}

// breakingMark is what follows a change that is breaking.
var breakingMark = map[bool]string{true: " [breaking]"}

// Diff compares old definitions with new ones.
func Diff(older, newer *definitions.Service) Report {
	r := Report{Service: newer.Name}
	if older.APIVersion != newer.APIVersion || older.Title != newer.Title {
		r.Notes = append(r.Notes, fmt.Sprintf("document: %s %s -> %s %s", older.Title, older.APIVersion, newer.Title, newer.APIVersion))
	}
	for _, w := range newer.Workarounds {
		if !slices.Contains(older.Workarounds, w) {
			r.Notes = append(r.Notes, "workaround added: "+w)
		}
	}
	for _, w := range older.Workarounds {
		if !slices.Contains(newer.Workarounds, w) {
			r.Notes = append(r.Notes, "workaround removed: "+w)
		}
	}

	r.Changes = append(r.Changes, diffOperations(older, newer)...)
	r.Changes = append(r.Changes, diffModels(older.Models(), newer.Models())...)
	r.Changes = append(r.Changes, diffConstants(older.Constants(), newer.Constants())...)

	return r
}

// keyed indexes a slice by key.
func keyed[T any](items []T, key func(T) string) (byKey map[string]T, keys []string) {
	byKey = make(map[string]T, len(items))
	keys = make([]string, 0, len(items))
	for _, it := range items {
		k := key(it)
		byKey[k] = it
		keys = append(keys, k)
	}

	return byKey, keys
}

// union returns the sorted keys of both maps.
func union(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)

	return slices.Compact(out)
}

func operationGroups(svc *definitions.Service) map[string]string {
	out := map[string]string{}
	for _, g := range svc.Groups {
		for _, o := range g.Operations {
			out[o.Key()] = g.Name
		}
	}

	return out
}

func diffOperations(older, newer *definitions.Service) []Change {
	oldOps, oldKeys := keyed(older.Operations(), func(o *definitions.Operation) string { return o.Key() })
	newOps, newKeys := keyed(newer.Operations(), func(o *definitions.Operation) string { return o.Key() })
	oldGroups, newGroups := operationGroups(older), operationGroups(newer)

	var out []Change
	for _, key := range union(oldKeys, newKeys) {
		o, n := oldOps[key], newOps[key]
		switch {
		case o == nil:
			out = append(out, Change{Kind: Added, Subject: operationSubject(n)})
		case n == nil:
			out = append(out, Change{Kind: Removed, Subject: operationSubject(o), Breaking: true})
		default:
			var d details
			if o.Name != n.Name {
				d.change(true, "method name %s -> %s", o.Name, n.Name)
			}
			if oldGroups[key] != newGroups[key] {
				d.change(false, "tag %s -> %s", oldGroups[key], newGroups[key])
			}
			if o.Deprecated != n.Deprecated {
				d.change(false, "deprecated %t -> %t", o.Deprecated, n.Deprecated)
			}
			if o.Description != n.Description {
				d.change(false, "description changed")
			}
			diffPathParameters(&d, o.PathParameters, n.PathParameters)
			diffOptions(&d, o.Options, n.Options)
			diffBody(&d, "request", o.Request, n.Request)
			diffBody(&d, "response", o.Response, n.Response)
			if !slices.Equal(o.ExpectedStatusCodes, n.ExpectedStatusCodes) {
				// a code no longer expected turns a success into an error
				gone := slices.ContainsFunc(o.ExpectedStatusCodes, func(c int) bool { return !slices.Contains(n.ExpectedStatusCodes, c) })
				d.change(gone, "expected status codes %v -> %v", o.ExpectedStatusCodes, n.ExpectedStatusCodes)
			}
			switch {
			case (o.Pageable == nil) != (n.Pageable == nil):
				d.change(o.Pageable != nil, "pageable %t -> %t", o.Pageable != nil, n.Pageable != nil)
			case o.Pageable != nil && !o.Pageable.ItemType.Equal(n.Pageable.ItemType):
				d.change(true, "pageable item type %s -> %s", o.Pageable.ItemType, n.Pageable.ItemType)
			case o.Pageable != nil && o.Pageable.ByPage != n.Pageable.ByPage:
				d.change(true, "pages by page number %t -> %t", o.Pageable.ByPage, n.Pageable.ByPage)
			}
			if c, ok := d.result(operationSubject(n)); ok {
				out = append(out, c)
			}
		}
	}

	return out
}

func operationSubject(o *definitions.Operation) string {
	return fmt.Sprintf("operation %s (%s)", o.Name, o.Key())
}

// details collects the differences within one subject.
type details struct {
	list []Detail
}

func (d *details) add(kind Kind, breaking bool, format string, args ...any) {
	d.list = append(d.list, Detail{Kind: kind, Text: fmt.Sprintf(format, args...), Breaking: breaking})
}

func (d *details) change(breaking bool, format string, args ...any) {
	d.add(Changed, breaking, format, args...)
}

// result turns the details into a Change of subject, when there are any.
func (d *details) result(subject string) (Change, bool) {
	if len(d.list) == 0 {
		return Change{}, false
	}

	return Change{
		Kind:     Changed,
		Subject:  subject,
		Details:  d.list,
		Breaking: slices.ContainsFunc(d.list, func(x Detail) bool { return x.Breaking }),
	}, true
}

func diffPathParameters(d *details, older, newer []definitions.PathParameter) {
	if len(older) != len(newer) {
		d.change(true, "path parameters %s -> %s", pathParams(older), pathParams(newer))
		return
	}
	for i := range older {
		o, n := older[i], newer[i]
		if o.Name != n.Name || o.Argument != n.Argument || !o.Type.Equal(n.Type) {
			d.change(true, "path parameter %d: %s %s -> %s %s", i+1, o.Argument, o.Type, n.Argument, n.Type)
		}
	}
}

func pathParams(ps []definitions.PathParameter) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, p.Argument+" "+p.Type.String())
	}

	return "(" + strings.Join(parts, ", ") + ")"
}

func diffOptions(d *details, older, newer []definitions.Option) {
	oldByName, oldNames := keyed(older, func(o definitions.Option) string { return o.In + " " + o.Name })
	newByName, newNames := keyed(newer, func(o definitions.Option) string { return o.In + " " + o.Name })
	for _, name := range union(oldNames, newNames) {
		o, oldOK := oldByName[name]
		n, newOK := newByName[name]
		label := strings.ToLower(name)
		switch {
		case !oldOK:
			d.add(Added, false, "option %s: %s", label, n.Type)
		case !newOK:
			d.add(Removed, true, "option %s: %s", label, o.Type)
		default:
			if !o.Type.Equal(n.Type) {
				d.change(true, "option %s: %s -> %s", label, o.Type, n.Type)
			}
			if o.Field != n.Field {
				d.change(true, "option %s: field %s -> %s", label, o.Field, n.Field)
			}
			if o.Deprecated != n.Deprecated {
				d.change(false, "option %s: deprecated %t -> %t", label, o.Deprecated, n.Deprecated)
			}
			if o.Required != n.Required {
				d.change(n.Required, "option %s: required %t -> %t", label, o.Required, n.Required)
			}
			if o.CommaSeparated != n.CommaSeparated {
				d.change(true, "option %s: sent %s -> %s", label, wireForm[o.CommaSeparated], wireForm[n.CommaSeparated])
			}
			if o.DeepObject != n.DeepObject {
				d.change(true, "option %s: deep object %t -> %t", label, o.DeepObject, n.DeepObject)
			}
			if o.Description != n.Description {
				d.change(false, "option %s: description changed", label)
			}
		}
	}
}

// wireForm names how a list option travels, by whether it is comma-separated.
var wireForm = map[bool]string{true: "comma-separated", false: "one key per value"}

func diffBody(d *details, what string, older, newer *definitions.Body) {
	switch {
	case older == nil && newer == nil:
	case older == nil:
		d.add(Added, what == "request", "%s %s (%s)", what, newer.Type, newer.ContentType)
	case newer == nil:
		d.add(Removed, true, "%s %s (%s)", what, older.Type, older.ContentType)
	case !older.Type.Equal(newer.Type):
		d.change(true, "%s %s -> %s", what, older.Type, newer.Type)
	case older.ContentType != newer.ContentType:
		d.change(false, "%s content type %s -> %s", what, older.ContentType, newer.ContentType)
	}
}

func diffModels(older, newer map[string]*definitions.Model) []Change {
	var out []Change
	for _, name := range union(mapKeys(older), mapKeys(newer)) {
		o, n := older[name], newer[name]
		switch {
		case o == nil:
			out = append(out, Change{Kind: Added, Subject: "model " + name})
		case n == nil:
			out = append(out, Change{Kind: Removed, Subject: "model " + name, Breaking: true})
		default:
			var d details
			if !slices.Equal(o.Union, n.Union) {
				d.change(true, "union %v -> %v", o.Union, n.Union)
			}
			if o.WrittenWhole != n.WrittenWhole {
				d.change(false, "written whole %t -> %t", o.WrittenWhole, n.WrittenWhole)
			}
			if o.Description != n.Description {
				d.change(false, "description changed")
			}
			oldFields, oldNames := keyed(o.Fields, func(f definitions.Field) string { return f.JSONName })
			newFields, newNames := keyed(n.Fields, func(f definitions.Field) string { return f.JSONName })
			for _, field := range union(oldNames, newNames) {
				of, oldOK := oldFields[field]
				nf, newOK := newFields[field]
				switch {
				case !oldOK:
					d.add(Added, false, "field %s: %s", field, nf.Type)
				case !newOK:
					d.add(Removed, true, "field %s: %s", field, of.Type)
				case !of.Type.Equal(nf.Type):
					d.change(true, "field %s: %s -> %s", field, of.Type, nf.Type)
				case of.Name != nf.Name:
					d.change(true, "field %s: Go name %s -> %s", field, of.Name, nf.Name)
				case of.Required != nf.Required:
					// the field is sent where it was left out, or left out
					// where it was sent: no caller's code changes
					d.change(false, "field %s: required %t -> %t", field, of.Required, nf.Required)
				case of.KeepsNull != nf.KeepsNull:
					// a number or string held by pointer, or no longer: every
					// reader of the field changes
					d.change(true, "field %s: keeps null %t -> %t", field, of.KeepsNull, nf.KeepsNull)
				case of.Nullable != nf.Nullable:
					// the generator reads nothing from it: booleans are
					// pointers and lists omitzero whatever the document says
					d.change(false, "field %s: nullable %t -> %t", field, of.Nullable, nf.Nullable)
				case of.Description != nf.Description:
					d.change(false, "field %s: description changed", field)
				}
			}
			if c, ok := d.result("model " + name); ok {
				out = append(out, c)
			}
		}
	}

	return out
}

func diffConstants(older, newer map[string]*definitions.Constant) []Change {
	var out []Change
	for _, name := range union(mapKeys(older), mapKeys(newer)) {
		o, n := older[name], newer[name]
		switch {
		case o == nil:
			out = append(out, Change{Kind: Added, Subject: "constant " + name})
		case n == nil:
			out = append(out, Change{Kind: Removed, Subject: "constant " + name, Breaking: true})
		default:
			var d details
			oldValues, oldNames := keyed(o.Values, func(v definitions.ConstantValue) string { return v.Value })
			newValues, newNames := keyed(n.Values, func(v definitions.ConstantValue) string { return v.Value })
			for _, v := range union(oldNames, newNames) {
				_, oldOK := oldValues[v]
				_, newOK := newValues[v]
				switch {
				case !oldOK:
					d.add(Added, false, "value %q", v)
				case !newOK:
					d.add(Removed, true, "value %q", v)
				}
			}
			if c, ok := d.result("constant " + name); ok {
				out = append(out, c)
			}
		}
	}

	return out
}

func mapKeys[V any](m map[string]V) []string {
	return slices.Collect(maps.Keys(m))
}
