package tools

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// rowStore is a stand-in's rows of one kind by number, made, changed and removed by the calls a NAS takes for them, and answered to a query for one
// by its number, or for all.
type rowStore struct {
	mu   sync.Mutex
	rows map[int]map[string]any
	next int
}

// storeRows has the stand-in keep rows of one kind, under the methods named for it, starting with these; made tells a new row from what create was
// sent, and changed applies what update was sent.
func storeRows(t *testing.T, nas *truenastest.NAS, kind string, made, changed func(row, sent map[string]any), rows ...map[string]any) *rowStore {
	t.Helper()

	s := &rowStore{rows: map[int]map[string]any{}}
	for _, row := range rows {
		s.next++
		row["id"] = float64(s.next)
		s.rows[s.next] = row
	}
	nas.Handle(kind+".query", func(params []json.RawMessage) (any, error) {
		var options struct {
			Get bool `json:"get"`
		}
		var filters [][]any
		if len(params) > 0 {
			filters = filtersOf(t, params[0])
		}
		if len(params) > 1 {
			_ = json.Unmarshal(params[1], &options)
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		found := []map[string]any{}
		for id := 1; id <= s.next; id++ {
			row, ok := s.rows[id]
			passes := ok
			for _, f := range filters {
				if text, ok := f[2].(string); ok {
					passes = passes && row[acctest.Str(f[0])] == text
				} else {
					passes = passes && acctest.NumOr0(row[acctest.Str(f[0])]) == acctest.NumOr0(f[2])
				}
			}
			if passes {
				found = append(found, row)
			}
		}
		switch {
		case !options.Get:
			return found, nil
		case len(found) == 0:
			return nil, truenastest.ErrNoMatch
		}

		return found[0], nil
	})
	nas.Handle(kind+".create", func(params []json.RawMessage) (any, error) {
		var sent map[string]any
		_ = json.Unmarshal(params[0], &sent)

		s.mu.Lock()
		defer s.mu.Unlock()
		s.next++
		row := map[string]any{"id": float64(s.next)}
		made(row, sent)
		s.rows[s.next] = row

		return row, nil
	})
	nas.Handle(kind+".update", func(params []json.RawMessage) (any, error) {
		var id int
		var sent map[string]any
		_ = json.Unmarshal(params[0], &id)
		_ = json.Unmarshal(params[1], &sent)

		s.mu.Lock()
		defer s.mu.Unlock()
		changed(s.rows[id], sent)

		return s.rows[id], nil
	})
	nas.Handle(kind+".delete", func(params []json.RawMessage) (any, error) {
		var id int
		_ = json.Unmarshal(params[0], &id)

		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.rows, id)

		return true, nil
	})

	return s
}

// copied has a row take what was sent as it is.
func copied(row, sent map[string]any) {
	maps.Copy(row, sent)
}

// snapshotTaskRowSent is a snapshot task as the stand-in keeps one.
func snapshotTaskRowSent(dataset string, enabled bool) map[string]any {
	return map[string]any{"dataset": dataset, "recursive": false, "exclude": []string{}, "lifetime_value": 2, "lifetime_unit": "WEEK", "naming_schema": defaultNaming, "allow_empty": true, "enabled": enabled, "schedule": map[string]any{"minute": "0", "hour": "*", "dom": "*", "month": "*", "dow": "*", "begin": "00:00", "end": "23:59"}}
}

