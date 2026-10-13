// Package setup takes a TrueNAS fresh out of its installer to the one acceptance/layout describes, a step at a time, through the SDK. Making a NAS
// from nothing is the hardest thing asked of the SDK anywhere, so the steps are not hidden away in a script: the live tests walk them one by one and
// read the NAS back after each, and the test environment runs the same steps to hand a ready NAS to anything else that wants one.
//
// Every step makes sure of something and can be run again: it looks first, and makes only what is not there.
package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// readOnlyGroup is the group TrueNAS keeps for users who may read everything and change nothing.
const readOnlyGroup = "truenas_readonly_administrators"

// NAS is a TrueNAS being set up, through a connection logged in as its administrator.
type NAS struct {
	Conn *truenasrpc.Conn
	// Logf is told what is being done, and may be nil
	Logf func(format string, args ...any)
}

// Made is what a test needs to log into a NAS that has been set up, beside the administrator's password.
type Made struct {
	Version     string
	Key         string
	ReadOnlyKey string
}

// Call is one call a step makes, written down as what it is so that a test can hold every one of them to the NAS's own description of its API.
type Call struct {
	Method string
	Params []any
	// Job says the NAS runs the method as a job, which is started and waited for
	Job bool
}

func SystemInfo() Call { return Call{Method: "system.info"} }

func NetworkConfig() Call { return Call{Method: "network.configuration.config"} }

func Pools(filters ...any) Call {
	return Call{Method: "pool.query", Params: []any{append([]any{}, filters...)}}
}

func SetHostname() Call {
	return Call{Method: "network.configuration.update", Params: []any{map[string]any{"hostname": layout.Hostname}}}
}

func UnusedDisks() Call {
	return Call{Method: "disk.details", Params: []any{map[string]any{"type": "UNUSED"}}}
}

func CreatePool(disks []string) Call {
	return Call{Method: "pool.create", Job: true, Params: []any{map[string]any{"name": layout.Pool, "topology": map[string]any{"data": []any{map[string]any{"type": "MIRROR", "disks": disks}}}}}}
}

func FindDatasets(names []string) Call {
	return Call{Method: "pool.dataset.query", Params: []any{[]any{[]any{"id", "in", names}}}}
}

func CreateDataset(name string) Call {
	return Call{Method: "pool.dataset.create", Params: []any{map[string]any{"name": name}}}
}

func FindUser(username string) Call {
	return Call{Method: "user.query", Params: []any{[]any{[]any{"username", "=", username}}}}
}

func FindGroup(name string) Call {
	return Call{Method: "group.query", Params: []any{[]any{[]any{"group", "=", name}}}}
}

func CreateUser(username string, group int, password string) Call {
	return Call{Method: "user.create", Params: []any{map[string]any{"username": username, "full_name": "May read and not change", "group_create": true, "groups": []int{group}, "password": password, "smb": false}}}
}

func CreateKey(name, username string) Call {
	return Call{Method: "api_key.create", Params: []any{map[string]any{"name": name, "username": username}}}
}

func (n *NAS) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

// run makes a call the way the NAS runs it: at once, or as a job that is started, followed and waited for.
func (n *NAS) run(ctx context.Context, c Call, into any, follow func(truenasrpc.JobStatus)) error {
	if !c.Job {
		return n.Conn.Call(ctx, c.Method, c.Params, into)
	}

	job, err := n.Conn.Start(ctx, c.Method, c.Params)
	if err != nil {
		return err
	}

	ended := make(chan struct{})
	job.OnUpdate(func(s truenasrpc.JobStatus) {
		n.logf("  %s: %s %.0f%% %s", c.Method, s.State, s.Percent, s.Description)
		if follow != nil {
			follow(s)
		}
		if s.State.Finished() {
			close(ended)
		}
	})

	if err := job.Wait(ctx, into); err != nil {
		return err
	}

	// whoever follows a job is told of its end a moment after the wait for it is over, so the step is not done until they have been
	select {
	case <-ended:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: its end was never told to whoever followed it: %w", c.Method, context.Cause(ctx))
	}
}

// Disposable is nil for a NAS that may be set up and tested against: one already named as a test NAS is, or one with no pool at all, which is
// what a NAS is straight out of its installer. Any other NAS is somebody's, and is refused before anything is asked of it but what it has.
func (n *NAS) Disposable(ctx context.Context) error {
	// the name it is set to have, which is the one a setup gives: the name it answers to at this moment can lag a change by a little
	var info struct {
		Hostname string `json:"hostname"`
	}
	if err := n.run(ctx, NetworkConfig(), &info, nil); err != nil {
		return err
	}
	if info.Hostname == layout.Hostname {
		return nil
	}

	var pools []json.RawMessage
	if err := n.run(ctx, Pools(), &pools, nil); err != nil {
		return err
	}
	if len(pools) > 0 {
		return fmt.Errorf("the TrueNAS at %s is named %q and has %d pools: a test NAS is one named %q, or one fresh out of its installer with no pool, and this is neither", n.Conn.Address(), info.Hostname, len(pools), layout.Hostname)
	}

	return nil
}

// Name gives the NAS the name a test NAS goes by, and answers the version it runs.
func (n *NAS) Name(ctx context.Context) (version string, err error) {
	var info struct {
		Version string `json:"version"`
	}
	if err := n.run(ctx, SystemInfo(), &info, nil); err != nil {
		return "", err
	}

	var config struct {
		Hostname string `json:"hostname"`
	}
	if err := n.run(ctx, NetworkConfig(), &config, nil); err != nil {
		return "", err
	}
	if config.Hostname == layout.Hostname {
		return info.Version, nil
	}

	n.logf("naming TrueNAS %s %s", info.Version, layout.Hostname)

	return info.Version, n.run(ctx, SetHostname(), nil, nil)
}

