package truenasrpc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// written is a log that keeps what it is sent, for a test to read.
type written struct {
	mu    sync.Mutex
	lines []string
}

func (w *written) Debugf(format string, args ...any) { w.add(format, args...) }
func (w *written) Tracef(format string, args ...any) { w.add(format, args...) }

func (w *written) add(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *written) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return strings.Join(w.lines, "\n")
}

// connect is a connection to a stand-in NAS that trusts its certificate and closes when the test ends.
func connect(t *testing.T, nas *truenastest.NAS, opts ...truenasrpc.Option) *truenasrpc.Conn {
	t.Helper()

	return connectAs(t, nas, truenasrpc.APIKey(nas.Username(), nas.Key()), opts...)
}

func connectAs(t *testing.T, nas *truenastest.NAS, cred truenasrpc.Credential, opts ...truenasrpc.Option) *truenasrpc.Conn {
	t.Helper()

	conn, err := truenasrpc.New(nas.Address(), cred, append([]truenasrpc.Option{truenasrpc.WithRootCAs(nas.Roots())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// sent reports whether anything the stand-in was sent holds the text given.
func sent(nas *truenastest.NAS, text string) bool {
	for _, call := range nas.Calls() {
		for _, param := range call.Params {
			if strings.Contains(string(param), text) {
				return true
			}
		}
	}

	return false
}

// eventually waits for something that happens on another goroutine, and fails the test when it has not after a few seconds.
func eventually(t *testing.T, what string, happened func() bool) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if happened() {
			return
		}
	}

	t.Fatalf("waited five seconds and %s", what)
}

// The whole point: a method is called by name with its arguments in the order given, and what the NAS answers is read into what the caller hands in.
func TestACallSendsItsArgumentsInOrderAndReadsTheAnswer(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Handle("pool.dataset.query", func(params []json.RawMessage) (any, error) {
		return map[string]any{"sent": len(params), "first": params[0], "second": params[1]}, nil
	})

	var got struct {
		Sent   int             `json:"sent"`
		First  [][]any         `json:"first"`
		Second map[string]bool `json:"second"`
	}
	if err := connect(t, nas).Call(t.Context(), "pool.dataset.query", []any{[][]any{{"name", "=", "tank"}}, map[string]bool{"count": true}}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Sent != 2 || len(got.First) != 1 || got.First[0][2] != "tank" || !got.Second["count"] {
		t.Errorf("the NAS was sent %+v, want the filter first and the options second", got)
	}
}

// A call with no arguments sends an empty list, which is what the NAS wants in place of nothing, and an answer nobody asked to read is not an error.
func TestACallWithNoArgumentsAndNowhereToReadIntoIsFine(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{"version": "25.10.7"})

	if err := connect(t, nas).Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}

	calls := nas.Calls("system.info")
	if len(calls) != 1 || calls[0].Params == nil || len(calls[0].Params) != 0 {
		t.Errorf("the NAS was sent %+v, want one call with an empty list of arguments", calls)
	}
}

// A program that builds its connection at start must not hang or fail there because the NAS is away: nothing is dialled until something is asked, and
// Connect is for the program that does want to know at once.
func TestNothingIsDialledUntilTheFirstCallOrConnect(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	conn := connect(t, nas)

	if nas.Dials() != 0 {
		t.Fatalf("making the connection dialled the NAS %d times, want none", nas.Dials())
	}

	if err := conn.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := conn.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if nas.Dials() != 1 || nas.Logins() != 1 {
		t.Errorf("connecting twice dialled %d times and logged in %d, want once each", nas.Dials(), nas.Logins())
	}
}

// Every login is a line in the NAS's audit log and counts against a limit on how often an address may try, so fifty calls made at once share one.
func TestManyCallsAtOnceShareOneConnectionAndOneLogin(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Handle("core.ping", func([]json.RawMessage) (any, error) { return "pong", nil })
	nas.Answer("system.info", map[string]any{"version": "25.10.7"})
	conn := connect(t, nas)

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for range 50 {
		wg.Go(func() { errs <- conn.Call(t.Context(), "system.info", nil, nil) })
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if nas.Dials() != 1 || nas.Logins() != 1 {
		t.Errorf("fifty calls dialled %d times and logged in %d, want once each", nas.Dials(), nas.Logins())
	}
}

// A TrueNAS runs ten calls for a connection, holds ten more and refuses the rest, so thirty sent at once would lose ten. The connection holds the
// others back itself, and all thirty are answered.
func TestNoMoreCallsAreSentAtOnceThanTheNASWillRun(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	gate := make(chan struct{})
	var arrived atomic.Int64
	nas.Handle("pool.query", func([]json.RawMessage) (any, error) {
		arrived.Add(1)
		<-gate

		return []any{}, nil
	})
	conn := connect(t, nas)

	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for range 30 {
		wg.Go(func() { errs <- conn.Call(t.Context(), "pool.query", nil, nil) })
	}

	eventually(t, "ten calls had not reached the NAS", func() bool { return arrived.Load() == truenasrpc.DefaultMaxCalls })
	// long enough for an eleventh to arrive if one were going to
	time.Sleep(100 * time.Millisecond)
	if most := nas.MostAtOnce(); most != truenasrpc.DefaultMaxCalls {
		t.Errorf("the NAS had %d calls from the connection at once, want %d", most, truenasrpc.DefaultMaxCalls)
	}

	close(gate)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// A TrueNAS answers a long call with the number of a job unless the connection has asked it to wait, and a caller reading that number as the result
// its description promises would be reading nonsense. The connection asks, before it logs in.
func TestALongCallAnswersItsResultNotTheNumberOfAJob(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Job("pool.scrub.scrub", func([]json.RawMessage) (any, error) { return map[string]any{"state": "FINISHED"}, nil })

	var got struct {
		State string `json:"state"`
	}
	if err := connect(t, nas).Call(t.Context(), "pool.scrub.scrub", []any{"tank"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "FINISHED" {
		t.Errorf("the scrub answered %+v, want its result", got)
	}

	calls := nas.Calls()
	if len(calls) < 2 || calls[0].Method != "core.set_options" || string(calls[0].Params[0]) != `{"legacy_jobs":false}` {
		t.Errorf("the connection opened with %+v, want it to ask for long calls to be waited for before anything else", calls)
	}
}

// The older login, with a key and no user name, still works on the versions that have it.
func TestAKeyAloneLogsInTheOlderWay(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{})

	if err := connectAs(t, nas, truenasrpc.APIKeyOnly(nas.Key())).Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(nas.Calls("auth.login_with_api_key")) != 1 || len(nas.Calls("auth.login_ex")) != 0 {
		t.Errorf("a key alone logged in with %+v, want the older login only", nas.Calls())
	}
}

// A wrong key must say so in words a person can act on, and must not be tried again on every call: a NAS counts failed logins against the address
// they come from, and an exporter polling every thirty seconds would lock itself out.
func TestAWrongKeyIsRefusedInPlainWordsAndNotTriedAgainAtOnce(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{})
	conn := connectAs(t, nas, truenasrpc.APIKey(nas.Username(), "9-notTheKeyThatWasMade"), truenasrpc.WithRedialWait(func(int) time.Duration { return time.Hour }))

	err := conn.Call(t.Context(), "system.info", nil, nil)
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "AUTH_ERR" || !truenasrpc.IsNotLoggedIn(err) {
		t.Fatalf("a wrong key failed with %v, want a refused login", err)
	}
	if !strings.Contains(err.Error(), "refused the API key") {
		t.Errorf("a wrong key reads %q, want it to say the key was refused", err)
	}

	again := conn.Call(t.Context(), "system.info", nil, nil)
	if !truenasrpc.IsNotLoggedIn(again) || !strings.Contains(again.Error(), "not tried again") {
		t.Errorf("the next call failed with %v, want the same refusal and a word that the NAS is being left alone", again)
	}
	if nas.Dials() != 1 {
		t.Errorf("two calls with a wrong key dialled the NAS %d times, want once", nas.Dials())
	}
}

// TrueNAS revokes a key that was ever sent without TLS, and a key can be given an end date: either way the login answers "expired", and the person
// reading the error needs to know a new key is the fix.
func TestARevokedKeySaysANewOneIsNeeded(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Revoke()

	err := connect(t, nas).Connect(t.Context())
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "EXPIRED" {
		t.Fatalf("a revoked key failed with %v, want an expired login", err)
	}
	if !strings.Contains(err.Error(), "make a new one") {
		t.Errorf("a revoked key reads %q, want it to say a new key is needed", err)
	}
}

// A NAS restarts for an update while the program talking to it keeps running. The next call after that must simply work, on a new connection, with
// nobody restarting anything.
func TestAConnectionThatWentAwayIsDialledAgainByTheNextCall(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{"version": "25.10.7"})
	conn := connect(t, nas)

	if err := conn.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}

	nas.Drop()

	// a call made in the instant before the loss is noticed fails as one that got no answer; the first made after it succeeds
	eventually(t, "no call had succeeded after the connection was dropped", func() bool {
		err := conn.Call(t.Context(), "system.info", nil, nil)
		if err != nil && !truenasrpc.IsDisconnected(err) {
			t.Fatalf("a call after the drop failed with %v, want only a lost connection or success", err)
		}

		return err == nil
	})
	if nas.Dials() != 2 || nas.Logins() != 2 {
		t.Errorf("after a drop the NAS was dialled %d times and logged into %d, want twice each", nas.Dials(), nas.Logins())
	}
}

// A call that was with the NAS when the connection went may or may not have been carried out. The error has to say it had been sent, because a
// caller may repeat a read on that and must not repeat a write.
func TestACallWaitingWhenTheConnectionGoesFailsAsOneThatWasSent(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	reached := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	nas.Handle("pool.create", func([]json.RawMessage) (any, error) {
		close(reached)
		<-release

		return nil, nil
	})
	conn := connect(t, nas)

	failed := make(chan error, 1)
	go func() { failed <- conn.Call(t.Context(), "pool.create", nil, nil) }()

	<-reached
	nas.Drop()

	err := <-failed
	lost, ok := errors.AsType[*truenasrpc.ConnError](err)
	if !ok || !lost.Sent || !truenasrpc.IsDisconnected(err) {
		t.Errorf("the call failed with %v, want a lost connection that says the call had been sent", err)
	}
}

// A NAS that is down is asked again after a wait, not on every call, and is used again once it is back without anyone stepping in.
func TestANASThatIsDownIsLeftAloneBetweenTriesAndUsedAgainWhenItIsBack(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{})
	nas.Down()

	var failures []int
	var mu sync.Mutex
	wait := atomic.Int64{}
	wait.Store(int64(time.Hour))
	conn := connect(t, nas, truenasrpc.WithRedialWait(func(n int) time.Duration {
		mu.Lock()
		defer mu.Unlock()

		failures = append(failures, n)

		return time.Duration(wait.Load())
	}))

	for range 3 {
		if err := conn.Call(t.Context(), "system.info", nil, nil); !truenasrpc.IsDisconnected(err) {
			t.Fatalf("a call to a NAS that is down failed with %v, want a connection that could not be made", err)
		}
	}

	mu.Lock()
	tried := len(failures)
	mu.Unlock()
	if tried != 1 {
		t.Errorf("three calls to a NAS that is down dialled it %d times, want once", tried)
	}

	// a fresh connection with a short wait, to see it come back
	wait.Store(int64(10 * time.Millisecond))
	back := connect(t, nas, truenasrpc.WithRedialWait(func(int) time.Duration { return 10 * time.Millisecond }))
	if err := back.Call(t.Context(), "system.info", nil, nil); !truenasrpc.IsDisconnected(err) {
		t.Fatalf("a call to a NAS that is down failed with %v, want a connection that could not be made", err)
	}

	nas.Up()
	eventually(t, "the NAS was not used again once it was back", func() bool { return back.Call(t.Context(), "system.info", nil, nil) == nil })
}

