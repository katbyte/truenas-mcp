//go:build acceptance

package tools_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// The tools that change a NAS are proved by changing the throwaway one through them and reading it back through them, each change tried as a dry run
// and each removal unconfirmed first, which must leave the NAS as it was.

// cleared removes a dataset and everything under it when the test ends, if the tools being tested left it there.
func cleared(t *testing.T, conn *truenasrpc.Conn, name string) {
	t.Helper()

	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		var found []struct{}
		if err := conn.Call(ctx, "pool.dataset.query", []any{[]any{[]any{"name", "=", name}}, map[string]any{"select": []string{"name"}, "extra": map[string]any{"flat": true, "retrieve_children": false, "properties": []string{}}}}, &found); err != nil || len(found) == 0 {
			return
		}
		if err := conn.Call(ctx, "pool.dataset.delete", []any{name, map[string]any{"recursive": true}}, nil); err != nil {
			t.Errorf("removing %s after the test: %v", name, err)
		}
	})
}

// changes is what a write answered it did.
func changes(t *testing.T, out map[string]any) []string {
	t.Helper()

	return acctest.Strs(t, out["changes"], "changes")
}

// A dataset is made, changed and removed through the tools alone: a dry run makes nothing, what is made reads back with the settings asked, an update
// moves only what it is given, and a removal says what would go, refuses to take the datasets under it unasked, and then takes them all.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestADatasetIsMadeChangedAndRemovedThroughTheTools(t *testing.T) {
	conn, _ := planter(t)
	name := layout.Pool + "/made-by-tools"
	cleared(t, conn, name)

	dry := suite.Call(t, "dataset_create", map[string]any{"dataset": name, "quota_bytes": 1 << 30, "dry_run": true})
	if !acctest.BoolOf(dry["dry_run"]) || len(changes(t, dry)) != 1 {
		t.Errorf("a dry run answered %v", dry)
	}
	if msg := suite.CallErr(t, "dataset_get", map[string]any{"dataset": name}); !strings.Contains(msg, "no dataset") {
		t.Fatalf("a dry run made the dataset: %q", msg)
	}

	made := acctest.Object(t, suite.Call(t, "dataset_create", map[string]any{"dataset": name, "quota_bytes": 1 << 30, "compression": "zstd", "comments": "made by the tools"})["dataset"], "dataset")
	if made["name"] != name || made["type"] != "FILESYSTEM" || acctest.NumOr0(made["quota_bytes"]) != 1<<30 || made["compression"] != "ZSTD" {
		t.Errorf("made %v", made)
	}
	if got := suite.Call(t, "dataset_get", map[string]any{"dataset": name}); got["comments"] != "made by the tools" {
		t.Errorf("the comments read back as %v", got["comments"])
	}
	suite.Call(t, "dataset_create", map[string]any{"dataset": name + "/child"})
	volume := acctest.Object(t, suite.Call(t, "dataset_create", map[string]any{"dataset": name + "/disk", "volume_bytes": 64 << 20, "sparse": true})["dataset"], "dataset")
	if volume["type"] != "VOLUME" {
		t.Errorf("a volume reads back as %v", volume)
	}

	updated := suite.Call(t, "dataset_update", map[string]any{"dataset": name, "quota_bytes": 2 << 30, "read_only": true, "comments": ""})
	if row := acctest.Object(t, updated["dataset"], "dataset"); len(changes(t, updated)) != 3 || acctest.NumOr0(row["quota_bytes"]) != 2<<30 || !acctest.BoolOf(row["read_only"]) {
		t.Errorf("updated %v", updated)
	}
	if got := suite.Call(t, "dataset_get", map[string]any{"dataset": name}); got["comments"] != nil {
		t.Errorf("the comments taken away read back as %v", got["comments"])
	}
	if same := suite.Call(t, "dataset_update", map[string]any{"dataset": name, "quota_bytes": 2 << 30}); len(changes(t, same)) != 0 {
		t.Errorf("asked for what is already so: %v", same)
	}
	grown := suite.Call(t, "dataset_update", map[string]any{"dataset": name + "/disk", "volume_bytes": 128 << 20})
	if got := suite.Call(t, "dataset_get", map[string]any{"dataset": name + "/disk"}); len(changes(t, grown)) != 1 || acctest.NumOr0(got["volume_bytes"]) != 128<<20 {
		t.Errorf("a volume grown reads back as %v, after %v", got, grown)
	}

	asked := suite.Call(t, "dataset_delete", map[string]any{"dataset": name})
	if under := acctest.Strs(t, asked["under"], "under"); !slices.Equal(under, []string{name + "/child", name + "/disk"}) || acctest.BoolOf(asked["removed"]) || len(acctest.Strs(t, asked["would_go"], "would_go")) == 0 {
		t.Errorf("without confirm: %v", asked)
	}
	if msg := suite.CallErr(t, "dataset_delete", map[string]any{"dataset": name, "confirm": true}); !strings.Contains(msg, "under it") {
		t.Errorf("confirmed with datasets under it and no recursive: %q", msg)
	}
	suite.Call(t, "dataset_get", map[string]any{"dataset": name + "/child"})

	if gone := suite.Call(t, "dataset_delete", map[string]any{"dataset": name, "recursive": true, "confirm": true}); !acctest.BoolOf(gone["removed"]) {
		t.Errorf("confirmed: %v", gone)
	}
	if msg := suite.CallErr(t, "dataset_get", map[string]any{"dataset": name + "/child"}); !strings.Contains(msg, "no dataset") {
		t.Errorf("a dataset under the one removed is still there: %q", msg)
	}
}

