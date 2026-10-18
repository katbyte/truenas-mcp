//go:build acceptance

package tools_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// The lists are proved by making one of what each lists through the SDK, listing it through the tool, and removing it again. They are made through
// the SDK until there are tools to make them with.

// made makes something on the NAS, answering its number, and removes it when the test ends.
func made(t *testing.T, conn *truenasrpc.Conn, kind string, with map[string]any, removal ...any) int64 {
	t.Helper()

	var row struct {
		ID int64 `json:"id"`
	}
	if err := conn.Call(t.Context(), kind+".create", []any{with}, &row); err != nil {
		t.Fatalf("%s.create: %v", kind, err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(t.Context()), kind+".delete", append([]any{row.ID}, removal...), nil); err != nil {
			t.Errorf("%s.delete: %v", kind, err)
		}
	})

	return row.ID
}

// listed is the rows a list tool answers under its name for them, asked with the arguments.
func listed(t *testing.T, tool, rows string, args map[string]any) []map[string]any {
	t.Helper()

	return acctest.Rows(t, suite.Call(t, tool, args)[rows], rows)
}

// rowWith is the first row whose field is the value, or nil.
func rowWith(rows []map[string]any, field string, value any) map[string]any {
	if i := slices.IndexFunc(rows, func(row map[string]any) bool { return row[field] == value }); i >= 0 {
		return rows[i]
	}

	return nil
}

// A dataset's snapshots are listed a page at a time, each with when it was taken and what it holds, read from the snapshot itself; a dataset there is
// none of says so.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestADatasetsSnapshotsAreListedAPageAtATime(t *testing.T) {
	conn, _ := planter(t)
	dataset := layout.Datasets[0]
	for _, name := range []string{"listed-1", "listed-2", "listed-3"} {
		if err := conn.Call(t.Context(), "pool.snapshot.create", []any{map[string]any{"dataset": dataset, "name": name}}, nil); err != nil {
			t.Fatalf("taking a snapshot: %v", err)
		}
		t.Cleanup(func() {
			if err := conn.Call(context.WithoutCancel(t.Context()), "pool.snapshot.delete", []any{dataset + "@" + name}, nil); err != nil {
				t.Errorf("deleting the snapshot %s: %v", name, err)
			}
		})
	}

	all := suite.Call(t, "snapshot_list", map[string]any{"dataset": dataset, "limit": 1000})
	rows := acctest.Rows(t, all["snapshots"], "snapshots")
	for _, name := range []string{"listed-1", "listed-2", "listed-3"} {
		row := rowWith(rows, "snapshot", name)
		if row == nil || row["name"] != dataset+"@"+name || row["dataset"] != dataset || acctest.Str(row["created"]) == "" || acctest.NumOr0(row["referenced_bytes"]) <= 0 {
			t.Errorf("the snapshot %s is listed as %v", name, row)
		}
	}

	// a page of one, and the one after it
	first := suite.Call(t, "snapshot_list", map[string]any{"dataset": dataset, "limit": 1})
	if acctest.NumOr0(first["total"]) != len(rows) || len(acctest.Rows(t, first["snapshots"], "snapshots")) != 1 || acctest.NumOr0(first["next_offset"]) != 1 {
		t.Errorf("a page of one is %v, of %d", first, len(rows))
	}

	if msg := suite.CallErr(t, "snapshot_list", map[string]any{"dataset": layout.Pool + "/nowhere"}); !strings.Contains(msg, "no dataset") {
		t.Errorf("a dataset there is none of: %q", msg)
	}
}

