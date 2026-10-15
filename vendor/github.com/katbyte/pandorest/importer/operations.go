package importer

import (
	"cmp"
	"fmt"
	"go/token"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/katbyte/pandorest/config"
	"github.com/katbyte/pandorest/definitions"
	"github.com/katbyte/pandorest/openapi"
)

var pathParamRe = regexp.MustCompile(`\{([^}]+)\}`)

const acceptHeader = "Accept"

// importOperations builds every operation, grouped by tag.
func (im *importer) importOperations() map[string]*definitions.Group {
	groups := map[string]*definitions.Group{}
	names := map[string]string{} // each method name, and the operation that made it
	operationIDs := map[string]string{}

	for _, path := range openapi.SortedKeys(im.spec.Paths) {
		for _, m := range im.spec.Paths[path].Methods() {
			op := m.Operation
			where := m.Method + " " + path

			if op.OperationID != "" {
				if prev, ok := operationIDs[op.OperationID]; ok {
					im.fail(fmt.Sprintf("%s: operationId %q is also %s", where, op.OperationID, prev))
				}
				operationIDs[op.OperationID] = where
			}

			tag, name := im.groupName(where, op)
			g := groups[name]
			if g == nil {
				g = &definitions.Group{Name: name, Tag: tag}
				groups[name] = g
			} else if g.Tag != tag {
				im.fail(fmt.Sprintf("%s: tags %q and %q both make group %s", where, g.Tag, tag, name))
			}
			o := im.operation(m.Method, path, op)
			// two operations that make one name are a failure, not a second
			// one numbered: which would get the number follows the order of
			// the paths, so a refreshed document could rename a method
			if prev, ok := names[o.Name]; ok {
				im.fail(fmt.Sprintf("%s: makes the method %s, which %s makes too: give one of them a name of its own with a workaround (the operation's Name), or take one out", where, o.Name, prev))
			}
			names[o.Name] = where
			// a command the operation queues is an operation of its own, with the same request fixed to the command's name
			commands := im.commandOperations(where, &o, op)
			g.Operations = append(g.Operations, o)
			// a query's one-record and count forms are operations of their own over the same method
			for _, f := range im.queryForms(&o) {
				if prev, ok := names[f.Name]; ok {
					im.fail(fmt.Sprintf("%s: its %s form makes the method %s, which %s makes too", where, f.Call.Form, f.Name, prev))
				}
				names[f.Name] = where + " " + f.Call.Form
				g.Operations = append(g.Operations, f)
			}
			for _, c := range commands {
				if prev, ok := names[c.Name]; ok {
					im.fail(fmt.Sprintf("%s: the command %s makes the method %s, which %s makes too", where, c.Command, c.Name, prev))
				}
				names[c.Name] = where + " " + c.Command
				g.Operations = append(g.Operations, c)
			}
		}
	}

	for _, g := range groups {
		slices.SortFunc(g.Operations, func(a, b definitions.Operation) int { return strings.Compare(a.Name, b.Name) })
	}
	im.resolveCommandGets(groups)

	return groups
}

// commandOperations are the operations a command set makes beside the one that queues: one per command, sending the command's own
// schema as the body and answering as the queueing operation does. The set's Get is held as its key until every operation is named.
func (im *importer) commandOperations(where string, o *definitions.Operation, op *openapi.Operation) []definitions.Operation {
	if op.Commands == nil {
		return nil
	}
	set := op.Commands
	o.Commands = &definitions.CommandSet{Get: set.Get, Status: set.Status, Finished: set.Finished, Failed: set.Failed}
	var out []definitions.Operation
	for _, name := range openapi.SortedKeys(set.Commands) {
		c := definitions.Operation{Name: name, Method: o.Method, Path: o.Path, Description: "Queues the " + name + " command.", Response: o.Response, ExpectedStatusCodes: o.ExpectedStatusCodes, Command: name}
		c.Request = &definitions.Body{ContentType: "application/json", Type: reference(typeName(set.Commands[name]))}
		im.claimName(c.Name, where+" command "+name)
		out = append(out, c)
	}

	return out
}

