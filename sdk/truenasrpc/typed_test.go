package truenasrpc_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/katbyte/pandorest/client"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	nasclient "github.com/katbyte/truenas-mcp/sdk/truenas/client"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// the connection is everything the generated client asks of one: calls, and files both ways
var _ interface {
	client.Caller
	client.Uploader
	client.Downloader
} = (*truenasrpc.Conn)(nil)

// The client generated from the NAS's description calls through the connection: an answer reads into its types, a moment as a NAS writes one
// included, a refusal reads as pandorest's own kind of error, and a one-row query that matches nothing says nothing is there.
func TestTheGeneratedClientCallsThroughTheConnection(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", json.RawMessage(`{"version": "25.10.7", "hostname": "stand-in", "physmem": 8589934592, "cores": 4, "boottime": {"$date": 1790938328000}, "system_serial": null, "ecc_memory": true}`))
	nas.Refuse("system.reboot", &truenastest.Error{Errno: 13, ErrName: "EACCES", Reason: "Not authorized"})
	nas.Refuse("pool.query", truenastest.ErrNoMatch)

	typed, err := nasclient.New(connect(t, nas))
	if err != nil {
		t.Fatal(err)
	}

	info, err := typed.System.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a := info.Answer; a.Version != "25.10.7" || a.Hostname != "stand-in" || a.Physmem != 8589934592 || a.Cores != 4 || !a.EccMemory || !a.Boottime.Equal(time.UnixMilli(1790938328000)) {
		t.Errorf("system.info read through the typed client as %+v", a)
	}

	_, err = typed.SystemReboot.Info(t.Context())
	if !truenasrpc.IsNoSuchMethod(err) {
		t.Errorf("a method the stand-in has no answer for = %v", err)
	}
	_, err = typed.System.Reboot(t.Context(), "tests", truenas.SystemRebootOptions{})
	var refused *client.CallError
	if !errors.As(err, &refused) || refused.Method != "system.reboot" || refused.ErrName != "EACCES" || refused.Reason != "Not authorized" {
		t.Errorf("a refusal through the typed client = %v, read as %+v", err, refused)
	}

	if _, err = typed.Pool.QueryOne(t.Context(), []any{[]any{"name", "=", "nothing"}}, truenas.PoolQueryOptions{}); !truenasrpc.IsNotFound(err) {
		t.Errorf("a one-row query that matches nothing = %v", err)
	}
}

