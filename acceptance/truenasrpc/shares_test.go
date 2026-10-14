//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// running starts one of the NAS's services for as long as a test lasts, and says whether the NAS lists it as running.
func running(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name string) {
	t.Helper()

	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "service.control", []any{"STOP", name}, nil); err != nil {
			t.Errorf("stopping %s: %v", name, err)
		}
	})
	told(ctx, t, conn, "service.control", "START", name)

	var found []service
	if err := conn.Call(ctx, "service.query", []any{[]any{[]any{"service", "=", name}}}, &found); err != nil || len(found) != 1 || found[0].State != "RUNNING" {
		t.Errorf("started, %s reads as %+v (%v)", name, found, err)
	}
}

// field reads one named value of a row a NAS answered, as the JSON it was.
func field(t *testing.T, row json.RawMessage, name string) string {
	t.Helper()

	var values map[string]json.RawMessage
	if err := json.Unmarshal(row, &values); err != nil {
		t.Fatalf("a row that does not read: %.200s: %v", row, err)
	}

	return string(values[name])
}

// A dataset is shared over SMB and another over NFS. Each share is changed and read back, the service that serves it is started with the
// share in place, the share is switched off and then removed. Who may use the SMB share is read and set.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAShareOfEachKindIsMadeChangedServedSwitchedOffAndRemoved(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	// a NAS shares nothing over SMB until it has a user who may use it, which a furnished NAS has
	furnished(t)

	windows, unix := layout.Pool+"/offered-smb", layout.Pool+"/offered-nfs"
	gone(ctx, t, conn, windows, unix)
	for _, dataset := range []string{windows, unix} {
		if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": dataset}}, nil); err != nil {
			t.Fatalf("making %s: %v", dataset, err)
		}
	}

	smb := madeOne(ctx, t, conn, "sharing.smb", map[string]any{"name": "offered", "path": "/mnt/" + windows, "comment": "made by the tests"})
	nfs := madeOne(ctx, t, conn, "sharing.nfs", map[string]any{"path": "/mnt/" + unix, "comment": "made by the tests", "networks": []string{"192.0.2.0/24"}})

	var changed json.RawMessage
	if err := conn.Call(ctx, "sharing.smb.update", []any{smb, map[string]any{"comment": "changed by the tests", "readonly": true}}, &changed); err != nil {
		t.Fatalf("changing the SMB share: %v", err)
	}
	if field(t, changed, "comment") != `"changed by the tests"` || field(t, changed, "readonly") != "true" || field(t, changed, "enabled") != "true" || field(t, changed, "locked") != "false" {
		t.Errorf("changed, the SMB share reads as %s", changed)
	}
	if err := conn.Call(ctx, "sharing.nfs.update", []any{nfs, map[string]any{"ro": true, "maproot_user": "root"}}, &changed); err != nil {
		t.Fatalf("changing the NFS share: %v", err)
	}
	if field(t, changed, "ro") != "true" || field(t, changed, "maproot_user") != `"root"` || field(t, changed, "networks") != `["192.0.2.0/24"]` {
		t.Errorf("changed, the NFS share reads as %s", changed)
	}

	// who may use the SMB share: everyone, until it is said otherwise
	type entry struct {
		May  string `json:"ae_perm"`
		Kind string `json:"ae_type"`
		Who  string `json:"ae_who_sid"`
	}
	var allowed struct {
		Share string  `json:"share_name"`
		ACL   []entry `json:"share_acl"`
	}
	asked(ctx, t, conn, "sharing.smb.getacl", []any{map[string]any{"share_name": "offered"}}, &allowed)
	if len(allowed.ACL) != 1 || allowed.ACL[0].May != "FULL" || allowed.ACL[0].Who != "S-1-1-0" {
		t.Errorf("who may use a new SMB share reads as %+v; want everyone, in full", allowed.ACL)
	}
	if err := conn.Call(ctx, "sharing.smb.setacl", []any{map[string]any{"share_name": "offered", "share_acl": []any{map[string]any{"ae_perm": "READ", "ae_type": "ALLOWED", "ae_who_sid": "S-1-1-0"}}}}, nil); err != nil {
		t.Errorf("saying everyone may only read the SMB share: %v", err)
	}
	asked(ctx, t, conn, "sharing.smb.getacl", []any{map[string]any{"share_name": "offered"}}, &allowed)
	if len(allowed.ACL) != 1 || allowed.ACL[0].May != "READ" {
		t.Errorf("set to read only for everyone, who may use the SMB share reads as %+v", allowed.ACL)
	}

	// each is served, and nobody is using either
	running(ctx, t, conn, "cifs")
	running(ctx, t, conn, "nfs")

	var clients int
	asked(ctx, t, reader(t), "nfs.client_count", nil, &clients)
	if clients != 0 {
		t.Errorf("%d machines are said to be using NFS, with none there", clients)
	}

	for kind, id := range map[string]int64{"sharing.smb": smb, "sharing.nfs": nfs} {
		if err := conn.Call(ctx, kind+".update", []any{id, map[string]any{"enabled": false}}, &changed); err != nil || field(t, changed, "enabled") != "false" {
			t.Errorf("%s: switched off, the share reads as %s (%v)", kind, changed, err)
		}
	}
}

