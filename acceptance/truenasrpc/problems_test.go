//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// The tests here give the NAS a real problem, read how it says so, and put it right. They are what an audit has to find, so each is made for
// real and not described: a pool with a disk gone, a dataset at its limit, a pool with little left, a task that failed.

// alert is one thing a NAS says is wrong with it.
type alert struct {
	UUID      string `json:"uuid"`
	Class     string `json:"klass"`
	Level     string `json:"level"`
	Formatted string `json:"formatted"`
	Dismissed bool   `json:"dismissed"`
	OneShot   bool   `json:"one_shot"`
}

// alerted is what the NAS says is wrong with it now, of one class.
func alerted(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, class string) []alert {
	t.Helper()

	var all, of []alert
	if err := conn.Call(ctx, "alert.list", nil, &all); err != nil {
		t.Fatalf("listing what the NAS says is wrong: %v", err)
	}
	for _, one := range all {
		if one.Class == class {
			of = append(of, one)
		}
	}

	return of
}

// unwell is what the NAS says is wrong now with the pool the journey made. A NAS goes on saying a pool is in trouble for a minute or so after
// the pool is gone, so what it says of another pool is not what a test of this one is asking after.
func unwell(ctx context.Context, t *testing.T, conn *truenasrpc.Conn) []alert {
	t.Helper()

	var of []alert
	for _, one := range alerted(ctx, t, conn, "VolumeStatus") {
		if strings.Contains(one.Formatted, "Pool "+layout.Pool+" ") {
			of = append(of, one)
		}
	}

	return of
}

// eventually waits for something to be so, asking every few seconds, and reports whether it came to be in time.
func eventually(ctx context.Context, limit time.Duration, so func() bool) bool {
	deadline := time.Now().Add(limit)
	for !so() {
		if time.Now().After(deadline) {
			return false
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(3 * time.Second):
		}
	}

	return true
}

// pooled is a pool as the NAS has it: its state and the disks it is made of. A pool of one disk has that disk where a pair has its mirror.
type pooled struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Healthy   bool   `json:"healthy"`
	Warning   bool   `json:"warning"`
	Size      int64  `json:"size"`
	Allocated int64  `json:"allocated"`
	Topology  struct {
		Data []struct {
			Type     string `json:"type"`
			GUID     string `json:"guid"`
			Disk     string `json:"disk"`
			Status   string `json:"status"`
			Children []struct {
				GUID   string `json:"guid"`
				Disk   string `json:"disk"`
				Status string `json:"status"`
			} `json:"children"`
		} `json:"data"`
		Spare []struct {
			GUID   string `json:"guid"`
			Disk   string `json:"disk"`
			Status string `json:"status"`
		} `json:"spare"`
	} `json:"topology"`
}

// pool is the pool the journey made, and poolNamed any pool by its name: found says whether there is one.
func pool(ctx context.Context, t *testing.T, conn *truenasrpc.Conn) pooled {
	t.Helper()

	made, found := poolNamed(ctx, t, conn, layout.Pool)
	if !found {
		t.Fatal("the pool the journey made is not found")
	}

	return made
}

func poolNamed(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name string) (pooled, bool) {
	t.Helper()

	var pools []pooled
	if err := conn.Call(ctx, "pool.query", []any{[]any{[]any{"name", "=", name}}}, &pools); err != nil || len(pools) > 1 {
		t.Fatalf("looking for the pool %s: %d found, %v", name, len(pools), err)
	}
	if len(pools) == 0 {
		return pooled{}, false
	}

	return pools[0], true
}

// spare is a disk nothing uses, as the NAS lists one: it is named one way to make or grow a pool and another to replace a disk.
type spare struct {
	Name       string `json:"name"`
	Identifier string `json:"identifier"`
	Serial     string `json:"serial"`
}

func spares(ctx context.Context, t *testing.T, conn *truenasrpc.Conn) []spare {
	t.Helper()

	var unused []spare
	if err := conn.Call(ctx, "disk.details", []any{map[string]any{"type": "UNUSED"}}, &unused); err != nil {
		t.Fatalf("listing the disks nothing uses: %v", err)
	}

	return unused
}

