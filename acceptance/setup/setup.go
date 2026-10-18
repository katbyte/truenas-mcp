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

	"github.com/katbyte/go-kt/pointer"
	pandorest "github.com/katbyte/pandorest/client"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenas/client"
	"github.com/katbyte/truenas-mcp/sdk/truenas/pool"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// readOnlyGroup is the group TrueNAS keeps for users who may read everything and change nothing.
const readOnlyGroup = "truenas_readonly_administrators"

// NAS is a TrueNAS being set up, through a connection logged in as its administrator.
type NAS struct {
	Conn *truenasrpc.Conn
	// Logf is told what is being done, and may be nil
	Logf func(format string, args ...any)
	// typed is the client over Conn, made when the first step is taken
	typed *client.Client
}

// Made is what a test needs to log into a NAS that has been set up, beside the administrator's password.
type Made struct {
	Version     string
	Key         string
	ReadOnlyKey string
}

func (n *NAS) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

// api is the client generated from the NAS's description, over the administrator's connection: every step is made through it, so making a NAS
// is a test of the typed client as much as of the connection.
func (n *NAS) api() (*client.Client, error) {
	if n.typed == nil {
		typed, err := client.New(n.Conn)
		if err != nil {
			return nil, err
		}
		n.typed = typed
	}

	return n.typed, nil
}

// follow starts a job and waits for it, telling follow how it goes, for the one step whose progress a test watches.
func (n *NAS) follow(ctx context.Context, method string, params []any, follow func(truenasrpc.JobStatus)) error {
	job, err := n.Conn.Start(ctx, method, params)
	if err != nil {
		return err
	}

	ended := make(chan struct{})
	job.OnUpdate(func(s truenasrpc.JobStatus) {
		n.logf("  %s: %s %.0f%% %s", method, s.State, s.Percent, s.Description)
		if follow != nil {
			follow(s)
		}
		if s.State.Finished() {
			close(ended)
		}
	})

	if err := job.Wait(ctx, nil); err != nil {
		return err
	}

	// whoever follows a job is told of its end a moment after the wait for it is over, so the step is not done until they have been
	select {
	case <-ended:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: its end was never told to whoever followed it: %w", method, context.Cause(ctx))
	}
}

// Disposable is nil for a NAS that may be set up and tested against: one already named as a test NAS is, or one with no pool at all, which is
// what a NAS is straight out of its installer. Any other NAS is somebody's, and is refused before anything is asked of it but what it has.
func (n *NAS) Disposable(ctx context.Context) error {
	api, err := n.api()
	if err != nil {
		return err
	}

	// the name it is set to have, which is the one a setup gives: the name it answers to at this moment can lag a change by a little
	config, err := api.NetworkConfiguration.Config(ctx)
	if err != nil {
		return err
	}
	hostname := ""
	if config.Answer != nil {
		hostname = config.Answer.Hostname
	}
	if hostname == layout.Hostname {
		return nil
	}

	pools, err := api.Pool.Query(ctx, nil, truenas.PoolQueryOptions{})
	if err != nil {
		return err
	}
	if len(pools.Answer) > 0 {
		return fmt.Errorf("the TrueNAS at %s is named %q and has %d pools: a test NAS is one named %q, or one fresh out of its installer with no pool, and this is neither", n.Conn.Address(), hostname, len(pools.Answer), layout.Hostname)
	}

	return nil
}

// Name gives the NAS the name a test NAS goes by, and answers the version it runs.
func (n *NAS) Name(ctx context.Context) (version string, err error) {
	api, err := n.api()
	if err != nil {
		return "", err
	}
	info, err := api.System.Info(ctx)
	if err != nil {
		return "", err
	}
	if info.Answer == nil {
		return "", errors.New("system.info: the TrueNAS answered nothing")
	}
	version = info.Answer.Version

	config, err := api.NetworkConfiguration.Config(ctx)
	if err != nil {
		return "", err
	}
	if config.Answer != nil && config.Answer.Hostname == layout.Hostname {
		return version, nil
	}

	n.logf("naming TrueNAS %s %s", version, layout.Hostname)
	_, err = api.NetworkConfiguration.Update(ctx, truenas.NetworkConfigurationUpdateData{Hostname: layout.Hostname})

	return version, err
}

