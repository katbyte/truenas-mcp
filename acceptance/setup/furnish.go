package setup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/katbyte/go-kt/pointer"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenas/client"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// named is the filter that finds what goes by a name, under whatever a kind of thing calls its name.
func named(field, name string) []any {
	return []any{[]any{field, "=", name}}
}

// kind is one kind of thing a furnished NAS has: how many of it the NAS has by the furnished NAS's name for it, and how to make it.
type kind struct {
	what   string
	count  func(ctx context.Context, api *client.Client) (int, error)
	create func(ctx context.Context, api *client.Client) error
}

// kinds are the things a furnished NAS has one of, but for the tasks, which are run once after they are made.
var kinds = []kind{
	{
		what: "the SMB share",
		count: func(ctx context.Context, api *client.Client) (int, error) {
			found, err := api.SharingSmb.Query(ctx, named("name", layout.SMBShare), truenas.SharingSmbQueryOptions{})
			return len(found.Answer), err
		},
		create: func(ctx context.Context, api *client.Client) error {
			_, err := api.SharingSmb.Create(ctx, truenas.SharingSmbCreateData{Name: layout.SMBShare, Path: "/mnt/" + layout.SMBDataset})
			return err
		},
	},
	{
		what: "the NFS share",
		count: func(ctx context.Context, api *client.Client) (int, error) {
			found, err := api.SharingNfs.Query(ctx, named("path", "/mnt/"+layout.NFSDataset), truenas.SharingNfsQueryOptions{})
			return len(found.Answer), err
		},
		create: func(ctx context.Context, api *client.Client) error {
			_, err := api.SharingNfs.Create(ctx, truenas.SharingNfsCreateData{Path: "/mnt/" + layout.NFSDataset})
			return err
		},
	},
	{
		what: "the iSCSI target",
		count: func(ctx context.Context, api *client.Client) (int, error) {
			found, err := api.IscsiTarget.Query(ctx, named("name", layout.Target), truenas.IscsiTargetQueryOptions{})
			return len(found.Answer), err
		},
		create: func(ctx context.Context, api *client.Client) error {
			_, err := api.IscsiTarget.Create(ctx, truenas.IscsiTargetCreate{Name: layout.Target})
			return err
		},
	},
	{
		what: "the virtual machine",
		count: func(ctx context.Context, api *client.Client) (int, error) {
			found, err := api.Vm.Query(ctx, named("name", layout.Machine), truenas.VmQueryOptions{})
			return len(found.Answer), err
		},
		create: func(ctx context.Context, api *client.Client) error {
			_, err := api.Vm.Create(ctx, truenas.VmCreate{Name: layout.Machine, Memory: 256, Autostart: pointer.To(false)})
			return err
		},
	},
	{
		what: "the replication",
		count: func(ctx context.Context, api *client.Client) (int, error) {
			found, err := api.Replication.Query(ctx, named("name", layout.Replication), truenas.ReplicationQueryOptions{})
			return len(found.Answer), err
		},
		create: func(ctx context.Context, api *client.Client) error {
			_, err := api.Replication.Create(ctx, truenas.ReplicationCreate{Name: layout.Replication, Direction: truenas.ReplicationDirectionPUSH, Transport: truenas.ReplicationTransportLOCAL, SourceDatasets: []string{layout.TaskDataset}, TargetDataset: layout.ReplicationTarget, Recursive: pointer.To(false), Auto: pointer.To(false), RetentionPolicy: truenas.ReplicationRetentionPolicyNONE, AlsoIncludeNamingSchema: []string{layout.SnapshotSchema}})
			return err
		},
	},
}

// snapshotTasks is the furnished NAS's snapshot task, as the NAS has it: none, or the one.
func snapshotTasks(ctx context.Context, api *client.Client) ([]truenas.PoolSnapshotTaskQueryResultItem, error) {
	found, err := api.PoolSnapshottask.Query(ctx, []any{[]any{"dataset", "=", layout.TaskDataset}, []any{"naming_schema", "=", layout.SnapshotSchema}}, truenas.PoolSnapshottaskQueryOptions{})
	return found.Answer, err
}

// copyTasks is the furnished NAS's copy task, as the NAS has it: none, or the one.
func copyTasks(ctx context.Context, api *client.Client) ([]truenas.RsyncTaskQueryResultItem, error) {
	found, err := api.Rsynctask.Query(ctx, named("desc", layout.CopyTask), truenas.RsynctaskQueryOptions{})
	return found.Answer, err
}