// put writes a small file on the NAS, over whatever is there, and got reads one back.
func put(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, path, text string) {
	t.Helper()
	ledger.count("filesystem.put")

	job, err := conn.Upload(ctx, "filesystem.put", []any{path, map[string]any{}}, strings.NewReader(text))
	if err == nil {
		err = job.Wait(ctx, nil)
	}
	if err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func got(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, path string) string {
	t.Helper()
	ledger.count("filesystem.get")

	var text strings.Builder
	if _, err := conn.Download(ctx, "filesystem.get", []any{path}, &text); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return text.String()
}

// A pool that loses one disk of a pair keeps working and is no longer safe. The NAS says so in the pool's own state at once and as an alert
// within a couple of minutes, and takes both back when the disk returns.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAPoolThatLosesADiskSaysSoAndIsWholeAgainWhenTheDiskIsBack(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 12*time.Minute)

	before := pool(ctx, t, conn)
	if !before.Healthy || len(before.Topology.Data) != 1 || len(before.Topology.Data[0].Children) != 2 {
		t.Fatalf("the pool is not the healthy pair of disks the journey made: %+v", before)
	}
	gone := before.Topology.Data[0].Children[0]

	back := func() {
		if err := conn.Call(context.WithoutCancel(ctx), "pool.online", []any{before.ID, map[string]any{"label": gone.GUID}}, nil); err != nil {
			t.Errorf("putting the disk back: %v", err)
		}
	}

	if err := conn.Call(ctx, "pool.offline", []any{before.ID, map[string]any{"label": gone.GUID}}, nil); err != nil {
		t.Fatalf("taking a disk out of the pool: %v", err)
	}
	returned := false
	t.Cleanup(func() {
		if !returned {
			back()
		}
	})

	without := pool(ctx, t, conn)
	if without.Healthy || without.Status != "DEGRADED" || without.Topology.Data[0].Children[0].Status != "OFFLINE" || without.Topology.Data[0].Children[1].Status != "ONLINE" {
		t.Errorf("with a disk out the pool reads as %s (healthy %t), its disks %s and %s; want DEGRADED, OFFLINE and ONLINE", without.Status, without.Healthy, without.Topology.Data[0].Children[0].Status, without.Topology.Data[0].Children[1].Status)
	}

	var said []alert
	if !eventually(ctx, 4*time.Minute, func() bool {
		said = unwell(ctx, t, conn)
		return len(said) > 0
	}) {
		t.Error("the NAS raised no alert for a pool with a disk out")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)

		if one.Level != "CRITICAL" || !strings.Contains(one.Formatted, layout.Pool) {
			t.Errorf("the alert is %s and does not name the pool, or is not critical: %s", one.Level, one.Formatted)
		}
	}

	back()
	returned = true

	var whole pooled
	if !eventually(ctx, 3*time.Minute, func() bool { whole = pool(ctx, t, conn); return whole.Healthy && whole.Status == "ONLINE" }) {
		t.Errorf("with the disk back the pool reads as %s (healthy %t)", whole.Status, whole.Healthy)
	}
	if !eventually(ctx, 4*time.Minute, func() bool { return len(unwell(ctx, t, conn)) == 0 }) {
		t.Error("the NAS still says the pool is not healthy, with the disk back")
	}
}

// sized is how much of something is used and how much is left, as a NAS says it of a dataset.
type sized struct {
	Used      struct{ Parsed int64 } `json:"used"`
	Available struct{ Parsed int64 } `json:"available"`
	Refquota  struct{ Parsed int64 } `json:"refquota"`
}

func dataset(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name string) sized {
	t.Helper()

	var found []sized
	if err := conn.Call(ctx, "pool.dataset.query", []any{[]any{[]any{"id", "=", name}}}, &found); err != nil || len(found) != 1 {
		t.Fatalf("the dataset %s is not found: %d found, %v", name, len(found), err)
	}

	return found[0]
}

