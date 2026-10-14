package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// standIn is a NAS that answers system.info and system.state as the test NAS does.
func standIn(t *testing.T, state string) *truenastest.NAS {
	t.Helper()

	nas := truenastest.New(t)
	nas.Answer("system.state", state)
	nas.Answer("system.info", json.RawMessage(`{"version": "25.10.7", "buildtime": {"$date": 1790000000000}, "hostname": "truenas-mcp-test", "physmem": 8589934592, "model": "QEMU Virtual CPU version 2.5+", "cores": 4, "physical_cores": 2, "loadavg": [0.1, 0.2, 0.3], "uptime": "1 day", "uptime_seconds": 90061.5, "system_serial": "serial-1", "system_product": "Standard PC (Q35 + ICH9, 2009)", "system_product_version": "pc-q35-11.0", "license": null, "boottime": {"$date": 1790938328000}, "datetime": {"$date": 1791028389500}, "timezone": "America/Los_Angeles", "system_manufacturer": "QEMU", "ecc_memory": true}`))

	return nas
}

// server_info says what the NAS is, in its own words: its version and state, its name and hardware, when it started, and who the key belongs to.
func TestServerInfoSaysWhatTheNASIsAndWhoTheKeyBelongsTo(t *testing.T) {
	t.Parallel()

	nas := standIn(t, "READY")
	out := session(t, nas, Options{}).Call(t, "server_info", nil)

	want := map[string]any{"address": "wss://" + strings.TrimPrefix(nas.Address(), "https://") + "/api/current", "version": "25.10.7", "state": "READY", "hostname": "truenas-mcp-test", "user": nas.Username(), "cpu": "QEMU Virtual CPU version 2.5+", "manufacturer": "QEMU", "product": "Standard PC (Q35 + ICH9, 2009)", "serial": "serial-1", "booted": "2026-10-02T10:52:08Z", "timezone": "America/Los_Angeles"}
	for field, value := range want {
		if out[field] != value {
			t.Errorf("%s = %v, want %v", field, out[field], value)
		}
	}
	if acctest.Num(t, out["cores"], "cores") != 4 || acctest.Num(t, out["physical_cores"], "physical_cores") != 2 || acctest.Decimal(t, out["memory_bytes"], "memory_bytes") != 8589934592 || acctest.Num(t, out["uptime_seconds"], "uptime_seconds") != 90061 || !acctest.BoolOf(out["ecc_memory"]) {
		t.Errorf("the hardware and uptime read as %v", out)
	}
	if roles := acctest.Strs(t, out["roles"], "roles"); len(roles) != 1 || roles[0] != fullAdmin {
		t.Errorf("roles = %v", roles)
	}
	if out["truenas_mcp_version"] == "" || out["note"] != nil {
		t.Errorf("the build is %v and the note %v", out["truenas_mcp_version"], out["note"])
	}
}

// A user who may only read is told so when the tools that change something are registered: the NAS refuses every one of them. A NAS writes the roles
// as a set, which reads as a list.
func TestServerInfoWarnsThatAKeyThatMayOnlyReadCannotChangeAnything(t *testing.T) {
	t.Parallel()

	nas := standIn(t, "READY")
	nas.Answer("auth.me", json.RawMessage(`{"pw_name": "watcher", "privilege": {"roles": {"$set": ["READONLY_ADMIN", "SHARING_READ"]}}}`))

	reading := session(t, nas, Options{}).Call(t, "server_info", nil)
	if roles := acctest.Strs(t, reading["roles"], "roles"); len(roles) != 2 || roles[0] != readOnlyAdmin || roles[1] != "SHARING_READ" {
		t.Errorf("roles = %v", roles)
	}
	if reading["note"] != nil {
		t.Errorf("a session that only reads, on a key that only reads, is warned: %v", reading["note"])
	}

	writing := session(t, nas, Options{EnableWrite: true}).Call(t, "server_info", nil)
	if note := acctest.Str(writing["note"]); !strings.Contains(note, "may only read") {
		t.Errorf("a session that may change things, on a key that may only read, is told %q", note)
	}
}

// A NAS that is starting or stopping answers, and what it says is not to be relied on until it is ready.
func TestServerInfoSaysANASThatIsNotReadyIsNot(t *testing.T) {
	t.Parallel()

	out := session(t, standIn(t, "BOOTING"), Options{}).Call(t, "server_info", nil)
	if note := acctest.Str(out["note"]); out["state"] != "BOOTING" || !strings.Contains(note, "until it is READY") {
		t.Errorf("a NAS that is starting reads as %v, with the note %q", out["state"], note)
	}
}

// What the NAS refuses is an error that says what was asked, and a NAS that is not there is one too.
func TestServerInfoFailsSayingWhatTheNASWouldNotAnswer(t *testing.T) {
	t.Parallel()

	nas := standIn(t, "READY")
	nas.Refuse("system.info", &truenastest.Error{Errno: 13, ErrName: "EACCES", Reason: "Not authorized"})
	if msg := session(t, nas, Options{}).CallErr(t, "server_info", nil); !strings.Contains(msg, "would not describe itself") || !strings.Contains(msg, "Not authorized") {
		t.Errorf("a NAS that will not describe itself: %q", msg)
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	if _, err := RegisterAll(srv, unreachable(t), Options{}); err != nil {
		t.Fatal(err)
	}
	if msg := connect(t, srv).CallErr(t, "server_info", nil); !strings.Contains(msg, "could not be asked") || !strings.Contains(msg, "127.0.0.1:1") {
		t.Errorf("a NAS that is not there: %q", msg)
	}
}
