package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// defaultHDDCelsius is how warm a spinning disk gets before audit_disks warns of it: their makers rate most for 55 or 60
	defaultHDDCelsius = 50
	// defaultSSDCelsius is the same for a solid state disk, NVMe among them, which run warmer
	defaultSSDCelsius = 70
	// hotterStill is how far past the warning a disk that names no limit of its own has to be before it is critical
	hotterStill = 10
)

// diskAudit looks at the disks for heat and for what their own SMART checks say.
type diskAudit struct {
	hddCelsius, ssdCelsius float64
}

func (a diskAudit) run(ctx context.Context, r *registry) (*findings, error) {
	disks, err := r.typed.Disk.Query(ctx, nil, truenas.DiskQueryOptions{Extra: map[string]any{"pools": true}})
	if err != nil {
		return nil, fmt.Errorf("listing the disks: %w", err)
	}
	heat, err := r.typed.Disk.Temperatures(ctx, nil, true)
	if err != nil {
		return nil, fmt.Errorf("reading the disks' temperatures: %w", err)
	}
	alerts, err := r.typed.Alert.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the alerts: %w", err)
	}

	// the description gives the temperatures no shape: each disk's name, and its degrees with the most its maker rates it for, or nothing
	var read map[string][]*float64
	if len(heat.Answer) > 0 {
		if err := json.Unmarshal(heat.Answer, &read); err != nil {
			return nil, fmt.Errorf("reading the disks' temperatures: %w", err)
		}
	}

	f := &findings{}
	for _, d := range disks.Answer {
		f.checked++

		reading := read[d.Name]
		if len(reading) == 0 || reading[0] == nil {
			continue
		}
		var limit float64
		if len(reading) > 1 && reading[1] != nil {
			limit = *reading[1]
		}
		a.temperature(f, d, *reading[0], limit)
	}

	for _, alert := range alerts.Answer {
		if alert.Dismissed || !strings.HasPrefix(alert.Klass, "SMART") {
			continue
		}
		severity := warning
		if alert.Level == "CRITICAL" || alert.Level == "ALERT" || alert.Level == "EMERGENCY" {
			severity = critical
		}
		name, _ := argument(alert.Args, "name")
		f.add(severity, "smart_"+strings.ToLower(strings.TrimPrefix(alert.Klass, "SMART")), "disk", orNothing(name, alert.Klass), "", fmt.Sprintf("the disk's own SMART check says: %s The disk may be failing: replace it before it does", strings.TrimSpace(alert.Formatted)))
	}

	return f, nil
}

// temperature is what is wrong with how warm one disk is: past the most its maker rates it for is critical, past the warning for its kind a
// warning.
func (a diskAudit) temperature(f *findings, d truenas.DiskQueryResultItem, celsius, limit float64) {
	warm := a.hddCelsius
	if d.Type == "SSD" {
		warm = a.ssdCelsius
	}
	hot := warm + hotterStill
	if limit > 0 {
		hot = limit
	}

	switch {
	case celsius >= hot:
		f.add(critical, "disk_too_hot", "disk", d.Name, d.Pool, fmt.Sprintf("the disk %s (serial %s) is at %.0f°C, at or past %.0f°C: heat this high shortens a disk's life and can lose data. Look at the cooling", d.Name, d.Serial, celsius, hot))
	case celsius >= warm:
		f.add(warning, "disk_hot", "disk", d.Name, d.Pool, fmt.Sprintf("the disk %s (serial %s) is at %.0f°C, warmer than the %.0f°C a %s should run at", d.Name, d.Serial, celsius, warm, strings.ToLower(orNothing(d.Type, "disk"))))
	}
}

// argument is one of an alert's arguments, as text, when the alert has them by name.
func argument(args any, name string) (string, bool) {
	named, ok := args.(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := named[name].(string)

	return value, ok
}

func registerDiskAudits(r *registry) {
	type auditDisksIn struct {
		HDDCelsius float64 `json:"hdd_celsius,omitempty" jsonschema:"how warm a spinning disk gets before it is a warning, in °C; 50 unless given"`
		SSDCelsius float64 `json:"ssd_celsius,omitempty" jsonschema:"how warm a solid state or NVMe disk gets before it is a warning, in °C; 70 unless given"`
		auditIn
	}
	add(r, readTool, &mcp.Tool{
		Name:        "audit_disks",
		Description: "Find the disks that are failing or too hot: a disk past the most its maker rates it for, or warmer than its kind should run, and a disk whose own SMART checks failed a self-test, report uncorrectable errors, or are running out of spare blocks. Errors ZFS counts against a disk are in audit_pools.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in auditDisksIn) (*mcp.CallToolResult, auditOut, error) {
		a := diskAudit{hddCelsius: in.HDDCelsius, ssdCelsius: in.SSDCelsius}
		if a.hddCelsius <= 0 {
			a.hddCelsius = defaultHDDCelsius
		}
		if a.ssdCelsius <= 0 {
			a.ssdCelsius = defaultSSDCelsius
		}
		f, err := a.run(ctx, r)
		if err != nil {
			return nil, auditOut{}, err
		}

		return nil, f.answer(in.pageIn), nil
	})
}
