package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// asked is the options of every call of a method the stand-in was sent, as JSON, for a test to see what a tool asked for.
func asked(nas *truenastest.NAS, method string) []string {
	var out []string
	for _, c := range nas.Calls(method) {
		if len(c.Params) > 1 {
			out = append(out, string(c.Params[1]))
		}
	}

	return out
}

// A dataset's snapshots are counted and named by the NAS one dataset at a time, then each read by its name, so a NAS of thousands of snapshots is
// never read whole. Each says when it was taken, what it holds and whether something holds it. A dataset there is none of says so.
func TestSnapshotListReadsOneDatasetsSnapshotsAPageAtATime(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Handle("pool.dataset.query", func(params []json.RawMessage) (any, error) {
		if !strings.Contains(string(params[0]), `"tank/media"`) {
			return nil, truenastest.ErrNoMatch
		}

		return map[string]any{"name": "tank/media"}, nil
	})
	snapshot := func(name, created, used, refs string) map[string]any {
		dataset, short, _ := strings.Cut(name, "@")
		return map[string]any{
			"id": name, "name": name, "dataset": dataset, "snapshot_name": short, "pool": "tank", "type": "SNAPSHOT", "createtxg": "1",
			"properties": map[string]any{"creation": prop(created), "used": prop(used), "referenced": prop("9000"), "userrefs": prop(refs)},
		}
	}
	all := []map[string]any{snapshot("tank/media@auto-1", "1790000000", "100", "0"), snapshot("tank/media@auto-2", "1790003600", "0", "1"), snapshot("tank/media@auto-3", "1790007200", "50", "0")}
	nas.Handle("pool.snapshot.query", func(params []json.RawMessage) (any, error) {
		var options struct {
			Count  bool     `json:"count"`
			Select []string `json:"select"`
			Limit  int      `json:"limit"`
			Offset int      `json:"offset"`
		}
		if err := json.Unmarshal(params[1], &options); err != nil {
			return nil, err
		}
		switch {
		case options.Count:
			return len(all), nil
		case slices.Equal(options.Select, []string{"name"}):
			page := all[min(options.Offset, len(all)):]
			page = page[:min(options.Limit, len(page))]
			names := make([]map[string]any, 0, len(page))
			for _, s := range page {
				names = append(names, map[string]any{"name": s["name"]})
			}

			return names, nil
		}
		for _, s := range all {
			if strings.Contains(string(params[0]), `"`+acctest.Str(s["name"])+`"`) {
				return []map[string]any{s}, nil
			}
		}

		return []map[string]any{}, nil
	})

	suite := session(t, nas, Options{})
	out := suite.Call(t, "snapshot_list", map[string]any{"dataset": "tank/media", "limit": 2, "offset": 1})
	rows := acctest.Rows(t, out["snapshots"], "snapshots")
	if len(rows) != 2 || acctest.Num(t, out["total"], "total") != 3 || out["next_offset"] != nil {
		t.Fatalf("listed %v", out)
	}
	if a := rows[0]; a["name"] != "tank/media@auto-2" || a["snapshot"] != "auto-2" || a["dataset"] != "tank/media" || a["created"] != "2026-09-21T15:13:20Z" || acctest.Num(t, a["used_bytes"], "used_bytes") != 0 || acctest.Num(t, a["referenced_bytes"], "referenced_bytes") != 9000 || !acctest.BoolOf(a["held"]) {
		t.Errorf("the second snapshot reads as %v", a)
	}
	if b := rows[1]; b["name"] != "tank/media@auto-3" || acctest.Num(t, b["used_bytes"], "used_bytes") != 50 || b["held"] != nil {
		t.Errorf("the third snapshot reads as %v", b)
	}

	// each snapshot was asked after by its name, for the few properties answered
	for _, options := range asked(nas, "pool.snapshot.query") {
		if strings.Contains(options, "properties") && !strings.Contains(options, `"properties":["creation","used","referenced","userrefs"]`) {
			t.Errorf("a snapshot was asked after with %s", options)
		}
	}

	if msg := suite.CallErr(t, "snapshot_list", map[string]any{"dataset": "tank/nowhere"}); !strings.Contains(msg, `no dataset "tank/nowhere"`) {
		t.Errorf("a dataset there is none of: %q", msg)
	}
	if msg := suite.CallErr(t, "snapshot_list", map[string]any{"dataset": ""}); !strings.Contains(msg, "say which dataset") {
		t.Errorf("no dataset: %q", msg)
	}
}

