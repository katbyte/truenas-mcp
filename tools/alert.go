package tools

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// alertLevels are the levels an alert can be at, the gravest first.
var alertLevels = []string{"EMERGENCY", "ALERT", "CRITICAL", "ERROR", "WARNING", "NOTICE", "INFO"}

func registerAlertTools(r *registry) {
	type alertRow struct {
		ID        string `json:"id"                  jsonschema:"the alert's own id"`
		Level     string `json:"level"               jsonschema:"how grave it is: EMERGENCY, ALERT, CRITICAL, ERROR, WARNING, NOTICE or INFO"`
		Class     string `json:"class"               jsonschema:"the kind of alert, as the NAS names it: VolumeStatus, ScrubPaused, ReplicationFailed, CertificateExpired"`
		Text      string `json:"text"                jsonschema:"what the NAS says, written by the NAS from what it found"`
		Raised    string `json:"raised,omitempty"    jsonschema:"when it was raised"`
		LastSeen  string `json:"last_seen,omitempty" jsonschema:"when the NAS last found it still so"`
		Dismissed bool   `json:"dismissed"           jsonschema:"whether someone has dismissed it; the NAS clears an alert by itself once what raised it is gone"`
	}
	type alertListIn struct {
		Dismissed bool   `json:"dismissed,omitempty" jsonschema:"list the dismissed alerts too"`
		AtLeast   string `json:"at_least,omitempty"  jsonschema:"only alerts at this level or graver: WARNING leaves out NOTICE and INFO"`
		pageIn
	}
	type alertListOut struct {
		Alerts []alertRow `json:"alerts"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "alert_list",
		Description: "List the alerts the NAS has raised, the gravest first: its own word on what is wrong, from a failing disk or a pool that lost data to a task that failed or a certificate running out. Dismissed alerts are left out unless asked for.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in alertListIn) (*mcp.CallToolResult, alertListOut, error) {
		floor := len(alertLevels)
		if in.AtLeast != "" {
			if floor = slices.Index(alertLevels, strings.ToUpper(in.AtLeast)); floor < 0 {
				return nil, alertListOut{}, fmt.Errorf("at_least %q is no level: it is one of %s", in.AtLeast, strings.Join(alertLevels, ", "))
			}
			floor++
		}

		found, err := r.typed.Alert.List(ctx)
		if err != nil {
			return nil, alertListOut{}, fmt.Errorf("listing the alerts: %w", err)
		}

		rows := make([]alertRow, 0, len(found.Answer))
		for _, a := range found.Answer {
			level := slices.Index(alertLevels, a.Level)
			if (a.Dismissed && !in.Dismissed) || level < 0 && in.AtLeast != "" || level >= floor {
				continue
			}
			rows = append(rows, alertRow{ID: a.Uuid, Level: a.Level, Class: a.Klass, Text: a.Formatted, Raised: when(a.Datetime.Time), LastSeen: when(a.LastOccurrence.Time), Dismissed: a.Dismissed})
		}
		slices.SortStableFunc(rows, func(a, b alertRow) int {
			return cmp.Or(cmp.Compare(slices.Index(alertLevels, a.Level), slices.Index(alertLevels, b.Level)), cmp.Compare(b.Raised, a.Raised))
		})

		out := alertListOut{}
		out.Alerts, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}
