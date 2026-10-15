//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/described"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenas/client"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// question is one thing truenas-exporter asks a NAS: the method, what it asks it with, and the places in the answer it reads. It is the exporter
// this SDK was made for, it asks through the typed client with a key that may only read, and it reads these places and no others (lib/nas in
// its repository).
type question struct {
	// name is what the test calls the question: its method, and what it is asked for when the exporter asks the method more than one way
	name   string
	method string

	// a query's filters, the fields it selects, which are all the NAS answers with, and its extra options
	filters []any
	fields  []any
	extra   map[string]any

	// reads are the places of each row, or of the one answer, the exporter reads: a name, a name inside it after a dot, every row of a list
	// after "[]". Each has to be there, though what is there may be nothing. They are the fields selected unless given
	reads []string
	// rows is how many rows the answer has to have at least, on the NAS as the test leaves it
	rows int
}

// called is what the test calls a question.
func (q question) called() string {
	if q.name != "" {
		return q.name
	}

	return q.method
}

// params is what a question is asked with, as the typed client sends it: a query's filters and the options it sets, and nothing for the rest.
func (q question) params() []any {
	if q.fields == nil {
		return nil
	}

	options := map[string]any{"select": q.fields}
	if q.extra != nil {
		options["extra"] = q.extra
	}
	filters := q.filters
	if filters == nil {
		filters = []any{}
	}

	return []any{filters, options}
}

// places is the places a question reads.
func (q question) places() []string {
	if q.reads != nil {
		return q.reads
	}

	places := make([]string, 0, len(q.fields))
	for _, field := range q.fields {
		places = append(places, fmt.Sprint(field))
	}

	return places
}

// what the exporter asks for of each thing, named as lib/nas names it
var (
	poolFields        = []any{"name", "status", "healthy", "warning", "size", "allocated", "free", "freeing", "fragmentation", "scan", "topology"}
	diskFields        = []any{"name", "serial", "lunid", "model", "type", "bus", "size", "enclosure"}
	datasetProperties = []string{"used", "available", "usedbydataset", "usedbysnapshots", "quota", "refquota", "compressratio"}
	datasetFields     = []any{"name", "pool", "type", "used.parsed", "available.parsed", "usedbydataset.parsed", "usedbysnapshots.parsed", "quota.parsed", "refquota.parsed", "compressratio.parsed"}
	snapshotFields    = []any{"name", "pool", "snapshot_count"}
	shareFields       = []any{"enabled", "locked"}
	jobFields         = []any{"job.state", "job.progress.percent", "job.time_started", "job.time_finished"}

	// everyDataset is a filter every dataset passes: asked for datasets without the ones under each, a NAS goes below the pools only when it is
	// given a filter
	everyDataset = []any{[]any{"type", "in", []string{"FILESYSTEM", "VOLUME"}}}

	// datasetsAsked and snapshotsAsked are the extra options of the exporter's two dataset questions: each dataset without the ones under it,
	// with the properties it reads, or with none and a count of its snapshots
	datasetsAsked  = map[string]any{"retrieve_children": false, "properties": datasetProperties}
	snapshotsAsked = map[string]any{"retrieve_children": false, "properties": []string{}, "snapshots_count": true}
)

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
	// a task's last run is read only of a task that has run, so it is not looked for in every row
	task := []string{"id", "enabled", "state.state"}

	return []question{
		{method: "system.info", reads: []string{"version", "system_product", "system_manufacturer", "model", "physmem", "cores", "physical_cores", "loadavg", "uptime_seconds", "boottime", "datetime", "ecc_memory"}},
		{method: "system.state"},
		{method: "update.status", reads: []string{"code", "status"}},
		{method: "directoryservices.status", reads: []string{"type", "status"}},
		{method: "api_key.my_keys", reads: []string{"expires_at", "revoked"}, rows: 1},
		{method: "pool.query", fields: poolFields, reads: ofPool(), rows: 1},
		{method: "boot.get_state", reads: ofPool()},
		{method: "disk.query", fields: diskFields, rows: 5},
		{method: "disk.temperatures"},
		{method: "pool.dataset.query", filters: everyDataset, fields: datasetFields, extra: datasetsAsked, rows: 3},
		{name: "pool.dataset.query for snapshots", method: "pool.dataset.query", filters: everyDataset, fields: snapshotFields, extra: snapshotsAsked, rows: 3},
		{method: "sharing.smb.query", fields: shareFields, rows: 1},
		{method: "sharing.nfs.query", fields: shareFields, rows: 1},
		{method: "iscsi.target.query", fields: []any{"id"}, rows: 1},
		{method: "nfs.client_count"},
		{method: "iscsi.global.client_count"},
		{method: "certificate.query", fields: []any{"name", "until"}, rows: 1},
		{method: "interface.query", fields: []any{"name", "type", "state.link_state", "state.mtu"}, rows: 1},
		{method: "alert.list", reads: []string{"level", "klass", "dismissed"}},
		{method: "service.query", fields: []any{"service", "state", "enable"}, rows: 5},
		{method: "core.get_jobs", filters: []any{[]any{"state", "in", []string{"RUNNING", "WAITING"}}}, fields: []any{"method", "state"}},
		{method: "docker.status", reads: []string{"status"}},
		{method: "app.query", fields: []any{"name", "state", "upgrade_available"}},
		{method: "vm.query", fields: []any{"name", "status.state"}, rows: 1},
		{method: "pool.snapshottask.query", fields: []any{"id", "dataset", "enabled", "state.state", "state.datetime"}, reads: append([]string{"dataset"}, task...), rows: 1},
		{method: "replication.query", fields: []any{"id", "name", "enabled", "state.state", "state.datetime"}, reads: append([]string{"name"}, task...), rows: 1},
		{method: "rsynctask.query", fields: append([]any{"id", "desc", "enabled"}, jobFields...), rows: 1},
		{method: "cloudsync.query", fields: append([]any{"id", "description", "enabled"}, jobFields...)},
		{method: "cloud_backup.query", fields: append([]any{"id", "description", "enabled"}, jobFields...)},
		{method: "pool.scrub.query", fields: []any{"id", "pool_name", "enabled"}, rows: 1},
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
		t.Run(q.called(), func(t *testing.T) {
			var answer json.RawMessage
			if err := only.Call(within(t, readWait), q.method, q.params(), &answer); err != nil {
				t.Fatalf("the exporter's question is refused: %v", err)
			}
			if q.fields == nil {
				heldTo(t, methods[q.method], answer, false)
			} else {
				heldToSelected(t, methods[q.method], answer)
			}

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

			for _, row := range rows {
				for _, place := range q.places() {
					if lacks := lacking(row, place); lacks != "" {
						t.Errorf("the exporter reads %s, and the answer has %s", place, lacks)
					}
				}
			}
		})
	}
}