// A snapshot task is made with the schedule, keeping and naming asked, and the NAS's own naming when none is given; a dry run makes none, and a
// schedule, a time to keep or a window that cannot be read, and an exclusion with nothing recursive to leave out of, are each refused.
func TestSnapshotTaskCreateMakesATaskOnTheScheduleAsked(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	storeDatasets(t, nas, "tank", "tank/media")
	storeRows(t, nas, "pool.snapshottask", copied, copied)
	suite := session(t, nas, everything)

	dry := suite.Call(t, "snapshottask_create", map[string]any{"dataset": "tank/media", "schedule": "0 3 * * 0", "keep_for": "4 weeks", "enabled": false, "dry_run": true})
	if changes := acctest.Strs(t, dry["changes"], "changes"); !slices.Equal(changes, []string{"would make a task that takes a snapshot of tank/media at 0 3 * * 0, kept for 4 WEEK, switched off"}) || len(nas.Calls("pool.snapshottask.create")) != 0 {
		t.Errorf("a dry run answered %v", dry)
	}

	made := suite.Call(t, "snapshottask_create", map[string]any{"dataset": "tank/media", "schedule": "0 3 * * 0", "keep_for": "4 weeks", "window": "01:00-05:00", "skip_unchanged": true})
	if row := acctest.Object(t, made["snapshot_task"], "snapshot_task"); row["dataset"] != "tank/media" || row["schedule"] != "0 3 * * 0" || row["keep_for"] != "4 WEEK" || row["window"] != "01:00-05:00" || !acctest.BoolOf(row["skip_unchanged"]) || !acctest.BoolOf(row["enabled"]) || row["naming_schema"] != defaultNaming {
		t.Errorf("made %v", made)
	}

	for want, args := range map[string]map[string]any{
		"is no schedule":                {"dataset": "tank/media", "schedule": "daily", "keep_for": "2 WEEK"},
		"is no time to keep a snapshot": {"dataset": "tank/media", "schedule": "0 0 * * *", "keep_for": "a fortnight"},
		"is no window":                  {"dataset": "tank/media", "schedule": "0 0 * * *", "keep_for": "2 WEEK", "window": "morning"},
		"give recursive too":            {"dataset": "tank/media", "schedule": "0 0 * * *", "keep_for": "2 WEEK", "exclude": []string{"tank/media/x"}},
		`no dataset "tank/none"`:        {"dataset": "tank/none", "schedule": "0 0 * * *", "keep_for": "2 WEEK"},
	} {
		if msg := suite.CallErr(t, "snapshottask_create", args); !strings.Contains(msg, want) {
			t.Errorf("snapshottask_create %v = %q, want it to say %q", args, msg, want)
		}
	}
	if n := len(nas.Calls("pool.snapshottask.create")); n != 1 {
		t.Errorf("a refused create reached the NAS: %d creates", n)
	}
}

// An update moves only what it is given, says each setting from what to what, and names the snapshots whose removal a change to the keeping moves,
// as the NAS answers them; asked for what is already so, it sends nothing.
func TestSnapshotTaskUpdateMovesOnlyWhatItIsGivenAndNamesTheSnapshotsItMoves(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	storeDatasets(t, nas, "tank", "tank/media")
	storeRows(t, nas, "pool.snapshottask", copied, copied, snapshotTaskRowSent("tank/media", true))
	nas.Answer("pool.snapshottask.update_will_change_retention_for", map[string][]string{"tank/media": {"auto-1", "auto-2"}})
	suite := session(t, nas, everything)

	changed := suite.Call(t, "snapshottask_update", map[string]any{"id": 1, "keep_for": "1 WEEK", "enabled": false, "schedule": "0 * * * *"})
	if changes := acctest.Strs(t, changed["changes"], "changes"); !slices.Equal(changes, []string{"set how long it keeps snapshots from 2 WEEK to 1 WEEK", "set whether it runs from on to off"}) {
		t.Errorf("changed %v", changes)
	}
	if note := acctest.Str(changed["note"]); !strings.Contains(note, "2 snapshots it took") || !strings.Contains(note, "tank/media@auto-1, tank/media@auto-2") {
		t.Errorf("the snapshots whose removal moves: %q", note)
	}
	if row := acctest.Object(t, changed["snapshot_task"], "snapshot_task"); row["keep_for"] != "1 WEEK" || acctest.BoolOf(row["enabled"]) {
		t.Errorf("read back as %v", row)
	}
	if sent := nas.Calls("pool.snapshottask.update"); len(sent) != 1 || string(sent[0].Params[1]) != `{"enabled":false,"fixate_removal_date":false,"lifetime_unit":"WEEK","lifetime_value":1}` {
		t.Errorf("the NAS was sent %v", sent)
	}

	same := suite.Call(t, "snapshottask_update", map[string]any{"id": 1, "keep_for": "1 week"})
	if len(acctest.Strs(t, same["changes"], "changes")) != 0 || len(nas.Calls("pool.snapshottask.update")) != 1 {
		t.Errorf("asked for what is already so: %v", same)
	}
	if msg := suite.CallErr(t, "snapshottask_update", map[string]any{"id": 9, "enabled": true}); !strings.Contains(msg, "no snapshot task 9") {
		t.Errorf("a task there is none of: %q", msg)
	}
}