// A pool's scrub and disk layout, which the description leaves shapeless, read into the records sdk/pandorest gives them: a vdev with its disks
// under it and their counts, and a scrub with its times.
func TestAPoolsScrubAndLayoutReadIntoTypes(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("pool.query", json.RawMessage(`[{"id": 1, "name": "tank", "status": "ONLINE", "healthy": true, "scan": {"function": "SCRUB", "state": "FINISHED", "start_time": {"$date": 1790845202000}, "end_time": {"$date": 1790848802000}, "percentage": 100.0, "errors": 2, "pause": null, "total_secs_left": null}, "autotrim": {"parsed": "off", "rawvalue": "off", "source": "DEFAULT", "value": "off"}, "expand": null, "topology": {"data": [{"name": "mirror-0", "type": "MIRROR", "path": null, "guid": "1", "status": "DEGRADED", "stats": {"read_errors": 0, "checksum_errors": 3, "ops": [0, 1, 2, 0, 0, 0, 0]}, "children": [{"name": "sda1", "type": "DISK", "path": "/dev/sda1", "guid": "2", "status": "ONLINE", "stats": {"checksum_errors": 3}, "children": [], "device": "sda1", "disk": "sda", "unavail_disk": null}, {"name": "3", "type": "DISK", "path": null, "guid": "3", "status": "UNAVAIL", "children": [], "device": null, "disk": null, "unavail_disk": {"identifier": "{serial}x", "name": "sdb", "serial": "x"}}], "unavail_disk": null}], "log": [], "cache": [], "spare": [], "special": [], "dedup": []}}]`))

	typed, err := nasclient.New(connect(t, nas))
	if err != nil {
		t.Fatal(err)
	}
	pools, err := typed.Pool.Query(t.Context(), nil, truenas.PoolQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	pool := pools.Answer[0]
	if s := pool.Scan; s == nil || s.Function != "SCRUB" || s.Errors != 2 || !s.EndTime.Equal(time.UnixMilli(1790848802000)) || pool.Autotrim == nil || pool.Autotrim.Value != "off" {
		t.Errorf("the scrub and trim read as %+v, %+v", pool.Scan, pool.Autotrim)
	}
	mirror := pool.Topology.Data[0]
	if mirror.Type != "MIRROR" || mirror.Status != "DEGRADED" || mirror.Stats.ChecksumErrors != 3 || len(mirror.Stats.Ops) != 7 || len(mirror.Children) != 2 {
		t.Errorf("the mirror reads as %+v", mirror)
	}
	if good, gone := mirror.Children[0], mirror.Children[1]; good.Disk != "sda" || good.Stats.ChecksumErrors != 3 || gone.Status != "UNAVAIL" || gone.UnavailDisk == nil || gone.UnavailDisk.Name != "sdb" {
		t.Errorf("the disks read as %+v and %+v", good, gone)
	}
}

// A task's state and its last run, which the description leaves shapeless, read into the records sdk/pandorest gives them: where a snapshot task or a
// replication stands, and how a copy task's last run ended.
func TestATasksStateAndLastRunReadIntoTypes(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("pool.snapshottask.query", json.RawMessage(`[{"id": 1, "dataset": "tank/a", "enabled": true, "state": {"state": "ERROR", "datetime": {"$date": 1790845202000}, "error": "No space left.", "warnings": ["slow"]}}]`))
	nas.Answer("replication.query", json.RawMessage(`[{"id": 1, "name": "to the shelf", "state": {"state": "HOLD", "datetime": {"$date": 1790845202000}, "reason": "Dataset is locked."}, "job": {"id": 7, "method": "replication.run", "state": "SUCCESS", "time_finished": {"$date": 1790845202000}}}]`))
	nas.Answer("rsynctask.query", json.RawMessage(`[{"id": 1, "desc": "a copy", "job": {"id": 9, "method": "rsynctask.run", "state": "FAILED", "error": "rsync: failed to connect", "time_started": {"$date": 1790845200000}, "time_finished": {"$date": 1790845202000}}}]`))

	typed, err := nasclient.New(connect(t, nas))
	if err != nil {
		t.Fatal(err)
	}

	snapshots, err := typed.PoolSnapshottask.Query(t.Context(), nil, truenas.PoolSnapshottaskQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s := snapshots.Answer[0].State; s == nil || s.State != "ERROR" || s.Error != "No space left." || len(s.Warnings) != 1 || !s.Datetime.Equal(time.UnixMilli(1790845202000)) {
		t.Errorf("the snapshot task's state reads as %+v", s)
	}

	replications, err := typed.Replication.Query(t.Context(), nil, truenas.ReplicationQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r := replications.Answer[0]; r.State == nil || r.State.State != "HOLD" || r.State.Reason != "Dataset is locked." || r.Job == nil || r.Job.State != "SUCCESS" {
		t.Errorf("the replication reads as %+v with %+v", r.State, r.Job)
	}

	copies, err := typed.Rsynctask.Query(t.Context(), nil, truenas.RsynctaskQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if j := copies.Answer[0].Job; j == nil || j.State != "FAILED" || j.Error != "rsync: failed to connect" || j.TimeFinished.Sub(j.TimeStarted.Time) != 2*time.Second {
		t.Errorf("the copy task's last run reads as %+v", j)
	}
}

// A ZFS GUID is an unsigned 64-bit number, written bare, and half of them are larger than a signed one holds: one such reads whole.
func TestAZFSGUIDLargerThanASignedNumberReads(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("zfs.resource.query", json.RawMessage(`[{"name": "tank", "pool": "tank", "type": "FILESYSTEM", "guid": 17562920642453302503, "createtxg": 1}]`))

	typed, err := nasclient.New(connect(t, nas))
	if err != nil {
		t.Fatal(err)
	}
	found, err := typed.ZfsResource.Query(t.Context(), truenas.ZfsResourceQueryData{})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Answer) != 1 || found.Answer[0].Guid != 17562920642453302503 {
		t.Errorf("the dataset reads as %+v", found.Answer)
	}
}
