package importer

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/katbyte/pandorest/config"
	"github.com/katbyte/pandorest/definitions"
	"github.com/katbyte/pandorest/openapi"
)

// idLike matches a name that looks like an id: id, seriesId, episode_ids.
var idLike = regexp.MustCompile(`(?i)(^id$|ids?$)`)

// place is one spot an id can sit: a model's field, an operation's option or
// path parameter, with the name the document gives it and how to set its type.
type place struct {
	where, name string // "SeriesResource" or "GET /api/v3/series/{id}"; the field, option or parameter name
	typ         *definitions.TypeRef
	option      *definitions.Option // set for an option, for a joined list
	path        bool                // a path parameter
	model       string              // the model a field belongs to, "" otherwise
}

// idKind is one kind of id being applied: its config, its base, and what it reached.
type idKind struct {
	cfg    config.ID
	base   definitions.ObjectType
	fromOf int
	byName int
	byIn   int
	// joinedUsed is each Joined entry that reached a parameter, so one that reached none is refused
	joinedUsed map[string]bool
}

// applyIDs types every place a kind of id sits (config.Service.IDs) and
// returns the kinds the package declares. A place is a kind by its name
// (Names), by being the Id of a model the kind is of (Of), by being the bare
// {id} of an operation that answers or takes such a model, or by In; Except
// leaves a named place alone. It fails for a kind that reaches nothing, two
// kinds on one place, a name both in a kind and in Plain, a model Of names
// that is not there or has no Id, and an In or Except that matches nothing;
// it logs what each kind reached and the id-looking names no kind or Plain
// covers, which is the list a service's IDs are written from. A service
// that names no kind and nothing Plain has not opted in, and hears nothing.
func (im *importer) applyIDs(groups map[string]*definitions.Group) []definitions.IDKind {
	if len(im.cfg.IDs) == 0 {
		if len(im.cfg.Plain) > 0 {
			im.logUncovered(places(groups), nil)
		}

		return nil
	}
	kinds := make([]*idKind, 0, len(im.cfg.IDs))
	seen := map[string]bool{}
	for _, c := range im.cfg.IDs {
		if seen[c.Name] {
			im.fail(fmt.Sprintf("IDs declares %s twice", c.Name))
		}
		seen[c.Name] = true
		base, ok := im.idBase(c)
		if !ok {
			continue
		}
		kinds = append(kinds, &idKind{cfg: c, base: base})
	}
	for _, p := range im.cfg.Plain {
		if strings.Contains(p, " ") {
			continue // a place, which may leave one spot of a typed name alone
		}
		for _, c := range im.cfg.IDs {
			if slices.ContainsFunc(c.Names, func(n string) bool { return strings.EqualFold(n, p) }) {
				im.fail(fmt.Sprintf("%s is both a name of %s and in Plain", p, c.Name))
			}
		}
	}

	var plainPlaces []string
	for _, entry := range im.cfg.Plain {
		if strings.Contains(entry, " ") {
			plainPlaces = append(plainPlaces, entry)
		}
	}
	places := places(groups)
	typed := map[*definitions.TypeRef]string{}
	inUsed, exceptUsed := map[string]bool{}, map[string]bool{}
	for i := range places {
		p := &places[i]
		var claimed []*idKind
		var sources []string
		for _, k := range kinds {
			source, ok := claims(k, p, groups, inUsed, exceptUsed, plainPlaces)
			if ok {
				claimed = append(claimed, k)
				sources = append(sources, source)
			}
			if source == "Except" {
				// left alone on purpose: decided, so not among the uncovered
				typed[p.typ] = "-"
			}
		}
		switch len(claimed) {
		case 0:
			continue
		case 1:
		default:
			names := make([]string, 0, len(claimed))
			for _, k := range claimed {
				names = append(names, k.cfg.Name)
			}
			im.fail(fmt.Sprintf("%s %s is claimed by %s: narrow one with In or Except", p.where, p.name, strings.Join(names, " and ")))

			continue
		}
		k := claimed[0]
		if !im.typeAsID(k, p, sources[0]) {
			continue
		}
		typed[p.typ] = k.cfg.Name
	}

	out := make([]definitions.IDKind, 0, len(kinds))
	for _, k := range kinds {
		total := k.fromOf + k.byName + k.byIn
		if total == 0 {
			im.fail(fmt.Sprintf("the id kind %s reaches no field, option or path parameter: check its Of, Names and In against the document", k.cfg.Name))

			continue
		}
		for _, entry := range k.cfg.In {
			switch {
			case inUsed["stale "+k.cfg.Name+" "+entry]:
				im.fail(fmt.Sprintf("%s: In %q is not needed, Of or Names type it, so remove it", k.cfg.Name, entry))
			case !inUsed[k.cfg.Name+" "+entry]:
				im.fail(fmt.Sprintf("%s: In %q matches no plain id", k.cfg.Name, entry))
			}
		}
		for _, entry := range k.cfg.Except {
			if !exceptUsed[k.cfg.Name+" "+entry] {
				im.fail(fmt.Sprintf("%s: Except %q leaves nothing alone", k.cfg.Name, entry))
			}
		}
		for _, entry := range k.cfg.Joined {
			if !k.joinedUsed[strings.ToLower(entry)] {
				im.fail(fmt.Sprintf("%s: Joined %q matches no string parameter: it takes a parameter's name, not a place", k.cfg.Name, entry))
			}
		}
		im.log(fmt.Sprintf("%s: ids: %s in %s (%d from Of, %d by name, %d by In)", im.cfg.Name, k.cfg.Name, placesWord(total), k.fromOf, k.byName, k.byIn))
		if k.cfg.Shape != "" {
			re, err := regexp.Compile(k.cfg.Shape)
			switch {
			case err != nil:
				im.fail(fmt.Sprintf("%s: Shape %q is not a regular expression: %v", k.cfg.Name, k.cfg.Shape, err))
			case k.cfg.Example == "":
				im.fail(k.cfg.Name + ": a Shape needs an Example, a value of it for the generated tests")
			case !re.MatchString(k.cfg.Example):
				im.fail(fmt.Sprintf("%s: the Example %q is not of the Shape %q", k.cfg.Name, k.cfg.Example, k.cfg.Shape))
			case k.base != definitions.String && !digitsRe.MatchString(k.cfg.Example):
				im.fail(fmt.Sprintf("%s: the Example %q is not a number, which the kind is", k.cfg.Name, k.cfg.Example))
			}
		}
		out = append(out, definitions.IDKind{Name: k.cfg.Name, Base: k.base, Shape: k.cfg.Shape, Example: k.cfg.Example})
	}
	im.logUncovered(places, typed)

	return out
}

