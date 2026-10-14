//go:build acceptance

package truenasrpc_test

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// machine tells the virtual machine the NAS runs in to do something, the way a person at its console would, and answers what it said back.
// It is how a test does to the NAS what no API can: break its hardware.
func machine(ctx context.Context, t *testing.T, socket, command string) string {
	t.Helper()

	var dialer net.Dialer
	console, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		t.Fatalf("reaching the machine's console: %v", err)
	}
	defer func() { _ = console.Close() }()
	_ = console.SetDeadline(time.Now().Add(30 * time.Second))

	// the console says it is ready for more with a prompt, once when it is reached and once after each thing it is told
	said := bufio.NewReader(console)
	prompted := func() string {
		var text strings.Builder
		for !strings.HasSuffix(text.String(), "(qemu) ") {
			b, err := said.ReadByte()
			if err != nil {
				t.Fatalf("the machine's console went quiet after %q: %v", text.String(), err)
			}
			text.WriteByte(b)
		}

		return text.String()
	}

	prompted()
	if _, err := console.Write([]byte(command + "\n")); err != nil {
		t.Fatalf("telling the machine %q: %v", command, err)
	}

	return prompted()
}

// console is where the machine the NAS runs in is told what to do. A test that needs it is skipped where there is none, which is anywhere the
// NAS was not started by this repository's own script.
func console(t *testing.T) string {
	t.Helper()

	socket := os.Getenv("TRUENAS_TEST_MONITOR")
	if socket == "" {
		home, _ := os.UserHomeDir()
		socket = filepath.Join(home, ".cache", "truenas-mcp", "testenv", "run", "monitor.sock")
	}
	if _, err := os.Stat(socket); err != nil { //nolint:gosec // G703: the path is where the person running the tests says their own machine's console is
		t.Skipf("the machine the NAS runs in has no console at %s to break a disk from: set TRUENAS_TEST_MONITOR, which \"make testacc\" does", socket)
	}

	return socket
}

// drive is the machine's name for a disk the NAS knows by another: the machine numbers its disks, and gave each a serial that ends in its number.
func drive(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, socket, disk string) string {
	t.Helper()

	var known []spare
	if err := conn.Call(ctx, "disk.query", []any{[]any{[]any{"name", "=", disk}}}, &known); err != nil || len(known) != 1 {
		t.Fatalf("looking up the disk %s: %d found, %v", disk, len(known), err)
	}

	name := "data" + strings.TrimPrefix(known[0].Serial, layout.DiskSerial)
	if has := machine(ctx, t, socket, "info block"); !strings.Contains(has, name+" ") {
		t.Fatalf("the machine has no drive %s for the disk with serial %s: %s", name, known[0].Serial, has)
	}

	return name
}

