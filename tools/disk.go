package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerDiskTools(r *registry) {
	type diskRow struct {
		Name        string   `json:"name"                          jsonschema:"the disk, as the system names it (sda, nvme0n1)"`
		Serial      string   `json:"serial"                        jsonschema:"its serial number, which stays the same when the system renames it"`
		Model       string   `json:"model,omitempty"               jsonschema:"its make and model"`
		Type        string   `json:"type"                          jsonschema:"SSD or HDD"`
		SizeBytes   int64    `json:"size_bytes"                    jsonschema:"how big it is, in bytes"`
		RPM         int      `json:"rpm,omitempty"                 jsonschema:"how fast a spinning disk turns"`
		Bus         string   `json:"bus,omitempty"                 jsonschema:"how it is connected: ATA, SCSI, NVME, USB"`
		Pool        string   `json:"pool,omitempty"                jsonschema:"the pool it is part of; none for a disk no pool uses, which a new pool or a replacement can take"`
		Temperature *float64 `json:"temperature_celsius,omitempty" jsonschema:"how warm it is, in degrees Celsius; none for a disk that does not say"`
	}
	type diskListIn struct {
		pageIn
	}
	type diskListOut struct {
		Disks []diskRow `json:"disks"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "disk_list",
		Description: "List the NAS's disks: each one's serial number, model, size, the pool it is part of, and how warm it is. A disk no pool uses is free for a new pool or to replace one that failed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in diskListIn) (*mcp.CallToolResult, diskListOut, error) {
		found, err := r.typed.Disk.Query(ctx, nil, truenas.DiskQueryOptions{Extra: map[string]any{"pools": true}, OrderBy: []string{"name"}})
		if err != nil {
			return nil, diskListOut{}, fmt.Errorf("listing the disks: %w", err)
		}
		heat, err := r.typed.Disk.Temperatures(ctx, nil, false)
		if err != nil {
			return nil, diskListOut{}, fmt.Errorf("reading the disks' temperatures: %w", err)
		}
		// the description gives the temperatures no shape: each disk's name, and its degrees or nothing
		var celsius map[string]*float64
		if len(heat.Answer) > 0 {
			if err := json.Unmarshal(heat.Answer, &celsius); err != nil {
				return nil, diskListOut{}, fmt.Errorf("reading the disks' temperatures: %w", err)
			}
		}

		rows := make([]diskRow, 0, len(found.Answer))
		for _, d := range found.Answer {
			rows = append(rows, diskRow{Name: d.Name, Serial: d.Serial, Model: d.Model, Type: d.Type, SizeBytes: int64(d.Size), RPM: d.Rotationrate, Bus: d.Bus, Pool: d.Pool, Temperature: celsius[d.Name]})
		}
		out := diskListOut{}
		out.Disks, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}
