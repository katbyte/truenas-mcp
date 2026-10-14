//go:build acceptance

package truenasrpc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/acceptance/s3stand"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// bucketed starts storage for the NAS to copy to, on the machine the tests run on, and makes an account for it on the NAS. A NAS in a virtual
// machine reaches the machine it runs on at an address of its own, 10.0.2.2 unless TRUENAS_TEST_HOST says otherwise. What the NAS is said to
// have copied is then read from the storage itself, and not only from what the NAS says of it.
func bucketed(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name string) (storage *s3stand.Server, account int64) {
	t.Helper()

	storage, err := s3stand.Start()
	if err != nil {
		t.Fatalf("starting storage for the NAS to copy to: %v", err)
	}
	t.Cleanup(storage.Close)

	host := os.Getenv("TRUENAS_TEST_HOST")
	if host == "" {
		host = "10.0.2.2"
	}

	provider := map[string]any{"type": "S3", "access_key_id": marked("access-key"), "secret_access_key": marked("secret-key"), "endpoint": "http://" + net.JoinHostPort(host, strconv.Itoa(storage.Port())), "region": "us-east-1"}
	account = madeOne(ctx, t, conn, "cloudsync.credentials", map[string]any{"name": name, "provider": provider})

	var good struct {
		Valid bool    `json:"valid"`
		Error *string `json:"error"`
	}
	asked(ctx, t, conn, "cloudsync.credentials.verify", []any{provider}, &good)
	if !good.Valid || storage.Asked() == 0 {
		t.Fatalf("the NAS says the account is good: %t (%v), having asked the storage %d times: can it reach %s?", good.Valid, good.Error, storage.Asked(), provider["endpoint"])
	}

	return storage, account
}

// lastRun is how the last run of a cloud task went, by the job its row keeps.
func lastRun(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, kind string, id int64) string {
	t.Helper()

	var found []struct {
		Job *struct {
			State string `json:"state"`
		} `json:"job"`
	}
	asked(ctx, t, conn, kind+".query", []any{[]any{[]any{"id", "=", id}}}, &found)
	if len(found) != 1 || found[0].Job == nil {
		return "never"
	}

	return found[0].Job.State
}