// The tasks that protect data are listed as they were made: a snapshot task's schedule and how long it keeps its snapshots, a replication's source and
// target, and an rsync task's other end, each switched off; the pool's own scrub task is there, and the cloud tasks answer even when there are none.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheTasksThatProtectDataAreListedAsTheyWereMade(t *testing.T) {
	conn, _ := planter(t)
	const schema = "listed-%Y-%m-%d_%H-%M"
	snapshotTask := made(t, conn, "pool.snapshottask", map[string]any{"dataset": layout.Datasets[0], "naming_schema": schema, "lifetime_value": 3, "lifetime_unit": "DAY", "enabled": false, "schedule": map[string]any{"minute": "15", "hour": "4", "dom": "*", "month": "*", "dow": "*"}})
	made(t, conn, "replication", map[string]any{"name": "listed copy", "direction": "PUSH", "transport": "LOCAL", "source_datasets": []string{layout.Datasets[0]}, "target_dataset": layout.Pool + "/listed-copy", "recursive": false, "auto": false, "enabled": false, "retention_policy": "NONE", "also_include_naming_schema": []string{schema}})
	made(t, conn, "rsynctask", map[string]any{"path": "/mnt/" + layout.Datasets[0], "user": "root", "mode": "MODULE", "remotehost": "127.0.0.1", "remotemodule": "listed", "direction": "PUSH", "desc": "listed copy", "enabled": false})

	tasks := acctest.Rows(t, suite.Call(t, "snapshottask_list", nil)["snapshot_tasks"], "snapshot_tasks")
	if task := rowWith(tasks, "id", float64(snapshotTask)); task == nil || task["schedule"] != "15 4 * * *" || task["keep_for"] != "3 DAY" || task["naming_schema"] != schema || acctest.BoolOf(task["enabled"]) {
		t.Errorf("the snapshot task is listed as %v", task)
	}

	if copied := rowWith(listed(t, "replication_list", "replications", nil), "name", "listed copy"); copied == nil || copied["transport"] != "LOCAL" || copied["target_dataset"] != layout.Pool+"/listed-copy" || acctest.BoolOf(copied["auto"]) {
		t.Errorf("the replication is listed as %v", copied)
	}

	if copied := rowWith(listed(t, "rsynctask_list", "rsync_tasks", nil), "description", "listed copy"); copied == nil || copied["remote"] != "127.0.0.1::listed" || copied["path"] != "/mnt/"+layout.Datasets[0] || copied["last_run"] != nil {
		t.Errorf("the rsync task is listed as %v", copied)
	}

	if scrubs := acctest.Rows(t, suite.Call(t, "pool_scrub_list", nil)["scrubs"], "scrubs"); rowWith(scrubs, "pool", layout.Pool) == nil {
		t.Errorf("the pool's scrub task is not listed: %v", scrubs)
	}
	for tool, list := range map[string]string{"cloudsync_list": "cloud_syncs", "cloud_backup_list": "cloud_backups"} {
		if out := suite.Call(t, tool, nil); out[list] == nil {
			t.Errorf("%s answers %v", tool, out)
		}
	}
}

// The shares are listed as they were made: an SMB share that may only be read, an NFS share open to one machine, and an iSCSI target with the volume
// it offers.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheSharesAreListedAsTheyWereMade(t *testing.T) {
	conn, _ := planter(t)
	made(t, conn, "sharing.smb", map[string]any{"name": "listed", "path": "/mnt/" + layout.Datasets[0], "comment": "listed by a test", "readonly": true})
	made(t, conn, "sharing.nfs", map[string]any{"path": "/mnt/" + layout.Datasets[1], "comment": "listed by a test", "hosts": []string{"127.0.0.1"}})
	made(t, conn, "iscsi.target", map[string]any{"name": "listed"})
	volume := layout.Pool + "/listed-volume"
	if err := conn.Call(t.Context(), "pool.dataset.create", []any{map[string]any{"name": volume, "type": "VOLUME", "volsize": 16 << 20, "sparse": true}}, nil); err != nil {
		t.Fatalf("making a volume to offer: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(t.Context()), "pool.dataset.delete", []any{volume}, nil); err != nil {
			t.Errorf("deleting the volume: %v", err)
		}
	})
	made(t, conn, "iscsi.extent", map[string]any{"name": "listed", "type": "DISK", "disk": "zvol/" + volume}, false, true)

	if share := rowWith(listed(t, "sharing_smb_list", "shares", nil), "name", "listed"); share == nil || share["path"] != "/mnt/"+layout.Datasets[0] || !acctest.BoolOf(share["read_only"]) || share["comment"] != "listed by a test" {
		t.Errorf("the SMB share is listed as %v", share)
	}
	nfs := acctest.Rows(t, suite.Call(t, "sharing_nfs_list", nil)["shares"], "shares")
	if share := rowWith(nfs, "path", "/mnt/"+layout.Datasets[1]); share == nil || !slices.Equal(acctest.Strs(t, share["hosts"], "hosts"), []string{"127.0.0.1"}) {
		t.Errorf("the NFS share is listed as %v", share)
	}
	if target := rowWith(listed(t, "iscsi_target_list", "targets", nil), "name", "listed"); target == nil || target["mode"] != "ISCSI" {
		t.Errorf("the iSCSI target is listed as %v", target)
	}
	if extent := rowWith(listed(t, "iscsi_extent_list", "extents", nil), "name", "listed"); extent == nil || extent["type"] != "DISK" || extent["disk"] != "zvol/"+volume {
		t.Errorf("the iSCSI extent is listed as %v", extent)
	}
}

