//go:build acceptance

package tools_test

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenas/client"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// The audits are proved by giving the NAS the problem each is to find, and taking it away again. The problems are made through the SDK until there
// are tools to make them with; this file is named to run last, since a disk is taken out of the pool.

// planter is the administrator's connection and the typed client over it, for making a problem.
func planter(t *testing.T) (*truenasrpc.Conn, *client.Client) {
	t.Helper()

	if !suite.Ready {
		t.Skip(suite.NotReady)
	}
	conn, err := connection(truenasrpc.APIKey(nas.username, nas.made.Key))
	if err != nil {
		t.Fatal(err)
	}
	typed, err := client.New(conn)
	if err != nil {
		t.Fatal(err)
	}

	return conn, typed
}

// found is each finding of an audit, asked with nothing, as check:name.
func found(t *testing.T, tool string) []string {
	t.Helper()

	rows := acctest.Rows(t, suite.Call(t, tool, nil)["findings"], "findings")
	checks := make([]string, 0, len(rows))
	for _, row := range rows {
		checks = append(checks, acctest.Str(row["check"])+":"+acctest.Str(row["name"]))
	}

	return checks
}

// eventually asks until the check holds or the time is up.
func eventually(within time.Duration, check func() bool) bool {
	for deadline := time.Now().Add(within); ; time.Sleep(3 * time.Second) {
		if check() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// A whole pool that was never scrubbed is told of and nothing worse; with a disk of its mirror taken out, it is unhealthy, its mirror is not
// ONLINE and the disk is gone, each critical. With the disk back, the pool is whole again.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestAuditPoolsFindsADiskTakenOutOfThePool(t *testing.T) {
	conn, typed := planter(t)
	ctx := t.Context()

	before := found(t, "audit_pools")
	for _, check := range before {
		if strings.HasPrefix(check, "pool_unhealthy") || strings.HasPrefix(check, "disk_") || strings.HasPrefix(check, "vdev_") {
			t.Errorf("a whole pool is found to have %s", check)
		}
	}
	t.Logf("a whole pool, as the NAS is set up, is found to have: %v", before)

	pool, err := typed.Pool.QueryOne(ctx, []any{[]any{"name", "=", layout.Pool}}, truenas.PoolQueryOptions{})
	if err != nil || pool.Answer == nil || pool.Answer.Topology == nil || len(pool.Answer.Topology.Data) == 0 || len(pool.Answer.Topology.Data[0].Children) < 2 {
		t.Fatalf("the pool reads as %+v (%v)", pool.Answer, err)
	}
	gone := pool.Answer.Topology.Data[0].Children[0]

	if err := conn.Call(ctx, "pool.offline", []any{pool.Answer.Id, map[string]any{"label": gone.Guid}}, nil); err != nil {
		t.Fatalf("taking %s out of the pool: %v", gone.Disk, err)
	}
	t.Cleanup(func() {
		after := context.WithoutCancel(ctx)
		if err := conn.Call(after, "pool.online", []any{pool.Answer.Id, map[string]any{"label": gone.Guid}}, nil); err != nil {
			t.Errorf("putting %s back in the pool: %v", gone.Disk, err)
		}
		if !eventually(3*time.Minute, func() bool {
			back, err := typed.Pool.QueryOne(after, []any{[]any{"name", "=", layout.Pool}}, truenas.PoolQueryOptions{})
			return err == nil && back.Answer != nil && back.Answer.Healthy
		}) {
			t.Error("with the disk back, the pool is not healthy again")
		}
	})

	var damaged []string
	if !eventually(time.Minute, func() bool {
		damaged = found(t, "audit_pools")
		return slices.Contains(damaged, "pool_unhealthy:"+layout.Pool)
	}) {
		t.Fatalf("with a disk taken out, audit_pools finds %v", damaged)
	}
	if !slices.Contains(damaged, "vdev_not_online:mirror-0") || !slices.ContainsFunc(damaged, func(c string) bool { return strings.HasPrefix(c, "disk_not_online:") }) {
		t.Errorf("with %s taken out, audit_pools finds %v", gone.Disk, damaged)
	}
	for _, row := range acctest.Rows(t, suite.Call(t, "audit_pools", nil)["findings"], "findings") {
		if row["check"] == "disk_not_online" && (row["severity"] != "critical" || !strings.Contains(acctest.Str(row["detail"]), "OFFLINE")) {
			t.Errorf("the disk taken out reads as %v", row)
		}
	}
}

// A dataset with a refquota is let be while it has room, and found filling once it uses more of it than the audit is told to allow. A NAS takes no
// refquota under 1 GiB, so a tenth of one is written and the audit told to warn past a twentieth; that a full one is critical is held by the
// stand-in.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestAuditSpaceFindsADatasetFillingItsQuota(t *testing.T) {
	conn, _ := planter(t)
	ctx := t.Context()

	const limit = 1 << 30
	name := layout.Pool + "/audit-full"
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name, "refquota": limit, "compression": "OFF"}}, nil); err != nil {
		t.Fatalf("making a dataset with a refquota: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "pool.dataset.delete", []any{name, map[string]any{"recursive": true, "force": true}}, nil); err != nil {
			t.Errorf("removing the dataset: %v", err)
		}
	})

	if checks := found(t, "audit_space"); slices.ContainsFunc(checks, func(c string) bool { return strings.HasSuffix(c, ":"+name) }) {
		t.Errorf("empty, the dataset is found to have %v", checks)
	}

	// nothing that could be squeezed, so that what is sent is what is used
	job, err := conn.StartUpload(ctx, "filesystem.put", []any{"/mnt/" + name + "/a-tenth", map[string]any{}}, io.LimitReader(rand.NewChaCha8([32]byte{2}), limit/10))
	if err != nil {
		t.Fatalf("writing to the dataset: %v", err)
	}
	if err := job.Wait(ctx, nil); err != nil {
		t.Fatalf("writing to the dataset: %v", err)
	}

	var rows []map[string]any
	if !eventually(time.Minute, func() bool {
		rows = acctest.Rows(t, suite.Call(t, "audit_space", map[string]any{"quota_percent": 5})["findings"], "findings")
		return slices.ContainsFunc(rows, func(row map[string]any) bool { return row["check"] == "refquota_filling" && row["name"] == name })
	}) {
		t.Errorf("a tenth full, told to warn past a twentieth, audit_space finds %v", rows)
	}
}

