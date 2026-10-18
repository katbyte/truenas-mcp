package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/katbyte/go-kt/pointer"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Starting, stopping and setting up the NAS's services, and dismissing its alerts.

// serviceFound reads one service by its name or its number, saying what to do when there is none.
func serviceFound(ctx context.Context, r *registry, service string) (*truenas.ServiceQueryResultItem, error) {
	filter := []any{"service", "=", service}
	if id, err := strconv.Atoi(service); err == nil {
		filter = []any{"id", "=", id}
	}
	found, err := r.typed.Service.QueryOne(ctx, []any{filter}, truenas.ServiceQueryOptions{})
	switch {
	case truenasrpc.IsNotFound(err):
		return nil, fmt.Errorf("the NAS has no service %q: service_list lists them, as the NAS names them (cifs is SMB)", service)
	case err != nil:
		return nil, fmt.Errorf("reading the service %s: %w", service, err)
	case found.Answer == nil:
		return nil, fmt.Errorf("the NAS answered nothing for the service %s", service)
	}

	return found.Answer, nil
}

// serviceRowOf is a service as a list answers it.
func serviceRowOf(s *truenas.ServiceQueryResultItem) serviceRow {
	return serviceRow{ID: s.Id, Service: s.Service, State: s.State, Enabled: s.Enable}
}

// serviceIn is a service named to a tool.
type serviceIn struct {
	Service string `json:"service"           jsonschema:"the service, as the NAS names it (cifs is SMB, nfs, ssh, iscsitarget, ups, snmp, ftp), or its number"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"say what would be done and do nothing"`
}

// serviceChangeOut is what a change to a service answers.
type serviceChangeOut struct {
	Service *serviceRow `json:"service,omitempty" jsonschema:"the service as it is now, read back"`
	changed
}

// serviceRead reads a service back after a change, the note saying so when it cannot be.
func serviceRead(ctx context.Context, r *registry, id int, c *changed) *serviceRow {
	s, err := serviceFound(ctx, r, strconv.Itoa(id))
	if err != nil {
		c.noted("it could not be read back afterwards: %v", err)

		return nil
	}
	row := serviceRowOf(s)

	return &row
}