// resolveCommandGets turns each command set's Get from the key of the operation that answers a queued command into that
// operation's Go name, failing for one that is not there or takes no id in its path.
func (im *importer) resolveCommandGets(groups map[string]*definitions.Group) {
	byKey := map[string]*definitions.Operation{}
	for _, g := range groups {
		for i := range g.Operations {
			byKey[g.Operations[i].Key()] = &g.Operations[i]
		}
	}
	for _, g := range groups {
		for i := range g.Operations {
			set := g.Operations[i].Commands
			if set == nil {
				continue
			}
			get := byKey[set.Get]
			switch {
			case get == nil:
				im.fail(fmt.Sprintf("%s: the commands' Get %s is not an operation of the document", g.Operations[i].Key(), set.Get))
			case len(get.PathParameters) != 1:
				im.fail(fmt.Sprintf("%s: the commands' Get %s takes %d path parameters, not the command's id alone", g.Operations[i].Key(), set.Get, len(get.PathParameters)))
			default:
				set.Get = get.Name
			}
		}
	}
}

// groupName returns the operation's tag and the group it names.
func (im *importer) groupName(where string, op *openapi.Operation) (tag, name string) {
	switch len(op.Tags) {
	case 0:
		im.fail(where + ": has no tag, so no group to live in")
		return "", "Untagged"
	case 1:
	default:
		im.warn(fmt.Sprintf("%s: has tags %v; grouped under the first", where, op.Tags))
	}
	tag = op.Tags[0]
	name = openapi.Camel(strings.TrimSuffix(tag, im.cfg.TagSuffix))
	if name == "" || name == definitions.CommonGroup {
		im.fail(fmt.Sprintf("%s: tag %q cannot be a group name", where, tag))
	}

	return tag, name
}

// pathName is the method name an operation's method and path make.
func (im *importer) pathName(method, path string) string {
	return pathMethodName(method, path, im.cfg.PathPrefix, im.cfg.Words)
}

func (im *importer) operation(method, path string, op *openapi.Operation) definitions.Operation {
	if method == openapi.MethodCall {
		return im.callOperation(path, op)
	}
	want := im.pathName(method, path)
	switch {
	case op.Name != "":
		// a workaround named it
		want = op.Name
		if !token.IsIdentifier(want) || !token.IsExported(want) {
			im.fail(fmt.Sprintf("%s %s: a workaround names it %q, which is not an exported Go name", method, path, want))
		}
	case im.cfg.Naming != config.OperationIDNaming:
	case op.OperationID == "":
		im.fail(fmt.Sprintf("%s %s: has no operationId to name its method after", method, path))
	default:
		want = openapi.Camel(op.OperationID)
	}

	description := cleanText(op.Summary)
	if description == "" {
		description = cleanText(op.Description)
	}
	o := definitions.Operation{
		Name:        want,
		OperationID: op.OperationID,
		Method:      method,
		Path:        path,
		Description: description,
		Deprecated:  op.Deprecated,
	}

	// path parameters in template order
	for _, match := range pathParamRe.FindAllStringSubmatch(path, -1) {
		prm := op.Parameter(openapi.InPath, match[1])
		if prm == nil {
			im.fail(fmt.Sprintf("%s %s: path parameter {%s} is not declared", method, path, match[1]))
			prm = &openapi.Parameter{Name: match[1], In: openapi.InPath}
		}
		o.PathParameters = append(o.PathParameters, definitions.PathParameter{
			Name:     match[1],
			Argument: argName(match[1]),
			Type:     im.pathParamType(prm.Schema),
		})
	}
	for _, prm := range op.Parameters {
		if prm.In == openapi.InPath && !strings.Contains(path, "{"+prm.Name+"}") {
			im.fail(fmt.Sprintf("%s %s: declares path parameter %q the template does not have", method, path, prm.Name))
		}
	}

	fields := map[string]string{}
	for _, prm := range op.Parameters {
		var in string
		switch prm.In {
		case openapi.InQuery:
			in = definitions.InQuery
		case openapi.InHeader:
			// OpenAPI says a header parameter named Accept, Content-Type or
			// Authorization is to be ignored: the body, the response and the
			// credentials set those (TMDB declares Content-Type on its
			// rating operations anyway). An Accept a workaround added is
			// kept: it is there because the server chooses its answer by it
			kept := prm.Added && strings.EqualFold(prm.Name, acceptHeader)
			if !kept && slices.ContainsFunc([]string{acceptHeader, "Content-Type", "Authorization"}, func(h string) bool { return strings.EqualFold(h, prm.Name) }) {
				continue
			}
			in = definitions.InHeader
		default:
			continue
		}
		field := cmp.Or(prm.Field, fieldName(prm.Name))
		if prev, ok := fields[field]; ok {
			im.warn(fmt.Sprintf("%s %s: parameters %q and %q are both field %s; %q skipped", method, path, prev, prm.Name, field, prm.Name))
			continue
		}
		fields[field] = prm.Name
		opt := definitions.Option{
			Name:        prm.Name,
			Field:       field,
			In:          in,
			Description: cleanText(prm.Description),
			Deprecated:  prm.Deprecated,
			Required:    prm.Required,
			Type:        im.optionType(prm.Schema),
		}
		if opt.Type.Type == definitions.List {
			opt.CommaSeparated = !prm.Exploded()
		}
		if opt.Type.Type == definitions.Dictionary {
			opt.DeepObject = prm.DeepObject()
		}
		o.Options = append(o.Options, opt)
	}

	// the owner the operation's inline request and response types are named
	// after: the method itself where methods are named by operationId, since
	// a path-built name (GetN3TvBySeriesIdSeasonBySeasonNumber) is what that
	// naming is there to avoid
	owner := im.pathName(method, path)
	if im.cfg.Naming == config.OperationIDNaming || op.Name != "" {
		owner = o.Name
	}
	o.Request = im.requestBody(method, path, owner, op.RequestBody)
	o.Response = im.responseBody(method, path, owner, op)
	o.ExpectedStatusCodes = im.expectedStatusCodes(method, path, op, o.Response)

	return o
}