// The tasks that protect data say what each does, when, and how its last run went, in one form whatever the kind: a schedule as cron, a state or a
// last run with the NAS's own reason. What the NAS keeps of a task that is secret, a replication's key and a cloud account's, is never asked for.
func TestTheTasksThatProtectDataSayWhatTheyDoAndHowTheyLastWent(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	schedule := `{"minute": "0", "hour": "3", "dom": "*", "month": "*", "dow": "7"}`
	failed := `{"id": 9, "method": "rsynctask.run", "state": "FAILED", "error": "rsync: connection refused", "time_finished": {"$date": 1790000000000}}`
	nas.Answer("pool.snapshottask.query", json.RawMessage(`[{"id": 1, "dataset": "tank/media", "recursive": true, "naming_schema": "auto-%Y-%m-%d", "lifetime_value": 2, "lifetime_unit": "WEEK", "enabled": true, "schedule": `+schedule+`, "state": {"state": "FINISHED", "datetime": {"$date": 1790000000000}, "last_snapshot": "tank/media@auto-2026-09-21"}}]`))
	nas.Answer("replication.query", json.RawMessage(`[{"id": 2, "name": "to the other", "direction": "PUSH", "transport": "SSH", "source_datasets": ["tank/media"], "target_dataset": "backup/media", "recursive": false, "auto": true, "enabled": true, "state": {"state": "ERROR", "error": "no route to host"}}]`))
	nas.Answer("rsynctask.query", json.RawMessage(`[{"id": 3, "desc": "photos away", "path": "/mnt/tank/photos", "direction": "PUSH", "mode": "MODULE", "remotehost": "far", "remotemodule": "photos", "user": "root", "enabled": false, "locked": false, "schedule": `+schedule+`, "job": `+failed+`}]`))
	nas.Answer("cloudsync.query", json.RawMessage(`[{"id": 4, "description": "to the cloud", "path": "/mnt/tank/docs", "direction": "PUSH", "transfer_mode": "COPY", "credentials": {"name": "my bucket"}, "enabled": true, "locked": true, "schedule": `+schedule+`, "job": null}]`))
	nas.Answer("cloud_backup.query", json.RawMessage(`[{"id": 5, "description": "restic", "path": "/mnt/tank/home", "credentials": {"name": "my bucket"}, "keep_last": 7, "enabled": true, "locked": false, "schedule": `+schedule+`}]`))
	nas.Answer("pool.scrub.query", json.RawMessage(`[{"id": 6, "pool": 1, "pool_name": "tank", "threshold": 35, "enabled": true, "description": "", "schedule": {"minute": "00", "hour": "00", "dom": "*", "month": "*", "dow": "7"}}]`))

	suite := session(t, nas, Options{})

	tasks := acctest.Rows(t, suite.Call(t, "snapshottask_list", nil)["snapshot_tasks"], "snapshot_tasks")
	if len(tasks) != 1 || tasks[0]["schedule"] != "0 3 * * 7" || tasks[0]["keep_for"] != "2 WEEK" || tasks[0]["last_snapshot"] != "tank/media@auto-2026-09-21" || !acctest.BoolOf(tasks[0]["recursive"]) {
		t.Errorf("the snapshot tasks read as %v", tasks)
	}
	if state := acctest.Object(t, tasks[0]["state"], "state"); state["state"] != "FINISHED" || state["when"] != "2026-09-21T14:13:20Z" {
		t.Errorf("the snapshot task stands as %v", tasks[0]["state"])
	}

	replications := acctest.Rows(t, suite.Call(t, "replication_list", nil)["replications"], "replications")
	if len(replications) != 1 || replications[0]["target_dataset"] != "backup/media" || replications[0]["transport"] != "SSH" || !acctest.BoolOf(replications[0]["auto"]) {
		t.Errorf("the replications read as %v", replications)
	}
	if state := acctest.Object(t, replications[0]["state"], "state"); state["state"] != "ERROR" || state["error"] != "no route to host" {
		t.Errorf("the replication stands as %v", replications[0]["state"])
	}

	copies := acctest.Rows(t, suite.Call(t, "rsynctask_list", nil)["rsync_tasks"], "rsync_tasks")
	if len(copies) != 1 || copies[0]["remote"] != "far::photos" || acctest.BoolOf(copies[0]["enabled"]) || copies[0]["description"] != "photos away" {
		t.Errorf("the rsync tasks read as %v", copies)
	}
	if run := acctest.Object(t, copies[0]["last_run"], "last_run"); run["state"] != "FAILED" || run["error"] != "rsync: connection refused" || run["finished"] != "2026-09-21T14:13:20Z" {
		t.Errorf("the rsync task's last run reads as %v", copies[0]["last_run"])
	}

	syncs := acctest.Rows(t, suite.Call(t, "cloudsync_list", nil)["cloud_syncs"], "cloud_syncs")
	if len(syncs) != 1 || syncs[0]["credential"] != "my bucket" || syncs[0]["transfer_mode"] != "COPY" || !acctest.BoolOf(syncs[0]["locked"]) || syncs[0]["last_run"] != nil {
		t.Errorf("the cloud syncs read as %v", syncs)
	}
	backups := acctest.Rows(t, suite.Call(t, "cloud_backup_list", nil)["cloud_backups"], "cloud_backups")
	if len(backups) != 1 || acctest.Num(t, backups[0]["keep_last"], "keep_last") != 7 || backups[0]["credential"] != "my bucket" {
		t.Errorf("the cloud backups read as %v", backups)
	}
	scrubs := acctest.Rows(t, suite.Call(t, "pool_scrub_list", nil)["scrubs"], "scrubs")
	if len(scrubs) != 1 || scrubs[0]["pool"] != "tank" || scrubs[0]["schedule"] != "00 00 * * 7" || acctest.Num(t, scrubs[0]["threshold_days"], "threshold_days") != 35 {
		t.Errorf("the scrub tasks read as %v", scrubs)
	}

	// a replication, a cloud sync and a cloud backup are asked for the fields answered alone, so their secrets never reach this process
	for method, secret := range map[string]string{"replication.query": "encryption_key", "cloudsync.query": "attributes", "cloud_backup.query": "password"} {
		for _, options := range asked(nas, method) {
			if !strings.Contains(options, `"select"`) || strings.Contains(options, secret) {
				t.Errorf("%s was asked with %s", method, options)
			}
		}
	}
}

