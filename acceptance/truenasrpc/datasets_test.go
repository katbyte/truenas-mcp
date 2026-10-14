//go:build acceptance

package truenasrpc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// setting is one setting of a dataset, as a NAS writes each: the value as it would be given, and the same parsed.
type setting struct {
	Value  string `json:"value"`
	Parsed any    `json:"parsed"`
}

// stored is a dataset as the NAS has it, with the settings these tests change.
type stored struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Compression setting  `json:"compression"`
	Atime       setting  `json:"atime"`
	Readonly    setting  `json:"readonly"`
	Quota       setting  `json:"quota"`
	Volsize     *setting `json:"volsize"`
	Encrypted   bool     `json:"encrypted"`
	Locked      bool     `json:"locked"`
	KeyLoaded   bool     `json:"key_loaded"`
	KeyFormat   setting  `json:"key_format"`
	Root        *string  `json:"encryption_root"`
}

// one is the dataset of a name, which has to be there.
func one(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name string) stored {
	t.Helper()

	var found []stored
	if err := conn.Call(ctx, "pool.dataset.query", []any{[]any{[]any{"id", "=", name}}}, &found); err != nil || len(found) != 1 {
		t.Fatalf("the dataset %s is not found: %d found, %v", name, len(found), err)
	}

	return found[0]
}

// noted checks the note kept on a dataset reads back. The description lists it as the dataset's "comments"; a real NAS answers it one level
// down, among the dataset's user properties, and has no "comments" of the dataset's own.
func noted(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name, note string) {
	t.Helper()

	var found []struct {
		Comments   json.RawMessage `json:"comments"`
		Properties struct {
			Comments setting `json:"comments"`
		} `json:"user_properties"`
	}
	if err := conn.Call(ctx, "pool.dataset.query", []any{[]any{[]any{"id", "=", name}}}, &found); err != nil || len(found) != 1 {
		t.Fatalf("the dataset %s is not found: %d found, %v", name, len(found), err)
	}
	if found[0].Properties.Comments.Value != note {
		t.Errorf("the note on %s reads back as %q among its user properties, want %q", name, found[0].Properties.Comments.Value, note)
	}
	if found[0].Comments != nil {
		t.Errorf("the note on %s is answered as the dataset's own comments too, which a real NAS was seen not to do: %s", name, found[0].Comments)
	}
}

// gone removes a dataset and everything under it when the test ends, whether or not it is still there.
func gone(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, names ...string) {
	t.Helper()

	t.Cleanup(func() {
		for _, name := range names {
			if err := conn.Call(context.WithoutCancel(ctx), "pool.dataset.delete", []any{name, map[string]any{"recursive": true, "force": true}}, nil); err != nil && !truenasrpc.IsNotFound(err) {
				t.Errorf("removing %s: %v", name, err)
			}
		}
	})
}

