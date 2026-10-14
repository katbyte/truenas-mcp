package generator

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/katbyte/pandorest/definitions"
)

// timeFile declares Time and Day, the dates of a service whose config gives the layouts they are
// written in: a time.Time read from any of them and written in the first, left out of a body and a
// query when zero.
func (g *gen) timeFile() string {
	unreadFor := func(keep bool) unread {
		if keep {
			return keepingUnread
		}

		return failingUnread
	}

	var b strings.Builder
	if g.opts.ClientService != "" {
		fmt.Fprintf(&b, "// sendDateAs is the layout the service's own file picks for one parameter's date, or \"\" for the usual.\nfunc sendDateAs(operation, parameter string) string {\n\tif %[1]s.SendDateAs == nil {\n\t\treturn \"\"\n\t}\n\n\treturn %[1]s.SendDateAs(operation, parameter)\n}\n\n", g.opts.ClientService)
	}
	if len(g.svc.Dates) > 0 {
		g.declare("Time", "time.go")
		b.WriteString(dateType("Time", "a date and time", g.svc.Title, g.svc.Dates, unreadFor(g.svc.DatesKeepUnread)))
	}
	if len(g.svc.Days) > 0 {
		g.declare("Day", "time.go")
		b.WriteString(dateType("Day", "a day", g.svc.Title, g.svc.Days, unreadFor(g.svc.DaysKeepUnread)))
	}
	if g.kinds[definitions.UnixSeconds] {
		g.declare("Unix", "time.go")
		b.WriteString(epochType("Unix", "seconds", g.svc.Title))
	}
	if g.kinds[definitions.UnixMillis] {
		g.declare("UnixMilli", "time.go")
		b.WriteString(epochType("UnixMilli", "milliseconds", g.svc.Title))
	}

	return g.fileHeader(b.String())
}

// unread is how a date type treats a date no layout reads: its declaration, and what UnmarshalJSON
// does with the error.
type unread struct{ decl, handle string }

// failingUnread fails the decode on a date no layout reads; keepingUnread (KeepUnread) has the type hold
// the text as Unread and read as zero.
var (
	failingUnread = unread{"type %[1]s struct{ time.Time }\n", "\treturn err\n"}
	keepingUnread = unread{
		"type %[1]s struct {\n\ttime.Time\n\t// Unread is the text of a date no layout read, which is held as zero.\n\tUnread string\n}\n\n// UnreadDate is the text of a date no layout read, or \"\".\nfunc (t %[1]s) UnreadDate() string { return t.Unread }\n",
		"\tvar unread *client.UnreadDateError\n\tif errors.As(err, &unread) {\n\t\t// a date no layout reads is kept as its text and held as zero (KeepUnread)\n\t\tt.Unread = unread.Text\n\n\t\treturn nil\n\t}\n\n\treturn err\n",
	}
)

// dateType renders one date type over its layouts.
func dateType(name, what, title string, layouts []string, u unread) string {
	quoted := make([]string, 0, len(layouts))
	for _, l := range layouts {
		quoted = append(quoted, strconv.Quote(l))
	}
	lower := strings.ToLower(name)
	decl, unread := u.decl, u.handle
	// a service that writes a date as {"$date": milliseconds} goes through the base client's forms, which know that layout;
	// any other keeps the plain ones, so its time.go does not move
	marshal, text := "json.Marshal(t.Format(%[4]sLayouts[0]))", "t.Format(%[4]sLayouts[0])"
	asLayout := "t.Format(layout)"
	if slices.Contains(layouts, dateObjectMillis) {
		marshal, text, asLayout = "client.MarshalTime(t.Time, %[4]sLayouts[0])", "client.FormatTime(t.Time, %[4]sLayouts[0])", "client.FormatTime(t.Time, layout)"
	}

	return fmt.Sprintf(`// %[1]s is %[2]s as %[3]s writes it: read from any of %[4]sLayouts, written in the first, and
// left out of a body or a query when zero, which is what a null or an empty date reads as.
`+decl+`

// %[4]sLayouts are the layouts %[3]s writes %[2]s in, the first the one it is sent in.
var %[4]sLayouts = []string{%[5]s}

// MarshalJSON writes the date in the first layout, or null when zero.
func (t %[1]s) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}

	return `+marshal+`
}

// UnmarshalJSON reads the date in any of the layouts; null and "" are zero.
func (t *%[1]s) UnmarshalJSON(data []byte) error {
	parsed, err := client.ParseTime(data, %[4]sLayouts)
	if err != nil {
`+unread+`	}
	t.Time = parsed

	return nil
}

// String is the date in the first layout, which is how a query carries it.
func (t %[1]s) String() string { return `+text+` }

// As is the date in the layout given, or in the first when none is: what a query carries for a
// parameter the service's own file picks a layout for (client.Service.SendDateAs).
func (t %[1]s) As(layout string) string {
	if layout == "" {
		return t.String()
	}

	return `+asLayout+`
}

`, name, what, title, lower, strings.Join(quoted, ", "))
}

// epochType renders a date type over seconds or milliseconds since 1970: read from a number or a
// string of digits, written as a number, null and 0 zero.
func epochType(name, unit, title string) string {
	from, to := "time.Unix(n, 0)", "t.Unix()"
	if unit == "milliseconds" {
		from, to = "time.UnixMilli(n)", "t.UnixMilli()"
	}

	return fmt.Sprintf(`// %[1]s is a date and time %[2]s writes as %[3]s since 1970: read from a number, or a string of digits,
// written as a number, and left out of a body or a query when zero, which is what null and 0 read as.
type %[1]s struct{ time.Time }

// MarshalJSON writes the date as %[3]s since 1970, or null when zero.
func (t %[1]s) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}

	return json.Marshal(%[5]s)
}

// UnmarshalJSON reads a number of %[3]s since 1970; null and 0 are zero.
func (t *%[1]s) UnmarshalJSON(data []byte) error {
	n, err := client.ParseUnix(data)
	if err != nil {
		return err
	}
	if n != 0 {
		t.Time = %[4]s.UTC()
	}

	return nil
}

// String is the date as %[3]s since 1970, which is how a query carries it.
func (t %[1]s) String() string { return strconv.FormatInt(%[5]s, 10) }

`, name, title, unit, from, to)
}
