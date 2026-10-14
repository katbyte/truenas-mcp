//go:build acceptance

package truenasrpc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
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

// The last test but one of the run, by its file's name, because what it does cannot be undone: a disk of the pool is taken away from under
// the NAS, as a disk that has died is, so every read and write of it fails. The NAS has to notice by itself and say the pool is no longer
// safe, in the pool's state and in an alert. A spare disk is then put in its place, and the pool has to be whole again on the spare.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestADiskThatDiesIsNoticedAndASpareTakesItsPlace(t *testing.T) {
	socket := os.Getenv("TRUENAS_TEST_MONITOR")
	if socket == "" {
		home, _ := os.UserHomeDir()
		socket = filepath.Join(home, ".cache", "truenas-mcp", "testenv", "run", "monitor.sock")
	}
	if _, err := os.Stat(socket); err != nil { //nolint:gosec // G703: the path is where the person running the tests says their own machine's console is
		t.Skipf("the machine the NAS runs in has no console at %s to break a disk from: set TRUENAS_TEST_MONITOR, which \"make testacc\" does", socket)
	}

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

	// the machine knows its disks by number, and gave each a serial that ends in it
	var known []spare
	if err := conn.Call(ctx, "disk.query", []any{[]any{[]any{"name", "=", dying.Disk}}}, &known); err != nil || len(known) != 1 {
		t.Fatalf("looking up the disk %s: %d found, %v", dying.Disk, len(known), err)
	}
	drive := "data" + strings.TrimPrefix(known[0].Serial, layout.DiskSerial)
	if has := machine(ctx, t, socket, "info block"); !strings.Contains(has, drive+" ") {
		t.Fatalf("the machine has no drive %s for the disk with serial %s: %s", drive, known[0].Serial, has)
	}

	t.Logf("taking %s (%s, serial %s) away from under the NAS: %q", dying.Disk, drive, known[0].Serial, machine(ctx, t, socket, "drive_del "+drive))

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
	t.Logf("the pool reads as %s, the dead disk as %s and the other as %s", without.Status, without.Topology.Data[0].Children[0].Status, without.Topology.Data[0].Children[1].Status)
	if without.Status != "DEGRADED" || without.Topology.Data[0].Children[0].Status == "ONLINE" || without.Topology.Data[0].Children[1].Status != "ONLINE" {
		t.Errorf("with a disk dead the pool reads as %s, its disks %s and %s; want DEGRADED, the dead one anything but ONLINE and the other ONLINE", without.Status, without.Topology.Data[0].Children[0].Status, without.Topology.Data[0].Children[1].Status)
	}

	// what it wrote with a disk dead is all there
	if back := got(ctx, t, conn, "/mnt/"+layout.Datasets[0]+"/after-the-disk-died.txt"); back != "written with one disk of the pair dead\n" {
		t.Errorf("written with a disk dead, the file reads back as %q", back)
	}

	var said []alert
	if !eventually(ctx, 4*time.Minute, func() bool { said = alerted(ctx, t, conn, "VolumeStatus"); return len(said) > 0 }) {
		t.Error("the NAS raised no alert for a pool with a dead disk")
	}
	for _, one := range said {
		t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
	}

	// how a NAS lists a disk that no longer answers is read, not judged: it is what a check of disks will have to make sense of
	var listed json.RawMessage
	if err := conn.Call(ctx, "disk.details", nil, &listed); err != nil {
		t.Logf("listing the disks with one dead fails with: %v", err)
	} else {
		t.Logf("with one dead, the NAS lists its disks as: %.1500s", listed)
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

	if !eventually(ctx, 4*time.Minute, func() bool { return len(alerted(ctx, t, conn, "VolumeStatus")) == 0 }) {
		t.Error("the NAS still says the pool is not healthy, with a spare in the dead disk's place")
	}
}