// digitsRe is what the Example of a number kind must be.
var digitsRe = regexp.MustCompile(`^-?\d+$`)

// idBase is the base type of a kind: its Base, or the type of the Id of the
// first model it is of.
func (im *importer) idBase(c config.ID) (definitions.ObjectType, bool) {
	switch c.Base {
	case "int":
		return definitions.Integer, true
	case "int64":
		return definitions.Integer64, true
	case "string":
		return definitions.String, true
	case "":
	default:
		im.fail(fmt.Sprintf("%s: Base %q is not int, int64 or string", c.Name, c.Base))

		return "", false
	}
	if len(c.Of) == 0 {
		im.fail(c.Name + ": an id kind needs the models it is of (Of) or a Base")

		return "", false
	}
	var base definitions.ObjectType
	for _, name := range c.Of {
		m := im.models[typeName(name)]
		if m == nil {
			im.fail(fmt.Sprintf("%s: Of names %s, which is not a model of the document", c.Name, name))

			return "", false
		}
		f := idField(m.Fields)
		if f == nil {
			im.fail(fmt.Sprintf("%s: Of names %s, which has no id", c.Name, name))

			return "", false
		}
		switch f.Type.Type {
		case definitions.Integer, definitions.Integer64, definitions.String:
		default:
			im.fail(fmt.Sprintf("%s: the id of %s is a %s, not a number or a string", c.Name, name, f.Type.Type))

			return "", false
		}
		if base != "" && base != f.Type.Type {
			im.fail(fmt.Sprintf("%s: the ids of %s are not all of one type", c.Name, strings.Join(c.Of, ", ")))

			return "", false
		}
		base = f.Type.Type
	}

	return base, true
}

