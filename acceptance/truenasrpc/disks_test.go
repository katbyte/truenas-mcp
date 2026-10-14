//go:build acceptance

package truenasrpc_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
)

// A disk that can say how hot it is says so, and says what is too hot for it. The test NAS has one such disk, of another kind than the rest,
// so that there is a temperature to read: the others answer nothing, which is what most disks in a virtual machine do and what a check of
// temperatures has to take as "not known" and not as cold. It is asked with the key that may only read, as whatever watches a NAS asks.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestADiskSaysHowHotItIsAndWhatIsTooHotForIt(t *testing.T) {
	conn := reader(t)
	ctx := within(t, 8*time.Minute)

	var disks []struct {
		Name   string `json:"name"`
		Serial string `json:"serial"`
		Bus    string `json:"bus"`
		Type   string `json:"type"`
	}
	if err := conn.Call(ctx, "disk.query", nil, &disks); err != nil {
		t.Fatalf("listing the disks: %v", err)
	}
	var hot string
	names := make([]string, 0, len(disks))
	for _, disk := range disks {
		names = append(names, disk.Name)
		if disk.Serial == layout.HotDiskSerial {
			hot = disk.Name
			t.Logf("the disk that says how hot it is: %+v", disk)
		}
	}
	if hot == "" {
		t.Fatalf("the NAS has no disk with the serial %s among %+v", layout.HotDiskSerial, disks)
	}

	// a NAS reads its disks' temperatures every so often, so the first answers after it starts may have none
	var now map[string]*float64
	if !eventually(ctx, 4*time.Minute, func() bool {
		asked(ctx, t, conn, "disk.temperatures", nil, &now)

		return now[hot] != nil
	}) {
		t.Fatalf("the disk that can say how hot it is never did: %v", now)
	}
	if *now[hot] < 10 || *now[hot] > 90 {
		t.Errorf("%s is said to be at %v degrees", hot, *now[hot])
	}
	for name, at := range now {
		if name != hot && at != nil {
			t.Logf("%s says it is at %v degrees too", name, *at)
		}
	}

	// asked with what is too hot for each disk, the answer is the two numbers together
	var with map[string]json.RawMessage
	asked(ctx, t, conn, "disk.temperatures", []any{[]string{hot}, true}, &with)
	var pair []*float64
	if err := json.Unmarshal(with[hot], &pair); err != nil || len(pair) != 2 || pair[0] == nil {
		t.Fatalf("asked with what is too hot for it, the disk answers %s (%v); want how hot it is and what is too hot", with[hot], err)
	}
	t.Logf("%s is at %v degrees", hot, *pair[0])
	if pair[1] == nil || *pair[1] <= *pair[0] {
		t.Errorf("what is too hot for %s is said to be %v, with the disk at %v", hot, pair[1], *pair[0])
	}

	// nothing is too hot, so the NAS has nothing to say of it, and what it kept of the last days reads as described
	var alerts []json.RawMessage
	asked(ctx, t, conn, "disk.temperature_alerts", []any{names}, &alerts)
	if len(alerts) != 0 {
		t.Errorf("the NAS says %d disks are too hot: %s", len(alerts), alerts)
	}
	asked(ctx, t, conn, "disk.temperature_agg", []any{names, 7}, nil)
}
