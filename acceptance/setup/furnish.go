package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// named is the filter that finds what goes by a name, under whatever a kind of thing calls its name.
func named(field, name string) []any {
	return []any{[]any{[]any{field, "=", name}}}
}

func CreateSMBUser(password string) Call {
	return Call{Method: "user.create", Params: []any{map[string]any{"username": layout.SMBUser, "full_name": "A user who may use SMB", "password": password, "group_create": true, "smb": true}}}
}

func FindSMBShare() Call {
	return Call{Method: "sharing.smb.query", Params: named("name", layout.SMBShare)}
}

func CreateSMBShare() Call {
	return Call{Method: "sharing.smb.create", Params: []any{map[string]any{"name": layout.SMBShare, "path": "/mnt/" + layout.SMBDataset}}}
}

func FindNFSShare() Call {
	return Call{Method: "sharing.nfs.query", Params: named("path", "/mnt/"+layout.NFSDataset)}
}

func CreateNFSShare() Call {
	return Call{Method: "sharing.nfs.create", Params: []any{map[string]any{"path": "/mnt/" + layout.NFSDataset}}}
}

func FindTarget() Call {
	return Call{Method: "iscsi.target.query", Params: named("name", layout.Target)}
}

func CreateTarget() Call {
	return Call{Method: "iscsi.target.create", Params: []any{map[string]any{"name": layout.Target}}}
}

func FindMachine() Call { return Call{Method: "vm.query", Params: named("name", layout.Machine)} }

func CreateMachine() Call {
	return Call{Method: "vm.create", Params: []any{map[string]any{"name": layout.Machine, "memory": 256, "autostart": false}}}
}

func FindSnapshotTask() Call {
	return Call{Method: "pool.snapshottask.query", Params: []any{[]any{[]any{"dataset", "=", layout.TaskDataset}, []any{"naming_schema", "=", layout.SnapshotSchema}}}}
}

func CreateSnapshotTask() Call {
	return Call{Method: "pool.snapshottask.create", Params: []any{map[string]any{"dataset": layout.TaskDataset, "naming_schema": layout.SnapshotSchema, "lifetime_value": 1, "lifetime_unit": "WEEK"}}}
}

func RunSnapshotTask(id int64) Call {
	return Call{Method: "pool.snapshottask.run", Params: []any{id}}
}

func FindReplication() Call {
	return Call{Method: "replication.query", Params: named("name", layout.Replication)}
}

func CreateReplication() Call {
	return Call{Method: "replication.create", Params: []any{map[string]any{"name": layout.Replication, "direction": "PUSH", "transport": "LOCAL", "source_datasets": []string{layout.TaskDataset}, "target_dataset": layout.ReplicationTarget, "recursive": false, "auto": false, "retention_policy": "NONE", "also_include_naming_schema": []string{layout.SnapshotSchema}}}}
}

func FindCopyTask() Call {
	return Call{Method: "rsynctask.query", Params: named("desc", layout.CopyTask)}
}

func CreateCopyTask() Call {
	return Call{Method: "rsynctask.create", Params: []any{map[string]any{"path": "/mnt/" + layout.TaskDataset, "user": "root", "mode": "MODULE", "remotehost": "127.0.0.1", "remotemodule": "nothing-listens-here", "direction": "PUSH", "desc": layout.CopyTask, "enabled": false}}}
}

func RunCopyTask(id int64) Call {
	return Call{Method: "rsynctask.run", Job: true, Params: []any{id}}
}

// listed is a row of any list, as far as furnishing a NAS reads one: its number, and how a task says its last run went.
type listed struct {
	ID    int64 `json:"id"`
	State struct {
		State string `json:"state"`
	} `json:"state"`
	Job json.RawMessage `json:"job"`
}

