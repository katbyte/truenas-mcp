package tools

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/katbyte/go-kt/version"
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

		me, err := r.typed.Auth.Me(ctx)
		if err != nil {
			return nil, out, fmt.Errorf("the NAS at %s could not be asked who the API key belongs to: %w", out.Address, err)
		}
		out.User, out.Roles = me.PwName, rolesOf(me.Privilege)

		state, err := r.typed.System.State(ctx)
		if err != nil {
			return nil, out, fmt.Errorf("the NAS at %s answers, and would not say whether it is ready: %w", out.Address, err)
		}
		if state.Answer != nil {
			out.State = *state.Answer
		}

		info, err := r.typed.System.Info(ctx)
		if err != nil {
			return nil, out, fmt.Errorf("the NAS at %s answers, and would not describe itself: %w", out.Address, err)
		}
		out.Version, out.Hostname, out.Timezone = info.Version, info.Hostname, info.Timezone
		out.CPU, out.Cores, out.PhysicalCores, out.MemoryBytes, out.ECCMemory = info.Model, info.Cores, info.PhysicalCores, int64(info.Physmem), info.EccMemory
		out.Serial, out.Product, out.Manufacturer = info.SystemSerial, info.SystemProduct, info.SystemManufacturer
		out.Booted, out.UptimeSeconds = info.Boottime.UTC().Format(time.RFC3339), int64(info.UptimeSeconds)

		switch {
		case out.State != "READY":
			out.Note = "the NAS is " + out.State + ": until it is READY much of what it holds cannot be read, and what can may be out of date"
		case r.opts.EnableWrite && !slices.Contains(out.Roles, fullAdmin) && slices.Contains(out.Roles, readOnlyAdmin):
			out.Note = "the API key's user may only read: the tools that change something are registered, and the NAS will refuse them"
		}

		return nil, out, nil
	})
}

// rolesOf is the roles a user's privilege names, sorted. Its description leaves the privilege shapeless, and a NAS writes the roles as a plain list or
// wrapped as a set ({"$set": [...]}), so both are read.
func rolesOf(privilege map[string]any) []string {
	held := privilege["roles"]
	if set, wrapped := held.(map[string]any); wrapped {
		held = set["$set"]
	}
	listed, ok := held.([]any)
	if !ok {
		return []string{}
	}

	roles := make([]string, 0, len(listed))
	for _, role := range listed {
		if name, ok := role.(string); ok {
			roles = append(roles, name)
		}
	}
	slices.Sort(roles)

	return roles
}