// one makes sure the NAS has one of a kind of thing, making it if it has none.
func (n *NAS) one(ctx context.Context, api *client.Client, k kind) error {
	have, err := k.count(ctx, api)
	if err != nil || have > 0 {
		return err
	}

	n.logf("making %s", k.what)
	if err := k.create(ctx, api); err != nil {
		return fmt.Errorf("making %s: %w", k.what, err)
	}
	if have, err = k.count(ctx, api); err != nil {
		return err
	}
	if have != 1 {
		return fmt.Errorf("%s was made, and the NAS then has %d of it", k.what, have)
	}

	return nil
}

// Furnish gives a NAS that has been set up one of each thing a NAS is watched for, as acceptance/layout lists them, making only what is
// missing: run again it changes nothing. It is for whatever wants more to look at than a pool and two datasets, the tests of what an
// exporter reads above all.
func (n *NAS) Furnish(ctx context.Context) error {
	api, err := n.api()
	if err != nil {
		return err
	}

	// a NAS shares nothing over SMB until it has a user who may use it, who is never logged in as, so nobody is told the password
	users, err := api.User.Query(ctx, named("username", layout.SMBUser), truenas.UserQueryOptions{})
	if err != nil {
		return err
	}
	if len(users.Answer) == 0 {
		password, unmade := secret()
		if unmade != nil {
			return unmade
		}
		if _, err := api.User.Create(ctx, truenas.UserCreate{Username: layout.SMBUser, FullName: "A user who may use SMB", Password: password, GroupCreate: pointer.To(true), Smb: pointer.To(true)}); err != nil {
			return err
		}
	}

	if _, err := n.datasetsOf(ctx, []string{layout.SMBDataset, layout.NFSDataset}); err != nil {
		return err
	}
	for _, k := range kinds {
		if err := n.one(ctx, api, k); err != nil {
			return err
		}
	}

	// the snapshot task is run once, so that it has a last run and the run went well
	tasks, err := snapshotTasks(ctx, api)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		n.logf("making the snapshot task")
		if _, err := api.PoolSnapshottask.Create(ctx, truenas.PoolSnapshottaskCreateData{Dataset: layout.TaskDataset, NamingSchema: layout.SnapshotSchema, LifetimeValue: 1, LifetimeUnit: truenas.PoolLifetimeUnitWEEK}); err != nil {
			return err
		}
		if tasks, err = snapshotTasks(ctx, api); err != nil {
			return err
		}
	}
	if len(tasks) != 1 {
		return fmt.Errorf("the NAS has %d of the snapshot task", len(tasks))
	}
	if t := tasks[0]; t.State != nil && t.State.State == "PENDING" {
		if _, err := api.PoolSnapshottask.Run(ctx, t.Id); err != nil {
			return err
		}
		if err := settled(ctx, api); err != nil {
			return err
		}
	}

	// and the copy task once, which fails, since nothing listens where it copies to: that it failed is the point
	copies, err := copyTasks(ctx, api)
	if err != nil {
		return err
	}
	if len(copies) == 0 {
		n.logf("making the copy task")
		if _, err := api.Rsynctask.Create(ctx, truenas.RsyncTaskCreate{Path: "/mnt/" + layout.TaskDataset, User: "root", Mode: truenas.RsyncTaskModeMODULE, Remotehost: "127.0.0.1", Remotemodule: "nothing-listens-here", Direction: truenas.RsyncTaskDirectionPUSH, Desc: layout.CopyTask, Enabled: pointer.To(false)}); err != nil {
			return err
		}
		if copies, err = copyTasks(ctx, api); err != nil {
			return err
		}
	}
	if len(copies) != 1 {
		return fmt.Errorf("the NAS has %d of the copy task", len(copies))
	}
	if copies[0].Job == nil {
		_, err := api.Rsynctask.Run(ctx, copies[0].Id)
		if _, refused := errors.AsType[*truenasrpc.Error](err); err != nil && !refused {
			return err
		}
		if err == nil {
			return errors.New("a copy to a place that is not there ran without failing, and a furnished NAS is to have a task that failed")
		}
	}

	return nil
}

// settled waits for the snapshot task that was just run to say how the run went.
func settled(ctx context.Context, api *client.Client) error {
	for try := 0; ; try++ {
		tasks, err := snapshotTasks(ctx, api)
		if err != nil {
			return err
		}
		if len(tasks) == 1 && tasks[0].State != nil && tasks[0].State.State != "PENDING" && tasks[0].State.State != "RUNNING" {
			if state := tasks[0].State.State; state != "FINISHED" {
				return fmt.Errorf("the snapshot task was run and says %s", state)
			}

			return nil
		}
		if try >= 60 {
			return errors.New("the snapshot task was run and never said how the run went")
		}

		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(2 * time.Second):
		}
	}
}