func (im *importer) pathParamType(s *openapi.Schema) definitions.TypeRef {
	if s == nil {
		return definitions.TypeRef{Type: definitions.String}
	}
	if ref := s.RefName(); ref != "" && im.kindOf(ref) == kindEnum {
		return reference(typeName(ref))
	}
	switch s.Type {
	case openapi.TypeInteger:
		return integerType(s)
	case openapi.TypeNumber:
		return definitions.TypeRef{Type: definitions.Double}
	}

	return im.stringType(s)
}

func (im *importer) optionType(s *openapi.Schema) definitions.TypeRef {
	str := definitions.TypeRef{Type: definitions.String}
	if s == nil {
		return str
	}
	if ref := s.RefName(); ref != "" {
		if im.kindOf(ref) == kindEnum {
			return reference(typeName(ref))
		}
		if im.spec.Components.Schemas[ref] == nil {
			im.warn(fmt.Sprintf("parameter refers to undefined schema %s; sent as a string", ref))
		}
		return str
	}
	switch s.Type {
	case openapi.TypeString:
		return im.stringType(s)
	case openapi.TypeBoolean:
		return definitions.TypeRef{Type: definitions.Boolean}
	case openapi.TypeInteger:
		return integerType(s)
	case openapi.TypeNumber:
		return definitions.TypeRef{Type: definitions.Double}
	case openapi.TypeArray:
		item := str
		if s.Items != nil {
			if ref := s.Items.RefName(); ref != "" && im.kindOf(ref) == kindEnum {
				item = reference(typeName(ref))
			} else if s.Items.Type == openapi.TypeInteger {
				item = definitions.TypeRef{Type: definitions.Integer}
				if s.Items.Format == "int64" {
					item = definitions.TypeRef{Type: definitions.Integer64}
				}
			}
		}
		return definitions.TypeRef{Type: definitions.List, NestedItem: &item}
	case openapi.TypeObject:
		return definitions.TypeRef{Type: definitions.Dictionary, NestedItem: &str}
	}

	return str
}

// xmlMedia reports whether a media type is XML: Emby pairs every JSON body
// with an application/xml twin, which is not a file.
func xmlMedia(ct string) bool {
	ct = strings.ToLower(ct)
	return ct == "application/xml" || ct == "text/xml"
}

func (im *importer) requestBody(method, path, owner string, rb *openapi.RequestBody) *definitions.Body {
	if rb == nil || len(rb.Content) == 0 {
		return nil
	}
	if ct, ok := openapi.PickJSON(rb.Content); ok {
		s := rb.Content[ct].Schema
		switch {
		case s == nil:
			return &definitions.Body{ContentType: "application/json", Type: definitions.TypeRef{Type: definitions.RawObject}}
		// an inline object with its fields declared (TMDB writes every body
		// this way) is a model like any other
		case s.Type == openapi.TypeArray, s.RefName() != "", s.Type == openapi.TypeObject && len(s.Properties) > 0:
			return &definitions.Body{ContentType: "application/json", Type: im.typeRef(s, owner, "Request")}
		default:
			return &definitions.Body{ContentType: "application/json", Type: definitions.TypeRef{Type: definitions.RawObject}}
		}
	}
	// application/octet-stream, image/*, text/plain: sent as the caller's bytes
	for _, ct := range openapi.SortedKeys(rb.Content) {
		if !xmlMedia(ct) {
			return &definitions.Body{ContentType: ct, Type: definitions.TypeRef{Type: definitions.RawFile}}
		}
	}
	im.fail(fmt.Sprintf("%s %s: request body is XML only", method, path))

	return nil
}