// What the NAS shares says what each share offers and to whom, and whether it is on, read only or locked; the iSCSI targets say their portals and the
// extents what they offer.
func TestTheSharesSayWhatEachOffersAndToWhom(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("sharing.smb.query", json.RawMessage(`[{"id": 1, "name": "media", "path": "/mnt/tank/media", "purpose": "DEFAULT_SHARE", "comment": "films", "enabled": true, "readonly": true, "browsable": false, "locked": true}]`))
	nas.Answer("sharing.nfs.query", json.RawMessage(`[{"id": 2, "path": "/mnt/tank/nfs", "enabled": true, "ro": false, "hosts": ["client.lan"], "networks": [], "maproot_user": "root", "locked": null}]`))
	nas.Answer("iscsi.target.query", json.RawMessage(`[{"id": 3, "name": "vmstore", "alias": "the vm disks", "mode": "ISCSI", "groups": [{"portal": 1, "initiator": 2, "authmethod": "NONE", "auth": null}], "auth_networks": []}]`))
	nas.Answer("iscsi.extent.query", json.RawMessage(`[{"id": 4, "name": "vm1", "type": "DISK", "disk": "zvol/tank/vm1", "path": "zvol/tank/vm1", "enabled": true, "ro": false, "locked": false}]`))

	suite := session(t, nas, Options{})
	smb := acctest.Rows(t, suite.Call(t, "sharing_smb_list", nil)["shares"], "shares")
	if len(smb) != 1 || smb[0]["name"] != "media" || !acctest.BoolOf(smb[0]["read_only"]) || acctest.BoolOf(smb[0]["browsable"]) || !acctest.BoolOf(smb[0]["locked"]) {
		t.Errorf("the SMB shares read as %v", smb)
	}
	nfs := acctest.Rows(t, suite.Call(t, "sharing_nfs_list", nil)["shares"], "shares")
	if len(nfs) != 1 || nfs[0]["maproot_user"] != "root" || nfs[0]["locked"] != nil || len(acctest.Strs(t, nfs[0]["hosts"], "hosts")) != 1 || nfs[0]["networks"] == nil {
		t.Errorf("the NFS shares read as %v", nfs)
	}
	targets := acctest.Rows(t, suite.Call(t, "iscsi_target_list", nil)["targets"], "targets")
	if len(targets) != 1 || targets[0]["name"] != "vmstore" || len(acctest.RowsOfAny(targets[0]["portals"])) != 1 {
		t.Errorf("the iSCSI targets read as %v", targets)
	}
	extents := acctest.Rows(t, suite.Call(t, "iscsi_extent_list", nil)["extents"], "extents")
	if len(extents) != 1 || extents[0]["disk"] != "zvol/tank/vm1" || extents[0]["type"] != "DISK" {
		t.Errorf("the iSCSI extents read as %v", extents)
	}
}

