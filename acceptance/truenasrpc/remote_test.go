//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// The tests here are of what a NAS does with another machine: a copy sent over SSH, a copy sent to storage somewhere else. The test NAS has no
// other machine, so it is its own: its SSH server is started and it is set up to reach itself, the way it would be set up to reach another
// NAS. Stopping that server is then the other end going away, which is how these copies fail in life.

// service is one of the things a NAS runs for others, as it lists it.
type service struct {
	ID      int64  `json:"id"`
	Service string `json:"service"`
	Enable  bool   `json:"enable"`
	State   string `json:"state"`
	Pids    []int  `json:"pids"`
}

// sshServer is the NAS's SSH server, as whoever asks is told of it.
func sshServer(ctx context.Context, t *testing.T, conn *truenasrpc.Conn) service {
	t.Helper()

	var found []service
	if err := conn.Call(ctx, "service.query", []any{[]any{[]any{"service", "=", "ssh"}}}, &found); err != nil || len(found) != 1 {
		t.Fatalf("looking for the SSH server among the services: %d found, %v", len(found), err)
	}

	return found[0]
}

// told tells a service what to do by one of the calls there are for it, and fails the test unless the NAS says it did.
func told(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, method string, params ...any) {
	t.Helper()

	var did bool
	if err := conn.Call(ctx, method, params, &did); err != nil || !did {
		t.Fatalf("%s %v: answered %t, %v", method, params, did, err)
	}
}

// sshOff leaves the NAS's SSH server as a NAS has it to start with, stopped and not set to start, when the test ends.
func sshOff(ctx context.Context, t *testing.T, conn *truenasrpc.Conn) {
	t.Helper()

	t.Cleanup(func() {
		after := context.WithoutCancel(ctx)

		if err := conn.Call(after, "service.control", []any{"STOP", "ssh"}, nil); err != nil {
			t.Errorf("stopping the SSH server: %v", err)
		}
		if err := conn.Call(after, "service.update", []any{"ssh", map[string]any{"enable": false}}, nil); err != nil {
			t.Errorf("setting the SSH server not to start with the NAS: %v", err)
		}
	})
}

// A service is set to start with the NAS, started, restarted, told to read its settings again and stopped, by each of the calls there are for
// it, and what the NAS lists says so each time, to the user who may only read as well. A service that is set to start and is not running is
// what a check of services looks for, and is how the first step leaves it. A service the NAS has never heard of is not started.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAServiceIsStartedStoppedAndSetToStartWithTheNAS(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 8*time.Minute)

	if before := sshServer(ctx, t, conn); before.Enable || before.State != "STOPPED" {
		t.Fatalf("the SSH server is not as a NAS has it to start with: %+v", before)
	}
	sshOff(ctx, t, conn)

	if err := conn.Call(ctx, "service.update", []any{"ssh", map[string]any{"enable": true}}, nil); err != nil {
		t.Fatalf("setting the SSH server to start with the NAS: %v", err)
	}
	if set := sshServer(ctx, t, reader(t)); !set.Enable || set.State != "STOPPED" || len(set.Pids) != 0 {
		t.Errorf("set to start with the NAS and not yet started, the SSH server reads as %+v", set)
	}

	told(ctx, t, conn, "service.control", "START", "ssh")
	running := sshServer(ctx, t, reader(t))
	if running.State != "RUNNING" || len(running.Pids) == 0 {
		t.Errorf("started, the SSH server reads as %+v", running)
	}

	told(ctx, t, conn, "service.reload", "ssh")
	told(ctx, t, conn, "service.restart", "ssh")
	if again := sshServer(ctx, t, conn); again.State != "RUNNING" || len(again.Pids) == 0 || slices.Equal(again.Pids, running.Pids) {
		t.Errorf("restarted, the SSH server reads as %+v, and before as %+v; want it running as another process", again, running)
	}

	told(ctx, t, conn, "service.stop", "ssh")
	if stopped := sshServer(ctx, t, conn); stopped.State != "STOPPED" || len(stopped.Pids) != 0 {
		t.Errorf("stopped, the SSH server reads as %+v", stopped)
	}
	told(ctx, t, conn, "service.start", "ssh")
	told(ctx, t, conn, "service.control", "RESTART", "ssh")
	told(ctx, t, conn, "service.control", "RELOAD", "ssh")
	told(ctx, t, conn, "service.control", "STOP", "ssh")
	if stopped := sshServer(ctx, t, conn); stopped.State != "STOPPED" {
		t.Errorf("stopped a second time, the SSH server reads as %+v", stopped)
	}

	// the user who may only read may not start anything
	if err := reader(t).Call(ctx, "service.control", []any{"START", "ssh"}, nil); !truenasrpc.IsNotAllowed(err) {
		t.Errorf("the user who may only read starts a service, or is refused some other way: %v", err)
	}

	var did bool
	err := conn.Call(ctx, "service.control", []any{"START", "nothing-by-this-name"}, &did)
	if err == nil && did {
		t.Error("a service the NAS has never heard of is said to have started")
	}
	t.Logf("starting a service the NAS has never heard of answers %t, %v", did, err)
}

