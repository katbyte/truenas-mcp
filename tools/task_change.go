package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/go-kt/pointer"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenas/poolscrub"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Making, changing, running and removing the tasks that take snapshots and scrub pools, and scrubbing a pool now.

// defaultNaming is how a snapshot task names its snapshots unless told otherwise, as the NAS itself does.
const defaultNaming = "auto-%Y-%m-%d_%H-%M"

// scrubStartWait is how long starting a scrub waits to hear it refused before saying it is running: a scrub can take days, and the NAS answers for
// it only at the end.
const scrubStartWait = 5 * time.Second

// cronLine is a schedule given as one line of cron.
type cronLine struct {
	minute, hour, dom, month, dow string
}

// cronOfLine reads one line of cron, five fields: minute, hour, day of the month, month and day of the week.
func cronOfLine(line string) (cronLine, error) {
	fields := strings.Fields(line)
	if len(fields) != 5 {
		return cronLine{}, fmt.Errorf("%q is no schedule: one is five fields of cron, minute hour day-of-month month day-of-week, as 0 3 * * 0 for three in the morning every Sunday", line)
	}

	return cronLine{minute: fields[0], hour: fields[1], dom: fields[2], month: fields[3], dow: fields[4]}, nil
}

// String is the schedule as a list answers it.
func (c cronLine) String() string { return cron(c.minute, c.hour, c.dom, c.month, c.dow) }

// windowOfLine reads the times of day a task runs between, as 09:00-18:00.
func windowOfLine(line string) (begin, end string, err error) {
	begin, end, ok := strings.Cut(strings.ReplaceAll(line, " ", ""), "-")
	_, beginErr := time.Parse("15:04", begin)
	_, endErr := time.Parse("15:04", end)
	if !ok || beginErr != nil || endErr != nil {
		return "", "", fmt.Errorf("%q is no window: one is two times of day, as 09:00-18:00", line)
	}

	return begin, end, nil
}

// lifetimeOf reads how long a snapshot is kept, as 2 WEEK or 3 days.
func lifetimeOf(line string) (int, truenas.PoolLifetimeUnit, error) {
	fields := strings.Fields(strings.ToUpper(line))
	if len(fields) == 2 {
		n, err := strconv.Atoi(fields[0])
		unit := strings.TrimSuffix(fields[1], "S")
		if err == nil && n > 0 && slices.Contains([]string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"}, unit) {
			return n, truenas.PoolLifetimeUnit(unit), nil
		}
	}

	return 0, "", fmt.Errorf("%q is no time to keep a snapshot for: one is a number and HOUR, DAY, WEEK, MONTH or YEAR, as 2 WEEK", line)
}

// snapshotTaskFound reads one snapshot task by its number, saying what to do when there is none.
func snapshotTaskFound(ctx context.Context, r *registry, id int) (*truenas.PoolSnapshotTaskQueryResultItem, error) {
	found, err := r.typed.PoolSnapshottask.QueryOne(ctx, []any{[]any{"id", "=", id}}, truenas.PoolSnapshottaskQueryOptions{})
	switch {
	case truenasrpc.IsNotFound(err):
		return nil, fmt.Errorf("the NAS has no snapshot task %d: snapshottask_list lists them", id)
	case err != nil:
		return nil, fmt.Errorf("reading the snapshot task %d: %w", id, err)
	case found.Answer == nil:
		return nil, fmt.Errorf("the NAS answered nothing for the snapshot task %d", id)
	}

	return found.Answer, nil
}

// snapshotTaskRead reads a snapshot task back after a change, the note saying so when it cannot be.
func snapshotTaskRead(ctx context.Context, r *registry, id int, c *changed) *snapshotTaskRow {
	t, err := snapshotTaskFound(ctx, r, id)
	if err != nil {
		c.noted("it could not be read back afterwards: %v", err)

		return nil
	}
	row := snapshotTaskRowOf(t)

	return &row
}

// snapshotTaskWords is what a snapshot task does, in words.
func snapshotTaskWords(row snapshotTaskRow) string {
	words := fmt.Sprintf("takes a snapshot of %s at %s, kept for %s", row.Dataset, row.Schedule, row.KeepFor)
	if row.Recursive {
		words = fmt.Sprintf("takes a snapshot of %s and every dataset under it at %s, kept for %s", row.Dataset, row.Schedule, row.KeepFor)
	}

	return words
}

// retained is the snapshots a change to a snapshot task changes the removal of, as the NAS answers them by dataset, in order.
func retained(by map[string][]string) []string {
	var names []string
	for _, dataset := range slices.Sorted(maps.Keys(by)) {
		for _, snapshot := range by[dataset] {
			names = append(names, dataset+"@"+snapshot)
		}
	}

	return names
}