// What the NAS says went wrong reaches the caller in the NAS's words, and a caller can ask what kind of failure it was without reading codes.
func TestARefusalSaysWhatKindItIs(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Refuse("pool.create", truenastest.ErrNotAuthorized)
	nas.Refuse("pool.get_instance", truenastest.NoSuchInstance("Pool 7"))
	nas.Refuse("filesystem.stat", &truenastest.Error{Errno: 2, ErrName: "ENOENT", Reason: "Path /mnt/nowhere not found"})
	nas.Refuse("pool.dataset.create", &truenastest.InvalidError{Reason: "[EINVAL] pool_dataset_create.name: Dataset name is required", Invalid: []truenastest.Invalid{{Attribute: "pool_dataset_create.name", Message: "Dataset name is required", Errno: 22}}})
	conn := connect(t, nas)

	for _, tc := range []struct {
		method string
		is     func(error) bool
		reads  string
	}{
		{"pool.create", truenasrpc.IsNotAllowed, "pool.create: Not authorized (EACCES)"},
		// a real NAS says a thing it was asked for by id is not there as an argument it refuses, and anything else not there as a call that
		// failed: both are "not found", and the first is not a bad argument
		{"pool.get_instance", truenasrpc.IsNotFound, "pool.get_instance: Pool 7 does not exist (ENOENT)"},
		{"filesystem.stat", truenasrpc.IsNotFound, "filesystem.stat: Path /mnt/nowhere not found (ENOENT)"},
		{"pool.dataset.create", truenasrpc.IsInvalid, "pool.dataset.create: the TrueNAS refused what it was sent: pool_dataset_create.name: Dataset name is required"},
		{"pool.no_such_thing", truenasrpc.IsNoSuchMethod, "the TrueNAS has no method pool.no_such_thing"},
	} {
		err := conn.Call(t.Context(), tc.method, nil, nil)
		if err == nil || !tc.is(err) {
			t.Errorf("%s failed with %v, which is not the kind expected", tc.method, err)

			continue
		}
		if err.Error() != tc.reads {
			t.Errorf("%s reads %q, want %q", tc.method, err, tc.reads)
		}

		kinds := 0
		for _, is := range []func(error) bool{truenasrpc.IsNotAllowed, truenasrpc.IsNotFound, truenasrpc.IsInvalid, truenasrpc.IsNoSuchMethod, truenasrpc.IsNotLoggedIn, truenasrpc.IsTooManyCalls, truenasrpc.IsDisconnected} {
			if is(err) {
				kinds++
			}
		}
		if kinds != 1 {
			t.Errorf("%s is %d kinds of failure at once, want one: %v", tc.method, kinds, err)
		}
	}

	// the connection is none the worse for five refusals
	if nas.Dials() != 1 {
		t.Errorf("five refusals dialled the NAS %d times, want once", nas.Dials())
	}
}

