package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// poolRow is what a list says of a pool: enough to see which is in trouble or filling up.
type poolRow struct {
	ID                   int     `json:"id"                      jsonschema:"the pool's number, which pool_get takes as well as its name"`
	Name                 string  `json:"name"                    jsonschema:"the pool's name"`
	Status               string  `json:"status"                  jsonschema:"ZFS's word for the pool as a whole: ONLINE, DEGRADED, FAULTED, OFFLINE, UNAVAIL or REMOVED. It stays ONLINE through damage ZFS can still read past"`
	Healthy              bool    `json:"healthy"                 jsonschema:"false when anything is wrong with the pool, even while its status is ONLINE"`
	Warning              bool    `json:"warning"                 jsonschema:"true when the NAS has a warning about the pool, such as an upgrade it is waiting for"`
	StatusDetail         string  `json:"status_detail,omitempty" jsonschema:"what ZFS says is wrong, when something is"`
	SizeBytes            int64   `json:"size_bytes"              jsonschema:"how big the pool is, in bytes"`
	AllocatedBytes       int64   `json:"allocated_bytes"         jsonschema:"how much of it is used, in bytes"`
	FreeBytes            int64   `json:"free_bytes"              jsonschema:"how much of it is free, in bytes"`
	UsedPercent          float64 `json:"used_percent"            jsonschema:"how full it is, in per cent; ZFS slows as a pool passes 80"`
	FragmentationPercent int     `json:"fragmentation_percent"   jsonschema:"how broken up its free space is, in per cent"`
	Disks                int     `json:"disks"                   jsonschema:"how many disks it is made of, spares, logs and caches included"`
	LastScrub            string  `json:"last_scrub,omitempty"    jsonschema:"when its last scrub or resilver ended; none for a pool never scrubbed, or one scrubbing now"`
	ScrubErrors          int     `json:"scrub_errors"            jsonschema:"how many errors the last scrub or resilver found"`
}

// poolRowOf is a pool as a list says of it.
func poolRowOf(p truenas.PoolQueryResultItem) poolRow {
	row := poolRow{ID: p.Id, Name: p.Name, Status: p.Status, Healthy: p.Healthy, Warning: p.Warning, StatusDetail: p.StatusDetail}
	row.SizeBytes, row.AllocatedBytes, row.FreeBytes = int64(p.Size), int64(p.Allocated), int64(p.Free)
	if p.Size > 0 {
		row.UsedPercent = float64(p.Allocated) * 100 / float64(p.Size)
	}
	row.FragmentationPercent, _ = strconv.Atoi(strings.TrimSuffix(p.Fragmentation, "%"))
	if p.Scan != nil {
		row.LastScrub, row.ScrubErrors = when(p.Scan.EndTime.Time), p.Scan.Errors
	}
	for _, vdev := range vdevsOf(p.Topology) {
		row.Disks += len(disksOf(vdev.PoolVdev))
	}

	return row
}

// role is a vdev with the part of the pool it serves.
type role struct {
	truenas.PoolVdev

	Role string
}

// vdevsOf is every vdev of a pool's layout, each with the part it serves: data, log, cache, spare, special or dedup.
func vdevsOf(t *truenas.PoolTopology) []role {
	if t == nil {
		return nil
	}

	var all []role
	for _, part := range []struct {
		name  string
		vdevs []truenas.PoolVdev
	}{{"data", t.Data}, {"log", t.Log}, {"cache", t.Cache}, {"spare", t.Spare}, {"special", t.Special}, {"dedup", t.Dedup}} {
		for _, vdev := range part.vdevs {
			all = append(all, role{PoolVdev: vdev, Role: part.name})
		}
	}

	return all
}

// disksOf is the disks a vdev is made of: itself, for a disk alone.
func disksOf(vdev truenas.PoolVdev) []truenas.PoolVdev {
	if len(vdev.Children) == 0 {
		return []truenas.PoolVdev{vdev}
	}

	var disks []truenas.PoolVdev
	for _, child := range vdev.Children {
		disks = append(disks, disksOf(child)...)
	}

	return disks
}

// errorCounts are what ZFS counts against a vdev or a disk.
type errorCounts struct {
	ReadErrors     int `json:"read_errors"     jsonschema:"reads that failed"`
	WriteErrors    int `json:"write_errors"    jsonschema:"writes that failed"`
	ChecksumErrors int `json:"checksum_errors" jsonschema:"reads that came back other than what was written: a disk that lies, or a cable or controller at fault"`
}

func countsOf(s *truenas.PoolVdevStats) errorCounts {
	if s == nil {
		return errorCounts{}
	}

	return errorCounts{ReadErrors: s.ReadErrors, WriteErrors: s.WriteErrors, ChecksumErrors: s.ChecksumErrors}
}

// poolNamed is one pool, by its name or its number; a pool there is none of says so, and where to look.
func poolNamed(ctx context.Context, r *registry, pool string) (truenas.PoolQueryResultItem, error) {
	filter := []any{"name", "=", pool}
	if id, err := strconv.Atoi(pool); err == nil {
		filter = []any{"id", "=", id}
	}

	found, err := r.typed.Pool.QueryOne(ctx, []any{filter}, truenas.PoolQueryOptions{})
	switch {
	case truenasrpc.IsNotFound(err):
		return truenas.PoolQueryResultItem{}, fmt.Errorf("the NAS has no pool %q: pool_list lists the pools it has", pool)
	case err != nil:
		return truenas.PoolQueryResultItem{}, fmt.Errorf("reading the pool %s: %w", pool, err)
	}
	if found.Answer == nil {
		return truenas.PoolQueryResultItem{}, fmt.Errorf("the NAS answered nothing for the pool %s", pool)
	}

	return *found.Answer, nil
}

