package truenasrpc_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// heard collects the events a subscription is handed, for a test to read.
type heard struct {
	mu     sync.Mutex
	events []truenasrpc.Event
}

func (h *heard) take(e truenasrpc.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.events = append(h.events, e)
}

func (h *heard) all() []truenasrpc.Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.events)
}

// kinds is what was heard, as "kind id" for each event in order.
func (h *heard) kinds() string {
	events := h.all()
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, strings.TrimSpace(e.Kind+" "+string(e.ID)))
	}

	return strings.Join(out, ", ")
}

// A NAS says when an alert is raised or a pool changes, to whoever asked. That is how a program knows at once instead of asking over and over, so
// each thing said has to arrive, whole and in the order it was said.
func TestWhatTheNASSaysOfSomethingWatchedIsHandedOverInOrder(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	conn := connect(t, nas)

	alerts, everything := &heard{}, &heard{}
	sub, err := conn.Subscribe(t.Context(), "alert.list", alerts.take)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Subscribe(t.Context(), "*", everything.take); err != nil {
		t.Fatal(err)
	}

	nas.Say("alert.list", "added", "a1", map[string]any{"level": "WARNING", "formatted": "a disk is warm"})
	nas.Say("pool.query", "changed", 3, map[string]any{"status": "DEGRADED"})
	nas.Say("alert.list", "changed", "a1", map[string]any{"level": "CRITICAL", "formatted": "a disk is hot"})
	nas.Say("alert.list", "removed", "a1", nil)

	eventually(t, "what was said of alerts was not all heard: "+alerts.kinds(), func() bool { return len(alerts.all()) == 3 })
	if got := alerts.kinds(); got != `added "a1", changed "a1", removed "a1"` {
		t.Errorf("of alerts, in order, was heard: %s", got)
	}
	if first := alerts.all()[0]; first.Collection != "alert.list" || !strings.Contains(string(first.Fields), "a disk is warm") {
		t.Errorf("the first thing heard was %+v, want the alert as the NAS wrote it", first)
	}

	eventually(t, "a subscription to everything did not hear everything: "+everything.kinds(), func() bool { return len(everything.all()) == 4 })
	if second := everything.all()[1]; second.Collection != "pool.query" || string(second.ID) != "3" {
		t.Errorf("a subscription to everything heard %+v second, want the pool's change under the pool's name", second)
	}

	// once closed it hears no more, and the NAS is told to stop saying
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	nas.Say("alert.list", "added", "a2", map[string]any{})
	eventually(t, "the NAS was not told to stop saying of alerts", func() bool { return nas.Subscribed("alert.list") == 0 })
	eventually(t, "the subscription to everything did not hear what came after", func() bool { return len(everything.all()) == 5 })

	if len(alerts.all()) != 3 || sub.Err() != nil {
		t.Errorf("a closed subscription heard %s and ended with %v, want nothing more and no error", alerts.kinds(), sub.Err())
	}
	select {
	case <-sub.Done():
	default:
		t.Error("a closed subscription does not say it is over")
	}
}

// Some of what a NAS says, it says once for every time it was asked: two parts of a program that each watch the same figures would both be told
// everything twice. The NAS is asked once however many are listening, and stops being asked when the last of them does.
func TestWatchingTheSameThingTwiceAsksTheNASOnce(t *testing.T) {
	t.Parallel()

	const figures = `reporting.realtime:{"interval":2}`

	nas := truenastest.New(t)
	conn := connect(t, nas)

	one, two := &heard{}, &heard{}
	first, err := conn.Subscribe(t.Context(), figures, one.take)
	if err != nil {
		t.Fatal(err)
	}
	second, err := conn.Subscribe(t.Context(), figures, two.take)
	if err != nil {
		t.Fatal(err)
	}
	if nas.Subscribed(figures) != 1 {
		t.Fatalf("two listeners asked the NAS %d times, want once", nas.Subscribed(figures))
	}

	nas.Say(figures, "added", nil, map[string]any{"cpu": 12})
	eventually(t, "both listeners did not hear", func() bool { return len(one.all()) == 1 && len(two.all()) == 1 })

	_ = first.Close()
	nas.Say(figures, "added", nil, map[string]any{"cpu": 14})
	eventually(t, "the listener left did not go on hearing", func() bool { return len(two.all()) == 2 })

	if len(one.all()) != 1 || nas.Subscribed(figures) != 1 {
		t.Errorf("after one of two closed, it had heard %d and the NAS was asked %d times, want 1 and still 1", len(one.all()), nas.Subscribed(figures))
	}

	_ = second.Close()
	eventually(t, "the NAS was not told to stop once nobody was listening", func() bool { return nas.Subscribed(figures) == 0 })
}

// A NAS restarts for an update while the program watching it keeps running. Nobody should have to notice and subscribe again; but whatever was
// said while the connection was away was not heard, and a program keeping a copy of what it watches has to be told so, to ask for it afresh.
func TestWatchingGoesOnThroughANASThatRestartsAndSaysThereWasAGap(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	conn := connect(t, nas, truenasrpc.WithRedialWait(func(int) time.Duration { return 10 * time.Millisecond }))

	alerts := &heard{}
	if _, err := conn.Subscribe(t.Context(), "alert.list", alerts.take); err != nil {
		t.Fatal(err)
	}

	nas.Say("alert.list", "added", "before", map[string]any{})
	eventually(t, "the first alert was not heard", func() bool { return len(alerts.all()) == 1 })

	nas.Down()
	time.Sleep(50 * time.Millisecond)
	nas.Up()

	eventually(t, "the NAS was not asked again once it was back", func() bool { return nas.Subscribed("alert.list") == 1 })
	nas.Say("alert.list", "added", "after", map[string]any{})

	eventually(t, "what was said after the restart was not heard: "+alerts.kinds(), func() bool { return len(alerts.all()) == 3 })
	if got := alerts.kinds(); got != `added "before", resumed, added "after"` {
		t.Errorf("through a restart was heard: %s", got)
	}
}