func registerServiceChanges(r *registry) {
	for _, control := range []struct {
		name, description string
		verb              truenas.ServiceControlVerb
		// needed is whether a service in this state needs the verb at all
		needed      func(state string) bool
		would, done string
	}{
		{
			name:        "service_start",
			description: "Start a service now, such as SMB (cifs), NFS or SSH: a share is only served while its service runs. Whether it starts when the NAS does is service_update's. dry_run says what would be done.",
			verb:        truenas.ServiceControlVerbSTART,
			needed:      func(state string) bool { return state != "RUNNING" },
			would:       "would start %s", done: "started %s",
		},
		{
			name:        "service_stop",
			description: "Stop a service now: machines using it lose it, such as every share it serves, until it is started again. It still starts when the NAS does if it is set to. dry_run says what would be done.",
			verb:        truenas.ServiceControlVerbSTOP,
			needed:      func(state string) bool { return state == "RUNNING" },
			would:       "would stop %s", done: "stopped %s",
		},
		{
			name:        "service_restart",
			description: "Restart a service, or start it if it is stopped: machines using it are cut off for a moment, and it comes back with its settings read afresh. dry_run says what would be done.",
			verb:        truenas.ServiceControlVerbRESTART,
			needed:      func(string) bool { return true },
			would:       "would restart %s", done: "restarted %s",
		},
		{
			name:        "service_reload",
			description: "Have a running service read its settings afresh without stopping it, where it can; machines using it stay connected. dry_run says what would be done.",
			verb:        truenas.ServiceControlVerbRELOAD,
			needed:      func(state string) bool { return state == "RUNNING" },
			would:       "would have %s read its settings afresh", done: "had %s read its settings afresh",
		},
	} {
		add(r, writeTool, &mcp.Tool{Name: control.name, Description: control.description}, func(ctx context.Context, _ *mcp.CallToolRequest, in serviceIn) (*mcp.CallToolResult, serviceChangeOut, error) {
			s, err := serviceFound(ctx, r, strings.TrimSpace(in.Service))
			if err != nil {
				return nil, serviceChangeOut{}, err
			}
			row := serviceRowOf(s)

			out := serviceChangeOut{Service: &row, Changes: []string{}, DryRun: in.DryRun}
			if !control.needed(row.State) {
				out.noted("nothing needed doing: %s is %s", row.Service, strings.ToLower(row.State))

				return nil, out, nil
			}
			out.did(control.would, control.done, row.Service)
			if in.DryRun {
				return nil, out, nil
			}

			// told not to be silent, the NAS says why a service would not start rather than only that it did not
			done, err := r.typed.Service.Control(ctx, control.verb, row.Service, truenas.ServiceControlOptions{Silent: pointer.To(false)})
			switch {
			case err != nil:
				return nil, out, fmt.Errorf("%s was not changed: %w", row.Service, err)
			case done.Answer != nil && !*done.Answer:
				out.noted("the NAS answered that it did not do what was asked")
			}
			out.Service = serviceRead(ctx, r, row.ID, &out.changed)

			return nil, out, nil
		})
	}

	type serviceUpdateIn struct {
		Service string `json:"service"           jsonschema:"the service, as the NAS names it (cifs is SMB), or its number"`
		Enabled bool   `json:"enabled"           jsonschema:"whether it starts when the NAS does"`
		DryRun  bool   `json:"dry_run,omitempty" jsonschema:"say what would change and change nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "service_update",
		Description: "Set whether a service starts when the NAS does. It is not started or stopped now: service_start and service_stop do that. dry_run says what would change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in serviceUpdateIn) (*mcp.CallToolResult, serviceChangeOut, error) {
		s, err := serviceFound(ctx, r, strings.TrimSpace(in.Service))
		if err != nil {
			return nil, serviceChangeOut{}, err
		}
		row := serviceRowOf(s)

		out := serviceChangeOut{Service: &row, Changes: []string{}, DryRun: in.DryRun}
		var moves settings
		moves.move("whether "+row.Service+" starts with the NAS", onOff(row.Enabled), onOff(in.Enabled))
		moves.say(&out.changed)
		if in.DryRun || len(moves) == 0 {
			return nil, out, nil
		}

		if _, err := r.typed.Service.Update(ctx, row.ID, truenas.ServiceUpdate{Enable: pointer.To(in.Enabled)}); err != nil {
			return nil, out, fmt.Errorf("%s was not changed: %w", row.Service, err)
		}
		out.Service = serviceRead(ctx, r, row.ID, &out.changed)

		return nil, out, nil
	})
}

func registerAlertChanges(r *registry) {
	type alertChangeIn struct {
		ID     string `json:"id"                jsonschema:"the alert's own id, as alert_list answers it"`
		DryRun bool   `json:"dry_run,omitempty" jsonschema:"say what would be done and do nothing"`
	}
	type alertChangeOut struct {
		Alert *alertRow `json:"alert,omitempty" jsonschema:"the alert as it is now, read back"`
		changed
	}
	// alertFound is one alert by its id, dismissed or not
	alertFound := func(ctx context.Context, id string) (*alertRow, error) {
		found, err := r.typed.Alert.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the alerts: %w", err)
		}
		for i := range found.Answer {
			if found.Answer[i].Uuid == id {
				row := alertRowOf(&found.Answer[i])

				return &row, nil
			}
		}

		return nil, fmt.Errorf("the NAS has no alert %q: alert_list lists them, and the NAS clears an alert by itself once what raised it is gone", id)
	}

	for _, change := range []struct {
		name, description string
		dismiss           bool
		would, done       string
	}{
		{name: "alert_dismiss", description: "Dismiss an alert, so it is no longer counted among what is wrong or listed unless dismissed alerts are asked for. What raised it is not fixed, and alert_restore brings it back, but for some alerts the NAS raised once for something that happened, which it removes for good when dismissed. dry_run says what would be done.", dismiss: true, would: "would dismiss the alert %s", done: "dismissed the alert %s"},
		{name: "alert_restore", description: "Bring back an alert that was dismissed, so it is counted among what is wrong again. dry_run says what would be done.", would: "would bring back the alert %s", done: "brought back the alert %s"},
	} {
		add(r, writeTool, &mcp.Tool{Name: change.name, Description: change.description}, func(ctx context.Context, _ *mcp.CallToolRequest, in alertChangeIn) (*mcp.CallToolResult, alertChangeOut, error) {
			id := strings.TrimSpace(in.ID)
			row, err := alertFound(ctx, id)
			if err != nil {
				return nil, alertChangeOut{}, err
			}

			out := alertChangeOut{Alert: row, Changes: []string{}, DryRun: in.DryRun}
			if row.Dismissed == change.dismiss {
				out.noted("nothing needed doing: it is already %s", map[bool]string{true: "dismissed", false: "not dismissed"}[row.Dismissed])

				return nil, out, nil
			}
			out.did(change.would, change.done, row.Class)
			if change.dismiss && row.OneShot {
				out.noted("the NAS raised it once, for something that happened, and may remove such an alert for good when it is dismissed")
			}
			if in.DryRun {
				return nil, out, nil
			}

			if change.dismiss {
				_, err = r.typed.Alert.Dismiss(ctx, id)
			} else {
				_, err = r.typed.Alert.Restore(ctx, id)
			}
			if err != nil {
				return nil, out, fmt.Errorf("the alert %s was not changed: %w", id, err)
			}
			switch out.Alert, err = alertFound(ctx, id); {
			case err == nil:
			case change.dismiss && row.OneShot && strings.HasPrefix(err.Error(), "the NAS has no alert"):
				out.Note = "the NAS removed it for good when it was dismissed, as it does with some alerts raised once: it cannot be brought back"
			default:
				out.noted("it could not be read back afterwards: %v", err)
			}

			return nil, out, nil
		})
	}
}