// A cloud sync to a bucket. The NAS will not make the bucket, which is made for it, and lists it. A sync puts there what the directory holds, and a second run what has
// changed since, read from the storage itself. Another, set to encrypt, leaves nothing on the storage that can be read: neither a file's
// name nor what it holds. With the storage down a run fails and is said to have, and with it back the next one works.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestACloudSyncToABucketMirrorsEncryptsAndSaysSoWhenTheStorageIsDown(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 15*time.Minute)

	const bucket = "made-by-the-nas"
	from := layout.Pool + "/to-a-bucket"
	gone(ctx, t, conn, from)
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": from}}, nil); err != nil {
		t.Fatalf("making a dataset: %v", err)
	}
	put(ctx, t, conn, "/mnt/"+from+"/note.txt", "kept in a bucket too\n")
	put(ctx, t, conn, "/mnt/"+from+"/passing.txt", "here for now\n")

	storage, account := bucketed(ctx, t, conn, "storage in a bucket, for a cloud sync")

	// a NAS makes buckets only on the few kinds of storage it knows how to, which S3 is not: seen on a real NAS
	if err := conn.Call(ctx, "cloudsync.create_bucket", []any{account, bucket}, nil); err == nil || !strings.Contains(err.Error(), "can't create buckets") {
		t.Errorf("asked to make a bucket on S3 storage, which a real NAS was seen to refuse, the NAS answers: %v", err)
	}
	storage.Make(bucket)

	var listed []struct {
		Name string `json:"Name"`
	}
	asked(ctx, t, conn, "cloudsync.list_buckets", []any{account}, &listed)
	if len(listed) != 1 || listed[0].Name != bucket {
		t.Errorf("the NAS lists the storage's buckets as %+v, want the one", listed)
	}

	mirror := madeOne(ctx, t, conn, "cloudsync", map[string]any{"description": "a mirror in a bucket", "path": "/mnt/" + from, "credentials": account, "attributes": map[string]any{"bucket": bucket, "folder": "mirrored"}, "direction": "PUSH", "transfer_mode": "SYNC", "enabled": false})
	if err := conn.Call(ctx, "cloudsync.sync", []any{mirror}, nil); err != nil {
		t.Fatalf("mirroring to the bucket: %v", err)
	}
	if held, ok := storage.Object(bucket, "mirrored/note.txt"); !ok || string(held) != "kept in a bucket too\n" {
		t.Errorf("the storage holds %q for the file that was mirrored (there: %t); it holds %v", held, ok, storage.Keys(bucket))
	}

	// a file changes, and the next run brings the storage up to what the directory holds now
	put(ctx, t, conn, "/mnt/"+from+"/passing.txt", "changed since\n")
	if err := conn.Call(ctx, "cloudsync.sync", []any{mirror}, nil); err != nil {
		t.Fatalf("mirroring to the bucket a second time: %v", err)
	}
	if held, _ := storage.Object(bucket, "mirrored/passing.txt"); string(held) != "changed since\n" {
		t.Errorf("after the second run the storage holds %q for the file that changed", held)
	}
	if held, _ := storage.Object(bucket, "mirrored/note.txt"); string(held) != "kept in a bucket too\n" {
		t.Errorf("after the second run the storage holds %q for the file that stayed as it was", held)
	}

	// set to encrypt, nothing readable reaches the storage
	secret := madeOne(ctx, t, conn, "cloudsync", map[string]any{"description": "kept in a bucket where nobody can read it", "path": "/mnt/" + from, "credentials": account, "attributes": map[string]any{"bucket": bucket, "folder": "unreadable"}, "direction": "PUSH", "transfer_mode": "COPY", "enabled": false, "encryption": true, "filename_encryption": true, "encryption_password": marked("sync-password"), "encryption_salt": marked("sync-salt")})
	if err := conn.Call(ctx, "cloudsync.sync", []any{secret}, nil); err != nil {
		t.Fatalf("copying to the bucket, encrypted: %v", err)
	}
	unreadable := 0
	for _, key := range storage.Keys(bucket) {
		if !strings.HasPrefix(key, "unreadable/") {
			continue
		}

		unreadable++
		held, _ := storage.Object(bucket, key)
		if strings.Contains(key, "note") || bytes.Contains(held, []byte("kept in a bucket")) {
			t.Errorf("set to encrypt, the storage holds %s, which can be read: %.40q", key, held)
		}
	}
	if unreadable == 0 {
		t.Errorf("set to encrypt, nothing reached the storage: it holds %v", storage.Keys(bucket))
	}

	// the storage goes down, and comes back
	storage.Gone(true)
	err := conn.Call(ctx, "cloudsync.sync", []any{mirror}, nil)
	if err == nil {
		t.Fatal("a cloud sync to storage that is down ran without failing")
	}
	t.Logf("with the storage down the run fails with: %v", err)
	if state := lastRun(ctx, t, conn, "cloudsync", mirror); state != "FAILED" {
		t.Errorf("after a run with the storage down the cloud sync says its last run was %s, want FAILED", state)
	}
	said := alertsNaming(ctx, t, conn, "CloudSyncTaskFailed", "a mirror in a bucket")
	if len(said) == 0 {
		t.Error("the NAS raised no alert for a cloud sync whose storage was down")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}

	storage.Gone(false)
	if err := conn.Call(ctx, "cloudsync.sync", []any{mirror}, nil); err != nil {
		t.Errorf("mirroring with the storage back: %v", err)
	}
	if state := lastRun(ctx, t, conn, "cloudsync", mirror); state != "SUCCESS" {
		t.Errorf("after a run with the storage back the cloud sync says its last run was %s", state)
	}
	if still := alertsNaming(ctx, t, conn, "CloudSyncTaskFailed", "a mirror in a bucket"); len(still) != 0 {
		t.Errorf("the NAS still says the cloud sync failed, after a run that worked: %+v", still)
	}
}

