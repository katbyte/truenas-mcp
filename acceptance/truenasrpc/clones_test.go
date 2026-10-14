//go:build acceptance

package truenasrpc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// A snapshot is renamed, which a NAS will not do until it is told the risk is understood, and given a note of its own. A copy is made from
// it and then promoted: the copy becomes the dataset the snapshot belongs to, and the dataset it was copied from becomes the one that rests
// on it, so the old one can be removed and the copy stays.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestASnapshotIsRenamedAndACopyMadeFromItTakesTheDatasetsPlace(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	first, second := layout.Pool+"/the-first", layout.Pool+"/the-second"
	gone(ctx, t, conn, second, first)

	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": first}}, nil); err != nil {
		t.Fatalf("making a dataset: %v", err)
	}
	put(ctx, t, conn, "/mnt/"+first+"/note.txt", "written to the first\n")
	if err := conn.Call(ctx, "pool.snapshot.create", []any{map[string]any{"dataset": first, "name": "taken"}}, nil); err != nil {
		t.Fatalf("taking a snapshot: %v", err)
	}

	// a snapshot is not renamed until the caller says the risk is understood
	err := conn.Call(ctx, "pool.snapshot.rename", []any{first + "@taken", map[string]any{"new_name": first + "@renamed"}}, nil)
	if !truenasrpc.IsInvalid(err) || !strings.Contains(err.Error(), "force") {
		t.Errorf("a snapshot is renamed without the risk being owned to, or refused some other way: %v", err)
	}
	if err := conn.Call(ctx, "pool.snapshot.rename", []any{first + "@taken", map[string]any{"new_name": first + "@renamed", "force": true}}, nil); err != nil {
		t.Fatalf("renaming the snapshot: %v", err)
	}
	if has := snapshotsOf(ctx, t, conn, first); !slices.Equal(has, []string{first + "@renamed"}) {
		t.Errorf("renamed, the dataset's snapshots are %v", has)
	}

	var noted struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := conn.Call(ctx, "pool.snapshot.update", []any{first + "@renamed", map[string]any{"user_properties_update": []any{map[string]any{"key": "tests:why", "value": "kept for a reason"}}}}, &noted); err != nil {
		t.Fatalf("putting a note on the snapshot: %v", err)
	}
	if !strings.Contains(string(noted.Properties["tests:why"]), "kept for a reason") {
		t.Errorf("the note on the snapshot reads back as %s", noted.Properties["tests:why"])
	}

	if err := conn.Call(ctx, "pool.snapshot.clone", []any{map[string]any{"snapshot": first + "@renamed", "dataset_dst": second}}, nil); err != nil {
		t.Fatalf("making a copy from the snapshot: %v", err)
	}
	put(ctx, t, conn, "/mnt/"+second+"/note.txt", "written to the second\n")

	// a dataset nothing was copied to make cannot be promoted
	if err := conn.Call(ctx, "pool.dataset.promote", []any{first}, nil); err == nil {
		t.Error("a dataset that is no copy of anything was promoted")
	} else {
		t.Logf("promoting a dataset that is no copy is refused with: %v", err)
	}
	if err := conn.Call(ctx, "pool.dataset.promote", []any{second}, nil); err != nil {
		t.Fatalf("promoting the copy: %v", err)
	}
	if of, was := snapshotsOf(ctx, t, conn, second), snapshotsOf(ctx, t, conn, first); !slices.Equal(of, []string{second + "@renamed"}) || len(was) != 0 {
		t.Errorf("promoted, the copy's snapshots are %v and the first dataset's %v; want the one snapshot moved to the copy", of, was)
	}

	// the first now rests on the second, so it is the one that can go
	if err := conn.Call(ctx, "pool.dataset.delete", []any{first, map[string]any{"recursive": true}}, nil); err != nil {
		t.Errorf("removing the dataset the copy was made from, after the copy was promoted: %v", err)
	}
	if holds := got(ctx, t, conn, "/mnt/"+second+"/note.txt"); holds != "written to the second\n" {
		t.Errorf("the promoted copy holds %q", holds)
	}
}

