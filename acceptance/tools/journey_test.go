//go:build acceptance

package tools_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
)

// One journey from nothing to a NAS the audits find nothing wrong with, through the tools alone: a dataset is made, given a snapshot task that the
// audit finds switched off and then does not, shared over SMB with its service set to start, which the audit finds stopped until it is started, and
// then everything made is taken away again, each removal said first and only then confirmed. The NAS is read back through the tools after every step.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestFromNothingToANASTheAuditsPassThroughTheToolsAlone(t *testing.T) {
	conn, _ := planter(t)
	dataset := layout.Pool + "/journey"
	path := "/mnt/" + dataset
	cleared(t, conn, dataset)

	// a dataset to protect and share, tried first
	if dry := suite.Call(t, "dataset_create", map[string]any{"dataset": dataset, "dry_run": true}); len(changes(t, dry)) != 1 {
		t.Fatalf("a dry run answered %v", dry)
	}
	suite.Call(t, "dataset_create", map[string]any{"dataset": dataset, "comments": "the journey"})
	if got := suite.Call(t, "dataset_get", map[string]any{"dataset": dataset}); got["comments"] != "the journey" {
		t.Fatalf("the dataset reads back as %v", got)
	}

	// a snapshot task made switched off is what the task audit finds, and switched on it is not
	task := acctest.Object(t, suite.Call(t, "snapshottask_create", map[string]any{"dataset": dataset, "schedule": "0 0 1 1 *", "keep_for": "1 DAY", "enabled": false})["snapshot_task"], "snapshot_task")
	id := acctest.NumOr0(task["id"])
	off := fmt.Sprintf("snapshot_task_off:%s (%d)", dataset, id)
	if findings := found(t, "audit_tasks"); !slices.Contains(findings, off) {
		t.Errorf("the task audit does not find the task switched off: %v", findings)
	}
	suite.Call(t, "snapshottask_update", map[string]any{"id": id, "enabled": true})
	if findings := found(t, "audit_tasks"); slices.Contains(findings, off) {
		t.Errorf("the task audit still finds the task switched off once it is on: %v", findings)
	}
	suite.Call(t, "snapshottask_run", map[string]any{"id": id})
	var snapshot string
	if !eventually(time.Minute, func() bool {
		rows := listed(t, "snapshot_list", "snapshots", map[string]any{"dataset": dataset})
		if len(rows) == 1 {
			snapshot = acctest.Str(rows[0]["name"])
		}

		return snapshot != ""
	}) {
		t.Fatal("the task run took no snapshot")
	}

	// shared over SMB, with its service set to start and stopped, which the system audit finds until the service is started
	suite.Call(t, "sharing_smb_create", map[string]any{"name": "journey", "path": path})
	smb := rowWith(listed(t, "service_list", "services", nil), "service", "cifs")
	if smb == nil {
		t.Fatal("the NAS lists no SMB service")
	}
	t.Cleanup(func() {
		if smb["state"] != "RUNNING" {
			suite.Call(t, "service_stop", map[string]any{"service": "cifs"})
		}
		suite.Call(t, "service_update", map[string]any{"service": "cifs", "enabled": acctest.BoolOf(smb["enabled"])})
	})
	suite.Call(t, "service_update", map[string]any{"service": "cifs", "enabled": true})
	suite.Call(t, "service_stop", map[string]any{"service": "cifs"})
	if findings := found(t, "audit_system"); !slices.Contains(findings, "service_stopped:cifs") {
		t.Errorf("the system audit does not find SMB stopped while it is set to start: %v", findings)
	}
	suite.Call(t, "service_start", map[string]any{"service": "cifs"})
	if findings := found(t, "audit_system"); slices.Contains(findings, "service_stopped:cifs") {
		t.Errorf("the system audit still finds SMB stopped once it is started: %v", findings)
	}
	if share := rowWith(listed(t, "sharing_smb_list", "shares", nil), "name", "journey"); share == nil || share["path"] != path {
		t.Errorf("the share is listed as %v", share)
	}

	// and all of it taken away, each removal said before it is confirmed
	for _, removal := range []struct {
		tool string
		args map[string]any
	}{
		{"sharing_smb_delete", map[string]any{"share": "journey"}},
		{"snapshot_delete", map[string]any{"snapshot": snapshot}},
		{"snapshottask_delete", map[string]any{"id": id}},
		{"dataset_delete", map[string]any{"dataset": dataset, "recursive": true}},
	} {
		said := suite.Call(t, removal.tool, removal.args)
		if acctest.BoolOf(said["removed"]) || len(acctest.Strs(t, said["would_go"], "would_go")) == 0 {
			t.Errorf("%s without confirm: %v", removal.tool, said)
		}
		removal.args["confirm"] = true
		if done := suite.Call(t, removal.tool, removal.args); !acctest.BoolOf(done["removed"]) {
			t.Errorf("%s confirmed: %v", removal.tool, done)
		}
	}
	if msg := suite.CallErr(t, "dataset_get", map[string]any{"dataset": dataset}); !strings.Contains(msg, "no dataset") {
		t.Errorf("the dataset is still there at the end: %q", msg)
	}
}
