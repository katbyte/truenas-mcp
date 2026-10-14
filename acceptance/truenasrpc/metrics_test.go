//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenas/client"
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

// furnishedTask is one of the tasks a furnished NAS has, found by what it is known by.
func furnishedTask(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, list, field, value string) tasked {
	t.Helper()

	var found []tasked
	if err := conn.Call(ctx, list, []any{[]any{[]any{field, "=", value}}}, &found); err != nil || len(found) == 0 {
		t.Fatalf("%s: the furnished task is not found: %d found, %v", list, len(found), err)
	}

	return found[0]
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

	// one of each thing the exporter counts, made the way anything else that wants a furnished NAS has it made, and left there: the tests
	// after this one read a NAS with something of every kind on it
	furnished(t)

	// each task was left in the state it is meant to be seen in
	if state, _ := furnishedTask(ctx, t, conn, "pool.snapshottask.query", "dataset", layout.TaskDataset).state(); state != "FINISHED" {
		t.Errorf("the furnished snapshot task says %s, want it finished", state)
	}
	if state, _ := furnishedTask(ctx, t, conn, "replication.query", "name", layout.Replication).state(); state != "PENDING" {
		t.Errorf("the furnished replication says %s, want it never run", state)
	}
	var copying []struct {
		Job *struct {
			State string `json:"state"`
		} `json:"job"`
	}
	if err := conn.Call(ctx, "rsynctask.query", []any{[]any{[]any{"desc", "=", layout.CopyTask}}}, &copying); err != nil || len(copying) != 1 || copying[0].Job == nil || copying[0].Job.State != "FAILED" {
		t.Errorf("the furnished copy task reads as %+v (%v), want its one run failed", copying, err)
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

// answered is a typed call as the test makes it: what it read, whatever its type.
func answered[R any](call func(context.Context) (R, error)) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		r, err := call(ctx)

		return reflect.ValueOf(r).FieldByName("Answer").Interface(), err
	}
}

// typedQuestions is how the exporter asks each of its questions through the typed client: the call, and what it answered.
func typedQuestions(typed *client.Client) map[string]func(ctx context.Context) (any, error) {
	sizes := []string{"used", "available", "usedbydataset", "usedbysnapshots", "quota", "refquota", "compressratio"}

	return map[string]func(ctx context.Context) (any, error){
		"system.info":              answered(typed.System.Info),
		"system.state":             answered(typed.System.State),
		"update.status":            answered(typed.Update.Status),
		"directoryservices.status": answered(typed.Directoryservices.Status),
		"api_key.my_keys":          answered(typed.ApiKey.MyKeys),
		"pool.query": answered(func(ctx context.Context) (truenas.PoolQueryResponse, error) {
			return typed.Pool.Query(ctx, nil, truenas.PoolQueryOptions{})
		}),
		"boot.get_state": answered(typed.Boot.GetState),
		"disk.query": answered(func(ctx context.Context) (truenas.DiskQueryResponse, error) {
			return typed.Disk.Query(ctx, nil, truenas.DiskQueryOptions{})
		}),
		"disk.temperatures": answered(func(ctx context.Context) (truenas.DiskTemperaturesResponse, error) {
			return typed.Disk.Temperatures(ctx, nil, false)
		}),
		"pool.dataset.query": answered(func(ctx context.Context) (truenas.PoolDatasetQueryResponse, error) {
			return typed.PoolDataset.Query(ctx, nil, truenas.PoolDatasetQueryOptions{Extra: map[string]any{"flat": true, "retrieve_children": true, "properties": sizes, "snapshots_count": true}})
		}),
		"sharing.smb.query": answered(func(ctx context.Context) (truenas.SharingSmbQueryResponse, error) {
			return typed.SharingSmb.Query(ctx, nil, truenas.SharingSmbQueryOptions{})
		}),
		"sharing.nfs.query": answered(func(ctx context.Context) (truenas.SharingNfsQueryResponse, error) {
			return typed.SharingNfs.Query(ctx, nil, truenas.SharingNfsQueryOptions{})
		}),
		"iscsi.target.query": answered(func(ctx context.Context) (truenas.IscsiTargetQueryResponse, error) {
			return typed.IscsiTarget.Query(ctx, nil, truenas.IscsiTargetQueryOptions{})
		}),
		"nfs.client_count":          answered(typed.Nfs.ClientCount),
		"iscsi.global.client_count": answered(typed.IscsiGlobal.ClientCount),
		"certificate.query": answered(func(ctx context.Context) (truenas.CertificateQueryResponse, error) {
			return typed.Certificate.Query(ctx, nil, truenas.CertificateQueryOptions{})
		}),
		"interface.query": answered(func(ctx context.Context) (truenas.InterfaceQueryResponse, error) {
			return typed.Interface.Query(ctx, nil, truenas.InterfaceQueryOptions{})
		}),
		"alert.list": answered(typed.Alert.List),
		"service.query": answered(func(ctx context.Context) (truenas.ServiceQueryResponse, error) {
			return typed.Service.Query(ctx, nil, truenas.ServiceQueryOptions{})
		}),
		"core.get_jobs": answered(func(ctx context.Context) (truenas.CoreGetJobsResponse, error) {
			return typed.Core.GetJobs(ctx, []any{[]any{"state", "in", []string{"RUNNING", "WAITING"}}}, truenas.CoreGetJobsOptions{})
		}),
		"docker.status": answered(typed.Docker.Status),
		"app.query": answered(func(ctx context.Context) (truenas.AppQueryResponse, error) {
			return typed.App.Query(ctx, nil, truenas.AppQueryOptions{})
		}),
		"vm.query": answered(func(ctx context.Context) (truenas.VmQueryResponse, error) {
			return typed.Vm.Query(ctx, nil, truenas.VmQueryOptions{})
		}),
		"pool.snapshottask.query": answered(func(ctx context.Context) (truenas.PoolSnapshottaskQueryResponse, error) {
			return typed.PoolSnapshottask.Query(ctx, nil, truenas.PoolSnapshottaskQueryOptions{})
		}),
		"replication.query": answered(func(ctx context.Context) (truenas.ReplicationQueryResponse, error) {
			return typed.Replication.Query(ctx, nil, truenas.ReplicationQueryOptions{})
		}),
		"rsynctask.query": answered(func(ctx context.Context) (truenas.RsynctaskQueryResponse, error) {
			return typed.Rsynctask.Query(ctx, nil, truenas.RsynctaskQueryOptions{})
		}),
		"cloudsync.query": answered(func(ctx context.Context) (truenas.CloudsyncQueryResponse, error) {
			return typed.Cloudsync.Query(ctx, nil, truenas.CloudsyncQueryOptions{})
		}),
		"cloud_backup.query": answered(func(ctx context.Context) (truenas.CloudBackupQueryResponse, error) {
			return typed.CloudBackup.Query(ctx, nil, truenas.CloudBackupQueryOptions{})
		}),
		"pool.scrub.query": answered(func(ctx context.Context) (truenas.PoolScrubQueryResponse, error) {
			return typed.PoolScrub.Query(ctx, nil, truenas.PoolScrubQueryOptions{})
		}),
	}
}

