//go:build acceptance

package truenasrpc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// marked is a value put where a NAS keeps a secret, which no line of trace may then show: every connection the tests make is traced, so a
// secret the trace does not know to hide fails the run.
func marked(what string) string {
	value := "not-a-real-" + what + "-0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	ledger.hide(value)

	return value
}

// chap is a secret of the length iSCSI takes one in, twelve to sixteen characters, hidden from the trace like any other.
func chap() string {
	const value = "chap0f1e2d3c4b5a"
	ledger.hide(value)

	return value
}

// same reports whether a value a NAS answered is the value it was given.
func same(answered json.RawMessage, given any) bool {
	want, err := json.Marshal(given)
	if err != nil {
		return false
	}

	var a, b bytes.Buffer
	if json.Compact(&a, answered) != nil || json.Compact(&b, want) != nil {
		return false
	}

	return bytes.Equal(a.Bytes(), b.Bytes())
}

// A NAS keeps a list of each kind of thing it is set up with: users and groups, who may do what, keys, scheduled commands, routes, the places
// alerts and backups go, and what it offers over iSCSI and NVMe. One of each kind that needs nothing outside the NAS is made, fetched by
// itself and held to the description, changed, read back changed, and removed, after which asking for it says it is not there. A secret
// given to any of them is never in a trace.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestOneOfEachKindOfThingANASKeepsIsMadeChangedAndRemoved(t *testing.T) {
	conn := admin(t)

	for _, kind := range []struct {
		name    string
		with    map[string]any
		change  map[string]any
		removal []any
	}{
		{name: "group", with: map[string]any{"name": "testers"}, change: map[string]any{"sudo_commands_nopasswd": []string{"/usr/bin/true"}}},
		{name: "user", with: map[string]any{"username": "made-by-tests", "full_name": "Made by the tests", "password": marked("password"), "group_create": true}, change: map[string]any{"full_name": "Changed by the tests"}, removal: []any{map[string]any{"delete_group": true}}},
		{name: "privilege", with: map[string]any{"name": "watchers", "roles": []string{"READONLY_ADMIN"}, "web_shell": false}, change: map[string]any{"name": "onlookers"}},
		{name: "api_key", with: map[string]any{"name": "made-by-tests", "username": layout.AdminUser}, change: map[string]any{"name": "renamed-by-tests"}},
		{name: "cronjob", with: map[string]any{"user": "root", "command": "true", "description": "made by the tests", "enabled": false}, change: map[string]any{"description": "changed by the tests"}},
		{name: "initshutdownscript", with: map[string]any{"type": "COMMAND", "command": "true", "when": "POSTINIT", "enabled": false}, change: map[string]any{"comment": "changed by the tests"}},
		{name: "system.ntpserver", with: map[string]any{"address": "127.0.0.1", "force": true}, change: map[string]any{"prefer": true}},
		{name: "staticroute", with: map[string]any{"destination": "192.0.2.0/24", "gateway": "10.0.2.2", "description": "made by the tests"}, change: map[string]any{"description": "changed by the tests"}},
		{name: "kerberos.realm", with: map[string]any{"realm": "TESTS.EXAMPLE"}, change: map[string]any{"kdc": []string{"kdc.tests.example"}}},
		{name: "tunable", with: map[string]any{"type": "SYSCTL", "var": "vm.swappiness", "value": "11", "comment": "made by the tests"}, change: map[string]any{"comment": "changed by the tests"}},
		{name: "alertservice", with: map[string]any{"name": "made by the tests", "attributes": map[string]any{"type": "Slack", "url": "https://hooks.example.test/" + marked("webhook")}, "level": "WARNING", "enabled": false}, change: map[string]any{"level": "CRITICAL"}},
		{name: "reporting.exporters", with: map[string]any{"name": "made-by-tests", "enabled": false, "attributes": map[string]any{"exporter_type": "GRAPHITE", "destination_ip": "127.0.0.1", "destination_port": 2003, "namespace": "tests"}}, change: map[string]any{"name": "changed-by-tests"}},
		{name: "cloudsync.credentials", with: map[string]any{"name": "made by the tests", "provider": map[string]any{"type": "S3", "access_key_id": marked("access-key"), "secret_access_key": marked("secret-key")}}, change: map[string]any{"name": "changed by the tests"}},
		{name: "acme.dns.authenticator", with: map[string]any{"name": "made-by-tests", "attributes": map[string]any{"authenticator": "cloudflare", "api_token": marked("token")}}, change: map[string]any{"name": "changed-by-tests"}},
		{name: "iscsi.portal", with: map[string]any{"listen": []any{map[string]any{"ip": "0.0.0.0"}}, "comment": "made by the tests"}, change: map[string]any{"comment": "changed by the tests"}},
		{name: "iscsi.initiator", with: map[string]any{"comment": "made by the tests"}, change: map[string]any{"comment": "changed by the tests"}},
		{name: "iscsi.auth", with: map[string]any{"tag": 7, "user": "made-by-tests", "secret": chap()}, change: map[string]any{"user": "changed-by-tests"}},
		{name: "iscsi.extent", with: map[string]any{"name": "made-by-tests", "type": "FILE", "path": "/mnt/" + layout.Datasets[0] + "/extent-made-by-tests", "filesize": 16 << 20}, change: map[string]any{"comment": "changed by the tests"}, removal: []any{true, true}},
		{name: "nvmet.subsys", with: map[string]any{"name": "made-by-tests"}, change: map[string]any{"allow_any_host": true}},
		{name: "nvmet.host", with: map[string]any{"hostnqn": "nqn.2014-08.org.nvmexpress:uuid:11111111-2222-3333-4444-555555555555"}, change: map[string]any{"dhchap_hash": "SHA-384"}},
		{name: "nvmet.port", with: map[string]any{"addr_trtype": "TCP", "addr_traddr": "0.0.0.0", "addr_trsvcid": 4421}, change: map[string]any{"enabled": false}},
	} {
		t.Run(kind.name, func(t *testing.T) {
			ctx := within(t, 4*time.Minute)

			var answer json.RawMessage
			if err := conn.Call(ctx, kind.name+".create", []any{kind.with}, &answer); err != nil {
				t.Fatalf("making it: %v", err)
			}

			// most answer what was made, and a few only its number
			var made struct {
				ID  json.RawMessage `json:"id"`
				Key string          `json:"key"`
			}
			if json.Unmarshal(answer, &made) != nil || made.ID == nil {
				made.ID = answer
			}
			if made.Key != "" {
				ledger.secret(made.Key)
			}

			removed := false
			remove := func(ctx context.Context) error {
				return conn.Call(ctx, kind.name+".delete", append([]any{made.ID}, kind.removal...), nil)
			}
			t.Cleanup(func() {
				if !removed {
					if err := remove(context.WithoutCancel(ctx)); err != nil {
						t.Errorf("removing it: %v", err)
					}
				}
			})

			var fetched map[string]json.RawMessage
			asked(ctx, t, conn, kind.name+".get_instance", []any{made.ID}, &fetched)

			var listed []json.RawMessage
			if err := conn.Call(ctx, kind.name+".query", []any{[]any{[]any{"id", "=", made.ID}}}, &listed); err != nil || len(listed) != 1 {
				t.Errorf("asked for by its id, %d are listed (%v)", len(listed), err)
			}

			if err := conn.Call(ctx, kind.name+".update", []any{made.ID, kind.change}, nil); err != nil {
				t.Fatalf("changing it: %v", err)
			}
			asked(ctx, t, conn, kind.name+".get_instance", []any{made.ID}, &fetched)
			for name, value := range kind.change {
				if !same(fetched[name], value) {
					t.Errorf("changed, its %s reads back as %s, want %v", name, fetched[name], value)
				}
			}

			if err := remove(ctx); err != nil {
				t.Fatalf("removing it: %v", err)
			}
			removed = true

			if err := conn.Call(ctx, kind.name+".get_instance", []any{made.ID}, nil); !truenasrpc.IsNotFound(err) {
				t.Errorf("removed, asking for it is answered: %v", err)
			}
		})
	}
}

