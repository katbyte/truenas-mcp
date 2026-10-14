//go:build acceptance

package tools_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/tools"
)

// server_info says which NAS this is as the NAS says it: the version setting it up read, the name it was given, a machine that has been up since it
// last started, and the administrator the key belongs to, who may do everything.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestServerInfoSaysWhichNASThisIsAndThatItsKeyMayDoEverything(t *testing.T) {
	out := suite.Call(t, "server_info", nil)

	if out["version"] != nas.made.Version || out["state"] != "READY" || out["hostname"] != layout.Hostname || out["user"] != nas.username {
		t.Errorf("the NAS reads as version %v, %v, named %v, with the key of %v", out["version"], out["state"], out["hostname"], out["user"])
	}
	if roles := acctest.Strs(t, out["roles"], "roles"); !slices.Contains(roles, "FULL_ADMIN") {
		t.Errorf("the administrator's roles = %v", roles)
	}
	if acctest.NumOr0(out["cores"]) < 1 || acctest.DecimalOr0(out["memory_bytes"]) < 1<<30 || acctest.Str(out["cpu"]) == "" || acctest.Str(out["timezone"]) == "" {
		t.Errorf("the hardware reads as %v", out)
	}

	booted, err := time.Parse(time.RFC3339, acctest.Str(out["booted"]))
	uptime := time.Duration(acctest.NumOr0(out["uptime_seconds"])) * time.Second
	if err != nil || booted.After(time.Now()) || uptime <= 0 || time.Until(booted.Add(uptime)).Abs() > 5*time.Minute {
		t.Errorf("the NAS started at %v (%v) and has been up %v, which do not add up to now", out["booted"], err, uptime)
	}
	if acctest.Str(out["truenas_mcp_version"]) == "" {
		t.Errorf("this server's build reads as %v", out["truenas_mcp_version"])
	}
}

// The user who may only read is told it may only read, and warned when it is given the tools that change something, which the NAS will refuse.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestServerInfoTellsAKeyThatMayOnlyReadThatItMayOnlyRead(t *testing.T) {
	reading := asReader(t, tools.Options{}).Call(t, "server_info", nil)
	if roles := acctest.Strs(t, reading["roles"], "roles"); reading["user"] != layout.ReadOnlyUser || !slices.Contains(roles, "READONLY_ADMIN") || slices.Contains(roles, "FULL_ADMIN") {
		t.Errorf("the user who may only read reads as %v with the roles %v", reading["user"], roles)
	}
	if reading["note"] != nil {
		t.Errorf("reading only, the user who may only read is warned: %v", reading["note"])
	}

	writing := asReader(t, tools.Options{EnableWrite: true}).Call(t, "server_info", nil)
	if note := acctest.Str(writing["note"]); !strings.Contains(note, "may only read") {
		t.Errorf("given the tools that change something, the user who may only read is told %q", note)
	}
}
