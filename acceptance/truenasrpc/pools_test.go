//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
)

// A NAS with a pool already is given a second one, on a single disk, which is as unsafe as a pool gets: the NAS makes it and calls it healthy,
// so that it rests on one disk is something only its layout says. Everything a pool's disks can have done to them is then done: a spare is
// added and taken away, a second disk is attached, which turns the one disk into a pair, and detached again. The pool is moved away without
// being destroyed, found again and brought back with what it held, and at last removed, which gives both disks back.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestASecondPoolIsMadeGrownTakenApartMovedAwayAndBroughtBack(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 25*time.Minute)

	const name, note = "scratch", "/mnt/scratch/first/note.txt"
	free := spares(ctx, t, conn)
	if len(free) < 2 {
		t.Fatalf("the NAS has %d disks nothing uses, and a second pool wants two", len(free))
	}

	var made struct {
		ID int64 `json:"id"`
	}
	if err := conn.Call(ctx, "pool.create", []any{map[string]any{"name": name, "topology": map[string]any{"data": []any{map[string]any{"type": "STRIPE", "disks": []string{free[0].Name}}}}}}, &made); err != nil {
		t.Fatalf("making a pool on one disk: %v", err)
	}

	// the pool is known by a new number once it has been away, so it is removed by whatever number it goes by at the end
	removed := false
	remove := func(ctx context.Context) error {
		still, found := poolNamed(ctx, t, conn, name)
		if !found {
			return nil
		}

		return conn.Call(ctx, "pool.export", []any{still.ID, map[string]any{"destroy": true, "cascade": true}}, nil)
	}
	t.Cleanup(func() {
		if !removed {
			if err := remove(context.WithoutCancel(ctx)); err != nil {
				t.Errorf("removing the pool: %v", err)
			}
		}
	})

	alone, found := poolNamed(ctx, t, conn, name)
	if !found || alone.ID != made.ID || !alone.Healthy || alone.Status != "ONLINE" || len(alone.Topology.Data) != 1 {
		t.Fatalf("the new pool reads as %+v", alone)
	}
	if on := alone.Topology.Data[0]; on.Type != "DISK" || on.Disk != free[0].Name || len(on.Children) != 0 {
		t.Errorf("a pool on one disk reads as a %s on %q with %d under it; want the one DISK, %s", on.Type, on.Disk, len(on.Children), free[0].Name)
	}

	// both pools are there, and the first is as it was
	var all []pooled
	if err := conn.Call(ctx, "pool.query", nil, &all); err != nil || len(all) != 2 {
		t.Fatalf("the NAS lists %d pools (%v), want the two", len(all), err)
	}
	if first := pool(ctx, t, conn); !first.Healthy {
		t.Errorf("the first pool reads as %s with a second made beside it", first.Status)
	}

	// it holds a dataset and a file like any other
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name + "/first"}}, nil); err != nil {
		t.Fatalf("making a dataset on the second pool: %v", err)
	}
	put(ctx, t, conn, note, "on the second pool\n")

	// a spare waits beside the pool for a disk to fail, and can be taken away again
	if err := conn.Call(ctx, "pool.update", []any{made.ID, map[string]any{"topology": map[string]any{"spares": []string{free[1].Name}}}}, nil); err != nil {
		t.Fatalf("adding a spare: %v", err)
	}
	spared, _ := poolNamed(ctx, t, conn, name)
	if len(spared.Topology.Spare) != 1 || spared.Topology.Spare[0].Disk != free[1].Name {
		t.Fatalf("with a spare added the pool's spares read as %+v; want the one, %s", spared.Topology.Spare, free[1].Name)
	}
	if err := conn.Call(ctx, "pool.remove", []any{made.ID, map[string]any{"label": spared.Topology.Spare[0].GUID}}, nil); err != nil {
		t.Fatalf("taking the spare away: %v", err)
	}
	if !eventually(ctx, 2*time.Minute, func() bool {
		unspared, _ := poolNamed(ctx, t, conn, name)

		return len(unspared.Topology.Spare) == 0 && len(spares(ctx, t, conn)) == len(free)-1
	}) {
		t.Fatal("the spare was taken away, and the pool still has it or the disk is not free")
	}

	// attached, the second disk makes the one a pair
	if err := conn.Call(ctx, "pool.attach", []any{made.ID, map[string]any{"target_vdev": alone.Topology.Data[0].GUID, "new_disk": free[1].Name}}, nil); err != nil {
		t.Fatalf("attaching a second disk: %v", err)
	}

	var paired pooled
	if !eventually(ctx, 5*time.Minute, func() bool {
		paired, _ = poolNamed(ctx, t, conn, name)

		return paired.Healthy && len(paired.Topology.Data) == 1 && paired.Topology.Data[0].Type == "MIRROR" && len(paired.Topology.Data[0].Children) == 2 && paired.Topology.Data[0].Children[1].Status == "ONLINE"
	}) {
		t.Fatalf("with a second disk attached the pool reads as %+v", paired)
	}
	if back := got(ctx, t, conn, note); back != "on the second pool\n" {
		t.Errorf("with a second disk attached the file reads back as %q", back)
	}

	// and detached, the pair is one disk again
	var second string
	for _, disk := range paired.Topology.Data[0].Children {
		if disk.Disk == free[1].Name {
			second = disk.GUID
		}
	}
	if err := conn.Call(ctx, "pool.detach", []any{made.ID, map[string]any{"label": second}}, nil); err != nil {
		t.Fatalf("detaching the second disk: %v", err)
	}
	if single, _ := poolNamed(ctx, t, conn, name); !single.Healthy || len(single.Topology.Data) != 1 || single.Topology.Data[0].Type != "DISK" {
		t.Errorf("with the second disk detached the pool reads as %+v", single)
	}

	// moved away without being destroyed, the pool is not the NAS's any more, and its disk still holds it
	if err := conn.Call(ctx, "pool.export", []any{made.ID, map[string]any{}}, nil); err != nil {
		t.Fatalf("moving the pool away: %v", err)
	}
	if _, still := poolNamed(ctx, t, conn, name); still {
		t.Fatal("the pool was moved away and is still listed")
	}

	var findable []struct {
		Name string `json:"name"`
		GUID string `json:"guid"`
	}
	if err := conn.Call(ctx, "pool.import_find", nil, &findable); err != nil {
		t.Fatalf("looking for pools to bring in: %v", err)
	}
	var guid string
	for _, one := range findable {
		if one.Name == name {
			guid = one.GUID
		}
	}
	if guid == "" {
		t.Fatalf("the pool moved away is not among those found to bring in: %+v", findable)
	}

	if err := conn.Call(ctx, "pool.import_pool", []any{map[string]any{"guid": guid}}, nil); err != nil {
		t.Fatalf("bringing the pool back: %v", err)
	}
	back, found := poolNamed(ctx, t, conn, name)
	if !found || !back.Healthy {
		t.Fatalf("brought back, the pool reads as %+v", back)
	}
	if held := got(ctx, t, conn, note); held != "on the second pool\n" {
		t.Errorf("brought back, the pool's file reads as %q", held)
	}

	if err := remove(ctx); err != nil {
		t.Fatalf("removing the pool: %v", err)
	}
	removed = true

	if _, still := poolNamed(ctx, t, conn, name); still {
		t.Error("the pool was removed and is still listed")
	}

	var given []spare
	if !eventually(ctx, 2*time.Minute, func() bool { given = spares(ctx, t, conn); return len(given) == len(free) }) {
		t.Errorf("the pool gone, the NAS has %d disks nothing uses, and had %d", len(given), len(free))
	}
	if !slices.ContainsFunc(given, func(one spare) bool { return one.Name == free[0].Name }) || pool(ctx, t, conn).Name != layout.Pool {
		t.Errorf("the disks given back are %+v, and %s was one of the pool's", given, free[0].Name)
	}
}

