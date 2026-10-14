//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// A NAS is restarted, as an update or a power cut restarts one, with the run's connections open to it. Nothing is done to them: each has to
// find the NAS gone, wait, and be logged in again by itself when it is back, and whatever was listening has to be told it missed something.
// What is different after a restart is then read, since it is what a check finds the morning after: a service set to start is running, the
// pool is whole, and a dataset kept under a passphrase is locked until someone gives the passphrase again. It runs late in the run, by its
// file's name, because a restart forgets every job the tests before it ran.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestARestartIsRiddenOutAndWhatIsDifferentAfterItIsRead(t *testing.T) {
	conn, only := admin(t), reader(t)
	ctx := within(t, 25*time.Minute)

	furnished(t)

	name, passphrase, note := layout.Pool+"/locked-by-a-restart", "the words given again after a restart", "/mnt/"+layout.Pool+"/locked-by-a-restart/note.txt"
	gone(ctx, t, conn, name)
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name, "encryption": true, "inherit_encryption": false, "encryption_options": map[string]any{"passphrase": passphrase}}}, nil); err != nil {
		t.Fatalf("making a dataset kept under a passphrase: %v", err)
	}
	put(ctx, t, conn, note, "written before the restart\n")

	sshOff(ctx, t, conn)
	if err := conn.Call(ctx, "service.update", []any{"ssh", map[string]any{"enable": true}}, nil); err != nil {
		t.Fatalf("setting the SSH server to start with the NAS: %v", err)
	}

	// each call is given a short while of its own, so that a NAS that is not there is found out and not waited on
	answered := func(as *truenasrpc.Conn, method string, into any) error {
		short, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		return as.Call(short, method, nil, into)
	}

	var before string
	if err := answered(conn, "system.boot_id", &before); err != nil || before == "" {
		t.Fatalf("asking which start of the NAS this is: %q, %v", before, err)
	}

	var heard struct {
		mu      sync.Mutex
		resumed int
	}
	listening, err := conn.Subscribe(ctx, "alert.list", func(e truenasrpc.Event) {
		if e.Kind == truenasrpc.Resumed {
			heard.mu.Lock()
			heard.resumed++
			heard.mu.Unlock()
		}
	})
	if err != nil {
		t.Fatalf("listening to what the NAS says is wrong: %v", err)
	}
	defer func() { _ = listening.Close() }()

	started := time.Now()
	restart, err := conn.Start(ctx, "system.reboot", []any{"restarted by the tests"})
	if err != nil {
		t.Fatalf("telling the NAS to restart: %v", err)
	}

	if !eventually(ctx, 4*time.Minute, func() bool {
		var now string

		return answered(conn, "system.boot_id", &now) != nil || now != before
	}) {
		t.Fatal("told to restart, the NAS went on answering as before")
	}
	t.Logf("the NAS stopped answering %s after it was told to restart", time.Since(started).Round(time.Second))

	// the same connection answers again, by itself, from a NAS that has started afresh and says it is ready
	var after, state string
	if !eventually(ctx, 15*time.Minute, func() bool {
		if answered(conn, "system.boot_id", &after) != nil || after == before {
			return false
		}

		return answered(conn, "system.state", &state) == nil && state == "READY"
	}) {
		t.Fatalf("the NAS never came back ready on the connection that was open to it: start %q then %q, state %q", before, after, state)
	}
	t.Logf("the NAS was back and ready %s after it was told to restart", time.Since(started).Round(time.Second))

	if err := answered(only, "system.info", nil); err != nil {
		t.Errorf("the connection of the user who may only read did not come back by itself: %v", err)
	}

	// what was listening is told there was a gap, and a job from before the restart is not one the NAS knows any more
	if !eventually(ctx, time.Minute, func() bool {
		heard.mu.Lock()
		defer heard.mu.Unlock()

		return heard.resumed > 0
	}) {
		t.Error("what was listening through the restart was not told it had missed anything")
	}
	t.Logf("waited for, the job that restarted the NAS says: %v", restart.Wait(ctx, nil))

	var whole pooled
	if !eventually(ctx, 3*time.Minute, func() bool { whole = pool(ctx, t, conn); return whole.Healthy && whole.Status == "ONLINE" }) {
		t.Errorf("after the restart the pool reads as %s, healthy %t", whole.Status, whole.Healthy)
	}

	var ssh service
	if !eventually(ctx, 3*time.Minute, func() bool { ssh = sshServer(ctx, t, only); return ssh.State == "RUNNING" }) {
		t.Errorf("set to start with the NAS, the SSH server reads as %+v after a restart", ssh)
	}

	// a dataset kept under a passphrase comes back locked, and what it holds is out of reach until the passphrase is given again
	if now := one(ctx, t, conn, name); !now.Locked || now.KeyLoaded {
		t.Errorf("after the restart the dataset kept under a passphrase reads as locked %t with its key loaded %t; want it locked", now.Locked, now.KeyLoaded)
	}
	if !unlocked(ctx, t, conn, name, passphrase) {
		t.Fatal("the dataset is not unlocked by its passphrase after the restart")
	}
	if holds := got(ctx, t, conn, note); holds != "written before the restart\n" {
		t.Errorf("unlocked after the restart, the dataset holds %q", holds)
	}

	// what the furnished NAS's tasks say of their last runs, and what the NAS says is wrong, the morning after: written down and not held
	for list, by := range map[string][2]string{"pool.snapshottask.query": {"dataset", layout.TaskDataset}, "replication.query": {"name", layout.Replication}} {
		state, why := furnishedTask(ctx, t, conn, list, by[0], by[1]).state()
		t.Logf("%s: the furnished task says %s (%s) after the restart", list, state, why)
	}
	var copying []struct {
		Job json.RawMessage `json:"job"`
	}
	if err := conn.Call(ctx, "rsynctask.query", []any{[]any{[]any{"desc", "=", layout.CopyTask}}}, &copying); err != nil || len(copying) != 1 {
		t.Errorf("the furnished copy task is not found after the restart: %d found, %v", len(copying), err)
	} else {
		t.Logf("the furnished copy task, which failed before the restart, keeps of its last run: %.200s", copying[0].Job)
	}

	var said []alert
	if err := conn.Call(ctx, "alert.list", nil, &said); err != nil {
		t.Fatalf("listing what the NAS says is wrong after the restart: %v", err)
	}
	for _, one := range said {
		t.Logf("after the restart the NAS says (%s, %s): %s", one.Level, one.Class, one.Formatted)
	}
}