// A NAS can end a session under a connection that is still open. The call that finds out fails, and the next one logs in again unasked.
func TestASessionTheNASEndedIsLoggedIntoAgain(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	var asked atomic.Int64
	nas.Handle("system.info", func([]json.RawMessage) (any, error) {
		if asked.Add(1) == 1 {
			return nil, &truenastest.Error{Errno: 207, ErrName: "ENOTAUTHENTICATED", Reason: "Not authenticated"}
		}

		return map[string]any{}, nil
	})
	conn := connect(t, nas)

	if err := conn.Call(t.Context(), "system.info", nil, nil); !truenasrpc.IsNotLoggedIn(err) {
		t.Fatalf("the call failed with %v, want one that says nobody is logged in", err)
	}
	if err := conn.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}
	if nas.Logins() != 2 {
		t.Errorf("the NAS was logged into %d times, want a second login after it ended the first", nas.Logins())
	}
}

// The key opens the whole NAS. It must not reach a log at any level, nor an error, even when the NAS quotes it back; and the other secrets that pass
// through (a share's password, a cloud account's keys) must not reach the trace either.
func TestNoSecretReachesTheLogOrAnError(t *testing.T) {
	t.Parallel()

	const (
		password = "hunter2-a-share-password"
		cloudKey = "AKIA-a-cloud-account-key"
		bareKey  = "a-dataset-encryption-key-with-no-name-on-it"
	)

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{"version": "25.10.7", "hostname": "shown-in-the-trace"})
	nas.Answer("cloudsync.credentials.query", []any{map[string]any{"id": 1, "name": "offsite", "provider": map[string]any{"type": "S3", "access_key_id": cloudKey, "secret_access_key": cloudKey}}})
	nas.Answer("pool.dataset.export_key", bareKey)
	nas.Handle("user.update", func([]json.RawMessage) (any, error) { return 1, nil })
	nas.Refuse("mail.send", &truenastest.Error{Errno: 22, ErrName: "EINVAL", Reason: "could not log in with " + nas.Key()})

	log := &written{}
	conn := connect(t, nas, truenasrpc.WithLog(log))

	for _, call := range []struct {
		method string
		params []any
	}{
		{"system.info", nil},
		{"cloudsync.credentials.query", nil},
		{"pool.dataset.export_key", []any{"tank/secret"}},
		{"user.update", []any{7, map[string]any{"password": password, "full_name": "shown-too"}}},
	} {
		if err := conn.Call(t.Context(), call.method, call.params, nil); err != nil {
			t.Fatal(err)
		}
	}

	err := conn.Call(t.Context(), "mail.send", nil, nil)
	if err == nil || strings.Contains(err.Error(), nas.Key()) || !strings.Contains(err.Error(), "could not log in with ***") {
		t.Errorf("a refusal that quotes the key reads %q, want the key blanked and the rest kept", err)
	}

	text := log.text()
	for name, secret := range map[string]string{"the API key": nas.Key(), "a password sent": password, "a cloud account's key": cloudKey, "a bare dataset key": bareKey} {
		if strings.Contains(text, secret) {
			t.Errorf("%s is in the log", name)
		}
	}

	// the trace has to be worth having: what is not secret is there to read
	for _, want := range []string{"auth.login_ex", "system.info", "shown-in-the-trace", "shown-too", "tank/secret"} {
		if want == "tank/secret" {
			// the arguments of a call that answers a bare key are left out whole, the dataset's name with them
			if strings.Contains(text, want) {
				t.Errorf("the log shows %q, the argument of a call whose arguments are left out", want)
			}

			continue
		}
		if !strings.Contains(text, want) {
			t.Errorf("the log does not show %q:\n%s", want, text)
		}
	}
}

