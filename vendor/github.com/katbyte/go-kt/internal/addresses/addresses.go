// Package addresses blanks what an address in text may carry a credential in: the one rule behind outside.BlankAddresses and the registry's write
// line, kept here so the registry can also be told what was blanked.
package addresses

import (
	"regexp"
	"strings"
)

// Blanked stands for a value that is not shown.
const Blanked = "REDACTED"

// found finds each address: a scheme, "://" and what follows up to a space, a quote or an angle bracket.
var found = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// closers are the brackets an address may be written inside.
var closers = map[byte]byte{'[': ']', '(': ')', '{': '}'}

// Blank blanks, in each address in text, who it signs in as, every value after its "?" and what follows its "#", and hands each piece blanked to
// hidden when given. An address inside brackets ends at the closing one; any other mark against its end goes with the value. A secret in the path has
// no rule to find it.
func Blank(text string, hidden func(string)) string {
	places := found.FindAllStringIndex(text, -1)
	if places == nil {
		return text
	}
	if hidden == nil {
		hidden = func(string) {}
	}

	var out strings.Builder
	done := 0
	for _, place := range places {
		start, end := place[0], place[1]
		if closer, inside := closers[before(text, start)]; inside {
			if within := strings.TrimRight(text[start:end], ".,;:!?"); strings.HasSuffix(within, string(closer)) {
				end = start + len(within) - 1
			}
		}

		out.WriteString(text[done:start])
		out.WriteString(one(text[start:end], hidden))
		done = end
	}
	out.WriteString(text[done:])

	return out.String()
}

// before is the byte before a place in text, 0 at its start.
func before(text string, at int) byte {
	if at == 0 {
		return 0
	}

	return text[at-1]
}

// one blanks a single address.
func one(raw string, hidden func(string)) string {
	rest, fragment, _ := strings.Cut(raw, "#")
	rest, query, hasQuery := strings.Cut(rest, "?")
	scheme, rest, _ := strings.Cut(rest, "://")
	host, path, hasPath := strings.Cut(rest, "/")

	if at := strings.LastIndex(host, "@"); at >= 0 {
		user, password, _ := strings.Cut(host[:at], ":")
		hidden(user)
		hidden(password)
		host = Blanked + host[at:]
	}
	out := scheme + "://" + host
	if hasPath {
		out += "/" + path
	}

	if hasQuery {
		parts := strings.Split(query, "&")
		for i, part := range parts {
			name, value, named := strings.Cut(part, "=")
			switch {
			case named && value != "":
				hidden(value)
				parts[i] = name + "=" + Blanked
			case !named && part != "":
				hidden(part)
				parts[i] = Blanked
			}
		}
		out += "?" + strings.Join(parts, "&")
	}
	if fragment != "" {
		hidden(fragment)
		out += "#" + Blanked
	}

	return out
}