// A dataset under a key of its own, inside another that has one, is told to take the key of the one above it. Before, the keys of the two
// come as one file naming both; after, the file names one, and the dataset says the one above is where its key is kept. No key is ever in a
// trace.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestADatasetTakesTheKeyOfTheOneAboveItAndTheKeysComeAsOneFile(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	above, below := layout.Pool+"/keyed-above", layout.Pool+"/keyed-above/keyed-below"
	gone(ctx, t, conn, above)

	for _, name := range []string{above, below} {
		if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name, "encryption": true, "inherit_encryption": false, "encryption_options": map[string]any{"generate_key": true}}}, nil); err != nil {
			t.Fatalf("making %s under a key of its own: %v", name, err)
		}

		// the key is asked for so that the run knows it, and fails if a line of trace ever holds it
		var key string
		if err := conn.Call(ctx, "pool.dataset.export_key", []any{name}, &key); err != nil || len(key) != 64 {
			t.Fatalf("asking for the key of %s: %d characters, %v", name, len(key), err)
		}
		ledger.hide(key)
	}
	if made := one(ctx, t, conn, below); made.Root == nil || *made.Root != below {
		t.Errorf("under a key of its own, the dataset's key is said to be kept at %v", made.Root)
	}

	keys := func() map[string]string {
		ledger.count("pool.dataset.export_keys")

		var file bytes.Buffer
		if _, err := conn.Download(ctx, "pool.dataset.export_keys", []any{above}, &file); err != nil {
			t.Fatalf("fetching the keys as a file: %v", err)
		}

		var written map[string]string
		if err := json.Unmarshal(file.Bytes(), &written); err != nil {
			t.Fatalf("the file of keys is %d bytes that do not read as keys by dataset: %v", file.Len(), err)
		}

		return written
	}
	if both := keys(); len(both) != 2 || len(both[above]) != 64 || len(both[below]) != 64 || both[above] == both[below] {
		t.Errorf("the file of keys names %d datasets, want the two with a key each", len(both))
	}

	if err := conn.Call(ctx, "pool.dataset.inherit_parent_encryption_properties", []any{below}, nil); err != nil {
		t.Fatalf("telling the dataset to take the key of the one above it: %v", err)
	}
	if now := one(ctx, t, conn, below); now.Root == nil || *now.Root != above || !now.Encrypted {
		t.Errorf("having taken the key of the one above, the dataset's key is said to be kept at %v", now.Root)
	}
	if left := keys(); len(left) != 1 || len(left[above]) != 64 {
		t.Errorf("the file of keys names %d datasets, want the one whose key both now open with", len(left))
	}

	// a dataset already on the key of the one above has nothing to take
	if err := conn.Call(ctx, "pool.dataset.inherit_parent_encryption_properties", []any{below}, nil); err == nil {
		t.Error("a dataset already under the key of the one above it was told to take it again, and that was not refused")
	} else {
		t.Logf("taking the key of the one above a second time is refused with: %v", err)
	}
}

// The pool a NAS starts from is scrubbed like any other, and how many days pass between its scrubs is set and read back.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestTheBootPoolIsScrubbedAndHowOftenIsSet(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 15*time.Minute)

	var before struct {
		Days int `json:"boot_scrub"`
	}
	if err := conn.Call(ctx, "system.advanced.config", nil, &before); err != nil || before.Days == 0 {
		t.Fatalf("reading how often the boot pool is scrubbed: %d days, %v", before.Days, err)
	}

	var set int
	if err := conn.Call(ctx, "boot.set_scrub_interval", []any{before.Days + 3}, &set); err != nil || set != before.Days+3 {
		t.Fatalf("setting the boot pool to be scrubbed every %d days answered %d (%v)", before.Days+3, set, err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "boot.set_scrub_interval", []any{before.Days}, nil); err != nil {
			t.Errorf("putting back how often the boot pool is scrubbed: %v", err)
		}
	})

	var now struct {
		Days int `json:"boot_scrub"`
	}
	if err := conn.Call(ctx, "system.advanced.config", nil, &now); err != nil || now.Days != before.Days+3 {
		t.Errorf("set to every %d days, the boot pool's scrubs read as every %d (%v)", before.Days+3, now.Days, err)
	}

	if err := conn.Call(ctx, "boot.scrub", nil, nil); err != nil {
		t.Fatalf("scrubbing the boot pool: %v", err)
	}

	var state struct {
		Healthy bool `json:"healthy"`
		Scan    struct {
			Function string `json:"function"`
			State    string `json:"state"`
			Errors   int64  `json:"errors"`
		} `json:"scan"`
	}
	asked(ctx, t, reader(t), "boot.get_state", nil, &state)
	if !state.Healthy || state.Scan.Function != "SCRUB" || state.Scan.Errors != 0 {
		t.Errorf("scrubbed, the boot pool reads as healthy %t, last looked over by a %s that is %s and found %d errors", state.Healthy, state.Scan.Function, state.Scan.State, state.Scan.Errors)
	}
}
