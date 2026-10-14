package importer

import (
	"go/token"
	"strings"
	"unicode"

	"github.com/katbyte/pandorest/openapi"
)

// lowerFirst lower-cases the first rune of an identifier.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])

	return string(r)
}

// typeName is the Go type name for a component schema.
func typeName(schemaName string) string { return openapi.Camel(schemaName) }

// fieldName is the Go field name for a property or option. Names are kept as
// the spec spells them (Id, ImdbId, Url) so they read like the API docs.
func fieldName(prop string) string { return openapi.Camel(prop) }

// reservedArgs are identifiers a generated method already uses (its locals,
// the packages it imports), plus Go keywords' neighbours among the
// predeclared names; a path argument that collides gets a "Param" suffix
// (e.g. `type` → `typeParam`).
var reservedArgs = func() map[string]bool {
	m := map[string]bool{}
	for n := range strings.FieldsSeq("c ctx input contentType options opts req resp result model items err client context http fmt url strconv io json " +
		"string int int64 float32 float64 bool error any len cap new make nil true false") {
		m[n] = true
	}

	return m
}()

// argName is the Go parameter name for a path parameter.
func argName(param string) string {
	n := lowerFirst(openapi.Camel(param))
	if n == "" {
		return "arg"
	}
	if token.IsKeyword(n) || reservedArgs[n] {
		n += "Param"
	}

	return n
}

// pathMethodName builds a method name from the HTTP method and path: GET
// /Items/{Id}/Similar → GetItemsByIdSimilar, GET
// /Videos/{Id}/stream.{Container} → GetVideosByIdStreamByContainer, GET
// /Videos/{Id}/master.m3u8 → GetVideosByIdMasterM3u8. prefix is trimmed from
// the path first (/api/v3 makes GET /api/v3/series/{id} GetSeriesById), and
// words spells a run-together segment (episodefile → EpisodeFile).
func pathMethodName(method, path, prefix string, words map[string]string) string {
	if prefix != "" && (path == prefix || strings.HasPrefix(path, prefix+"/")) {
		path = strings.TrimPrefix(path, prefix)
	}
	var b strings.Builder
	b.WriteString(openapi.Camel(strings.ToLower(method)))
	for seg := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		for _, piece := range splitTemplate(seg) {
			if piece.param {
				b.WriteString("By")
			}
			if w, ok := words[strings.ToLower(piece.text)]; ok && !piece.param {
				b.WriteString(w)
				continue
			}
			b.WriteString(openapi.Camel(piece.text))
		}
	}

	return b.String()
}

// templatePiece is a literal or a {parameter} piece of a path template segment.
type templatePiece struct {
	text  string
	param bool
}

// splitTemplate splits "stream.{Container}" into ["stream.", {Container}].
func splitTemplate(seg string) []templatePiece {
	var out []templatePiece
	for seg != "" {
		open := strings.IndexByte(seg, '{')
		if open < 0 {
			out = append(out, templatePiece{text: seg})
			break
		}
		if open > 0 {
			out = append(out, templatePiece{text: seg[:open]})
		}
		seg = seg[open+1:]
		closeIdx := strings.IndexByte(seg, '}')
		if closeIdx < 0 {
			out = append(out, templatePiece{text: seg})
			break
		}
		out = append(out, templatePiece{text: seg[:closeIdx], param: true})
		seg = seg[closeIdx+1:]
	}

	return out
}

// cleanText normalises a description for the definitions: no carriage
// returns, no trailing blanks on lines, no surrounding space.
func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}