// scrubTaskFound reads one scrub task by its number, saying what to do when there is none.
func scrubTaskFound(ctx context.Context, r *registry, id int) (*truenas.PoolScrubQueryResultItem, error) {
	found, err := r.typed.PoolScrub.QueryOne(ctx, []any{[]any{"id", "=", id}}, truenas.PoolScrubQueryOptions{})
	switch {
	case truenasrpc.IsNotFound(err):
		return nil, fmt.Errorf("the NAS has no scrub task %d: pool_scrub_list lists them", id)
	case err != nil:
		return nil, fmt.Errorf("reading the scrub task %d: %w", id, err)
	case found.Answer == nil:
		return nil, fmt.Errorf("the NAS answered nothing for the scrub task %d", id)
	}

	return found.Answer, nil
}

// scrubTaskRead reads a scrub task back after a change, the note saying so when it cannot be.
func scrubTaskRead(ctx context.Context, r *registry, id int, c *changed) *scrubRow {
	t, err := scrubTaskFound(ctx, r, id)
	if err != nil {
		c.noted("it could not be read back afterwards: %v", err)

		return nil
	}
	row := scrubRowOf(t)

	return &row
}

func registerSnapshotTaskChanges(r *registry) {
	type snapshotTaskChangeOut struct {
		SnapshotTask *snapshotTaskRow `json:"snapshot_task,omitempty" jsonschema:"the task as it is now, read back; absent on a dry run of making one"`
		changed
	}

	type snapshotTaskCreateIn struct {
		Dataset       string   `json:"dataset"                  jsonschema:"the dataset or volume it takes snapshots of, by its whole name (tank/media)"`
		Schedule      string   `json:"schedule"                 jsonschema:"when it runs, as cron: minute hour day-of-month month day-of-week; 0 * * * * is every hour, 0 0 * * * every midnight"`
		KeepFor       string   `json:"keep_for"                 jsonschema:"how long it keeps each snapshot before removing it: a number and HOUR, DAY, WEEK, MONTH or YEAR, as 2 WEEK"`
		NamingSchema  string   `json:"naming_schema,omitempty"  jsonschema:"how its snapshots are named, in strftime terms with the year, month, day, hour and minute in it; auto-%Y-%m-%d_%H-%M when not given"`
		Recursive     bool     `json:"recursive,omitempty"      jsonschema:"also take every dataset and volume under it, at the same moment"`
		Exclude       []string `json:"exclude,omitempty"        jsonschema:"with recursive, the datasets under it to leave out, by their whole names"`
		Window        string   `json:"window,omitempty"         jsonschema:"only take snapshots between these times of day, as 09:00-18:00; at any hour when not given"`
		SkipUnchanged bool     `json:"skip_unchanged,omitempty" jsonschema:"take no snapshot of a dataset unchanged since the last one"`
		Enabled       *bool    `json:"enabled,omitempty"        jsonschema:"whether it runs; on when not given"`
		DryRun        bool     `json:"dry_run,omitempty"        jsonschema:"say what would be made and make nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "snapshottask_create",
		Description: "Make a task that takes a snapshot of a dataset on a schedule and removes each one once it has been kept as long as asked. A replication copies what such a task takes. dry_run says what would be made.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotTaskCreateIn) (*mcp.CallToolResult, snapshotTaskChangeOut, error) {
		dataset := strings.Trim(strings.TrimSpace(in.Dataset), "/")
		schedule, err := cronOfLine(in.Schedule)
		if err != nil {
			return nil, snapshotTaskChangeOut{}, err
		}
		keep, unit, err := lifetimeOf(in.KeepFor)
		if err != nil {
			return nil, snapshotTaskChangeOut{}, err
		}
		begin, end := "00:00", "23:59"
		if in.Window != "" {
			if begin, end, err = windowOfLine(in.Window); err != nil {
				return nil, snapshotTaskChangeOut{}, err
			}
		}
		if len(in.Exclude) > 0 && !in.Recursive {
			return nil, snapshotTaskChangeOut{}, errors.New("exclude leaves out datasets under the one a recursive task takes: give recursive too")
		}
		naming := cmp.Or(strings.TrimSpace(in.NamingSchema), defaultNaming)
		if err := datasetNamed(ctx, r, dataset); err != nil {
			return nil, snapshotTaskChangeOut{}, err
		}

		out := snapshotTaskChangeOut{Changes: []string{}, DryRun: in.DryRun}
		words := snapshotTaskWords(snapshotTaskRow{Dataset: dataset, Recursive: in.Recursive, Schedule: schedule.String(), KeepFor: fmt.Sprintf("%d %s", keep, unit)})
		enabled := in.Enabled == nil || *in.Enabled
		if !enabled {
			words += ", switched off"
		}
		out.did("would make a task that %s", "made a task that %s", words)
		if in.DryRun {
			return nil, out, nil
		}

		made, err := r.typed.PoolSnapshottask.Create(ctx, truenas.PoolSnapshottaskCreateData{Dataset: dataset, Recursive: pointer.To(in.Recursive), Exclude: in.Exclude, LifetimeValue: keep, LifetimeUnit: unit, NamingSchema: naming, Schedule: &truenas.ReplicationTimeCronModel{Minute: schedule.minute, Hour: schedule.hour, Dom: schedule.dom, Month: schedule.month, Dow: schedule.dow, Begin: begin, End: end}, AllowEmpty: pointer.To(!in.SkipUnchanged), Enabled: pointer.To(enabled)})
		switch {
		case err != nil:
			return nil, out, fmt.Errorf("the snapshot task was not made: %w", err)
		case made.Answer == nil:
			return nil, out, errors.New("the NAS answered nothing for the snapshot task it made: snapshottask_list lists what it has")
		}
		out.SnapshotTask = snapshotTaskRead(ctx, r, made.Answer.Id, &out.changed)

		return nil, out, nil
	})

	type snapshotTaskUpdateIn struct {
		ID             int      `json:"id"                         jsonschema:"the task's number, as snapshottask_list answers it"`
		Dataset        string   `json:"dataset,omitempty"          jsonschema:"the dataset or volume it takes snapshots of from now on, by its whole name"`
		Schedule       string   `json:"schedule,omitempty"         jsonschema:"when it runs, as cron: minute hour day-of-month month day-of-week"`
		KeepFor        string   `json:"keep_for,omitempty"         jsonschema:"how long it keeps each snapshot before removing it, as 2 WEEK"`
		NamingSchema   string   `json:"naming_schema,omitempty"    jsonschema:"how its snapshots are named, in strftime terms"`
		Recursive      *bool    `json:"recursive,omitempty"        jsonschema:"whether it also takes every dataset and volume under it"`
		AddExclude     []string `json:"add_exclude,omitempty"      jsonschema:"datasets under it to leave out from now on, by their whole names"`
		RemoveExclude  []string `json:"remove_exclude,omitempty"   jsonschema:"datasets under it to take again"`
		Window         string   `json:"window,omitempty"           jsonschema:"only take snapshots between these times of day, as 09:00-18:00; 00:00-23:59 is at any hour"`
		SkipUnchanged  *bool    `json:"skip_unchanged,omitempty"   jsonschema:"whether it takes no snapshot of a dataset unchanged since the last one"`
		Enabled        *bool    `json:"enabled,omitempty"          jsonschema:"whether it runs"`
		KeepRemovalDue bool     `json:"keep_removal_due,omitempty" jsonschema:"have the snapshots it took already still removed when they were due to be, on the old terms"`
		DryRun         bool     `json:"dry_run,omitempty"          jsonschema:"say what would change and change nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "snapshottask_update",
		Description: "Change a snapshot task: its dataset, schedule, how long it keeps snapshots, how it names them, what it leaves out, or switch it on or off. Only what is given changes, and the answer says each setting it moved from what to what. A change to how long it keeps them or how it names them changes when the snapshots it took already are removed, and the answer names them. dry_run says what would change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotTaskUpdateIn) (*mcp.CallToolResult, snapshotTaskChangeOut, error) {
		t, err := snapshotTaskFound(ctx, r, in.ID)
		if err != nil {
			return nil, snapshotTaskChangeOut{}, err
		}
		row := snapshotTaskRowOf(t)

		out := snapshotTaskChangeOut{Changes: []string{}, DryRun: in.DryRun}
		data := truenas.PoolSnapshottaskUpdateData{}
		var moves settings
		if dataset := strings.Trim(strings.TrimSpace(in.Dataset), "/"); dataset != "" && dataset != row.Dataset {
			if err := datasetNamed(ctx, r, dataset); err != nil {
				return nil, out, err
			}
			moves.move("the dataset", row.Dataset, dataset)
			data.Dataset = dataset
		}
		schedule := &truenas.ReplicationTimeCronModel{}
		if t.Schedule != nil {
			*schedule = *t.Schedule
		}
		scheduled := false
		if in.Schedule != "" {
			line, err := cronOfLine(in.Schedule)
			if err != nil {
				return nil, out, err
			}
			if line.String() != row.Schedule {
				moves.move("the schedule", row.Schedule, line.String())
				schedule.Minute, schedule.Hour, schedule.Dom, schedule.Month, schedule.Dow, scheduled = line.minute, line.hour, line.dom, line.month, line.dow, true
			}
		}
		if in.Window != "" {
			begin, end, err := windowOfLine(in.Window)
			if err != nil {
				return nil, out, err
			}
			if window := windowOf(begin, end); window != row.Window {
				moves.move("the window", cmp.Or(row.Window, "any hour"), cmp.Or(window, "any hour"))
				schedule.Begin, schedule.End, scheduled = begin, end, true
			}
		}
		if scheduled {
			data.Schedule = schedule
		}
		if in.KeepFor != "" {
			keep, unit, err := lifetimeOf(in.KeepFor)
			if err != nil {
				return nil, out, err
			}
			if kept := fmt.Sprintf("%d %s", keep, unit); kept != row.KeepFor {
				moves.move("how long it keeps snapshots", row.KeepFor, kept)
				data.LifetimeValue, data.LifetimeUnit = keep, unit
			}
		}
		if naming := strings.TrimSpace(in.NamingSchema); naming != "" && naming != row.NamingSchema {
			moves.move("the naming schema", row.NamingSchema, naming)
			data.NamingSchema = naming
		}
		if in.Recursive != nil && *in.Recursive != row.Recursive {
			moves.move("recursive", onOff(row.Recursive), onOff(*in.Recursive))
			data.Recursive = in.Recursive
		}
		exclude := slices.Clone(row.Exclude)
		for _, d := range in.AddExclude {
			if !slices.Contains(exclude, d) {
				exclude = append(exclude, d)
			}
		}
		exclude = slices.DeleteFunc(exclude, func(d string) bool { return slices.Contains(in.RemoveExclude, d) })
		if !slices.Equal(exclude, row.Exclude) && (len(exclude) > 0 || len(row.Exclude) > 0) {
			moves.move("what it leaves out", cmp.Or(strings.Join(row.Exclude, ", "), "nothing"), cmp.Or(strings.Join(exclude, ", "), "nothing"))
			data.Exclude = orEmpty(exclude)
		}
		if in.SkipUnchanged != nil && *in.SkipUnchanged != row.SkipUnchanged {
			moves.move("skipping unchanged datasets", onOff(row.SkipUnchanged), onOff(*in.SkipUnchanged))
			data.AllowEmpty = pointer.To(!*in.SkipUnchanged)
		}
		if in.Enabled != nil && *in.Enabled != row.Enabled {
			moves.move("whether it runs", onOff(row.Enabled), onOff(*in.Enabled))
			data.Enabled = in.Enabled
		}
		moves.say(&out.changed)
		if len(moves) == 0 {
			out.SnapshotTask = &row

			return nil, out, nil
		}

		if data.Dataset != "" || data.LifetimeValue != 0 || data.NamingSchema != "" {
			will, err := r.typed.PoolSnapshottask.UpdateWillChangeRetentionFor(ctx, in.ID, truenas.PoolSnapshottaskUpdateWillChangeRetentionForData{Dataset: data.Dataset, LifetimeValue: data.LifetimeValue, LifetimeUnit: data.LifetimeUnit, NamingSchema: data.NamingSchema})
			switch names := retained(will.Answer); {
			case err != nil:
				out.noted("the NAS could not say which snapshots it took would be removed on other terms: %v", err)
			case len(names) > 0 && in.KeepRemovalDue:
				out.noted("%s it took are still removed when they were due to be: %s", several(len(names), "snapshot"), inWords(names))
			case len(names) > 0:
				out.noted("%s it took are removed on the new terms, or kept until removed by hand if they no longer fit them; keep_removal_due keeps them due when they were: %s", several(len(names), "snapshot"), inWords(names))
			default:
			}
		}
		if in.DryRun {
			out.SnapshotTask = &row

			return nil, out, nil
		}

		data.FixateRemovalDate = pointer.To(in.KeepRemovalDue)
		if _, err := r.typed.PoolSnapshottask.Update(ctx, in.ID, data); err != nil {
			return nil, out, fmt.Errorf("the snapshot task %d was not changed: %w", in.ID, err)
		}
		out.SnapshotTask = snapshotTaskRead(ctx, r, in.ID, &out.changed)

		return nil, out, nil
	})

	type snapshotTaskDeleteIn struct {
		ID             int  `json:"id"                         jsonschema:"the task's number, as snapshottask_list answers it"`
		KeepRemovalDue bool `json:"keep_removal_due,omitempty" jsonschema:"have the snapshots it took still removed when they were due to be; without this they are kept until removed by hand"`
		Confirm        bool `json:"confirm,omitempty"          jsonschema:"remove it: without this the answer says what would go and nothing is removed"`
	}
	type snapshotTaskDeleteOut struct {
		SnapshotTask snapshotTaskRow `json:"snapshot_task" jsonschema:"the task, as it was"`
		removal
	}
	add(r, deleteTool, &mcp.Tool{
		Name:        "snapshottask_delete",
		Description: "Remove a snapshot task, so no more snapshots are taken on its schedule. The snapshots it took stay, and are kept until removed by hand unless keep_removal_due has them removed when they were due. A task an enabled replication copies from is refused by the NAS. Without confirm the answer says what would go and removes nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotTaskDeleteIn) (*mcp.CallToolResult, snapshotTaskDeleteOut, error) {
		t, err := snapshotTaskFound(ctx, r, in.ID)
		if err != nil {
			return nil, snapshotTaskDeleteOut{}, err
		}
		will, err := r.typed.PoolSnapshottask.DeleteWillChangeRetentionFor(ctx, in.ID)
		if err != nil {
			return nil, snapshotTaskDeleteOut{}, fmt.Errorf("asking the NAS which snapshots the task %d would leave, so nothing was removed: %w", in.ID, err)
		}

		out := snapshotTaskDeleteOut{SnapshotTask: snapshotTaskRowOf(t)}
		out.goes("the task %d, which %s", in.ID, snapshotTaskWords(out.SnapshotTask))
		names := retained(will.Answer)
		if len(names) > 0 {
			if in.KeepRemovalDue {
				out.Note = fmt.Sprintf("%s it took are still removed when they were due to be: %s", several(len(names), "snapshot"), inWords(names))
			} else {
				out.Note = fmt.Sprintf("%s it took are kept until removed by hand: %s", several(len(names), "snapshot"), inWords(names))
			}
		}
		if !in.Confirm {
			out.Note = joined("nothing was removed: confirm removes it", out.Note)

			return nil, out, nil
		}

		if _, err := r.typed.PoolSnapshottask.Delete(ctx, in.ID, truenas.PoolSnapshottaskDeleteOptions{FixateRemovalDate: pointer.To(in.KeepRemovalDue)}); err != nil {
			return nil, out, fmt.Errorf("the snapshot task %d was not removed: %w", in.ID, err)
		}
		switch _, err := snapshotTaskFound(ctx, r, in.ID); {
		case err == nil:
			return nil, out, fmt.Errorf("the NAS answered that the snapshot task %d was removed, and still has it", in.ID)
		case !strings.HasPrefix(err.Error(), "the NAS has no snapshot task"):
			out.Note = joined(out.Note, "it was removed, and could not be looked for afterwards: "+err.Error())
		default:
		}
		out.done()

		return nil, out, nil
	})

	type snapshotTaskRunIn struct {
		ID     int  `json:"id"                jsonschema:"the task's number, as snapshottask_list answers it"`
		DryRun bool `json:"dry_run,omitempty" jsonschema:"say what would be taken and take nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "snapshottask_run",
		Description: "Have a snapshot task take its snapshot now, as it would on its schedule, kept and named the same. It runs in the background: snapshottask_list says how it went, and snapshot_list shows the snapshot. A task switched off is not run. dry_run says what would be taken.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotTaskRunIn) (*mcp.CallToolResult, snapshotTaskChangeOut, error) {
		t, err := snapshotTaskFound(ctx, r, in.ID)
		if err != nil {
			return nil, snapshotTaskChangeOut{}, err
		}
		row := snapshotTaskRowOf(t)
		if !row.Enabled {
			return nil, snapshotTaskChangeOut{}, fmt.Errorf("the snapshot task %d is switched off, and the NAS runs only one that is on: snapshottask_update with enabled turns it on", in.ID)
		}

		out := snapshotTaskChangeOut{SnapshotTask: &row, Changes: []string{}, DryRun: in.DryRun}
		out.did("would have the task %d take its snapshot of %s now", "had the task %d take its snapshot of %s now", in.ID, row.Dataset)
		if in.DryRun {
			return nil, out, nil
		}
		if _, err := r.typed.PoolSnapshottask.Run(ctx, in.ID); err != nil {
			return nil, out, fmt.Errorf("the snapshot task %d was not run: %w", in.ID, err)
		}
		out.noted("it runs in the background: snapshottask_list says how it went, and snapshot_list shows the snapshot")

		return nil, out, nil
	})
}

func registerScrubChanges(r *registry) {
	type scrubTaskChangeOut struct {
		ScrubTask *scrubRow `json:"scrub_task,omitempty" jsonschema:"the scrub task as it is now, read back; absent on a dry run of making one"`
		changed
	}

	type scrubCreateIn struct {
		Pool          string `json:"pool"                     jsonschema:"the pool to scrub, by name or by number"`
		Schedule      string `json:"schedule,omitempty"       jsonschema:"when it may start, as cron: minute hour day-of-month month day-of-week; 0 0 * * 0, midnight every Sunday, when not given"`
		ThresholdDays int    `json:"threshold_days,omitempty" jsonschema:"how many days must pass since the last scrub before a scheduled one runs, 1 or more; 35 when not given"`
		Description   string `json:"description,omitempty"    jsonschema:"what it is for, in words"`
		Enabled       *bool  `json:"enabled,omitempty"        jsonschema:"whether it runs; on when not given"`
		DryRun        bool   `json:"dry_run,omitempty"        jsonschema:"say what would be made and make nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "pool_scrub_create",
		Description: "Make the task that scrubs a pool on a schedule, reading its every block to find damage while the other copy can still mend it. A pool has one such task at most. dry_run says what would be made.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in scrubCreateIn) (*mcp.CallToolResult, scrubTaskChangeOut, error) {
		schedule, err := cronOfLine(cmp.Or(in.Schedule, "0 0 * * 0"))
		if err != nil {
			return nil, scrubTaskChangeOut{}, err
		}
		if in.ThresholdDays < 0 {
			return nil, scrubTaskChangeOut{}, errors.New("threshold_days is a number of days, 1 or more")
		}
		p, err := poolNamed(ctx, r, in.Pool)
		if err != nil {
			return nil, scrubTaskChangeOut{}, err
		}
		has, err := r.typed.PoolScrub.Query(ctx, []any{[]any{"pool", "=", p.Id}}, truenas.PoolScrubQueryOptions{})
		switch {
		case err != nil:
			return nil, scrubTaskChangeOut{}, fmt.Errorf("reading the scrub tasks of %s: %w", p.Name, err)
		case len(has.Answer) > 0:
			return nil, scrubTaskChangeOut{}, fmt.Errorf("the pool %s already has the scrub task %d: pool_scrub_update changes it", p.Name, has.Answer[0].Id)
		}

		out := scrubTaskChangeOut{Changes: []string{}, DryRun: in.DryRun}
		enabled := in.Enabled == nil || *in.Enabled
		off := ""
		if !enabled {
			off = ", switched off"
		}
		out.did("would make a task that scrubs %s at %s%s", "made a task that scrubs %s at %s%s", p.Name, schedule, off)
		if in.DryRun {
			return nil, out, nil
		}

		made, err := r.typed.PoolScrub.Create(ctx, truenas.PoolScrubCreateData{Pool: p.Id, Schedule: &truenas.Schedule{Minute: schedule.minute, Hour: schedule.hour, Dom: schedule.dom, Month: schedule.month, Dow: schedule.dow}, Threshold: in.ThresholdDays, Description: in.Description, Enabled: pointer.To(enabled)})
		switch {
		case err != nil:
			return nil, out, fmt.Errorf("the scrub task for %s was not made: %w", p.Name, err)
		case made.Answer == nil:
			return nil, out, errors.New("the NAS answered nothing for the scrub task it made: pool_scrub_list lists what it has")
		}
		out.ScrubTask = scrubTaskRead(ctx, r, made.Answer.Id, &out.changed)

		return nil, out, nil
	})

	type scrubUpdateIn struct {
		ID            int     `json:"id"                       jsonschema:"the scrub task's number, as pool_scrub_list answers it"`
		Schedule      string  `json:"schedule,omitempty"       jsonschema:"when it may start, as cron"`
		ThresholdDays int     `json:"threshold_days,omitempty" jsonschema:"how many days must pass since the last scrub before a scheduled one runs, 1 or more"`
		Description   *string `json:"description,omitempty"    jsonschema:"what it is for, in words; empty takes it away"`
		Enabled       *bool   `json:"enabled,omitempty"        jsonschema:"whether it runs"`
		DryRun        bool    `json:"dry_run,omitempty"        jsonschema:"say what would change and change nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "pool_scrub_update",
		Description: "Change a pool's scrub task: when it may start, how long it waits between scrubs, its description, or switch it on or off. Only what is given changes, and the answer says each setting it moved from what to what. dry_run says what would change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in scrubUpdateIn) (*mcp.CallToolResult, scrubTaskChangeOut, error) {
		t, err := scrubTaskFound(ctx, r, in.ID)
		if err != nil {
			return nil, scrubTaskChangeOut{}, err
		}
		row := scrubRowOf(t)
		if in.ThresholdDays < 0 {
			return nil, scrubTaskChangeOut{}, errors.New("threshold_days is a number of days, 1 or more")
		}

		out := scrubTaskChangeOut{Changes: []string{}, DryRun: in.DryRun}
		data := truenas.PoolScrubUpdateData{}
		var moves settings
		if in.Schedule != "" {
			line, err := cronOfLine(in.Schedule)
			if err != nil {
				return nil, out, err
			}
			if line.String() != row.Schedule {
				moves.move("the schedule", row.Schedule, line.String())
				data.Schedule = &truenas.Schedule{Minute: line.minute, Hour: line.hour, Dom: line.dom, Month: line.month, Dow: line.dow}
			}
		}
		if in.ThresholdDays > 0 && in.ThresholdDays != row.ThresholdDays {
			moves.move("the days between scrubs", strconv.Itoa(row.ThresholdDays), strconv.Itoa(in.ThresholdDays))
			data.Threshold = in.ThresholdDays
		}
		clearDescription := false
		if in.Description != nil && *in.Description != row.Description {
			moves.move("the description", quotedOrNone(row.Description), quotedOrNone(*in.Description))
			data.Description, clearDescription = *in.Description, *in.Description == ""
		}
		if in.Enabled != nil && *in.Enabled != row.Enabled {
			moves.move("whether it runs", onOff(row.Enabled), onOff(*in.Enabled))
			data.Enabled = in.Enabled
		}
		moves.say(&out.changed)
		if in.DryRun || len(moves) == 0 {
			out.ScrubTask = &row

			return nil, out, nil
		}

		// the client leaves out a setting it is given empty, so a description is taken away by a call of its own
		if len(moves) > 1 || !clearDescription {
			if _, err := r.typed.PoolScrub.Update(ctx, in.ID, data); err != nil {
				return nil, out, fmt.Errorf("the scrub task %d was not changed: %w", in.ID, err)
			}
		}
		if clearDescription {
			if err := r.nas.Call(ctx, "pool.scrub.update", []any{in.ID, map[string]any{"description": ""}}, nil); err != nil {
				return nil, out, fmt.Errorf("the description of the scrub task %d was not taken away: %w", in.ID, err)
			}
		}
		out.ScrubTask = scrubTaskRead(ctx, r, in.ID, &out.changed)

		return nil, out, nil
	})

	type scrubDeleteIn struct {
		ID      int  `json:"id"                jsonschema:"the scrub task's number, as pool_scrub_list answers it"`
		Confirm bool `json:"confirm,omitempty" jsonschema:"remove it: without this the answer says what would go and nothing is removed"`
	}
	type scrubDeleteOut struct {
		ScrubTask scrubRow `json:"scrub_task" jsonschema:"the scrub task, as it was"`
		removal
	}
	add(r, deleteTool, &mcp.Tool{
		Name:        "pool_scrub_delete",
		Description: "Remove a pool's scrub task, so the pool is scrubbed only when someone starts one. Without confirm the answer says what would go and removes nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in scrubDeleteIn) (*mcp.CallToolResult, scrubDeleteOut, error) {
		t, err := scrubTaskFound(ctx, r, in.ID)
		if err != nil {
			return nil, scrubDeleteOut{}, err
		}

		out := scrubDeleteOut{ScrubTask: scrubRowOf(t)}
		out.goes("the task %d, which scrubs %s at %s: the pool is scrubbed only when someone starts one", in.ID, out.ScrubTask.Pool, out.ScrubTask.Schedule)
		if !in.Confirm {
			out.Note = "nothing was removed: confirm removes it"

			return nil, out, nil
		}

		if _, err := r.typed.PoolScrub.Delete(ctx, in.ID); err != nil {
			return nil, out, fmt.Errorf("the scrub task %d was not removed: %w", in.ID, err)
		}
		switch _, err := scrubTaskFound(ctx, r, in.ID); {
		case err == nil:
			return nil, out, fmt.Errorf("the NAS answered that the scrub task %d was removed, and still has it", in.ID)
		case !strings.HasPrefix(err.Error(), "the NAS has no scrub task"):
			out.Note = "it was removed, and could not be looked for afterwards: " + err.Error()
		default:
		}
		out.done()

		return nil, out, nil
	})

	type scrubNowIn struct {
		Pool   string `json:"pool"              jsonschema:"the pool, by name or by number"`
		DryRun bool   `json:"dry_run,omitempty" jsonschema:"say what would be done and do nothing"`
	}
	type scrubNowOut struct {
		Pool string   `json:"pool"           jsonschema:"the pool"`
		Scan *scanRow `json:"scan,omitempty" jsonschema:"its scrub as it is now, read back"`
		changed
	}
	// scrubbing is whether a pool's scrub is running, and whether it is paused part way
	scrubbing := func(p truenas.PoolQueryResultItem) (running, paused bool) {
		s := scanOf(p.Scan)
		if s == nil || s.Function != "SCRUB" || s.State != "SCANNING" {
			return false, false
		}

		return !s.Paused, s.Paused
	}
	// scanRead reads a pool's scrub back after a change, the note saying so when it cannot be
	scanRead := func(ctx context.Context, name string, c *changed) *scanRow {
		p, err := poolNamed(ctx, r, name)
		if err != nil {
			c.noted("it could not be read back afterwards: %v", err)

			return nil
		}

		return scanOf(p.Scan)
	}

	add(r, writeTool, &mcp.Tool{
		Name:        "pool_scrub_start",
		Description: "Start scrubbing a pool now, reading its every block to find damage, or take up a paused scrub where it stopped. A scrub runs in the background for minutes or days and slows the pool while it does: pool_get says how far it has got. dry_run says what would be done.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in scrubNowIn) (*mcp.CallToolResult, scrubNowOut, error) {
		p, err := poolNamed(ctx, r, in.Pool)
		if err != nil {
			return nil, scrubNowOut{}, err
		}
		running, paused := scrubbing(p)

		out := scrubNowOut{Pool: p.Name, Scan: scanOf(p.Scan), Changes: []string{}, DryRun: in.DryRun}
		switch {
		case running:
			out.noted("nothing needed doing: %s is being scrubbed already", p.Name)

			return nil, out, nil
		case paused:
			out.did("would take up the paused scrub of %s", "took up the paused scrub of %s", p.Name)
		default:
			out.did("would start scrubbing %s", "started scrubbing %s", p.Name)
		}
		if in.DryRun {
			return nil, out, nil
		}

		// the NAS answers for a scrub only when it ends, so it is started and left to run once it has had a moment to be refused
		method, params := poolscrub.ScrubCall(p.Name, truenas.PoolScrubActionSTART)
		job, err := r.nas.Start(ctx, method, params)
		if err != nil {
			return nil, out, fmt.Errorf("the scrub of %s was not started: %w", p.Name, err)
		}
		select {
		case <-job.Done():
			if err := job.Wait(ctx, nil); err != nil {
				return nil, out, fmt.Errorf("the scrub of %s was not started: %w", p.Name, err)
			}
		case <-time.After(scrubStartWait):
		case <-ctx.Done():
			return nil, out, ctx.Err()
		}
		out.Scan = scanRead(ctx, p.Name, &out.changed)

		return nil, out, nil
	})

	for _, stop := range []struct {
		name, description string
		action            truenas.PoolScrubAction
		would, did        string
		paused            bool
	}{
		{name: "pool_scrub_pause", description: "Pause a pool's running scrub where it is, to spare the pool while it is busy: pool_scrub_start takes it up where it stopped. dry_run says what would be done.", action: truenas.PoolScrubActionPAUSE, would: "would pause the scrub of %s", did: "paused the scrub of %s"},
		{name: "pool_scrub_stop", description: "Stop a pool's scrub, running or paused, for good: the next one starts from the beginning. dry_run says what would be done.", action: truenas.PoolScrubActionSTOP, would: "would stop the scrub of %s", did: "stopped the scrub of %s", paused: true},
	} {
		add(r, writeTool, &mcp.Tool{Name: stop.name, Description: stop.description}, func(ctx context.Context, _ *mcp.CallToolRequest, in scrubNowIn) (*mcp.CallToolResult, scrubNowOut, error) {
			p, err := poolNamed(ctx, r, in.Pool)
			if err != nil {
				return nil, scrubNowOut{}, err
			}
			running, paused := scrubbing(p)

			out := scrubNowOut{Pool: p.Name, Scan: scanOf(p.Scan), Changes: []string{}, DryRun: in.DryRun}
			if !running && (!paused || !stop.paused) {
				out.noted("nothing needed doing: no scrub of %s is running", p.Name)

				return nil, out, nil
			}
			out.did(stop.would, stop.did, p.Name)
			if in.DryRun {
				return nil, out, nil
			}

			if _, err := r.typed.PoolScrub.Scrub(ctx, p.Name, stop.action); err != nil {
				return nil, out, fmt.Errorf("the scrub of %s was not changed: %w", p.Name, err)
			}
			out.Scan = scanRead(ctx, p.Name, &out.changed)

			return nil, out, nil
		})
	}
}
