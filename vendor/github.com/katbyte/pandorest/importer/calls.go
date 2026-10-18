package importer

import (
	"fmt"
	"go/token"
	"slices"
	"strings"

	"github.com/katbyte/pandorest/definitions"
	"github.com/katbyte/pandorest/openapi"
)

// callOperation builds the operation of a JSON-RPC method: named after the method (pool.query is PoolQuery), its arguments
// typed in the order the server takes them, its answer the method's, and no path parameters, options or status codes. A method
// is a read when the role the config names may call it, or the config says so by name.
func (im *importer) callOperation(name string, op *openapi.Operation) definitions.Operation {
	where := openapi.MethodCall + " " + name
	want := openapi.Camel(op.OperationID)
	if op.Name != "" {
		want = op.Name
		if !token.IsIdentifier(want) || !token.IsExported(want) {
			im.fail(fmt.Sprintf("%s: a workaround names it %q, which is not an exported Go name", where, want))
		}
	}
	if op.Call == nil {
		im.fail(where + ": a call with nothing said of its method")
		op.Call = &openapi.CallInfo{Method: name}
	}
	o := definitions.Operation{
		Name:        want,
		OperationID: op.OperationID,
		Method:      openapi.MethodCall,
		Path:        name,
		Description: cleanText(op.Description),
		Deprecated:  op.Deprecated,
		Call:        &definitions.Call{Method: op.Call.Method, Job: op.Call.Job, Filterable: op.Call.Filterable, Item: op.Call.Item, NoAuth: op.Call.NoAuth, Roles: op.Call.Roles, Downloadable: op.Call.Downloadable, Uploadable: op.Call.Uploadable},
	}
	o.Call.Read = im.isRead(name, op.Call.Roles)
	im.methods[name] = true

	args := map[string]string{}
	for _, prm := range op.Parameters {
		if prm.In != openapi.InCall {
			im.fail(fmt.Sprintf("%s: parameter %q is in %q, where a call's are in %q alone", where, prm.Name, prm.In, openapi.InCall))

			continue
		}
		arg := callArgName(prm.Name)
		if prev, ok := args[arg]; ok {
			im.fail(fmt.Sprintf("%s: parameters %q and %q are both the argument %s", where, prev, prm.Name, arg))

			continue
		}
		args[arg] = prm.Name
		o.Arguments = append(o.Arguments, definitions.Argument{
			Name:        prm.Name,
			Argument:    arg,
			Type:        im.typeRef(prm.Schema, want, fieldName(prm.Name)),
			Required:    prm.Required,
			Description: cleanText(prm.Description),
		})
	}
	o.Response = im.responseBody(openapi.MethodCall, name, want, op)

	return o
}

// queryForms are the one-record and count forms of a query, each an operation of its own over the same method: <Name>One
// answers one record of the list the query answers and <Name>Count how many, by setting the get or count flag in the options
// argument, which the form names. A query with no options argument, or one answering something other than a list of records,
// has no forms.
func (im *importer) queryForms(o *definitions.Operation) []definitions.Operation {
	if o.Call == nil || !o.Call.Filterable || o.Response == nil || o.Response.Type.Type != definitions.List || o.Response.Type.NestedItem == nil {
		return nil
	}
	options := slices.IndexFunc(o.Arguments, func(a definitions.Argument) bool { return a.Name == "options" })
	if options < 0 {
		im.warn(fmt.Sprintf("%s %s: a query with no options argument, so no one-record or count form", openapi.MethodCall, o.Call.Method))

		return nil
	}
	item := *o.Response.Type.NestedItem

	suffixes, flags := []string{"One", "Count"}, []string{definitions.QueryGet, definitions.QueryCount}
	answers := []definitions.TypeRef{item, {Type: definitions.Integer}}

	forms := make([]definitions.Operation, 0, len(flags))
	for i, flag := range flags {
		f := *o
		f.Name = o.Name + suffixes[i]
		f.OperationID = o.OperationID + "#" + flag
		f.Path = o.Path + "#" + flag
		call := *o.Call
		call.Form = flag
		f.Call = &call
		f.Arguments = slices.Clone(o.Arguments)
		f.Arguments[options].Required = true
		f.Response = &definitions.Body{ContentType: o.Response.ContentType, Type: answers[i]}
		forms = append(forms, f)
	}

	return forms
}

// callReserved are the identifiers a generated call method uses itself, which an argument of the name gets a "Param" suffix to
// keep clear of (core.bulk's params is paramsParam); the predeclared names and Go's keywords are kept clear of as well.
var callReserved = func() map[string]bool {
	m := map[string]bool{}
	for n := range strings.FieldsSeq("c ctx call params answer result err client context errors " +
		"string int int64 float32 float64 bool error any len cap new make nil true false") {
		m[n] = true
	}

	return m
}()

// callArgName is the Go parameter name for a call's argument.
func callArgName(param string) string {
	n := lowerFirst(openapi.Camel(param))
	if n == "" {
		return "arg"
	}
	if token.IsKeyword(n) || callReserved[n] {
		n += "Param"
	}

	return n
}

// isRead reports whether a method changes nothing: the config's ReadRole is among the roles that may call it, or the config
// names it in Reads; one named in NotReads is not, whatever its roles.
func (im *importer) isRead(name string, roles []string) bool {
	switch {
	case slices.Contains(im.cfg.NotReads, name):
		return false
	case slices.Contains(im.cfg.Reads, name):
		return true
	default:
		return im.cfg.ReadRole != "" && slices.Contains(roles, im.cfg.ReadRole)
	}
}

// checkReads fails the import for a name in Reads or NotReads that is no method of the document, so the lists cannot go stale.
func (im *importer) checkReads() {
	for _, list := range []struct {
		field string
		names []string
	}{{"Reads", im.cfg.Reads}, {"NotReads", im.cfg.NotReads}} {
		for _, name := range list.names {
			if !im.methods[name] {
				im.fail(fmt.Sprintf("the config's %s names %s, which is no method of the document: change or remove the entry", list.field, name))
			}
		}
	}
}
