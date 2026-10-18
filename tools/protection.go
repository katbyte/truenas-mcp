package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/katbyte/go-kt/pointer"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What keeps a NAS's data safe: its snapshots, the tasks that take them, and the tasks that copy data elsewhere or check it.

// lastRun is how a task's last run went, as each list answers it.
type lastRun struct {
	State    string `json:"state,omitempty"    jsonschema:"how it went: SUCCESS, FAILED, ABORTED, or RUNNING while it runs; none for a task that has not run"`
	Finished string `json:"finished,omitempty" jsonschema:"when it ended"`
	Error    string `json:"error,omitempty"    jsonschema:"why it failed, in the NAS's words"`
}

// runOf is a task's last run from the job the NAS keeps of it, or nil for a task that has never run.
func runOf(job *truenas.CoreGetJobsItemQueryResultItem) *lastRun {
	if job == nil {
		return nil
	}

	return &lastRun{State: job.State, Finished: when(job.TimeFinished.Time), Error: job.Error}
}

// standing is where a snapshot task or a replication stands, as the replication engine says.
type standing struct {
	State string `json:"state,omitempty" jsonschema:"PENDING before its first run, RUNNING, FINISHED, ERROR, or HOLD when it cannot run"`
	When  string `json:"when,omitempty"  jsonschema:"when it last changed"`
	Error string `json:"error,omitempty" jsonschema:"why it failed or is held, in the NAS's words"`
}

// standingOf is a task's state as an answer gives it, or nil when the NAS says nothing of it.
func standingOf(s *truenas.TaskState) *standing {
	if s == nil || s.State == "" {
		return nil
	}

	return &standing{State: s.State, When: when(s.Datetime.Time), Error: orNothing(s.Error, s.Reason)}
}

// cron is a schedule as one line of cron, "0 3 * * 7" for three in the morning every Sunday, or "" for none.
func cron(minute, hour, dom, month, dow string) string {
	if minute+hour+dom+month+dow == "" {
		return ""
	}
	parts := []string{minute, hour, dom, month, dow}
	for i, p := range parts {
		if p == "" {
			parts[i] = "*"
		}
	}

	return strings.Join(parts, " ")
}

// cronOf is a task's schedule as one line of cron.
func cronOf(s *truenas.Schedule) string {
	if s == nil {
		return ""
	}

	return cron(s.Minute, s.Hour, s.Dom, s.Month, s.Dow)
}

// windowOf is the times of day a task runs between, as 09:00-18:00, or "" for a task that runs at any hour.
func windowOf(begin, end string) string {
	if begin == "" && end == "" || begin == "00:00" && end == "23:59" {
		return ""
	}

	return begin + "-" + end
}

// snapshotsAtOnce is how many snapshots are asked after at once: each is one quick question, and the NAS takes ten at a time from one connection.
const snapshotsAtOnce = 8

func registerProtectionTools(r *registry) {
	registerSnapshotTools(r)
	registerSnapshotTaskTools(r)
	registerReplicationTools(r)
	registerCopyTaskTools(r)
	registerScrubTools(r)
}

// snapshotRow is one snapshot as a list answers it.
type snapshotRow struct {
	Name            string `json:"name"              jsonschema:"the snapshot's whole name, dataset@snapshot, by which it is named to any other tool"`
	Dataset         string `json:"dataset"           jsonschema:"the dataset it is a snapshot of"`
	Snapshot        string `json:"snapshot"          jsonschema:"its own name, after the @"`
	Created         string `json:"created,omitempty" jsonschema:"when it was taken"`
	UsedBytes       int64  `json:"used_bytes"        jsonschema:"the space only it holds, which deleting it would free, in bytes"`
	ReferencedBytes int64  `json:"referenced_bytes"  jsonschema:"how much data it holds, shared or not, in bytes: what the dataset held when it was taken"`
	Held            bool   `json:"held,omitempty"    jsonschema:"whether something holds it, which keeps it from being deleted"`
}

