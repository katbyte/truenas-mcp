package tools

import (
	"context"
	"fmt"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Who may use the NAS: its users and groups.

func registerAccountTools(r *registry) {
	type userRow struct {
		ID               int      `json:"id"                      jsonschema:"the user's number in the NAS's own records, which other tools take"`
		UID              int      `json:"uid"                     jsonschema:"the user's number on the system, which owns files"`
		Username         string   `json:"username"                jsonschema:"the name they log in with"`
		FullName         string   `json:"full_name,omitempty"     jsonschema:"their full name"`
		Email            string   `json:"email,omitempty"         jsonschema:"their email address"`
		PrimaryGroup     string   `json:"primary_group,omitempty" jsonschema:"the group their files belong to unless set otherwise"`
		Groups           []int    `json:"groups"                  jsonschema:"the other groups they are in, by number: group_list names them"`
		Roles            []string `json:"roles"                   jsonschema:"what they may do through the NAS's interface and API: FULL_ADMIN, READONLY_ADMIN, SHARING_ADMIN and the like; none for a user who only uses shares"`
		Builtin          bool     `json:"builtin"                 jsonschema:"whether the system made it for its own use"`
		Locked           bool     `json:"locked"                  jsonschema:"whether they are kept from logging in"`
		PasswordDisabled bool     `json:"password_disabled"       jsonschema:"whether they have no password to log in with"`
		SMB              bool     `json:"smb"                     jsonschema:"whether they may use SMB shares"`
		SSHKey           bool     `json:"ssh_key"                 jsonschema:"whether they have an SSH key to log in with"`
		TwoFactor        bool     `json:"two_factor"              jsonschema:"whether they log in with a one-time password as well"`
		APIKeys          int      `json:"api_keys"                jsonschema:"how many API keys they have"`
		Shell            string   `json:"shell,omitempty"         jsonschema:"what they get when they log in to a shell"`
		Home             string   `json:"home,omitempty"          jsonschema:"their home directory"`
	}
	type userListIn struct {
		Builtin bool `json:"builtin,omitempty" jsonschema:"list the users the system made for its own use too, as root and daemon"`
		pageIn
	}
	type userListOut struct {
		Users []userRow `json:"users"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "user_list",
		Description: "List the NAS's users: each one's name, groups and what they may do through the NAS, and how they log in: password, SSH key, one-time password, and whether they are locked out. The users the system made for itself are left out unless asked for.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in userListIn) (*mcp.CallToolResult, userListOut, error) {
		var filters []any
		if !in.Builtin {
			filters = []any{[]any{"builtin", "=", false}}
		}
		// asked for no more than is answered: a user's row holds the hashes of their passwords
		found, err := r.typed.User.Query(ctx, filters, truenas.UserQueryOptions{Select: []any{"id", "uid", "username", "full_name", "email", "group", "groups", "roles", "builtin", "locked", "password_disabled", "smb", "sshpubkey", "twofactor_auth_configured", "api_keys", "shell", "home"}, OrderBy: []string{"username"}})
		if err != nil {
			return nil, userListOut{}, fmt.Errorf("listing the users: %w", err)
		}

		rows := make([]userRow, 0, len(found.Answer))
		for i := range found.Answer {
			u := &found.Answer[i]
			row := userRow{ID: u.Id, UID: u.Uid, Username: u.Username, FullName: u.FullName, Email: u.Email, Groups: orEmpty(u.Groups), Roles: orEmpty(u.Roles), Builtin: u.Builtin, Locked: u.Locked, PasswordDisabled: u.PasswordDisabled, SMB: u.Smb, SSHKey: u.Sshpubkey != "", TwoFactor: u.TwofactorAuthConfigured, APIKeys: len(u.ApiKeys), Shell: u.Shell, Home: u.Home}
			// the primary group is a record the description gives no shape, and its name is under bsdgrp_group
			if group, ok := u.Group["bsdgrp_group"].(string); ok {
				row.PrimaryGroup = group
			}
			rows = append(rows, row)
		}
		out := userListOut{}
		out.Users, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})

	type groupRow struct {
		ID      int      `json:"id"      jsonschema:"the group's number in the NAS's own records, which other tools take and user_list gives"`
		GID     int      `json:"gid"     jsonschema:"the group's number on the system, which owns files"`
		Name    string   `json:"name"    jsonschema:"its name"`
		Users   []int    `json:"users"   jsonschema:"the users in it, by number: user_list names them"`
		Roles   []string `json:"roles"   jsonschema:"what its users may do through the NAS's interface and API"`
		Builtin bool     `json:"builtin" jsonschema:"whether the system made it for its own use"`
		SMB     bool     `json:"smb"     jsonschema:"whether it may be given access to SMB shares"`
		Sudo    bool     `json:"sudo"    jsonschema:"whether its users may run commands as root"`
	}
	type groupListIn struct {
		Builtin bool `json:"builtin,omitempty" jsonschema:"list the groups the system made for its own use too, as wheel and daemon"`
		pageIn
	}
	type groupListOut struct {
		Groups []groupRow `json:"groups"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "group_list",
		Description: "List the NAS's groups: the users in each, what its users may do through the NAS, and whether they may use SMB or run commands as root. The groups the system made for itself are left out unless asked for.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in groupListIn) (*mcp.CallToolResult, groupListOut, error) {
		var filters []any
		if !in.Builtin {
			filters = []any{[]any{"builtin", "=", false}}
		}
		found, err := r.typed.Group.Query(ctx, filters, truenas.GroupQueryOptions{OrderBy: []string{"group"}})
		if err != nil {
			return nil, groupListOut{}, fmt.Errorf("listing the groups: %w", err)
		}

		rows := make([]groupRow, 0, len(found.Answer))
		for _, g := range found.Answer {
			rows = append(rows, groupRow{ID: g.Id, GID: g.Gid, Name: g.Name, Users: orEmpty(g.Users), Roles: orEmpty(g.Roles), Builtin: g.Builtin, SMB: g.Smb, Sudo: len(g.SudoCommands)+len(g.SudoCommandsNopasswd) > 0})
		}
		out := groupListOut{}
		out.Groups, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})
}