// A caller that gives up on a slow call gets its context's error at once, and has not broken the connection for the calls beside it.
func TestACallGivesUpWhenItsContextDoesAndLeavesTheConnectionWorking(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	nas.Handle("replication.run", func([]json.RawMessage) (any, error) {
		<-release

		return nil, nil
	})
	nas.Answer("system.info", map[string]any{})
	conn := connect(t, nas)

	// connected first, so that the short deadline below is the call's alone and not the dial's as well
	if err := conn.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	failed := make(chan error, 1)
	go func() { failed <- conn.Call(ctx, "replication.run", []any{3}, nil) }()

	select {
	case err := <-failed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the call failed with %v, want its context's deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call had not given up two seconds after a 50ms deadline")
	}

	if err := conn.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}
	if nas.Dials() != 1 {
		t.Errorf("giving up on a call dialled the NAS again (%d times), want the one connection kept", nas.Dials())
	}
}

// A context with no deadline would wait on a hung call for ever, so one is given a limit; a call that needs longer says so with its own context.
func TestACallWithNoDeadlineOfItsOwnIsGivenOne(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	nas.Handle("pool.query", func([]json.RawMessage) (any, error) {
		<-release

		return nil, nil
	})

	conn := connect(t, nas, truenasrpc.WithCallWait(50*time.Millisecond))
	if err := conn.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}

	err := conn.Call(context.WithoutCancel(t.Context()), "pool.query", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the call failed with %v, want the deadline it was given", err)
	}
}

// A TrueNAS closes the connection on a message over 64 kB, which would fail every other call on it. One that long is refused before it leaves.
func TestACallTooLongToSendIsRefusedBeforeItLeaves(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{})
	nas.Handle("filesystem.put", func([]json.RawMessage) (any, error) { return nil, nil })
	conn := connect(t, nas)

	err := conn.Call(t.Context(), "filesystem.put", []any{strings.Repeat("x", 70_000)}, nil)
	if err == nil || !strings.Contains(err.Error(), "drops the connection") {
		t.Fatalf("a 70 kB call failed with %v, want it refused for its length", err)
	}

	if err := conn.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(nas.Calls("filesystem.put")) != 0 || nas.Dials() != 1 {
		t.Errorf("the NAS saw %d of the long call and %d dials, want none and one", len(nas.Calls("filesystem.put")), nas.Dials())
	}
}

// A TrueNAS comes with a certificate it signed itself, so the first thing anyone meets is a certificate error. It has to say what to do about it,
// and switching the check off has to work for the person who chooses that.
func TestACertificateNothingVouchesForSaysWhatToDo(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{})

	wary, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wary.Close() })

	err = wary.Call(t.Context(), "system.info", nil, nil)
	if !truenasrpc.IsDisconnected(err) || !strings.Contains(err.Error(), "name the authority that signed it") {
		t.Errorf("an untrusted certificate failed with %v, want it to say what to do", err)
	}
	if nas.Dials() != 0 {
		t.Errorf("the NAS was connected to %d times through a certificate nothing vouched for", nas.Dials())
	}

	trusting, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key(), truenasrpc.EvenUnchecked()), truenasrpc.WithoutCertificateCheck())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trusting.Close() })

	if err := trusting.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Errorf("with the check switched off the call failed: %v", err)
	}
}

// Before version 26 a TrueNAS takes a key only as it is. With the certificate check switched off that hands the key to whatever answers at the
// address, and something in the middle of a newer NAS's connection could claim to be an older one to be handed it. So a key or a password is not
// sent as it is to a NAS nothing vouches for, unless the caller says it may be; and the refusal is the login's, not a NAS that is down.
func TestAKeyIsNotSentAsItIsToANASNothingVouchesFor(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{})
	_, secret, _ := strings.Cut(nas.Key(), "-")

	for name, cred := range map[string]truenasrpc.Credential{
		"a key and its user": truenasrpc.APIKey(nas.Username(), nas.Key()),
		"a key alone":        truenasrpc.APIKeyOnly(nas.Key()),
		"a password":         truenasrpc.Password(nas.Username(), nas.Password()),
	} {
		conn, err := truenasrpc.New(nas.Address(), cred, truenasrpc.WithoutCertificateCheck())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })

		err = conn.Call(t.Context(), "system.info", nil, nil)
		if !truenasrpc.IsNotLoggedIn(err) || truenasrpc.IsDisconnected(err) || !strings.Contains(err.Error(), "nothing vouches for its certificate") {
			t.Errorf("%s, unchecked, failed with %v, want a login given up for the certificate", name, err)
		}
	}
	if sent(nas, secret) || sent(nas, nas.Password()) || nas.Logins() != 0 {
		t.Errorf("a NAS nothing vouched for was sent a secret, or logged into: %+v", nas.Calls())
	}

	for name, cred := range map[string]truenasrpc.Credential{
		"a key and its user": truenasrpc.APIKey(nas.Username(), nas.Key(), truenasrpc.EvenUnchecked()),
		"a key alone":        truenasrpc.APIKeyOnly(nas.Key(), truenasrpc.EvenUnchecked()),
		"a password":         truenasrpc.Password(nas.Username(), nas.Password(), truenasrpc.EvenUnchecked()),
	} {
		conn, err := truenasrpc.New(nas.Address(), cred, truenasrpc.WithoutCertificateCheck())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })

		if err := conn.Call(t.Context(), "system.info", nil, nil); err != nil {
			t.Errorf("%s, told it may be sent anyway, failed with %v", name, err)
		}
	}
}

