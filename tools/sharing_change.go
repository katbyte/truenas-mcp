package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/katbyte/go-kt/pointer"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Making, changing and removing SMB and NFS shares.

// smbShareFound reads one SMB share by its name or its number, saying what to do when there is none.
func smbShareFound(ctx context.Context, r *registry, share string) (*truenas.SmbShareQueryResultItem, error) {
	filter := []any{"name", "=", share}
	if id, err := strconv.Atoi(share); err == nil {
		filter = []any{"id", "=", id}
	}
	found, err := r.typed.SharingSmb.QueryOne(ctx, []any{filter}, truenas.SharingSmbQueryOptions{})
	switch {
	case truenasrpc.IsNotFound(err):
		return nil, fmt.Errorf("the NAS has no SMB share %q: sharing_smb_list lists them, by name and number", share)
	case err != nil:
		return nil, fmt.Errorf("reading the SMB share %s: %w", share, err)
	case found.Answer == nil:
		return nil, fmt.Errorf("the NAS answered nothing for the SMB share %s", share)
	}

	return found.Answer, nil
}

// nfsShareFound reads one NFS share by the directory it shares or its number, saying what to do when there is none.
func nfsShareFound(ctx context.Context, r *registry, share string) (*truenas.NfsShareQueryResultItem, error) {
	filter := []any{"path", "=", share}
	if id, err := strconv.Atoi(share); err == nil {
		filter = []any{"id", "=", id}
	}
	found, err := r.typed.SharingNfs.QueryOne(ctx, []any{filter}, truenas.SharingNfsQueryOptions{})
	switch {
	case truenasrpc.IsNotFound(err):
		return nil, fmt.Errorf("the NAS has no NFS share %q: sharing_nfs_list lists them, by directory and number", share)
	case err != nil:
		return nil, fmt.Errorf("reading the NFS share %s: %w", share, err)
	case found.Answer == nil:
		return nil, fmt.Errorf("the NAS answered nothing for the NFS share %s", share)
	}

	return found.Answer, nil
}

// served notes when the service a share needs is not running, so the share is there and serves nothing.
func served(ctx context.Context, r *registry, service, shares string, c *changed) {
	found, err := r.typed.Service.QueryOne(ctx, []any{[]any{"service", "=", service}}, truenas.ServiceQueryOptions{})
	switch {
	case err != nil || found.Answer == nil:
		c.noted("whether the %s service runs could not be read: %v", shares, err)
	case found.Answer.State != "RUNNING":
		c.noted("the %s service is not running, so no %s share is served until it is started", shares, shares)
	default:
	}
}

// pathOf is a directory to share as given: under /mnt, where every pool is.
func pathOf(given string) (string, error) {
	path := strings.TrimSuffix(strings.TrimSpace(given), "/")
	if !strings.HasPrefix(path, "/mnt/") {
		return "", fmt.Errorf("%q is not a directory on a pool: a share's path is under /mnt, as a dataset's mountpoint is (/mnt/tank/media)", given)
	}

	return path, nil
}

// securities are the ways an NFS share may let clients prove who they are, as given, or an error naming those there are.
func securities(given []string) ([]truenas.SharingNfsSecurity, error) {
	out := make([]truenas.SharingNfsSecurity, 0, len(given))
	for _, s := range given {
		s = strings.ToUpper(strings.TrimSpace(s))
		if !slices.Contains([]string{"SYS", "KRB5", "KRB5I", "KRB5P"}, s) {
			return nil, fmt.Errorf("%q is no way for an NFS client to prove who it is: SYS, KRB5, KRB5I or KRB5P", s)
		}
		out = append(out, truenas.SharingNfsSecurity(s))
	}

	return out, nil
}

