package truenasrpc_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/katbyte/pandorest/client"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	nasclient "github.com/katbyte/truenas-mcp/sdk/truenas/client"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// the connection is everything the generated client asks of one: calls, and files both ways
var _ interface {
	client.Caller
	client.Uploader
	client.Downloader
} = (*truenasrpc.Conn)(nil)

// The client generated from the NAS's description calls through the connection: an answer reads into its types, a moment as a NAS writes one
// included, a refusal reads as pandorest's own kind of error, and a one-row query that matches nothing says nothing is there.
func TestTheGeneratedClientCallsThroughTheConnection(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", json.RawMessage(`{"version": "25.10.7", "hostname": "stand-in", "physmem": 8589934592, "cores": 4, "boottime": {"$date": 1790938328000}, "system_serial": null, "ecc_memory": true}`))
	nas.Refuse("system.reboot", &truenastest.Error{Errno: 13, ErrName: "EACCES", Reason: "Not authorized"})
	nas.Refuse("pool.query", truenastest.ErrNoMatch)

	typed, err := nasclient.New(connect(t, nas))
	if err != nil {
		t.Fatal(err)
	}

	info, err := typed.System.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a := info.Answer; a.Version != "25.10.7" || a.Hostname != "stand-in" || a.Physmem != 8589934592 || a.Cores != 4 || !a.EccMemory || !a.Boottime.Equal(time.UnixMilli(1790938328000)) {
		t.Errorf("system.info read through the typed client as %+v", a)
	}

	_, err = typed.SystemReboot.Info(t.Context())
	if !truenasrpc.IsNoSuchMethod(err) {
		t.Errorf("a method the stand-in has no answer for = %v", err)
	}
	_, err = typed.System.Reboot(t.Context(), "tests", truenas.SystemRebootOptions{})
	var refused *client.CallError
	if !errors.As(err, &refused) || refused.Method != "system.reboot" || refused.ErrName != "EACCES" || refused.Reason != "Not authorized" {
		t.Errorf("a refusal through the typed client = %v, read as %+v", err, refused)
	}

	if _, err = typed.Pool.QueryOne(t.Context(), []any{[]any{"name", "=", "nothing"}}, truenas.PoolQueryOptions{}); !truenasrpc.IsNotFound(err) {
		t.Errorf("a one-row query that matches nothing = %v", err)
	}
}
