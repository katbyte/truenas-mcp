package definitions

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Types lists every type an operation mentions directly.
func (o *Operation) Types() []TypeRef {
	var out []TypeRef
	for _, p := range o.PathParameters {
		out = append(out, p.Type)
	}
	for _, a := range o.Arguments {
		out = append(out, a.Type)
	}
	for _, opt := range o.Options {
		out = append(out, opt.Type)
	}
	if o.Request != nil {
		out = append(out, o.Request.Type)
	}
	if o.Response != nil {
		out = append(out, o.Response.Type)
	}

	return out
}

// Validate checks a service's definitions are self-consistent: every
// reference resolves, and no two operations share a name or a method and
// path. The importer runs it on what it produces and the generator on what it
// reads, so hand-edited definitions fail before any code is written.
func Validate(svc *Service) error {
	var problems []string
	models, constants := svc.Models(), svc.Constants()
	var check func(where string, t TypeRef)
	check = func(where string, t TypeRef) {
		switch t.Type {
		case Reference:
			if models[t.ReferenceName] == nil && constants[t.ReferenceName] == nil {
				problems = append(problems, fmt.Sprintf("%s: reference to undefined type %q", where, t.ReferenceName))
			}
		case List, Dictionary:
			if t.NestedItem == nil {
				problems = append(problems, where+": "+string(t.Type)+" without a nested item")
				return
			}
			check(where, *t.NestedItem)
		case Boolean, Integer, Integer64, Float, Double, String, RawObject, Any, RawFile, DateTime, Date, UnixSeconds, UnixMillis:
		case ID:
			if svc.IDBase(t.ReferenceName) == "" {
				problems = append(problems, fmt.Sprintf("%s: id of a kind the service does not declare %q", where, t.ReferenceName))
			}
		case Named:
			if t.GoType == "" {
				problems = append(problems, where+": a named type with no Go type")
			}
		default:
			problems = append(problems, fmt.Sprintf("%s: unknown type %q", where, t.Type))
		}
	}

	names, keys := map[string]bool{}, map[string]bool{}
	for _, o := range svc.Operations() {
		if names[o.Name] {
			problems = append(problems, "two operations are named "+o.Name)
		}
		if keys[o.Key()] {
			problems = append(problems, "two operations are "+o.Key())
		}
		names[o.Name], keys[o.Key()] = true, true
		for i, t := range o.Types() {
			check(fmt.Sprintf("%s type %d", o.Name, i), t)
		}
		if len(o.ExpectedStatusCodes) == 0 && o.Call == nil {
			problems = append(problems, o.Name+": no expected status codes")
		}
		if o.Call != nil && (len(o.PathParameters) > 0 || len(o.Options) > 0 || o.Request != nil) {
			problems = append(problems, o.Name+": a call has arguments alone, not path parameters, options or a request body")
		}
		if o.Call != nil && o.Call.Form != "" && (!o.Call.Filterable || (o.Call.Form != QueryGet && o.Call.Form != QueryCount)) {
			problems = append(problems, fmt.Sprintf("%s: a query form %q, which is %q or %q on a filterable call alone", o.Name, o.Call.Form, QueryGet, QueryCount))
		}
	}
	for name, m := range models {
		for _, f := range m.Fields {
			check(name+"."+f.Name, f.Type)
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return errors.New(svc.Name + ": invalid definitions:\n  " + strings.Join(problems, "\n  "))
	}

	return nil
}
