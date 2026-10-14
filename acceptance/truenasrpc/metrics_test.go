//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// question is one thing truenas-metrics asks a NAS: the method, the arguments it passes, and the places in the answer it reads. It is the
// exporter this SDK was made for, it asks with a key that may only read, and it reads these places and no others (lib/nas in its repository).
type question struct {
	method string
	params []any
	// reads are the places of each row, or of the one answer, the exporter reads: a name, a name inside it after a dot, every row of a list
	// after "[]". Each has to be there, though what is there may be nothing
	reads []string
	// rows is how many rows the answer has to have at least, on the NAS as the test leaves it
	rows int
}

// vdev is what the exporter reads of a pool's disk or group of disks, and of each one under it. It reads which disk each is as well, which
// only a disk says: a pair of disks has no disk of its own, and no such name at all.
var vdev = []string{"name", "type", "guid", "status", "children", "stats.read_errors", "stats.write_errors", "stats.checksum_errors", "stats.size", "stats.allocated", "stats.fragmentation"}

// ofPool is what the exporter reads of a pool, which is the same of the pool a NAS starts from.
func ofPool() []string {
	whole := []string{"name", "status", "healthy", "warning", "size", "allocated", "free", "freeing", "fragmentation", "scan.function", "scan.state", "scan.start_time", "scan.end_time", "scan.percentage", "scan.bytes_to_process", "scan.bytes_processed", "scan.bytes_issued", "scan.pause", "scan.errors", "scan.total_secs_left", "topology.log", "topology.cache", "topology.spare", "topology.special", "topology.dedup"}

	reads := make([]string, 0, len(whole)+2*len(vdev)+1)
	reads = append(reads, whole...)
	for _, place := range vdev {
		reads = append(reads, "topology.data[]."+place, "topology.data[].children[]."+place)
	}

	return append(reads, "topology.data[].children[].disk")
}

// questions is everything the exporter asks, as it asks it.
func questions() []question {
	sizes := []string{"used", "available", "usedbydataset", "usedbysnapshots", "quota", "refquota", "compressratio"}
	ofDataset := make([]string, 0, 4+len(sizes))
	ofDataset = append(ofDataset, "name", "pool", "type", "snapshot_count")
	for _, size := range sizes {
		ofDataset = append(ofDataset, size+".parsed")
	}

	task := []string{"id", "enabled", "state.state"}
	run := []string{"id", "enabled", "job"}

	return []question{
		{method: "system.info", reads: []string{"version", "system_product", "system_manufacturer", "model", "physmem", "cores", "physical_cores", "loadavg", "uptime_seconds", "boottime", "datetime", "ecc_memory"}},
		{method: "system.state"},
		{method: "update.status", reads: []string{"code", "status"}},
		{method: "directoryservices.status", reads: []string{"type", "status"}},
		{method: "api_key.my_keys", reads: []string{"expires_at", "revoked"}, rows: 1},
		{method: "pool.query", reads: ofPool(), rows: 1},
		{method: "boot.get_state", reads: ofPool()},
		{method: "disk.query", reads: []string{"name", "serial", "lunid", "model", "type", "bus", "size", "enclosure"}, rows: 5},
		{method: "disk.temperatures"},
		{method: "pool.dataset.query", params: []any{[]any{}, map[string]any{"extra": map[string]any{"flat": true, "retrieve_children": true, "properties": sizes, "snapshots_count": true}}}, reads: ofDataset, rows: 3},
		{method: "sharing.smb.query", reads: []string{"enabled", "locked"}, rows: 1},
		{method: "sharing.nfs.query", reads: []string{"enabled", "locked"}, rows: 1},
		{method: "iscsi.target.query", rows: 1},
		{method: "nfs.client_count"},
		{method: "iscsi.global.client_count"},
		{method: "certificate.query", reads: []string{"name", "until"}, rows: 1},
		{method: "interface.query", reads: []string{"name", "type", "state.link_state", "state.mtu"}, rows: 1},
		{method: "alert.list", reads: []string{"level", "klass", "dismissed"}},
		{method: "service.query", reads: []string{"service", "state", "enable"}, rows: 5},
		{method: "core.get_jobs", params: []any{[]any{[]any{"state", "in", []string{"RUNNING", "WAITING"}}}}, reads: []string{"method", "state"}},
		{method: "docker.status", reads: []string{"status"}},
		{method: "app.query", reads: []string{"name", "state", "upgrade_available"}},
		{method: "vm.query", reads: []string{"name", "status.state"}, rows: 1},
		{method: "pool.snapshottask.query", reads: append([]string{"dataset"}, task...), rows: 1},
		{method: "replication.query", reads: append([]string{"name"}, task...), rows: 1},
		{method: "rsynctask.query", reads: append([]string{"desc"}, run...), rows: 1},
		{method: "cloudsync.query", reads: append([]string{"description"}, run...)},
		{method: "cloud_backup.query", reads: append([]string{"description"}, run...)},
		{method: "pool.scrub.query", reads: []string{"id", "pool_name", "enabled"}, rows: 1},
	}
}

