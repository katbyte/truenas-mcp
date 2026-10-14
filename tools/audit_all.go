package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// audits are every audit, by name, each as audit_all runs it: with what it takes left as nothing, so each holds the NAS to its usual limits.
func audits(now time.Time) []struct {
	name string
	run  func(context.Context, *registry) (*findings, error)
} {
	return []struct {
		name string
		run  func(context.Context, *registry) (*findings, error)
	}{
		{"audit_pools", poolAudit{scrubDays: defaultScrubDays, now: now}.run},
		{"audit_space", spaceAudit{poolPercent: defaultPoolPercent, quotaPercent: defaultQuotaPercent}.run},
		{"audit_tasks", taskAudit{}.run},
		{"audit_disks", diskAudit{hddCelsius: defaultHDDCelsius, ssdCelsius: defaultSSDCelsius}.run},
		{"audit_system", systemAudit{certificateDays: defaultCertificateDays, now: now}.run},
	}
}

func registerAllAudit(r *registry) {
	type auditCount struct {
		Audit    string `json:"audit"`
		Critical int    `json:"critical"`
		Warning  int    `json:"warning"`
		Notice   int    `json:"notice"`
		Checked  int    `json:"checked"  jsonschema:"how many things it looked at"`
	}
	type auditAllOut struct {
		Audits   []auditCount `json:"audits"   jsonschema:"each audit, with how many findings of each severity it has: run the audit for its own limits"`
		Critical int          `json:"critical" jsonschema:"how many critical findings there are in all"`
		Warning  int          `json:"warning"  jsonschema:"how many warnings there are in all"`
		Notice   int          `json:"notice"   jsonschema:"how many notices there are in all"`
		auditOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "audit_all",
		Description: "Run every audit at its usual limits and say what is wrong with the NAS, the gravest first: each finding names the audit that found it, and each audit is counted by severity. Start here, then run an audit with a count above none for its own limits.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in auditIn) (*mcp.CallToolResult, auditAllOut, error) {
		all := &findings{}
		out := auditAllOut{}
		for _, audit := range audits(time.Now()) {
			found, err := audit.run(ctx, r)
			if err != nil {
				return nil, auditAllOut{}, fmt.Errorf("%s: %w", audit.name, err)
			}

			count := auditCount{Audit: audit.name, Checked: found.checked}
			for _, f := range found.all {
				f.Audit = audit.name
				all.all = append(all.all, f)
				switch f.Severity {
				case critical:
					count.Critical++
				case warning:
					count.Warning++
				default:
					count.Notice++
				}
			}
			all.checked += found.checked
			out.Critical, out.Warning, out.Notice = out.Critical+count.Critical, out.Warning+count.Warning, out.Notice+count.Notice
			out.Audits = append(out.Audits, count)
		}
		out.auditOut = all.answer(in.pageIn)

		return nil, out, nil
	})
}