// A volume is offered over iSCSI from end to end: somewhere to listen, who may connect, a target, the volume as what it offers, and the two
// joined. The service is started and lists nobody connected. A name already taken is said to be, and everything is taken apart in order.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAVolumeIsOfferedOverISCSIFromEndToEnd(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	volume := layout.Pool + "/offered-volume"
	gone(ctx, t, conn, volume)
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": volume, "type": "VOLUME", "volsize": 64 << 20, "sparse": true}}, nil); err != nil {
		t.Fatalf("making a volume: %v", err)
	}

	portal := madeOne(ctx, t, conn, "iscsi.portal", map[string]any{"listen": []any{map[string]any{"ip": "0.0.0.0"}}, "comment": "for the volume the tests offer"})
	initiator := madeOne(ctx, t, conn, "iscsi.initiator", map[string]any{"comment": "anyone, for the volume the tests offer"})
	target := madeOne(ctx, t, conn, "iscsi.target", map[string]any{"name": "offered-by-tests", "groups": []any{map[string]any{"portal": portal, "initiator": initiator}}}, true)
	extent := madeOne(ctx, t, conn, "iscsi.extent", map[string]any{"name": "offered-by-tests", "type": "DISK", "disk": "zvol/" + volume}, false, true)
	joined := madeOne(ctx, t, conn, "iscsi.targetextent", map[string]any{"target": target, "extent": extent, "lunid": 0}, true)

	var taken, free *string
	asked(ctx, t, conn, "iscsi.target.validate_name", []any{"offered-by-tests"}, &taken)
	asked(ctx, t, conn, "iscsi.target.validate_name", []any{"nothing-has-this-name"}, &free)
	if taken == nil || free != nil {
		t.Errorf("a name in use is said to be free, or a free one in use: %v, %v", taken, free)
	} else {
		t.Logf("a target's name that is taken is refused with: %s", *taken)
	}

	var row json.RawMessage
	if err := conn.Call(ctx, "iscsi.target.update", []any{target, map[string]any{"alias": "offered by the tests"}}, &row); err != nil || field(t, row, "alias") != `"offered by the tests"` {
		t.Errorf("given another name to go by, the target reads as %s (%v)", row, err)
	}
	if err := conn.Call(ctx, "iscsi.targetextent.update", []any{joined, map[string]any{"lunid": 3}}, &row); err != nil || field(t, row, "lunid") != "3" {
		t.Errorf("moved to another number, the join reads as %s (%v)", row, err)
	}
	asked(ctx, t, conn, "iscsi.extent.get_instance", []any{extent}, &row)
	if field(t, row, "locked") != "false" || field(t, row, "enabled") != "true" || !strings.Contains(field(t, row, "path"), volume) {
		t.Errorf("the volume as the target offers it reads as %s", row)
	}

	running(ctx, t, conn, "iscsitarget")

	var sessions []json.RawMessage
	asked(ctx, t, reader(t), "iscsi.global.sessions", nil, &sessions)
	var clients int
	asked(ctx, t, reader(t), "iscsi.global.client_count", nil, &clients)
	if len(sessions) != 0 || clients != 0 {
		t.Errorf("%d sessions and %d machines are said to be connected, with none there", len(sessions), clients)
	}
}

