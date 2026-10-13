//go:build acceptance

package truenasrpc_test

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/acceptance/setup"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// A NAS comes out of its installer with an administrator and nothing else. Taking it from there to one with a name, a pool, datasets, users and
// keys is the most the SDK is asked to do in one go: a password login, a job that is followed while it runs, a handful of writes, and a second
// user whose rights have to hold. Each step is taken through the SDK and the NAS is read back after it, so a step that claims to have worked and
// did not is caught where it happened. The tests after this one use what it made.
//
// The file is named so that it runs first. Every step makes only what is missing, so it also passes on a NAS that is already set up.
//
//nolint:paralleltest // the tests share one NAS, and this one makes what the rest use
func TestANASIsSetUpFromNothingAndReadBackAfterEveryStep(t *testing.T) {
	conn := root(t)
	n := &setup.NAS{Conn: conn, Logf: t.Logf}
	ctx := within(t, 20*time.Minute)
	out := &setup.Made{}

	// each step needs the one before it, so the journey stops at the first that fails
	const stopped = "the journey stops here: every step after this one needs it"

	if !t.Run("it is given the name a test NAS goes by", func(t *testing.T) {
		version, err := n.Name(ctx)
		if err != nil || version == "" {
			t.Fatalf("naming it: version %q, %v", version, err)
		}
		out.Version = version

		var config struct {
			Hostname string `json:"hostname"`
		}
		if err := conn.Call(ctx, "network.configuration.config", nil, &config); err != nil || config.Hostname != layout.Hostname {
			t.Fatalf("read back, the NAS is named %q (%v), want %q", config.Hostname, err, layout.Hostname)
		}
	}) {
		t.Fatal(stopped)
	}

	if !t.Run("its pool is made of two disks, and the making is followed", func(t *testing.T) {
		var mu sync.Mutex
		var told []truenasrpc.JobStatus
		made, err := n.Pool(ctx, func(s truenasrpc.JobStatus) {
			mu.Lock()
			defer mu.Unlock()

			told = append(told, s)
		})
		if err != nil {
			t.Fatalf("making the pool: %v", err)
		}

		mu.Lock()
		followed := slices.Clone(told)
		mu.Unlock()

		// on a NAS that had the pool already there was nothing to follow
		if made && (len(followed) == 0 || followed[len(followed)-1].State != truenasrpc.JobSuccess || followed[len(followed)-1].Method != "pool.create") {
			t.Errorf("whoever followed the pool being made was told %+v, want its end last", followed)
		}

		var pools []struct {
			Name     string `json:"name"`
			Status   string `json:"status"`
			Healthy  bool   `json:"healthy"`
			Topology struct {
				Data []struct {
					Type     string            `json:"type"`
					Children []json.RawMessage `json:"children"`
				} `json:"data"`
			} `json:"topology"`
		}
		if err := conn.Call(ctx, "pool.query", nil, &pools); err != nil {
			t.Fatal(err)
		}
		if len(pools) != 1 || pools[0].Name != layout.Pool || pools[0].Status != "ONLINE" || !pools[0].Healthy {
			t.Fatalf("read back, the NAS has %+v, want the one pool %s, online and healthy", pools, layout.Pool)
		}
		if data := pools[0].Topology.Data; len(data) != 1 || data[0].Type != "MIRROR" || len(data[0].Children) != 2 {
			t.Errorf("read back, the pool is laid out as %+v, want one mirror of two disks", data)
		}

		// the two disks left over are still nobody's, for a test to make a pool of its own from
		unused, err := n.Unused(ctx)
		if err != nil {
			t.Fatal(err)
		}
		spare := 0
		for _, d := range unused {
			if d.Serial == layout.DiskSerial+"3" || d.Serial == layout.DiskSerial+"4" {
				spare++
			}
		}
		if spare != 2 {
			t.Errorf("read back, the unused disks are %+v, want the two the pool was not made of among them", unused)
		}
	}) {
		t.Fatal(stopped)
	}

	if !t.Run("its datasets are made", func(t *testing.T) {
		if _, err := n.Datasets(ctx); err != nil {
			t.Fatalf("making the datasets: %v", err)
		}

		var datasets []struct {
			ID         string `json:"id"`
			Mountpoint string `json:"mountpoint"`
		}
		if err := conn.Call(ctx, "pool.dataset.query", []any{[]any{[]any{"id", "in", layout.Datasets}}}, &datasets); err != nil {
			t.Fatal(err)
		}
		if len(datasets) != len(layout.Datasets) {
			t.Fatalf("read back, of the datasets %v the NAS has %+v", layout.Datasets, datasets)
		}
		for _, d := range datasets {
			if d.Mountpoint != "/mnt/"+d.ID {
				t.Errorf("read back, %s is mounted at %q, want /mnt/%s", d.ID, d.Mountpoint, d.ID)
			}
		}
	}) {
		t.Fatal(stopped)
	}

	if !t.Run("a user who may only read is made, whose key reads and is refused a write", func(t *testing.T) {
		if _, err := n.ReadOnlyUser(ctx); err != nil {
			t.Fatalf("making the user: %v", err)
		}

		key, err := n.Key(ctx, layout.ReadOnlyUser)
		if err != nil {
			t.Fatalf("making the user's key: %v", err)
		}
		out.ReadOnlyKey = key

		only := as(t, truenasrpc.APIKey(layout.ReadOnlyUser, key))
		if err := only.Call(ctx, "pool.query", nil, nil); err != nil {
			t.Errorf("the user who may only read could not read the pools: %v", err)
		}
		if err := only.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": layout.Pool + "/not-allowed"}}, nil); !truenasrpc.IsNotAllowed(err) {
			t.Errorf("the user who may only read was answered %v for a write, want not allowed", err)
		}

		var missing []json.RawMessage
		if err := conn.Call(ctx, "pool.dataset.query", []any{[]any{[]any{"id", "=", layout.Pool + "/not-allowed"}}}, &missing); err != nil || len(missing) != 0 {
			t.Errorf("read back, the write that was refused left %d datasets behind (%v)", len(missing), err)
		}
	}) {
		t.Fatal(stopped)
	}

	if !t.Run("the administrator is given a key, which logs in without the password", func(t *testing.T) {
		key, err := n.Key(ctx, layout.AdminUser)
		if err != nil {
			t.Fatalf("making the key: %v", err)
		}
		out.Key = key

		var who struct {
			Name string `json:"pw_name"`
		}
		if err := as(t, truenasrpc.APIKey(layout.AdminUser, key)).Call(ctx, "auth.me", nil, &who); err != nil || who.Name != layout.AdminUser {
			t.Errorf("logged in with the new key, the NAS says the user is %q (%v), want %s", who.Name, err, layout.AdminUser)
		}

		var keys []struct {
			Name string `json:"name"`
		}
		if err := conn.Call(ctx, "api_key.query", nil, &keys); err != nil || !slices.ContainsFunc(keys, func(k struct {
			Name string `json:"name"`
		},
		) bool {
			return strings.HasPrefix(k.Name, "truenas-mcp-test-"+layout.AdminUser)
		}) {
			t.Errorf("read back, the NAS's keys are %+v (%v), want the one just made among them", keys, err)
		}
	}) {
		t.Fatal(stopped)
	}

	if !t.Run("set up a second time, nothing is made again", func(t *testing.T) {
		pool, err := n.Pool(ctx, nil)
		if err != nil || pool {
			t.Errorf("the pool was made again: %v, %v", pool, err)
		}
		if datasets, err := n.Datasets(ctx); err != nil || len(datasets) != 0 {
			t.Errorf("datasets were made again: %v, %v", datasets, err)
		}
		if user, err := n.ReadOnlyUser(ctx); err != nil || user {
			t.Errorf("the user was made again: %v, %v", user, err)
		}
	}) {
		t.Fatal(stopped)
	}

	held.mu.Lock()
	held.made = out
	held.mu.Unlock()
	ledger.secret(out.Key, out.ReadOnlyKey)
}