func registerPoolTools(r *registry) {
	type poolListIn struct {
		pageIn
	}
	type poolListOut struct {
		Pools []poolRow `json:"pools"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "pool_list",
		Description: "List the NAS's storage pools, by name: each one's state, how full it is, how many disks it has and its last scrub. The boot pool is not among them. pool_get describes one, down to each disk.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in poolListIn) (*mcp.CallToolResult, poolListOut, error) {
		found, err := r.typed.Pool.Query(ctx, nil, truenas.PoolQueryOptions{OrderBy: []string{"name"}})
		if err != nil {
			return nil, poolListOut{}, fmt.Errorf("listing the pools: %w", err)
		}

		rows := make([]poolRow, 0, len(found.Answer))
		for _, p := range found.Answer {
			rows = append(rows, poolRowOf(p))
		}
		out := poolListOut{}
		out.Pools, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})

	type diskOut struct {
		Name   string `json:"name"           jsonschema:"the partition ZFS uses, or ZFS's number for a disk it cannot find"`
		Disk   string `json:"disk,omitempty" jsonschema:"the disk, as the system names it (sda); none for a disk that is gone"`
		Status string `json:"status"         jsonschema:"ONLINE, DEGRADED, FAULTED, OFFLINE, UNAVAIL or REMOVED"`
		errorCounts
		// Missing is the disk the NAS last knew in this place, for one that is gone
		Missing string `json:"missing,omitempty" jsonschema:"for a disk that is not ONLINE, the disk the NAS last knew in its place, by name and serial number"`
	}
	type vdevOut struct {
		Role   string `json:"role"   jsonschema:"the part of the pool it serves: data, log, cache, spare, special or dedup"`
		Name   string `json:"name"   jsonschema:"the vdev's name, mirror-0 or raidz2-0, or the disk's for a disk alone"`
		Type   string `json:"type"   jsonschema:"MIRROR, RAIDZ1, RAIDZ2, RAIDZ3, DRAID1, DRAID2, DRAID3, STRIPE or DISK"`
		Status string `json:"status" jsonschema:"ONLINE, DEGRADED, FAULTED, OFFLINE, UNAVAIL or REMOVED"`
		errorCounts
		SizeBytes      int64     `json:"size_bytes,omitempty"      jsonschema:"how big it is, in bytes"`
		AllocatedBytes int64     `json:"allocated_bytes,omitempty" jsonschema:"how much of it is used, in bytes"`
		Disks          []diskOut `json:"disks"`
	}
	type scanOut struct {
		Function    string  `json:"function"               jsonschema:"SCRUB or RESILVER"`
		State       string  `json:"state"                  jsonschema:"SCANNING while it runs, then FINISHED or CANCELED"`
		Started     string  `json:"started,omitempty"      jsonschema:"when it started"`
		Ended       string  `json:"ended,omitempty"        jsonschema:"when it ended; none while it runs"`
		Percent     float64 `json:"percent"                jsonschema:"how far it got, in per cent"`
		Errors      int     `json:"errors"                 jsonschema:"how many errors it found"`
		SecondsLeft *int    `json:"seconds_left,omitempty" jsonschema:"how long it expects to go on, while it runs and can tell"`
	}
	type poolGetIn struct {
		Pool string `json:"pool" jsonschema:"the pool, by name or by number"`
	}
	type poolGetOut struct {
		poolRow
		StatusCode string    `json:"status_code,omitempty" jsonschema:"ZFS's code for what is wrong: FAILING_DEV for a disk with errors, CORRUPT_DATA for data lost"`
		Autotrim   bool      `json:"autotrim"              jsonschema:"whether the pool trims its disks as space is freed"`
		Scan       *scanOut  `json:"scan,omitempty"        jsonschema:"the last scrub or resilver, or the one running"`
		Vdevs      []vdevOut `json:"vdevs"                 jsonschema:"what the pool is made of: each vdev, and the disks under it"`
	}
	add(r, readTool, &mcp.Tool{
		Name:        "pool_get",
		Description: "Describe one pool: its state and space, its last scrub, and what it is made of, down to each disk with ZFS's error counts. A disk that is gone names the disk the NAS last knew in its place.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in poolGetIn) (*mcp.CallToolResult, poolGetOut, error) {
		p, err := poolNamed(ctx, r, in.Pool)
		if err != nil {
			return nil, poolGetOut{}, err
		}

		out := poolGetOut{poolRow: poolRowOf(p), StatusCode: p.StatusCode, Autotrim: p.Autotrim != nil && p.Autotrim.Value == "on"}
		if s := p.Scan; s != nil && s.Function != "" {
			out.Scan = &scanOut{Function: s.Function, State: s.State, Started: when(s.StartTime.Time), Ended: when(s.EndTime.Time), Percent: s.Percentage, Errors: s.Errors, SecondsLeft: s.TotalSecsLeft}
		}
		for _, vdev := range vdevsOf(p.Topology) {
			v := vdevOut{Role: vdev.Role, Name: vdev.Name, Type: vdev.Type, Status: vdev.Status, errorCounts: countsOf(vdev.Stats)}
			if vdev.Stats != nil {
				v.SizeBytes, v.AllocatedBytes = int64(vdev.Stats.Size), int64(vdev.Stats.Allocated)
			}
			for _, disk := range disksOf(vdev.PoolVdev) {
				d := diskOut{Name: disk.Name, Disk: disk.Disk, Status: disk.Status, errorCounts: countsOf(disk.Stats)}
				if gone := disk.UnavailDisk; gone != nil {
					d.Missing = strings.TrimSpace(gone.Name + " " + gone.Serial)
				}
				v.Disks = append(v.Disks, d)
			}
			out.Vdevs = append(out.Vdevs, v)
		}

		return nil, out, nil
	})
}