// lacking says what a value does not have of a place: nothing when the place is there, whatever is at it.
func lacking(value any, place string) string {
	if place == "" {
		return ""
	}
	name, rest, _ := strings.Cut(place, ".")
	each := strings.HasSuffix(name, "[]")
	name = strings.TrimSuffix(name, "[]")

	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Sprintf("something that is no object where %s is looked for", name)
	}
	at, has := object[name]
	switch {
	case !has:
		return "no " + name
	case at == nil:
		return ""
	case !each:
		return lacking(at, rest)
	}

	rows, ok := at.([]any)
	if !ok {
		return name + " that is no list"
	}
	for _, row := range rows {
		if lacks := lacking(row, rest); lacks != "" {
			return name + " with " + lacks
		}
	}

	return ""
}

// madeOne makes one of something on the NAS and removes it when the test ends, answering its number.
func madeOne(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, kind string, with map[string]any, removal ...any) int64 {
	t.Helper()

	var made struct {
		ID int64 `json:"id"`
	}
	var answer json.RawMessage
	if err := conn.Call(ctx, kind+".create", []any{with}, &answer); err != nil {
		t.Fatalf("%s.create: %v", kind, err)
	}
	// most answer what was made, and a few only its number
	if json.Unmarshal(answer, &made) != nil || made.ID == 0 {
		if err := json.Unmarshal(answer, &made.ID); err != nil || made.ID == 0 {
			t.Fatalf("%s.create answered %s, with no number to know what was made by", kind, answer)
		}
	}

	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), kind+".delete", append([]any{made.ID}, removal...), nil); err != nil {
			t.Errorf("%s.delete: %v", kind, err)
		}
	})

	return made.ID
}