// moved is a list with some added and some taken away, and whether that changed it.
func moved(list, add, remove []string) ([]string, bool) {
	out := slices.Clone(list)
	for _, a := range add {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	out = slices.DeleteFunc(out, func(s string) bool { return slices.Contains(remove, s) })

	return orEmpty(out), !slices.Equal(out, list) && len(out)+len(list) > 0
}

// listedOrNone is a list as a change says it.
func listedOrNone(list []string) string { return orNone(strings.Join(list, ", ")) }

func registerSMBShareChanges(r *registry) {
	type smbShareChangeOut struct {
		Share *smbShareRow `json:"share,omitempty" jsonschema:"the share as it is now, read back; absent on a dry run of making one"`
		changed
	}

	type smbShareCreateIn struct {
		Name        string `json:"name"                   jsonschema:"the name other machines see it by"`
		Path        string `json:"path"                   jsonschema:"the directory to share, under /mnt, as a dataset's mountpoint is (/mnt/tank/media)"`
		Purpose     string `json:"purpose,omitempty"      jsonschema:"what it is set up for, which decides much of how it behaves: DEFAULT_SHARE when not given, TIMEMACHINE_SHARE for macOS backups, MULTIPROTOCOL_SHARE for a directory NFS shares too, and the like"`
		Comment     string `json:"comment,omitempty"      jsonschema:"what it is for, in words"`
		ReadOnly    bool   `json:"read_only,omitempty"    jsonschema:"let it only be read"`
		Hidden      bool   `json:"hidden,omitempty"       jsonschema:"keep it from machines that look for shares; it is still there for those that name it"`
		AccessBased bool   `json:"access_based,omitempty" jsonschema:"show a machine only the files and folders its user may read"`
		Enabled     *bool  `json:"enabled,omitempty"      jsonschema:"whether it is offered; on when not given"`
		DryRun      bool   `json:"dry_run,omitempty"      jsonschema:"say what would be made and make nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "sharing_smb_create",
		Description: "Share a directory over SMB, which Windows, macOS and Linux machines mount, by a name other machines see. Who may use it is decided by the directory's permissions. The answer says when the SMB service is not running, as then no share is served. dry_run says what would be made.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in smbShareCreateIn) (*mcp.CallToolResult, smbShareChangeOut, error) {
		name := strings.TrimSpace(in.Name)
		path, err := pathOf(in.Path)
		switch {
		case err != nil:
			return nil, smbShareChangeOut{}, err
		case name == "":
			return nil, smbShareChangeOut{}, errors.New("say what to call the share: the name other machines see it by")
		}
		if _, err := smbShareFound(ctx, r, name); err == nil {
			return nil, smbShareChangeOut{}, fmt.Errorf("the NAS already has an SMB share named %s: sharing_smb_update changes it", name)
		} else if !strings.HasPrefix(err.Error(), "the NAS has no SMB share") {
			return nil, smbShareChangeOut{}, err
		}

		out := smbShareChangeOut{Changes: []string{}, DryRun: in.DryRun}
		out.did("would share %s over SMB as %s", "shared %s over SMB as %s", path, name)
		if in.DryRun {
			served(ctx, r, "cifs", "SMB", &out.changed)

			return nil, out, nil
		}

		made, err := r.typed.SharingSmb.Create(ctx, truenas.SharingSmbCreateData{Name: name, Path: path, Purpose: truenas.SharingSmbPurpose(strings.ToUpper(strings.TrimSpace(in.Purpose))), Comment: in.Comment, Readonly: pointer.To(in.ReadOnly), Browsable: pointer.To(!in.Hidden), AccessBasedShareEnumeration: pointer.To(in.AccessBased), Enabled: pointer.To(in.Enabled == nil || *in.Enabled)})
		switch {
		case err != nil:
			return nil, out, fmt.Errorf("the SMB share %s was not made: %w", name, err)
		case made.Answer == nil:
			return nil, out, errors.New("the NAS answered nothing for the share it made: sharing_smb_list lists what it has")
		}
		if s, err := smbShareFound(ctx, r, strconv.Itoa(made.Answer.Id)); err != nil {
			out.noted("it could not be read back afterwards: %v", err)
		} else {
			row := smbShareRowOf(s)
			out.Share = &row
		}
		served(ctx, r, "cifs", "SMB", &out.changed)

		return nil, out, nil
	})

	type smbShareUpdateIn struct {
		Share       string  `json:"share"                  jsonschema:"the share, by its name or its number"`
		Name        string  `json:"name,omitempty"         jsonschema:"a new name for it: machines that have it mounted by the old one lose it"`
		Path        string  `json:"path,omitempty"         jsonschema:"another directory for it to share, under /mnt"`
		Purpose     string  `json:"purpose,omitempty"      jsonschema:"what it is set up for: DEFAULT_SHARE, TIMEMACHINE_SHARE, MULTIPROTOCOL_SHARE and the like"`
		Comment     *string `json:"comment,omitempty"      jsonschema:"what it is for, in words; empty takes it away"`
		ReadOnly    *bool   `json:"read_only,omitempty"    jsonschema:"whether it may only be read"`
		Hidden      *bool   `json:"hidden,omitempty"       jsonschema:"whether it is kept from machines that look for shares"`
		AccessBased *bool   `json:"access_based,omitempty" jsonschema:"whether a machine is shown only the files and folders its user may read"`
		Enabled     *bool   `json:"enabled,omitempty"      jsonschema:"whether it is offered at all"`
		DryRun      bool    `json:"dry_run,omitempty"      jsonschema:"say what would change and change nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "sharing_smb_update",
		Description: "Change an SMB share: its name, directory, purpose or comment, whether it may only be read, is hidden or is offered at all. Only what is given changes, and the answer says each setting it moved from what to what. dry_run says what would change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in smbShareUpdateIn) (*mcp.CallToolResult, smbShareChangeOut, error) {
		s, err := smbShareFound(ctx, r, strings.TrimSpace(in.Share))
		if err != nil {
			return nil, smbShareChangeOut{}, err
		}
		row := smbShareRowOf(s)

		out := smbShareChangeOut{Changes: []string{}, DryRun: in.DryRun}
		data := truenas.SharingSmbUpdateData{}
		var moves settings
		var blank []string
		if name := strings.TrimSpace(in.Name); name != "" && name != row.Name {
			moves.move("the name", row.Name, name)
			data.Name = name
		}
		if in.Path != "" {
			path, err := pathOf(in.Path)
			if err != nil {
				return nil, out, err
			}
			if path != row.Path {
				moves.move("the directory", row.Path, path)
				data.Path = path
			}
		}
		if purpose := strings.ToUpper(strings.TrimSpace(in.Purpose)); purpose != "" && purpose != row.Purpose {
			moves.move("the purpose", row.Purpose, purpose)
			data.Purpose = truenas.SharingSmbPurpose(purpose)
		}
		if in.Comment != nil && *in.Comment != row.Comment {
			moves.move("the comment", quotedOrNone(row.Comment), quotedOrNone(*in.Comment))
			data.Comment = *in.Comment
			if *in.Comment == "" {
				blank = append(blank, "comment")
			}
		}
		if in.ReadOnly != nil && *in.ReadOnly != row.ReadOnly {
			moves.move("read only", onOff(row.ReadOnly), onOff(*in.ReadOnly))
			data.Readonly = in.ReadOnly
		}
		if in.Hidden != nil && *in.Hidden == row.Browsable {
			moves.move("hidden", onOff(!row.Browsable), onOff(*in.Hidden))
			data.Browsable = pointer.To(!*in.Hidden)
		}
		if in.AccessBased != nil && *in.AccessBased != row.AccessBased {
			moves.move("showing only what a user may read", onOff(row.AccessBased), onOff(*in.AccessBased))
			data.AccessBasedShareEnumeration = in.AccessBased
		}
		if in.Enabled != nil && *in.Enabled != row.Enabled {
			moves.move("whether it is offered", onOff(row.Enabled), onOff(*in.Enabled))
			data.Enabled = in.Enabled
		}
		moves.say(&out.changed)
		if in.DryRun || len(moves) == 0 {
			out.Share = &row

			return nil, out, nil
		}

		if len(moves) > len(blank) {
			if _, err := r.typed.SharingSmb.Update(ctx, row.ID, data); err != nil {
				return nil, out, fmt.Errorf("the SMB share %s was not changed: %w", row.Name, err)
			}
		}
		if err := blankOut(ctx, r, "sharing.smb.update", row.ID, blank); err != nil {
			return nil, out, fmt.Errorf("the comment of the SMB share %s was not taken away: %w", row.Name, err)
		}
		if s, err := smbShareFound(ctx, r, strconv.Itoa(row.ID)); err != nil {
			out.noted("it could not be read back afterwards: %v", err)
		} else {
			row := smbShareRowOf(s)
			out.Share = &row
		}

		return nil, out, nil
	})

	type smbShareDeleteIn struct {
		Share   string `json:"share"             jsonschema:"the share, by its name or its number"`
		Confirm bool   `json:"confirm,omitempty" jsonschema:"remove it: without this the answer says what would go and nothing is removed"`
	}
	type smbShareDeleteOut struct {
		Share smbShareRow `json:"share" jsonschema:"the share, as it was"`
		removal
	}
	add(r, deleteTool, &mcp.Tool{
		Name:        "sharing_smb_delete",
		Description: "Stop sharing a directory over SMB. Machines that have it mounted lose it; the directory and its files stay as they are. Without confirm the answer says what would go and removes nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in smbShareDeleteIn) (*mcp.CallToolResult, smbShareDeleteOut, error) {
		s, err := smbShareFound(ctx, r, strings.TrimSpace(in.Share))
		if err != nil {
			return nil, smbShareDeleteOut{}, err
		}

		out := smbShareDeleteOut{Share: smbShareRowOf(s)}
		out.goes("the SMB share %s of %s: machines that have it mounted lose it, and the files stay", out.Share.Name, out.Share.Path)
		if !in.Confirm {
			out.Note = "nothing was removed: confirm removes it"

			return nil, out, nil
		}

		if _, err := r.typed.SharingSmb.Delete(ctx, out.Share.ID); err != nil {
			return nil, out, fmt.Errorf("the SMB share %s was not removed: %w", out.Share.Name, err)
		}
		switch _, err := smbShareFound(ctx, r, strconv.Itoa(out.Share.ID)); {
		case err == nil:
			return nil, out, fmt.Errorf("the NAS answered that the SMB share %s was removed, and still has it", out.Share.Name)
		case !strings.HasPrefix(err.Error(), "the NAS has no SMB share"):
			out.Note = "it was removed, and could not be looked for afterwards: " + err.Error()
		default:
		}
		out.done()

		return nil, out, nil
	})
}

