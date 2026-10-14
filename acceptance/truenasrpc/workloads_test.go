//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// app is an app as a NAS lists it, as far as a check of apps reads one.
type app struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Custom    bool   `json:"custom_app"`
	Workloads struct {
		Containers int      `json:"containers"`
		Images     []string `json:"images"`
		Details    []struct {
			Service string `json:"service_name"`
			Image   string `json:"image"`
			State   string `json:"state"`
		} `json:"container_details"`
	} `json:"active_workloads"`
}

// appNamed is one app by its name, asked for by whoever is given.
func appNamed(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name string) app {
	t.Helper()

	var found []app
	asked(ctx, t, conn, "app.query", []any{[]any{[]any{"name", "=", name}}}, &found)
	if len(found) != 1 {
		t.Fatalf("%d apps are named %s", len(found), name)
	}

	return found[0]
}

// A NAS runs apps once it is told which pool to keep them on. It is told, and two apps of the plainest kind are made: one that does nothing
// and goes on doing it, which is run, stopped, started and made afresh, and one that fails the moment it starts, which is what a check of
// apps has to find. Each is listed with the state it is in, to the user who may only read. Both are removed, and the NAS is told to keep
// apps nowhere, as it was. The apps come from a registry that asks for no account, so the NAS has to reach the internet.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAnAppIsRunStoppedAndRemovedAndOneThatFailsSaysSo(t *testing.T) {
	conn, only := admin(t), reader(t)
	ctx := within(t, 25*time.Minute)

	status := func() string {
		var is struct {
			Status      string `json:"status"`
			Description string `json:"description"`
		}
		asked(ctx, t, only, "docker.status", nil, &is)

		return is.Status
	}
	if is := status(); is != "UNCONFIGURED" {
		t.Fatalf("before it is told where to keep apps, the NAS says of running them: %s", is)
	}

	var kept struct {
		Pool    *string `json:"pool"`
		Dataset *string `json:"dataset"`
	}
	if err := conn.Call(ctx, "docker.update", []any{map[string]any{"pool": layout.Pool}}, &kept); err != nil {
		t.Fatalf("telling the NAS to keep apps on the pool: %v", err)
	}
	t.Cleanup(func() {
		after := context.WithoutCancel(ctx)

		if err := conn.Call(after, "docker.update", []any{map[string]any{"pool": nil}}, nil); err != nil {
			t.Errorf("telling the NAS to keep apps nowhere: %v", err)
		}
		if err := conn.Call(after, "pool.dataset.delete", []any{layout.Pool + "/ix-apps", map[string]any{"recursive": true, "force": true}}, nil); err != nil && !truenasrpc.IsNotFound(err) {
			t.Errorf("removing where the apps were kept: %v", err)
		}
	})
	if kept.Pool == nil || *kept.Pool != layout.Pool || kept.Dataset == nil {
		t.Errorf("told to keep apps on the pool, the NAS says it keeps them on %v, in %v", kept.Pool, kept.Dataset)
	}
	if !eventually(ctx, 5*time.Minute, func() bool { return status() == "RUNNING" }) {
		t.Fatalf("told where to keep apps, the NAS says of running them: %s", status())
	}

	made := func(name string, service map[string]any) {
		if err := conn.Call(ctx, "app.create", []any{map[string]any{"custom_app": true, "app_name": name, "custom_compose_config": map[string]any{"services": map[string]any{name: service}}}}, nil); err != nil {
			t.Fatalf("making the app %s: %v", name, err)
		}
		t.Cleanup(func() {
			if err := conn.Call(context.WithoutCancel(ctx), "app.delete", []any{name, map[string]any{"remove_images": true, "force_remove_custom_app": true}}, nil); err != nil && !truenasrpc.IsNotFound(err) {
				t.Errorf("removing the app %s: %v", name, err)
			}
		})
	}
	in := func(name, state string) app {
		var is app
		if !eventually(ctx, 3*time.Minute, func() bool { is = appNamed(ctx, t, only, name); return is.State == state }) {
			t.Errorf("the app %s reads as %+v, want it %s", name, is, state)
		}

		return is
	}

	made("idles", map[string]any{"image": "registry.k8s.io/pause:3.10", "restart": "unless-stopped"})
	if idle := in("idles", "RUNNING"); !idle.Custom || idle.Workloads.Containers != 1 {
		t.Errorf("running, the app that does nothing reads as %+v", idle)
	}

	if err := conn.Call(ctx, "app.stop", []any{"idles"}, nil); err != nil {
		t.Errorf("stopping the app: %v", err)
	}
	in("idles", "STOPPED")
	if err := conn.Call(ctx, "app.start", []any{"idles"}, nil); err != nil {
		t.Errorf("starting the app: %v", err)
	}
	in("idles", "RUNNING")
	if err := conn.Call(ctx, "app.redeploy", []any{"idles"}, nil); err != nil {
		t.Errorf("making the app afresh: %v", err)
	}
	in("idles", "RUNNING")

	var settings json.RawMessage
	asked(ctx, t, conn, "app.config", []any{"idles"}, &settings)

	// an app that fails as soon as it starts
	made("fails", map[string]any{"image": "registry.k8s.io/e2e-test-images/busybox:1.36.1-1", "command": []string{"sh", "-c", "exit 3"}, "restart": "no"})
	t.Logf("the app that fails reads as: %+v", in("fails", "CRASHED"))

	// both are in what the NAS lists, and the one that works is not held to be at fault for the one beside it
	if idle := appNamed(ctx, t, only, "idles"); idle.State != "RUNNING" {
		t.Errorf("with an app that fails beside it, the app that does nothing reads as %s", idle.State)
	}

	for _, name := range []string{"fails", "idles"} {
		if err := conn.Call(ctx, "app.delete", []any{name, map[string]any{"remove_images": true}}, nil); err != nil {
			t.Errorf("removing the app %s: %v", name, err)
		}
	}
	var left []app
	asked(ctx, t, only, "app.query", nil, &left)
	if len(left) != 0 {
		t.Errorf("with both apps removed the NAS lists %+v", left)
	}
}