// places lists every spot an id can sit.
func places(groups map[string]*definitions.Group) []place {
	var out []place
	for _, name := range openapi.SortedKeys(groups) {
		g := groups[name]
		for mi := range g.Models {
			m := &g.Models[mi]
			for fi := range m.Fields {
				f := &m.Fields[fi]
				out = append(out, place{where: m.Name, name: f.JSONName, typ: &f.Type, model: m.Name})
			}
		}
		for oi := range g.Operations {
			o := &g.Operations[oi]
			where := o.Method + " " + o.Path
			for pi := range o.PathParameters {
				p := &o.PathParameters[pi]
				out = append(out, place{where: where, name: p.Name, typ: &p.Type, path: true})
			}
			for ai := range o.Options {
				a := &o.Options[ai]
				out = append(out, place{where: where, name: a.Name, typ: &a.Type, option: a})
			}
			for ai := range o.Arguments {
				a := &o.Arguments[ai]
				out = append(out, place{where: where, name: a.Name, typ: &a.Type})
			}
		}
	}

	return out
}

// claims reports whether a kind claims a place, and by what: "In", "name"
// or "Of".
func claims(k *idKind, p *place, groups map[string]*definitions.Group, inUsed, exceptUsed map[string]bool, plainPlaces []string) (string, bool) {
	for _, entry := range k.cfg.Except {
		if openapi.PlaceOf(entry).Matches(p.where, p.name) {
			exceptUsed[k.cfg.Name+" "+entry] = true

			return "Except", false
		}
	}
	for _, entry := range plainPlaces {
		if openapi.PlaceOf(entry).Matches(p.where, p.name) {
			return "Except", false
		}
	}
	inferred := ""
	switch {
	case p.option != nil && slices.ContainsFunc(k.cfg.Joined, func(n string) bool { return strings.EqualFold(n, p.name) }):
		// a joined string is a parameter's shape alone, so a field of the name is not claimed by it; this goes before Names so a name in
		// both lists (prowlarr's tags, a joined parameter and fifteen fields) still counts the parameter as the Joined entry's use
		if k.joinedUsed == nil {
			k.joinedUsed = map[string]bool{}
		}
		k.joinedUsed[strings.ToLower(p.name)] = true
		inferred = "name"
	case slices.ContainsFunc(k.cfg.Names, func(n string) bool { return strings.EqualFold(n, p.name) }):
		inferred = "name"
	case strings.EqualFold(p.name, "id") && p.model != "" && slices.ContainsFunc(k.cfg.Of, func(n string) bool { return typeName(n) == p.model }):
		inferred = "Of"
	case strings.EqualFold(p.name, "id") && p.path && pathIsOf(k, p.where, groups):
		inferred = "Of"
	}
	for _, entry := range k.cfg.In {
		if openapi.PlaceOf(entry).Matches(p.where, p.name) {
			if inferred != "" {
				// stale: the inference types it the same, so the entry would sit unneeded in every list
				inUsed[k.cfg.Name+" "+entry] = false
				inUsed["stale "+k.cfg.Name+" "+entry] = true

				return inferred, true
			}
			inUsed[k.cfg.Name+" "+entry] = true

			return "In", true
		}
	}

	return inferred, inferred != ""
}

// pathIsOf reports whether the path of the operation at where is one of the
// kind's: an operation on that path, by any method, answers or takes one of
// idField is a model's own id, spelt id or Id (Emby and Jellyfin capitalise theirs), or nil.
func idField(fields []definitions.Field) *definitions.Field {
	for i := range fields {
		if strings.EqualFold(fields[i].JSONName, "id") {
			return &fields[i]
		}
	}

	return nil
}