// What runs on the NAS says whether each thing runs and starts with the NAS: a service, an app with the ports it listens on once each, and a virtual
// machine with the processors and memory it is given. An app is asked for what is answered alone, since its settings can hold its passwords.
func TestWhatRunsOnTheNASSaysWhetherItRuns(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("service.query", json.RawMessage(`[{"id": 1, "service": "cifs", "state": "RUNNING", "enable": true, "pids": [10]}, {"id": 2, "service": "nfs", "state": "STOPPED", "enable": false, "pids": []}]`))
	nas.Answer("app.query", json.RawMessage(`[{"name": "plex", "state": "RUNNING", "version": "1.2.3", "upgrade_available": true, "image_updates_available": false, "custom_app": false, "active_workloads": {"containers": 2, "used_ports": [{"container_port": 32400, "protocol": "tcp", "host_ports": [{"host_port": 32400, "host_ip": "0.0.0.0"}, {"host_port": 32400, "host_ip": "::"}]}, {"container_port": 80, "protocol": "tcp", "host_ports": [{"host_port": 8080, "host_ip": "0.0.0.0"}]}]}}]`))
	nas.Answer("vm.query", json.RawMessage(`[{"id": 1, "name": "small", "description": "a test", "autostart": false, "vcpus": 1, "cores": 2, "threads": 1, "memory": 512, "devices": [{"id": 1}, {"id": 2}], "status": {"state": "STOPPED", "pid": null, "domain_state": "SHUTOFF"}}]`))

	suite := session(t, nas, Options{})
	services := acctest.Rows(t, suite.Call(t, "service_list", nil)["services"], "services")
	if len(services) != 2 || services[0]["service"] != "cifs" || services[0]["state"] != "RUNNING" || !acctest.BoolOf(services[0]["enabled"]) || acctest.BoolOf(services[1]["enabled"]) {
		t.Errorf("the services read as %v", services)
	}
	apps := acctest.Rows(t, suite.Call(t, "app_list", nil)["apps"], "apps")
	if len(apps) != 1 || acctest.Num(t, apps[0]["containers"], "containers") != 2 || !acctest.BoolOf(apps[0]["upgrade_available"]) {
		t.Errorf("the apps read as %v", apps)
	}
	if ports := acctest.RowsOfAny(apps[0]["ports"]); len(ports) != 2 || acctest.Num(t, ports[0], "port") != 8080 || acctest.Num(t, ports[1], "port") != 32400 {
		t.Errorf("the app listens on %v, want 8080 and 32400 once each", apps[0]["ports"])
	}
	for _, options := range asked(nas, "app.query") {
		if !strings.Contains(options, `"select"`) || strings.Contains(options, `"config"`) {
			t.Errorf("the apps were asked for with %s", options)
		}
	}
	machines := acctest.Rows(t, suite.Call(t, "vm_list", nil)["machines"], "machines")
	if len(machines) != 1 || acctest.Num(t, machines[0]["cpus"], "cpus") != 2 || acctest.Num(t, machines[0]["memory_bytes"], "memory_bytes") != 512<<20 || machines[0]["state"] != "STOPPED" || acctest.Num(t, machines[0]["devices"], "devices") != 2 {
		t.Errorf("the virtual machines read as %v", machines)
	}
}

