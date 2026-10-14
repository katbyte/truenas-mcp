package setup_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/acceptance/setup"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// holding is what a stand-in NAS holds, which changes as the setup makes things on it.
type holding struct {
	mu       sync.Mutex
	hostname string
	pools    []string
	datasets []string
	users    []string
	keys     int
	// kept is what the NAS holds of each kind of thing a furnished NAS has, by the start of the methods that list and make it
	kept map[string][]map[string]any
}

// standIn is a stand-in NAS that keeps what it is asked to make and answers what it has, as far as the setup asks.
func standIn(t *testing.T, has *holding) *truenastest.NAS {
	t.Helper()

	nas := truenastest.New(t)
	rows := func(names []string, field string) []any {
		out := make([]any, 0, len(names))
		for _, name := range names {
			out = append(out, map[string]any{field: name})
		}

		return out
	}
	named := func(params []json.RawMessage, field string) string {
		var given map[string]json.RawMessage
		_ = json.Unmarshal(params[0], &given)

		var name string
		_ = json.Unmarshal(given[field], &name)

		return name
	}

	nas.Handle("system.info", func([]json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		return map[string]any{"version": "25.10.7", "hostname": has.hostname}, nil
	})
	nas.Handle("network.configuration.config", func([]json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		return map[string]any{"hostname": has.hostname}, nil
	})
	nas.Handle("network.configuration.update", func(params []json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		has.hostname = named(params, "hostname")

		return map[string]any{"hostname": has.hostname}, nil
	})
	nas.Handle("pool.query", func([]json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		return rows(has.pools, "name"), nil
	})
	nas.Answer("disk.details", []any{map[string]any{"name": "vde", "serial": layout.DiskSerial + "4"}, map[string]any{"name": "vdc", "serial": layout.DiskSerial + "2"}, map[string]any{"name": "vdb", "serial": layout.DiskSerial + "1"}, map[string]any{"name": "vdd", "serial": layout.DiskSerial + "3"}})
	nas.LongJob("pool.create", func(run *truenastest.Run, params []json.RawMessage) (any, error) {
		run.Progress(50, "Creating pool")

		has.mu.Lock()
		defer has.mu.Unlock()

		has.pools = append(has.pools, named(params, "name"))

		return map[string]any{"name": named(params, "name")}, nil
	})
	nas.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		return rows(has.datasets, "id"), nil
	})
	nas.Handle("pool.dataset.create", func(params []json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		has.datasets = append(has.datasets, named(params, "name"))

		return map[string]any{}, nil
	})
	nas.Handle("user.query", func(params []json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		// asked for by name, as the setup asks
		var filters [][]json.RawMessage
		if len(params) > 0 && json.Unmarshal(params[0], &filters) == nil && len(filters) == 1 {
			var wanted string
			_ = json.Unmarshal(filters[0][2], &wanted)

			if !slices.Contains(has.users, wanted) {
				return []any{}, nil
			}

			return rows([]string{wanted}, "username"), nil
		}

		return rows(has.users, "username"), nil
	})
	nas.Answer("group.query", []any{map[string]any{"id": 44}})
	nas.Handle("user.create", func(params []json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		has.users = append(has.users, named(params, "username"))

		return 71, nil
	})
	nas.Handle("api_key.create", func(params []json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		has.keys++

		return map[string]any{"key": "5-aKeyMadeFor" + named(params, "username")}, nil
	})

	// each kind of thing a furnished NAS has is listed, by whatever it was asked for by, and made, with a number of its own. A task says it has
	// never run until it is
	for _, kind := range []string{"sharing.smb", "sharing.nfs", "iscsi.target", "vm", "pool.snapshottask", "replication", "rsynctask"} {
		nas.Handle(kind+".query", func(params []json.RawMessage) (any, error) {
			has.mu.Lock()
			defer has.mu.Unlock()

			var filters [][]json.RawMessage
			if len(params) > 0 {
				_ = json.Unmarshal(params[0], &filters)
			}

			out := []any{}
			for _, row := range has.kept[kind] {
				fits := true
				for _, filter := range filters {
					var field string
					_ = json.Unmarshal(filter[0], &field)
					had, _ := json.Marshal(row[field])
					fits = fits && string(had) == string(filter[2])
				}
				if fits {
					out = append(out, row)
				}
			}

			return out, nil
		})
		nas.Handle(kind+".create", func(params []json.RawMessage) (any, error) {
			has.mu.Lock()
			defer has.mu.Unlock()

			var row map[string]any
			_ = json.Unmarshal(params[0], &row)
			row["id"], row["state"], row["job"] = len(has.kept[kind])+1, map[string]any{"state": "PENDING"}, nil

			if has.kept == nil {
				has.kept = map[string][]map[string]any{}
			}
			has.kept[kind] = append(has.kept[kind], row)

			return row, nil
		})
	}
	nas.Handle("pool.snapshottask.run", func([]json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		for _, task := range has.kept["pool.snapshottask"] {
			task["state"] = map[string]any{"state": "FINISHED"}
		}

		return nil, nil
	})
	nas.Job("rsynctask.run", func([]json.RawMessage) (any, error) {
		has.mu.Lock()
		defer has.mu.Unlock()

		for _, task := range has.kept["rsynctask"] {
			task["job"] = map[string]any{"state": "FAILED"}
		}

		return nil, &truenastest.Error{Errno: 14, ErrName: "EFAULT", Reason: "rsync command returned 10 - SOCKETIO. Check logs for further information."}
	})

	return nas
}

