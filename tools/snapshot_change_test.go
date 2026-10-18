package tools

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// snapshotStore is a stand-in's snapshots by whole name, each with the moment it was written, kept as a NAS keeps them and answered as a NAS answers
// the questions the tools ask: one snapshot by its id, the names among some datasets' snapshots, or the names of one dataset's written after a moment.
type snapshotStore struct {
	mu   sync.Mutex
	txg  map[string]int
	held map[string]bool
	next int
}

// snapshotAnswered is a snapshot as a NAS answers one asked after by its id.
func snapshotAnswered(name string, txg int, held bool) map[string]any {
	dataset, short, _ := strings.Cut(name, "@")
	refs := "0"
	if held {
		refs = "1"
	}

	return map[string]any{
		"id": name, "name": name, "dataset": dataset, "snapshot_name": short, "pool": "tank", "type": "SNAPSHOT", "createtxg": strconv.Itoa(txg),
		"properties": map[string]any{"creation": prop("1790000000"), "used": prop("2048"), "referenced": prop("9000"), "userrefs": prop(refs)},
	}
}

// storeSnapshots has the stand-in keep snapshots, starting with these, each written after the one before.
func storeSnapshots(t *testing.T, nas *truenastest.NAS, names ...string) *snapshotStore {
	t.Helper()

	s := &snapshotStore{txg: map[string]int{}, held: map[string]bool{}}
	for _, name := range names {
		s.next++
		s.txg[name] = s.next
	}
	nas.Handle("pool.snapshot.query", func(params []json.RawMessage) (any, error) {
		var options struct {
			Select []string       `json:"select"`
			Extra  map[string]any `json:"extra"`
		}
		_ = json.Unmarshal(params[1], &options)
		filters := filtersOf(t, params[0])

		s.mu.Lock()
		defer s.mu.Unlock()
		if len(filters) == 1 && filters[0][0] == "id" {
			name := acctest.Str(filters[0][2])
			if txg, ok := s.txg[name]; ok {
				return []map[string]any{snapshotAnswered(name, txg, s.held[name])}, nil
			}

			return []map[string]any{}, nil
		}
		if !slices.Equal(options.Select, []string{"name"}) {
			t.Errorf("snapshots were asked for with %s, which reads every snapshot a NAS has", params[1])
		}
		after, ok := options.Extra["min_txg"].(float64)
		if !ok && options.Extra["min_txg"] != nil {
			t.Errorf("snapshots were asked for from %v", options.Extra["min_txg"])
		}
		names := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(s.txg)) {
			dataset, _, _ := strings.Cut(name, "@")
			passes := float64(s.txg[name]) >= after
			for _, f := range filters {
				switch {
				case f[0] == "dataset" && f[1] == "=":
					passes = passes && dataset == f[2]
				case f[1] == "in":
					of := acctest.Strs(t, f[2], "in")
					passes = passes && (f[0] == "dataset" && slices.Contains(of, dataset) || f[0] == "name" && slices.Contains(of, name))
				default:
				}
			}
			if passes {
				names = append(names, map[string]any{"name": name})
			}
		}

		return names, nil
	})
	nas.Handle("pool.snapshot.create", func(params []json.RawMessage) (any, error) {
		var data struct {
			Dataset string `json:"dataset"`
			Name    string `json:"name"`
		}
		_ = json.Unmarshal(params[0], &data)

		s.mu.Lock()
		defer s.mu.Unlock()
		s.next++
		name := data.Dataset + "@" + data.Name
		s.txg[name] = s.next

		return snapshotAnswered(name, s.next, false), nil
	})
	nas.Handle("pool.snapshot.delete", func(params []json.RawMessage) (any, error) {
		var name string
		_ = json.Unmarshal(params[0], &name)

		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.txg, name)

		return true, nil
	})
	nas.Answer("pool.snapshot.rollback", nil)

	return s
}

// has is whether the stand-in keeps a snapshot by this whole name.
func (s *snapshotStore) has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.txg[name]

	return ok
}

// A snapshot is taken of a dataset that is there, by a name not yet taken, and read back; a dry run takes none, and a name with an @ in it, one
// taken already and a dataset there is none of are each refused.
func TestSnapshotCreateTakesOneByANameNotYetTaken(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	storeDatasets(t, nas, "tank", "tank/media")
	snapshots := storeSnapshots(t, nas, "tank/media@first")
	suite := session(t, nas, everything)

	dry := suite.Call(t, "snapshot_create", map[string]any{"dataset": "tank/media", "name": "before-upgrade", "dry_run": true})
	if changes := acctest.Strs(t, dry["changes"], "changes"); !slices.Equal(changes, []string{"would take the snapshot tank/media@before-upgrade"}) || snapshots.has("tank/media@before-upgrade") {
		t.Errorf("a dry run answered %v", dry)
	}

	taken := suite.Call(t, "snapshot_create", map[string]any{"dataset": "tank/media", "name": "before-upgrade", "recursive": true})
	if row := acctest.Object(t, taken["snapshot"], "snapshot"); row["name"] != "tank/media@before-upgrade" || row["snapshot"] != "before-upgrade" {
		t.Errorf("took %v", taken)
	}
	if sent := nas.Calls("pool.snapshot.create"); len(sent) != 1 || string(sent[0].Params[0]) != `{"dataset":"tank/media","name":"before-upgrade","recursive":true}` {
		t.Errorf("the NAS was asked to take %v", sent)
	}

	for want, args := range map[string]map[string]any{
		"with no @, / or space":              {"dataset": "tank/media", "name": "a@b"},
		"already has a snapshot":             {"dataset": "tank/media", "name": "first"},
		`the NAS has no dataset "tank/nope"`: {"dataset": "tank/nope", "name": "x"},
	} {
		if msg := suite.CallErr(t, "snapshot_create", args); !strings.Contains(msg, want) {
			t.Errorf("snapshot_create %v = %q, want it to say %q", args, msg, want)
		}
	}
}