// reachesItself sets the NAS up to reach itself over SSH the way it is set up to reach another NAS: a key is made and kept, and the NAS logs
// into the other one, itself here, to have the key let in. It answers the kept key and the kept connection, and leaves the SSH server
// running, which the setting up does unasked. Everything is taken back when the test ends.
func reachesItself(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name string) (key, link int64) {
	t.Helper()

	// whoever sets this up is let in as root, whose list of keys that may log in is put back as it was
	var root []struct {
		ID   int64   `json:"id"`
		Keys *string `json:"sshpubkey"`
	}
	if err := conn.Call(ctx, "user.query", []any{[]any{[]any{"username", "=", "root"}}}, &root); err != nil || len(root) != 1 {
		t.Fatalf("looking up root: %d found, %v", len(root), err)
	}
	sshOff(ctx, t, conn)
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "user.update", []any{root[0].ID, map[string]any{"sshpubkey": root[0].Keys}}, nil); err != nil {
			t.Errorf("putting root's keys back as they were: %v", err)
		}
	})

	var pair struct {
		Private string `json:"private_key"`
		Public  string `json:"public_key"`
	}
	if err := conn.Call(ctx, "keychaincredential.generate_ssh_key_pair", nil, &pair); err != nil || !strings.Contains(pair.Private, "PRIVATE KEY") || !strings.HasPrefix(pair.Public, "ssh-") {
		t.Fatalf("having the NAS make a key: %v, and a public half that reads %.20q", err, pair.Public)
	}
	// from here on no line of trace may hold the key, wherever the NAS says it again
	if lines := strings.Split(strings.TrimSpace(pair.Private), "\n"); len(lines) > 3 {
		ledger.hide(lines[1])
		ledger.hide(lines[len(lines)-2])
	}
	ledger.hide(nas.password)

	key = madeOne(ctx, t, conn, "keychaincredential", map[string]any{"name": name + ", its key", "type": "SSH_KEY_PAIR", "attributes": map[string]any{"private_key": pair.Private, "public_key": pair.Public}}, map[string]any{"cascade": true})

	var made struct {
		ID         int64 `json:"id"`
		Attributes struct {
			Host    string `json:"host"`
			Port    int    `json:"port"`
			User    string `json:"username"`
			Key     int64  `json:"private_key"`
			HostKey string `json:"remote_host_key"`
		} `json:"attributes"`
	}
	setup := map[string]any{"private_key": map[string]any{"generate_key": false, "existing_key_id": key}, "connection_name": name, "setup_type": "SEMI-AUTOMATIC", "semi_automatic_setup": map[string]any{"url": "https://127.0.0.1", "verify_ssl": false, "admin_username": nas.username, "password": nas.password, "username": "root", "connect_timeout": 10}}
	if err := conn.Call(ctx, "keychaincredential.setup_ssh_connection", []any{setup}, &made); err != nil {
		t.Fatalf("setting the NAS up to reach itself over SSH: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "keychaincredential.delete", []any{made.ID, map[string]any{"cascade": true}}, nil); err != nil && !truenasrpc.IsNotFound(err) {
			t.Errorf("removing the connection: %v", err)
		}
	})
	if made.Attributes.Host != "127.0.0.1" || made.Attributes.Port != 22 || made.Attributes.User != "root" || made.Attributes.Key != key || !strings.Contains(made.Attributes.HostKey, "ssh-") {
		t.Errorf("the connection is kept as %s@%s:%d with key %d and a record of the other end that reads %.30q", made.Attributes.User, made.Attributes.Host, made.Attributes.Port, made.Attributes.Key, made.Attributes.HostKey)
	}

	// the kept key and connection are read back, which says both again
	var kept []json.RawMessage
	asked(ctx, t, conn, "keychaincredential.query", []any{[]any{[]any{"id", "in", []int64{key, made.ID}}}}, &kept)
	if len(kept) != 2 {
		t.Errorf("%d of the key and the connection are kept, want both", len(kept))
	}

	// setting it up started the other end's SSH server and set it to start with the NAS, without being asked to
	if on := sshServer(ctx, t, conn); on.State != "RUNNING" || !on.Enable {
		t.Errorf("with a NAS set up to be reached over SSH, its SSH server reads as %+v; it was seen started and set to start", on)
	}

	return key, made.ID
}