// A dataset with a limit on it fills up, and the NAS's own count of what is used and what is left says how near it is: that count is what a
// check of space reads, since the NAS looks at quotas itself only once an hour. A file that would go over the limit is refused.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestADatasetFilledToItsLimitShowsHowFullItIsAndTakesNoMore(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 15*time.Minute)

	const limit = 1 << 30
	name := layout.Pool + "/full"

	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name, "refquota": limit, "compression": "OFF"}}, nil); err != nil {
		t.Fatalf("making a dataset with a limit: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "pool.dataset.delete", []any{name, map[string]any{"recursive": true, "force": true}}, nil); err != nil {
			t.Errorf("removing the dataset: %v", err)
		}
	})

	empty := dataset(ctx, t, conn, name)
	if empty.Refquota.Parsed != limit || empty.Available.Parsed > limit || empty.Available.Parsed < limit*9/10 {
		t.Errorf("empty, the dataset reads as limited to %d with %d left; want %d and nearly all of it", empty.Refquota.Parsed, empty.Available.Parsed, limit)
	}

	// nothing that could be squeezed, so that what is sent is what is used
	fill := func(file string, size int64) error {
		ledger.count("filesystem.put")

		job, err := conn.Upload(ctx, "filesystem.put", []any{"/mnt/" + name + "/" + file, map[string]any{}}, io.LimitReader(rand.NewChaCha8([32]byte{1}), size))
		if err != nil {
			return err
		}

		return job.Wait(ctx, nil)
	}

	started := time.Now()
	if err := fill("most-of-it", limit*85/100); err != nil {
		t.Fatalf("filling the dataset: %v", err)
	}
	t.Logf("%d MiB went to the NAS in %s", limit*85/100>>20, time.Since(started).Round(time.Second))

	var full sized
	if !eventually(ctx, time.Minute, func() bool { full = dataset(ctx, t, conn, name); return full.Used.Parsed > limit*8/10 }) {
		t.Errorf("filled to 85%%, the dataset reads as %d used", full.Used.Parsed)
	}
	if of := full.Used.Parsed * 100 / limit; of < 80 || of > 90 || full.Available.Parsed > limit*2/10 {
		t.Errorf("filled to 85%%, the dataset reads as %d%% used with %d left", of, full.Available.Parsed)
	}

	err := fill("too-much", limit/2)
	if err == nil {
		t.Fatal("a file that goes over the limit was taken")
	}
	t.Logf("a file over the limit is refused with: %v", err)
}

// A pool with most of its space spoken for says so in what its top dataset has left, though nothing has been written: space kept aside for
// a volume is space nothing else can have. It is the number a check of space has to read, since the pool's own count of what is written does
// not move.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAPoolWithMostOfItsSpaceSpokenForShowsLittleLeft(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 5*time.Minute)

	// what a dataset just removed took is given back a little after it goes, so the pool is let settle before it is measured
	var before sized
	if !eventually(ctx, 2*time.Minute, func() bool {
		before = dataset(ctx, t, conn, layout.Pool)

		return before.Used.Parsed < (before.Used.Parsed+before.Available.Parsed)/20
	}) {
		t.Fatalf("the pool is not nearly empty to start with: %d used, %d left", before.Used.Parsed, before.Available.Parsed)
	}

	const mib = 1 << 20
	all := before.Used.Parsed + before.Available.Parsed
	name, kept := layout.Pool+"/spoken-for", (all*85/100-before.Used.Parsed)/mib*mib

	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name, "type": "VOLUME", "volsize": kept, "sparse": false, "force_size": true}}, nil); err != nil {
		t.Fatalf("keeping %d MiB of the pool aside: %v", kept/mib, err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "pool.dataset.delete", []any{name, map[string]any{"recursive": true, "force": true}}, nil); err != nil {
			t.Errorf("giving the space back: %v", err)
		}
	})

	after := dataset(ctx, t, conn, layout.Pool)
	if of := after.Used.Parsed * 100 / all; of < 80 || of > 92 {
		t.Errorf("with %d MiB kept aside of %d the pool reads as %d%% used (%d used, %d left)", kept/mib, all/mib, of, after.Used.Parsed, after.Available.Parsed)
	}

	// the pool's own count is of what has been written, which is next to nothing: read alone it says the pool is empty
	if written := pool(ctx, t, conn); written.Size == 0 || written.Allocated*100/written.Size > 10 {
		t.Errorf("with space only kept aside, the pool says %d of its %d bytes are written", written.Allocated, written.Size)
	}
}