// A NAS has one page of settings for each of its services and for itself. One setting on each is changed, read back changed, and put back
// as it was. They are the settings that do nothing a test could be hurt by: a description, a count, a time zone.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestOneSettingOnEachPageIsChangedReadBackAndPutBack(t *testing.T) {
	conn := admin(t)

	for _, page := range []struct {
		name    string
		setting string
		to      any
	}{
		{"ftp", "clients", 7},
		{"ssh", "compression", true},
		{"smb", "description", "changed by the tests"},
		{"nfs", "allow_nonroot", true},
		{"snmp", "location", "a test bench"},
		{"ups", "description", "changed by the tests"},
		{"mail", "fromname", "The test NAS"},
		{"system.general", "timezone", "Europe/London"},
		{"system.advanced", "motd", "changed by the tests"},
		{"network.configuration", "domain", "tests.example"},
		{"iscsi.global", "pool_avail_threshold", 75},
		{"nvmet.global", "xport_referral", false},
		{"replication.config", "max_parallel_replication_tasks", 3},
		{"reporting", "tier1_days", 400},
		{"auth.twofactor", "window", 2},
		{"audit", "retention", 14},
		{"update", "autocheck", false},
		{"system.security", "min_password_length", 12},
	} {
		t.Run(page.name, func(t *testing.T) {
			ctx := within(t, 4*time.Minute)

			var before map[string]json.RawMessage
			asked(ctx, t, conn, page.name+".config", nil, &before)
			was, has := before[page.setting]
			if !has {
				t.Fatalf("the page has no %s", page.setting)
			}
			if same(was, page.to) {
				t.Fatalf("its %s is %s already, so changing it to that would show nothing", page.setting, was)
			}

			if err := conn.Call(ctx, page.name+".update", []any{map[string]any{page.setting: page.to}}, nil); err != nil {
				t.Fatalf("changing its %s: %v", page.setting, err)
			}
			t.Cleanup(func() {
				if err := conn.Call(context.WithoutCancel(ctx), page.name+".update", []any{map[string]any{page.setting: was}}, nil); err != nil {
					t.Errorf("putting its %s back: %v", page.setting, err)
				}
			})

			var after map[string]json.RawMessage
			asked(ctx, t, conn, page.name+".config", nil, &after)
			if !same(after[page.setting], page.to) {
				t.Errorf("changed, its %s reads back as %s, want %v", page.setting, after[page.setting], page.to)
			}
		})
	}
}