func registerSnapshotTools(r *registry) {
	type snapshotListIn struct {
		Dataset string `json:"dataset" jsonschema:"the dataset whose snapshots to list, by its whole name (tank/media); those of the datasets under it are not listed"`
		pageIn
	}
	type snapshotListOut struct {
		Snapshots []snapshotRow `json:"snapshots"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "snapshot_list",
		Description: "List one dataset's snapshots by name, with when each was taken, the space only it holds and how much data it holds. Snapshots of the datasets under it are not listed: list each by its own name.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotListIn) (*mcp.CallToolResult, snapshotListOut, error) {
		if in.Dataset == "" {
			return nil, snapshotListOut{}, errors.New("say which dataset's snapshots to list: dataset_list names them")
		}
		if err := datasetNamed(ctx, r, in.Dataset); err != nil {
			return nil, snapshotListOut{}, err
		}

		// counted and named one dataset at a time, which a NAS does without reading every snapshot it has; asked for anything more, it reads them all
		of := []any{[]any{"dataset", "=", in.Dataset}}
		counted, err := r.typed.PoolSnapshot.QueryCount(ctx, of, truenas.PoolSnapshotQueryOptions{})
		if err != nil {
			return nil, snapshotListOut{}, fmt.Errorf("counting the snapshots of %s: %w", in.Dataset, err)
		}
		total := pointer.From(counted.Answer)
		limit, offset := in.args()
		names, err := r.typed.PoolSnapshot.Query(ctx, of, truenas.PoolSnapshotQueryOptions{Select: []any{"name"}, OrderBy: []string{"name"}, Limit: limit, Offset: offset})
		if err != nil {
			return nil, snapshotListOut{}, fmt.Errorf("listing the snapshots of %s: %w", in.Dataset, err)
		}

		// and each is then asked after by its name, which a NAS answers from that snapshot alone
		rows := make([]snapshotRow, len(names.Answer))
		failed := make([]error, len(names.Answer))
		var wg sync.WaitGroup
		at := make(chan struct{}, snapshotsAtOnce)
		for i, named := range names.Answer {
			wg.Go(func() {
				at <- struct{}{}
				defer func() { <-at }()

				rows[i], failed[i] = snapshotOf(ctx, r, named.Name)
			})
		}
		wg.Wait()
		for _, err := range failed {
			if err != nil {
				return nil, snapshotListOut{}, err
			}
		}

		return nil, snapshotListOut{Snapshots: rows, pageOut: paged(offset, len(rows), total)}, nil
	})
}

// snapshotProperties are the properties of a snapshot a list answers, the only ones the NAS is asked to read.
var snapshotProperties = []string{"creation", "used", "referenced", "userrefs"}

// snapshotFound reads one snapshot by its whole name, which a NAS answers from that snapshot alone: nil when it has none by that name.
func snapshotFound(ctx context.Context, r *registry, name string) (*truenas.PoolSnapshotQueryResultItem, error) {
	found, err := r.typed.PoolSnapshot.Query(ctx, []any{[]any{"id", "=", name}}, truenas.PoolSnapshotQueryOptions{Extra: map[string]any{"properties": snapshotProperties}})
	switch {
	case truenasrpc.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the snapshot %s: %w", name, err)
	case len(found.Answer) == 0:
		return nil, nil
	}

	return &found.Answer[0], nil
}

// snapshotOf is one snapshot as a list answers it, asked after by its whole name.
func snapshotOf(ctx context.Context, r *registry, name string) (snapshotRow, error) {
	s, err := snapshotFound(ctx, r, name)
	switch {
	case err != nil:
		return snapshotRow{}, err
	case s == nil:
		return snapshotRow{}, fmt.Errorf("the snapshot %s went while it was being listed: list again", name)
	}

	return snapshotRowOf(s), nil
}

// snapshotRowOf is a snapshot as a list answers it.
func snapshotRowOf(s *truenas.PoolSnapshotQueryResultItem) snapshotRow {
	raw := func(property string) string { return s.Properties[property].Rawvalue }
	number := func(property string) int64 {
		n, _ := strconv.ParseInt(raw(property), 10, 64)

		return n
	}

	row := snapshotRow{Name: s.Name, Dataset: s.Dataset, Snapshot: s.SnapshotName, UsedBytes: number("used"), ReferencedBytes: number("referenced"), Held: number("userrefs") > 0}
	if created := number("creation"); created > 0 {
		row.Created = when(time.Unix(created, 0))
	}

	return row
}

// snapshotTaskRow is a snapshot task as a list answers it.
type snapshotTaskRow struct {
	ID            int       `json:"id"                       jsonschema:"the task's number"`
	Dataset       string    `json:"dataset"                  jsonschema:"the dataset it takes snapshots of"`
	Recursive     bool      `json:"recursive"                jsonschema:"whether it takes the datasets under it too"`
	NamingSchema  string    `json:"naming_schema"            jsonschema:"how its snapshots are named, in strftime terms: auto-%Y-%m-%d_%H-%M"`
	Schedule      string    `json:"schedule"                 jsonschema:"when it runs, as cron: minute hour day-of-month month day-of-week"`
	KeepFor       string    `json:"keep_for"                 jsonschema:"how long its snapshots are kept before it deletes them: 2 WEEK"`
	Enabled       bool      `json:"enabled"                  jsonschema:"whether it runs; a task switched off takes no snapshots"`
	State         *standing `json:"state,omitempty"          jsonschema:"where it stands, as the NAS says"`
	LastSnapshot  string    `json:"last_snapshot,omitempty"  jsonschema:"the last snapshot it took"`
	Window        string    `json:"window,omitempty"         jsonschema:"the times of day it takes snapshots between, as 09:00-18:00; none when it takes them at any hour"`
	Exclude       []string  `json:"exclude,omitempty"        jsonschema:"with recursive, the datasets under it leaves out"`
	SkipUnchanged bool      `json:"skip_unchanged,omitempty" jsonschema:"whether it takes no snapshot of a dataset unchanged since the last"`
}

// snapshotTaskRowOf is a snapshot task as a list answers it.
func snapshotTaskRowOf(t *truenas.PoolSnapshotTaskQueryResultItem) snapshotTaskRow {
	row := snapshotTaskRow{ID: t.Id, Dataset: t.Dataset, Recursive: t.Recursive, NamingSchema: t.NamingSchema, KeepFor: fmt.Sprintf("%d %s", t.LifetimeValue, t.LifetimeUnit), Enabled: t.Enabled, State: standingOf(t.State)}
	if s := t.Schedule; s != nil {
		row.Schedule = cron(s.Minute, s.Hour, s.Dom, s.Month, s.Dow)
		row.Window = windowOf(s.Begin, s.End)
	}
	if t.State != nil {
		row.LastSnapshot = t.State.LastSnapshot
	}
	row.Exclude, row.SkipUnchanged = t.Exclude, !t.AllowEmpty

	return row
}

func registerSnapshotTaskTools(r *registry) {
	type snapshotTaskListIn struct {
		pageIn
	}
	type snapshotTaskListOut struct {
		Tasks []snapshotTaskRow `json:"snapshot_tasks"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "snapshottask_list",
		Description: "List the tasks that take snapshots on a schedule: the dataset each takes, when it runs, how long it keeps them, whether it is switched on, and how its last run went.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotTaskListIn) (*mcp.CallToolResult, snapshotTaskListOut, error) {
		found, err := r.typed.PoolSnapshottask.Query(ctx, nil, truenas.PoolSnapshottaskQueryOptions{OrderBy: []string{"id"}})
		if err != nil {
			return nil, snapshotTaskListOut{}, fmt.Errorf("listing the snapshot tasks: %w", err)
		}

		rows := make([]snapshotTaskRow, 0, len(found.Answer))
		for i := range found.Answer {
			rows = append(rows, snapshotTaskRowOf(&found.Answer[i]))
		}
		out := snapshotTaskListOut{}
		out.Tasks, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}

func registerReplicationTools(r *registry) {
	type replicationRow struct {
		ID             int       `json:"id"                 jsonschema:"the replication's number"`
		Name           string    `json:"name"               jsonschema:"its name"`
		Direction      string    `json:"direction"          jsonschema:"PUSH, from this NAS to another, or PULL, from another to this one"`
		Transport      string    `json:"transport"          jsonschema:"how it reaches the other end: SSH, SSH+NETCAT, or LOCAL for a copy on this NAS"`
		SourceDatasets []string  `json:"source_datasets"    jsonschema:"the datasets it copies"`
		TargetDataset  string    `json:"target_dataset"     jsonschema:"the dataset it copies them into"`
		Recursive      bool      `json:"recursive"          jsonschema:"whether it copies the datasets under them too"`
		Auto           bool      `json:"auto"               jsonschema:"whether it runs by itself, on its schedule or after the snapshot tasks it follows; otherwise only when run"`
		Schedule       string    `json:"schedule,omitempty" jsonschema:"when it runs, as cron, for one that does not follow snapshot tasks"`
		Enabled        bool      `json:"enabled"            jsonschema:"whether it runs at all"`
		State          *standing `json:"state,omitempty"    jsonschema:"where it stands, as the NAS says"`
	}
	type replicationListIn struct {
		pageIn
	}
	type replicationListOut struct {
		Replications []replicationRow `json:"replications"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "replication_list",
		Description: "List the replications, which copy datasets' snapshots to another dataset, here or on another NAS: what each copies where, how, whether it runs by itself, and how its last run went.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in replicationListIn) (*mcp.CallToolResult, replicationListOut, error) {
		// asked for what is answered and no more: a replication holds the key it encrypts with
		found, err := r.typed.Replication.Query(ctx, nil, truenas.ReplicationQueryOptions{Select: []any{"id", "name", "direction", "transport", "source_datasets", "target_dataset", "recursive", "auto", "schedule", "enabled", "state"}, OrderBy: []string{"id"}})
		if err != nil {
			return nil, replicationListOut{}, fmt.Errorf("listing the replications: %w", err)
		}

		rows := make([]replicationRow, 0, len(found.Answer))
		for i := range found.Answer {
			t := &found.Answer[i]
			row := replicationRow{ID: t.Id, Name: t.Name, Direction: string(t.Direction), Transport: string(t.Transport), SourceDatasets: t.SourceDatasets, TargetDataset: t.TargetDataset, Recursive: t.Recursive, Auto: t.Auto, Enabled: t.Enabled, State: standingOf(t.State)}
			if s := t.Schedule; s != nil {
				row.Schedule = cron(s.Minute, s.Hour, s.Dom, s.Month, s.Dow)
			}
			rows = append(rows, row)
		}
		out := replicationListOut{}
		out.Replications, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}

// jobFields are what is asked of a task's last run.
var jobFields = []any{"job.state", "job.time_finished", "job.error"}

func registerCopyTaskTools(r *registry) {
	type rsyncTaskRow struct {
		ID          int      `json:"id"                    jsonschema:"the task's number"`
		Description string   `json:"description,omitempty" jsonschema:"what it is for, in its owner's words"`
		Path        string   `json:"path"                  jsonschema:"the directory on this NAS it copies from or into"`
		Direction   string   `json:"direction"             jsonschema:"PUSH, from this NAS, or PULL, into it"`
		Mode        string   `json:"mode"                  jsonschema:"MODULE, to an rsync server's module, or SSH"`
		Remote      string   `json:"remote"                jsonschema:"where the other end is: host and module, or host and path"`
		User        string   `json:"user"                  jsonschema:"the user it runs as on this NAS"`
		Schedule    string   `json:"schedule"              jsonschema:"when it runs, as cron"`
		Enabled     bool     `json:"enabled"               jsonschema:"whether it runs"`
		Locked      bool     `json:"locked,omitempty"      jsonschema:"whether its directory is on a locked dataset, so it copies nothing"`
		LastRun     *lastRun `json:"last_run,omitempty"    jsonschema:"how its last run went; none if it has not run"`
	}
	type rsyncTaskListIn struct {
		pageIn
	}
	type rsyncTaskListOut struct {
		Tasks []rsyncTaskRow `json:"rsync_tasks"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "rsynctask_list",
		Description: "List the rsync tasks, which copy a directory to or from another machine on a schedule: where each copies, when, whether it is switched on, and how its last run went.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in rsyncTaskListIn) (*mcp.CallToolResult, rsyncTaskListOut, error) {
		found, err := r.typed.Rsynctask.Query(ctx, nil, truenas.RsynctaskQueryOptions{Select: append([]any{"id", "desc", "path", "direction", "mode", "remotehost", "remotemodule", "remotepath", "user", "schedule", "enabled", "locked"}, jobFields...), OrderBy: []string{"id"}})
		if err != nil {
			return nil, rsyncTaskListOut{}, fmt.Errorf("listing the rsync tasks: %w", err)
		}

		rows := make([]rsyncTaskRow, 0, len(found.Answer))
		for _, t := range found.Answer {
			remote := t.Remotehost + "::" + t.Remotemodule
			if t.Mode == "SSH" {
				remote = t.Remotehost + ":" + t.Remotepath
			}
			rows = append(rows, rsyncTaskRow{ID: t.Id, Description: t.Desc, Path: t.Path, Direction: string(t.Direction), Mode: string(t.Mode), Remote: remote, User: t.User, Schedule: cronOf(t.Schedule), Enabled: t.Enabled, Locked: t.Locked, LastRun: runOf(t.Job)})
		}
		out := rsyncTaskListOut{}
		out.Tasks, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})

	type cloudSyncRow struct {
		ID           int      `json:"id"                    jsonschema:"the task's number"`
		Description  string   `json:"description,omitempty" jsonschema:"what it is for, in its owner's words"`
		Path         string   `json:"path"                  jsonschema:"the directory on this NAS it copies from or into"`
		Direction    string   `json:"direction"             jsonschema:"PUSH, to the cloud, or PULL, from it"`
		TransferMode string   `json:"transfer_mode"         jsonschema:"SYNC, which makes the other side the same and deletes what is not there, COPY, which only adds, or MOVE, which deletes the source after"`
		Credential   string   `json:"credential"            jsonschema:"the cloud account it uses, by name"`
		Schedule     string   `json:"schedule"              jsonschema:"when it runs, as cron"`
		Enabled      bool     `json:"enabled"               jsonschema:"whether it runs"`
		Locked       bool     `json:"locked,omitempty"      jsonschema:"whether its directory is on a locked dataset, so it copies nothing"`
		LastRun      *lastRun `json:"last_run,omitempty"    jsonschema:"how its last run went; none if it has not run"`
	}
	type cloudSyncListIn struct {
		pageIn
	}
	type cloudSyncListOut struct {
		Tasks []cloudSyncRow `json:"cloud_syncs"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "cloudsync_list",
		Description: "List the cloud sync tasks, which copy a directory to or from a cloud account on a schedule: where each copies, how, when, and how its last run went.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cloudSyncListIn) (*mcp.CallToolResult, cloudSyncListOut, error) {
		// asked for the account's name alone: the rest of it is the account's keys
		found, err := r.typed.Cloudsync.Query(ctx, nil, truenas.CloudsyncQueryOptions{Select: append([]any{"id", "description", "path", "direction", "transfer_mode", "credentials.name", "schedule", "enabled", "locked"}, jobFields...), OrderBy: []string{"id"}})
		if err != nil {
			return nil, cloudSyncListOut{}, fmt.Errorf("listing the cloud sync tasks: %w", err)
		}

		rows := make([]cloudSyncRow, 0, len(found.Answer))
		for _, t := range found.Answer {
			row := cloudSyncRow{ID: t.Id, Description: t.Description, Path: t.Path, Direction: string(t.Direction), TransferMode: string(t.TransferMode), Schedule: cronOf(t.Schedule), Enabled: t.Enabled, Locked: t.Locked, LastRun: runOf(t.Job)}
			if t.Credentials != nil {
				row.Credential = t.Credentials.Name
			}
			rows = append(rows, row)
		}
		out := cloudSyncListOut{}
		out.Tasks, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})

	type cloudBackupRow struct {
		ID          int      `json:"id"                    jsonschema:"the task's number"`
		Description string   `json:"description,omitempty" jsonschema:"what it is for, in its owner's words"`
		Path        string   `json:"path"                  jsonschema:"the directory on this NAS it backs up"`
		Credential  string   `json:"credential"            jsonschema:"the cloud account it backs up to, by name"`
		Schedule    string   `json:"schedule"              jsonschema:"when it runs, as cron"`
		KeepLast    int      `json:"keep_last"             jsonschema:"how many backups it keeps before deleting the oldest"`
		Enabled     bool     `json:"enabled"               jsonschema:"whether it runs"`
		Locked      bool     `json:"locked,omitempty"      jsonschema:"whether its directory is on a locked dataset, so it backs up nothing"`
		LastRun     *lastRun `json:"last_run,omitempty"    jsonschema:"how its last run went; none if it has not run"`
	}
	type cloudBackupListIn struct {
		pageIn
	}
	type cloudBackupListOut struct {
		Tasks []cloudBackupRow `json:"cloud_backups"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "cloud_backup_list",
		Description: "List the cloud backup tasks, which back a directory up to a cloud account with restic on a schedule: what each backs up where, when, how many backups it keeps, and how its last run went.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cloudBackupListIn) (*mcp.CallToolResult, cloudBackupListOut, error) {
		// asked for the account's name alone, and never the password the backups are encrypted with
		found, err := r.typed.CloudBackup.Query(ctx, nil, truenas.CloudBackupQueryOptions{Select: append([]any{"id", "description", "path", "credentials.name", "schedule", "keep_last", "enabled", "locked"}, jobFields...), OrderBy: []string{"id"}})
		if err != nil {
			return nil, cloudBackupListOut{}, fmt.Errorf("listing the cloud backup tasks: %w", err)
		}

		rows := make([]cloudBackupRow, 0, len(found.Answer))
		for _, t := range found.Answer {
			row := cloudBackupRow{ID: t.Id, Description: t.Description, Path: t.Path, Schedule: cronOf(t.Schedule), KeepLast: t.KeepLast, Enabled: t.Enabled, Locked: t.Locked, LastRun: runOf(t.Job)}
			if t.Credentials != nil {
				row.Credential = t.Credentials.Name
			}
			rows = append(rows, row)
		}
		out := cloudBackupListOut{}
		out.Tasks, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}

// scrubRow is a scrub task as a list answers it.
type scrubRow struct {
	ID            int    `json:"id"                    jsonschema:"the scrub task's number"`
	Pool          string `json:"pool"                  jsonschema:"the pool it scrubs"`
	Schedule      string `json:"schedule"              jsonschema:"when it may start, as cron"`
	ThresholdDays int    `json:"threshold_days"        jsonschema:"how many days must pass since the last scrub before a scheduled one runs"`
	Enabled       bool   `json:"enabled"               jsonschema:"whether it runs"`
	Description   string `json:"description,omitempty" jsonschema:"what it is for, in its owner's words"`
}

// scrubRowOf is a scrub task as a list answers it.
func scrubRowOf(t *truenas.PoolScrubQueryResultItem) scrubRow {
	return scrubRow{ID: t.Id, Pool: t.PoolName, Schedule: cronOf(t.Schedule), ThresholdDays: t.Threshold, Enabled: t.Enabled, Description: t.Description}
}

func registerScrubTools(r *registry) {
	type scrubListIn struct {
		pageIn
	}
	type scrubListOut struct {
		Scrubs []scrubRow `json:"scrubs"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "pool_scrub_list",
		Description: "List the scrub tasks, which read a pool's every block on a schedule to find damage before it is needed: the pool each scrubs, when, and how long it waits between scrubs. pool_get says how the last scrub went.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in scrubListIn) (*mcp.CallToolResult, scrubListOut, error) {
		found, err := r.typed.PoolScrub.Query(ctx, nil, truenas.PoolScrubQueryOptions{OrderBy: []string{"id"}})
		if err != nil {
			return nil, scrubListOut{}, fmt.Errorf("listing the scrub tasks: %w", err)
		}

		rows := make([]scrubRow, 0, len(found.Answer))
		for _, t := range found.Answer {
			rows = append(rows, scrubRowOf(&t))
		}
		out := scrubListOut{}
		out.Scrubs, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}