// The network says what is in force, which DHCP may have given rather than anything set: an interface's addresses now, and the gateway and name servers
// the NAS uses.
func TestTheNetworkSaysWhatIsInForce(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("interface.query", json.RawMessage(`[{"id": "enp1s0", "name": "enp1s0", "type": "PHYSICAL", "description": "", "ipv4_dhcp": true, "ipv6_auto": false, "mtu": 1500, "aliases": [], "state": {"name": "enp1s0", "link_state": "LINK_STATE_UP", "link_address": "52:54:00:12:34:56", "mtu": 1500, "aliases": [{"type": "LINK", "address": "52:54:00:12:34:56"}, {"type": "INET", "address": "10.0.2.15", "netmask": 24, "broadcast": "10.0.2.255"}, {"type": "INET6", "address": "fe80::1", "netmask": 64}]}}]`))
	nas.Answer("network.configuration.config", json.RawMessage(`{"id": 1, "hostname": "nas", "domain": "local", "ipv4gateway": "", "ipv6gateway": "", "nameserver1": "", "nameserver2": "", "nameserver3": "", "hosts": [], "state": {"ipv4gateway": "10.0.2.2", "ipv6gateway": "", "nameserver1": "10.0.2.3", "nameserver2": "", "nameserver3": "", "hosts": []}}`))

	suite := session(t, nas, Options{})
	interfaces := acctest.Rows(t, suite.Call(t, "interface_list", nil)["interfaces"], "interfaces")
	if len(interfaces) != 1 || interfaces[0]["link_state"] != "LINK_STATE_UP" || interfaces[0]["mac"] != "52:54:00:12:34:56" || !acctest.BoolOf(interfaces[0]["dhcp"]) {
		t.Errorf("the interfaces read as %v", interfaces)
	}
	if addresses := acctest.Strs(t, interfaces[0]["addresses"], "addresses"); !slices.Equal(addresses, []string{"10.0.2.15/24", "fe80::1/64"}) {
		t.Errorf("the interface has the addresses %v", interfaces[0]["addresses"])
	}
	network := suite.Call(t, "network_configuration_get", nil)
	if network["hostname"] != "nas" || network["ipv4_gateway"] != "10.0.2.2" {
		t.Errorf("the network reads as %v", network)
	}
	if servers := acctest.Strs(t, network["nameservers"], "nameservers"); !slices.Equal(servers, []string{"10.0.2.3"}) {
		t.Errorf("the name servers are %v, want the one DHCP gave", network["nameservers"])
	}
}

// The users and groups leave out what the system made for itself unless asked for, and a user is asked for what is answered alone, since a user's row
// holds the hashes of their passwords.
func TestTheUsersAndGroupsLeaveOutTheSystemsOwnUnlessAsked(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("user.query", json.RawMessage(`[{"id": 70, "uid": 3000, "username": "kt", "full_name": "K T", "group": {"id": 41, "bsdgrp_group": "kt"}, "groups": [42], "roles": ["FULL_ADMIN"], "builtin": false, "locked": false, "password_disabled": false, "smb": true, "sshpubkey": "ssh-ed25519 AAAA", "twofactor_auth_configured": true, "api_keys": [1, 2], "shell": "/usr/bin/zsh", "home": "/mnt/tank/home/kt"}]`))
	nas.Answer("group.query", json.RawMessage(`[{"id": 42, "gid": 3001, "name": "media", "users": [70], "roles": [], "builtin": false, "smb": true, "sudo_commands": [], "sudo_commands_nopasswd": ["ALL"]}]`))

	suite := session(t, nas, Options{})
	users := acctest.Rows(t, suite.Call(t, "user_list", nil)["users"], "users")
	if len(users) != 1 || users[0]["primary_group"] != "kt" || !acctest.BoolOf(users[0]["ssh_key"]) || !acctest.BoolOf(users[0]["two_factor"]) || acctest.Num(t, users[0]["api_keys"], "api_keys") != 2 {
		t.Errorf("the users read as %v", users)
	}
	groups := acctest.Rows(t, suite.Call(t, "group_list", nil)["groups"], "groups")
	if len(groups) != 1 || !acctest.BoolOf(groups[0]["sudo"]) || len(acctest.RowsOfAny(groups[0]["users"])) != 1 {
		t.Errorf("the groups read as %v", groups)
	}
	suite.Call(t, "user_list", map[string]any{"builtin": true})

	users1, users2 := nas.Calls("user.query")[0], nas.Calls("user.query")[1]
	if !strings.Contains(string(users1.Params[0]), `"builtin","=",false`) || strings.Contains(string(users2.Params[0]), "builtin") {
		t.Errorf("the users were asked for with %s, then with builtin %s", users1.Params[0], users2.Params[0])
	}
	if options := string(users1.Params[1]); !strings.Contains(options, `"select"`) || strings.Contains(options, "unixhash") || strings.Contains(options, "smbhash") {
		t.Errorf("the users were asked for with %s", options)
	}
}
