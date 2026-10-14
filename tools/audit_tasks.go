package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// taskAudit looks at the tasks that protect data, snapshots, replications, copies and cloud syncs and backups, for ones that failed, are held or
// cannot run.
type taskAudit struct{}

func (taskAudit) run(ctx context.Context, r *registry) (*findings, error) {
	snapshots, err := r.typed.PoolSnapshottask.Query(ctx, nil, truenas.PoolSnapshottaskQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the snapshot tasks: %w", err)
	}
	replications, err := r.typed.Replication.Query(ctx, nil, truenas.ReplicationQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the replications: %w", err)
	}
	copies, err := r.typed.Rsynctask.Query(ctx, nil, truenas.RsynctaskQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the copy tasks: %w", err)
	}
	syncs, err := r.typed.Cloudsync.Query(ctx, nil, truenas.CloudsyncQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the cloud syncs: %w", err)
	}
	backups, err := r.typed.CloudBackup.Query(ctx, nil, truenas.CloudBackupQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the cloud backups: %w", err)
	}

	f := &findings{}
	for _, t := range snapshots.Answer {
		stood(f, "snapshot task", fmt.Sprintf("%s (%d)", t.Dataset, t.Id), poolOf(t.Dataset), t.Enabled, t.State)
	}
	for i := range replications.Answer {
		t := &replications.Answer[i]
		pool := ""
		if len(t.SourceDatasets) > 0 {
			pool = poolOf(t.SourceDatasets[0])
		}
		stood(f, "replication", fmt.Sprintf("%s (%d)", t.Name, t.Id), pool, t.Enabled, t.State)
	}
	for _, t := range copies.Answer {
		ran(f, "copy task", fmt.Sprintf("%s (%d)", orNothing(t.Desc, t.Path), t.Id), poolOf(strings.TrimPrefix(t.Path, "/mnt/")), t.Enabled, t.Locked, t.Job)
	}
	for _, t := range syncs.Answer {
		ran(f, "cloud sync", fmt.Sprintf("%s (%d)", orNothing(t.Description, t.Path), t.Id), poolOf(strings.TrimPrefix(t.Path, "/mnt/")), t.Enabled, t.Locked, t.Job)
	}
	for _, t := range backups.Answer {
		ran(f, "cloud backup", fmt.Sprintf("%s (%d)", orNothing(t.Description, t.Path), t.Id), poolOf(strings.TrimPrefix(t.Path, "/mnt/")), t.Enabled, t.Locked, t.Job)
	}

	return f, nil
}

// checkName is a kind of task as part of a check's one word: "cloud sync" is cloud_sync.
func checkName(kind string) string { return strings.ReplaceAll(kind, " ", "_") }

// stood is what is wrong with a snapshot task or a replication, by where the replication engine says it stands.
func stood(f *findings, kind, name, pool string, enabled bool, state *truenas.TaskState) {
	f.checked++
	if !enabled {
		f.add(notice, checkName(kind)+"_off", "task", name, pool, fmt.Sprintf("the %s %s is switched off, so it does not run", kind, name))
	}

	switch {
	case state == nil:
	case state.State == "ERROR":
		f.add(critical, checkName(kind)+"_failed", "task", name, pool, fmt.Sprintf("the %s %s failed%s: %s", kind, name, at(state.Datetime.Time), orNothing(state.Error, "the NAS gave no reason")))
	case state.State == "HOLD":
		f.add(warning, checkName(kind)+"_held", "task", name, pool, fmt.Sprintf("the %s %s is held and does not run: %s", kind, name, orNothing(state.Reason, "the NAS gave no reason")))
	}
}

// ran is what is wrong with a copy task, a cloud sync or a cloud backup, by how its last run ended and whether what it copies can be read.
func ran(f *findings, kind, name, pool string, enabled, locked bool, job *truenas.CoreGetJobsItemQueryResultItem) {
	f.checked++
	if !enabled {
		f.add(notice, checkName(kind)+"_off", "task", name, pool, fmt.Sprintf("the %s %s is switched off, so it does not run", kind, name))
	}

	switch {
	case locked:
		// a copy task on a locked dataset ends SUCCESS, having copied nothing, so this is the only place it shows
		f.add(warning, "task_on_locked_dataset", "task", name, pool, fmt.Sprintf("the %s %s copies a dataset that is locked, so it copies nothing until the dataset is unlocked", kind, name))
	case job == nil:
	case job.State == "FAILED" || job.State == "ABORTED":
		f.add(critical, checkName(kind)+"_failed", "task", name, pool, fmt.Sprintf("the last run of the %s %s %s%s: %s", kind, name, strings.ToLower(job.State), at(job.TimeFinished.Time), orNothing(job.Error, "the NAS gave no reason")))
	}
}

// poolOf is the pool a dataset is on, its name before the first slash.
func poolOf(dataset string) string {
	pool, _, _ := strings.Cut(dataset, "/")

	return pool
}

func registerTaskAudits(r *registry) {
	add(r, readTool, &mcp.Tool{
		Name:        "audit_tasks",
		Description: "Find the tasks that protect data and are not doing it: a snapshot task, replication, copy (rsync) task, cloud sync or cloud backup whose last run failed, one held, one copying a locked dataset, which a copy task reports as success, and one switched off.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in auditIn) (*mcp.CallToolResult, auditOut, error) {
		f, err := taskAudit{}.run(ctx, r)
		if err != nil {
			return nil, auditOut{}, err
		}

		return nil, f.answer(in.pageIn), nil
	})
}