// A dataset is made with settings of its own, and each is read back as it was given. It is then changed, made read-only so that a write is
// refused, given a child under a parent that did not exist, renamed with everything under it, given a directory with an owner, a limit for
// one user, and a volume that is grown.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestADatasetIsMadeChangedRenamedAndRemoved(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	name, renamed := layout.Pool+"/made", layout.Pool+"/renamed"
	gone(ctx, t, conn, name, renamed)

	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name, "comments": "made by the tests", "compression": "ZSTD", "atime": "OFF", "quota": 2 << 30}}, nil); err != nil {
		t.Fatalf("making the dataset: %v", err)
	}
	made := one(ctx, t, conn, name)
	if made.Type != "FILESYSTEM" || made.Compression.Value != "ZSTD" || made.Atime.Value != "OFF" || made.Quota.Parsed != float64(2<<30) {
		t.Errorf("the dataset reads back as %+v; want what it was made with", made)
	}
	noted(ctx, t, conn, name, "made by the tests")

	if err := conn.Call(ctx, "pool.dataset.update", []any{name, map[string]any{"compression": "LZ4", "comments": "changed by the tests", "readonly": "ON"}}, nil); err != nil {
		t.Fatalf("changing the dataset: %v", err)
	}
	if changed := one(ctx, t, conn, name); changed.Compression.Value != "LZ4" || changed.Readonly.Value != "ON" {
		t.Errorf("changed, the dataset reads back as %+v", changed)
	}
	noted(ctx, t, conn, name, "changed by the tests")

	// read-only, it takes no file
	ledger.count("filesystem.put")
	job, err := conn.StartUpload(ctx, "filesystem.put", []any{"/mnt/" + name + "/refused.txt", map[string]any{}}, strings.NewReader("not to be written\n"))
	if err == nil {
		err = job.Wait(ctx, nil)
	}
	if err == nil {
		t.Error("a dataset that is read-only took a file")
	}
	t.Logf("a file for a read-only dataset is refused with: %v", err)

	if err := conn.Call(ctx, "pool.dataset.update", []any{name, map[string]any{"readonly": "OFF"}}, nil); err != nil {
		t.Fatalf("letting the dataset be written again: %v", err)
	}
	put(ctx, t, conn, "/mnt/"+name+"/note.txt", "kept through a rename\n")

	// a child, under a parent that is made with it
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name + "/deeper/child", "create_ancestors": true}}, nil); err != nil {
		t.Fatalf("making a child and the parent it needs: %v", err)
	}
	one(ctx, t, conn, name+"/deeper")

	// a NAS checks nothing that a rename might break, and will not rename until it is told that is understood
	err = conn.Call(ctx, "pool.dataset.rename", []any{name, map[string]any{"new_name": renamed}}, nil)
	if !truenasrpc.IsInvalid(err) || !strings.Contains(err.Error(), "force") {
		t.Errorf("a rename that does not say it is understood was answered: %v", err)
	}

	// asked to take what is under it along, it refuses: that is for renaming a snapshot of every dataset at once
	err = conn.Call(ctx, "pool.dataset.rename", []any{name, map[string]any{"new_name": renamed, "recursive": true, "force": true}}, nil)
	if err == nil || !strings.Contains(err.Error(), "recursive rename must be a snapshot") {
		t.Errorf("a rename of a dataset said to be recursive was answered: %v", err)
	}

	// renamed, everything under it goes with it without being asked
	if err := conn.Call(ctx, "pool.dataset.rename", []any{name, map[string]any{"new_name": renamed, "force": true}}, nil); err != nil {
		t.Fatalf("renaming the dataset: %v", err)
	}
	one(ctx, t, conn, renamed+"/deeper/child")
	if err := conn.Call(ctx, "pool.dataset.get_instance", []any{name}, nil); !truenasrpc.IsNotFound(err) {
		t.Errorf("renamed, the old name is still answered: %v", err)
	}
	if kept := got(ctx, t, conn, "/mnt/"+renamed+"/note.txt"); kept != "kept through a rename\n" {
		t.Errorf("renamed, the dataset's file reads as %q", kept)
	}

	// a directory in it, and who it belongs to
	inside := "/mnt/" + renamed + "/inside"
	if err := conn.Call(ctx, "filesystem.mkdir", []any{map[string]any{"path": inside}}, nil); err != nil {
		t.Fatalf("making a directory: %v", err)
	}
	if err := conn.Call(ctx, "filesystem.setperm", []any{map[string]any{"path": inside, "mode": "750", "uid": 65534, "gid": 65534}}, nil); err != nil {
		t.Fatalf("saying whose the directory is: %v", err)
	}
	var is struct {
		Mode int `json:"mode"`
		UID  int `json:"uid"`
		GID  int `json:"gid"`
	}
	if err := conn.Call(ctx, "filesystem.stat", []any{inside}, &is); err != nil || is.Mode&0o777 != 0o750 || is.UID != 65534 || is.GID != 65534 {
		t.Errorf("the directory reads as mode %o owned by %d and %d (%v); want 750 and 65534", is.Mode&0o777, is.UID, is.GID, err)
	}

	// a limit for one user of it
	if err := conn.Call(ctx, "pool.dataset.set_quota", []any{renamed, []any{map[string]any{"quota_type": "USER", "id": "nobody", "quota_value": 1 << 30}}}, nil); err != nil {
		t.Fatalf("limiting one user: %v", err)
	}
	var limits []struct {
		Name  string `json:"name"`
		Quota int64  `json:"quota"`
	}
	if err := conn.Call(ctx, "pool.dataset.get_quota", []any{renamed, "USER"}, &limits); err != nil {
		t.Fatalf("reading the users' limits: %v", err)
	}
	var limited bool
	for _, limit := range limits {
		limited = limited || limit.Quota == 1<<30
	}
	if !limited {
		t.Errorf("the user's limit is not among those read back: %+v", limits)
	}

	// a volume, which is grown
	volume := renamed + "/volume"
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": volume, "type": "VOLUME", "volsize": 64 << 20, "sparse": true}}, nil); err != nil {
		t.Fatalf("making a volume: %v", err)
	}
	if made := one(ctx, t, conn, volume); made.Type != "VOLUME" || made.Volsize == nil || made.Volsize.Parsed != float64(64<<20) {
		t.Errorf("the volume reads back as a %s of %+v", made.Type, made.Volsize)
	}
	if err := conn.Call(ctx, "pool.dataset.update", []any{volume, map[string]any{"volsize": 128 << 20}}, nil); err != nil {
		t.Fatalf("growing the volume: %v", err)
	}
	if grown := one(ctx, t, conn, volume); grown.Volsize == nil || grown.Volsize.Parsed != float64(128<<20) {
		t.Errorf("grown, the volume reads back as %+v", grown.Volsize)
	}

	if err := conn.Call(ctx, "pool.dataset.delete", []any{renamed, map[string]any{"recursive": true}}, nil); err != nil {
		t.Fatalf("removing the dataset and all under it: %v", err)
	}
	if err := conn.Call(ctx, "pool.dataset.get_instance", []any{renamed}, nil); !truenasrpc.IsNotFound(err) {
		t.Errorf("removed, the dataset is still answered: %v", err)
	}
}

