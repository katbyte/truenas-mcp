package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
	nas.Handle("pool.dataset.query", datasetsAsANASAnswers(
		map[string]any{"name": "tank/near", "pool": "tank", "type": "FILESYSTEM", "used": prop("920"), "usedbydataset": prop("100"), "quota": prop("1000"), "refquota": prop("0")},
		map[string]any{"name": "tank/own", "pool": "tank", "type": "FILESYSTEM", "used": prop("990"), "usedbydataset": prop("990"), "quota": prop("0"), "refquota": prop("1000")},
		map[string]any{"name": "tank/free", "pool": "tank", "type": "VOLUME", "used": prop("10"), "usedbydataset": prop("10"), "quota": prop("0"), "refquota": prop("0")},
	))

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

// A task that failed is critical, one held or copying a locked dataset a warning, and one switched off a notice, whichever kind of task it is; a task
// whose last run worked is let be.
func TestAuditTasksFindsTasksThatFailedAreHeldOrCopyNothing(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("pool.snapshottask.query", json.RawMessage(`[{"id": 1, "dataset": "tank/a", "enabled": true, "state": {"state": "FINISHED", "datetime": {"$date": 1790845202000}}}, {"id": 2, "dataset": "tank/b", "enabled": true, "state": {"state": "ERROR", "datetime": {"$date": 1790845202000}, "error": "No space left."}}]`))
	nas.Answer("replication.query", json.RawMessage(`[{"id": 1, "name": "to the shelf", "enabled": true, "source_datasets": ["tank/a"], "state": {"state": "HOLD", "reason": "Dataset is locked."}, "job": null}]`))
	nas.Answer("rsynctask.query", json.RawMessage(`[{"id": 1, "desc": "a copy to nowhere", "path": "/mnt/tank/a", "enabled": false, "locked": false, "job": {"id": 9, "state": "FAILED", "error": "rsync: failed to connect", "time_finished": {"$date": 1790845202000}}}, {"id": 2, "desc": "behind a lock", "path": "/mnt/tank/locked", "enabled": true, "locked": true, "job": {"id": 10, "state": "SUCCESS"}}]`))
	nas.Answer("cloudsync.query", json.RawMessage(`[{"id": 1, "description": "to the bucket", "path": "/mnt/tank/a", "enabled": true, "locked": false, "job": {"id": 11, "state": "SUCCESS"}}]`))
	nas.Answer("cloud_backup.query", json.RawMessage(`[{"id": 1, "description": "offsite", "path": "/mnt/tank/b", "enabled": true, "locked": false, "job": {"id": 12, "state": "FAILED", "error": "403 Forbidden"}}]`))

	out := session(t, nas, Options{}).Call(t, "audit_tasks", nil)
	if got := strings.Join(checksOf(t, out), " "); got != "copy_task_failed:a copy to nowhere (1) cloud_backup_failed:offsite (1) snapshot_task_failed:tank/b (2) task_on_locked_dataset:behind a lock (2) replication_held:to the shelf (1) copy_task_off:a copy to nowhere (1)" {
		t.Errorf("audit_tasks found %s", got)
	}
	if acctest.Num(t, out["checked"], "checked") != 7 {
		t.Errorf("checked %v, want every task", out["checked"])
	}
	for _, row := range acctest.Rows(t, out["findings"], "findings") {
		if row["check"] == "snapshot_task_failed" && (!strings.Contains(acctest.Str(row["detail"]), "No space left.") || row["pool"] != "tank") {
			t.Errorf("the failed snapshot task reads as %v", row)
		}
	}
}