// alertsNaming is what the NAS says is wrong now, of one class, that names something.
func alertsNaming(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, class, names string) []alert {
	t.Helper()

	var of []alert
	for _, one := range alerted(ctx, t, conn, class) {
		if strings.Contains(one.Formatted, names) {
			of = append(of, one)
		}
	}

	return of
}

// nextMinute waits for the next minute to start: snapshots are named by the minute, so two cannot be taken in one.
func nextMinute(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(62-time.Now().Second()) * time.Second):
	}
}

// A replication to another NAS goes over SSH. It copies a dataset, and then, with the other end gone, the next run fails: the task says why
// and an alert follows. With the other end back the run after that copies what the failed one could not, and the alert goes. Around it, what
// the other end holds is listed, a dataset is made on it, and the kept connection says what uses it.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAReplicationOverSSHCopiesAndSaysSoWhenTheOtherEndIsGone(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 20*time.Minute)

	const name, schema = "a copy sent over SSH", "sent-%Y-%m-%d_%H-%M"
	source, received := layout.Pool+"/sent", layout.Pool+"/received"
	target := received + "/sent"
	gone(ctx, t, conn, source, received)

	_, link := reachesItself(ctx, t, conn, "the NAS itself, for a replication")

	var seen string
	asked(ctx, t, conn, "keychaincredential.remote_ssh_host_key_scan", []any{map[string]any{"host": "127.0.0.1", "port": 22, "connect_timeout": 10}}, &seen)
	if !strings.Contains(seen, "ssh-ed25519") {
		t.Errorf("asked what the other end proves itself with, the NAS answers %.60q", seen)
	}

	for _, dataset := range []string{source, received} {
		if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": dataset}}, nil); err != nil {
			t.Fatalf("making %s: %v", dataset, err)
		}
	}
	put(ctx, t, conn, "/mnt/"+source+"/note.txt", "sent the first day\n")

	snapshots := madeOne(ctx, t, conn, "pool.snapshottask", map[string]any{"dataset": source, "naming_schema": schema, "lifetime_value": 1, "lifetime_unit": "HOUR"})
	copying := madeOne(ctx, t, conn, "replication", map[string]any{"name": name, "direction": "PUSH", "transport": "SSH", "ssh_credentials": link, "source_datasets": []string{source}, "target_dataset": target, "recursive": false, "auto": false, "also_include_naming_schema": []string{schema}, "retention_policy": "NONE", "readonly": "SET", "retries": 1})

	// what the other end holds is read over the connection, and a dataset is made there
	var there []string
	asked(ctx, t, conn, "replication.list_datasets", []any{"SSH", link}, &there)
	if !slices.Contains(there, received) {
		t.Errorf("the other end is said to hold %v, which lacks %s", there, received)
	}
	if err := conn.Call(ctx, "replication.create_dataset", []any{received + "/made-from-afar", "SSH", link}, nil); err != nil {
		t.Errorf("making a dataset on the other end: %v", err)
	}
	one(ctx, t, conn, received+"/made-from-afar")

	var uses []struct {
		Title  string `json:"title"`
		Unbind string `json:"unbind_method"`
	}
	asked(ctx, t, conn, "keychaincredential.used_by", []any{link}, &uses)
	if len(uses) != 1 || !strings.Contains(uses[0].Title, name) {
		t.Errorf("the connection is said to be used by %+v, want the one replication", uses)
	}

	taken := 0
	snapshot := func(holds string) {
		put(ctx, t, conn, "/mnt/"+source+"/note.txt", holds)

		taken++
		if err := conn.Call(ctx, "pool.snapshottask.run", []any{snapshots}, nil); err != nil {
			t.Fatalf("taking a snapshot: %v", err)
		}
		if !eventually(ctx, 2*time.Minute, func() bool { return len(snapshotsOf(ctx, t, conn, source)) == taken }) {
			t.Fatalf("the dataset has %d snapshots, want %d", len(snapshotsOf(ctx, t, conn, source)), taken)
		}
	}
	copied := func(holds string) {
		if err := conn.Call(ctx, "replication.run", []any{copying}, nil); err != nil {
			t.Fatalf("sending the copy: %v", err)
		}
		if state, why := task(ctx, t, conn, "replication.query", copying).state(); state != "FINISHED" {
			t.Errorf("the replication says %s (%s) after a run that worked", state, why)
		}
		if has := got(ctx, t, conn, "/mnt/"+target+"/note.txt"); has != holds {
			t.Errorf("the copy holds %q, want %q", has, holds)
		}
	}

	snapshot("sent the first day\n")
	copied("sent the first day\n")

	// the other end goes away
	told(ctx, t, conn, "service.control", "STOP", "ssh")
	nextMinute(ctx)
	snapshot("sent the second day\n")

	err := conn.Call(ctx, "replication.run", []any{copying}, nil)
	if err == nil {
		t.Fatal("a replication to an end that is not there ran without failing")
	}
	t.Logf("with the other end gone the run fails with: %v", err)

	state, why := task(ctx, t, conn, "replication.query", copying).state()
	if state != "ERROR" || why == "" {
		t.Errorf("after a run that could not reach the other end the replication says %s (%q), want ERROR and why", state, why)
	}
	t.Logf("the task says %s: %s", state, why)

	var said []alert
	if !eventually(ctx, 3*time.Minute, func() bool {
		said = alertsNaming(ctx, t, conn, "ReplicationFailed", name)

		return len(said) > 0
	}) {
		t.Error("the NAS raised no alert for a replication that could not reach the other end")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}
	if has := got(ctx, t, conn, "/mnt/"+target+"/note.txt"); has != "sent the first day\n" {
		t.Errorf("after the run that failed the copy holds %q, want what it held before", has)
	}

	// and comes back
	told(ctx, t, conn, "service.control", "START", "ssh")
	copied("sent the second day\n")
	if !eventually(ctx, 3*time.Minute, func() bool { return len(alertsNaming(ctx, t, conn, "ReplicationFailed", name)) == 0 }) {
		t.Error("the NAS still says the replication failed, after a run that worked")
	}
}