// A share or a task that rests on a dataset kept under a passphrase stops working when the dataset is locked. The NAS says so in the share's
// or the task's own row, as locked, which is where a check has to read it: a copy task that is run then copies nothing and its run reads as
// one that worked, with only an alert to say otherwise. Unlocked, each reads as it did.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestWhatRestsOnALockedDatasetSaysItIsLocked(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	// a NAS shares nothing over SMB until it has a user who may use it, which a furnished NAS has
	furnished(t)

	name, passphrase := layout.Pool+"/behind-a-lock", "the words that open what is shared"
	gone(ctx, t, conn, name)
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name, "encryption": true, "inherit_encryption": false, "encryption_options": map[string]any{"passphrase": passphrase}}}, nil); err != nil {
		t.Fatalf("making a dataset kept under a passphrase: %v", err)
	}

	resting := map[string]int64{
		"sharing.smb": madeOne(ctx, t, conn, "sharing.smb", map[string]any{"name": "behind-a-lock", "path": "/mnt/" + name}),
		"sharing.nfs": madeOne(ctx, t, conn, "sharing.nfs", map[string]any{"path": "/mnt/" + name}),
		"rsynctask":   madeOne(ctx, t, conn, "rsynctask", map[string]any{"path": "/mnt/" + name, "user": "root", "mode": "MODULE", "remotehost": "127.0.0.1", "remotemodule": "nothing-listens-here", "direction": "PUSH", "desc": "a copy of what is behind a lock", "enabled": false}),
	}
	// read by the user who may only read, as whatever watches a NAS reads it
	only := reader(t)
	locked := func(want string) {
		t.Helper()

		for kind, id := range resting {
			var row json.RawMessage
			asked(ctx, t, only, kind+".get_instance", []any{id}, &row)
			if is := field(t, row, "locked"); is != want {
				t.Errorf("%s: locked reads as %s, want %s", kind, is, want)
			}
		}
	}
	locked("false")

	if err := conn.Call(ctx, "pool.dataset.lock", []any{name}, nil); err != nil {
		t.Fatalf("locking the dataset: %v", err)
	}
	locked("true")

	// run, the copy task does nothing and ends as a run that worked does: only the alert says it did not run
	if err := conn.Call(ctx, "rsynctask.run", []any{resting["rsynctask"]}, nil); err != nil {
		t.Errorf("a copy of a locked dataset, which by the source ends quietly, failed with: %v", err)
	}
	var ran struct {
		Job *struct {
			State string `json:"state"`
		} `json:"job"`
	}
	asked(ctx, t, conn, "rsynctask.get_instance", []any{resting["rsynctask"]}, &ran)
	if ran.Job == nil || ran.Job.State != "SUCCESS" {
		t.Errorf("the copy task's last run reads as %+v; it was seen to read as one that worked, though nothing was copied", ran.Job)
	}

	var said []alert
	if !eventually(ctx, 2*time.Minute, func() bool {
		said = alerted(ctx, t, conn, "TaskLocked")

		return len(said) > 0
	}) {
		t.Error("the NAS raised no alert for a task that could not run because its dataset is locked")
	}
	for _, one := range append(said, alerted(ctx, t, conn, "ShareLocked")...) {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}

	if !unlocked(ctx, t, conn, name, passphrase) {
		t.Fatal("the dataset is not unlocked by its passphrase")
	}
	locked("false")
}