// A copy task whose run failed is found, by its description and number.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestAuditTasksFindsACopyTaskThatFailed(t *testing.T) {
	conn, _ := planter(t)
	ctx := t.Context()

	var task struct {
		ID int `json:"id"`
	}
	// switched on, so that it is held to its runs, and scheduled once a year, so that the only run is the one made here
	if err := conn.Call(ctx, "rsynctask.create", []any{map[string]any{"path": "/mnt/" + layout.Datasets[0], "user": "root", "mode": "MODULE", "remotehost": "127.0.0.1", "remotemodule": "nothing-listens-here", "direction": "PUSH", "desc": "an audited copy", "enabled": true, "schedule": map[string]any{"minute": "0", "hour": "0", "dom": "1", "month": "1", "dow": "*"}}}, &task); err != nil {
		t.Fatalf("making the task: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "rsynctask.delete", []any{task.ID}, nil); err != nil {
			t.Errorf("removing the task: %v", err)
		}
	})

	job, err := conn.Start(ctx, "rsynctask.run", []any{task.ID})
	if err != nil {
		t.Fatalf("running the task: %v", err)
	}
	if err := job.Wait(ctx, nil); err == nil {
		t.Fatal("a copy to a place that is not there ran without failing")
	}

	want := fmt.Sprintf("copy_task_failed:an audited copy (%d)", task.ID)
	var checks []string
	if !eventually(time.Minute, func() bool {
		checks = found(t, "audit_tasks")
		return slices.Contains(checks, want)
	}) {
		t.Errorf("with a copy task whose run failed, audit_tasks finds %v", checks)
	}
}

