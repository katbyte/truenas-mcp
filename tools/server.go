package tools

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/katbyte/go-kt/version"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fullAdmin is the role that may do everything, the administrator's; readOnlyAdmin may see everything and change nothing.
const (
	fullAdmin     = "FULL_ADMIN"
	readOnlyAdmin = "READONLY_ADMIN"
)

func registerServerTools(r *registry) {
	// serverInfoOut names this binary's build beside the NAS's version, because an MCP client keeps the binary it started with, and that is the one
	// that goes stale unnoticed.
	type serverInfoOut struct {
		TrueNASMCPVersion string   `json:"truenas_mcp_version"    jsonschema:"the build of this MCP server answering, e.g. v0.1.0+4@g8909c7c: the tag, the commits since it, and the commit"`
		Address           string   `json:"address"                jsonschema:"the address the NAS is asked at"`
		Version           string   `json:"version"                jsonschema:"the TrueNAS version the NAS runs, e.g. TrueNAS-25.10.7"`
		State             string   `json:"state"                  jsonschema:"READY, or BOOTING or SHUTTING_DOWN while the NAS is starting or stopping, when most of what it holds cannot be read"`
		Hostname          string   `json:"hostname"               jsonschema:"the name the NAS goes by"`
		User              string   `json:"user"                   jsonschema:"the user the API key belongs to"`
		Roles             []string `json:"roles"                  jsonschema:"what that user may do: FULL_ADMIN may do everything, READONLY_ADMIN may see everything and change nothing, and the others each cover one part of the NAS"`
		Manufacturer      string   `json:"manufacturer,omitempty" jsonschema:"who made the machine, as its firmware says"`
		Product           string   `json:"product,omitempty"      jsonschema:"the machine's model, as its firmware says"`
		Serial            string   `json:"serial,omitempty"       jsonschema:"the machine's serial number, as its firmware says"`
		CPU               string   `json:"cpu"                    jsonschema:"the processor's model"`
		Cores             int      `json:"cores"                  jsonschema:"how many processor threads the NAS has"`
		PhysicalCores     int      `json:"physical_cores"         jsonschema:"how many physical processor cores it has"`
		MemoryBytes       int64    `json:"memory_bytes"           jsonschema:"how much memory it has, in bytes"`
		ECCMemory         bool     `json:"ecc_memory"             jsonschema:"whether that memory corrects its own errors (ECC), which ZFS leans on to keep data whole"`
		Booted            string   `json:"booted"                 jsonschema:"when the NAS last started, in RFC 3339"`
		UptimeSeconds     int64    `json:"uptime_seconds"         jsonschema:"how long it has been running since, in seconds"`
		Timezone          string   `json:"timezone"               jsonschema:"the time zone the NAS keeps its clock in, which the times it writes without a zone are in"`
		Note              string   `json:"note,omitempty"         jsonschema:"what a caller should know before going further, when there is something"`
	}
	add(r, readTool, &mcp.Tool{
		Name:        "server_info",
		Description: "Check the NAS answers and say what it is: its TrueNAS version, whether it is ready, its name and hardware, how long it has been up, and the user the API key belongs to with what that user may do. Also names this MCP server's own build. A NAS that cannot be reached, or that refuses the key, is an error that says which. Start here.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, serverInfoOut, error) {
		out := serverInfoOut{TrueNASMCPVersion: version.Version, Address: r.nas.Address()}

		var me struct {
			Name      string `json:"pw_name"`
			Privilege struct {
				Roles truenasrpc.Set[string] `json:"roles"`
			} `json:"privilege"`
		}
		if err := r.nas.Call(ctx, "auth.me", nil, &me); err != nil {
			return nil, out, fmt.Errorf("the NAS at %s could not be asked who the API key belongs to: %w", out.Address, err)
		}
		out.User, out.Roles = me.Name, slices.Sorted(slices.Values(me.Privilege.Roles))

		if err := r.nas.Call(ctx, "system.state", nil, &out.State); err != nil {
			return nil, out, fmt.Errorf("the NAS at %s answers, and would not say whether it is ready: %w", out.Address, err)
		}

		var info struct {
			Version       string          `json:"version"`
			Hostname      string          `json:"hostname"`
			Memory        int64           `json:"physmem"`
			CPU           string          `json:"model"`
			Cores         int             `json:"cores"`
			PhysicalCores int             `json:"physical_cores"`
			Uptime        float64         `json:"uptime_seconds"`
			Serial        *string         `json:"system_serial"`
			Product       *string         `json:"system_product"`
			Manufacturer  *string         `json:"system_manufacturer"`
			Booted        truenasrpc.Time `json:"boottime"`
			Timezone      string          `json:"timezone"`
			ECC           bool            `json:"ecc_memory"`
		}
		if err := r.nas.Call(ctx, "system.info", nil, &info); err != nil {
			return nil, out, fmt.Errorf("the NAS at %s answers, and would not describe itself: %w", out.Address, err)
		}
		out.Version, out.Hostname, out.Timezone = info.Version, info.Hostname, info.Timezone
		out.CPU, out.Cores, out.PhysicalCores, out.MemoryBytes, out.ECCMemory = info.CPU, info.Cores, info.PhysicalCores, info.Memory, info.ECC
		out.Serial, out.Product, out.Manufacturer = deref(info.Serial), deref(info.Product), deref(info.Manufacturer)
		out.Booted, out.UptimeSeconds = info.Booted.Time.UTC().Format(time.RFC3339), int64(info.Uptime)

		switch {
		case out.State != "READY":
			out.Note = "the NAS is " + out.State + ": until it is READY much of what it holds cannot be read, and what can may be out of date"
		case r.opts.EnableWrite && !slices.Contains(out.Roles, fullAdmin) && slices.Contains(out.Roles, readOnlyAdmin):
			out.Note = "the API key's user may only read: the tools that change something are registered, and the NAS will refuse them"
		}

		return nil, out, nil
	})
}

// deref is the text behind p, or "" when there is none.
func deref(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}