// What runs on the NAS is listed: its services, the apps it has (none on a NAS whose apps are not set up), and a virtual machine made for the test,
// stopped, with the memory it was given.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestWhatRunsOnTheNASIsListed(t *testing.T) {
	conn, _ := planter(t)
	made(t, conn, "vm", map[string]any{"name": "listed", "memory": 256, "autostart": false})

	services := acctest.Rows(t, suite.Call(t, "service_list", nil)["services"], "services")
	for _, name := range []string{"cifs", "nfs", "ssh"} {
		if service := rowWith(services, "service", name); service == nil || (service["state"] != "RUNNING" && service["state"] != "STOPPED") {
			t.Errorf("the service %s is listed as %v", name, service)
		}
	}

	if out := suite.Call(t, "app_list", nil); out["apps"] == nil {
		t.Errorf("app_list answers %v", out)
	}

	if machine := rowWith(listed(t, "vm_list", "machines", nil), "name", "listed"); machine == nil || machine["state"] != "STOPPED" || acctest.NumOr0(machine["memory_bytes"]) != 256<<20 || acctest.NumOr0(machine["cpus"]) < 1 || acctest.BoolOf(machine["autostart"]) {
		t.Errorf("the virtual machine is listed as %v", machine)
	}
}

// The network is listed as it is in force: an interface whose link is up with the address DHCP gave it, and the NAS's name with the name servers it
// asks.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheNetworkIsListedAsItIsInForce(t *testing.T) {
	interfaces := acctest.Rows(t, suite.Call(t, "interface_list", nil)["interfaces"], "interfaces")
	up := slices.IndexFunc(interfaces, func(n map[string]any) bool {
		addresses := acctest.RowsOfAny(n["addresses"])

		return n["link_state"] == "LINK_STATE_UP" && len(addresses) > 0
	})
	if up < 0 {
		t.Errorf("no interface is up with an address: %v", interfaces)
	}

	network := suite.Call(t, "network_configuration_get", nil)
	if servers := acctest.RowsOfAny(network["nameservers"]); network["hostname"] != layout.Hostname || len(servers) == 0 {
		t.Errorf("the network reads as %v", network)
	}
}

// The users and groups are listed, the system's own left out unless asked for: the user who may only read is there with the role that says so, and
// root only when the system's own are asked for.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheUsersAndGroupsAreListed(t *testing.T) {
	users := acctest.Rows(t, suite.Call(t, "user_list", nil)["users"], "users")
	if reader := rowWith(users, "username", layout.ReadOnlyUser); reader == nil || !slices.Contains(acctest.Strs(t, reader["roles"], "roles"), "READONLY_ADMIN") {
		t.Errorf("the user who may only read is listed as %v", reader)
	}
	if rowWith(users, "username", "root") != nil {
		t.Error("root is listed without the system's own users being asked for")
	}
	if everyone := acctest.Rows(t, suite.Call(t, "user_list", map[string]any{"builtin": true})["users"], "users"); rowWith(everyone, "username", "root") == nil {
		t.Error("root is not listed when the system's own users are asked for")
	}

	if root := rowWith(listed(t, "group_list", "groups", map[string]any{"builtin": true}), "name", "root"); root == nil || !acctest.BoolOf(root["builtin"]) {
		t.Errorf("the root group is listed as %v", root)
	}
	if own := acctest.Rows(t, suite.Call(t, "group_list", nil)["groups"], "groups"); rowWith(own, "name", "root") != nil {
		t.Error("the root group is listed without the system's own groups being asked for")
	}
}