// A task that copies files somewhere else fails when the somewhere else is not there. The NAS says so three ways, and all three have to be
// read by anything looking for failed tasks: the run itself fails, the task keeps how its last run ended, and an alert is raised that stays
// until somebody dismisses it.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestATaskThatCannotReachWhereItCopiesToIsSaidToHaveFailed(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 8*time.Minute)

	var task struct {
		ID int64 `json:"id"`
	}
	if err := conn.Call(ctx, "rsynctask.create", []any{map[string]any{"path": "/mnt/" + layout.Datasets[0], "user": "root", "mode": "MODULE", "remotehost": "127.0.0.1", "remotemodule": "nothing-listens-here", "direction": "PUSH", "desc": "a copy to nowhere", "enabled": false}}, &task); err != nil {
		t.Fatalf("making the task: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "rsynctask.delete", []any{task.ID}, nil); err != nil {
			t.Errorf("removing the task: %v", err)
		}
	})

	job, err := conn.Start(ctx, "rsynctask.run", []any{task.ID})
	if err != nil {
		t.Fatalf("running the task: %v", err)
	}
	err = job.Wait(ctx, nil)
	if err == nil {
		t.Fatal("a copy to a place that is not there ran without failing")
	}
	t.Logf("the run fails with: %v", err)

	// the failure points at the job's log for the rest, which is fetched like any file, and holds what the copying program itself said
	var log strings.Builder
	if n, err := job.Logs(ctx, &log); err != nil || n == 0 || !strings.Contains(log.String(), "rsync") {
		t.Errorf("the failed run's log is %d bytes that say nothing of rsync (%v): %.300s", n, err, log.String())
	}
	t.Logf("the failed run's log: %.300s", strings.TrimSpace(log.String()))

	var tasks []struct {
		Job *struct {
			State string `json:"state"`
			Error string `json:"error"`
		} `json:"job"`
	}
	if err := conn.Call(ctx, "rsynctask.query", []any{[]any{[]any{"id", "=", task.ID}}}, &tasks); err != nil || len(tasks) != 1 {
		t.Fatalf("the task is not found: %d found, %v", len(tasks), err)
	}
	if tasks[0].Job == nil || tasks[0].Job.State != "FAILED" {
		t.Errorf("the task does not keep that its last run failed: %+v", tasks[0].Job)
	}

	said := alerted(ctx, t, conn, "RsyncFailed")
	if len(said) == 0 {
		t.Fatal("the NAS raised no alert for a task that failed")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}

	// such an alert stays until it is dismissed, and dismissed it is still listed, marked so
	if err := conn.Call(ctx, "alert.dismiss", []any{said[0].UUID}, nil); err != nil {
		t.Fatalf("dismissing the alert: %v", err)
	}
	for _, one := range alerted(ctx, t, conn, "RsyncFailed") {
		if one.UUID == said[0].UUID && !one.Dismissed {
			t.Error("the alert was dismissed and is not marked so")
		}
	}

	// and brought back, it is as it was
	if err := conn.Call(ctx, "alert.restore", []any{said[0].UUID}, nil); err != nil {
		t.Fatalf("bringing the alert back: %v", err)
	}
	for _, one := range alerted(ctx, t, conn, "RsyncFailed") {
		if one.UUID == said[0].UUID && one.Dismissed {
			t.Error("the alert was brought back and is still marked dismissed")
		}
	}

	var raw json.RawMessage
	if err := conn.Call(ctx, "alert.list", nil, &raw); err != nil {
		t.Fatal(err)
	}
	heldTo(t, description(t)["alert.list"], raw, true)
}