// unlocked asks the NAS to unlock a dataset with a passphrase, and reports whether it did.
func unlocked(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name, passphrase string) bool {
	t.Helper()

	var answer struct {
		Unlocked []string `json:"unlocked"`
	}
	if err := conn.Call(ctx, "pool.dataset.unlock", []any{name, map[string]any{"datasets": []any{map[string]any{"name": name, "passphrase": passphrase}}}}, &answer); err != nil {
		t.Logf("unlocking %s: %v", name, err)

		return false
	}

	return len(answer.Unlocked) == 1 && answer.Unlocked[0] == name
}

// A dataset kept under a passphrase is locked, which takes what it holds out of reach, and unlocked only by the right passphrase. Before
// unlocking, the NAS can be asked whether a passphrase would do. The passphrase is changed, and the old one no longer opens it. A dataset
// kept under a key the NAS made hands that key over, as an answer and as a file, and neither is ever in a trace.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAnEncryptedDatasetIsLockedUnlockedAndGivenANewPassphrase(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 12*time.Minute)

	name, keyed, note := layout.Pool+"/secret", layout.Pool+"/keyed", "/mnt/"+layout.Pool+"/secret/note.txt"
	first, second := "the first words to open it", "the second words to open it"
	gone(ctx, t, conn, name, keyed)

	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name, "encryption": true, "inherit_encryption": false, "encryption_options": map[string]any{"passphrase": first}}}, nil); err != nil {
		t.Fatalf("making an encrypted dataset: %v", err)
	}
	if made := one(ctx, t, conn, name); !made.Encrypted || made.Locked || !made.KeyLoaded || made.KeyFormat.Value != "PASSPHRASE" || made.Root == nil || *made.Root != name {
		t.Errorf("the encrypted dataset reads back as %+v", made)
	}
	put(ctx, t, conn, note, "kept under a passphrase\n")

	if err := conn.Call(ctx, "pool.dataset.lock", []any{name}, nil); err != nil {
		t.Fatalf("locking the dataset: %v", err)
	}
	if locked := one(ctx, t, conn, name); !locked.Locked || locked.KeyLoaded {
		t.Errorf("locked, the dataset reads back as locked %t with its key loaded %t", locked.Locked, locked.KeyLoaded)
	}
	if err := conn.Call(ctx, "filesystem.stat", []any{note}, nil); err == nil {
		t.Error("locked, the dataset's file is still there to be read")
	}

	// asked first, the NAS says whether a passphrase would open it
	would := func(passphrase string) bool {
		var summary []struct {
			Name       string `json:"name"`
			Successful bool   `json:"unlock_successful"`
		}
		if err := conn.Call(ctx, "pool.dataset.encryption_summary", []any{name, map[string]any{"datasets": []any{map[string]any{"name": name, "passphrase": passphrase}}}}, &summary); err != nil || len(summary) != 1 {
			t.Fatalf("asking whether a passphrase would open the dataset: %d answers, %v", len(summary), err)
		}

		return summary[0].Successful
	}
	if would("not the words at all") || !would(first) {
		t.Error("asked first, the NAS says the wrong passphrase would open the dataset, or the right one would not")
	}

	if unlocked(ctx, t, conn, name, "not the words at all") {
		t.Fatal("the wrong passphrase unlocked the dataset")
	}
	if !unlocked(ctx, t, conn, name, first) {
		t.Fatal("the right passphrase did not unlock the dataset")
	}
	if back := got(ctx, t, conn, note); back != "kept under a passphrase\n" {
		t.Errorf("unlocked, the file reads as %q", back)
	}

	if err := conn.Call(ctx, "pool.dataset.change_key", []any{name, map[string]any{"passphrase": second}}, nil); err != nil {
		t.Fatalf("changing the passphrase: %v", err)
	}
	if err := conn.Call(ctx, "pool.dataset.lock", []any{name}, nil); err != nil {
		t.Fatalf("locking the dataset again: %v", err)
	}
	if unlocked(ctx, t, conn, name, first) {
		t.Error("the passphrase that was changed still unlocks the dataset")
	}
	if !unlocked(ctx, t, conn, name, second) {
		t.Error("the new passphrase does not unlock the dataset")
	}

	// under a key the NAS made, the key can be had, and is the same had either way
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": keyed, "encryption": true, "inherit_encryption": false, "encryption_options": map[string]any{"generate_key": true}}}, nil); err != nil {
		t.Fatalf("making a dataset under a key: %v", err)
	}
	if made := one(ctx, t, conn, keyed); !made.Encrypted || made.KeyFormat.Value != "HEX" {
		t.Errorf("the dataset under a key reads back as %+v", made)
	}

	var key string
	if err := conn.Call(ctx, "pool.dataset.export_key", []any{keyed}, &key); err != nil || len(key) != 64 {
		t.Fatalf("asking for the dataset's key: %d characters, %v", len(key), err)
	}
	ledger.hide(key)

	ledger.count("pool.dataset.export_key")
	var file bytes.Buffer
	if _, err := conn.Download(ctx, "pool.dataset.export_key", []any{keyed, true}, &file); err != nil {
		t.Fatalf("fetching the dataset's key as a file: %v", err)
	}
	// as a file the key comes with the name of the dataset it opens, where the answer is the key alone
	var written map[string]string
	if err := json.Unmarshal(file.Bytes(), &written); err != nil || len(written) != 1 || written[keyed] != key {
		t.Errorf("the key fetched as a file is %d bytes naming %d datasets (%v), and not the one key that was answered", file.Len(), len(written), err)
	}
}
