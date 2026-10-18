package tools

import (
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// An SMB share is made by a name not yet taken, of a directory on a pool, and the answer says when the SMB service is not running; it is changed
// setting by setting, found by its name or its number, and removed only with confirm.
func TestAnSMBShareIsMadeChangedAndRemoved(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	shares := storeRows(t, nas, "sharing.smb", func(row, sent map[string]any) {
		copied(row, sent)
		row["locked"] = false
	}, copied)
	nas.Answer("service.query", map[string]any{"id": 4, "service": "cifs", "state": "STOPPED", "enable": false})
	suite := session(t, nas, everything)

	dry := suite.Call(t, "sharing_smb_create", map[string]any{"name": "media", "path": "/mnt/tank/media/", "dry_run": true})
	if changes := acctest.Strs(t, dry["changes"], "changes"); !slices.Equal(changes, []string{"would share /mnt/tank/media over SMB as media"}) || len(nas.Calls("sharing.smb.create")) != 0 {
		t.Errorf("a dry run answered %v", dry)
	}

	made := suite.Call(t, "sharing_smb_create", map[string]any{"name": "media", "path": "/mnt/tank/media", "comment": "films", "hidden": true})
	row := acctest.Object(t, made["share"], "share")
	if row["name"] != "media" || row["path"] != "/mnt/tank/media" || acctest.BoolOf(row["browsable"]) || !acctest.BoolOf(row["enabled"]) || !strings.Contains(acctest.Str(made["note"]), "SMB service is not running") {
		t.Errorf("made %v", made)
	}
	for want, args := range map[string]map[string]any{
		"already has an SMB share named media": {"name": "media", "path": "/mnt/tank/other"},
		"is not a directory on a pool":         {"name": "x", "path": "/etc"},
	} {
		if msg := suite.CallErr(t, "sharing_smb_create", args); !strings.Contains(msg, want) {
			t.Errorf("sharing_smb_create %v = %q, want it to say %q", args, msg, want)
		}
	}

	changed := suite.Call(t, "sharing_smb_update", map[string]any{"share": "media", "read_only": true, "hidden": false, "comment": ""})
	if changes := acctest.Strs(t, changed["changes"], "changes"); !slices.Equal(changes, []string{`set the comment from "films" to none`, "set read only from off to on", "set hidden from on to off"}) {
		t.Errorf("changed %v", changes)
	}
	if sent := nas.Calls("sharing.smb.update"); len(sent) != 2 || string(sent[0].Params[1]) != `{"browsable":true,"readonly":true}` || string(sent[1].Params[1]) != `{"comment":""}` {
		t.Errorf("the NAS was sent %v", sent)
	}
	if same := suite.Call(t, "sharing_smb_update", map[string]any{"share": "1", "read_only": true}); len(acctest.Strs(t, same["changes"], "changes")) != 0 {
		t.Errorf("asked for what is already so, by number: %v", same)
	}

	if asked := suite.Call(t, "sharing_smb_delete", map[string]any{"share": "media"}); acctest.BoolOf(asked["removed"]) || len(shares.rows) != 1 {
		t.Errorf("without confirm: %v", asked)
	}
	if gone := suite.Call(t, "sharing_smb_delete", map[string]any{"share": "media", "confirm": true}); !acctest.BoolOf(gone["removed"]) || len(shares.rows) != 0 {
		t.Errorf("confirmed: %v", gone)
	}
	if msg := suite.CallErr(t, "sharing_smb_delete", map[string]any{"share": "media", "confirm": true}); !strings.Contains(msg, `no SMB share "media"`) {
		t.Errorf("a share there is none of: %q", msg)
	}
}

// An NFS share is made to the machines and networks named, and changed by adding and taking away machines, networks and who a client's users act
// as, a mapping taken away by a call of its own; it is found by its directory or its number, and removed only with confirm.
func TestAnNFSShareIsMadeChangedAndRemoved(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	shares := storeRows(t, nas, "sharing.nfs", func(row, sent map[string]any) {
		copied(row, sent)
		row["locked"] = false
	}, copied)
	nas.Answer("service.query", map[string]any{"id": 5, "service": "nfs", "state": "RUNNING", "enable": true})
	suite := session(t, nas, everything)

	made := suite.Call(t, "sharing_nfs_create", map[string]any{"path": "/mnt/tank/media", "hosts": []string{"10.0.0.5"}, "maproot_user": "root", "security": []string{"sys"}})
	row := acctest.Object(t, made["share"], "share")
	if row["path"] != "/mnt/tank/media" || !slices.Equal(acctest.Strs(t, row["hosts"], "hosts"), []string{"10.0.0.5"}) || row["maproot_user"] != "root" || !slices.Equal(acctest.Strs(t, row["security"], "security"), []string{"SYS"}) || made["note"] != nil {
		t.Errorf("made %v", made)
	}
	if changes := acctest.Strs(t, made["changes"], "changes"); !slices.Equal(changes, []string{"shared /mnt/tank/media over NFS to 10.0.0.5"}) {
		t.Errorf("said it did %v", changes)
	}
	if msg := suite.CallErr(t, "sharing_nfs_create", map[string]any{"path": "/mnt/tank/x", "security": []string{"NTLM"}}); !strings.Contains(msg, "no way for an NFS client to prove") {
		t.Errorf("a way to prove who a client is that NFS has not: %q", msg)
	}

	changed := suite.Call(t, "sharing_nfs_update", map[string]any{"share": "/mnt/tank/media", "add_hosts": []string{"10.0.0.6"}, "remove_hosts": []string{"10.0.0.5"}, "add_networks": []string{"192.168.1.0/24"}, "maproot_user": ""})
	if changes := acctest.Strs(t, changed["changes"], "changes"); !slices.Equal(changes, []string{"set the machines allowed from 10.0.0.5 to 10.0.0.6", "set the networks allowed from none to 192.168.1.0/24", "set who root acts as from root to none"}) {
		t.Errorf("changed %v", changes)
	}
	if sent := nas.Calls("sharing.nfs.update"); len(sent) != 2 || string(sent[0].Params[1]) != `{"hosts":["10.0.0.6"],"networks":["192.168.1.0/24"]}` || string(sent[1].Params[1]) != `{"maproot_user":""}` {
		t.Errorf("the NAS was sent %v", sent)
	}
	if opened := suite.Call(t, "sharing_nfs_update", map[string]any{"share": "1", "remove_hosts": []string{"10.0.0.6"}, "remove_networks": []string{"192.168.1.0/24"}, "dry_run": true}); !slices.Equal(acctest.Strs(t, opened["changes"], "changes"), []string{"would set the machines allowed from 10.0.0.6 to none", "would set the networks allowed from 192.168.1.0/24 to none"}) {
		t.Errorf("taking away the last machine and network: %v", opened)
	}

	if asked := suite.Call(t, "sharing_nfs_delete", map[string]any{"share": "/mnt/tank/media"}); acctest.BoolOf(asked["removed"]) || len(shares.rows) != 1 {
		t.Errorf("without confirm: %v", asked)
	}
	if gone := suite.Call(t, "sharing_nfs_delete", map[string]any{"share": "1", "confirm": true}); !acctest.BoolOf(gone["removed"]) || len(shares.rows) != 0 {
		t.Errorf("confirmed: %v", gone)
	}
}
