//go:build acceptance

package truenasrpc_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// The first thing anyone does with a NAS: log in with a key and ask what it runs. The version it says is the one the environment said it
// installed, and nothing of the key reaches the log on the way.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAKeyLogsInAndTheNASSaysWhatItRuns(t *testing.T) {
	log := &written{}
	conn := admin(t, truenasrpc.WithLog(log))

	var info struct {
		Version  string `json:"version"`
		Hostname string `json:"hostname"`
	}
	if err := conn.Call(within(t, time.Minute), "system.info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version == "" || info.Version != made(t).Version {
		t.Errorf("the NAS says it runs %q, and said %q when it was set up", info.Version, made(t).Version)
	}
	if err := conn.Ping(within(t, time.Minute)); err != nil {
		t.Errorf("ping: %v", err)
	}

	_, secret, _ := strings.Cut(made(t).Key, "-")
	if text := log.text(); strings.Contains(text, secret) || !strings.Contains(text, "system.info") {
		t.Errorf("the key is in the log, or the call is not: %d bytes logged", len(text))
	}
}

// A person logs in with a password, and a key with no user name still works on the versions that have that login.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAPasswordAndAKeyAloneLogInToo(t *testing.T) {
	if err := root(t).Connect(within(t, time.Minute)); err != nil {
		t.Errorf("a password: %v", err)
	}
	if err := as(t, truenasrpc.APIKeyOnly(made(t).Key)).Connect(within(t, time.Minute)); err != nil {
		t.Errorf("a key alone: %v", err)
	}

	err := as(t, truenasrpc.APIKey(nas.username, "999-notAKeyThisNASEverMade")).Connect(within(t, time.Minute))
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "AUTH_ERR" {
		t.Errorf("a wrong key answered %v, want a refused login", err)
	}
}

// A program built on this tells what went wrong by asking the error, so each kind has to come back as the kind it is from a real NAS and not
// only from the stand-in.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestARealNASsRefusalsAreToldApart(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 2*time.Minute)

	if err := conn.Call(ctx, "pool.no_such_method", nil, nil); !truenasrpc.IsNoSuchMethod(err) {
		t.Errorf("a method the NAS has not got answered %v", err)
	}

	err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{}}, nil)
	if refusal, ok := errors.AsType[*truenasrpc.Error](err); !ok || !truenasrpc.IsInvalid(err) || len(refusal.Invalid()) == 0 {
		t.Errorf("a dataset with no name answered %v, want the argument refused and named", err)
	}

	if err := conn.Call(ctx, "pool.dataset.get_instance", []any{layout.Pool + "/not-a-dataset"}, nil); !truenasrpc.IsNotFound(err) {
		t.Errorf("a dataset that is not there answered %v, want not found", err)
	}

	// the user who may only read can read, and is told a write is not theirs to make
	only := reader(t)
	if err := only.Call(ctx, "pool.query", nil, nil); err != nil {
		t.Errorf("the user who may only read could not read the pools: %v", err)
	}
	if err := only.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": layout.Pool + "/not-allowed"}}, nil); !truenasrpc.IsNotAllowed(err) {
		t.Errorf("the user who may only read was answered %v for a write, want not allowed", err)
	}
}

// A scrub is the plainest job a NAS has. Started, it is answered at once with its number; it is followed to its end; it is found again by that
// number from another connection; and the same method called the ordinary way waits and answers when it is done.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAJobIsStartedFollowedFoundAgainAndWaitedFor(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 5*time.Minute)

	job, err := conn.Start(ctx, "pool.scrub.scrub", []any{layout.Pool, "START"})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var told []truenasrpc.JobState
	job.OnUpdate(func(s truenasrpc.JobStatus) {
		mu.Lock()
		defer mu.Unlock()

		told = append(told, s.State)
	})

	if err := job.Wait(ctx, nil); err != nil {
		t.Fatalf("the scrub: %v", err)
	}
	if status := job.Status(); job.ID() == 0 || status.State != truenasrpc.JobSuccess || status.Method != "pool.scrub.scrub" || status.Finished.IsZero() {
		t.Errorf("the scrub ended as %+v", status)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		last := slices.Clone(told)
		mu.Unlock()

		if len(last) > 0 && last[len(last)-1] == truenasrpc.JobSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("whoever followed the scrub was told %v, want its end last", last)

			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	again, err := admin(t).Job(ctx, job.ID())
	if err != nil {
		t.Fatalf("finding the scrub again by its number: %v", err)
	}
	if err := again.Wait(ctx, nil); err != nil || again.Status().State != truenasrpc.JobSuccess {
		t.Errorf("the scrub found again ended as %+v, %v", again.Status(), err)
	}

	if err := conn.Call(ctx, "pool.scrub.scrub", []any{layout.Pool, "START"}, nil); err != nil {
		t.Errorf("a scrub called the ordinary way: %v", err)
	}

	_, err = conn.Start(ctx, "system.info", nil)
	if _, notAJob := errors.AsType[*truenasrpc.NotAJobError](err); !notAJob {
		t.Errorf("starting a method that is not a job answered %v", err)
	}
	if _, err := conn.Job(ctx, 99_999_999); !truenasrpc.IsNotFound(err) {
		t.Errorf("a number the NAS has no job for answered %v, want not found", err)
	}
}