func registerNFSShareChanges(r *registry) {
	type nfsShareChangeOut struct {
		Share *nfsShareRow `json:"share,omitempty" jsonschema:"the share as it is now, read back; absent on a dry run of making one"`
		changed
	}

	type nfsShareCreateIn struct {
		Path         string   `json:"path"                    jsonschema:"the directory to share, under /mnt, which is also what other machines mount (/mnt/tank/media)"`
		Comment      string   `json:"comment,omitempty"       jsonschema:"what it is for, in words"`
		ReadOnly     bool     `json:"read_only,omitempty"     jsonschema:"let it only be read"`
		Hosts        []string `json:"hosts,omitempty"         jsonschema:"the machines, by name or address, allowed to mount it; with none and no networks, any machine may"`
		Networks     []string `json:"networks,omitempty"      jsonschema:"the networks allowed to mount it, as address/prefix (192.168.1.0/24)"`
		MaprootUser  string   `json:"maproot_user,omitempty"  jsonschema:"the user a client's root acts as here; root stays root when not given"`
		MaprootGroup string   `json:"maproot_group,omitempty" jsonschema:"the group a client's root acts as here"`
		MapallUser   string   `json:"mapall_user,omitempty"   jsonschema:"the user every client user acts as here, whoever they are on the client"`
		MapallGroup  string   `json:"mapall_group,omitempty"  jsonschema:"the group every client user acts as here"`
		Security     []string `json:"security,omitempty"      jsonschema:"how clients may prove who they are: SYS takes the client's word, KRB5, KRB5I and KRB5P need Kerberos; what the NFS service allows when not given"`
		Enabled      *bool    `json:"enabled,omitempty"       jsonschema:"whether it is offered; on when not given"`
		DryRun       bool     `json:"dry_run,omitempty"       jsonschema:"say what would be made and make nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "sharing_nfs_create",
		Description: "Share a directory over NFS, which Linux and other Unix machines mount, to the machines and networks named, or to any. A client's users act here as the users with the same numbers, unless maproot or mapall says who they act as. The answer says when the NFS service is not running, as then no share is served. dry_run says what would be made.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nfsShareCreateIn) (*mcp.CallToolResult, nfsShareChangeOut, error) {
		path, err := pathOf(in.Path)
		if err != nil {
			return nil, nfsShareChangeOut{}, err
		}
		security, err := securities(in.Security)
		switch {
		case err != nil:
			return nil, nfsShareChangeOut{}, err
		case in.MapallUser != "" && in.MaprootUser != "":
			return nil, nfsShareChangeOut{}, errors.New("mapall_user already covers a client's root: give it or maproot_user, not both")
		}

		out := nfsShareChangeOut{Changes: []string{}, DryRun: in.DryRun}
		to := "any machine"
		if len(in.Hosts)+len(in.Networks) > 0 {
			to = strings.Join(append(slices.Clone(in.Hosts), in.Networks...), ", ")
		}
		out.did("would share %s over NFS to %s", "shared %s over NFS to %s", path, to)
		if in.DryRun {
			served(ctx, r, "nfs", "NFS", &out.changed)

			return nil, out, nil
		}

		made, err := r.typed.SharingNfs.Create(ctx, truenas.SharingNfsCreateData{Path: path, Comment: in.Comment, Ro: pointer.To(in.ReadOnly), Hosts: in.Hosts, Networks: in.Networks, MaprootUser: in.MaprootUser, MaprootGroup: in.MaprootGroup, MapallUser: in.MapallUser, MapallGroup: in.MapallGroup, Security: security, Enabled: pointer.To(in.Enabled == nil || *in.Enabled)})
		switch {
		case err != nil:
			return nil, out, fmt.Errorf("the NFS share of %s was not made: %w", path, err)
		case made.Answer == nil:
			return nil, out, errors.New("the NAS answered nothing for the share it made: sharing_nfs_list lists what it has")
		}
		if s, err := nfsShareFound(ctx, r, strconv.Itoa(made.Answer.Id)); err != nil {
			out.noted("it could not be read back afterwards: %v", err)
		} else {
			row := nfsShareRowOf(s)
			out.Share = &row
		}
		served(ctx, r, "nfs", "NFS", &out.changed)

		return nil, out, nil
	})

	type nfsShareUpdateIn struct {
		Share          string   `json:"share"                     jsonschema:"the share, by the directory it shares or its number"`
		Path           string   `json:"path,omitempty"            jsonschema:"another directory for it to share, under /mnt: machines that have it mounted by the old one lose it"`
		Comment        *string  `json:"comment,omitempty"         jsonschema:"what it is for, in words; empty takes it away"`
		ReadOnly       *bool    `json:"read_only,omitempty"       jsonschema:"whether it may only be read"`
		AddHosts       []string `json:"add_hosts,omitempty"       jsonschema:"machines, by name or address, to allow to mount it"`
		RemoveHosts    []string `json:"remove_hosts,omitempty"    jsonschema:"machines to allow no longer; with no machines and no networks left, any machine may"`
		AddNetworks    []string `json:"add_networks,omitempty"    jsonschema:"networks to allow to mount it, as address/prefix"`
		RemoveNetworks []string `json:"remove_networks,omitempty" jsonschema:"networks to allow no longer"`
		MaprootUser    *string  `json:"maproot_user,omitempty"    jsonschema:"the user a client's root acts as here; empty leaves root as root"`
		MaprootGroup   *string  `json:"maproot_group,omitempty"   jsonschema:"the group a client's root acts as here; empty takes it away"`
		MapallUser     *string  `json:"mapall_user,omitempty"     jsonschema:"the user every client user acts as here; empty takes it away"`
		MapallGroup    *string  `json:"mapall_group,omitempty"    jsonschema:"the group every client user acts as here; empty takes it away"`
		Security       []string `json:"security,omitempty"        jsonschema:"how clients may prove who they are from now on, in place of what it has: SYS, KRB5, KRB5I, KRB5P"`
		Enabled        *bool    `json:"enabled,omitempty"         jsonschema:"whether it is offered at all"`
		DryRun         bool     `json:"dry_run,omitempty"         jsonschema:"say what would change and change nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "sharing_nfs_update",
		Description: "Change an NFS share: its directory or comment, which machines and networks may mount it, who a client's users act as, how clients prove who they are, whether it may only be read or is offered at all. Only what is given changes, and the answer says each setting it moved from what to what. Taking away the last machine and network lets any machine mount it. dry_run says what would change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nfsShareUpdateIn) (*mcp.CallToolResult, nfsShareChangeOut, error) {
		s, err := nfsShareFound(ctx, r, strings.TrimSpace(in.Share))
		if err != nil {
			return nil, nfsShareChangeOut{}, err
		}
		row := nfsShareRowOf(s)

		out := nfsShareChangeOut{Changes: []string{}, DryRun: in.DryRun}
		data := truenas.SharingNfsUpdateData{}
		var moves settings
		var blank []string
		if in.Path != "" {
			path, err := pathOf(in.Path)
			if err != nil {
				return nil, out, err
			}
			if path != row.Path {
				moves.move("the directory", row.Path, path)
				data.Path = path
			}
		}
		if in.Comment != nil && *in.Comment != row.Comment {
			moves.move("the comment", quotedOrNone(row.Comment), quotedOrNone(*in.Comment))
			data.Comment = *in.Comment
			if *in.Comment == "" {
				blank = append(blank, "comment")
			}
		}
		if in.ReadOnly != nil && *in.ReadOnly != row.ReadOnly {
			moves.move("read only", onOff(row.ReadOnly), onOff(*in.ReadOnly))
			data.Ro = in.ReadOnly
		}
		if hosts, changed := moved(row.Hosts, in.AddHosts, in.RemoveHosts); changed {
			moves.move("the machines allowed", listedOrNone(row.Hosts), listedOrNone(hosts))
			data.Hosts = hosts
		}
		if networks, changed := moved(row.Networks, in.AddNetworks, in.RemoveNetworks); changed {
			moves.move("the networks allowed", listedOrNone(row.Networks), listedOrNone(networks))
			data.Networks = networks
		}
		for _, m := range []struct {
			what, name string
			given      *string
			was        string
			into       *string
		}{
			{what: "who root acts as", name: "maproot_user", given: in.MaprootUser, was: row.MaprootUser, into: &data.MaprootUser},
			{what: "the group root acts as", name: "maproot_group", given: in.MaprootGroup, was: row.MaprootGroup, into: &data.MaprootGroup},
			{what: "who every user acts as", name: "mapall_user", given: in.MapallUser, was: row.MapallUser, into: &data.MapallUser},
			{what: "the group every user acts as", name: "mapall_group", given: in.MapallGroup, was: row.MapallGroup, into: &data.MapallGroup},
		} {
			if m.given == nil || *m.given == m.was {
				continue
			}
			moves.move(m.what, orNone(m.was), orNone(*m.given))
			*m.into = *m.given
			if *m.given == "" {
				blank = append(blank, m.name)
			}
		}
		if in.Security != nil {
			security, err := securities(in.Security)
			if err != nil {
				return nil, out, err
			}
			words := make([]string, 0, len(security))
			for _, s := range security {
				words = append(words, string(s))
			}
			if !slices.Equal(words, row.Security) {
				moves.move("how clients prove who they are", listedOrNone(row.Security), listedOrNone(words))
				data.Security = orEmpty(security)
			}
		}
		if in.Enabled != nil && *in.Enabled != row.Enabled {
			moves.move("whether it is offered", onOff(row.Enabled), onOff(*in.Enabled))
			data.Enabled = in.Enabled
		}
		moves.say(&out.changed)
		if in.DryRun || len(moves) == 0 {
			out.Share = &row

			return nil, out, nil
		}

		if len(moves) > len(blank) {
			if _, err := r.typed.SharingNfs.Update(ctx, row.ID, data); err != nil {
				return nil, out, fmt.Errorf("the NFS share of %s was not changed: %w", row.Path, err)
			}
		}
		if err := blankOut(ctx, r, "sharing.nfs.update", row.ID, blank); err != nil {
			return nil, out, fmt.Errorf("the NFS share of %s did not have %s taken away: %w", row.Path, strings.Join(blank, ", "), err)
		}
		if s, err := nfsShareFound(ctx, r, strconv.Itoa(row.ID)); err != nil {
			out.noted("it could not be read back afterwards: %v", err)
		} else {
			row := nfsShareRowOf(s)
			out.Share = &row
		}

		return nil, out, nil
	})

	type nfsShareDeleteIn struct {
		Share   string `json:"share"             jsonschema:"the share, by the directory it shares or its number"`
		Confirm bool   `json:"confirm,omitempty" jsonschema:"remove it: without this the answer says what would go and nothing is removed"`
	}
	type nfsShareDeleteOut struct {
		Share nfsShareRow `json:"share" jsonschema:"the share, as it was"`
		removal
	}
	add(r, deleteTool, &mcp.Tool{
		Name:        "sharing_nfs_delete",
		Description: "Stop sharing a directory over NFS. Machines that have it mounted lose it; the directory and its files stay as they are. Without confirm the answer says what would go and removes nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nfsShareDeleteIn) (*mcp.CallToolResult, nfsShareDeleteOut, error) {
		s, err := nfsShareFound(ctx, r, strings.TrimSpace(in.Share))
		if err != nil {
			return nil, nfsShareDeleteOut{}, err
		}

		out := nfsShareDeleteOut{Share: nfsShareRowOf(s)}
		out.goes("the NFS share of %s: machines that have it mounted lose it, and the files stay", out.Share.Path)
		if !in.Confirm {
			out.Note = "nothing was removed: confirm removes it"

			return nil, out, nil
		}

		if _, err := r.typed.SharingNfs.Delete(ctx, out.Share.ID); err != nil {
			return nil, out, fmt.Errorf("the NFS share of %s was not removed: %w", out.Share.Path, err)
		}
		switch _, err := nfsShareFound(ctx, r, strconv.Itoa(out.Share.ID)); {
		case err == nil:
			return nil, out, fmt.Errorf("the NAS answered that the NFS share of %s was removed, and still has it", out.Share.Path)
		case !strings.HasPrefix(err.Error(), "the NAS has no NFS share"):
			out.Note = "it was removed, and could not be looked for afterwards: " + err.Error()
		default:
		}
		out.done()

		return nil, out, nil
	})
}
