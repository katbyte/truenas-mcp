package tools

import (
	"context"
	"fmt"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What the NAS offers to other machines: SMB and NFS shares of its directories, and iSCSI targets of its volumes.

// smbShareRow is an SMB share as a list answers it.
type smbShareRow struct {
	ID          int    `json:"id"                     jsonschema:"the share's number"`
	Name        string `json:"name"                   jsonschema:"the name other machines see it by"`
	Path        string `json:"path"                   jsonschema:"the directory it shares"`
	Purpose     string `json:"purpose"                jsonschema:"what it is set up for, which decides much of how it behaves: DEFAULT_SHARE, TIMEMACHINE_SHARE, MULTIPROTOCOL_SHARE and the like"`
	Comment     string `json:"comment,omitempty"      jsonschema:"what it is for, in its owner's words"`
	Enabled     bool   `json:"enabled"                jsonschema:"whether it is offered at all"`
	ReadOnly    bool   `json:"read_only"              jsonschema:"whether it may only be read"`
	Browsable   bool   `json:"browsable"              jsonschema:"whether it is shown to machines that look for shares; a hidden one is still there for those that name it"`
	Locked      bool   `json:"locked,omitempty"       jsonschema:"whether its directory is on a locked dataset, so it serves nothing until the dataset is unlocked"`
	AccessBased bool   `json:"access_based,omitempty" jsonschema:"whether a machine is shown only the files and folders its user may read"`
}

// smbShareRowOf is an SMB share as a list answers it.
func smbShareRowOf(s *truenas.SmbShareQueryResultItem) smbShareRow {
	return smbShareRow{ID: s.Id, Name: s.Name, Path: s.Path, Purpose: string(s.Purpose), Comment: s.Comment, Enabled: s.Enabled, ReadOnly: s.Readonly, Browsable: s.Browsable, Locked: s.Locked != nil && *s.Locked, AccessBased: s.AccessBasedShareEnumeration}
}

func registerSharingTools(r *registry) {
	type smbShareListIn struct {
		pageIn
	}
	type smbShareListOut struct {
		Shares []smbShareRow `json:"shares"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "sharing_smb_list",
		Description: "List the SMB shares, which Windows, macOS and Linux machines mount: each one's name, the directory it shares, what it is set up for, and whether it is on, read only or locked.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in smbShareListIn) (*mcp.CallToolResult, smbShareListOut, error) {
		found, err := r.typed.SharingSmb.Query(ctx, nil, truenas.SharingSmbQueryOptions{OrderBy: []string{"name"}})
		if err != nil {
			return nil, smbShareListOut{}, fmt.Errorf("listing the SMB shares: %w", err)
		}

		rows := make([]smbShareRow, 0, len(found.Answer))
		for i := range found.Answer {
			rows = append(rows, smbShareRowOf(&found.Answer[i]))
		}
		out := smbShareListOut{}
		out.Shares, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})

	type nfsShareListIn struct {
		pageIn
	}
	type nfsShareListOut struct {
		Shares []nfsShareRow `json:"shares"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "sharing_nfs_list",
		Description: "List the NFS shares, which Linux and other Unix machines mount: the directory each shares, which machines may mount it, who a client's users act as, and whether it is on, read only or locked.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nfsShareListIn) (*mcp.CallToolResult, nfsShareListOut, error) {
		found, err := r.typed.SharingNfs.Query(ctx, nil, truenas.SharingNfsQueryOptions{OrderBy: []string{"path"}})
		if err != nil {
			return nil, nfsShareListOut{}, fmt.Errorf("listing the NFS shares: %w", err)
		}

		rows := make([]nfsShareRow, 0, len(found.Answer))
		for i := range found.Answer {
			rows = append(rows, nfsShareRowOf(&found.Answer[i]))
		}
		out := nfsShareListOut{}
		out.Shares, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})

	type iscsiTargetRow struct {
		ID           int      `json:"id"              jsonschema:"the target's number"`
		Name         string   `json:"name"            jsonschema:"its name, the last part of the IQN an initiator connects to"`
		Alias        string   `json:"alias,omitempty" jsonschema:"a friendlier name for it"`
		Mode         string   `json:"mode"            jsonschema:"ISCSI, FC for Fibre Channel, or BOTH"`
		Portals      []int    `json:"portals"         jsonschema:"the portals it is offered on, by number: the addresses and ports initiators connect to"`
		AuthNetworks []string `json:"auth_networks"   jsonschema:"the networks initiators may connect from, as address/prefix; with none, any may"`
	}
	type iscsiTargetListIn struct {
		pageIn
	}
	type iscsiTargetListOut struct {
		Targets []iscsiTargetRow `json:"targets"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "iscsi_target_list",
		Description: "List the iSCSI targets, which offer the NAS's volumes and files to other machines as disks: each one's name, the portals it is offered on and who may connect. iscsi_extent_list lists what they offer.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in iscsiTargetListIn) (*mcp.CallToolResult, iscsiTargetListOut, error) {
		found, err := r.typed.IscsiTarget.Query(ctx, nil, truenas.IscsiTargetQueryOptions{OrderBy: []string{"name"}})
		if err != nil {
			return nil, iscsiTargetListOut{}, fmt.Errorf("listing the iSCSI targets: %w", err)
		}

		rows := make([]iscsiTargetRow, 0, len(found.Answer))
		for _, t := range found.Answer {
			portals := make([]int, 0, len(t.Groups))
			for _, g := range t.Groups {
				portals = append(portals, g.Portal)
			}
			rows = append(rows, iscsiTargetRow{ID: t.Id, Name: t.Name, Alias: t.Alias, Mode: string(t.Mode), Portals: portals, AuthNetworks: orEmpty(t.AuthNetworks)})
		}
		out := iscsiTargetListOut{}
		out.Targets, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})

	type iscsiExtentRow struct {
		ID       int    `json:"id"                jsonschema:"the extent's number"`
		Name     string `json:"name"              jsonschema:"its name"`
		Type     string `json:"type"              jsonschema:"DISK, a volume offered whole, or FILE, a file offered as a disk"`
		Disk     string `json:"disk,omitempty"    jsonschema:"the volume it offers, for a DISK extent: zvol/tank/vm-disk"`
		Path     string `json:"path,omitempty"    jsonschema:"the file it offers, for a FILE extent"`
		Comment  string `json:"comment,omitempty" jsonschema:"what it is for, in its owner's words"`
		Enabled  bool   `json:"enabled"           jsonschema:"whether it is offered"`
		ReadOnly bool   `json:"read_only"         jsonschema:"whether initiators may only read it"`
		Locked   bool   `json:"locked,omitempty"  jsonschema:"whether it is on a locked dataset, so it offers nothing until the dataset is unlocked"`
	}
	type iscsiExtentListIn struct {
		pageIn
	}
	type iscsiExtentListOut struct {
		Extents []iscsiExtentRow `json:"extents"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "iscsi_extent_list",
		Description: "List the iSCSI extents, the volumes and files the NAS offers as disks through its targets: what each offers, and whether it is on, read only or locked.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in iscsiExtentListIn) (*mcp.CallToolResult, iscsiExtentListOut, error) {
		found, err := r.typed.IscsiExtent.Query(ctx, nil, truenas.IscsiExtentQueryOptions{OrderBy: []string{"name"}})
		if err != nil {
			return nil, iscsiExtentListOut{}, fmt.Errorf("listing the iSCSI extents: %w", err)
		}

		rows := make([]iscsiExtentRow, 0, len(found.Answer))
		for _, e := range found.Answer {
			rows = append(rows, iscsiExtentRow{ID: e.Id, Name: e.Name, Type: string(e.Type), Disk: e.Disk, Path: e.Path, Comment: e.Comment, Enabled: e.Enabled, ReadOnly: e.Ro, Locked: e.Locked != nil && *e.Locked})
		}
		out := iscsiExtentListOut{}
		out.Extents, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}

// nfsShareRow is an NFS share as a list answers it.
type nfsShareRow struct {
	ID           int      `json:"id"                      jsonschema:"the share's number"`
	Path         string   `json:"path"                    jsonschema:"the directory it shares, which is also what other machines mount"`
	Comment      string   `json:"comment,omitempty"       jsonschema:"what it is for, in its owner's words"`
	Enabled      bool     `json:"enabled"                 jsonschema:"whether it is offered at all"`
	ReadOnly     bool     `json:"read_only"               jsonschema:"whether it may only be read"`
	Hosts        []string `json:"hosts"                   jsonschema:"the machines, by name or address, allowed to mount it; with none and no networks, any machine may"`
	Networks     []string `json:"networks"                jsonschema:"the networks allowed to mount it, as address/prefix"`
	MaprootUser  string   `json:"maproot_user,omitempty"  jsonschema:"the user a client's root acts as here; none leaves root as root"`
	MapallUser   string   `json:"mapall_user,omitempty"   jsonschema:"the user every client user acts as here"`
	Locked       bool     `json:"locked,omitempty"        jsonschema:"whether its directory is on a locked dataset, so it serves nothing until the dataset is unlocked"`
	MaprootGroup string   `json:"maproot_group,omitempty" jsonschema:"the group a client's root acts as here"`
	MapallGroup  string   `json:"mapall_group,omitempty"  jsonschema:"the group every client user acts as here"`
	Security     []string `json:"security,omitempty"      jsonschema:"how clients may prove who they are: SYS takes the client's word, KRB5, KRB5I and KRB5P need Kerberos; none is what the NFS service allows"`
}

// nfsShareRowOf is an NFS share as a list answers it.
func nfsShareRowOf(s *truenas.NfsShareQueryResultItem) nfsShareRow {
	row := nfsShareRow{ID: s.Id, Path: s.Path, Comment: s.Comment, Enabled: s.Enabled, ReadOnly: s.Ro, Hosts: orEmpty(s.Hosts), Networks: orEmpty(s.Networks), MaprootUser: s.MaprootUser, MapallUser: s.MapallUser, Locked: s.Locked != nil && *s.Locked, MaprootGroup: s.MaprootGroup, MapallGroup: s.MapallGroup}
	for _, security := range s.Security {
		row.Security = append(row.Security, string(security))
	}

	return row
}

// orEmpty is a list as an answer gives it: empty rather than absent, so that none is told apart from not asked.
func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}

	return list
}
