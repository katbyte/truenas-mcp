package tools

import (
	"encoding/json"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// A list of disks says which pool each is in and how warm it is, and leaves the warmth out for a disk that does not say.
func TestDiskListSaysWhichPoolEachDiskIsInAndHowWarmItIs(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("disk.query", json.RawMessage(`[{"identifier": "{serial}A1", "name": "sda", "serial": "A1", "model": "WDC WD80", "type": "HDD", "size": 8000, "rotationrate": 7200, "bus": "ATA", "pool": "tank"}, {"identifier": "{serial}B2", "name": "sdb", "serial": "B2", "type": "SSD", "size": 500, "bus": "ATA", "pool": null}]`))
	nas.Answer("disk.temperatures", json.RawMessage(`{"sda": 41.0, "sdb": null}`))

	out := session(t, nas, Options{}).Call(t, "disk_list", nil)
	rows := acctest.Rows(t, out["disks"], "disks")
	if len(rows) != 2 || acctest.Num(t, out["total"], "total") != 2 {
		t.Fatalf("listed %v", out)
	}
	if a := rows[0]; a["serial"] != "A1" || a["pool"] != "tank" || acctest.Decimal(t, a["temperature_celsius"], "temperature_celsius") != 41 || acctest.Num(t, a["rpm"], "rpm") != 7200 {
		t.Errorf("sda reads as %v", a)
	}
	if b := rows[1]; b["pool"] != nil || b["temperature_celsius"] != nil || b["type"] != "SSD" {
		t.Errorf("sdb reads as %v", b)
	}
}