// From version 26 a TrueNAS is logged into by proving the key is held, both ways, so the key is never sent: not to the NAS, not to a log, not to
// anything listening. That is the login used wherever the NAS offers it, with nothing asked of the caller.
func TestANewerNASIsLoggedIntoWithoutTheKeyEverBeingSent(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t, truenastest.Version26())
	nas.Answer("system.info", map[string]any{})
	number, secret, _ := strings.Cut(nas.Key(), "-")

	log := &written{}
	if err := connect(t, nas, truenasrpc.WithLog(log)).Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}

	if nas.Logins() != 1 || len(nas.Calls("system.info")) != 1 {
		t.Fatalf("the NAS was logged into %d times and called %+v, want one login and the call", nas.Logins(), nas.Calls())
	}
	if sent(nas, secret) {
		t.Errorf("the key was sent to a NAS that takes a login without it: %+v", nas.Calls())
	}
	if strings.Contains(log.text(), secret) {
		t.Error("the key is in the log")
	}

	logins := nas.Calls("auth.login_ex")
	if len(logins) != 2 {
		t.Fatalf("the login took %+v, want its two messages", logins)
	}
	var first struct {
		Mechanism string `json:"mechanism"`
		RFC       string `json:"rfc_str"`
	}
	if err := json.Unmarshal(logins[0].Params[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.Mechanism != "SCRAM" || !strings.HasPrefix(first.RFC, "p=tls-server-end-point,,n="+nas.Username()+":"+number+",r=") {
		t.Errorf("the login opened with %+v, want one that names the user and the number of the key and is tied to the certificate", first)
	}

	// it does not need the certificate vouched for either: nothing is sent that whatever answers could use
	unchecked, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key()), truenasrpc.WithoutCertificateCheck())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unchecked.Close() })

	if err := unchecked.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Errorf("with the certificate check switched off the newer login failed: %v", err)
	}
	if sent(nas, secret) {
		t.Errorf("the key was sent to a NAS nothing vouched for: %+v", nas.Calls())
	}
}

// A wrong key gets the same plain refusal from the newer login as from the older, and a NAS reached through something that shows its own
// certificate refuses a right key the same way, so the refusal says that too.
func TestANewerNASRefusesAWrongKeyAndSaysWhatElseItMightBe(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t, truenastest.Version26())

	err := connectAs(t, nas, truenasrpc.APIKey(nas.Username(), "7-notTheKeyThatWasMade")).Connect(t.Context())
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "AUTH_ERR" {
		t.Fatalf("a wrong key failed with %v, want a refused login", err)
	}
	if !strings.Contains(err.Error(), "refused the API key") || !strings.Contains(err.Error(), "reverse proxy") {
		t.Errorf("a wrong key reads %q, want it to say the key was refused and what else refuses a right one", err)
	}

	// a key of another number is turned away before anything is proved, where the certificate has not come into it
	err = connectAs(t, nas, truenasrpc.APIKey(nas.Username(), "9-notTheKeyThatWasMade")).Connect(t.Context())
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "AUTH_ERR" || strings.Contains(err.Error(), "reverse proxy") {
		t.Errorf("a key the NAS has not got failed with %v, want a refused login and no word about the certificate", err)
	}
	if nas.Logins() != 0 {
		t.Errorf("a wrong key logged in %d times", nas.Logins())
	}
}

// The newer login is tied to the certificate the NAS showed. Something in the middle that shows a certificate of its own and passes every message
// along gets nothing for it: the NAS sees a login tied to a certificate that is not its own, and refuses. Left untied, the same login is passed
// along and taken, which is what WithoutChannelBinding gives up.
func TestALoginPassedAlongBySomethingInTheMiddleIsRefused(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t, truenastest.Version26())
	nas.Answer("system.info", map[string]any{})
	middle := truenastest.InTheMiddle(t, nas)
	_, secret, _ := strings.Cut(nas.Key(), "-")

	tied, err := truenasrpc.New(middle, truenasrpc.APIKey(nas.Username(), nas.Key()), truenasrpc.WithoutCertificateCheck())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tied.Close() })

	err = tied.Call(t.Context(), "system.info", nil, nil)
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "AUTH_ERR" {
		t.Errorf("a login passed along failed with %v, want the NAS to refuse it", err)
	}
	if nas.Logins() != 0 || len(nas.Calls("system.info")) != 0 || sent(nas, secret) {
		t.Errorf("something in the middle got a login, a call or the key through: %+v", nas.Calls())
	}

	untied, err := truenasrpc.New(middle, truenasrpc.APIKey(nas.Username(), nas.Key(), truenasrpc.WithoutChannelBinding()), truenasrpc.WithoutCertificateCheck())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = untied.Close() })

	if err := untied.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Errorf("a login tied to nothing was not taken through the same relay: %v", err)
	}
	if sent(nas, secret) {
		t.Errorf("the key was sent: %+v", nas.Calls())
	}
}

// The NAS proves it holds the key as well. Something answering in its place can say yes to any login, and with the certificate check switched off
// that is all it would take; it cannot make the proof, so the connection is given up before a single call is made on it.
func TestSomethingAnsweringInTheNASsPlaceIsFoundOutBeforeAnyCallIsMade(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t, truenastest.Impostor())
	nas.Answer("system.info", map[string]any{})

	conn, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key()), truenasrpc.WithoutCertificateCheck())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	err = conn.Call(t.Context(), "system.info", nil, nil)
	if !truenasrpc.IsNotLoggedIn(err) || !strings.Contains(err.Error(), "could not prove it holds the key") {
		t.Errorf("an impostor's yes was answered with %v, want the login given up", err)
	}
	if len(nas.Calls("system.info")) != 0 {
		t.Error("a call was made on a connection to something that could not prove it was the NAS")
	}
}