// one finds what a kind of thing has by the furnished NAS's name for it, and makes it if there is none.
func (n *NAS) one(ctx context.Context, find, create Call) (listed, error) {
	var rows []listed
	if err := n.run(ctx, find, &rows, nil); err != nil {
		return listed{}, err
	}
	if len(rows) > 0 {
		return rows[0], nil
	}

	n.logf("making it: %s", create.Method)
	if err := n.run(ctx, create, nil, nil); err != nil {
		return listed{}, err
	}
	if err := n.run(ctx, find, &rows, nil); err != nil {
		return listed{}, err
	}
	if len(rows) != 1 {
		return listed{}, fmt.Errorf("%s was called, and %s then finds %d of what it made", create.Method, find.Method, len(rows))
	}

	return rows[0], nil
}

// Furnish gives a NAS that has been set up one of each thing a NAS is watched for, as acceptance/layout lists them, making only what is
// missing: run again it changes nothing. It is for whatever wants more to look at than a pool and two datasets, the tests of what an
// exporter reads above all.
func (n *NAS) Furnish(ctx context.Context) error {
	// a NAS shares nothing over SMB until it has a user who may use it, who is never logged in as, so nobody is told the password
	var users []json.RawMessage
	if err := n.run(ctx, FindUser(layout.SMBUser), &users, nil); err != nil {
		return err
	}
	if len(users) == 0 {
		password := make([]byte, 16)
		if _, err := rand.Read(password); err != nil {
			return err
		}
		if err := n.run(ctx, CreateSMBUser("A1-"+hex.EncodeToString(password)), nil, nil); err != nil {
			return err
		}
	}

	shared := []string{layout.SMBDataset, layout.NFSDataset}
	var have []struct {
		ID string `json:"id"`
	}
	if err := n.run(ctx, FindDatasets(shared), &have, nil); err != nil {
		return err
	}
	for _, name := range shared {
		if slices.ContainsFunc(have, func(d struct {
			ID string `json:"id"`
		},
		) bool {
			return d.ID == name
		}) {
			continue
		}
		if err := n.run(ctx, CreateDataset(name), nil, nil); err != nil {
			return err
		}
	}

	for _, kind := range []struct{ find, create Call }{
		{FindSMBShare(), CreateSMBShare()}, {FindNFSShare(), CreateNFSShare()}, {FindTarget(), CreateTarget()}, {FindMachine(), CreateMachine()}, {FindReplication(), CreateReplication()},
	} {
		if _, err := n.one(ctx, kind.find, kind.create); err != nil {
			return err
		}
	}

	// the snapshot task is run once, so that it has a last run and the run went well
	snapshots, err := n.one(ctx, FindSnapshotTask(), CreateSnapshotTask())
	if err != nil {
		return err
	}
	if snapshots.State.State == "PENDING" {
		if err := n.run(ctx, RunSnapshotTask(snapshots.ID), nil, nil); err != nil {
			return err
		}
		if err := n.settled(ctx, FindSnapshotTask()); err != nil {
			return err
		}
	}

	// and the copy task once, which fails, since nothing listens where it copies to: that it failed is the point
	copying, err := n.one(ctx, FindCopyTask(), CreateCopyTask())
	if err != nil {
		return err
	}
	if len(copying.Job) == 0 || string(copying.Job) == "null" {
		err := n.run(ctx, RunCopyTask(copying.ID), nil, nil)
		if _, refused := errors.AsType[*truenasrpc.Error](err); err != nil && !refused {
			return err
		}
		if err == nil {
			return errors.New("a copy to a place that is not there ran without failing, and a furnished NAS is to have a task that failed")
		}
	}

	return nil
}

// settled waits for a task that was just run to say how the run went.
func (n *NAS) settled(ctx context.Context, find Call) error {
	for try := 0; ; try++ {
		var rows []listed
		if err := n.run(ctx, find, &rows, nil); err != nil {
			return err
		}
		if len(rows) == 1 && rows[0].State.State != "PENDING" && rows[0].State.State != "RUNNING" {
			if rows[0].State.State != "FINISHED" {
				return fmt.Errorf("%s: the task was run and says %s", find.Method, rows[0].State.State)
			}

			return nil
		}
		if try >= 60 {
			return fmt.Errorf("%s: the task was run and never said how the run went", find.Method)
		}

		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(2 * time.Second):
		}
	}
}
