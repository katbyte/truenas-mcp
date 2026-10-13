package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// readOnlyGroup is the group TrueNAS keeps for users who may read everything and change nothing.
const readOnlyGroup = "truenas_readonly_administrators"

// setupCall is one call the setup makes, written down as what it is so that a test can hold every one of them to the NAS's own description of
// its API: this program is first run for real on a machine nobody is watching.
type setupCall struct {
	Method string
	Params []any
	// Job says the NAS runs the method as a job, which is started and waited for
	Job bool
}

func callSystemInfo() setupCall { return setupCall{Method: "system.info"} }
func callPools() setupCall      { return setupCall{Method: "pool.query"} }

func callSetHostname() setupCall {
	return setupCall{Method: "network.configuration.update", Params: []any{map[string]any{"hostname": layout.Hostname}}}
}

func callUnusedDisks() setupCall {
	return setupCall{Method: "disk.details", Params: []any{map[string]any{"type": "UNUSED"}}}
}

func callCreatePool(disks []string) setupCall {
	return setupCall{Method: "pool.create", Job: true, Params: []any{map[string]any{"name": layout.Pool, "topology": map[string]any{"data": []any{map[string]any{"type": "MIRROR", "disks": disks}}}}}}
}

func callCreateDataset(name string) setupCall {
	return setupCall{Method: "pool.dataset.create", Params: []any{map[string]any{"name": name}}}
}

func callCreateKey(name, username string) setupCall {
	return setupCall{Method: "api_key.create", Params: []any{map[string]any{"name": name, "username": username}}}
}

func callFindGroup(name string) setupCall {
	return setupCall{Method: "group.query", Params: []any{[]any{[]any{"group", "=", name}}}}
}

func callCreateUser(username string, group int, password string) setupCall {
	return setupCall{Method: "user.create", Params: []any{map[string]any{"username": username, "full_name": "May read and not change", "group_create": true, "groups": []int{group}, "password": password, "smb": false}}}
}

// made is what setting a test NAS up comes to: what a test needs to log into it.
type made struct {
	Version     string
	Key         string
	ReadOnlyKey string
}

// provision sets a NAS fresh from its installer up as a test NAS: its name, its pool and datasets, a key for its administrator, and a second
// user who may only read, with a key of their own. It refuses a NAS that has a pool already, since the only NAS it is ever meant for has none.
func provision(ctx context.Context, conn *truenasrpc.Conn, logf func(string, ...any)) (*made, error) {
	run := func(c setupCall, into any) error {
		if !c.Job {
			return conn.Call(ctx, c.Method, c.Params, into)
		}

		job, err := conn.Start(ctx, c.Method, c.Params)
		if err != nil {
			return err
		}
		job.OnUpdate(func(s truenasrpc.JobStatus) { logf("  %s: %s %.0f%% %s", c.Method, s.State, s.Percent, s.Description) })

		return job.Wait(ctx, into)
	}

	var info struct {
		Version  string `json:"version"`
		Hostname string `json:"hostname"`
	}
	if err := run(callSystemInfo(), &info); err != nil {
		return nil, err
	}

	var pools []struct {
		Name string `json:"name"`
	}
	if err := run(callPools(), &pools); err != nil {
		return nil, err
	}
	if len(pools) > 0 {
		return nil, fmt.Errorf("the TrueNAS at %s (%s) already has %d pools: a test NAS is set up from one fresh out of its installer, and this is not one", conn.Address(), info.Hostname, len(pools))
	}
	logf("setting up TrueNAS %s as %s", info.Version, layout.Hostname)

	if err := run(callSetHostname(), nil); err != nil {
		return nil, err
	}

	disks, err := mirrorDisks(ctx, run, logf)
	if err != nil {
		return nil, err
	}
	if err := run(callCreatePool(disks), nil); err != nil {
		return nil, err
	}
	for _, dataset := range layout.Datasets {
		if err := run(callCreateDataset(dataset), nil); err != nil {
			return nil, err
		}
	}

	out := &made{Version: info.Version}

	var key struct {
		Key string `json:"key"`
	}
	if err := run(callCreateKey("truenas-mcp-test", layout.AdminUser), &key); err != nil {
		return nil, err
	}
	out.Key = key.Key

	var groups []struct {
		ID int `json:"id"`
	}
	if err := run(callFindGroup(readOnlyGroup), &groups); err != nil {
		return nil, err
	}
	if len(groups) != 1 {
		return nil, fmt.Errorf("the TrueNAS has %d groups named %s, and a user who may only read is made a member of the one", len(groups), readOnlyGroup)
	}

	password := make([]byte, 16)
	if _, err := rand.Read(password); err != nil {
		return nil, err
	}
	if err := run(callCreateUser(layout.ReadOnlyUser, groups[0].ID, hex.EncodeToString(password)), nil); err != nil {
		return nil, err
	}
	if err := run(callCreateKey("truenas-mcp-test-readonly", layout.ReadOnlyUser), &key); err != nil {
		return nil, err
	}
	out.ReadOnlyKey = key.Key

	if out.Key == "" || out.ReadOnlyKey == "" {
		return nil, errors.New("the TrueNAS made a key and did not say what it is")
	}

	return out, nil
}

