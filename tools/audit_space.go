package tools

import (
	"context"
	"fmt"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// defaultPoolPercent is how full a pool gets before audit_space warns of it: ZFS slows as a pool passes 80 per cent
	defaultPoolPercent = 80
	// fullPercent is how full a pool or a quota gets before it is critical: writes fail soon after
	fullPercent = 95
	// defaultQuotaPercent is how much of its quota a dataset uses before audit_space warns of it
	defaultQuotaPercent = 90
)

// spaceProperties are the only properties audit_space asks of each dataset, so that a NAS of thousands is not asked for every property of each.
var spaceProperties = []string{"used", "usedbydataset", "quota", "refquota", "available"}

// spaceAudit looks at the pools and the datasets for space running out.
type spaceAudit struct {
	poolPercent, quotaPercent float64
}

func (a spaceAudit) run(ctx context.Context, r *registry) (*findings, error) {
	pools, err := r.typed.Pool.Query(ctx, nil, truenas.PoolQueryOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the pools: %w", err)
	}
	datasets, err := r.typed.PoolDataset.Query(ctx, []any{everyDataset}, truenas.PoolDatasetQueryOptions{Extra: map[string]any{"flat": true, "retrieve_children": false, "properties": spaceProperties}})
	if err != nil {
		return nil, fmt.Errorf("listing the datasets: %w", err)
	}

	f := &findings{}
	for _, p := range pools.Answer {
		f.checked++
		if p.Size <= 0 {
			continue
		}

		used := float64(p.Allocated) * 100 / float64(p.Size)
		switch {
		case used >= fullPercent:
			f.add(critical, "pool_full", "pool", p.Name, p.Name, fmt.Sprintf("the pool %s is %.0f%% full, with %s free: writes will soon fail, and a ZFS pool this full is slow", p.Name, used, sized(int64(p.Free))))
		case used >= a.poolPercent:
			f.add(warning, "pool_filling", "pool", p.Name, p.Name, fmt.Sprintf("the pool %s is %.0f%% full, with %s free: ZFS slows as a pool passes 80%%, and dataset_list shows what takes the space", p.Name, used, sized(int64(p.Free))))
		}
	}

	for _, d := range datasets.Answer {
		f.checked++
		// a quota counts everything under the dataset and its snapshots; a refquota the dataset's own data alone
		a.quota(f, d, "quota", bytesOf(d.Used), bytesOf(d.Quota))
		a.quota(f, d, "refquota", bytesOf(d.Usedbydataset), bytesOf(d.Refquota))
	}

	return f, nil
}

// quota is what is wrong with how much of one of its quotas a dataset uses.
func (a spaceAudit) quota(f *findings, d truenas.PoolDatasetQueryResultItem, which string, used, limit int64) {
	if limit <= 0 {
		return
	}

	share := float64(used) * 100 / float64(limit)
	switch {
	case share >= fullPercent:
		f.add(critical, which+"_full", "dataset", d.Name, d.Pool, fmt.Sprintf("the dataset %s uses %.0f%% of its %s of %s: writes to it fail when it is reached", d.Name, share, which, sized(limit)))
	case share >= a.quotaPercent:
		f.add(warning, which+"_filling", "dataset", d.Name, d.Pool, fmt.Sprintf("the dataset %s uses %.0f%% of its %s of %s", d.Name, share, which, sized(limit)))
	}
}

// sized is a number of bytes as a person reads it.
func sized(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func registerSpaceAudits(r *registry) {
	type auditSpaceIn struct {
		PoolPercent  float64 `json:"pool_percent,omitempty"  jsonschema:"how full a pool gets before it is a warning, in per cent; 80 unless given. 95 is always critical"`
		QuotaPercent float64 `json:"quota_percent,omitempty" jsonschema:"how much of its quota a dataset uses before it is a warning, in per cent; 90 unless given. 95 is always critical"`
		auditIn
	}
	add(r, readTool, &mcp.Tool{
		Name:        "audit_space",
		Description: "Find where space is running out: a pool more than 80% full, and a dataset near or at its quota, counting what is under it and its snapshots, or its refquota, counting its own data alone. Each dataset is asked for its sizes alone, so a large NAS is read lightly.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in auditSpaceIn) (*mcp.CallToolResult, auditOut, error) {
		a := spaceAudit{poolPercent: in.PoolPercent, quotaPercent: in.QuotaPercent}
		if a.poolPercent <= 0 {
			a.poolPercent = defaultPoolPercent
		}
		if a.quotaPercent <= 0 {
			a.quotaPercent = defaultQuotaPercent
		}
		f, err := a.run(ctx, r)
		if err != nil {
			return nil, auditOut{}, err
		}

		return nil, f.answer(in.pageIn), nil
	})
}