// A disk past the most its maker rates it for is critical, one warmer than its kind should run a warning, and one that says nothing is let be; a
// failed SMART self-test is told of by the disk it names, and a dismissed one is not.
func TestAuditDisksFindsADiskTooHotAndOneFailingItsSMARTChecks(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("disk.query", json.RawMessage(`[{"name": "sda", "serial": "A1", "type": "HDD", "pool": "tank"}, {"name": "sdb", "serial": "B2", "type": "HDD", "pool": "tank"}, {"name": "nvme0n1", "serial": "N1", "type": "SSD"}, {"name": "sdc", "serial": "C3", "type": "HDD"}, {"name": "sdd", "serial": "D4", "type": "HDD"}]`))
	nas.Answer("disk.temperatures", json.RawMessage(`{"sda": [62.0, 60.0], "sdb": [52.0, null], "nvme0n1": [55.0, 85.0], "sdc": null, "sdd": [38.0, 60.0]}`))
	nas.Answer("alert.list", json.RawMessage(`[
		{"uuid": "s", "level": "WARNING", "klass": "SMARTFailedSelfTest", "args": {"name": "sdc", "serial": "C3"}, "formatted": "sdc (C3) failed a SMART selftest.", "dismissed": false},
		{"uuid": "o", "level": "CRITICAL", "klass": "SMARTSpareBlockCount", "args": {"name": "sdd", "serial": "D4"}, "formatted": "sdd (D4) is reporting a low spare block reserve (2) .", "dismissed": true},
		{"uuid": "v", "level": "CRITICAL", "klass": "VolumeStatus", "formatted": "Pool tank state is DEGRADED", "dismissed": false}
	]`))

	out := session(t, nas, Options{}).Call(t, "audit_disks", nil)
	if got := strings.Join(checksOf(t, out), " "); got != "disk_too_hot:sda smart_failedselftest:sdc disk_hot:sdb" {
		t.Errorf("audit_disks found %s", got)
	}
	if acctest.Num(t, out["checked"], "checked") != 5 {
		t.Errorf("checked %v, want every disk", out["checked"])
	}

	// told the NVMe disk's kind should run under 50, it is warm
	if got := strings.Join(checksOf(t, session(t, nas, Options{}).Call(t, "audit_disks", map[string]any{"ssd_celsius": 50})), " "); !strings.Contains(got, "disk_hot:nvme0n1") {
		t.Errorf("at 50°C for a solid state disk, audit_disks found %s", got)
	}
}

// The NAS's own alerts at warning or graver are told of, its SMART ones and dismissed ones left to others; a certificate run out is critical and one
// running out soon a warning, read in the NAS's own time zone; a service set to start that is stopped is a warning, and an update waiting a notice.
func TestAuditSystemFindsAlertsCertificatesStoppedServicesAndUpdates(t *testing.T) {
	t.Parallel()

	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	soon := time.Now().In(zone).Add(10 * 24 * time.Hour).Format(certificateTime)
	later := time.Now().In(zone).Add(200 * 24 * time.Hour).Format(certificateTime)

	nas := truenastest.New(t)
	nas.Answer("system.state", "READY")
	nas.Answer("alert.list", json.RawMessage(`[
		{"uuid": "c", "level": "CRITICAL", "klass": "VolumeStatus", "formatted": "Pool backup state is DEGRADED", "dismissed": false},
		{"uuid": "w", "level": "WARNING", "klass": "NTPHealthCheck", "formatted": "NTP is not synchronised", "dismissed": false},
		{"uuid": "s", "level": "WARNING", "klass": "SMARTFailedSelfTest", "formatted": "sdc failed a SMART selftest.", "dismissed": false},
		{"uuid": "d", "level": "CRITICAL", "klass": "CertificateExpired", "formatted": "gone", "dismissed": true},
		{"uuid": "i", "level": "INFO", "klass": "Update", "formatted": "nothing", "dismissed": false}
	]`))
	nas.Answer("system.general.config", map[string]any{"timezone": "America/Los_Angeles"})
	nas.Answer("certificate.query", []any{
		map[string]any{"id": 1, "name": "old", "until": "Mon Jan  1 00:00:00 2024"},
		map[string]any{"id": 2, "name": "soon", "until": soon},
		map[string]any{"id": 3, "name": "fine", "until": later},
		map[string]any{"id": 4, "name": "unread", "until": "whenever"},
	})
	nas.Answer("service.query", json.RawMessage(`[{"service": "ssh", "enable": true, "state": "STOPPED"}, {"service": "nfs", "enable": false, "state": "STOPPED"}, {"service": "cifs", "enable": true, "state": "RUNNING"}]`))
	nas.Answer("update.status", json.RawMessage(`{"code": "NORMAL", "status": {"current_version": {"train": "TrueNAS-25.10"}, "new_version": {"version": "25.10.8"}}}`))

	out := session(t, nas, Options{}).Call(t, "audit_system", nil)
	if got := strings.Join(checksOf(t, out), " "); got != "alert:VolumeStatus certificate_expired:old alert:NTPHealthCheck certificate_expiring:soon service_stopped:ssh update_available:updates" {
		t.Errorf("audit_system found %s", got)
	}
}