// mirrorDisks is the names of the two disks the pool is made of, found by their serials among the disks nothing uses. A NAS that has just
// started may not have looked at its disks yet, so it is asked again for a while.
func mirrorDisks(ctx context.Context, run func(setupCall, any) error, logf func(string, ...any)) ([]string, error) {
	wanted := []string{layout.DiskSerial + "1", layout.DiskSerial + "2"}

	for try := 0; ; try++ {
		var raw json.RawMessage
		if err := run(callUnusedDisks(), &raw); err != nil {
			return nil, err
		}

		// asked for the unused disks alone a NAS answers a list, and asked for both kinds, each under its own name: either is read
		type disk struct {
			Name   string `json:"name"`
			Serial string `json:"serial"`
		}
		var unused []disk
		if json.Unmarshal(raw, &unused) != nil {
			var both struct {
				Unused []disk `json:"unused"`
			}
			if err := json.Unmarshal(raw, &both); err != nil {
				return nil, fmt.Errorf("disk.details: the TrueNAS answered something that is not a list of disks: %w", err)
			}
			unused = both.Unused
		}

		names, found := make([]string, 0, len(wanted)), make([]string, 0, len(unused))
		for _, d := range unused {
			found = append(found, d.Name+" ("+d.Serial+")")
			if slices.Contains(wanted, d.Serial) {
				names = append(names, d.Name)
			}
		}
		if len(names) == len(wanted) {
			slices.Sort(names)

			return names, nil
		}

		if try >= 24 {
			return nil, fmt.Errorf("the TrueNAS has no unused disks with the serials %s: it lists %s", strings.Join(wanted, " and "), strings.Join(found, ", "))
		}
		logf("waiting for the TrueNAS to find its disks: it lists %s", strings.Join(found, ", "))

		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-time.After(5 * time.Second):
		}
	}
}

// certificateOf is the certificate a NAS shows, as a PEM file holds one. It is taken on sight, with nothing to check it against, which is right
// for a NAS that was made a minute ago by the program asking and for no other.
func certificateOf(ctx context.Context, server string) ([]byte, error) {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s is not the address of a NAS: give it as https://host:port", server)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}

	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} //nolint:gosec // G402: see above; the certificate is what is being fetched
	raw, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = raw.Close() }()

	conn, ok := raw.(*tls.Conn)
	if !ok || len(conn.ConnectionState().PeerCertificates) == 0 {
		return nil, fmt.Errorf("the NAS at %s showed no certificate", server)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: conn.ConnectionState().PeerCertificates[0].Raw}), nil
}