// A pool is looked after as well as made: scrubbed on a schedule and when asked, told to trim, told when it may rebuild itself, and brought up
// to what the NAS's version of ZFS can do. Each is set, read back, and put as it was.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAPoolIsScrubbedOnAScheduleAndItsCareIsSetAndReadBack(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)
	mine := pool(ctx, t, conn)

	// a pool is given a scrub task when it is made, and takes no second one
	type scrubbed struct {
		ID        int64  `json:"id"`
		Pool      int64  `json:"pool"`
		PoolName  string `json:"pool_name"`
		Threshold int    `json:"threshold"`
		Enabled   bool   `json:"enabled"`
	}
	var tasks []scrubbed
	if err := conn.Call(ctx, "pool.scrub.query", []any{[]any{[]any{"pool_name", "=", layout.Pool}}}, &tasks); err != nil || len(tasks) != 1 {
		t.Fatalf("the pool has %d scrub tasks (%v), want the one it was made with", len(tasks), err)
	}
	was := tasks[0]

	err := conn.Call(ctx, "pool.scrub.create", []any{map[string]any{"pool": mine.ID}}, nil)
	if err == nil {
		t.Error("a pool that has a scrub task was given a second")
	}
	t.Logf("a second scrub task for a pool is refused with: %v", err)

	var changed scrubbed
	if err := conn.Call(ctx, "pool.scrub.update", []any{was.ID, map[string]any{"threshold": 21, "enabled": false}}, &changed); err != nil || changed.Threshold != 21 || changed.Enabled {
		t.Errorf("the scrub task changed reads as %+v (%v); want every 21 days and switched off", changed, err)
	}
	if err := conn.Call(ctx, "pool.scrub.update", []any{was.ID, map[string]any{"threshold": was.Threshold, "enabled": was.Enabled}}, nil); err != nil {
		t.Errorf("putting the scrub task back: %v", err)
	}

	// asked to scrub whatever was last scrubbed more than no days ago, the pool is scrubbed, and says how it went
	if err := conn.Call(ctx, "pool.scrub.run", []any{layout.Pool, 0}, nil); err != nil {
		t.Fatalf("asking for a scrub: %v", err)
	}

	var scan struct {
		Scan struct {
			Function string `json:"function"`
			State    string `json:"state"`
			Errors   int64  `json:"errors"`
		} `json:"scan"`
	}
	if !eventually(ctx, 3*time.Minute, func() bool {
		var found []json.RawMessage
		if err := conn.Call(ctx, "pool.query", []any{[]any{[]any{"name", "=", layout.Pool}}}, &found); err != nil || len(found) != 1 {
			t.Fatalf("reading the pool: %v", err)
		}
		_ = json.Unmarshal(found[0], &scan)

		return scan.Scan.Function == "SCRUB" && scan.Scan.State == "FINISHED"
	}) {
		t.Errorf("scrubbed, the pool's last scan reads as %+v", scan.Scan)
	}
	if scan.Scan.Errors != 0 {
		t.Errorf("the scrub of a sound pool found %d errors", scan.Scan.Errors)
	}

	// told to trim, it says so
	trims := func() string {
		var found []struct {
			Autotrim struct {
				Value string `json:"value"`
			} `json:"autotrim"`
		}
		if err := conn.Call(ctx, "pool.query", []any{[]any{[]any{"name", "=", layout.Pool}}}, &found); err != nil || len(found) != 1 {
			t.Fatalf("reading the pool: %v", err)
		}

		return found[0].Autotrim.Value
	}
	if err := conn.Call(ctx, "pool.update", []any{mine.ID, map[string]any{"autotrim": "ON"}}, nil); err != nil {
		t.Fatalf("telling the pool to trim: %v", err)
	}
	if now := trims(); now != "on" {
		t.Errorf("told to trim, the pool says %q", now)
	}
	if err := conn.Call(ctx, "pool.update", []any{mine.ID, map[string]any{"autotrim": "OFF"}}, nil); err != nil || trims() != "off" {
		t.Errorf("told not to trim, the pool says %q (%v)", trims(), err)
	}

	// when a pool may rebuild itself at full speed is one setting for the NAS
	type window struct {
		Enabled bool   `json:"enabled"`
		Begin   string `json:"begin"`
		End     string `json:"end"`
		Weekday []int  `json:"weekday"`
	}
	var before, set window
	if err := conn.Call(ctx, "pool.resilver.config", nil, &before); err != nil {
		t.Fatal(err)
	}
	if err := conn.Call(ctx, "pool.resilver.update", []any{map[string]any{"enabled": !before.Enabled, "begin": "22:00", "end": "06:00", "weekday": []int{6, 7}}}, &set); err != nil || set.Enabled == before.Enabled || set.Begin != "22:00" || set.End != "06:00" || len(set.Weekday) != 2 {
		t.Errorf("the time a pool may rebuild in reads as %+v (%v) once changed", set, err)
	}
	if err := conn.Call(ctx, "pool.resilver.update", []any{map[string]any{"enabled": before.Enabled, "begin": before.Begin, "end": before.End, "weekday": before.Weekday}}, nil); err != nil {
		t.Errorf("putting it back: %v", err)
	}

	// a pool just made is already all its version of ZFS offers, and asking again changes nothing
	var upgraded bool
	if err := conn.Call(ctx, "pool.upgrade", []any{mine.ID}, &upgraded); err != nil || !upgraded {
		t.Errorf("bringing the pool up to date answered %t (%v)", upgraded, err)
	}
	if err := conn.Call(ctx, "pool.is_upgraded", []any{mine.ID}, &upgraded); err != nil || !upgraded {
		t.Errorf("the pool says it is up to date: %t (%v)", upgraded, err)
	}
}