func administrator(t *testing.T, nas *truenastest.NAS) *setup.NAS {
	t.Helper()

	conn, err := truenasrpc.New(nas.Address(), truenasrpc.Password(nas.Username(), nas.Password()), truenasrpc.WithRootCAs(nas.Roots()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &setup.NAS{Conn: conn, Logf: t.Logf}
}

// The live tests count on finding the same NAS every time: its name, its pool of two particular disks, its datasets, and two keys, one of them
// for a user who may only read. This is the setup run against a stand-in, so that what it asks for is known before a real NAS is asked; and run a
// second time, so that a NAS which is already set up is left as it is.
func TestANASFreshOutOfItsInstallerIsSetUpAndSettingItUpAgainChangesNothing(t *testing.T) {
	t.Parallel()

	has := &holding{hostname: "truenas"}
	nas := standIn(t, has)
	n := administrator(t, nas)

	made, err := n.All(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if made.Key != "5-aKeyMadeFor"+layout.AdminUser || made.ReadOnlyKey != "5-aKeyMadeFor"+layout.ReadOnlyUser || made.Version != "25.10.7" {
		t.Errorf("the setup came to %+v, want a key for each of the two users", made)
	}
	if has.hostname != layout.Hostname || !slices.Equal(has.pools, []string{layout.Pool}) || !slices.Equal(has.datasets, layout.Datasets) || !slices.Equal(has.users, []string{layout.ReadOnlyUser}) {
		t.Errorf("the NAS now holds %+v, want what the layout says", has)
	}

	for method, want := range map[string]string{
		"pool.create": `"data":[{"disks":["vdb","vdc"],"type":"MIRROR"}]`,
		"user.create": `"groups":[44]`,
	} {
		calls := nas.Calls(method)
		if len(calls) != 1 || !strings.Contains(string(calls[0].Params[0]), want) {
			t.Errorf("%s was called with %+v, want once with %s", method, calls, want)
		}
	}

	if _, err := n.All(t.Context()); err != nil {
		t.Fatal(err)
	}
	if again := nas.Calls("network.configuration.update", "pool.create", "pool.dataset.create", "user.create"); len(again) != 1+1+len(layout.Datasets)+1 {
		t.Errorf("setting a NAS up a second time made things again: %d calls that make something, want those of the first time only", len(again))
	}
	if has.keys != 4 {
		t.Errorf("%d keys were made over two setups, want two each time, since a key can only be read when it is made", has.keys)
	}
}

// The making of the pool is a job, and whoever asks is told how it goes: that is what a live test watches to know the SDK follows a real job.
func TestTheMakingOfThePoolIsFollowed(t *testing.T) {
	t.Parallel()

	n := administrator(t, standIn(t, &holding{hostname: "truenas"}))

	var mu sync.Mutex
	var told []truenasrpc.JobStatus
	made, err := n.Pool(t.Context(), func(s truenasrpc.JobStatus) {
		mu.Lock()
		defer mu.Unlock()

		told = append(told, s)
	})
	if err != nil || !made {
		t.Fatalf("the pool: made %v, %v", made, err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(told) == 0 || told[len(told)-1].Method != "pool.create" {
		t.Errorf("whoever followed the pool being made was told %+v", told)
	}
}

// The setup makes a pool and a user and renames the NAS. Pointed by mistake at a NAS that is in use, it must change nothing: a NAS with a pool
// and another name is somebody's, and is refused before anything is asked of it but what it has.
func TestANASThatIsSomebodysIsLeftAlone(t *testing.T) {
	t.Parallel()

	nas := standIn(t, &holding{hostname: "somebodys-nas", pools: []string{"somebodys-pool"}})
	n := administrator(t, nas)

	if _, err := n.All(t.Context()); err == nil || !strings.Contains(err.Error(), "has 1 pools") {
		t.Fatalf("a NAS with a pool and another name was answered %v, want it refused", err)
	}
	if changed := nas.Calls("network.configuration.update", "pool.create", "pool.dataset.create", "user.create", "api_key.create"); len(changed) != 0 {
		t.Errorf("somebody's NAS was changed: %+v", changed)
	}

	// one that is a test NAS already, pool and all, is taken
	ours := administrator(t, standIn(t, &holding{hostname: layout.Hostname, pools: []string{layout.Pool}}))
	if err := ours.Disposable(t.Context()); err != nil {
		t.Errorf("a NAS named as a test NAS was refused: %v", err)
	}
}

// A furnished NAS has one of each thing a NAS is watched for, with a task in each state a task can be left in. Furnished from a NAS that
// was only set up, each is made once: a share of each kind on a dataset of its own, a target, a machine, a snapshot task that is then run, a
// replication that is not, and a copy task whose one run fails. Furnished again, nothing is made or run a second time.
func TestANASIsFurnishedWithOneOfEachThingAndFurnishingItAgainChangesNothing(t *testing.T) {
	t.Parallel()

	has := &holding{hostname: "truenas"}
	nas := standIn(t, has)
	n := administrator(t, nas)

	if _, err := n.All(t.Context()); err != nil {
		t.Fatal(err)
	}

	making := []string{"sharing.smb.create", "sharing.nfs.create", "iscsi.target.create", "vm.create", "pool.snapshottask.create", "replication.create", "rsynctask.create", "pool.snapshottask.run", "rsynctask.run"}
	for range 2 {
		if err := n.Furnish(t.Context()); err != nil {
			t.Fatal(err)
		}

		for _, method := range making {
			if calls := nas.Calls(method); len(calls) != 1 {
				t.Errorf("%s was called %d times, want once however often the NAS is furnished", method, len(calls))
			}
		}
		if users := nas.Calls("user.create"); len(users) != 2 {
			t.Errorf("%d users were made, want the one who may only read and the one who may use SMB", len(users))
		}
	}

	if !slices.Contains(has.datasets, layout.SMBDataset) || !slices.Contains(has.datasets, layout.NFSDataset) || !slices.Contains(has.users, layout.SMBUser) {
		t.Errorf("the NAS holds the datasets %v and the users %v, want a dataset for each share and the user who may use SMB", has.datasets, has.users)
	}
	if smb := has.kept["sharing.smb"]; len(smb) != 1 || smb[0]["name"] != layout.SMBShare || smb[0]["path"] != "/mnt/"+layout.SMBDataset {
		t.Errorf("the SMB share is %+v", smb)
	}
	if tasks := has.kept["pool.snapshottask"]; len(tasks) != 1 || tasks[0]["dataset"] != layout.TaskDataset {
		t.Errorf("the snapshot task is %+v", tasks)
	}

	// a secret is never the same twice, and never one a person chose
	created := nas.Calls("user.create")
	if string(created[0].Params[0]) == string(created[1].Params[0]) || strings.Contains(string(created[1].Params[0]), `"password":""`) {
		t.Error("the user who may use SMB was made with no password, or the same one as another user")
	}
}

// The setup is first run for real on a machine nobody is watching, against an API whose argument names are easy to get one letter wrong. Every
// call it makes is held to the NAS's own description: that the method is there, that it is a job where the setup waits for one, and that no
// argument is given a name the method does not take or left without one it needs.
func TestEveryCallTheSetupMakesIsOneTheNASDescribes(t *testing.T) {
	t.Parallel()

	documents, err := filepath.Glob("../../api-defs/truenas-methods-*.json")
	if err != nil || len(documents) == 0 {
		t.Fatalf("no description of the API under api-defs (%v)", err)
	}

	calls := []setup.Call{
		setup.SystemInfo(), setup.NetworkConfig(), setup.Pools(), setup.Pools([]any{"name", "=", layout.Pool}), setup.SetHostname(), setup.UnusedDisks(), setup.CreatePool([]string{"vdb", "vdc"}),
		setup.FindDatasets(layout.Datasets), setup.CreateDataset(layout.Datasets[0]), setup.FindUser(layout.ReadOnlyUser), setup.FindGroup("a-group"),
		setup.CreateUser(layout.ReadOnlyUser, 44, "a-password"), setup.CreateKey("a-key", layout.AdminUser),
		setup.CreateSMBUser("a-password"), setup.FindSMBShare(), setup.CreateSMBShare(), setup.FindNFSShare(), setup.CreateNFSShare(), setup.FindTarget(), setup.CreateTarget(),
		setup.FindMachine(), setup.CreateMachine(), setup.FindSnapshotTask(), setup.CreateSnapshotTask(), setup.RunSnapshotTask(1), setup.FindReplication(), setup.CreateReplication(),
		setup.FindCopyTask(), setup.CreateCopyTask(), setup.RunCopyTask(1),
	}

	for _, document := range documents {
		raw, err := os.ReadFile(document) //nolint:gosec // G304: the files are the repository's own, found by the glob above
		if err != nil {
			t.Fatal(err)
		}

		var methods map[string]struct {
			Job     bool              `json:"job"`
			Accepts []json.RawMessage `json:"accepts"`
		}
		if err := json.Unmarshal(raw, &methods); err != nil {
			t.Fatalf("%s: %v", document, err)
		}

		for _, call := range calls {
			described, ok := methods[call.Method]
			if !ok {
				t.Errorf("%s: the NAS has no method %s", document, call.Method)

				continue
			}
			if described.Job != call.Job {
				t.Errorf("%s: %s is a job: %v, and the setup treats it as one: %v", document, call.Method, described.Job, call.Job)
			}
			if len(call.Params) > len(described.Accepts) {
				t.Errorf("%s: %s is given %d arguments and takes %d", document, call.Method, len(call.Params), len(described.Accepts))

				continue
			}

			for at, param := range call.Params {
				given, isObject := param.(map[string]any)
				if !isObject {
					continue
				}
				if problem := fits(given, described.Accepts[at]); problem != "" {
					t.Errorf("%s: %s, argument %d: %s", document, call.Method, at+1, problem)
				}
			}
		}
	}
}

// fits says what is wrong with an object given for an argument, by the description of what the argument takes, or nothing when it fits one of the
// shapes described: a name the shape has not got, or one it needs that is missing.
func fits(given map[string]any, described json.RawMessage) string {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		AnyOf      []json.RawMessage          `json:"anyOf"`
		OneOf      []json.RawMessage          `json:"oneOf"`
	}
	if json.Unmarshal(described, &schema) != nil {
		return "its description cannot be read"
	}

	if schema.Properties == nil {
		problems := []string{}
		for _, option := range append(schema.AnyOf, schema.OneOf...) {
			problem := fits(given, option)
			if problem == "" {
				return ""
			}
			problems = append(problems, problem)
		}

		return "it fits none of the shapes described: " + strings.Join(problems, "; ")
	}

	for name := range given {
		if _, known := schema.Properties[name]; !known {
			return "it has no " + name
		}
	}
	for _, name := range schema.Required {
		if _, has := given[name]; !has {
			return "it needs " + name
		}
	}

	return ""
}