// A cloud sync copies a directory to storage somewhere else, here the NAS itself over SFTP. A run copies what is there and the task says it
// went well. With the storage gone the next run fails: the task says so, the account is said to be no good, and an alert follows. With it
// back, the task that would bring the copy home is made and run. Around it, what the storage holds is listed and a copy is made once.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestACloudSyncCopiesAndSaysSoWhenTheStorageIsGone(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 20*time.Minute)

	from, to, home := layout.Pool+"/synced", layout.Pool+"/storage", layout.Pool+"/brought-home"
	gone(ctx, t, conn, from, to, home)
	for _, dataset := range []string{from, to, home} {
		if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": dataset}}, nil); err != nil {
			t.Fatalf("making %s: %v", dataset, err)
		}
	}
	put(ctx, t, conn, "/mnt/"+from+"/note.txt", "kept somewhere else too\n")

	key, _ := reachesItself(ctx, t, conn, "the NAS itself, for a cloud sync")
	provider := map[string]any{"type": "SFTP", "host": "127.0.0.1", "port": 22, "user": "root", "private_key": key}
	account := madeOne(ctx, t, conn, "cloudsync.credentials", map[string]any{"name": "storage reached over SFTP", "provider": provider})

	var good struct {
		Valid bool    `json:"valid"`
		Error *string `json:"error"`
	}
	asked(ctx, t, conn, "cloudsync.credentials.verify", []any{provider}, &good)
	if !good.Valid {
		t.Errorf("the account is said to be no good: %v", good.Error)
	}

	there := map[string]any{"folder": "/mnt/" + to + "/kept"}
	syncing := madeOne(ctx, t, conn, "cloudsync", map[string]any{"description": "a copy kept somewhere else", "path": "/mnt/" + from, "credentials": account, "attributes": there, "direction": "PUSH", "transfer_mode": "COPY", "enabled": false})

	// how the last run of a cloud sync went is in the job it keeps
	ran := func() string {
		var found []struct {
			Job *struct {
				State string `json:"state"`
			} `json:"job"`
		}
		asked(ctx, t, conn, "cloudsync.query", []any{[]any{[]any{"id", "=", syncing}}}, &found)
		if len(found) != 1 || found[0].Job == nil {
			return "never"
		}

		return found[0].Job.State
	}
	if state := ran(); state != "never" {
		t.Errorf("a cloud sync that has not run says its last run was %s", state)
	}

	if err := conn.Call(ctx, "cloudsync.sync", []any{syncing, map[string]any{"dry_run": true}}, nil); err != nil {
		t.Errorf("trying the copy without making it: %v", err)
	}
	if err := conn.Call(ctx, "cloudsync.sync", []any{syncing}, nil); err != nil {
		t.Fatalf("making the copy: %v", err)
	}
	if has := got(ctx, t, conn, "/mnt/"+to+"/kept/note.txt"); has != "kept somewhere else too\n" {
		t.Errorf("the storage holds %q", has)
	}
	if state := ran(); state != "SUCCESS" {
		t.Errorf("after a run that worked the cloud sync says its last run was %s", state)
	}

	var holds []struct {
		Name  string `json:"Name"`
		IsDir bool   `json:"IsDir"`
	}
	asked(ctx, t, conn, "cloudsync.list_directory", []any{map[string]any{"credentials": account, "attributes": there}}, &holds)
	if len(holds) != 1 || holds[0].Name != "note.txt" || holds[0].IsDir {
		t.Errorf("the storage is said to hold %+v, want the one file", holds)
	}

	if err := conn.Call(ctx, "cloudsync.sync_onetime", []any{map[string]any{"path": "/mnt/" + from, "credentials": account, "attributes": map[string]any{"folder": "/mnt/" + to + "/once"}, "direction": "PUSH", "transfer_mode": "COPY"}}, nil); err != nil {
		t.Errorf("making a copy once, with no task kept for it: %v", err)
	} else if has := got(ctx, t, conn, "/mnt/"+to+"/once/note.txt"); has != "kept somewhere else too\n" {
		t.Errorf("the copy made once holds %q", has)
	}

	var changed struct {
		Description string `json:"description"`
	}
	if err := conn.Call(ctx, "cloudsync.update", []any{syncing, map[string]any{"description": "a copy kept somewhere else, renamed"}}, &changed); err != nil || changed.Description != "a copy kept somewhere else, renamed" {
		t.Errorf("renamed, the cloud sync reads as %q (%v)", changed.Description, err)
	}

	// the storage goes away
	told(ctx, t, conn, "service.control", "STOP", "ssh")
	put(ctx, t, conn, "/mnt/"+from+"/later.txt", "written with the storage gone\n")

	err := conn.Call(ctx, "cloudsync.sync", []any{syncing}, nil)
	if err == nil {
		t.Fatal("a cloud sync to storage that is not there ran without failing")
	}
	t.Logf("with the storage gone the run fails with: %v", err)
	if state := ran(); state != "FAILED" {
		t.Errorf("after a run that could not reach the storage the cloud sync says its last run was %s, want FAILED", state)
	}

	var bad struct {
		Valid bool    `json:"valid"`
		Error *string `json:"error"`
	}
	asked(ctx, t, conn, "cloudsync.credentials.verify", []any{provider}, &bad)
	if bad.Valid || bad.Error == nil {
		t.Error("with the storage gone the account is still said to be good")
	} else {
		t.Logf("the account is said to be no good: %s", *bad.Error)
	}

	var said []alert
	if !eventually(ctx, 3*time.Minute, func() bool {
		said = alerted(ctx, t, conn, "CloudSyncTaskFailed")

		return len(said) > 0
	}) {
		t.Error("the NAS raised no alert for a cloud sync that could not reach its storage")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}

	// and comes back: the run after copies what the failed one could not, and the alert goes
	told(ctx, t, conn, "service.control", "START", "ssh")
	if err := conn.Call(ctx, "cloudsync.sync", []any{syncing}, nil); err != nil {
		t.Fatalf("making the copy with the storage back: %v", err)
	}
	if has := got(ctx, t, conn, "/mnt/"+to+"/kept/later.txt"); has != "written with the storage gone\n" {
		t.Errorf("with the storage back, it holds %q of what was written while it was gone", has)
	}
	if !eventually(ctx, 3*time.Minute, func() bool { return len(alerted(ctx, t, conn, "CloudSyncTaskFailed")) == 0 }) {
		t.Error("the NAS still says the cloud sync failed, after a run that worked")
	}

	// the task that would bring the copy home
	var back struct {
		ID        int64  `json:"id"`
		Direction string `json:"direction"`
		Path      string `json:"path"`
	}
	if err := conn.Call(ctx, "cloudsync.restore", []any{syncing, map[string]any{"description": "the copy brought home", "transfer_mode": "COPY", "path": "/mnt/" + home}}, &back); err != nil {
		t.Fatalf("making the task that brings the copy home: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "cloudsync.delete", []any{back.ID}, nil); err != nil {
			t.Errorf("removing the task that brings the copy home: %v", err)
		}
	})
	if back.Direction != "PULL" || back.Path != "/mnt/"+home {
		t.Errorf("the task that brings the copy home reads as a %s to %s", back.Direction, back.Path)
	}
	if err := conn.Call(ctx, "cloudsync.sync", []any{back.ID}, nil); err != nil {
		t.Fatalf("bringing the copy home: %v", err)
	}
	if has := got(ctx, t, conn, "/mnt/"+home+"/later.txt"); has != "written with the storage gone\n" {
		t.Errorf("brought home, the copy holds %q", has)
	}
}

