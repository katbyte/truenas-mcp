package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// A service is started, reloaded, restarted and stopped only where it needs to be, each time told not to be silent so that the NAS says why it
// would not; whether it starts with the NAS is set apart from that, and a service there is none of says how they are named.
func TestAServiceIsStartedAndStoppedOnlyWhereItNeedsToBe(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	var mu sync.Mutex
	ssh := map[string]any{"id": 9, "service": "ssh", "state": "STOPPED", "enable": false}
	nas.Handle("service.query", func(params []json.RawMessage) (any, error) {
		if !strings.Contains(string(params[0]), `"ssh"`) && !strings.Contains(string(params[0]), `9]`) {
			return nil, truenastest.ErrNoMatch
		}
		mu.Lock()
		defer mu.Unlock()

		return ssh, nil
	})
	nas.Job("service.control", func(params []json.RawMessage) (any, error) {
		var verb string
		_ = json.Unmarshal(params[0], &verb)

		mu.Lock()
		defer mu.Unlock()
		switch verb {
		case "START", "RESTART":
			ssh["state"] = "RUNNING"
		case "STOP":
			ssh["state"] = "STOPPED"
		}

		return true, nil
	})
	nas.Handle("service.update", func(params []json.RawMessage) (any, error) {
		var data struct {
			Enable bool `json:"enable"`
		}
		_ = json.Unmarshal(params[1], &data)

		mu.Lock()
		defer mu.Unlock()
		ssh["enable"] = data.Enable

		// a NAS answers an update to a service with its number
		return 9, nil
	})
	suite := session(t, nas, everything)

	if reloaded := suite.Call(t, "service_reload", map[string]any{"service": "ssh"}); !strings.Contains(acctest.Str(reloaded["note"]), "ssh is stopped") {
		t.Errorf("reloading a stopped service: %v", reloaded)
	}
	started := suite.Call(t, "service_start", map[string]any{"service": "ssh"})
	if row := acctest.Object(t, started["service"], "service"); row["state"] != "RUNNING" || !slices.Equal(acctest.Strs(t, started["changes"], "changes"), []string{"started ssh"}) {
		t.Errorf("started %v", started)
	}
	if again := suite.Call(t, "service_start", map[string]any{"service": "9"}); len(acctest.Strs(t, again["changes"], "changes")) != 0 {
		t.Errorf("starting a running service, by number: %v", again)
	}
	suite.Call(t, "service_reload", map[string]any{"service": "ssh"})
	suite.Call(t, "service_restart", map[string]any{"service": "ssh"})
	if dry := suite.Call(t, "service_stop", map[string]any{"service": "ssh", "dry_run": true}); !slices.Equal(acctest.Strs(t, dry["changes"], "changes"), []string{"would stop ssh"}) {
		t.Errorf("a dry run of stopping: %v", dry)
	}
	stopped := suite.Call(t, "service_stop", map[string]any{"service": "ssh"})
	if row := acctest.Object(t, stopped["service"], "service"); row["state"] != "STOPPED" {
		t.Errorf("stopped %v", stopped)
	}
	controls := nas.Calls("service.control")
	verbs := make([]string, 0, len(controls))
	for _, c := range controls {
		verbs = append(verbs, string(c.Params[0]))
		if !strings.Contains(string(c.Params[2]), `"silent":false`) {
			t.Errorf("a service was controlled with %s", c.Params[2])
		}
	}
	if !slices.Equal(verbs, []string{`"START"`, `"RELOAD"`, `"RESTART"`, `"STOP"`}) {
		t.Errorf("the NAS was asked to %v", verbs)
	}

	enabled := suite.Call(t, "service_update", map[string]any{"service": "ssh", "enabled": true})
	if row := acctest.Object(t, enabled["service"], "service"); !acctest.BoolOf(row["enabled"]) || row["state"] != "STOPPED" {
		t.Errorf("set to start with the NAS: %v", enabled)
	}
	if msg := suite.CallErr(t, "service_start", map[string]any{"service": "smb"}); !strings.Contains(msg, "cifs is SMB") {
		t.Errorf("a service there is none of: %q", msg)
	}
}

// An alert is dismissed and brought back by its id, each only where it needs to be, and read back after.
func TestAnAlertIsDismissedAndBroughtBack(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	var mu sync.Mutex
	dismissed := false
	nas.Handle("alert.list", func([]json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()

		return []map[string]any{{"uuid": "a1", "level": "WARNING", "klass": "ScrubPaused", "formatted": "the scrub of tank is paused", "dismissed": dismissed, "datetime": map[string]any{"$date": 1790000000000}, "last_occurrence": map[string]any{"$date": 1790000000000}}}, nil
	})
	setDismissed := func(to bool) truenastest.Handler {
		return func([]json.RawMessage) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			dismissed = to

			return nil, nil
		}
	}
	nas.Handle("alert.dismiss", setDismissed(true))
	nas.Handle("alert.restore", setDismissed(false))
	suite := session(t, nas, everything)

	if restored := suite.Call(t, "alert_restore", map[string]any{"id": "a1"}); !strings.Contains(acctest.Str(restored["note"]), "already not dismissed") || len(nas.Calls("alert.restore")) != 0 {
		t.Errorf("bringing back an alert never dismissed: %v", restored)
	}
	gone := suite.Call(t, "alert_dismiss", map[string]any{"id": "a1"})
	if row := acctest.Object(t, gone["alert"], "alert"); !acctest.BoolOf(row["dismissed"]) || !slices.Equal(acctest.Strs(t, gone["changes"], "changes"), []string{"dismissed the alert ScrubPaused"}) {
		t.Errorf("dismissed %v", gone)
	}
	back := suite.Call(t, "alert_restore", map[string]any{"id": "a1"})
	if row := acctest.Object(t, back["alert"], "alert"); acctest.BoolOf(row["dismissed"]) {
		t.Errorf("brought back %v", back)
	}
	if msg := suite.CallErr(t, "alert_dismiss", map[string]any{"id": "nope"}); !strings.Contains(msg, `no alert "nope"`) {
		t.Errorf("an alert there is none of: %q", msg)
	}
}
