package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// prop is a ZFS property as a NAS writes one in a dataset's row.
func prop(raw string) map[string]any {
	return map[string]any{"rawvalue": raw, "value": raw, "source": "LOCAL", "parsed": raw}
}

// films is a dataset as a NAS writes one, encrypted under a passphrase and with a quota.
var films = map[string]any{
	"id": "tank/media/films", "name": "tank/media/films", "pool": "tank", "type": "FILESYSTEM", "encrypted": true, "locked": false, "mountpoint": "/mnt/tank/media/films", "encryption_root": "tank/media",
	"used": prop("3000"), "available": prop("7000"), "quota": prop("10000"), "refquota": prop("0"), "compression": prop("lz4"), "compressratio": prop("1.50x"), "readonly": prop("off"),
	"usedbysnapshots": prop("500"), "usedbychildren": prop("0"), "reservation": prop("0"), "refreservation": prop("0"), "recordsize": prop("1M"), "sync": prop("standard"), "deduplication": prop("off"),
	"atime": prop("off"), "key_format": prop("passphrase"), "creation": prop("1790845202"), "origin": prop(""), "comments": prop("for the films"),
}

// named is a dataset that is nothing but its name, as many as a test needs beside films.
func named(name string) map[string]any {
	pool, _, _ := strings.Cut(name, "/")

	return map[string]any{"id": name, "name": name, "pool": pool, "type": "FILESYSTEM"}
}

// datasetsAsANASAnswers answers a dataset query from rows the way a real NAS does, where it surprises too: asked for rows alone with no filter it
// hands back only each pool's own dataset, and asked to count it counts every dataset whatever the filters.
func datasetsAsANASAnswers(rows ...map[string]any) truenastest.Handler {
	return func(params []json.RawMessage) (any, error) {
		var filters [][]any
		var options struct {
			Count  bool     `json:"count"`
			Select []string `json:"select"`
			Limit  int      `json:"limit"`
			Offset int      `json:"offset"`
			Extra  struct {
				RetrieveChildren *bool `json:"retrieve_children"`
			} `json:"extra"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params[0], &filters); err != nil {
				return nil, err
			}
		}
		if len(params) > 1 {
			if err := json.Unmarshal(params[1], &options); err != nil {
				return nil, err
			}
		}

		if options.Count {
			return len(rows), nil
		}

		var out []map[string]any
		for _, row := range rows {
			if len(filters) == 0 && options.Extra.RetrieveChildren != nil && !*options.Extra.RetrieveChildren && strings.Contains(fmt.Sprint(row["name"]), "/") {
				continue
			}
			if passes(row, filters) {
				out = append(out, row)
			}
		}

		out = out[min(options.Offset, len(out)):]
		if options.Limit > 0 {
			out = out[:min(options.Limit, len(out))]
		}
		if len(options.Select) > 0 {
			for i, row := range out {
				kept := map[string]any{}
				for _, field := range options.Select {
					kept[field] = row[field]
				}
				out[i] = kept
			}
		}

		return out, nil
	}
}

// passes is whether a row passes every filter, of the few kinds the tools send.
func passes(row map[string]any, filters [][]any) bool {
	for _, f := range filters {
		if len(f) != 3 {
			return false
		}
		have := fmt.Sprint(row[fmt.Sprint(f[0])])

		switch f[1] {
		case "=":
			if have != fmt.Sprint(f[2]) {
				return false
			}
		case "^":
			if !strings.HasPrefix(have, fmt.Sprint(f[2])) {
				return false
			}
		case "in":
			if among, ok := f[2].([]any); !ok || !slices.Contains(among, any(have)) {
				return false
			}
		default:
			return false
		}
	}

	return true
}

// A list of datasets is paged by the NAS itself: it is asked which pass what to keep to and then for the one page. The list goes below the pools when
// nothing narrows it, and its total is of what passes, not every dataset the NAS has. Each row says its space and whether it is locked.
func TestDatasetListIsPagedByTheNASAndSaysWhatEachHolds(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		asked []string
	)
	answer := datasetsAsANASAnswers(named("other"), named("other/backups"), named("tank"), named("tank/media"), named("tank/media/books"), films, named("tank/media/tv"))
	nas := truenastest.New(t)
	nas.Handle("pool.dataset.query", func(params []json.RawMessage) (any, error) {
		mu.Lock()
		asked = append(asked, string(params[0])+" "+string(params[1]))
		mu.Unlock()

		return answer(params)
	})
	s := session(t, nas, Options{})

	if all := s.Call(t, "dataset_list", nil); acctest.Num(t, all["total"], "total") != 7 || len(acctest.Rows(t, all["datasets"], "datasets")) != 7 {
		t.Errorf("with nothing to narrow it, listed %v", all)
	}

	mu.Lock()
	asked = nil
	mu.Unlock()
	out := s.Call(t, "dataset_list", map[string]any{"pool": "tank", "under": "tank/media/", "limit": 1, "offset": 1})
	rows := acctest.Rows(t, out["datasets"], "datasets")
	if len(rows) != 1 || acctest.Num(t, out["total"], "total") != 3 || acctest.Num(t, out["offset"], "offset") != 1 || acctest.Num(t, out["next_offset"], "next_offset") != 2 {
		t.Fatalf("listed %v", out)
	}
	if f := rows[0]; f["name"] != "tank/media/films" || acctest.Num(t, f["used_bytes"], "used_bytes") != 3000 || acctest.Num(t, f["quota_bytes"], "quota_bytes") != 10000 || f["refquota_bytes"] != nil || f["compression"] != "LZ4" || acctest.Decimal(t, f["compress_ratio"], "compress_ratio") != 1.5 || !acctest.BoolOf(f["encrypted"]) || !acctest.IsBool(f["read_only"], false) {
		t.Errorf("the dataset reads as %v", f)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 || !strings.Contains(asked[0], `["pool","=","tank"]`) || !strings.Contains(asked[0], `["name","^","tank/media/"]`) || !strings.Contains(asked[0], `"select":["name"]`) || !strings.Contains(asked[1], `"limit":1`) || !strings.Contains(asked[1], `"offset":1`) || !strings.Contains(asked[1], `"retrieve_children":false`) {
		t.Errorf("the NAS was asked %q", asked)
	}
}

// One dataset is described down to what its snapshots hold and how its key is given, and one there is none of says where to look.
func TestDatasetGetDescribesOneAndSaysWhenThereIsNone(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Handle("pool.dataset.query", func(params []json.RawMessage) (any, error) {
		if strings.Contains(string(params[0]), "tank/media/films") {
			return films, nil
		}

		return nil, truenastest.ErrNoMatch
	})
	s := session(t, nas, Options{})

	got := s.Call(t, "dataset_get", map[string]any{"dataset": "tank/media/films"})
	if acctest.Num(t, got["snapshots_bytes"], "snapshots_bytes") != 500 || got["key_format"] != "PASSPHRASE" || got["encryption_root"] != "tank/media" || got["sync"] != "STANDARD" || got["record_size"] != "1M" || got["created"] != "2026-10-01T09:00:02Z" || got["comments"] != "for the films" || got["origin"] != nil {
		t.Errorf("the dataset reads as %v", got)
	}

	if msg := s.CallErr(t, "dataset_get", map[string]any{"dataset": "tank/nothing"}); !strings.Contains(msg, `no dataset "tank/nothing"`) || !strings.Contains(msg, "dataset_list") {
		t.Errorf("a dataset there is none of: %q", msg)
	}
}
