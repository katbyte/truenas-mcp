// Package outside makes text from outside safe to hand to a model: a release's name, a file's, anything fetched from a service anyone can write to.
//
// What cannot be seen is the easiest place to hide an instruction, so Text takes out the characters that do not show or that change how the rest
// shows, and cuts what is left to a length. It does not judge the words: what is left is still a stranger's, data and not instructions.
//
// Clean what is read, not what is handed back: a path or an id a later call takes must go back as it came, or it no longer finds the thing it names.
// A server's own words for what went wrong are from outside too and may quote an address with the server's own key on it; BlankAddresses is for
// those, not for a link a caller is to follow.
package outside

import (
	"unicode"

	"github.com/katbyte/go-kt/internal/addresses"
)

// cut is what text cut short ends with, inside the limit it was cut to.
const cut = "…"

// BlankAddresses blanks what each address in text may carry a credential in: who it signs in as, every value after its "?", and what follows its "#".
// Host, path and parameter names stay. It is for a server's words about what went wrong, which quote the address it called with its key on the end;
// not for a link a caller is to use. An address inside brackets keeps its closing bracket; any other mark against its end goes with the value. A
// secret in the path has no rule to find it and is left.
func BlankAddresses(text string) string { return addresses.Blank(text, nil) }

// Text is s with what does not show taken out and cut to limit characters, ending in an ellipsis where cut; 0 or less is no limit. Out go the control
// characters (a tab or line break becomes a space), the format characters that are zero-width or steer the direction of text, the tag characters and
// variation selectors a message can hide in, and the fillers that show as nothing. Letters, marks and spaces of every script stay; an emoji loses the
// selector for its coloured form.
func Text(s string, limit int) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case breaks(r):
			r = ' '
		case hidden(r):
			continue
		}

		out = append(out, r)
		if limit > 0 && len(out) > limit {
			return string(out[:limit-1]) + cut
		}
	}

	return string(out)
}

// breaks reports whether a character ends a line or stands where a gap does, and so is read as a space: a tab, the line breaks among the control
// characters, and the line and paragraph separators.
func breaks(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', 0x85, 0x2028, 0x2029:
		return true
	}

	return false
}

// hidden reports whether a character does not show, or changes how what is around it is shown.
func hidden(r rune) bool {
	switch {
	case unicode.Is(unicode.Cc, r):
		// the C0 and C1 control characters and DEL
		return true
	case unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Prepended_Concatenation_Mark, r):
		// the format characters, all but the few that are signs written before a number, which show
		return true
	case unicode.Is(unicode.Variation_Selector, r), unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
		// the variation selectors, and what else Unicode says is to show as nothing: the fillers and the like
		return true
	}

	return false
}
