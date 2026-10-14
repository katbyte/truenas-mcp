package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// checksOf is each finding of an audit's answer as check:name, in the order given.
func checksOf(t *testing.T, out map[string]any) []string {
	t.Helper()

	rows := acctest.Rows(t, out["findings"], "findings")
	checks := make([]string, 0, len(rows))
	for _, row := range rows {
		checks = append(checks, acctest.Str(row["check"])+":"+acctest.Str(row["name"]))
	}

	return checks
}

// A pool that lost a disk and found errors on the other is critical on every count, the gravest first; a whole pool scrubbed long ago is overdue, and
// the boot pool, never scrubbed, says so. A pool with no scrub task switched on is told of.
func TestAuditPoolsFindsADamagedPoolADiskGoneAndAScrubOverdue(t *testing.T) {
	t.Parallel()

	nas := standInWithPools(t)
	// tank's scrub ended in 2026-10, which a later clock calls long ago
	nas.Answer("boot.get_state", json.RawMessage(`{"name": "boot-pool", "status": "ONLINE", "healthy": true, "scan": null, "topology": {"data": [{"name": "sda3", "type": "DISK", "status": "ONLINE", "children": []}], "log": [], "cache": [], "spare": [], "special": [], "dedup": []}}`))
	nas.Answer("pool.scrub.query", json.RawMessage(`[{"id": 1, "pool": 1, "pool_name": "tank", "enabled": true}, {"id": 2, "pool": 2, "pool_name": "backup", "enabled": false}]`))

	out := session(t, nas, Options{}).Call(t, "audit_pools", map[string]any{"scrub_days": 1})
	got := strings.Join(checksOf(t, out), " ")
	for _, want := range []string{"pool_unhealthy:backup", "vdev_not_online:mirror-0", "disk_not_online:8271", "disk_errors:sdd", "never_scrubbed:backup", "no_scrub_task:backup", "never_scrubbed:boot-pool"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit_pools found %s, and not %s", got, want)
		}
	}
	if strings.Contains(got, "no_scrub_task:boot-pool") || !strings.Contains(got, "scrub_overdue:tank") {
		t.Errorf("audit_pools found %s: no task asked of the boot pool, and tank's scrub of long ago overdue", got)
	}
	last := 0
	for _, row := range acctest.Rows(t, out["findings"], "findings") {
		at := slices.Index(severities, acctest.Str(row["severity"]))
		if at < last {
			t.Errorf("%v comes after a graver finding's lesser one", row)
		}
		last = at
	}
	if acctest.Num(t, out["checked"], "checked") != 3 || acctest.Num(t, acctest.Object(t, out["by_check"], "by_check")["never_scrubbed"], "never_scrubbed") != 2 {
		t.Errorf("checked %v, counted %v", out["checked"], out["by_check"])
	}
	for _, row := range acctest.Rows(t, out["findings"], "findings") {
		if row["check"] == "disk_not_online" && (row["severity"] != critical || !strings.Contains(acctest.Str(row["detail"]), "sde, serial WD-1")) {
			t.Errorf("the disk that is gone reads as %v", row)
		}
	}
}

// A pool past 80% is filling and past 95% is full, and a dataset near its quota or its refquota is told of, each by what that quota counts.
func TestAuditSpaceFindsAPoolFillingAndADatasetAtItsQuota(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("pool.query", json.RawMessage(`[{"id": 1, "name": "tank", "healthy": true, "size": 1000, "allocated": 850, "free": 150}, {"id": 2, "name": "full", "healthy": true, "size": 1000, "allocated": 990, "free": 10}, {"id": 3, "name": "roomy", "healthy": true, "size": 1000, "allocated": 100, "free": 900}]`))
	nas.Answer("pool.dataset.query", []any{
		map[string]any{"name": "tank/near", "pool": "tank", "used": prop("920"), "usedbydataset": prop("100"), "quota": prop("1000"), "refquota": prop("0")},
		map[string]any{"name": "tank/own", "pool": "tank", "used": prop("990"), "usedbydataset": prop("990"), "quota": prop("0"), "refquota": prop("1000")},
		map[string]any{"name": "tank/free", "pool": "tank", "used": prop("10"), "usedbydataset": prop("10"), "quota": prop("0"), "refquota": prop("0")},
	})

	out := session(t, nas, Options{}).Call(t, "audit_space", nil)
	if got := strings.Join(checksOf(t, out), " "); got != "pool_full:full refquota_full:tank/own pool_filling:tank quota_filling:tank/near" {
		t.Errorf("audit_space found %s", got)
	}
	if acctest.Num(t, out["checked"], "checked") != 6 {
		t.Errorf("checked %v, want the three pools and the three datasets", out["checked"])
	}

	// told to warn only past 90, the pool at 85 is let be
	if got := strings.Join(checksOf(t, session(t, nas, Options{}).Call(t, "audit_space", map[string]any{"pool_percent": 90})), " "); strings.Contains(got, "pool_filling:tank") {
		t.Errorf("at 90%%, audit_space found %s", got)
	}
}