// A NAS can stop saying of something: a log being followed comes to an end, or the right to watch is taken away. The subscription is over, and
// says why, so that a program does not sit waiting on something that will never speak again.
func TestTheNASEndingWhatWasWatchedEndsTheSubscriptionWithItsReason(t *testing.T) {
	t.Parallel()

	const log = `filesystem.file_tail_follow:/var/log/messages`

	nas := truenastest.New(t)
	conn := connect(t, nas)

	quiet, err := conn.Subscribe(t.Context(), log, func(truenasrpc.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	nas.EndSubscriptions(log, nil)

	select {
	case <-quiet.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a subscription the NAS ended is not over")
	}
	if !errors.Is(quiet.Err(), truenasrpc.ErrEnded) {
		t.Errorf("a subscription the NAS ended without a word ended with %v", quiet.Err())
	}

	failed, err := conn.Subscribe(t.Context(), log, func(truenasrpc.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	nas.EndSubscriptions(log, &truenastest.Error{Errno: 2, ErrName: "ENOENT", Reason: "the file is gone"})

	select {
	case <-failed.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a subscription the NAS ended with an error is not over")
	}
	if err := failed.Err(); !truenasrpc.IsNotFound(err) || !strings.Contains(err.Error(), "the file is gone") {
		t.Errorf("a subscription the NAS ended with an error ended with %v, want what the NAS said", err)
	}
}

// When the connection has to be made again and the NAS turns the key down, trying for ever would only get the address locked out. What was being
// watched ends, with the refusal as its reason.
func TestAKeyThatStopsWorkingEndsWhatWasWatchedAndIsNotTriedForEver(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("replication.run", work.answering(nil))
	conn := connect(t, nas, truenasrpc.WithRedialWait(func(int) time.Duration { return 10 * time.Millisecond }))

	sub, err := conn.Subscribe(t.Context(), "alert.list", func(truenasrpc.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	job, err := conn.Start(t.Context(), "replication.run", []any{1})
	if err != nil {
		t.Fatal(err)
	}

	nas.Revoke()
	nas.Drop()

	select {
	case <-sub.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a subscription whose connection could not be logged into again is not over")
	}
	if login, ok := errors.AsType[*truenasrpc.LoginError](sub.Err()); !ok || login.Response != "EXPIRED" {
		t.Errorf("the subscription ended with %v, want the refused login", sub.Err())
	}
	if err := job.Wait(t.Context(), nil); !truenasrpc.IsNotLoggedIn(err) {
		t.Errorf("the job being followed ended with %v, want the refused login", err)
	}

	dials := nas.Dials()
	time.Sleep(200 * time.Millisecond)
	if again := nas.Dials(); again != dials {
		t.Errorf("the NAS was dialled %d more times after it turned the key down", again-dials)
	}
	close(work.release)
}

// Closing the connection ends everything that hangs off it, and says it was the closing that did.
func TestCloseEndsWhatIsWatchedAndFollowed(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("replication.run", work.answering(nil))
	conn := connect(t, nas)

	sub, err := conn.Subscribe(t.Context(), "alert.list", func(truenasrpc.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	job, err := conn.Start(t.Context(), "replication.run", []any{1})
	if err != nil {
		t.Fatal(err)
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	<-sub.Done()
	if !errors.Is(sub.Err(), truenasrpc.ErrClosed) {
		t.Errorf("a subscription on a closed connection ended with %v", sub.Err())
	}
	if err := job.Wait(t.Context(), nil); !errors.Is(err, truenasrpc.ErrClosed) {
		t.Errorf("a job followed on a closed connection ended with %v", err)
	}
	if _, err := conn.Subscribe(t.Context(), "alert.list", func(truenasrpc.Event) {}); !errors.Is(err, truenasrpc.ErrClosed) {
		t.Errorf("subscribing on a closed connection answered %v", err)
	}
	if _, err := conn.Start(t.Context(), "replication.run", nil); !errors.Is(err, truenasrpc.ErrClosed) {
		t.Errorf("starting a job on a closed connection answered %v", err)
	}
	close(work.release)
}

// A program can ask whether the NAS is there without asking it anything else, and say it means to call methods TrueNAS keeps for itself, which a
// NAS otherwise writes a warning about each time.
func TestPingSaysTheNASIsThereAndPrivateMethodsAreAskedForOnEverySocket(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	conn := connect(t, nas, truenasrpc.WithPrivateMethods())

	if err := conn.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := conn.Subscribe(t.Context(), "alert.list", func(truenasrpc.Event) {}); err != nil {
		t.Fatal(err)
	}

	options := nas.Calls("core.set_options")
	if len(options) != 2 {
		t.Fatalf("two sockets set their options %d times", len(options))
	}
	for _, call := range options {
		if !strings.Contains(string(call.Params[0]), `"private_methods":true`) {
			t.Errorf("a socket set %s, want it to ask for private methods", call.Params[0])
		}
	}
}
