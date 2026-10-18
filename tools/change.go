package tools

import (
	"context"
	"fmt"
	"strings"
)

// What every write and delete answers beside the thing it changed. A write takes dry_run and says what it would do; a delete does nothing without
// confirm and says what would go.

// changed is what a write answers of what it did, or of what it would do on a dry run.
type changed struct {
	Changes []string `json:"changes"           jsonschema:"what was done, in words; empty when nothing needed doing"`
	DryRun  bool     `json:"dry_run,omitempty" jsonschema:"nothing was done: changes says what would be"`
	Note    string   `json:"note,omitempty"    jsonschema:"anything more the change needs said, such as what could not be read back after it"`
}

// did notes one change, said as it would be on a dry run or as it was done otherwise.
func (c *changed) did(would, done string, args ...any) {
	said := done
	if c.DryRun {
		said = would
	}

	c.Changes = append(c.Changes, fmt.Sprintf(said, args...))
}

// noted adds to the note.
func (c *changed) noted(format string, args ...any) {
	c.Note = joined(c.Note, fmt.Sprintf(format, args...))
}

// setting is one setting a change would move, from what it is to what it would be, in words.
type setting struct {
	what     string
	from, to string
}

// settings are the settings an update would move, said as one change each; none when the update asks for what is already so.
type settings []setting

// move notes a setting when it would change.
func (s *settings) move(what, from, to string) {
	if from != to {
		*s = append(*s, setting{what: what, from: from, to: to})
	}
}

// say notes each setting moved as a change; with none, the note says nothing needed doing.
func (s *settings) say(c *changed) {
	for _, m := range *s {
		c.did("would set %s from %s to %s", "set %s from %s to %s", m.what, m.from, m.to)
	}
	if len(*s) == 0 {
		c.noted("nothing needed changing: it is already as asked")
	}
}

// removal is what a delete answers: what it would take away, and whether it did.
type removal struct {
	WouldGo []string `json:"would_go,omitempty" jsonschema:"what removing it takes away; absent once it is removed"`
	Removed bool     `json:"removed"            jsonschema:"whether it was removed: without confirm nothing is"`
	Note    string   `json:"note,omitempty"     jsonschema:"anything more the removal needs said, such as what confirm does"`
}

// goes notes something the delete would take away.
func (r *removal) goes(format string, args ...any) {
	r.WouldGo = append(r.WouldGo, fmt.Sprintf(format, args...))
}

// done marks the delete made: what would go has gone.
func (r *removal) done() {
	r.Removed, r.WouldGo = true, nil
}

// joined is two notes as one.
func joined(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}

	return strings.TrimSuffix(a, ".") + ". " + b
}

// onOff is a yes or no as a setting says it.
func onOff(b bool) string {
	if b {
		return "on"
	}

	return "off"
}

// orNone is a setting's text, or none when it has none.
func orNone(s string) string {
	if s == "" {
		return "none"
	}

	return s
}

// sizedOrNone is a size limit as a person reads it, or none when there is no limit.
func sizedOrNone(n int64) string {
	if n == 0 {
		return "none"
	}

	return sized(n)
}

// inWords is names as a sentence lists them, the first hundred and how many more.
func inWords(names []string) string {
	const most = 100
	if len(names) <= most {
		return strings.Join(names, ", ")
	}

	return fmt.Sprintf("%s and %d more", strings.Join(names[:most], ", "), len(names)-most)
}

// several is a count of things in words: 1 snapshot, 3 snapshots. The plural is the one given, or the thing with an s.
func several(n int, one string, many ...string) string {
	if n == 1 {
		return "1 " + one
	}
	if len(many) > 0 {
		return fmt.Sprintf("%d %s", n, many[0])
	}

	return fmt.Sprintf("%d %ss", n, one)
}

// blankOut takes settings away by setting each to empty, in a call of its own: the client leaves out a setting it is given empty, so an update
// through it cannot.
func blankOut(ctx context.Context, r *registry, method string, id any, names []string) error {
	if len(names) == 0 {
		return nil
	}
	blank := make(map[string]any, len(names))
	for _, name := range names {
		blank[name] = ""
	}

	return r.nas.Call(ctx, method, []any{id, blank}, nil)
}