// A disk nothing uses is given a note of what it is for, and wiped, which leaves it a disk nothing uses.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestADiskNothingUsesIsDescribedAndWiped(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	free := spares(ctx, t, conn)
	if len(free) == 0 {
		t.Fatal("the NAS has no disk nothing uses")
	}
	disk := free[0]

	var noted struct {
		Description string `json:"description"`
	}
	if err := conn.Call(ctx, "disk.update", []any{disk.Identifier, map[string]any{"description": "kept for when a disk fails"}}, &noted); err != nil || noted.Description != "kept for when a disk fails" {
		t.Errorf("the disk's note reads as %q (%v)", noted.Description, err)
	}
	if err := conn.Call(ctx, "disk.update", []any{disk.Identifier, map[string]any{"description": ""}}, nil); err != nil {
		t.Errorf("taking the note off: %v", err)
	}

	if err := conn.Call(ctx, "disk.wipe", []any{disk.Name, "QUICK"}, nil); err != nil {
		t.Fatalf("wiping the disk: %v", err)
	}
	if !eventually(ctx, time.Minute, func() bool {
		return slices.ContainsFunc(spares(ctx, t, conn), func(one spare) bool { return one.Name == disk.Name })
	}) {
		t.Error("wiped, the disk is no longer among those nothing uses")
	}
}