// The exporter this SDK was made for asks a NAS about thirty things, with a key that may only read. A NAS is given one of everything it
// asks after, and every question is then asked exactly as the exporter asks it: it has to be answered, the answer has to be what the
// description says, and every place the exporter reads has to be there. A change to a NAS that would break the exporter fails here first.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestEverythingTheExporterAsksIsAnsweredToAKeyThatMayOnlyRead(t *testing.T) {
	conn, only := admin(t), reader(t)
	ctx := within(t, 12*time.Minute)
	methods := description(t)

	// one of each thing the exporter counts: a share of each kind, a target, a machine, and a task of each kind that can be made with
	// nothing outside the NAS
	for _, name := range []string{"shared-smb", "shared-nfs"} {
		dataset := layout.Pool + "/" + name
		if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": dataset}}, nil); err != nil {
			t.Fatalf("making %s: %v", dataset, err)
		}
		t.Cleanup(func() {
			if err := conn.Call(context.WithoutCancel(ctx), "pool.dataset.delete", []any{dataset, map[string]any{"recursive": true, "force": true}}, nil); err != nil {
				t.Errorf("removing %s: %v", dataset, err)
			}
		})
	}

	// a NAS shares nothing over SMB until it has a user who may use it
	madeOne(ctx, t, conn, "user", map[string]any{"username": "sharer", "full_name": "A user who may use SMB", "password": "only-for-the-test-NAS-1", "group_create": true, "smb": true}, map[string]any{"delete_group": true}) //nolint:gosec // G101: the password of a user made on a NAS that lives for one run
	madeOne(ctx, t, conn, "sharing.smb", map[string]any{"name": "shared", "path": "/mnt/" + layout.Pool + "/shared-smb"})
	madeOne(ctx, t, conn, "sharing.nfs", map[string]any{"path": "/mnt/" + layout.Pool + "/shared-nfs"})
	madeOne(ctx, t, conn, "iscsi.target", map[string]any{"name": "exported"})
	madeOne(ctx, t, conn, "vm", map[string]any{"name": "small", "memory": 256, "autostart": false})

	snapshots := madeOne(ctx, t, conn, "pool.snapshottask", map[string]any{"dataset": layout.Datasets[1], "naming_schema": "counted-%Y-%m-%d_%H-%M", "lifetime_value": 1, "lifetime_unit": "HOUR"})
	madeOne(ctx, t, conn, "replication", map[string]any{"name": "counted by the exporter", "direction": "PUSH", "transport": "LOCAL", "source_datasets": []string{layout.Datasets[1]}, "target_dataset": layout.Pool + "/copied", "recursive": false, "auto": false, "retention_policy": "NONE", "name_regex": "counted-.*"})
	madeOne(ctx, t, conn, "rsynctask", map[string]any{"path": "/mnt/" + layout.Datasets[1], "user": "root", "mode": "MODULE", "remotehost": "127.0.0.1", "remotemodule": "nowhere", "direction": "PUSH", "desc": "counted by the exporter", "enabled": false})

	if err := conn.Call(ctx, "pool.snapshottask.run", []any{snapshots}, nil); err != nil {
		t.Fatalf("running the snapshot task: %v", err)
	}
	t.Cleanup(func() {
		var taken []struct {
			Name string `json:"name"`
		}
		_ = conn.Call(context.WithoutCancel(ctx), "pool.snapshot.query", []any{[]any{[]any{"dataset", "=", layout.Datasets[1]}}}, &taken)
		for _, one := range taken {
			_ = conn.Call(context.WithoutCancel(ctx), "pool.snapshot.delete", []any{one.Name}, nil)
		}
	})
	if !eventually(ctx, 2*time.Minute, func() bool {
		state, _ := task(ctx, t, conn, "pool.snapshottask.query", snapshots).state()

		return state == "FINISHED"
	}) {
		t.Fatal("the snapshot task never finished its run")
	}

	for _, q := range questions() {
		t.Run(q.method, func(t *testing.T) {
			var answer json.RawMessage
			if err := only.Call(within(t, readWait), q.method, q.params, &answer); err != nil {
				t.Fatalf("the exporter's question is refused: %v", err)
			}
			heldTo(t, methods[q.method], answer, false)

			var value any
			if err := json.Unmarshal(answer, &value); err != nil {
				t.Fatal(err)
			}

			rows, listed := value.([]any)
			if !listed {
				rows = []any{value}
			}
			if listed && len(rows) < q.rows {
				t.Errorf("%d listed, and the NAS has at least %d", len(rows), q.rows)
			}
			if len(q.reads) == 0 {
				return
			}

			for _, row := range rows {
				for _, place := range q.reads {
					if lacks := lacking(row, place); lacks != "" {
						t.Errorf("the exporter reads %s, and the answer has %s", place, lacks)
					}
				}
			}
		})
	}
}