// standInForEveryAudit is a NAS with something for each audit to find: a damaged pool, one filling up, a failed task, a hot disk and an alert.
func standInForEveryAudit(t *testing.T) *truenastest.NAS {
	t.Helper()

	// tank was scrubbed in 2100, so that however late this runs no scrub is overdue
	nas := standInWith(t, strings.Replace(pools, `"end_time": {"$date": 1790848802000}`, `"end_time": {"$date": 4102444800000}`, 1))
	nas.Answer("boot.get_state", json.RawMessage(`{"name": "boot-pool", "status": "ONLINE", "healthy": true, "scan": {"function": "SCRUB", "state": "FINISHED", "end_time": {"$date": 4102444800000}, "errors": 0}}`))
	nas.Answer("pool.scrub.query", json.RawMessage(`[{"pool_name": "tank", "enabled": true}, {"pool_name": "backup", "enabled": true}]`))
	nas.Answer("pool.dataset.query", []any{})
	nas.Answer("pool.snapshottask.query", json.RawMessage(`[]`))
	nas.Answer("replication.query", json.RawMessage(`[]`))
	nas.Answer("rsynctask.query", json.RawMessage(`[{"id": 1, "desc": "a copy to nowhere", "path": "/mnt/tank/a", "enabled": true, "job": {"state": "FAILED", "error": "no route"}}]`))
	nas.Answer("cloudsync.query", json.RawMessage(`[]`))
	nas.Answer("cloud_backup.query", json.RawMessage(`[]`))
	nas.Answer("disk.query", json.RawMessage(`[{"name": "sda", "serial": "A1", "type": "HDD", "pool": "tank"}]`))
	nas.Answer("disk.temperatures", json.RawMessage(`{"sda": [55.0, 60.0]}`))
	nas.Answer("alert.list", json.RawMessage(`[{"uuid": "w", "level": "WARNING", "klass": "NTPHealthCheck", "formatted": "NTP is not synchronised", "dismissed": false}]`))
	nas.Answer("system.state", "READY")
	nas.Answer("system.general.config", map[string]any{"timezone": "UTC"})
	nas.Answer("certificate.query", []any{})
	nas.Answer("service.query", []any{})
	nas.Answer("update.status", json.RawMessage(`{"code": "NORMAL", "status": {"new_version": null}}`))

	return nas
}

// audit_all runs every audit at its usual limits, names the audit of each finding, and counts each audit by severity. One that cannot run fails the
// call, saying which.
func TestAuditAllRunsEveryAuditAndCountsEach(t *testing.T) {
	t.Parallel()

	nas := standInForEveryAudit(t)
	out := session(t, nas, Options{}).Call(t, "audit_all", map[string]any{"limit": 1000})

	counts := map[string]string{}
	for _, row := range acctest.Rows(t, out["audits"], "audits") {
		counts[acctest.Str(row["audit"])] = fmt.Sprintf("%d/%d/%d", acctest.NumOr0(row["critical"]), acctest.NumOr0(row["warning"]), acctest.NumOr0(row["notice"]))
	}
	// backup is unhealthy, its mirror and a disk are not ONLINE, its other disk counts errors, and it was never scrubbed
	want := map[string]string{"audit_pools": "3/2/0", "audit_space": "0/1/0", "audit_tasks": "1/0/0", "audit_disks": "0/1/0", "audit_system": "0/1/0"}
	for audit, count := range want {
		if counts[audit] != count {
			t.Errorf("%s counted %s, want %s (all: %v)", audit, counts[audit], count, counts)
		}
	}
	if acctest.NumOr0(out["critical"]) != 4 || acctest.NumOr0(out["warning"]) != 5 || acctest.NumOr0(out["total"]) != 9 {
		t.Errorf("in all, %v critical, %v warnings, %v findings", out["critical"], out["warning"], out["total"])
	}
	for _, row := range acctest.Rows(t, out["findings"], "findings") {
		if acctest.Str(row["audit"]) == "" {
			t.Errorf("a finding names no audit: %v", row)
		}
	}

	nas.Refuse("disk.query", &truenastest.Error{Errno: 13, ErrName: "EACCES", Reason: "Not authorized"})
	if msg := session(t, nas, Options{}).CallErr(t, "audit_all", nil); !strings.Contains(msg, "audit_disks") || !strings.Contains(msg, "Not authorized") {
		t.Errorf("with one audit refused, audit_all says %q", msg)
	}
}

// audit_all runs every audit there is: an audit added to the set and left out of it would be missed by whoever starts there.
func TestAuditAllRunsEveryAuditThereIs(t *testing.T) {
	t.Parallel()

	every := audits(time.Now())
	ran := make([]string, 0, len(every))
	for _, audit := range every {
		ran = append(ran, audit.name)
	}
	for _, name := range Toolsets["audit"] {
		if name != "audit_all" && !slices.Contains(ran, name) {
			t.Errorf("audit_all does not run %s", name)
		}
	}
}
