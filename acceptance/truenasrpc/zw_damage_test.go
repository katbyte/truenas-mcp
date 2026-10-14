//go:build acceptance

package truenasrpc_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// faults is how often a disk, or the disks under a pair, failed to read, failed to write, or gave back something other than was written.
type faults struct {
	Read     int64 `json:"read_errors"`
	Write    int64 `json:"write_errors"`
	Checksum int64 `json:"checksum_errors"`
}

// looked is a pool as a check of its health reads it: what it says of itself, what the last look over all of it found, and each disk's faults.
type looked struct {
	ID      int64   `json:"id"`
	Status  string  `json:"status"`
	Healthy bool    `json:"healthy"`
	Warning bool    `json:"warning"`
	Code    *string `json:"status_code"`
	Detail  *string `json:"status_detail"`
	Scan    *struct {
		Function *string `json:"function"`
		State    *string `json:"state"`
		Errors   *int64  `json:"errors"`
	} `json:"scan"`
	Topology struct {
		Data []struct {
			Type     string `json:"type"`
			Status   string `json:"status"`
			Stats    faults `json:"stats"`
			Children []struct {
				Disk   string `json:"disk"`
				Status string `json:"status"`
				Stats  faults `json:"stats"`
			} `json:"children"`
		} `json:"data"`
	} `json:"topology"`
}

// says is the pool's account of itself in a line, for the log.
func (p looked) says() string {
	text := func(s *string) string {
		if s == nil {
			return "nothing"
		}

		return *s
	}

	var out strings.Builder
	fmt.Fprintf(&out, "state %s, healthy %t, warning %t, code %s, detail %q", p.Status, p.Healthy, p.Warning, text(p.Code), text(p.Detail))
	if p.Scan != nil && p.Scan.Errors != nil {
		fmt.Fprintf(&out, ", last looked over by a %s that is %s and found %d errors", text(p.Scan.Function), text(p.Scan.State), *p.Scan.Errors)
	}
	for _, vdev := range p.Topology.Data {
		fmt.Fprintf(&out, "; %s %s %+v", vdev.Type, vdev.Status, vdev.Stats)
		for _, disk := range vdev.Children {
			fmt.Fprintf(&out, ", %s %s %+v", disk.Disk, disk.Status, disk.Stats)
		}
	}

	return out.String()
}

func lookedAt(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, name string) looked {
	t.Helper()

	var pools []looked
	if err := conn.Call(ctx, "pool.query", []any{[]any{[]any{"name", "=", name}}}, &pools); err != nil || len(pools) != 1 {
		t.Fatalf("looking at the pool %s: %d found, %v", name, len(pools), err)
	}

	return pools[0]
}

// scribbled writes rubbish over the part of a disk a new pool keeps its first files in, from under the NAS, as a disk that is going bad does
// to what it holds. The start and the end of the disk, where a pool keeps its account of itself, are left alone.
func scribbled(ctx context.Context, t *testing.T, socket, drive string) {
	t.Helper()

	for at := 32; at < 1056; at += 64 {
		if said := machine(ctx, t, socket, fmt.Sprintf(`qemu-io %s "write -P 0x5a %dM 64M"`, drive, at)); strings.Contains(strings.ToLower(said), "error") || strings.Contains(said, "not found") || strings.Contains(said, "unknown command") {
			t.Fatalf("writing over %s at %d MiB, the machine said: %s", drive, at, said)
		}
	}
}