// heldToSelected holds an answer to a query that selected some fields to the description, as heldTo does a whole answer: what it holds has to
// be as described, but for what is known to differ, and what it was not asked for is not missed. A known difference in a field it did not ask for
// cannot show, so one that does not show is not told of.
func heldToSelected(t *testing.T, m described.Method, answer json.RawMessage) {
	t.Helper()

	for _, difference := range m.Selected(answer) {
		if !slices.Contains(surprises[m.Name].differs, difference) {
			t.Errorf("not as described: %s", difference)
		}
	}
	for _, place := range described.Blanked(answer) {
		if !slices.Contains(secrets(t)[m.Name], place) {
			t.Errorf("something was kept back at %s, where api-defs lists no secret", place)
		}
	}
}

// alone is a typed call that takes nothing but the context, as the test makes it: what it read, whatever its type.
func alone[R any](call func(context.Context) (R, error)) func(context.Context, question) (any, error) {
	return func(ctx context.Context, _ question) (any, error) {
		r, err := call(ctx)

		return reflect.ValueOf(r).FieldByName("Answer").Interface(), err
	}
}

// typedQuestions is how the exporter asks each of its questions through the typed client, by what the test calls it: the call, made with the
// question's filters, fields and extra options, and what it answered.
func typedQuestions(typed *client.Client) map[string]func(context.Context, question) (any, error) {
	datasets := func(ctx context.Context, q question) (any, error) {
		r, err := typed.PoolDataset.Query(ctx, q.filters, truenas.PoolDatasetQueryOptions{Select: q.fields, Extra: q.extra})
		return r.Answer, err
	}

	return map[string]func(context.Context, question) (any, error){
		"system.info":              alone(typed.System.Info),
		"system.state":             alone(typed.System.State),
		"update.status":            alone(typed.Update.Status),
		"directoryservices.status": alone(typed.Directoryservices.Status),
		"api_key.my_keys":          alone(typed.ApiKey.MyKeys),
		"pool.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Pool.Query(ctx, q.filters, truenas.PoolQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"boot.get_state": alone(typed.Boot.GetState),
		"disk.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Disk.Query(ctx, q.filters, truenas.DiskQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"disk.temperatures": func(ctx context.Context, _ question) (any, error) {
			r, err := typed.Disk.Temperatures(ctx, nil, false)
			return r.Answer, err
		},
		"pool.dataset.query":               datasets,
		"pool.dataset.query for snapshots": datasets,
		"sharing.smb.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.SharingSmb.Query(ctx, q.filters, truenas.SharingSmbQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"sharing.nfs.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.SharingNfs.Query(ctx, q.filters, truenas.SharingNfsQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"iscsi.target.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.IscsiTarget.Query(ctx, q.filters, truenas.IscsiTargetQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"nfs.client_count":          alone(typed.Nfs.ClientCount),
		"iscsi.global.client_count": alone(typed.IscsiGlobal.ClientCount),
		"certificate.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Certificate.Query(ctx, q.filters, truenas.CertificateQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"interface.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Interface.Query(ctx, q.filters, truenas.InterfaceQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"alert.list": alone(typed.Alert.List),
		"service.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Service.Query(ctx, q.filters, truenas.ServiceQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"core.get_jobs": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Core.GetJobs(ctx, q.filters, truenas.CoreGetJobsOptions{Select: q.fields})
			return r.Answer, err
		},
		"docker.status": alone(typed.Docker.Status),
		"app.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.App.Query(ctx, q.filters, truenas.AppQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"vm.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Vm.Query(ctx, q.filters, truenas.VmQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"pool.snapshottask.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.PoolSnapshottask.Query(ctx, q.filters, truenas.PoolSnapshottaskQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"replication.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Replication.Query(ctx, q.filters, truenas.ReplicationQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"rsynctask.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Rsynctask.Query(ctx, q.filters, truenas.RsynctaskQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"cloudsync.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.Cloudsync.Query(ctx, q.filters, truenas.CloudsyncQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"cloud_backup.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.CloudBackup.Query(ctx, q.filters, truenas.CloudBackupQueryOptions{Select: q.fields})
			return r.Answer, err
		},
		"pool.scrub.query": func(ctx context.Context, q question) (any, error) {
			r, err := typed.PoolScrub.Query(ctx, q.filters, truenas.PoolScrubQueryOptions{Select: q.fields})
			return r.Answer, err
		},
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
		t.Run(q.called(), func(t *testing.T) {
			ask, ok := asked[q.called()]
			if !ok {
				t.Fatal("the exporter asks this, and the test has no typed call for it")
			}
			answer, refused := ask(within(t, readWait), q)
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

	// so are a task's state and last run: each furnished task reads as the state it was left in
	snapshots, err := typed.PoolSnapshottask.Query(within(t, readWait), []any{[]any{"dataset", "=", layout.TaskDataset}}, truenas.PoolSnapshottaskQueryOptions{})
	if err != nil || len(snapshots.Answer) == 0 || snapshots.Answer[0].State == nil || snapshots.Answer[0].State.State != "FINISHED" || snapshots.Answer[0].State.Datetime.IsZero() {
		t.Errorf("the furnished snapshot task reads as %+v (%v)", snapshots.Answer, err)
	}
	replications, err := typed.Replication.Query(within(t, readWait), []any{[]any{"name", "=", layout.Replication}}, truenas.ReplicationQueryOptions{})
	if err != nil || len(replications.Answer) == 0 || replications.Answer[0].State == nil || replications.Answer[0].State.State != "PENDING" {
		t.Errorf("the furnished replication reads as %+v (%v)", replications.Answer, err)
	}
	// and so is the count of a dataset's snapshots, which the description leaves out of a dataset's row: asked for the exporter's way, the dataset
	// the furnished snapshot task snapshots has some
	counted, err := typed.PoolDataset.Query(within(t, readWait), everyDataset, truenas.PoolDatasetQueryOptions{Select: snapshotFields, Extra: snapshotsAsked})
	var count *int
	for _, dataset := range counted.Answer {
		if dataset.Name == layout.TaskDataset {
			count = dataset.SnapshotCount
		}
	}
	if err != nil || count == nil || *count == 0 {
		t.Errorf("the furnished task's dataset reads as having %v snapshots, of %d datasets counted (%v)", count, len(counted.Answer), err)
	}
	copies, err := typed.Rsynctask.Query(within(t, readWait), []any{[]any{"desc", "=", layout.CopyTask}}, truenas.RsynctaskQueryOptions{})
	if err != nil || len(copies.Answer) == 0 || copies.Answer[0].Job == nil || copies.Answer[0].Job.State != "FAILED" || copies.Answer[0].Job.TimeFinished.IsZero() || copies.Answer[0].Job.Error == "" {
		t.Errorf("the furnished copy task reads as %+v (%v)", copies.Answer, err)
	}
}
