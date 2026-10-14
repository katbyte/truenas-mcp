package tools

import (
	"cmp"
	"slices"
	"time"
)

// Severities of what an audit finds, the gravest first: critical is data at risk or lost, warning what will become that if left, notice what is worth
// knowing and harms nothing yet.
const (
	critical = "critical"
	warning  = "warning"
	notice   = "notice"
)

var severities = []string{critical, warning, notice}

// auditFinding is one thing an audit found wrong.
type auditFinding struct {
	Audit    string `json:"audit,omitempty" jsonschema:"the audit that found it, in audit_all's answer"`
	Severity string `json:"severity"        jsonschema:"critical: data is at risk or lost; warning: it will be if left; notice: worth knowing, harming nothing yet"`
	Check    string `json:"check"           jsonschema:"what was found wrong, as one word that by_check counts"`
	Kind     string `json:"kind"            jsonschema:"what it is about: pool, vdev, disk, dataset, task, alert"`
	Name     string `json:"name"            jsonschema:"which one, as the tool that describes it takes it"`
	Pool     string `json:"pool,omitempty"  jsonschema:"the pool it is in"`
	Detail   string `json:"detail"          jsonschema:"the same in a sentence, with what it means and what to look at next"`
}

// auditIn is what every audit takes beside its own thresholds: how many findings to list, and from where.
type auditIn struct {
	pageIn
}

// auditOut is what every audit answers: what it found, the gravest first, counted by check, and how much it looked at.
type auditOut struct {
	Findings []auditFinding `json:"findings"`
	ByCheck  map[string]int `json:"by_check" jsonschema:"how many findings there are of each check, on this page or not"`
	Checked  int            `json:"checked"  jsonschema:"how many things the audit looked at: no findings beside nothing checked says there was nothing to look at, not that all is well"`
	pageOut
}

// findings collects what an audit finds, and answers it paged.
type findings struct {
	all     []auditFinding
	checked int
}

func (f *findings) add(severity, check, kind, name, pool, detail string) {
	f.all = append(f.all, auditFinding{Severity: severity, Check: check, Kind: kind, Name: name, Pool: pool, Detail: detail})
}

// answer is what was found, the gravest first and then by what it is about, one page of it, counted by check.
func (f *findings) answer(in pageIn) auditOut {
	slices.SortStableFunc(f.all, func(a, b auditFinding) int {
		return cmp.Or(cmp.Compare(slices.Index(severities, a.Severity), slices.Index(severities, b.Severity)), cmp.Compare(a.Pool, b.Pool), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Check, b.Check))
	})

	out := auditOut{ByCheck: map[string]int{}, Checked: f.checked}
	for _, found := range f.all {
		out.ByCheck[found.Check]++
	}
	out.Findings, out.pageOut = pageRows(f.all, in)

	return out
}

// at is when something happened, as part of a sentence: "" when it is not known.
func at(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return " at " + when(t)
}