// the kind's models, alone or as a list; or the path is under such a path
// (/movie/{id}/folder under /movie/{id}), whose {id} it carries. A delete
// answers nothing and a sub-path answers something else, so the GET or PUT
// beside them is what says whose id it is.
func pathIsOf(k *idKind, where string, groups map[string]*definitions.Group) bool {
	_, path, _ := strings.Cut(where, " ")
	for _, g := range groups {
		for oi := range g.Operations {
			o := &g.Operations[oi]
			if o.Path != path && !strings.HasPrefix(path, o.Path+"/") {
				continue
			}
			for _, body := range []*definitions.Body{o.Response, o.Request} {
				if body == nil {
					continue
				}
				t := body.Type
				if t.Type == definitions.List && t.NestedItem != nil {
					t = *t.NestedItem
				}
				if t.Type == definitions.Reference && slices.ContainsFunc(k.cfg.Of, func(n string) bool { return typeName(n) == t.ReferenceName }) {
					return true
				}
			}
		}
	}

	return false
}

// typeAsID sets a place's type to the kind, checking the base fits: a number
// kind on a number, a string kind on a string, a list of either on a list,
// and a kind of either on a path the document types as text, which the
// method then formats; a joined name is a list of the kind sent as one string.
func (im *importer) typeAsID(k *idKind, p *place, source string) bool {
	id := definitions.TypeRef{Type: definitions.ID, ReferenceName: k.cfg.Name}
	joined := p.option != nil && slices.ContainsFunc(k.cfg.Joined, func(n string) bool { return strings.EqualFold(n, p.name) })
	switch {
	case joined && p.typ.Type == definitions.String:
		*p.typ = definitions.TypeRef{Type: definitions.List, NestedItem: &id}
		p.option.CommaSeparated = true
	case joined:
		im.fail(fmt.Sprintf("%s %s is Joined for %s but is a %s, not a string", p.where, p.name, k.cfg.Name, p.typ.Type))

		return false
	case p.typ.Type == definitions.List && p.typ.NestedItem != nil && baseFits(k.base, p.typ.NestedItem.Type, false):
		*p.typ.NestedItem = id
	case baseFits(k.base, p.typ.Type, p.path):
		if p.option != nil && p.typ.Type == definitions.String && strings.HasSuffix(strings.ToLower(p.name), "ids") {
			// Emby's ExcludeItemIds is one text value carrying several, which only Joined says; typed as one id, a caller finds out by reading the options
			im.oneIDTexts = append(im.oneIDTexts, p.name+" in "+p.where)
		}
		*p.typ = id
	default:
		hint := ""
		if p.option != nil && p.typ.Type == definitions.String {
			hint = "; a string that carries several joined by commas is named in Joined"
		}
		im.fail(fmt.Sprintf("%s %s is a %s, which cannot be a %s (%s)%s", p.where, p.name, p.typ.Type, k.cfg.Name, k.base, hint))

		return false
	}
	switch source {
	case "Of":
		k.fromOf++
	case "In":
		k.byIn++
	default:
		k.byName++
	}

	return true
}

// baseFits reports whether a kind of the base can stand where the type is.
func baseFits(base, typ definitions.ObjectType, path bool) bool {
	switch base {
	case definitions.String:
		return typ == definitions.String
	default:
		return typ == definitions.Integer || typ == definitions.Integer64 || (path && typ == definitions.String)
	}
}

// plain reports whether Plain leaves a place alone: by its name, wherever
// it sits, or by a place as In takes it ("SeasonPassSeriesResource .id").
func (im *importer) plain(p *place) bool {
	for _, entry := range im.cfg.Plain {
		if strings.Contains(entry, " ") {
			if openapi.PlaceOf(entry).Matches(p.where, p.name) {
				return true
			}
		} else if strings.EqualFold(entry, p.name) {
			return true
		}
	}

	return false
}