// A task is removed only with confirm, and the answer says which snapshots it took are then kept until removed by hand; a task switched off is not
// run, and one switched on is.
func TestSnapshotTaskDeleteSaysWhatItLeavesAndRunOnlyRunsATaskThatIsOn(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	tasks := storeRows(t, nas, "pool.snapshottask", copied, copied, snapshotTaskRowSent("tank/media", true), snapshotTaskRowSent("tank/other", false))
	nas.Answer("pool.snapshottask.delete_will_change_retention_for", map[string][]string{"tank/media": {"auto-1"}})
	nas.Answer("pool.snapshottask.run", nil)
	suite := session(t, nas, everything)

	asked := suite.Call(t, "snapshottask_delete", map[string]any{"id": 1})
	if wouldGo := acctest.Strs(t, asked["would_go"], "would_go"); !slices.Equal(wouldGo, []string{"the task 1, which takes a snapshot of tank/media at 0 * * * *, kept for 2 WEEK"}) || !strings.Contains(acctest.Str(asked["note"]), "kept until removed by hand: tank/media@auto-1") {
		t.Errorf("without confirm: %v", asked)
	}
	gone := suite.Call(t, "snapshottask_delete", map[string]any{"id": 1, "keep_removal_due": true, "confirm": true})
	if !acctest.BoolOf(gone["removed"]) || len(tasks.rows) != 1 {
		t.Errorf("confirmed: %v", gone)
	}
	if sent := nas.Calls("pool.snapshottask.delete"); len(sent) != 1 || string(sent[0].Params[1]) != `{"fixate_removal_date":true}` {
		t.Errorf("the NAS was asked to delete with %v", sent)
	}

	if msg := suite.CallErr(t, "snapshottask_run", map[string]any{"id": 2}); !strings.Contains(msg, "switched off") {
		t.Errorf("a task switched off: %q", msg)
	}
	tasks.rows[2]["enabled"] = true
	if ran := suite.Call(t, "snapshottask_run", map[string]any{"id": 2}); len(acctest.Strs(t, ran["changes"], "changes")) != 1 || len(nas.Calls("pool.snapshottask.run")) != 1 {
		t.Errorf("run: %v", ran)
	}
}

// poolScanned is a pool as a NAS answers one, with its scan as given.
func poolScanned(scan map[string]any) map[string]any {
	return map[string]any{"id": 1, "name": "tank", "guid": "1", "status": "ONLINE", "healthy": true, "scan": scan}
}

// A pool's scrub task is made only where the pool has none, changed setting by setting, with a description taken away by a call of its own, and
// removed only with confirm.
func TestAScrubTaskIsMadeChangedAndRemoved(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("pool.query", poolScanned(nil))
	scrubs := storeRows(t, nas, "pool.scrub", func(row, sent map[string]any) {
		copied(row, sent)
		row["pool_name"], row["threshold"] = "tank", 35
	}, copied)
	suite := session(t, nas, everything)

	made := suite.Call(t, "pool_scrub_create", map[string]any{"pool": "tank", "description": "weekly"})
	if row := acctest.Object(t, made["scrub_task"], "scrub_task"); row["pool"] != "tank" || row["schedule"] != "0 0 * * 0" || acctest.NumOr0(row["threshold_days"]) != 35 || !acctest.BoolOf(row["enabled"]) {
		t.Errorf("made %v", made)
	}
	if msg := suite.CallErr(t, "pool_scrub_create", map[string]any{"pool": "tank"}); !strings.Contains(msg, "already has the scrub task 1") {
		t.Errorf("a second for one pool: %q", msg)
	}

	changed := suite.Call(t, "pool_scrub_update", map[string]any{"id": 1, "threshold_days": 7, "description": ""})
	if changes := acctest.Strs(t, changed["changes"], "changes"); !slices.Equal(changes, []string{`set the days between scrubs from 35 to 7`, `set the description from "weekly" to none`}) {
		t.Errorf("changed %v", changes)
	}
	if sent := nas.Calls("pool.scrub.update"); len(sent) != 2 || string(sent[0].Params[1]) != `{"threshold":7}` || string(sent[1].Params[1]) != `{"description":""}` {
		t.Errorf("the NAS was sent %v", sent)
	}

	if asked := suite.Call(t, "pool_scrub_delete", map[string]any{"id": 1}); acctest.BoolOf(asked["removed"]) || len(scrubs.rows) != 1 {
		t.Errorf("without confirm: %v", asked)
	}
	if gone := suite.Call(t, "pool_scrub_delete", map[string]any{"id": 1, "confirm": true}); !acctest.BoolOf(gone["removed"]) || len(scrubs.rows) != 0 {
		t.Errorf("confirmed: %v", gone)
	}
}

