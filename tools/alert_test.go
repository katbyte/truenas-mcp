package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// standInWithAlerts is a NAS with four alerts: a warning, a critical one, a notice someone dismissed, and an informational one.
func standInWithAlerts(t *testing.T) *truenastest.NAS {
	t.Helper()

	nas := truenastest.New(t)
	nas.Answer("alert.list", json.RawMessage(`[
		{"uuid": "w", "level": "WARNING", "klass": "TaskLocked", "formatted": "A task is on a locked dataset", "datetime": {"$date": 1790845202000}, "dismissed": false},
		{"uuid": "c", "level": "CRITICAL", "klass": "VolumeStatus", "formatted": "Pool backup state is DEGRADED", "datetime": {"$date": 1790845202000}, "last_occurrence": {"$date": 1790848802000}, "dismissed": false},
		{"uuid": "n", "level": "NOTICE", "klass": "ScrubFinished", "formatted": "Scrub finished", "datetime": {"$date": 1790845202000}, "dismissed": true},
		{"uuid": "i", "level": "INFO", "klass": "Update", "formatted": "An update is available", "datetime": {"$date": 1790845202000}, "dismissed": false}
	]`))

	return nas
}

// Alerts are listed the gravest first, with the dismissed ones left out unless asked for, and can be held to a level and graver.
func TestAlertListPutsTheGravestFirstAndLeavesOutWhatWasDismissed(t *testing.T) {
	t.Parallel()

	s := session(t, standInWithAlerts(t), Options{})
	ids := func(out map[string]any) string {
		rows := acctest.Rows(t, out["alerts"], "alerts")
		got := make([]string, 0, len(rows))
		for _, row := range rows {
			got = append(got, acctest.Str(row["id"]))
		}

		return strings.Join(got, ",")
	}

	all := s.Call(t, "alert_list", nil)
	if got := ids(all); got != "c,w,i" {
		t.Errorf("listed %s, want the critical one, the warning and the information, the dismissed notice left out", got)
	}
	if first := acctest.Rows(t, all["alerts"], "alerts")[0]; first["class"] != "VolumeStatus" || first["text"] != "Pool backup state is DEGRADED" || first["last_seen"] != "2026-10-01T10:00:02Z" {
		t.Errorf("the critical alert reads as %v", first)
	}
	if got := ids(s.Call(t, "alert_list", map[string]any{"dismissed": true})); got != "c,w,n,i" {
		t.Errorf("with the dismissed ones, listed %s", got)
	}
	if got := ids(s.Call(t, "alert_list", map[string]any{"at_least": "warning"})); got != "c,w" {
		t.Errorf("at warning and graver, listed %s", got)
	}
	if msg := s.CallErr(t, "alert_list", map[string]any{"at_least": "loud"}); !strings.Contains(msg, `"loud" is no level`) || !strings.Contains(msg, "WARNING") {
		t.Errorf("a level there is none of: %q", msg)
	}
}