// tasked is how a task says its last run went. The description gives this no shape at all, and it is what tells a task that failed from one
// that has not run yet.
type tasked struct {
	ID    int64           `json:"id"`
	State json.RawMessage `json:"state"`
}

func (k tasked) state() (state, why string) {
	var said struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(k.State, &said)

	return said.State, said.Error
}

func task(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, list string, id int64) tasked {
	t.Helper()

	var found []tasked
	if err := conn.Call(ctx, list, []any{[]any{[]any{"id", "=", id}}}, &found); err != nil || len(found) != 1 {
		t.Fatalf("%s: the task is not found: %d found, %v", list, len(found), err)
	}

	return found[0]
}

// A copy of a dataset onto another that already holds files, and shares no snapshot with it, would overwrite them, so a NAS refuses to make
// it. The run fails, the task keeps why, and an alert follows.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAReplicationThatWouldOverwriteWhatIsThereFailsAndIsSaidToHave(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)
	source, target := layout.Datasets[0], layout.Datasets[1]

	ledger.count("filesystem.put")
	in, err := conn.Upload(ctx, "filesystem.put", []any{"/mnt/" + target + "/in-the-way.txt", map[string]any{}}, strings.NewReader("a file a copy would overwrite\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Wait(ctx, nil); err != nil {
		t.Fatalf("putting a file in the way: %v", err)
	}

	if err := conn.Call(ctx, "pool.snapshot.create", []any{map[string]any{"dataset": source, "name": "copied-once"}}, nil); err != nil {
		t.Fatalf("taking a snapshot to copy: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "pool.snapshot.delete", []any{source + "@copied-once"}, nil); err != nil {
			t.Errorf("removing the snapshot: %v", err)
		}
	})

	var made struct {
		ID int64 `json:"id"`
	}
	if err := conn.Call(ctx, "replication.create", []any{map[string]any{"name": "a copy over what is there", "direction": "PUSH", "transport": "LOCAL", "source_datasets": []string{source}, "target_dataset": target, "recursive": false, "auto": false, "retention_policy": "NONE", "name_regex": "copied-.*"}}, &made); err != nil {
		t.Fatalf("making the replication: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "replication.delete", []any{made.ID}, nil); err != nil {
			t.Errorf("removing the replication: %v", err)
		}
	})

	before := task(ctx, t, conn, "replication.query", made.ID)
	t.Logf("before it has run, the task says: %s", before.State)

	job, err := conn.Start(ctx, "replication.run", []any{made.ID})
	if err != nil {
		t.Fatalf("running the replication: %v", err)
	}
	if err = job.Wait(ctx, nil); err == nil {
		t.Fatal("a copy over a dataset that holds files ran without failing")
	}
	t.Logf("the run fails with: %v", err)

	after := task(ctx, t, conn, "replication.query", made.ID)
	t.Logf("after it has failed, the task says: %s", after.State)
	if state, why := after.state(); state != "ERROR" || why == "" {
		t.Errorf("the task says its state is %q and why %q; want ERROR and a reason", state, why)
	}

	var said []alert
	if !eventually(ctx, 4*time.Minute, func() bool { said = alerted(ctx, t, conn, "ReplicationFailed"); return len(said) > 0 }) {
		t.Error("the NAS raised no alert for a replication that failed")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}
}