// A key can be kept as a file, in any of the forms TrueNAS hands one out in. One of them holds only the parts worked out of the key, so that the
// key itself is kept nowhere: enough for the newer login, and said plainly to be no use to a NAS that wants the key.
func TestAKeyFileLogsInWithTheKeyOrWithOnlyThePartsWorkedOutOfIt(t *testing.T) {
	t.Parallel()

	file := func(contents string) string {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	newer := truenastest.New(t, truenastest.Version26())
	newer.Answer("system.info", map[string]any{})
	older := truenastest.New(t)
	older.Answer("system.info", map[string]any{})
	_, secret, _ := strings.Cut(newer.Key(), "-")

	for name, contents := range map[string]string{
		"the key on a line of its own":   newer.Key() + "\n",
		"a JSON file of the key":         `{"raw_key": "` + newer.Key() + `"}`,
		"an INI file of the key":         "[TRUENAS_API_KEY]\nraw_key = " + newer.Key() + "\n",
		"a file of the worked-out parts": newer.KeyFile(),
	} {
		if err := connectAs(t, newer, truenasrpc.APIKeyFile(newer.Username(), file(contents))).Call(t.Context(), "system.info", nil, nil); err != nil {
			t.Errorf("%s did not log into a newer NAS: %v", name, err)
		}
	}
	if sent(newer, secret) {
		t.Errorf("the key was sent: %+v", newer.Calls())
	}

	if err := connectAs(t, older, truenasrpc.APIKeyFile(older.Username(), file(`{"raw_key": "`+older.Key()+`"}`))).Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Errorf("a JSON file of the key did not log into an older NAS: %v", err)
	}

	err := connectAs(t, older, truenasrpc.APIKeyFile(older.Username(), file(older.KeyFile()))).Call(t.Context(), "system.info", nil, nil)
	if !truenasrpc.IsNotLoggedIn(err) || !strings.Contains(err.Error(), "it needs the key itself") {
		t.Errorf("the worked-out parts of a key, given to a NAS that wants the key, failed with %v", err)
	}

	if _, err := truenasrpc.New(older.Address(), truenasrpc.APIKeyFile(older.Username(), filepath.Join(t.TempDir(), "not-there"))); err == nil || !strings.Contains(err.Error(), "key file") {
		t.Errorf("a key file that is not there = %v, want it refused when the connection is made", err)
	}
	if _, err := truenasrpc.New(older.Address(), truenasrpc.APIKeyOnly(older.KeyFile())); err == nil || !strings.Contains(err.Error(), "name of the user") {
		t.Errorf("the worked-out parts of a key with no user name = %v, want it refused when the connection is made", err)
	}
}

// A person logs in with a password, and with a one-time password too where their account asks for one. A one-time password is good for one login,
// so it is asked for afresh each time the connection has to be made again.
func TestAPasswordLogsInAndAOneTimePasswordIsAskedForAtEachLogin(t *testing.T) {
	t.Parallel()

	plain := truenastest.New(t)
	plain.Answer("system.info", map[string]any{})

	log := &written{}
	if err := connectAs(t, plain, truenasrpc.Password(plain.Username(), plain.Password()), truenasrpc.WithLog(log)).Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatalf("a password did not log in: %v", err)
	}
	if strings.Contains(log.text(), plain.Password()) {
		t.Error("the password is in the log")
	}

	err := connectAs(t, plain, truenasrpc.Password(plain.Username(), "not-the-password")).Connect(t.Context())
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "AUTH_ERR" || strings.Contains(err.Error(), "not-the-password") {
		t.Errorf("a wrong password failed with %v, want a refused login that does not quote it", err)
	}

	const token = "246810"
	twoFactor := truenastest.New(t, truenastest.WithOneTimePassword(token))
	twoFactor.Answer("system.info", map[string]any{})

	err = connectAs(t, twoFactor, truenasrpc.Password(twoFactor.Username(), twoFactor.Password())).Connect(t.Context())
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "OTP_REQUIRED" || !strings.Contains(err.Error(), "one-time password") {
		t.Errorf("an account with two-factor login and nowhere to get the second from failed with %v", err)
	}

	var asked atomic.Int64
	log = &written{}
	conn := connectAs(t, twoFactor, truenasrpc.Password(twoFactor.Username(), twoFactor.Password(), truenasrpc.WithOTP(func() (string, error) {
		asked.Add(1)

		return token, nil
	})), truenasrpc.WithLog(log))
	if err := conn.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatalf("a password and a one-time password did not log in: %v", err)
	}

	twoFactor.Drop()
	eventually(t, "the connection was not noticed to be gone", func() bool { return conn.Call(t.Context(), "system.info", nil, nil) == nil && twoFactor.Logins() == 2 })
	if asked.Load() != 2 {
		t.Errorf("two logins asked for a one-time password %d times, want once each", asked.Load())
	}
	if text := log.text(); strings.Contains(text, token) || strings.Contains(text, twoFactor.Password()) {
		t.Error("the password or the one-time password is in the log")
	}

	err = connectAs(t, twoFactor, truenasrpc.Password(twoFactor.Username(), twoFactor.Password(), truenasrpc.WithOTP(func() (string, error) { return "000000", nil }))).Connect(t.Context())
	if login, ok := errors.AsType[*truenasrpc.LoginError](err); !ok || login.Response != "AUTH_ERR" {
		t.Errorf("a wrong one-time password failed with %v, want a refused login", err)
	}

	err = connectAs(t, twoFactor, truenasrpc.Password(twoFactor.Username(), twoFactor.Password(), truenasrpc.WithOTP(func() (string, error) { return "", errors.New("the phone is in another room") }))).Connect(t.Context())
	if !truenasrpc.IsNotLoggedIn(err) || !strings.Contains(err.Error(), "the phone is in another room") {
		t.Errorf("a one-time password that could not be got failed with %v, want the reason passed on", err)
	}
}