// What the NAS says unasked is heard: here, what it says of its jobs, since a job is something a test can make happen.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestWhatTheNASSaysOfItsJobsIsHeard(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 5*time.Minute)

	heard := make(chan truenasrpc.Event, 64)
	sub, err := conn.Subscribe(ctx, "core.get_jobs", func(e truenasrpc.Event) {
		select {
		case heard <- e:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()

	job, err := conn.Start(ctx, "pool.scrub.scrub", []any{layout.Pool, "START"})
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Wait(ctx, nil); err != nil {
		t.Fatal(err)
	}

	for {
		select {
		case e := <-heard:
			var id int64
			if json.Unmarshal(e.ID, &id) == nil && id == job.ID() && (e.Kind == truenasrpc.Added || e.Kind == truenasrpc.Changed) && len(e.Fields) > 0 {
				return
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("nothing was heard of job %d", job.ID())
		}
	}
}

// A file is handed to the NAS, asked after, and fetched back the same; and the NAS's own configuration, the file a backup is, is fetched whole.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAFileIsHandedOverAndFetchedBackTheSame(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 5*time.Minute)

	contents := bytes.Repeat([]byte("a line of a file handed to the test NAS\n"), 5000)
	path := "/mnt/" + layout.Datasets[0] + "/handed-over.txt"

	job, err := conn.Upload(ctx, "filesystem.put", []any{path, map[string]any{}}, bytes.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Wait(ctx, nil); err != nil {
		t.Fatalf("the NAS dealing with the file: %v", err)
	}

	var stat struct {
		Size int64 `json:"size"`
	}
	if err := conn.Call(ctx, "filesystem.stat", []any{path}, &stat); err != nil || stat.Size != int64(len(contents)) {
		t.Errorf("the file on the NAS is %d bytes (%v), and %d were handed over", stat.Size, err, len(contents))
	}

	var back bytes.Buffer
	if copied, err := conn.Download(ctx, "filesystem.get", []any{path}, &back); err != nil || copied != int64(len(contents)) || !bytes.Equal(back.Bytes(), contents) {
		t.Errorf("%d bytes came back (%v), the same as were sent: %v", copied, err, bytes.Equal(back.Bytes(), contents))
	}

	var config bytes.Buffer
	if copied, err := conn.Download(ctx, "config.save", []any{map[string]any{}}, &config); err != nil || copied == 0 {
		t.Errorf("the NAS's configuration came to %d bytes: %v", copied, err)
	}

	if _, err := conn.Download(ctx, "filesystem.get", []any{"/mnt/" + layout.Datasets[0] + "/not-a-file"}, &back); err == nil {
		t.Error("fetching a file that is not there answered no error")
	}
}

// The description of the API kept in api-defs is what everything is generated from and checked against. A NAS of that version must have every
// method the description names, or the description is of something else.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestTheNASHasEveryMethodItsDescriptionNames(t *testing.T) {
	documents, err := filepath.Glob("../../api-defs/truenas-methods-*.json")
	if err != nil || len(documents) == 0 {
		t.Fatalf("no description of the API under api-defs (%v)", err)
	}

	var has map[string]json.RawMessage
	if err := admin(t).Call(within(t, 5*time.Minute), "core.get_methods", nil, &has); err != nil {
		t.Fatal(err)
	}

	for _, document := range documents {
		version := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(document), "truenas-methods-"), ".json")
		if !strings.Contains(made(t).Version, version) {
			continue
		}

		raw, err := os.ReadFile(document) //nolint:gosec // G304: the files are the repository's own, found by the glob above
		if err != nil {
			t.Fatal(err)
		}

		var described map[string]json.RawMessage
		if err := json.Unmarshal(raw, &described); err != nil {
			t.Fatal(err)
		}

		var missing, undescribed []string
		for name := range described {
			if _, ok := has[name]; !ok {
				missing = append(missing, name)
			}
		}
		for name := range has {
			if _, ok := described[name]; !ok {
				undescribed = append(undescribed, name)
			}
		}
		slices.Sort(missing)
		slices.Sort(undescribed)

		if len(missing) > 0 || len(undescribed) > 0 {
			t.Errorf("%s: the NAS lacks %d methods the description names %v, and has %d it does not %v", document, len(missing), missing, len(undescribed), undescribed)
		}
	}
}
