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

// datasetStore is a stand-in's datasets by whole name, kept as a NAS keeps them: made, changed and removed by the calls a NAS takes, and answered
// to a query as one row when one is asked for and a list otherwise.
type datasetStore struct {
	mu   sync.Mutex
	rows map[string]map[string]any
}

// filtersOf is a query's filters, each as [field, operator, value].
func filtersOf(t *testing.T, raw json.RawMessage) [][]any {
	t.Helper()

	var filters [][]any
	if err := json.Unmarshal(raw, &filters); err != nil {
		t.Errorf("filters %s: %v", raw, err)
	}

	return filters
}

// datasetNamedRow is a dataset as a NAS answers one, with the few properties the tools read.
func datasetNamedRow(name, kind string, settings map[string]any) map[string]any {
	pool, _, _ := strings.Cut(name, "/")
	row := map[string]any{"id": name, "name": name, "pool": pool, "type": kind, "encrypted": false, "locked": false, "used": prop("4096"), "available": prop("1000000"), "quota": prop("0"), "refquota": prop("0"), "volsize": prop("0"), "compression": prop("lz4"), "compressratio": prop("1.00x"), "readonly": prop("off"), "comments": prop("")}
	if kind == "FILESYSTEM" {
		row["mountpoint"] = "/mnt/" + name
	}
	maps.Copy(row, settings)

	return row
}