// A virtual machine is described, given a disk, changed and copied, under a name of letters and digits only, the only kind a NAS takes for one. Whether the NAS can run one is asked, and the machine is started: on a
// NAS that can, it runs and is switched off, and on one that cannot, which a NAS inside a virtual machine may be, starting it is refused.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAMachineIsGivenADiskChangedCopiedAndStartedWhereItCanBe(t *testing.T) {
	conn, only := admin(t), reader(t)
	ctx := within(t, 10*time.Minute)

	disk := layout.Pool + "/tried-disk"
	gone(ctx, t, conn, disk)

	machineID := madeOne(ctx, t, conn, "vm", map[string]any{"name": "tried", "memory": 256, "autostart": false, "description": "made by the tests"}, map[string]any{"zvols": false, "force": true})
	device := madeOne(ctx, t, conn, "vm.device", map[string]any{"vm": machineID, "attributes": map[string]any{"dtype": "DISK", "type": "VIRTIO", "create_zvol": true, "zvol_name": disk, "zvol_volsize": 64 << 20}}, map[string]any{"force": true})

	if made := one(ctx, t, conn, disk); made.Type != "VOLUME" {
		t.Errorf("the disk made for the machine reads as a %s", made.Type)
	}

	var row json.RawMessage
	if err := conn.Call(ctx, "vm.update", []any{machineID, map[string]any{"description": "changed by the tests", "vcpus": 2}}, &row); err != nil || field(t, row, "description") != `"changed by the tests"` || field(t, row, "vcpus") != "2" {
		t.Errorf("changed, the machine reads as %.300s (%v)", row, err)
	}
	if err := conn.Call(ctx, "vm.device.update", []any{device, map[string]any{"order": 1005}}, &row); err != nil || field(t, row, "order") != "1005" {
		t.Errorf("moved in the order the machine starts from, the disk reads as %.300s (%v)", row, err)
	}

	var copied bool
	if err := conn.Call(ctx, "vm.clone", []any{machineID, "triedcopy"}, &copied); err != nil || !copied {
		t.Fatalf("copying the machine: %t, %v", copied, err)
	}
	var copies []struct {
		ID int64 `json:"id"`
	}
	if err := conn.Call(ctx, "vm.query", []any{[]any{[]any{"name", "=", "triedcopy"}}}, &copies); err != nil || len(copies) != 1 {
		t.Fatalf("the copy of the machine is not listed: %d found, %v", len(copies), err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "vm.delete", []any{copies[0].ID, map[string]any{"zvols": true, "force": true}}, nil); err != nil {
			t.Errorf("removing the copy of the machine: %v", err)
		}
	})

	state := func() string {
		var is struct {
			State string `json:"state"`
		}
		asked(ctx, t, only, "vm.status", []any{machineID}, &is)

		return is.State
	}
	if is := state(); is != "STOPPED" {
		t.Errorf("never started, the machine reads as %s", is)
	}

	var able bool
	asked(ctx, t, only, "vm.supports_virtualization", nil, &able)

	err := conn.Call(ctx, "vm.start", []any{machineID}, nil)
	// a machine copied a moment before is now and then not yet known to the NAS's hypervisor, which says its domain is not found; a little later it is
	for tries := 0; err != nil && strings.Contains(err.Error(), "Domain not found") && tries < 6; tries++ {
		t.Logf("starting the machine is refused with %v, and is asked again", err)
		time.Sleep(5 * time.Second)
		err = conn.Call(ctx, "vm.start", []any{machineID}, nil)
	}
	switch {
	case able && err != nil:
		t.Errorf("a NAS that says it can run machines would not start one: %v", err)
	case !able && err == nil:
		t.Error("a NAS that says it cannot run machines started one")
	case !able:
		t.Logf("on a NAS that cannot run machines, starting one is refused with: %v", err)

		return
	}

	if !eventually(ctx, time.Minute, func() bool { return state() == "RUNNING" }) {
		t.Errorf("started, the machine reads as %s", state())
	}
	var used int64
	asked(ctx, t, only, "vm.get_memory_usage", []any{machineID}, &used)
	t.Logf("running, the machine is said to use %d bytes of memory", used)

	if err := conn.Call(ctx, "vm.poweroff", []any{machineID}, nil); err != nil {
		t.Errorf("switching the machine off: %v", err)
	}
	if !eventually(ctx, time.Minute, func() bool { return state() == "STOPPED" }) {
		t.Errorf("switched off, the machine reads as %s", state())
	}
}