// Without confirm a delete says what would go, with the snapshots of the same name under it when recursive, and removes nothing; a held snapshot is
// refused, since the NAS would not remove it. Removed, it is looked for again.
func TestSnapshotDeleteSaysWhatWouldGoAndRemovesOnlyWithConfirm(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	storeDatasets(t, nas, "tank", "tank/media", "tank/media/films", "tank/media/music")
	snapshots := storeSnapshots(t, nas, "tank/media@nightly", "tank/media/films@nightly", "tank/media/films@other", "tank/media@sent")
	snapshots.held["tank/media@sent"] = true
	suite := session(t, nas, everything)

	asked := suite.Call(t, "snapshot_delete", map[string]any{"snapshot": "tank/media@nightly", "recursive": true})
	if under := acctest.Strs(t, asked["under"], "under"); !slices.Equal(under, []string{"tank/media/films@nightly"}) {
		t.Errorf("the snapshots under it: %v", under)
	}
	if wouldGo := acctest.Strs(t, asked["would_go"], "would_go"); len(wouldGo) != 2 || wouldGo[0] != "the snapshot tank/media@nightly, taken 2026-09-21T14:13:20Z, and the 2.0 KiB only it holds" || acctest.BoolOf(asked["removed"]) {
		t.Errorf("without confirm: %v", asked)
	}
	if len(nas.Calls("pool.snapshot.delete")) != 0 {
		t.Fatal("a delete went ahead without confirm")
	}

	if msg := suite.CallErr(t, "snapshot_delete", map[string]any{"snapshot": "tank/media@sent", "confirm": true}); !strings.Contains(msg, "is held") {
		t.Errorf("a held snapshot: %q", msg)
	}
	if msg := suite.CallErr(t, "snapshot_delete", map[string]any{"snapshot": "tank/media", "confirm": true}); !strings.Contains(msg, "no snapshot's whole name") {
		t.Errorf("a dataset's name given as a snapshot's: %q", msg)
	}
	if msg := suite.CallErr(t, "snapshot_delete", map[string]any{"snapshot": "tank/media@gone", "confirm": true}); !strings.Contains(msg, "has no snapshot tank/media@gone") {
		t.Errorf("a snapshot there is none of: %q", msg)
	}

	gone := suite.Call(t, "snapshot_delete", map[string]any{"snapshot": "tank/media@nightly", "confirm": true})
	if !acctest.BoolOf(gone["removed"]) || snapshots.has("tank/media@nightly") {
		t.Errorf("confirmed: %v", gone)
	}
}

// Going back names the snapshots written after the one gone back to, asked for as names from that moment on, and goes back past them only with
// remove_newer; without confirm nothing changes.
func TestSnapshotRollbackNamesTheNewerSnapshotsFirst(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	storeDatasets(t, nas, "tank", "tank/media")
	storeSnapshots(t, nas, "tank/media@a", "tank/media@b", "tank/media@c", "tank/other@d")
	suite := session(t, nas, everything)

	asked := suite.Call(t, "snapshot_rollback", map[string]any{"snapshot": "tank/media@b"})
	if newer := acctest.Strs(t, asked["newer"], "newer"); !slices.Equal(newer, []string{"tank/media@c"}) || acctest.BoolOf(asked["rolled_back"]) || !strings.Contains(acctest.Str(asked["note"]), "remove_newer") {
		t.Errorf("without confirm: %v", asked)
	}
	if msg := suite.CallErr(t, "snapshot_rollback", map[string]any{"snapshot": "tank/media@b", "confirm": true}); !strings.Contains(msg, "will not go back past the snapshots of tank/media taken after tank/media@b: tank/media@c") {
		t.Errorf("confirmed without remove_newer: %q", msg)
	}
	if len(nas.Calls("pool.snapshot.rollback")) != 0 {
		t.Fatal("went back without being confirmed as it needs")
	}

	back := suite.Call(t, "snapshot_rollback", map[string]any{"snapshot": "tank/media@b", "remove_newer": true, "confirm": true})
	if !acctest.BoolOf(back["rolled_back"]) || back["would_go"] != nil {
		t.Errorf("confirmed: %v", back)
	}
	if sent := nas.Calls("pool.snapshot.rollback"); len(sent) != 1 || string(sent[0].Params[1]) != `{"recursive":true}` {
		t.Errorf("the NAS was asked to go back with %v", sent)
	}
	if last := newerAsked(nas); !strings.Contains(last, `"min_txg":3`) {
		t.Errorf("the newer snapshots were asked for with %s", last)
	}
}

// newerAsked is the options of the last query for snapshot names from a moment on.
func newerAsked(nas *truenastest.NAS) string {
	last := ""
	for _, options := range asked(nas, "pool.snapshot.query") {
		if strings.Contains(options, "min_txg") {
			last = options
		}
	}

	return last
}