// storeDatasets has the stand-in keep datasets, starting with these whole names, each a dataset of files.
func storeDatasets(t *testing.T, nas *truenastest.NAS, names ...string) *datasetStore {
	t.Helper()

	s := &datasetStore{rows: map[string]map[string]any{}}
	for _, name := range names {
		s.rows[name] = datasetNamedRow(name, "FILESYSTEM", nil)
	}
	nas.Handle("pool.dataset.query", func(params []json.RawMessage) (any, error) {
		var options struct {
			Get bool `json:"get"`
		}
		_ = json.Unmarshal(params[1], &options)

		s.mu.Lock()
		defer s.mu.Unlock()
		found := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(s.rows)) {
			passes := true
			for _, f := range filtersOf(t, params[0]) {
				value, ok := f[2].(string)
				if !ok {
					t.Errorf("a dataset was asked after by %v", f[2])
				}
				switch f[1] {
				case "=":
					passes = passes && name == value
				case "^":
					passes = passes && strings.HasPrefix(name, value)
				default:
				}
			}
			if passes {
				found = append(found, s.rows[name])
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
	nas.Handle("pool.dataset.create", func(params []json.RawMessage) (any, error) {
		var data map[string]any
		if err := json.Unmarshal(params[0], &data); err != nil {
			return nil, err
		}
		name := acctest.Str(data["name"])
		settings := map[string]any{}
		if q, ok := data["quota"].(float64); ok {
			settings["quota"] = prop(strconv.FormatInt(int64(q), 10))
		}
		if v, ok := data["volsize"].(float64); ok {
			settings["volsize"] = prop(strconv.FormatInt(int64(v), 10))
		}
		if c, ok := data["compression"].(string); ok {
			settings["compression"] = prop(strings.ToLower(c))
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		s.rows[name] = datasetNamedRow(name, acctest.Str(data["type"]), settings)

		return s.rows[name], nil
	})
	nas.Handle("pool.dataset.update", func(params []json.RawMessage) (any, error) {
		var name string
		var data map[string]any
		if json.Unmarshal(params[0], &name) != nil || json.Unmarshal(params[1], &data) != nil {
			t.Errorf("pool.dataset.update was sent %s", params)
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		row := s.rows[name]
		for key, value := range data {
			switch v := value.(type) {
			case float64:
				row[key] = prop(strconv.FormatInt(int64(v), 10))
			case string:
				row[key] = prop(strings.ToLower(v))
				if key == "comments" {
					row[key] = prop(v)
				}
			default:
			}
		}

		return row, nil
	})
	nas.Handle("pool.dataset.delete", func(params []json.RawMessage) (any, error) {
		var name string
		_ = json.Unmarshal(params[0], &name)

		s.mu.Lock()
		defer s.mu.Unlock()
		for other := range s.rows {
			if other == name || strings.HasPrefix(other, name+"/") {
				delete(s.rows, other)
			}
		}

		return true, nil
	})
	nas.Answer("pool.dataset.attachments", []map[string]any{})
	nas.Answer("pool.snapshot.query", 0)

	return s
}

// has is whether the stand-in keeps a dataset by this whole name.
func (s *datasetStore) has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.rows[name]

	return ok
}

// A dataset is made inside one that is there, with the settings asked, and read back as a list answers it; a dry run says what would be made and
// makes nothing. A name already taken, one with nothing to go in, a quota on a volume and a pool's own dataset are each refused before the NAS is
// asked to make anything.
func TestDatasetCreateMakesADatasetInsideOneThatIsThere(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	store := storeDatasets(t, nas, "tank", "tank/media")
	suite := session(t, nas, everything)

	dry := suite.Call(t, "dataset_create", map[string]any{"dataset": "tank/media/films", "quota_bytes": 1 << 30, "dry_run": true})
	if changes := acctest.Strs(t, dry["changes"], "changes"); len(changes) != 1 || changes[0] != "would make the dataset tank/media/films in tank/media, with a quota of 1.0 GiB" || !acctest.BoolOf(dry["dry_run"]) || dry["dataset"] != nil {
		t.Errorf("a dry run answered %v", dry)
	}
	if store.has("tank/media/films") || len(nas.Calls("pool.dataset.create")) != 0 {
		t.Fatal("a dry run made the dataset")
	}

	made := suite.Call(t, "dataset_create", map[string]any{"dataset": "tank/media/films", "quota_bytes": 1 << 30, "compression": "zstd"})
	row := acctest.Object(t, made["dataset"], "dataset")
	if row["name"] != "tank/media/films" || acctest.Num(t, row["quota_bytes"], "quota_bytes") != 1<<30 || row["compression"] != "ZSTD" || acctest.Strs(t, made["changes"], "changes")[0] != "made the dataset tank/media/films in tank/media, with a quota of 1.0 GiB, compression ZSTD" {
		t.Errorf("made %v", made)
	}
	if sent := nas.Calls("pool.dataset.create"); len(sent) != 1 || !strings.Contains(string(sent[0].Params[0]), `"type":"FILESYSTEM"`) || strings.Contains(string(sent[0].Params[0]), "refquota") {
		t.Errorf("the NAS was asked to make %v", sent)
	}

	volume := suite.Call(t, "dataset_create", map[string]any{"dataset": "tank/vm-disk", "volume_bytes": 8 << 30, "sparse": true})
	if row := acctest.Object(t, volume["dataset"], "dataset"); row["type"] != "VOLUME" {
		t.Errorf("a volume reads back as %v", row)
	}
	if sent := nas.Calls("pool.dataset.create"); !strings.Contains(string(sent[1].Params[0]), `"volsize":8589934592`) || !strings.Contains(string(sent[1].Params[0]), `"sparse":true`) {
		t.Errorf("the NAS was asked to make the volume as %s", sent[1].Params[0])
	}

	for want, args := range map[string]map[string]any{
		"already has a dataset tank/media":    {"dataset": "tank/media"},
		"inside a pool or another dataset":    {"dataset": "tank"},
		"would go in tank/nowhere":            {"dataset": "tank/nowhere/films"},
		"takes no quota":                      {"dataset": "tank/vm", "volume_bytes": 1 << 30, "quota_bytes": 1 << 20},
		"sparse is for a volume":              {"dataset": "tank/files", "sparse": true},
		"a size is a number of bytes above 0": {"dataset": "tank/files", "quota_bytes": -1},
	} {
		if msg := suite.CallErr(t, "dataset_create", args); !strings.Contains(msg, want) {
			t.Errorf("dataset_create %v = %q, want it to say %q", args, msg, want)
		}
	}
	if n := len(nas.Calls("pool.dataset.create")); n != 2 {
		t.Errorf("a refused create reached the NAS: %d creates", n)
	}
}

// An update moves only what it is given and says each setting from what to what; asked for what is already so, it changes nothing and says so.
// Comments are taken away by a call of their own, since the client leaves out a setting given empty. A volume is never shrunk, and a dataset of
// files has no size to grow.
func TestDatasetUpdateMovesOnlyWhatItIsGiven(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	store := storeDatasets(t, nas, "tank", "tank/media")
	// as a 25.10 answers them: among the user properties, not beside the other properties as the description has it
	store.rows["tank/media"]["user_properties"] = map[string]any{"comments": prop("films and music")}
	store.rows["tank/vm"] = datasetNamedRow("tank/vm", "VOLUME", map[string]any{"volsize": prop("1073741824")})
	suite := session(t, nas, everything)

	dry := suite.Call(t, "dataset_update", map[string]any{"dataset": "tank/media", "quota_bytes": 2 << 30, "read_only": true, "compression": "LZ4", "dry_run": true})
	if changes := acctest.Strs(t, dry["changes"], "changes"); !slices.Equal(changes, []string{"would set the quota from none to 2.0 GiB", "would set read only from off to on"}) {
		t.Errorf("a dry run would change %v", changes)
	}
	if len(nas.Calls("pool.dataset.update")) != 0 {
		t.Fatal("a dry run changed the dataset")
	}

	changed := suite.Call(t, "dataset_update", map[string]any{"dataset": "tank/media", "quota_bytes": 2 << 30, "comments": ""})
	if changes := acctest.Strs(t, changed["changes"], "changes"); !slices.Equal(changes, []string{"set the quota from none to 2.0 GiB", `set the comments from "films and music" to none`}) {
		t.Errorf("changed %v", changes)
	}
	if row := acctest.Object(t, changed["dataset"], "dataset"); acctest.Num(t, row["quota_bytes"], "quota_bytes") != 2<<30 {
		t.Errorf("read back as %v", row)
	}
	sent := nas.Calls("pool.dataset.update")
	if len(sent) != 2 || !strings.Contains(string(sent[0].Params[1]), `"quota":2147483648`) || string(sent[1].Params[1]) != `{"comments":""}` {
		t.Errorf("the NAS was sent %v", sent)
	}

	same := suite.Call(t, "dataset_update", map[string]any{"dataset": "tank/media", "quota_bytes": 2 << 30})
	if len(acctest.Strs(t, same["changes"], "changes")) != 0 || !strings.Contains(acctest.Str(same["note"]), "nothing needed changing") || len(nas.Calls("pool.dataset.update")) != 2 {
		t.Errorf("asked for what is already so: %v", same)
	}

	for want, args := range map[string]map[string]any{
		"will not shrink a volume":        {"dataset": "tank/vm", "volume_bytes": 1 << 20},
		"takes no quota":                  {"dataset": "tank/vm", "quota_bytes": 1 << 20},
		"no size of its own to grow":      {"dataset": "tank/media", "volume_bytes": 1 << 40},
		`the NAS has no dataset "tank/x"`: {"dataset": "tank/x", "read_only": true},
	} {
		if msg := suite.CallErr(t, "dataset_update", args); !strings.Contains(msg, want) {
			t.Errorf("dataset_update %v = %q, want it to say %q", args, msg, want)
		}
	}
}

// Without confirm a delete says what would go, the datasets under it and what the NAS has set on it, and removes nothing; one with datasets under
// it is removed only with recursive, and a pool's own dataset not at all. Removed, it is looked for again.
func TestDatasetDeleteSaysWhatWouldGoAndRemovesOnlyWithConfirm(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	store := storeDatasets(t, nas, "tank", "tank/media", "tank/media/films", "tank/media/music")
	nas.Answer("pool.dataset.attachments", []map[string]any{{"type": "SMB Share", "service": "cifs", "attachments": []string{"media"}}})
	nas.Answer("pool.snapshot.query", 4)
	suite := session(t, nas, everything)

	asked := suite.Call(t, "dataset_delete", map[string]any{"dataset": "tank/media"})
	wouldGo := acctest.Strs(t, asked["would_go"], "would_go")
	if !slices.Equal(wouldGo, []string{"the dataset tank/media and all it holds, 4.0 KiB", "4 snapshots of it", "SMB Share set on it, which the NAS removes with it: media"}) || acctest.BoolOf(asked["removed"]) || !strings.Contains(acctest.Str(asked["note"]), "confirm with recursive") {
		t.Errorf("without confirm: %v", asked)
	}
	if under := acctest.Strs(t, asked["under"], "under"); !slices.Equal(under, []string{"tank/media/films", "tank/media/music"}) {
		t.Errorf("under it: %v", under)
	}

	if msg := suite.CallErr(t, "dataset_delete", map[string]any{"dataset": "tank/media", "confirm": true}); !strings.Contains(msg, "has 2 datasets and volumes under it") {
		t.Errorf("confirmed with datasets under it and no recursive: %q", msg)
	}
	if msg := suite.CallErr(t, "dataset_delete", map[string]any{"dataset": "tank", "confirm": true}); !strings.Contains(msg, "goes only with the pool") {
		t.Errorf("a pool's own dataset: %q", msg)
	}
	if len(nas.Calls("pool.dataset.delete")) != 0 || !store.has("tank/media") {
		t.Fatal("a delete went ahead without being confirmed as it needs")
	}

	gone := suite.Call(t, "dataset_delete", map[string]any{"dataset": "tank/media", "recursive": true, "confirm": true})
	if !acctest.BoolOf(gone["removed"]) || gone["would_go"] != nil || store.has("tank/media/films") {
		t.Errorf("confirmed: %v", gone)
	}
	if sent := nas.Calls("pool.dataset.delete"); len(sent) != 1 || !strings.Contains(string(sent[0].Params[1]), `"recursive":true`) {
		t.Errorf("the NAS was asked to delete %v", sent)
	}
}
