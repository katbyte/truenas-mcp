package tools

import (
	"context"
	"fmt"
	"slices"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What runs on the NAS: its services, the apps it runs in containers, and its virtual machines.

func registerServiceTools(r *registry) {
	type serviceRow struct {
		ID      int    `json:"id"      jsonschema:"the service's number"`
		Service string `json:"service" jsonschema:"the service, as the NAS names it: cifs is SMB, nfs, ssh, iscsitarget, ups, snmp, ftp and the like"`
		State   string `json:"state"   jsonschema:"RUNNING or STOPPED"`
		Enabled bool   `json:"enabled" jsonschema:"whether it starts when the NAS does"`
	}
	type serviceListIn struct {
		pageIn
	}
	type serviceListOut struct {
		Services []serviceRow `json:"services"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "service_list",
		Description: "List the NAS's services, such as SMB, NFS, SSH and iSCSI: whether each is running and whether it starts when the NAS does. A share is only served while its service runs.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in serviceListIn) (*mcp.CallToolResult, serviceListOut, error) {
		found, err := r.typed.Service.Query(ctx, nil, truenas.ServiceQueryOptions{OrderBy: []string{"service"}})
		if err != nil {
			return nil, serviceListOut{}, fmt.Errorf("listing the services: %w", err)
		}

		rows := make([]serviceRow, 0, len(found.Answer))
		for _, s := range found.Answer {
			rows = append(rows, serviceRow{ID: s.Id, Service: s.Service, State: s.State, Enabled: s.Enable})
		}
		out := serviceListOut{}
		out.Services, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}

func registerAppTools(r *registry) {
	type appRow struct {
		Name                  string `json:"name"                    jsonschema:"the app's name, by which it is named to any other tool"`
		State                 string `json:"state"                   jsonschema:"RUNNING, STOPPED, DEPLOYING, STOPPING or CRASHED"`
		Version               string `json:"version"                 jsonschema:"the version of the app installed"`
		UpgradeAvailable      bool   `json:"upgrade_available"       jsonschema:"whether a newer version of the app is in the catalog"`
		ImageUpdatesAvailable bool   `json:"image_updates_available" jsonschema:"whether newer images of its containers can be pulled"`
		Custom                bool   `json:"custom"                  jsonschema:"whether it was set up by hand rather than from the catalog"`
		Containers            int    `json:"containers"              jsonschema:"how many containers it runs"`
		Ports                 []int  `json:"ports"                   jsonschema:"the ports on the NAS it listens on"`
	}
	type appListIn struct {
		pageIn
	}
	type appListOut struct {
		Apps []appRow `json:"apps"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "app_list",
		Description: "List the apps the NAS runs in containers: whether each is running, its version and whether a newer one or newer images are out, and the ports it listens on. A NAS whose apps are not set up has none.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in appListIn) (*mcp.CallToolResult, appListOut, error) {
		// asked for no more than is answered: an app's settings can hold its passwords
		found, err := r.typed.App.Query(ctx, nil, truenas.AppQueryOptions{Select: []any{"name", "state", "version", "upgrade_available", "image_updates_available", "custom_app", "active_workloads.containers", "active_workloads.used_ports"}, OrderBy: []string{"name"}})
		if err != nil {
			return nil, appListOut{}, fmt.Errorf("listing the apps: %w", err)
		}

		rows := make([]appRow, 0, len(found.Answer))
		for _, a := range found.Answer {
			row := appRow{Name: a.Name, State: string(a.State), Version: a.Version, UpgradeAvailable: a.UpgradeAvailable, ImageUpdatesAvailable: a.ImageUpdatesAvailable, Custom: a.CustomApp, Ports: []int{}}
			if w := a.ActiveWorkloads; w != nil {
				row.Containers = w.Containers
				for _, p := range w.UsedPorts {
					for _, host := range p.HostPorts {
						if port := host.HostPort; port > 0 && !slices.Contains(row.Ports, port) {
							row.Ports = append(row.Ports, port)
						}
					}
				}
			}
			slices.Sort(row.Ports)
			rows = append(rows, row)
		}
		out := appListOut{}
		out.Apps, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}

func registerVMTools(r *registry) {
	type vmRow struct {
		ID          int    `json:"id"                    jsonschema:"the machine's number"`
		Name        string `json:"name"                  jsonschema:"its name"`
		Description string `json:"description,omitempty" jsonschema:"what it is for, in its owner's words"`
		State       string `json:"state"                 jsonschema:"RUNNING, STOPPED or SUSPENDED"`
		Autostart   bool   `json:"autostart"             jsonschema:"whether it starts when the NAS does"`
		CPUs        int    `json:"cpus"                  jsonschema:"how many processors it is given: sockets times cores times threads"`
		MemoryBytes int64  `json:"memory_bytes"          jsonschema:"how much memory it is given, in bytes"`
		Devices     int    `json:"devices"               jsonschema:"how many devices it has: disks, network cards, displays and the like"`
	}
	type vmListIn struct {
		pageIn
	}
	type vmListOut struct {
		Machines []vmRow `json:"machines"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "vm_list",
		Description: "List the virtual machines the NAS runs: whether each is running, whether it starts with the NAS, and the processors, memory and devices it is given.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmListIn) (*mcp.CallToolResult, vmListOut, error) {
		found, err := r.typed.Vm.Query(ctx, nil, truenas.VmQueryOptions{OrderBy: []string{"name"}})
		if err != nil {
			return nil, vmListOut{}, fmt.Errorf("listing the virtual machines: %w", err)
		}

		rows := make([]vmRow, 0, len(found.Answer))
		for i := range found.Answer {
			m := &found.Answer[i]
			row := vmRow{ID: m.Id, Name: m.Name, Description: m.Description, Autostart: m.Autostart, CPUs: max(m.Vcpus, 1) * max(m.Cores, 1) * max(m.Threads, 1), MemoryBytes: int64(m.Memory) << 20, Devices: len(m.Devices)}
			if m.Status != nil {
				row.State = m.Status.State
			}
			rows = append(rows, row)
		}
		out := vmListOut{}
		out.Machines, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}
