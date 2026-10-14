package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// pools is what a NAS with two pools answers: one whole and scrubbed, one with a mirror that lost a disk and found errors on the other.
const pools = `[
	{"id": 1, "name": "tank", "status": "ONLINE", "healthy": true, "warning": false, "size": 1000, "allocated": 250, "free": 750, "fragmentation": "15", "scan": {"function": "SCRUB", "state": "FINISHED", "start_time": {"$date": 1790845202000}, "end_time": {"$date": 1790848802000}, "percentage": 100.0, "errors": 0}, "autotrim": {"value": "on"}, "topology": {"data": [{"name": "mirror-0", "type": "MIRROR", "status": "ONLINE", "stats": {"size": 1000, "allocated": 250}, "children": [{"name": "sda1", "type": "DISK", "status": "ONLINE", "disk": "sda", "children": []}, {"name": "sdb1", "type": "DISK", "status": "ONLINE", "disk": "sdb", "children": []}]}], "log": [], "cache": [], "spare": [{"name": "sdc1", "type": "DISK", "status": "AVAIL", "disk": "sdc", "children": []}], "special": [], "dedup": []}},
	{"id": 2, "name": "backup", "status": "DEGRADED", "healthy": false, "warning": false, "status_code": "FAILING_DEV", "status_detail": "One or more devices has been removed.", "size": 400, "allocated": 360, "free": 40, "fragmentation": "3%", "scan": null, "autotrim": {"value": "off"}, "topology": {"data": [{"name": "mirror-0", "type": "MIRROR", "status": "DEGRADED", "stats": {"checksum_errors": 4}, "children": [{"name": "sdd1", "type": "DISK", "status": "ONLINE", "disk": "sdd", "stats": {"read_errors": 1, "checksum_errors": 4}, "children": []}, {"name": "8271", "type": "DISK", "status": "REMOVED", "disk": null, "children": [], "unavail_disk": {"name": "sde", "serial": "WD-1"}}]}], "log": [], "cache": [], "spare": [], "special": [], "dedup": []}}
]`

// standInWithPools is a NAS that answers pool.query with the two pools, one alone for a query told to answer one row, and nothing for one that names
// no pool it has.
func standInWithPools(t *testing.T) *truenastest.NAS {
	t.Helper()

	return standInWith(t, pools)
}

// standInWith is standInWithPools with the pools given, as a NAS writes them.
func standInWith(t *testing.T, written string) *truenastest.NAS {
	t.Helper()

	nas := truenastest.New(t)
	nas.Handle("pool.query", func(params []json.RawMessage) (any, error) {
		var all []map[string]any
		if err := json.Unmarshal([]byte(written), &all); err != nil {
			return nil, err
		}
		if len(params) < 2 || !strings.Contains(string(params[1]), `"get":true`) {
			return all, nil
		}

		var filters [][]any
		if err := json.Unmarshal(params[0], &filters); err != nil {
			return nil, err
		}
		for _, p := range all {
			if field, named := filters[0][0].(string); named && p[field] == filters[0][2] {
				return p, nil
			}
		}

		return nil, truenastest.ErrNoMatch
	})

	return nas
}

// A list of pools says of each how full it is, how many disks it is made of and when it was last scrubbed, whichever way the NAS writes its
// fragmentation.
func TestPoolListSaysHowFullEachPoolIsAndWhenItWasScrubbed(t *testing.T) {
	t.Parallel()

	out := session(t, standInWithPools(t), Options{}).Call(t, "pool_list", nil)
	rows := acctest.Rows(t, out["pools"], "pools")
	if len(rows) != 2 || acctest.Num(t, out["total"], "total") != 2 {
		t.Fatalf("listed %v", out)
	}

	tank, backup := rows[0], rows[1]
	if tank["name"] != "tank" || acctest.Decimal(t, tank["used_percent"], "used_percent") != 25 || acctest.Num(t, tank["fragmentation_percent"], "fragmentation_percent") != 15 || acctest.Num(t, tank["disks"], "disks") != 3 || tank["last_scrub"] != "2026-10-01T10:00:02Z" {
		t.Errorf("tank reads as %v", tank)
	}
	if !acctest.IsBool(backup["healthy"], false) || backup["status"] != "DEGRADED" || acctest.Num(t, backup["fragmentation_percent"], "fragmentation_percent") != 3 || backup["last_scrub"] != nil || backup["status_detail"] == nil {
		t.Errorf("backup reads as %v", backup)
	}
}

// A pool is described down to each disk, by its name or its number, and a disk that is gone names the disk the NAS last knew in its place.
func TestPoolGetDescribesAPoolDownToEachDisk(t *testing.T) {
	t.Parallel()

	s := session(t, standInWithPools(t), Options{})

	tank := s.Call(t, "pool_get", map[string]any{"pool": "tank"})
	vdevs := acctest.Rows(t, tank["vdevs"], "vdevs")
	if len(vdevs) != 2 || vdevs[0]["role"] != "data" || vdevs[0]["type"] != "MIRROR" || len(acctest.Rows(t, vdevs[0]["disks"], "disks")) != 2 || vdevs[1]["role"] != "spare" || !acctest.BoolOf(tank["autotrim"]) {
		t.Errorf("tank reads as %v", tank)
	}
	if scan := acctest.Object(t, tank["scan"], "scan"); scan["function"] != "SCRUB" || scan["state"] != "FINISHED" || scan["started"] != "2026-10-01T09:00:02Z" {
		t.Errorf("tank's scrub reads as %v", scan)
	}

	backup := s.Call(t, "pool_get", map[string]any{"pool": "2"})
	mirror := acctest.Rows(t, backup["vdevs"], "vdevs")[0]
	disks := acctest.Rows(t, mirror["disks"], "disks")
	if backup["name"] != "backup" || backup["status_code"] != "FAILING_DEV" || backup["scan"] != nil || mirror["status"] != "DEGRADED" || acctest.Num(t, mirror["checksum_errors"], "checksum_errors") != 4 {
		t.Errorf("backup, by its number, reads as %v", backup)
	}
	if acctest.Num(t, disks[0]["read_errors"], "read_errors") != 1 || disks[1]["status"] != "REMOVED" || disks[1]["missing"] != "sde WD-1" || disks[1]["disk"] != nil {
		t.Errorf("backup's disks read as %v", disks)
	}

	if msg := s.CallErr(t, "pool_get", map[string]any{"pool": "nowhere"}); !strings.Contains(msg, `no pool "nowhere"`) || !strings.Contains(msg, "pool_list") {
		t.Errorf("a pool there is none of: %q", msg)
	}
}
