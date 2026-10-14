package main

import "github.com/katbyte/pandorest/openapi"

// A snapshot task's and a replication's state, and the last run of a replication, a copy task, a cloud sync and a cloud backup, are described as
// values of no shape, and a NAS writes each the same way every time: the state as zettarepl keeps it (plugins/zettarepl_/state.py, zettarepl.py),
// and the run as the job record core.get_jobs lists.

// taskShapes are the records declared for them.
var taskShapes = map[string]*openapi.Schema{
	"TaskState": record("Where a snapshot task or a replication stands, as the NAS's replication engine keeps it.", map[string]*openapi.Schema{
		"state":         said(text(), "PENDING before it first runs, RUNNING, WAITING, FINISHED, ERROR, or HOLD while something stops it running at all"),
		"datetime":      said(moment(), "when it came to stand so"),
		"error":         said(text(), "what went wrong, when it is in ERROR"),
		"reason":        said(text(), "why it is held or waiting"),
		"warnings":      said(listOf(text()), "what it warned of while it ran"),
		"last_snapshot": said(text(), "the newest snapshot it made or sent"),
		"progress":      said(&openapi.Schema{}, "how far a run has got, while it runs"),
	}),
}

// jobRecord is the job record core.get_jobs lists, which a task's last run is written as.
const jobRecord = "CoreGetJobsItemQueryResultItem"

// taskFields are the places that leave them open, and the shape each takes.
var taskFields = map[string]*openapi.Schema{
	"PoolSnapshotTaskQueryResultItem.state": ref("TaskState"),
	"ReplicationQueryResultItem.state":      ref("TaskState"),
	"ReplicationGetInstanceAnswer.state":    ref("TaskState"),
	"ReplicationCreateAnswer.state":         ref("TaskState"),
	"ReplicationUpdateAnswer.state":         ref("TaskState"),
	"ReplicationRestoreAnswer.state":        ref("TaskState"),
	"ReplicationQueryResultItem.job":        ref(jobRecord),
	"ReplicationGetInstanceAnswer.job":      ref(jobRecord),
	"ReplicationCreateAnswer.job":           ref(jobRecord),
	"ReplicationUpdateAnswer.job":           ref(jobRecord),
	"ReplicationRestoreAnswer.job":          ref(jobRecord),
	"RsyncTaskQueryResultItem.job":          ref(jobRecord),
	"CloudSyncQueryResultItem.job":          ref(jobRecord),
	"CloudBackupQueryResultItem.job":        ref(jobRecord),
}