// A disk that gives back something other than was written is the fault a pool exists to catch, and a NAS does not see it until it reads what
// was damaged, which a scrub does. A pool on a pair of disks is made, and one disk is written over from under it: the scrub finds the damage,
// repairs it from the other disk, and the pool is no longer called healthy, with the count of what was found against the disk at fault and
// none against the other. What the pool holds reads back as it was written. Then both disks are written over, so that no good copy is left:
// the scrub counts what it could not repair, and the pool says data is lost. It runs late in the run, on disks nothing else is using.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAScrubFindsADiskThatLiesRepairsItAndSaysWhatCouldNotBeRepaired(t *testing.T) {
	socket := console(t)
	conn := admin(t)
	ctx := within(t, 25*time.Minute)

	const name, size = "damaged", 256 << 20
	file := "/mnt/" + name + "/held/most-of-it"

	// two of the disks the machine numbers: the one that says how hot it is goes by another name there
	var pair []spare
	for _, disk := range spares(ctx, t, conn) {
		if disk.Serial != layout.HotDiskSerial && strings.HasPrefix(disk.Serial, layout.DiskSerial) {
			pair = append(pair, disk)
		}
	}
	if len(pair) < 2 {
		t.Fatalf("the NAS has %d disks nothing uses that the machine can write over, and a pair is wanted", len(pair))
	}
	lying, honest := pair[0], pair[1]
	drives := []string{drive(ctx, t, conn, socket, lying.Name), drive(ctx, t, conn, socket, honest.Name)}

	if err := conn.Call(ctx, "pool.create", []any{map[string]any{"name": name, "topology": map[string]any{"data": []any{map[string]any{"type": "MIRROR", "disks": []string{lying.Name, honest.Name}}}}}}, nil); err != nil {
		t.Fatalf("making a pool on a pair of disks: %v", err)
	}
	t.Cleanup(func() {
		after := context.WithoutCancel(ctx)

		still, found := poolNamed(after, t, conn, name)
		if !found {
			return
		}
		if err := conn.Call(after, "pool.export", []any{still.ID, map[string]any{"destroy": true, "cascade": true}}, nil); err != nil {
			t.Errorf("removing the pool: %v", err)
		}
	})

	// nothing that could be squeezed, so that what is sent is what the disks hold
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name + "/held", "compression": "OFF"}}, nil); err != nil {
		t.Fatalf("making a dataset on the pool: %v", err)
	}
	sent := sha256.New()
	ledger.count("filesystem.put")
	job, err := conn.StartUpload(ctx, "filesystem.put", []any{file, map[string]any{}}, io.TeeReader(io.LimitReader(rand.NewChaCha8([32]byte{7}), size), sent))
	if err == nil {
		err = job.Wait(ctx, nil)
	}
	if err != nil {
		t.Fatalf("filling the pool: %v", err)
	}

	// what was sent is on the disks once the pool says it holds that much
	if !eventually(ctx, 2*time.Minute, func() bool {
		holds, _ := poolNamed(ctx, t, conn, name)

		return holds.Allocated > size*9/10
	}) {
		t.Fatal("the pool never said it holds what was written to it")
	}
	time.Sleep(10 * time.Second)

	if before := lookedAt(ctx, t, conn, name); !before.Healthy || before.Status != "ONLINE" {
		t.Fatalf("before any damage the pool says: %s", before.says())
	}

	scrub := func() {
		if err := conn.Call(ctx, "pool.scrub.scrub", []any{name, "START"}, nil); err != nil {
			t.Fatalf("scrubbing the pool: %v", err)
		}
	}
	reads := func() (string, error) {
		ledger.count("filesystem.get")

		back := sha256.New()
		_, failed := conn.Download(ctx, "filesystem.get", []any{file}, back)

		return hex.EncodeToString(back.Sum(nil)), failed
	}

	// one disk of the pair lies
	t.Logf("writing over %s (%s to the machine), one disk of the pair", lying.Name, drives[0])
	scribbled(ctx, t, socket, drives[0])
	scrub()

	repaired := lookedAt(ctx, t, conn, name)
	t.Logf("with one disk written over, after a scrub, the pool says: %s", repaired.says())
	if repaired.Healthy {
		t.Error("the pool is still called healthy after a scrub found one of its disks giving back what was never written")
	}
	if len(repaired.Topology.Data) != 1 || len(repaired.Topology.Data[0].Children) != 2 {
		t.Fatalf("the pool does not read as one pair of disks: %s", repaired.says())
	}
	for _, disk := range repaired.Topology.Data[0].Children {
		switch {
		case disk.Disk == lying.Name && disk.Stats.Checksum == 0 && disk.Status == "ONLINE":
			t.Errorf("the disk that was written over, %s, reads as %s with nothing counted against it", disk.Disk, disk.Status)
		case disk.Disk == honest.Name && (disk.Stats.Checksum != 0 || disk.Status != "ONLINE"):
			t.Errorf("the disk that was left alone, %s, reads as %s with %d counted against it", disk.Disk, disk.Status, disk.Stats.Checksum)
		}
	}
	if repaired.Scan == nil || repaired.Scan.Errors == nil || *repaired.Scan.Errors != 0 {
		t.Error("the scrub is said to have left something unrepaired, with a good copy of everything on the other disk")
	}
	if back, err := reads(); err != nil || back != hex.EncodeToString(sent.Sum(nil)) {
		t.Errorf("with one disk written over, what the pool holds does not read back as it was written (%v)", err)
	}

	said := func() {
		var of []alert
		if !eventually(ctx, 4*time.Minute, func() bool {
			of = alertsNaming(ctx, t, conn, "VolumeStatus", "Pool "+name+" ")

			return len(of) > 0
		}) {
			t.Error("the NAS raised no alert for a pool a scrub found damage in")
		}
		for _, one := range of {
			t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
		}
	}
	said()

	// both disks lie, so no good copy of what they held is left
	t.Log("writing over both disks of the pair")
	scribbled(ctx, t, socket, drives[0])
	scribbled(ctx, t, socket, drives[1])
	scrub()

	lost := lookedAt(ctx, t, conn, name)
	t.Logf("with both disks written over, after a scrub, the pool says: %s", lost.says())
	if lost.Healthy {
		t.Error("the pool is called healthy after a scrub found damage it could not repair")
	}
	if lost.Scan == nil || lost.Scan.Errors == nil || *lost.Scan.Errors == 0 {
		t.Error("the scrub is said to have left nothing unrepaired, with both copies of what the pool held written over")
	}
	said()

	// what a read of the lost file does is written down and not held: a NAS may still have it in memory
	back, err := reads()
	t.Logf("reading the file both copies of were written over: as it was written %t, %v", back == hex.EncodeToString(sent.Sum(nil)), err)
}
