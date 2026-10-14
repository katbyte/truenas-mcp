package generator

import (
	"fmt"
	"strings"

	"github.com/katbyte/pandorest/definitions"
)

func (g *gen) modelFile(m *definitions.Model) string {
	g.declare(m.Name, "model "+m.Name)

	var b strings.Builder
	switch {
	case len(m.Members) > 0:
		fmt.Fprintf(&b, "// %s is one of several records, held as it came: ", m.Name)
		members := make([]string, 0, len(m.Members))
		for _, member := range m.Members {
			if len(member.Kinds) > 0 {
				members = append(members, fmt.Sprintf("%s (%s %s)", g.typeName(member.Model), m.Discriminator, strings.Join(quotedWords(member.Kinds), ", ")))
			} else {
				members = append(members, g.typeName(member.Model))
			}
		}
		b.WriteString(strings.Join(members, ", ") + ".\n")
		if m.Discriminator != "" {
			fmt.Fprintf(&b, "// Kind is the %q the record names itself by, and each As method hands the record over as that member, or an\n// error for a record of another kind. One is built from a member with client.UnionOf.\n", m.Discriminator)
		} else {
			b.WriteString("// Each As method hands the record over as that member, decoded strictly, so a record of another member's shape is an\n// error naming the key the member has no field for. One is built from a member with client.UnionOf.\n")
		}
	case len(m.Union) > 0:
		fmt.Fprintf(&b, "// %s is the %q schema, a oneOf/anyOf union kept as raw JSON;\n", m.Name, m.SchemaName)
		fmt.Fprintf(&b, "// decode it into one of: %s.\n", strings.Join(m.Union, ", "))
	case m.SchemaName != "":
		fmt.Fprintf(&b, "// %s is the %q schema.\n", m.Name, m.SchemaName)
	default:
		fmt.Fprintf(&b, "// %s is an object the document declares inline.\n", m.Name)
	}
	if m.Description != "" {
		b.WriteString("//\n")
		b.WriteString(comment("", m.Description))
	}

	switch {
	case len(m.Members) > 0:
		fmt.Fprintf(&b, "type %s struct {\n\tclient.Union\n}\n", m.Name)
		if m.Discriminator != "" {
			fmt.Fprintf(&b, "\n// Kind is the %q the record names itself by, \"\" for a union holding nothing.\nfunc (u %s) Kind() string { return u.Union.Kind(%q) }\n", m.Discriminator, m.Name, m.Discriminator)
		}
		for _, member := range m.Members {
			typ := g.typeName(member.Model)
			g.declare(m.Name+".As"+typ, "model "+m.Name)
			kinds := append([]string{fmt.Sprintf("%q", m.Discriminator)}, quotedWords(member.Kinds)...)
			fmt.Fprintf(&b, "\n// As%s hands the record over as a %s.\nfunc (u %s) As%s() (%s, error) { return client.UnionAs[%s](u.Union, %s) }\n", typ, typ, m.Name, typ, typ, typ, strings.Join(kinds, ", "))
		}
	case len(m.Union) > 0:
		fmt.Fprintf(&b, "type %s = json.RawMessage\n", m.Name)
	case len(m.Fields) == 0:
		fmt.Fprintf(&b, "type %s struct{}\n", m.Name) // gofumpt's spelling of an empty struct
	default:
		fmt.Fprintf(&b, "type %s struct {\n", m.Name)
		for i := range m.Fields {
			f := &m.Fields[i]
			if i > 0 && f.Description != "" {
				b.WriteString("\n")
			}
			b.WriteString(comment("\t", f.Description))
			fmt.Fprintf(&b, "\t%s %s `json:%q`\n", f.Name, g.fieldType(m, f), g.fieldTag(m, f))
		}
		b.WriteString("}\n")
		b.WriteString(g.accessors(m))
	}

	return g.fileHeader(b.String())
}

// quotedWords is each word as a Go string literal.
func quotedWords(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = fmt.Sprintf("%q", w)
	}

	return out
}

// accessors are GetName and GetId for a record that has a Name and an Id, so
// that code which handles records of several types alike can take them
// through one interface. A record with a field already so named gets none.
func (g *gen) accessors(m *definitions.Model) string {
	var name, id *definitions.Field
	for i := range m.Fields {
		f := &m.Fields[i]
		switch f.Name {
		case "GetName", "GetId":
			return ""
		case "Name":
			name = f
		case "Id":
			id = f
		}
	}
	if name == nil || id == nil {
		return ""
	}
	nameType, idType := g.fieldType(m, name), g.fieldType(m, id)
	if strings.TrimPrefix(nameType, "*") != "string" || strings.HasPrefix(idType, "[") || strings.HasPrefix(idType, "map") {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n// GetName is the record's Name, for code that handles records of several types alike.\n")
	if strings.HasPrefix(nameType, "*") {
		fmt.Fprintf(&b, "func (m %s) GetName() string {\n\tif m.Name == nil {\n\t\treturn \"\"\n\t}\n\n\treturn *m.Name\n}\n", m.Name)
	} else {
		fmt.Fprintf(&b, "func (m %s) GetName() string { return m.Name }\n", m.Name)
	}
	b.WriteString("\n// GetId is the record's Id, for code that handles records of several types alike.\n")
	fmt.Fprintf(&b, "func (m %s) GetId() %s { return m.Id }\n", m.Name, idType)

	return b.String()
}

func (g *gen) constantsFile(grp *definitions.Group) string {
	var b strings.Builder
	for i := range grp.Constants {
		c := &grp.Constants[i]
		possible := "PossibleValuesFor" + c.Name
		g.declare(c.Name, "constant "+c.Name)
		g.declare(possible, "constant "+c.Name)

		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "// %s is the %q enum.\n", c.Name, c.SchemaName)
		if c.Description != "" {
			b.WriteString("//\n")
			b.WriteString(comment("", c.Description))
		}
		fmt.Fprintf(&b, "type %s string\n\n", c.Name)

		fmt.Fprintf(&b, "// Values of %s.\nconst (\n", c.Name)
		for _, v := range c.Values {
			g.declare(v.Name, "constant "+c.Name)
			fmt.Fprintf(&b, "\t%s %s = %q\n", v.Name, c.Name, v.Value)
		}
		b.WriteString(")\n\n")

		fmt.Fprintf(&b, "// %s returns every value of %s.\n", possible, c.Name)
		fmt.Fprintf(&b, "func %s() []string {\n\treturn []string{\n", possible)
		for _, v := range c.Values {
			fmt.Fprintf(&b, "\t\tstring(%s),\n", v.Name)
		}
		b.WriteString("\t}\n}\n\n")
		fmt.Fprintf(&b, "// Known reports whether the value is one the document lists, or empty; a server may answer one it does not.\nfunc (v %s) Known() bool { return v == \"\" || slices.Contains(%s(), string(v)) }\n", c.Name, possible)
	}

	return g.fileHeader(b.String())
}