// The certificate a TrueNAS comes with is made out to "localhost", so it can never pass a check by name however it is trusted. Handing over the
// certificate itself is the setting that works on a NAS as it comes, without giving up knowing it is the NAS.
func TestTheCertificateANASComesWithIsAcceptedByItselfWhateverNameIsOnIt(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t, truenastest.WithCertificate(truenastest.SelfSigned(t, nil, "localhost", time.Now().AddDate(1, 0, 0))))
	nas.Answer("system.info", map[string]any{})
	cred := truenasrpc.APIKey(nas.Username(), nas.Key())

	// trusted as an authority, it is still refused for its name: the stand-in is dialled at 127.0.0.1, as a NAS is dialled at anything but localhost
	byName, err := truenasrpc.New(nas.Address(), cred, truenasrpc.WithRootCAs(nas.Roots()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = byName.Close() })

	err = byName.Call(t.Context(), "system.info", nil, nil)
	if !truenasrpc.IsDisconnected(err) || !strings.Contains(err.Error(), "made out to another name") || !strings.Contains(err.Error(), "hand over the certificate itself") {
		t.Errorf("a certificate made out to localhost failed with %v, want it to say the name is the trouble and what to do", err)
	}

	byKey, err := truenasrpc.New(nas.Address(), cred, truenasrpc.WithCertificate(nas.Certificate()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = byKey.Close() })

	if err := byKey.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Errorf("handed the certificate itself, the call failed: %v", err)
	}
	if nas.Dials() != 1 {
		t.Errorf("the NAS was connected to %d times, want once: not at all through the name check, and once by its key", nas.Dials())
	}
}

// What a NAS is known by is its key, so that renewing its certificate, or letting it run out, does not lock out the programs watching it; and
// anything showing another key is refused, since that is either a NAS given a new certificate or something that is not the NAS.
func TestANASIsKnownByItsKeyThroughARenewalAndAnExpiryAndNotByAnotherKey(t *testing.T) {
	t.Parallel()

	// the certificate on file ran out yesterday
	onFile := truenastest.SelfSigned(t, nil, "localhost", time.Now().AddDate(0, 0, -1))
	key, ok := onFile.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("the certificate's key is a %T", onFile.PrivateKey)
	}

	for _, tc := range []struct {
		name  string
		shown tls.Certificate
		taken bool
	}{
		{"the same certificate, though it has run out", onFile, true},
		{"a renewal made with the same key", truenastest.SelfSigned(t, key, "localhost", time.Now().AddDate(1, 0, 0)), true},
		{"a new certificate made with a new key", truenastest.SelfSigned(t, nil, "localhost", time.Now().AddDate(1, 0, 0)), false},
	} {
		nas := truenastest.New(t, truenastest.WithCertificate(tc.shown))
		nas.Answer("system.info", map[string]any{})

		conn, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key()), truenasrpc.WithCertificate(onFile.Leaf))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })

		err = conn.Call(t.Context(), "system.info", nil, nil)
		switch {
		case tc.taken && err != nil:
			t.Errorf("a NAS showing %s was refused: %v", tc.name, err)
		case !tc.taken && (!truenasrpc.IsDisconnected(err) || !strings.Contains(err.Error(), "not the one it is known by") || nas.Dials() != 0):
			t.Errorf("a NAS showing %s failed with %v after %d connections, want it refused before it is sent anything", tc.name, err, nas.Dials())
		}
	}
}

// A NAS that hangs, or a network that silently stops carrying the connection, leaves a socket that looks open. Without being asked whether it is
// still there, the next call would wait its whole deadline to find out.
func TestANASThatStopsAnsweringIsNoticedWithoutACall(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{})
	log := &written{}
	conn := connect(t, nas, truenasrpc.WithLog(log), truenasrpc.WithPing(20*time.Millisecond, 100*time.Millisecond))

	if err := conn.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}

	thaw := nas.Freeze()
	// the stand-in stops reading once it has taken the message it was already waiting for
	if err := conn.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the connection had not been given up on", func() bool { return strings.Contains(log.text(), "it stopped answering") })

	thaw()
	if err := conn.Call(t.Context(), "system.info", nil, nil); err != nil {
		t.Fatal(err)
	}
	if nas.Dials() != 2 {
		t.Errorf("the NAS was dialled %d times, want a second after the first connection was given up on", nas.Dials())
	}
}