// The exporter is to move onto the typed client generated from the NAS's description, so every question it asks is asked that way too, with the key
// that may only read, on the furnished NAS: each answer has to read into the client's types, secrets sent as ******** and dates as a NAS writes them
// included, and a list has the rows the NAS has.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestEverythingTheExporterAsksReadsIntoTheTypedClient(t *testing.T) {
	furnished(t)

	typed, err := client.New(reader(t))
	if err != nil {
		t.Fatal(err)
	}
	asked := typedQuestions(typed)

	for _, q := range questions() {
		t.Run(q.method, func(t *testing.T) {
			ask, ok := asked[q.method]
			if !ok {
				t.Fatal("the exporter asks this, and the test has no typed call for it")
			}
			answer, refused := ask(within(t, readWait))
			if refused != nil {
				t.Fatalf("asked through the typed client: %v", refused)
			}
			if v := reflect.ValueOf(answer); v.Kind() == reflect.Slice && v.Len() < q.rows {
				t.Errorf("%d listed, and the NAS has at least %d", v.Len(), q.rows)
			}
		})
	}
	if len(asked) != len(questions()) {
		t.Errorf("%d typed calls for the exporter's %d questions", len(asked), len(questions()))
	}

	// a pool's layout and scrub, which the description leaves shapeless, are given shapes in sdk/pandorest: what the exporter reads of them has to be
	// there on a real pool
	pools, err := typed.Pool.Query(within(t, readWait), nil, truenas.PoolQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range pools.Answer {
		if pool.Topology == nil || len(pool.Topology.Data) == 0 || pool.Autotrim == nil || pool.Autotrim.Value == "" {
			t.Errorf("the pool %s reads with no layout or trim: %+v", pool.Name, pool)

			continue
		}
		for _, vdev := range pool.Topology.Data {
			disks := vdev.Children
			if vdev.Type == "DISK" {
				disks = []truenas.PoolVdev{vdev}
			}
			for _, disk := range disks {
				if vdev.Type == "" || vdev.Stats == nil || disk.Type != "DISK" || disk.Disk == "" || disk.Status == "" || disk.Stats == nil || len(disk.Stats.Ops) == 0 {
					t.Errorf("in the pool %s, %s reads as %+v with the disk %+v", pool.Name, vdev.Name, vdev, disk)
				}
			}
		}
		if pool.Scan != nil && pool.Scan.Function != "" && (pool.Scan.State == "" || pool.Scan.StartTime.IsZero()) {
			t.Errorf("the pool %s's last scrub reads as %+v", pool.Name, pool.Scan)
		}
	}
}
