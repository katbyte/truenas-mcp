//go:build acceptance

package tools_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/tools"
)

// The test NAS's one pool is listed whole and described down to its two disks, which disk_list shows in it by their serial numbers, beside the disks
// no pool uses; a pool there is none of says so.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestThePoolIsListedAndDescribedDownToItsDisks(t *testing.T) {
	listed := suite.Call(t, "pool_list", nil)
	pools := acctest.Rows(t, listed["pools"], "pools")
	if len(pools) != 1 || pools[0]["name"] != layout.Pool || !acctest.BoolOf(pools[0]["healthy"]) || pools[0]["status"] != "ONLINE" || acctest.NumOr0(pools[0]["disks"]) != 2 || acctest.NumOr0(pools[0]["size_bytes"]) <= 0 {
		t.Fatalf("the pools are listed as %v", listed)
	}

	// by its number as well as its name
	described := suite.Call(t, "pool_get", map[string]any{"pool": layout.Pool})
	if byNumber := suite.Call(t, "pool_get", map[string]any{"pool": strconv.Itoa(acctest.NumOr0(pools[0]["id"]))}); byNumber["name"] != layout.Pool {
		t.Errorf("the pool fetched by its number is %v", byNumber["name"])
	}
	vdevs := acctest.Rows(t, described["vdevs"], "vdevs")
	if len(vdevs) != 1 || vdevs[0]["role"] != "data" || vdevs[0]["type"] != "MIRROR" || vdevs[0]["status"] != "ONLINE" {
		t.Fatalf("the pool is made of %v", vdevs)
	}
	mirrored := acctest.Rows(t, vdevs[0]["disks"], "disks")
	inPool := make([]string, 0, len(mirrored))
	for _, disk := range mirrored {
		if disk["status"] != "ONLINE" || acctest.NumOr0(disk["checksum_errors"]) != 0 || acctest.Str(disk["disk"]) == "" {
			t.Errorf("a disk of the mirror reads as %v", disk)
		}
		inPool = append(inPool, acctest.Str(disk["disk"]))
	}

	disks := acctest.Rows(t, suite.Call(t, "disk_list", nil)["disks"], "disks")
	serials := map[string]string{}
	for _, disk := range disks {
		if disk["pool"] == layout.Pool {
			serials[acctest.Str(disk["name"])] = acctest.Str(disk["serial"])
		}
	}
	for _, name := range inPool {
		if !strings.HasPrefix(serials[name], layout.DiskSerial) {
			t.Errorf("disk_list does not show %s in the pool with a test disk's serial: %v", name, serials)
		}
	}
	if len(disks) < 5 || len(serials) != 2 {
		t.Errorf("disk_list shows %d disks, %d of them in the pool", len(disks), len(serials))
	}
	// the NVMe disk is the one that says how warm it is
	if hot := slices.IndexFunc(disks, func(d map[string]any) bool { return d["serial"] == layout.HotDiskSerial }); hot < 0 || disks[hot]["temperature_celsius"] == nil {
		t.Errorf("the NVMe disk says no temperature: %v", disks)
	}

	if msg := suite.CallErr(t, "pool_get", map[string]any{"pool": "nowhere"}); !strings.Contains(msg, `no pool "nowhere"`) {
		t.Errorf("a pool there is none of: %q", msg)
	}
}

// The test NAS's datasets are listed by the NAS a page at a time, under the pool, and one is described; the user who may only read sees the same. With
// nothing to narrow it the list goes below the pools, which a NAS asked carelessly does not, and the total is of what is listed, not every dataset.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheDatasetsAreListedAPageAtATimeAndDescribed(t *testing.T) {
	everything := suite.Call(t, "dataset_list", nil)
	listed := acctest.Rows(t, everything["datasets"], "datasets")
	all := make([]string, 0, len(listed))
	for _, row := range listed {
		all = append(all, acctest.Str(row["name"]))
	}
	if !slices.Contains(all, layout.Pool) || !slices.Contains(all, layout.Datasets[0]) || acctest.NumOr0(everything["total"]) != len(all) {
		t.Errorf("with nothing to narrow it, the datasets listed are %v of %v", all, everything["total"])
	}

	var names []string
	total := -1
	for offset := 0; ; {
		page := suite.Call(t, "dataset_list", map[string]any{"under": layout.Pool, "limit": 1, "offset": offset})
		if total < 0 {
			total = acctest.NumOr0(page["total"])
		}
		for _, row := range acctest.Rows(t, page["datasets"], "datasets") {
			names = append(names, acctest.Str(row["name"]))
		}
		next := acctest.NumOr0(page["next_offset"])
		if next == 0 {
			break
		}
		offset = next
	}
	for _, want := range layout.Datasets {
		if !slices.Contains(names, want) {
			t.Errorf("paging through the datasets under %s found %v, and not %s", layout.Pool, names, want)
		}
	}
	if slices.Contains(names, layout.Pool) {
		t.Errorf("the datasets under %s include the pool's own: %v", layout.Pool, names)
	}
	if total != len(names) {
		t.Errorf("the datasets under %s are said to be %d, and paging through them found %v", layout.Pool, total, names)
	}

	got := suite.Call(t, "dataset_get", map[string]any{"dataset": layout.Datasets[0]})
	if got["name"] != layout.Datasets[0] || got["pool"] != layout.Pool || got["type"] != "FILESYSTEM" || acctest.NumOr0(got["available_bytes"]) <= 0 || acctest.Str(got["compression"]) == "" || acctest.Str(got["created"]) == "" {
		t.Errorf("the dataset reads as %v", got)
	}
	if msg := suite.CallErr(t, "dataset_get", map[string]any{"dataset": layout.Pool + "/nothing"}); !strings.Contains(msg, "no dataset") {
		t.Errorf("a dataset there is none of: %q", msg)
	}

	reading := asReader(t, tools.Options{}).Call(t, "dataset_list", map[string]any{"pool": layout.Pool})
	if acctest.NumOr0(reading["total"]) < len(layout.Datasets)+1 {
		t.Errorf("the user who may only read sees %v", reading)
	}
}

// The NAS's alerts are listed, the gravest first, and the user who may only read is shown them too.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheAlertsAreListedTheGravestFirst(t *testing.T) {
	out := suite.Call(t, "alert_list", map[string]any{"dismissed": true})
	levels := []string{"EMERGENCY", "ALERT", "CRITICAL", "ERROR", "WARNING", "NOTICE", "INFO"}
	last := -1
	for _, row := range acctest.Rows(t, out["alerts"], "alerts") {
		level := slices.Index(levels, acctest.Str(row["level"]))
		if level < last || acctest.Str(row["class"]) == "" || acctest.Str(row["text"]) == "" {
			t.Errorf("an alert out of order or with nothing said: %v", row)
		}
		last = level
	}
	t.Logf("the NAS has %d alerts", acctest.NumOr0(out["total"]))

	asReader(t, tools.Options{}).Call(t, "alert_list", nil)
}