func (im *importer) responseBody(method, path, owner string, op *openapi.Operation) *definitions.Body {
	for _, code := range openapi.SortedKeys(op.Responses) {
		if !strings.HasPrefix(code, "2") {
			continue
		}
		resp := op.Responses[code]
		if len(resp.Content) == 0 {
			continue
		}
		jsonType, hasJSON := openapi.PickJSON(resp.Content)
		other := ""
		for _, ct := range openapi.SortedKeys(resp.Content) {
			if !openapi.JSONMedia(ct) && !xmlMedia(ct) && other == "" {
				other = ct
			}
		}
		// a service that prefers JSON answers it whenever it is offered:
		// the text/plain Swashbuckle lists beside it is the same JSON, not
		// a file
		if hasJSON && im.cfg.PreferJSON {
			other = ""
		}
		if other != "" || !hasJSON {
			// text/css alongside JSON (Jellyfin's Branding/Css) is still a
			// file, and so is XML on its own (Emby's DLNA descriptions)
			if other == "" {
				other = openapi.SortedKeys(resp.Content)[0]
			}
			// text a workaround says is short enough to hold: read whole, where any other answer that is not JSON is a file
			if resp.Content[other].Text {
				return &definitions.Body{ContentType: other, Type: definitions.TypeRef{Type: definitions.String}}
			}
			return &definitions.Body{ContentType: other, Type: definitions.TypeRef{Type: definitions.RawFile}}
		}
		return &definitions.Body{ContentType: "application/json", Type: im.responseType(resp.Content[jsonType].Schema, owner)}
	}

	// an operation that answers a redirect answers no content of its own: what it says is where it sends its caller
	if _, only := redirects(op); method == http.MethodGet && !only {
		im.fail(fmt.Sprintf("GET %s: declares no response content, so nothing says whether it answers JSON or a file", path))
	}

	return nil
}

// redirects are the redirect statuses an operation documents, and whether it
// documents nothing else as an answer. One that documents only a redirect
// answers the redirect itself: a login that sends its caller on to where they
// were going. One that documents a 2xx beside it answers that, by way of the
// redirect, which is then followed. A redirect no operation documents is the
// base client's to refuse or follow, as its service says.
func redirects(op *openapi.Operation) (codes []int, only bool) {
	only = true
	for code := range op.Responses {
		n, err := strconv.Atoi(code)
		switch {
		case err != nil:
		case n >= 200 && n < 300:
			only = false
		case n >= 300 && n < 400:
			codes = append(codes, n)
		}
	}
	slices.Sort(codes)

	return codes, only && len(codes) > 0
}

// responseType maps a JSON response schema: untyped objects and "binary
// strings" (both specs use those for arbitrary JSON documents such as named
// configurations) are raw JSON for the caller.
func (im *importer) responseType(s *openapi.Schema, owner string) definitions.TypeRef {
	raw := definitions.TypeRef{Type: definitions.RawObject}
	if s == nil {
		return raw
	}
	if s.Type == openapi.TypeString && s.Format == "binary" {
		return raw
	}
	if s.RefName() == "" && (s.Type == openapi.TypeObject || s.Type == "") && len(s.Properties) == 0 {
		if _, ok := s.Additional(); !ok {
			return raw
		}
	}

	return im.typeRef(s, owner, "Answer")
}

// expectedStatusCodes are the statuses the generated method treats as an
// answer rather than an error: every 2xx the operation declares, and, for a
// service whose config says so (ExpectSameShape), any other status whose
// answer the document gives the same JSON shape as the success - a failure
// that still answers in full. A redirect the operation declares is expected
// too, and tells the base client what to do with one: handed back as the
// answer when the operation declares no 2xx, followed to the answer when it
// does.
func (im *importer) expectedStatusCodes(method, path string, op *openapi.Operation, success *definitions.Body) []int {
	codes, only := redirects(op)
	if only {
		return codes
	}

	for code, resp := range op.Responses {
		n, err := strconv.Atoi(code)
		if !strings.HasPrefix(code, "2") {
			if err == nil && im.cfg.ExpectSameShape && im.sameAnswer(resp, success, method+" "+path) {
				codes = append(codes, n)
			}
			continue
		}
		if err != nil {
			im.fail(fmt.Sprintf("%s %s: success response %q is not a status code", method, path, code))
			continue
		}
		codes = append(codes, n)
	}
	if !slices.ContainsFunc(codes, func(c int) bool { return c >= 200 && c < 300 }) {
		im.fail(fmt.Sprintf("%s %s: declares no success response", method, path))
	}
	slices.Sort(codes)

	return codes
}