// The last test but one of the run, by its file's name, because what it does cannot be undone: a disk of the pool is taken away from under
// the NAS, as a disk that has died is, so every read and write of it fails. The NAS has to notice by itself and say the pool is no longer
// safe, in the pool's state and in an alert. A spare disk is then put in its place, and the pool has to be whole again on the spare.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestADiskThatDiesIsNoticedAndASpareTakesItsPlace(t *testing.T) {
	socket := console(t)
	conn := admin(t)
	ctx := within(t, 25*time.Minute)

	before := pool(ctx, t, conn)
	if !before.Healthy || len(before.Topology.Data) != 1 || len(before.Topology.Data[0].Children) != 2 {
		t.Fatalf("the pool is not a healthy pair of disks to start with: %+v", before)
	}
	dying, staying := before.Topology.Data[0].Children[0], before.Topology.Data[0].Children[1]

	free := spares(ctx, t, conn)
	if len(free) == 0 {
		t.Fatal("the NAS has no disk nothing uses to put in a dead one's place")
	}

	dead := drive(ctx, t, conn, socket, dying.Disk)
	t.Logf("taking %s (%s to the machine) away from under the NAS", dying.Disk, dead)
	machine(ctx, t, socket, "drive_del "+dead)

	// a NAS finds a dead disk when it next uses it, so it is given something to write, and a scrub if that is not enough
	put(ctx, t, conn, "/mnt/"+layout.Datasets[0]+"/after-the-disk-died.txt", "written with one disk of the pair dead\n")

	var without pooled
	noticed := func() bool { without = pool(ctx, t, conn); return !without.Healthy }
	if !eventually(ctx, 90*time.Second, noticed) {
		if _, err := conn.Start(ctx, "pool.scrub.scrub", []any{layout.Pool, "START"}); err != nil {
			t.Logf("starting a scrub: %v", err)
		}
		if !eventually(ctx, 5*time.Minute, noticed) {
			t.Fatalf("the NAS never noticed a disk of the pool had died: the pool reads as %s", without.Status)
		}
	}
	// the pool's own word for its state is not to be gone by: seen on a real NAS, a pool with a dead disk still calls itself ONLINE. That it
	// is not healthy, and which disk is at fault, is what says so
	t.Logf("the pool reads as %s, the dead disk as %s and the other as %s", without.Status, without.Topology.Data[0].Children[0].Status, without.Topology.Data[0].Children[1].Status)
	if without.Topology.Data[0].Children[0].Status != "FAULTED" || without.Topology.Data[0].Children[1].Status != "ONLINE" {
		t.Errorf("with a disk dead the pool's disks read as %s and %s; want the dead one FAULTED and the other ONLINE", without.Topology.Data[0].Children[0].Status, without.Topology.Data[0].Children[1].Status)
	}

	// what it wrote with a disk dead is all there
	if back := got(ctx, t, conn, "/mnt/"+layout.Datasets[0]+"/after-the-disk-died.txt"); back != "written with one disk of the pair dead\n" {
		t.Errorf("written with a disk dead, the file reads back as %q", back)
	}

	var said []alert
	if !eventually(ctx, 4*time.Minute, func() bool {
		said = unwell(ctx, t, conn)
		return len(said) > 0
	}) {
		t.Error("the NAS raised no alert for a pool with a dead disk")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)

		if one.Level != "CRITICAL" || !strings.Contains(one.Formatted, "FAULTED") {
			t.Errorf("the alert is %s and does not say a disk is faulted: %s", one.Level, one.Formatted)
		}
	}

	// the list of disks says nothing of it: a dead disk is listed as the pool's, with its size, like the one beside it. Only the pool knows
	var listed struct {
		Used []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			Pool string `json:"imported_zpool"`
		} `json:"used"`
	}
	if err := conn.Call(ctx, "disk.details", nil, &listed); err != nil {
		t.Fatalf("listing the disks with one dead: %v", err)
	}
	for _, disk := range listed.Used {
		if disk.Name == dying.Disk && (disk.Pool != layout.Pool || disk.Size == 0) {
			t.Errorf("the dead disk is listed as %+v, and was seen listed as the pool's with its size", disk)
		}
	}

	// a spare is named by its identifier here, where making a pool names a disk by its name
	if err := conn.Call(ctx, "pool.replace", []any{before.ID, map[string]any{"label": dying.GUID, "disk": free[0].Identifier, "force": true}}, nil); err != nil {
		t.Fatalf("putting %s in the dead disk's place: %v", free[0].Name, err)
	}

	var whole pooled
	if !eventually(ctx, 8*time.Minute, func() bool {
		whole = pool(ctx, t, conn)

		return whole.Healthy && whole.Status == "ONLINE" && len(whole.Topology.Data[0].Children) == 2
	}) {
		t.Fatalf("with a spare in the dead disk's place the pool reads as %+v", whole)
	}

	if of := []string{whole.Topology.Data[0].Children[0].Disk, whole.Topology.Data[0].Children[1].Disk}; !slices.Contains(of, free[0].Name) || !slices.Contains(of, staying.Disk) {
		t.Errorf("the pool is whole on %v; want the disk that stayed, %s, and the spare, %s", of, staying.Disk, free[0].Name)
	}

	if !eventually(ctx, 4*time.Minute, func() bool { return len(unwell(ctx, t, conn)) == 0 }) {
		t.Error("the NAS still says the pool is not healthy, with a spare in the dead disk's place")
	}
}