// Two secrets a NAS does not treat as secret: the private keys its SSH server proves itself with, and the key a replication encrypts what it
// copies with. A NAS answers a secret it knows as one with a row of stars to anyone who is not a full administrator, and these it hands to a
// key that may only read. The connection hides them in what it traces all the same: every connection of the run is traced, and from the
// moment each is known here a line of trace that holds one fails the run.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestWhatANASDoesNotKeepSecretIsHandedToAKeyThatMayOnlyReadAndNeverTraced(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 8*time.Minute)

	// a NAS makes its SSH server's keys when the server first starts
	sshOff(ctx, t, conn)
	told(ctx, t, conn, "service.control", "START", "ssh")

	var settings map[string]json.RawMessage
	asked(ctx, t, reader(t), "ssh.config", nil, &settings)

	handed := 0
	for _, name := range []string{"privatekey", "host_key", "host_dsa_key", "host_ecdsa_key", "host_ed25519_key", "host_rsa_key"} {
		// a key the NAS does not have is answered as nothing, which reads here as no text
		var value string
		if err := json.Unmarshal(settings[name], &value); err != nil && string(settings[name]) != "null" {
			t.Errorf("%s does not read as text: %.40s", name, settings[name])

			continue
		}

		switch value {
		case "":
			t.Logf("%s: the NAS has none", name)
		case truenasrpc.HiddenSecret:
			t.Logf("%s: kept back from a key that may only read", name)
		default:
			handed++
			t.Logf("%s: handed to a key that may only read, %d characters of it", name, len(value))

			// a part from the middle, which no other key shares
			if len(value) > 160 {
				ledger.hide(value[100:160])
			}
		}
	}
	if handed == 0 {
		t.Error("the NAS handed none of its SSH server's private keys to a key that may only read: if it keeps them back now, they belong in the list of what it marks secret and not the list of what it does not")
	}

	// asked again by both users, now that the run knows the keys
	asked(ctx, t, reader(t), "ssh.config", nil, nil)
	asked(ctx, t, conn, "ssh.config", nil, nil)

	passphrase := marked("replication-passphrase")
	copying := madeOne(ctx, t, conn, "replication", map[string]any{"name": "a copy kept under a key", "direction": "PUSH", "transport": "LOCAL", "source_datasets": []string{layout.Datasets[0]}, "target_dataset": layout.Pool + "/kept-under-a-key", "recursive": false, "auto": false, "retention_policy": "NONE", "also_include_naming_schema": []string{"never-%Y-%m-%d_%H-%M"}, "encryption": true, "encryption_inherit": false, "encryption_key": passphrase, "encryption_key_format": "PASSPHRASE", "encryption_key_location": "$TrueNAS"})

	for who, as := range map[string]*truenasrpc.Conn{"the administrator": conn, "the user who may only read": reader(t)} {
		var found []struct {
			Key *string `json:"encryption_key"`
		}
		asked(ctx, t, as, "replication.query", []any{[]any{[]any{"id", "=", copying}}}, &found)

		switch {
		case len(found) != 1 || found[0].Key == nil:
			t.Errorf("asked by %s, the replication has no key to read", who)
		case *found[0].Key == truenasrpc.HiddenSecret:
			t.Errorf("asked by %s, the replication's key is kept back: if a NAS keeps it back now, it belongs in the list of what it marks secret", who)
		case *found[0].Key != passphrase:
			t.Errorf("asked by %s, the replication's key reads back as something other than it was given", who)
		default:
			t.Logf("asked by %s, the replication's key is handed over as it was given", who)
		}
	}
}