// A cloud backup keeps what a directory held each time it ran, so that any of those can be brought back. Two are made with the file changed
// between them, each is listed with what it holds, and the first is brought back beside the second: it holds what the file held then. What
// reaches the storage cannot be read there. With the storage down a run fails and is said to have, and with it back the next one works. A
// backup made too many is forgotten, by the number the task is told to keep.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestACloudBackupIsMadeListedBroughtBackAndSaysSoWhenTheStorageIsDown(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 20*time.Minute)

	const bucket = "backups"
	from, back := layout.Pool+"/backed-up", layout.Pool+"/brought-back"
	gone(ctx, t, conn, from, back)
	for _, dataset := range []string{from, back} {
		if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": dataset}}, nil); err != nil {
			t.Fatalf("making %s: %v", dataset, err)
		}
	}
	put(ctx, t, conn, "/mnt/"+from+"/note.txt", "as it was the first night\n")

	storage, account := bucketed(ctx, t, conn, "storage in a bucket, for a cloud backup")
	storage.Make(bucket)

	backup := madeOne(ctx, t, conn, "cloud_backup", map[string]any{"description": "the nightly backup", "path": "/mnt/" + from, "credentials": account, "attributes": map[string]any{"bucket": bucket, "folder": "of-the-nas"}, "password": marked("backup-password"), "keep_last": 2, "enabled": false})

	type night struct {
		ID    string   `json:"id"`
		Host  string   `json:"hostname"`
		Paths []string `json:"paths"`
	}
	nights := func() []night {
		var kept []night
		asked(ctx, t, conn, "cloud_backup.list_snapshots", []any{backup}, &kept)

		return kept
	}
	if kept := nights(); len(kept) != 0 || lastRun(ctx, t, conn, "cloud_backup", backup) != "never" {
		t.Errorf("before it has run, the backup holds %+v", kept)
	}

	if err := conn.Call(ctx, "cloud_backup.sync", []any{backup}, nil); err != nil {
		t.Fatalf("making the first backup: %v", err)
	}
	first := nights()
	if len(first) != 1 || lastRun(ctx, t, conn, "cloud_backup", backup) != "SUCCESS" {
		t.Fatalf("after one run the backup holds %+v, and its last run is said to be %s", first, lastRun(ctx, t, conn, "cloud_backup", backup))
	}
	t.Logf("the first backup: %+v", first[0])

	// nothing that reached the storage can be read there
	kept := storage.Keys(bucket)
	if len(kept) == 0 {
		t.Fatal("the NAS says the backup worked, and nothing reached the storage")
	}
	for _, key := range kept {
		if held, _ := storage.Object(bucket, key); !strings.HasPrefix(key, "of-the-nas/") || bytes.Contains(held, []byte("the first night")) {
			t.Errorf("the storage holds %s, which is outside where the backup was to go or can be read", key)
		}
	}

	var holds []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	asked(ctx, t, conn, "cloud_backup.list_snapshot_directory", []any{backup, first[0].ID, "/"}, &holds)
	if !slices.ContainsFunc(holds, func(h struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	},
	) bool {
		return h.Name == "note.txt" && h.Type == "file"
	}) {
		t.Errorf("the first backup is said to hold %+v, which lacks the file", holds)
	}

	put(ctx, t, conn, "/mnt/"+from+"/note.txt", "as it was the second night\n")
	if err := conn.Call(ctx, "cloud_backup.sync", []any{backup}, nil); err != nil {
		t.Fatalf("making the second backup: %v", err)
	}
	if both := nights(); len(both) != 2 {
		t.Fatalf("after two runs the backup holds %d", len(both))
	}

	// the first is brought back, and holds what the file held then
	if err := conn.Call(ctx, "cloud_backup.restore", []any{backup, first[0].ID, "/", "/mnt/" + back, map[string]any{}}, nil); err != nil {
		t.Fatalf("bringing the first backup back: %v", err)
	}
	if was := got(ctx, t, conn, "/mnt/"+back+"/note.txt"); was != "as it was the first night\n" {
		t.Errorf("brought back, the first backup holds %q", was)
	}

	// told to keep two, a third run forgets the first
	put(ctx, t, conn, "/mnt/"+from+"/note.txt", "as it was the third night\n")
	if err := conn.Call(ctx, "cloud_backup.sync", []any{backup}, nil); err != nil {
		t.Fatalf("making the third backup: %v", err)
	}
	last := nights()
	if len(last) != 2 || slices.ContainsFunc(last, func(n night) bool { return n.ID == first[0].ID }) {
		t.Errorf("told to keep two, after three runs the backup holds %+v; want the last two", last)
	}

	if len(last) > 0 {
		if err := conn.Call(ctx, "cloud_backup.delete_snapshot", []any{backup, last[0].ID}, nil); err != nil {
			t.Errorf("removing one backup: %v", err)
		} else if left := nights(); len(left) != len(last)-1 {
			t.Errorf("with one backup removed, %d are left of %d", len(left), len(last))
		}
	}

	var renamed json.RawMessage
	if err := conn.Call(ctx, "cloud_backup.update", []any{backup, map[string]any{"description": "the nightly backup, renamed"}}, &renamed); err != nil || field(t, renamed, "description") != `"the nightly backup, renamed"` {
		t.Errorf("renamed, the backup reads as %.200s (%v)", renamed, err)
	}

	// the storage goes down, and comes back
	storage.Gone(true)
	err := conn.Call(ctx, "cloud_backup.sync", []any{backup}, nil)
	if err == nil {
		t.Fatal("a cloud backup to storage that is down ran without failing")
	}
	t.Logf("with the storage down the run fails with: %v", err)
	if state := lastRun(ctx, t, conn, "cloud_backup", backup); state != "FAILED" {
		t.Errorf("after a run with the storage down the backup says its last run was %s, want FAILED", state)
	}
	said := alerted(ctx, t, conn, "CloudBackupTaskFailed")
	if len(said) == 0 {
		t.Error("the NAS raised no alert for a cloud backup whose storage was down")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}

	storage.Gone(false)
	if err := conn.Call(ctx, "cloud_backup.sync", []any{backup}, nil); err != nil {
		t.Errorf("making a backup with the storage back: %v", err)
	}
	if still := alerted(ctx, t, conn, "CloudBackupTaskFailed"); len(still) != 0 {
		t.Errorf("the NAS still says the backup failed, after a run that worked: %+v", still)
	}
}