// Pool makes the pool, of the two disks layout says, unless the NAS has it. made says it was made now, and follow is told how the making went.
func (n *NAS) Pool(ctx context.Context, follow func(truenasrpc.JobStatus)) (made bool, err error) {
	api, err := n.api()
	if err != nil {
		return false, err
	}
	pools, err := api.Pool.Query(ctx, []any{[]any{"name", "=", layout.Pool}}, truenas.PoolQueryOptions{})
	if err != nil {
		return false, err
	}
	if len(pools.Answer) > 0 {
		return false, nil
	}

	disks, err := n.mirrorDisks(ctx)
	if err != nil {
		return false, err
	}
	n.logf("making the pool %s of %s", layout.Pool, strings.Join(disks, " and "))

	mirror, err := pandorest.UnionOf(truenas.PoolCreateTopologyDataVdevNonDRAID{Type: truenas.PoolCreateTopologyDataVdevNonDRAIDTypeMIRROR, Disks: disks})
	if err != nil {
		return false, err
	}
	method, params := pool.CreateCall(truenas.PoolCreateData{Name: layout.Pool, Topology: &truenas.DataTopology{Data: []truenas.DataTopologyData{{Union: mirror}}}})

	return true, n.follow(ctx, method, params, follow)
}

// Datasets makes whichever of the pool's datasets the NAS has not got, and answers those it made.
func (n *NAS) Datasets(ctx context.Context) (made []string, err error) {
	return n.datasetsOf(ctx, layout.Datasets)
}

// datasetsOf makes whichever of these datasets the NAS has not got, and answers those it made.
func (n *NAS) datasetsOf(ctx context.Context, names []string) (made []string, err error) {
	api, err := n.api()
	if err != nil {
		return nil, err
	}
	have, err := api.PoolDataset.Query(ctx, []any{[]any{"id", "in", names}}, truenas.PoolDatasetQueryOptions{})
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		if slices.ContainsFunc(have.Answer, func(d truenas.PoolDatasetQueryResultItem) bool { return d.Id == name }) {
			continue
		}

		dataset, err := pandorest.UnionOf(truenas.PoolDatasetCreateFilesystem{Name: name})
		if err != nil {
			return made, err
		}
		if _, err := api.PoolDataset.Create(ctx, truenas.PoolDatasetCreateData{Union: dataset}); err != nil {
			return made, err
		}
		made = append(made, name)
	}

	return made, nil
}

// ReadOnlyUser makes the user who may read everything and change nothing, unless the NAS has one. made says it was made now.
func (n *NAS) ReadOnlyUser(ctx context.Context) (made bool, err error) {
	api, err := n.api()
	if err != nil {
		return false, err
	}
	users, err := api.User.Query(ctx, []any{[]any{"username", "=", layout.ReadOnlyUser}}, truenas.UserQueryOptions{})
	if err != nil {
		return false, err
	}
	if len(users.Answer) > 0 {
		return false, nil
	}

	groups, err := api.Group.Query(ctx, []any{[]any{"group", "=", readOnlyGroup}}, truenas.GroupQueryOptions{})
	if err != nil {
		return false, err
	}
	if len(groups.Answer) != 1 {
		return false, fmt.Errorf("the TrueNAS has %d groups named %s, and a user who may only read is made a member of the one", len(groups.Answer), readOnlyGroup)
	}

	// the user logs in with a key and never with this, so nobody is told it
	password, err := secret()
	if err != nil {
		return false, err
	}
	_, err = api.User.Create(ctx, truenas.UserCreate{Username: layout.ReadOnlyUser, FullName: "May read and not change", GroupCreate: pointer.To(true), Groups: []int{groups.Answer[0].Id}, Password: password, Smb: pointer.To(false)})

	return true, err
}

// Key makes an API key for a user and answers it. A key can be read only when it is made, so each asking makes another, named apart from the
// last by the time: a NAS that is set up twice has the keys of both, and both work.
func (n *NAS) Key(ctx context.Context, username string) (string, error) {
	api, err := n.api()
	if err != nil {
		return "", err
	}
	key, err := api.ApiKey.Create(ctx, truenas.ApiKeyCreate{Name: "truenas-mcp-test-" + username + "-" + strconv.FormatInt(time.Now().UnixNano(), 36), Username: username})
	if err != nil {
		return "", err
	}
	if key.Answer == nil || key.Answer.Key == "" {
		return "", errors.New("the TrueNAS made a key and did not say what it is")
	}

	return key.Answer.Key, nil
}

// secret is a password nobody chose and nobody is told, for a user who never logs in with one.
func secret() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	return "A1-" + hex.EncodeToString(raw), nil
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
	api, err := n.api()
	if err != nil {
		return nil, err
	}
	details, err := api.Disk.Details(ctx, truenas.DiskDetailsData{Type: truenas.DiskDetailsDataTypeUNUSED})
	if err != nil {
		return nil, err
	}
	raw := details.Answer

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