// Pool makes the pool, of the two disks layout says, unless the NAS has it. made says it was made now, and follow is told how the making went.
func (n *NAS) Pool(ctx context.Context, follow func(truenasrpc.JobStatus)) (made bool, err error) {
	var pools []json.RawMessage
	if err := n.run(ctx, Pools([]any{"name", "=", layout.Pool}), &pools, nil); err != nil {
		return false, err
	}
	if len(pools) > 0 {
		return false, nil
	}

	disks, err := n.mirrorDisks(ctx)
	if err != nil {
		return false, err
	}
	n.logf("making the pool %s of %s", layout.Pool, strings.Join(disks, " and "))

	return true, n.run(ctx, CreatePool(disks), nil, follow)
}

// Datasets makes whichever of the pool's datasets the NAS has not got, and answers those it made.
func (n *NAS) Datasets(ctx context.Context) (made []string, err error) {
	var have []struct {
		ID string `json:"id"`
	}
	if err := n.run(ctx, FindDatasets(layout.Datasets), &have, nil); err != nil {
		return nil, err
	}

	for _, name := range layout.Datasets {
		if slices.ContainsFunc(have, func(d struct {
			ID string `json:"id"`
		},
		) bool {
			return d.ID == name
		}) {
			continue
		}

		if err := n.run(ctx, CreateDataset(name), nil, nil); err != nil {
			return made, err
		}
		made = append(made, name)
	}

	return made, nil
}

// ReadOnlyUser makes the user who may read everything and change nothing, unless the NAS has one. made says it was made now.
func (n *NAS) ReadOnlyUser(ctx context.Context) (made bool, err error) {
	var users []json.RawMessage
	if err := n.run(ctx, FindUser(layout.ReadOnlyUser), &users, nil); err != nil {
		return false, err
	}
	if len(users) > 0 {
		return false, nil
	}

	var groups []struct {
		ID int `json:"id"`
	}
	if err := n.run(ctx, FindGroup(readOnlyGroup), &groups, nil); err != nil {
		return false, err
	}
	if len(groups) != 1 {
		return false, fmt.Errorf("the TrueNAS has %d groups named %s, and a user who may only read is made a member of the one", len(groups), readOnlyGroup)
	}

	// the user logs in with a key and never with this, so nobody is told it
	password := make([]byte, 16)
	if _, err := rand.Read(password); err != nil {
		return false, err
	}

	return true, n.run(ctx, CreateUser(layout.ReadOnlyUser, groups[0].ID, hex.EncodeToString(password)), nil, nil)
}

// Key makes an API key for a user and answers it. A key can be read only when it is made, so each asking makes another, named apart from the
// last by the time: a NAS that is set up twice has the keys of both, and both work.
func (n *NAS) Key(ctx context.Context, username string) (string, error) {
	var key struct {
		Key string `json:"key"`
	}
	if err := n.run(ctx, CreateKey("truenas-mcp-test-"+username+"-"+strconv.FormatInt(time.Now().UnixNano(), 36), username), &key, nil); err != nil {
		return "", err
	}
	if key.Key == "" {
		return "", errors.New("the TrueNAS made a key and did not say what it is")
	}

	return key.Key, nil
}

// All takes every step in order, and answers what a test needs to log into the NAS that results.
func (n *NAS) All(ctx context.Context) (*Made, error) {
	if err := n.Disposable(ctx); err != nil {
		return nil, err
	}

	version, err := n.Name(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := n.Pool(ctx, nil); err != nil {
		return nil, err
	}
	if _, err := n.Datasets(ctx); err != nil {
		return nil, err
	}
	if _, err := n.ReadOnlyUser(ctx); err != nil {
		return nil, err
	}

	made := &Made{Version: version}
	if made.Key, err = n.Key(ctx, layout.AdminUser); err != nil {
		return nil, err
	}
	if made.ReadOnlyKey, err = n.Key(ctx, layout.ReadOnlyUser); err != nil {
		return nil, err
	}

	return made, nil
}

// Disk is a disk as the NAS lists one.
type Disk struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
}

// Unused is the disks nothing on the NAS uses.
func (n *NAS) Unused(ctx context.Context) ([]Disk, error) {
	var raw json.RawMessage
	if err := n.run(ctx, UnusedDisks(), &raw, nil); err != nil {
		return nil, err
	}

	// asked for the unused disks alone a NAS answers a list, and asked for both kinds, each under its own name: either is read
	var unused []Disk
	if json.Unmarshal(raw, &unused) == nil {
		return unused, nil
	}

	var both struct {
		Unused []Disk `json:"unused"`
	}
	if err := json.Unmarshal(raw, &both); err != nil {
		return nil, fmt.Errorf("disk.details: the TrueNAS answered something that is not a list of disks: %w", err)
	}

	return both.Unused, nil
}

// mirrorDisks is the names of the two disks the pool is made of, found by their serials among the disks nothing uses. A NAS that has just
// started may not have looked at its disks yet, so it is asked again for a while.
func (n *NAS) mirrorDisks(ctx context.Context) ([]string, error) {
	wanted := []string{layout.DiskSerial + "1", layout.DiskSerial + "2"}

	for try := 0; ; try++ {
		unused, err := n.Unused(ctx)
		if err != nil {
			return nil, err
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
		n.logf("waiting for the TrueNAS to find its disks: it lists %s", strings.Join(found, ", "))

		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-time.After(5 * time.Second):
		}
	}
}