// A scrub is started where none runs and left running, a running one paused, and a paused one taken up again or stopped; asked for what is already
// so, nothing is sent.
func TestAScrubIsStartedPausedAndStopped(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	var mu sync.Mutex
	scan := map[string]any{"function": "SCRUB", "state": "FINISHED", "percentage": 100, "errors": 0, "pause": nil}
	nas.Handle("pool.query", func([]json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()

		return poolScanned(scan), nil
	})
	nas.Job("pool.scrub.scrub", func(params []json.RawMessage) (any, error) {
		var action string
		_ = json.Unmarshal(params[1], &action)

		mu.Lock()
		defer mu.Unlock()
		switch action {
		case "START":
			scan = map[string]any{"function": "SCRUB", "state": "SCANNING", "percentage": 10, "errors": 0, "pause": nil}
		case "PAUSE":
			scan["pause"] = map[string]any{"$date": 1790000000000}
		case "STOP":
			scan = map[string]any{"function": "SCRUB", "state": "CANCELED", "percentage": 10, "errors": 0, "pause": nil}
		}

		return nil, nil
	})
	suite := session(t, nas, everything)
	actions := func() []string {
		calls := nas.Calls("pool.scrub.scrub")
		sent := make([]string, 0, len(calls))
		for _, c := range calls {
			sent = append(sent, string(c.Params[1]))
		}

		return sent
	}

	if paused := suite.Call(t, "pool_scrub_pause", map[string]any{"pool": "tank"}); len(acctest.Strs(t, paused["changes"], "changes")) != 0 {
		t.Errorf("pausing with no scrub running: %v", paused)
	}
	started := suite.Call(t, "pool_scrub_start", map[string]any{"pool": "tank"})
	if scanned := acctest.Object(t, started["scan"], "scan"); scanned["state"] != "SCANNING" || acctest.Strs(t, started["changes"], "changes")[0] != "started scrubbing tank" {
		t.Errorf("started %v", started)
	}
	if again := suite.Call(t, "pool_scrub_start", map[string]any{"pool": "tank"}); !strings.Contains(acctest.Str(again["note"]), "being scrubbed already") {
		t.Errorf("starting a running scrub: %v", again)
	}
	paused := suite.Call(t, "pool_scrub_pause", map[string]any{"pool": "tank"})
	if scanned := acctest.Object(t, paused["scan"], "scan"); !acctest.BoolOf(scanned["paused"]) {
		t.Errorf("paused %v", paused)
	}
	if again := suite.Call(t, "pool_scrub_start", map[string]any{"pool": "tank", "dry_run": true}); acctest.Strs(t, again["changes"], "changes")[0] != "would take up the paused scrub of tank" {
		t.Errorf("taking up a paused scrub: %v", again)
	}
	stopped := suite.Call(t, "pool_scrub_stop", map[string]any{"pool": "tank"})
	if scanned := acctest.Object(t, stopped["scan"], "scan"); scanned["state"] != "CANCELED" {
		t.Errorf("stopped %v", stopped)
	}
	if sent := actions(); !slices.Equal(sent, []string{`"START"`, `"PAUSE"`, `"STOP"`}) {
		t.Errorf("the NAS was asked to %v", sent)
	}
}