// logUncovered lists the id-looking names no kind typed and Plain does not
// name, with where they sit, a few places each; and, since Plain "id" is one
// word over many places, the models whose bare id it leaves plain, where a
// kind's id under another model's name would hide.
func (im *importer) logUncovered(places []place, typed map[*definitions.TypeRef]string) {
	if n := len(im.oneIDTexts); n == 1 {
		im.log(fmt.Sprintf("%s: 1 text parameter named like a list is typed as one id (name it in Joined if it carries several joined by commas): %s", im.cfg.Name, im.oneIDTexts[0]))
	} else if n > 1 {
		im.log(fmt.Sprintf("%s: %d text parameters named like lists are typed as one id each (name each in Joined if it carries several joined by commas): %s", im.cfg.Name, n, strings.Join(im.oneIDTexts, ", ")))
	}
	var plainModels []string
	byName := map[string][]string{}
	for i := range places {
		p := &places[i]
		if strings.EqualFold(p.name, "id") && p.model != "" && (typed[p.typ] == "" || typed[p.typ] == "-") && im.plain(p) {
			plainModels = append(plainModels, p.model)
		}
		if !idLike.MatchString(p.name) || typed[p.typ] != "" || im.plain(p) {
			continue
		}
		switch p.typ.Type {
		case definitions.Integer, definitions.Integer64, definitions.String:
		case definitions.List:
			if p.typ.NestedItem == nil || (p.typ.NestedItem.Type != definitions.Integer && p.typ.NestedItem.Type != definitions.Integer64 && p.typ.NestedItem.Type != definitions.String) {
				continue
			}
		default:
			continue
		}
		byName[p.name] = append(byName[p.name], p.where)
	}
	if len(plainModels) > 0 && len(im.cfg.IDs) > 0 {
		// a model named for a kind's model (SeasonPassSeriesResource for SeriesResource) is where a kind's id hides, so it goes first
		looksLike := func(model string) bool {
			for _, c := range im.cfg.IDs {
				for _, of := range c.Of {
					stem := strings.TrimSuffix(strings.TrimSuffix(typeName(of), "Resource"), "Dto")
					if stem != "" && typeName(of) != model && strings.Contains(model, stem) {
						return true
					}
				}
			}

			return false
		}
		slices.SortStableFunc(plainModels, func(a, b string) int {
			switch la, lb := looksLike(a), looksLike(b); {
			case la && !lb:
				return -1
			case lb && !la:
				return 1
			default:
				return strings.Compare(a, b)
			}
		})
		im.log(fmt.Sprintf("%s: the bare id of %s is left plain (those named like a kind's model first): %s", im.cfg.Name, placesWord(len(plainModels)), strings.Join(plainModels, ", ")))
	}
	if len(byName) == 0 {
		return
	}
	lines := make([]string, 0, len(byName))
	for _, name := range openapi.SortedKeys(byName) {
		spots := byName[name]
		more := ""
		if len(spots) > 3 {
			more = fmt.Sprintf(" and %d more", len(spots)-3)
			spots = spots[:3]
		}
		lines = append(lines, fmt.Sprintf("%s in %s%s", name, strings.Join(spots, ", "), more))
	}
	heading := fmt.Sprintf("%d names look like ids and have no kind", len(byName))
	if len(byName) == 1 {
		heading = "1 name looks like an id and has no kind"
	}
	im.log(fmt.Sprintf("%s: %s (give each a kind in IDs, or leave it in Plain): %s", im.cfg.Name, heading, strings.Join(lines, "; ")))
}

// placesWord words a count of places.
func placesWord(n int) string {
	if n == 1 {
		return "1 place"
	}

	return fmt.Sprintf("%d places", n)
}

// VerifyIDs is the test a service's IDs and Plain must pass against its
// document, with its workarounds already applied: every kind reaches a
// place, no two kinds claim one, every In and Except matches, and no name is
// both in a kind and in Plain. It is what the import refuses, as an error
// a test can read; a repository calls it from the test it already has.
func VerifyIDs(cfg config.Service, spec *openapi.Spec) error {
	_, err := FromSpec(cfg, spec, nil, nil)

	return err
}