// sameAnswer reports whether a response declares JSON of the success
// answer's type.
func (im *importer) sameAnswer(resp *openapi.Response, success *definitions.Body, owner string) bool {
	if resp == nil || success == nil || success.Type.Type == definitions.RawFile {
		return false
	}
	ct, ok := openapi.PickJSON(resp.Content)
	if !ok {
		return false
	}

	return im.responseType(resp.Content[ct].Schema, owner).Equal(success.Type)
}

// markPageable flags the list operations: the GETs that take the paging
// parameters one of the service's paging shapes names and answer a model
// with its paging properties (Emby's StartIndex and Limit, Items and
// TotalRecordCount; Sonarr's page and pageSize, records and totalRecords).
// The shapes are tried in order, and an operation that takes a shape's Start
// and matches none is named in the log with why, which is how a service's
// next shape is found. A service with no paging configured has none.
func (im *importer) markPageable(o *definitions.Operation) {
	if im.cfg.Paging == nil || o.Method != http.MethodGet {
		return
	}
	var reasons []string
	for _, paging := range append([]config.Paging{*im.cfg.Paging}, im.cfg.Pagings...) {
		pageable, reason := im.pageable(o, &paging)
		if pageable != nil {
			o.Pageable = pageable

			return
		}
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) > 0 {
		im.warn(fmt.Sprintf("%s %s takes a paging parameter and gets no Complete method: %s", o.Method, o.Path, strings.Join(reasons, "; ")))
	}
}

// pageable is how o pages by one shape, or why it does not: empty when it
// does not take the shape's Start at all, which is no list of this shape.
func (im *importer) pageable(o *definitions.Operation, paging *config.Paging) (pageable *definitions.Pageable, reason string) {
	var start, limit string
	for _, opt := range o.Options {
		if opt.In != definitions.InQuery || opt.Type.Type != definitions.Integer {
			continue
		}
		switch {
		case strings.EqualFold(opt.Name, paging.Start):
			start = opt.Field
		case paging.Limit != "" && strings.EqualFold(opt.Name, paging.Limit):
			limit = opt.Field
		}
	}
	if start == "" {
		return nil, ""
	}
	if paging.Limit != "" && limit == "" {
		return nil, fmt.Sprintf("it takes %s but not %s", paging.Start, paging.Limit)
	}
	if o.Response == nil || o.Response.Type.Type != definitions.Reference || im.models[o.Response.Type.ReferenceName] == nil {
		return nil, fmt.Sprintf("it takes %s but answers no object", paging.Start)
	}
	model := im.models[o.Response.Type.ReferenceName]
	var items, total *definitions.Field
	lists := 0
	for i := range model.Fields {
		f := &model.Fields[i]
		switch {
		case f.Type.Type == definitions.List && f.Type.NestedItem != nil && (paging.Items == "" || strings.EqualFold(f.JSONName, paging.Items)):
			items = f
			lists++
		case strings.EqualFold(f.JSONName, paging.Total) && f.Type.Type == definitions.Integer:
			total = f
		}
	}
	switch {
	case paging.Items != "" && items == nil:
		return nil, fmt.Sprintf("it takes %s but its answer %s has no list %s", paging.Start, model.Name, paging.Items)
	case items == nil:
		return nil, fmt.Sprintf("it takes %s but its answer %s has no list", paging.Start, model.Name)
	case paging.Items == "" && lists > 1:
		return nil, fmt.Sprintf("it takes %s but its answer %s has %d lists, so Items must name one", paging.Start, model.Name, lists)
	case total == nil:
		return nil, fmt.Sprintf("it takes %s but its answer %s has no %s", paging.Start, model.Name, paging.Total)
	}

	return &definitions.Pageable{
		StartIndexOption: start,
		LimitOption:      limit,
		ByPage:           paging.ByPage,
		FirstPage:        paging.FirstPage,
		ItemsField:       items.Name,
		TotalField:       total.Name,
		ItemType:         *items.Type.NestedItem,
	}, ""
}