// A task that takes snapshots is run, takes one, and keeps how the run went: pending before it has ever run, finished after, and failed, with
// why, when it is run again in the same minute and the snapshot it would take is already there. The description gives none of that a shape,
// and a check of tasks reads nothing else. A task that failed is said in an alert as well.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestASnapshotTaskKeepsHowItsLastRunWentAndSaysSoWhenItFails(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 8*time.Minute)
	of := layout.Datasets[0]

	var made struct {
		ID int64 `json:"id"`
	}
	if err := conn.Call(ctx, "pool.snapshottask.create", []any{map[string]any{"dataset": of, "naming_schema": "kept-%Y-%m-%d_%H-%M", "lifetime_value": 1, "lifetime_unit": "HOUR"}}, &made); err != nil {
		t.Fatalf("making the task: %v", err)
	}

	type snapshot struct {
		Name string `json:"name"`
	}
	taken := func(ctx context.Context) []snapshot {
		var all, kept []snapshot
		if err := conn.Call(ctx, "pool.snapshot.query", []any{[]any{[]any{"dataset", "=", of}}}, &all); err != nil {
			t.Fatalf("listing the snapshots: %v", err)
		}
		for _, one := range all {
			if strings.HasPrefix(one.Name, of+"@kept-") {
				kept = append(kept, one)
			}
		}

		return kept
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "pool.snapshottask.delete", []any{made.ID}, nil); err != nil {
			t.Errorf("removing the task: %v", err)
		}
		for _, one := range taken(context.WithoutCancel(ctx)) {
			if err := conn.Call(context.WithoutCancel(ctx), "pool.snapshot.delete", []any{one.Name}, nil); err != nil {
				t.Errorf("removing the snapshot %s: %v", one.Name, err)
			}
		}
	})

	before := task(ctx, t, conn, "pool.snapshottask.query", made.ID)
	if state, _ := before.state(); state != "PENDING" {
		t.Errorf("before it has run, the task says: %s; want it pending", before.State)
	}

	// early in a minute, so that the second run falls in the same one
	if into := time.Now().Second(); into > 40 {
		time.Sleep(time.Duration(62-into) * time.Second)
	}

	if err := conn.Call(ctx, "pool.snapshottask.run", []any{made.ID}, nil); err != nil {
		t.Fatalf("running the task: %v", err)
	}
	if !eventually(ctx, 2*time.Minute, func() bool { return len(taken(ctx)) == 1 }) {
		t.Fatalf("the task was run and took %d snapshots, want 1", len(taken(ctx)))
	}

	var after tasked
	if !eventually(ctx, time.Minute, func() bool {
		after = task(ctx, t, conn, "pool.snapshottask.query", made.ID)
		state, _ := after.state()

		return state == "FINISHED"
	}) {
		t.Errorf("having taken its snapshot, the task says: %s", after.State)
	}
	var finished struct {
		LastSnapshot string `json:"last_snapshot"`
	}
	if err := json.Unmarshal(after.State, &finished); err != nil || finished.LastSnapshot != taken(ctx)[0].Name {
		t.Errorf("the task says the last snapshot it took is %q (%v), and it took %s", finished.LastSnapshot, err, taken(ctx)[0].Name)
	}

	// run again in the same minute, the snapshot it would take is there already
	if err := conn.Call(ctx, "pool.snapshottask.run", []any{made.ID}, nil); err != nil {
		t.Fatalf("running the task again: %v", err)
	}

	var failed tasked
	if !eventually(ctx, time.Minute, func() bool {
		failed = task(ctx, t, conn, "pool.snapshottask.query", made.ID)
		state, _ := failed.state()

		return state == "ERROR"
	}) {
		t.Fatalf("run a second time in the same minute, the task says: %s; want it failed", failed.State)
	}
	if _, why := failed.state(); !strings.Contains(why, "already exists") || len(taken(ctx)) != 1 {
		t.Errorf("the task failed with %q and has %d snapshots; want it to say the snapshot is already there, and one", why, len(taken(ctx)))
	}

	var said []alert
	if !eventually(ctx, 4*time.Minute, func() bool { said = alerted(ctx, t, conn, "SnapshotFailed"); return len(said) > 0 }) {
		t.Error("the NAS raised no alert for a snapshot task that failed")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}

	var raw json.RawMessage
	if err := conn.Call(ctx, "pool.snapshottask.query", nil, &raw); err != nil {
		t.Fatal(err)
	}
	heldTo(t, description(t)["pool.snapshottask.query"], raw, true)
}