// The test machine's NVMe disk says how warm it is, which the others do not: at the warning for its kind it is let be, and told that its kind
// should run cooler than it is, it is found warm.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestAuditDisksFindsTheDiskThatIsWarm(t *testing.T) {
	disks := acctest.Rows(t, suite.Call(t, "disk_list", nil)["disks"], "disks")
	hot := slices.IndexFunc(disks, func(d map[string]any) bool { return d["serial"] == layout.HotDiskSerial })
	if hot < 0 || disks[hot]["temperature_celsius"] == nil {
		t.Fatalf("the NVMe disk says no temperature: %v", disks)
	}
	name, celsius := acctest.Str(disks[hot]["name"]), acctest.DecimalOr0(disks[hot]["temperature_celsius"])

	if checks := found(t, "audit_disks"); slices.ContainsFunc(checks, func(c string) bool { return strings.HasSuffix(c, ":"+name) }) && celsius < 70 {
		t.Errorf("at %.0f°C, under the warning for its kind, the NVMe disk is found to have %v", celsius, checks)
	}

	rows := acctest.Rows(t, suite.Call(t, "audit_disks", map[string]any{"ssd_celsius": celsius - 5})["findings"], "findings")
	if !slices.ContainsFunc(rows, func(row map[string]any) bool {
		return row["name"] == name && (row["check"] == "disk_hot" || row["check"] == "disk_too_hot")
	}) {
		t.Errorf("told its kind should run under %.0f°C, the NVMe disk at %.0f°C is not found warm: %v", celsius-5, celsius, rows)
	}
}

// The NAS's own certificate is read in its time zone: told to warn of any certificate running out within a hundred years, audit_system finds it.
// audit_all runs every audit on the NAS and counts each; a snapshot task switched off is made first, as the NAS otherwise has no task to look at.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestAuditSystemReadsTheNASsCertificateAndAuditAllRunsEveryAudit(t *testing.T) {
	conn, _ := planter(t)
	ctx := t.Context()

	var task struct {
		ID int `json:"id"`
	}
	if err := conn.Call(ctx, "pool.snapshottask.create", []any{map[string]any{"dataset": layout.Datasets[0], "naming_schema": "audited-%Y-%m-%d_%H-%M", "lifetime_value": 1, "lifetime_unit": "HOUR", "enabled": false}}, &task); err != nil {
		t.Fatalf("making a snapshot task: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "pool.snapshottask.delete", []any{task.ID}, nil); err != nil {
			t.Errorf("removing the snapshot task: %v", err)
		}
	})

	rows := acctest.Rows(t, suite.Call(t, "audit_system", map[string]any{"certificate_days": 36500})["findings"], "findings")
	if !slices.ContainsFunc(rows, func(row map[string]any) bool { return row["check"] == "certificate_expiring" }) {
		t.Errorf("told to warn of any certificate running out within a hundred years, audit_system finds %v", rows)
	}
	for _, row := range rows {
		if row["check"] == "certificate_expired" {
			t.Errorf("a certificate of a NAS installed today is found run out: %v", row)
		}
	}

	all := suite.Call(t, "audit_all", map[string]any{"limit": 1000})
	counted := acctest.Rows(t, all["audits"], "audits")
	ran := make([]string, 0, len(counted))
	for _, row := range counted {
		ran = append(ran, acctest.Str(row["audit"]))
		if acctest.NumOr0(row["checked"]) == 0 {
			t.Errorf("%s checked nothing on the NAS", row["audit"])
		}
	}
	for _, want := range []string{"audit_pools", "audit_space", "audit_tasks", "audit_disks", "audit_system"} {
		if !slices.Contains(ran, want) {
			t.Errorf("audit_all ran %v, and not %s", ran, want)
		}
	}
	off := fmt.Sprintf("snapshot_task_off:%s (%d)", layout.Datasets[0], task.ID)
	if !slices.ContainsFunc(acctest.Rows(t, all["findings"], "findings"), func(row map[string]any) bool {
		return row["audit"] == "audit_tasks" && acctest.Str(row["check"])+":"+acctest.Str(row["name"]) == off
	}) {
		t.Errorf("audit_all does not find %s among %v", off, all["findings"])
	}
	t.Logf("audit_all finds %v critical, %v warnings and %v notices on the NAS", all["critical"], all["warning"], all["notice"])
}
