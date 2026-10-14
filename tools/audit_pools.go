package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultScrubDays is how long since a pool's last scrub before audit_pools calls it overdue: TrueNAS scrubs every 35 days unless told otherwise.
const defaultScrubDays = 35

// fine are the states a disk or a vdev is in when nothing is wrong with it: a spare waiting is AVAIL, and one standing in for a disk is INUSE.
var fine = []string{"ONLINE", "AVAIL", "INUSE"}

// poolAudit looks at the pools, the boot pool among them, for what ZFS says is wrong.
type poolAudit struct {
	scrubDays int
	now       time.Time
}

func (a poolAudit) run(ctx context.Context, r *registry) (*findings, error) {
	pools, err := r.typed.Pool.Query(ctx, nil, truenas.PoolQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the pools: %w", err)
	}
	boot, err := r.typed.Boot.GetState(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the boot pool: %w", err)
	}
	tasks, err := r.typed.PoolScrub.Query(ctx, nil, truenas.PoolScrubQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the scrub tasks: %w", err)
	}

	scheduled := map[string]bool{}
	for _, task := range tasks.Answer {
		scheduled[task.PoolName] = scheduled[task.PoolName] || task.Enabled
	}

	f := &findings{}
	for _, p := range pools.Answer {
		a.pool(f, p, true, scheduled[p.Name])
	}
	if b := boot.Answer; b != nil {
		// the boot pool is scrubbed by its own setting, not a task, and its layout reads the same
		a.pool(f, truenas.PoolQueryResultItem{Name: b.Name, Status: b.Status, Healthy: b.Healthy, Warning: b.Warning, StatusCode: b.StatusCode, StatusDetail: b.StatusDetail, Scan: b.Scan, Topology: b.Topology}, false, true)
	}

	return f, nil
}

// pool is what is wrong with one pool; tasked says whether its scrubs are a task's to schedule, as every pool's but the boot pool's are.
func (a poolAudit) pool(f *findings, p truenas.PoolQueryResultItem, tasked, scheduled bool) {
	f.checked++

	if !p.Healthy {
		f.add(critical, "pool_unhealthy", "pool", p.Name, p.Name, fmt.Sprintf("the pool %s is not healthy: ZFS says %s%s. %s", p.Name, p.Status, sayCode(p.StatusCode), orNothing(p.StatusDetail, "pool_get shows which disks")))
	}
	if p.Warning {
		f.add(notice, "pool_warning", "pool", p.Name, p.Name, fmt.Sprintf("the NAS has a warning about the pool %s, often that its ZFS features can be upgraded: %s", p.Name, orNothing(p.StatusDetail, "alert_list says what")))
	}

	for _, vdev := range vdevsOf(p.Topology) {
		if len(vdev.Children) > 0 && !slices.Contains(fine, vdev.Status) {
			f.add(critical, "vdev_not_online", "vdev", vdev.Name, p.Name, fmt.Sprintf("the %s vdev %s of the pool %s is %s: it has lost a disk, and one more may lose the pool's data", strings.ToLower(vdev.Type), vdev.Name, p.Name, vdev.Status))
		}
		for _, disk := range disksOf(vdev.PoolVdev) {
			a.disk(f, disk, p.Name)
		}
	}

	switch s := p.Scan; {
	case s != nil && s.Errors > 0:
		f.add(critical, "scrub_found_errors", "pool", p.Name, p.Name, fmt.Sprintf("the last %s of the pool %s found %d errors: what it could not repair is lost, and pool_get shows which disks they were on", strings.ToLower(s.Function), p.Name, s.Errors))
	case s == nil || s.Function == "":
		f.add(warning, "never_scrubbed", "pool", p.Name, p.Name, fmt.Sprintf("the pool %s has never been scrubbed, so nothing has read all of it back to see that it is whole", p.Name))
	case s.State == "FINISHED" && !s.EndTime.IsZero() && a.now.Sub(s.EndTime.Time) > time.Duration(a.scrubDays)*24*time.Hour:
		f.add(warning, "scrub_overdue", "pool", p.Name, p.Name, fmt.Sprintf("the pool %s was last scrubbed %d days ago, more than %d", p.Name, int(a.now.Sub(s.EndTime.Time).Hours()/24), a.scrubDays))
	}
	if tasked && !scheduled {
		f.add(warning, "no_scrub_task", "pool", p.Name, p.Name, fmt.Sprintf("the pool %s has no scrub task that is switched on, so it is never scrubbed unless someone asks", p.Name))
	}
}

// disk is what is wrong with one disk of a pool.
func (poolAudit) disk(f *findings, disk truenas.PoolVdev, pool string) {
	name := orNothing(disk.Disk, disk.Name)
	if !slices.Contains(fine, disk.Status) {
		gone := ""
		if disk.UnavailDisk != nil {
			gone = fmt.Sprintf(": the disk last seen there was %s, serial %s", disk.UnavailDisk.Name, disk.UnavailDisk.Serial)
		}
		f.add(critical, "disk_not_online", "disk", name, pool, fmt.Sprintf("a disk of the pool %s is %s%s", pool, disk.Status, gone))
	}
	if c := countsOf(disk.Stats); c.ReadErrors+c.WriteErrors+c.ChecksumErrors > 0 {
		f.add(warning, "disk_errors", "disk", name, pool, fmt.Sprintf("the disk %s of the pool %s has had %d read, %d write and %d checksum errors since the pool was imported: a disk, cable or controller is failing", name, pool, c.ReadErrors, c.WriteErrors, c.ChecksumErrors))
	}
}

// sayCode is ZFS's code for what is wrong, as part of a sentence.
func sayCode(code string) string {
	if code == "" || code == "OK" {
		return ""
	}

	return " (" + code + ")"
}

// orNothing is text, or what to say in its place when there is none.
func orNothing(text, otherwise string) string {
	if strings.TrimSpace(text) == "" {
		return otherwise
	}

	return text
}

func registerPoolAudits(r *registry) {
	type auditPoolsIn struct {
		ScrubDays int `json:"scrub_days,omitempty" jsonschema:"how long since a pool's last scrub before it is overdue, in days; 35 unless given"`
		auditIn
	}
	add(r, readTool, &mcp.Tool{
		Name:        "audit_pools",
		Description: "Find what is wrong with the NAS's pools, the boot pool among them: a pool that is not healthy, a vdev or a disk that is not ONLINE, a disk ZFS has counted errors against, a scrub that found errors, and a pool not scrubbed for 35 days or with no scrub task. A damaged pool can still read ONLINE, so this goes by health and each disk.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in auditPoolsIn) (*mcp.CallToolResult, auditOut, error) {
		days := in.ScrubDays
		if days <= 0 {
			days = defaultScrubDays
		}
		f, err := poolAudit{scrubDays: days, now: time.Now()}.run(ctx, r)
		if err != nil {
			return nil, auditOut{}, err
		}

		return nil, f.answer(in.pageIn), nil
	})
}