// Closing ends the calls waiting on an answer, and every call after it says the connection is closed, not that the NAS is unreachable.
func TestCloseEndsWaitingCallsAndRefusesNewOnes(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	reached := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	nas.Handle("pool.query", func([]json.RawMessage) (any, error) {
		close(reached)
		<-release

		return nil, nil
	})
	conn := connect(t, nas)

	failed := make(chan error, 1)
	go func() { failed <- conn.Call(t.Context(), "pool.query", nil, nil) }()

	<-reached
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	if err := <-failed; !errors.Is(err, truenasrpc.ErrClosed) {
		t.Errorf("the waiting call failed with %v, want the connection closed", err)
	}
	if err := conn.Call(t.Context(), "pool.query", nil, nil); !errors.Is(err, truenasrpc.ErrClosed) {
		t.Errorf("a call after closing failed with %v, want the connection closed", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("closing twice failed: %v", err)
	}
}

// What is wrong with a connection's settings is said when it is made, in the words of the setting, not at the first call.
func TestSettingsThatCannotWorkAreRefusedWhenTheConnectionIsMade(t *testing.T) {
	t.Parallel()

	key := truenasrpc.APIKey("metrics", "1-aKeyOfSomeLength")

	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		address string
		cred    truenasrpc.Credential
		opts    []truenasrpc.Option
		reads   string
	}{
		{"no address", "", key, nil, "address is required"},
		{"no key", "nas.example.net", truenasrpc.APIKey("metrics", " "), nil, "API key is required"},
		{"no user name", "nas.example.net", truenasrpc.APIKey("", "1-aKeyOfSomeLength"), nil, "name of the user"},
		{"no key, older login", "nas.example.net", truenasrpc.APIKeyOnly(""), nil, "API key is required"},
		{"no credential", "nas.example.net", truenasrpc.Credential{}, nil, "logged into with an API key"},
		{"http", "http://nas.example.net", key, nil, "revokes an API key sent without it"},
		{"ws", "ws://nas.example.net/api/current", key, nil, "revokes an API key sent without it"},
		{"another scheme", "ftp://nas.example.net", key, nil, "neither https nor wss"},
		{"a password in the address", "https://admin:hunter2@nas.example.net", key, nil, "given apart from the address"},
		{"more calls than the NAS holds", "nas.example.net", key, []truenasrpc.Option{truenasrpc.WithMaxCalls(21)}, "refuses the twenty-first"},
		{"an authority file that is not there", "nas.example.net", key, []truenasrpc.Option{truenasrpc.WithCAFile("testdata/no-such-file.pem")}, "certificate authority file"},
		{"a certificate file that is not there", "nas.example.net", key, []truenasrpc.Option{truenasrpc.WithCertificateFile("testdata/no-such-file.pem")}, "certificate file"},
		{"a certificate file with no certificate in it", "nas.example.net", key, []truenasrpc.Option{truenasrpc.WithCertificateFile(empty)}, "holds no PEM certificate"},
		{"two ways of trusting the certificate", "nas.example.net", key, []truenasrpc.Option{truenasrpc.WithRootCAs(x509.NewCertPool()), truenasrpc.WithoutCertificateCheck()}, "more than one way of trusting"},
	} {
		_, err := truenasrpc.New(tc.address, tc.cred, tc.opts...)
		if err == nil || !strings.Contains(err.Error(), tc.reads) {
			t.Errorf("%s: failed with %v, want it to say %q", tc.name, err, tc.reads)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error quotes the password in the address: %v", tc.name, err)
		}
	}
}

// A program running on the NAS itself reaches it over the NAS's own socket, where the NAS can see who is at the other end: nothing is sent to log
// in, no key is kept anywhere, and everything else works as it does over the network.
func TestAProgramOnTheNASItselfIsLoggedInByWhoItRunsAs(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t, truenastest.OnSocket())
	nas.Answer("system.info", map[string]any{"version": "25.10.7"})
	nas.Job("pool.scrub.scrub", func([]json.RawMessage) (any, error) { return "clean", nil })

	conn, err := truenasrpc.New(nas.Address(), truenasrpc.LocalUser())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var info struct {
		Version string `json:"version"`
	}
	if err := conn.Call(t.Context(), "system.info", nil, &info); err != nil || info.Version != "25.10.7" {
		t.Fatalf("a call over the socket answered %+v, %v", info, err)
	}
	if conn.Address() != nas.Address() || !strings.HasPrefix(conn.Address(), "unix:///") {
		t.Errorf("the connection says it reaches the NAS at %s, want the socket %s", conn.Address(), nas.Address())
	}
	if len(nas.Calls("auth.login_ex", "auth.login_with_api_key", "auth.mechanism_choices")) != 0 || len(nas.Calls("auth.me")) != 1 {
		t.Errorf("over the socket the NAS was sent %+v, want it only asked who is logged in", nas.Calls())
	}

	job, err := conn.Start(t.Context(), "pool.scrub.scrub", []any{"tank"})
	if err != nil {
		t.Fatal(err)
	}
	var answer string
	if err := job.Wait(t.Context(), &answer); err != nil || answer != "clean" {
		t.Errorf("a job started over the socket answered %q, %v", answer, err)
	}

	// a key works over the socket too, for a program that is to act as someone other than who it runs as
	keyed, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keyed.Close() })

	if err := keyed.Call(t.Context(), "system.info", nil, nil); err != nil || len(nas.Calls("auth.login_ex")) != 1 {
		t.Errorf("a key over the socket: %v, after %+v", err, nas.Calls("auth.login_ex"))
	}
}

// The NAS only takes a connection over its socket for a user it knows. A program run as anyone else is told so when it connects, in words about the
// user, and a login that only a socket can give is refused for a NAS reached any other way.
func TestAUserTheNASDoesNotKnowIsToldSoAndTheSocketsLoginIsOnlyForTheSocket(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t, truenastest.Stranger())
	nas.Answer("system.info", map[string]any{})

	conn, err := truenasrpc.New(nas.Address(), truenasrpc.LocalUser())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	err = conn.Call(t.Context(), "system.info", nil, nil)
	if !truenasrpc.IsNotLoggedIn(err) || truenasrpc.IsDisconnected(err) || !strings.Contains(err.Error(), "does not know the user this program runs as") {
		t.Errorf("a user the NAS has no account for was answered %v", err)
	}
	if len(nas.Calls("system.info")) != 0 {
		t.Error("a call was made for a user the NAS does not know")
	}

	if _, err := truenasrpc.New("nas.example.net", truenasrpc.LocalUser()); err == nil || !strings.Contains(err.Error(), "own socket") {
		t.Errorf("the socket's login for a NAS reached over the network = %v, want it refused when the connection is made", err)
	}
	gone, err := truenasrpc.New("unix:///nowhere/at/all.sock", truenasrpc.LocalUser())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gone.Close() })

	if err := gone.Connect(t.Context()); !truenasrpc.IsDisconnected(err) || !strings.Contains(err.Error(), "unix:///nowhere/at/all.sock") {
		t.Errorf("a socket that is not there was answered %v, want the NAS said to be unreachable at the socket", err)
	}
}