// A snapshot is taken, gone back to and removed through the tools alone. Going back past a newer snapshot is refused until it is asked for, and
// then the newer one is gone; a recursive snapshot is taken and removed with the one under it.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestASnapshotIsTakenGoneBackToAndRemovedThroughTheTools(t *testing.T) {
	conn, _ := planter(t)
	name := layout.Pool + "/snapshotted-by-tools"
	cleared(t, conn, name)
	suite.Call(t, "dataset_create", map[string]any{"dataset": name})
	suite.Call(t, "dataset_create", map[string]any{"dataset": name + "/child"})
	snapshots := func(dataset string) []string {
		rows := listed(t, "snapshot_list", "snapshots", map[string]any{"dataset": dataset})
		names := make([]string, 0, len(rows))
		for _, row := range rows {
			names = append(names, acctest.Str(row["snapshot"]))
		}

		return names
	}

	if dry := suite.Call(t, "snapshot_create", map[string]any{"dataset": name, "name": "first", "dry_run": true}); len(changes(t, dry)) != 1 || len(snapshots(name)) != 0 {
		t.Fatalf("a dry run answered %v", dry)
	}
	taken := acctest.Object(t, suite.Call(t, "snapshot_create", map[string]any{"dataset": name, "name": "first"})["snapshot"], "snapshot")
	if taken["name"] != name+"@first" || acctest.Str(taken["created"]) == "" {
		t.Errorf("took %v", taken)
	}
	suite.Call(t, "snapshot_create", map[string]any{"dataset": name, "name": "second"})

	asked := suite.Call(t, "snapshot_rollback", map[string]any{"snapshot": name + "@first"})
	if newer := acctest.Strs(t, asked["newer"], "newer"); !slices.Equal(newer, []string{name + "@second"}) || acctest.BoolOf(asked["rolled_back"]) {
		t.Errorf("without confirm: %v", asked)
	}
	if msg := suite.CallErr(t, "snapshot_rollback", map[string]any{"snapshot": name + "@first", "confirm": true}); !strings.Contains(msg, name+"@second") {
		t.Errorf("confirmed without remove_newer: %q", msg)
	}
	if back := suite.Call(t, "snapshot_rollback", map[string]any{"snapshot": name + "@first", "remove_newer": true, "confirm": true}); !acctest.BoolOf(back["rolled_back"]) {
		t.Errorf("confirmed: %v", back)
	}
	if left := snapshots(name); !slices.Equal(left, []string{"first"}) {
		t.Errorf("after going back, the snapshots are %v", left)
	}

	if kept := suite.Call(t, "snapshot_delete", map[string]any{"snapshot": name + "@first"}); acctest.BoolOf(kept["removed"]) || len(snapshots(name)) != 1 {
		t.Errorf("without confirm: %v", kept)
	}
	if gone := suite.Call(t, "snapshot_delete", map[string]any{"snapshot": name + "@first", "confirm": true}); !acctest.BoolOf(gone["removed"]) || len(snapshots(name)) != 0 {
		t.Errorf("confirmed: %v", gone)
	}

	suite.Call(t, "snapshot_create", map[string]any{"dataset": name, "name": "both", "recursive": true})
	if under := snapshots(name + "/child"); !slices.Equal(under, []string{"both"}) {
		t.Errorf("a recursive snapshot left the one under it with %v", under)
	}
	asked = suite.Call(t, "snapshot_delete", map[string]any{"snapshot": name + "@both", "recursive": true})
	if under := acctest.Strs(t, asked["under"], "under"); !slices.Equal(under, []string{name + "/child@both"}) {
		t.Errorf("the snapshots under it: %v", under)
	}
	suite.Call(t, "snapshot_delete", map[string]any{"snapshot": name + "@both", "recursive": true, "confirm": true})
	if left := snapshots(name + "/child"); len(left) != 0 {
		t.Errorf("a recursive removal left %v under it", left)
	}

	suite.Call(t, "dataset_delete", map[string]any{"dataset": name, "recursive": true, "confirm": true})
}
