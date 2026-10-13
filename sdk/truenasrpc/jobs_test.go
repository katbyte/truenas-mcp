package truenasrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// held is a job function that waits to be let go, and says when it has started.
type held struct {
	started chan *truenastest.Run
	release chan struct{}
}

func hold() *held {
	return &held{started: make(chan *truenastest.Run, 64), release: make(chan struct{})}
}

// answering is the job function: it runs until let go or stopped, and then answers the value given.
func (h *held) answering(result any) truenastest.JobHandler {
	return func(run *truenastest.Run, _ []json.RawMessage) (any, error) {
		h.started <- run

		select {
		case <-h.release:
			return result, nil
		case <-run.Aborted():
			return nil, errors.New("stopped")
		}
	}
}

// running waits for the job function to have started, and answers the job as the function sees it.
func (h *held) running(t *testing.T) *truenastest.Run {
	t.Helper()

	select {
	case run := <-h.started:
		return run
	case <-time.After(5 * time.Second):
		t.Fatal("waited five seconds and the job had not started")

		return nil
	}
}

// A scrub or a replication runs for hours. A program that starts one wants to know it was taken on, go on with other things, and come back for the
// answer: so a job is answered at once, with its number, and waited for separately.
func TestAJobIsStartedWithoutWaitingAndItsAnswerReadWhenItIsDone(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("pool.scrub.scrub", work.answering(map[string]any{"errors": 0}))
	conn := connect(t, nas)

	job, err := conn.Start(t.Context(), "pool.scrub.scrub", []any{"tank"})
	if err != nil {
		t.Fatal(err)
	}
	run := work.running(t)

	if job.ID() != int64(run.ID()) || job.Status().Method != "pool.scrub.scrub" {
		t.Errorf("the job started is number %d, %+v, want the NAS's number %d for the scrub", job.ID(), job.Status(), run.ID())
	}
	select {
	case <-job.Done():
		t.Fatal("the job is over before it was let go")
	default:
	}

	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if err := job.Wait(short, nil); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "still running") {
		t.Errorf("giving up on the wait answered %v, want it to say the job is still running", err)
	}

	close(work.release)

	var answer struct {
		Errors int `json:"errors"`
	}
	answer.Errors = -1
	if err := job.Wait(t.Context(), &answer); err != nil || answer.Errors != 0 {
		t.Errorf("the job answered %+v, %v, want what its function returned", answer, err)
	}
	if status := job.Status(); status.State != truenasrpc.JobSuccess || status.Finished.IsZero() || status.Started.IsZero() {
		t.Errorf("the job ended as %+v, want it done, with when it started and ended", status)
	}

	// it was started on a socket of its own, one that was never asked to wait for jobs, and no other was dialled for it
	if calls := nas.Calls("pool.scrub.scrub"); len(calls) != 1 || string(calls[0].Params[0]) != `"tank"` || nas.Dials() != 1 || len(nas.Calls("core.set_options")) != 0 {
		t.Errorf("the scrub was called %+v over %d connections, want once, with its argument, over one", calls, nas.Dials())
	}
}

// Call waits for a job on the NAS's side, where each waiting call takes one of the ten places a NAS gives a connection. Jobs that are started
// take none, so a dozen of them running leave every call its place.
func TestAJobThatWasStartedTakesNoneOfThePlacesCallsAreMadeIn(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("replication.run", work.answering(nil))
	nas.Answer("system.info", map[string]any{})
	conn := connect(t, nas, truenasrpc.WithMaxCalls(2))

	for range 12 {
		if _, err := conn.Start(t.Context(), "replication.run", []any{1}); err != nil {
			t.Fatal(err)
		}
	}

	short, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	for range 4 {
		if err := conn.Call(short, "system.info", nil, nil); err != nil {
			t.Fatalf("a call made beside twelve running jobs failed: %v", err)
		}
	}
	close(work.release)
}

// How far along a job is, is the reason to follow one. Each thing the NAS says of it is passed on, the last being that it is over; a watcher that
// is slower than the job is told the latest and not every step, so it can never fall behind.
func TestAJobsProgressIsToldAsItGoesAndLastOfAllThatItIsOver(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("update.run", work.answering("done"))
	conn := connect(t, nas)

	job, err := conn.Start(t.Context(), "update.run", nil)
	if err != nil {
		t.Fatal(err)
	}
	run := work.running(t)

	var mu sync.Mutex
	var told []truenasrpc.JobStatus
	job.OnUpdate(func(status truenasrpc.JobStatus) {
		mu.Lock()
		defer mu.Unlock()

		told = append(told, status)
	})
	last := func() truenasrpc.JobStatus {
		mu.Lock()
		defer mu.Unlock()

		if len(told) == 0 {
			return truenasrpc.JobStatus{}
		}

		return told[len(told)-1]
	}

	for _, step := range []struct {
		percent int
		doing   string
	}{{25, "downloading"}, {60, "installing"}, {90, "tidying up"}} {
		run.Progress(step.percent, step.doing)
		eventually(t, "the watcher was not told the job was "+step.doing, func() bool { return last().Percent == float64(step.percent) && last().Description == step.doing })
	}
	if status := job.Status(); status.State != truenasrpc.JobRunning || status.Percent != 90 {
		t.Errorf("the job stands at %+v, want it running at 90", status)
	}

	close(work.release)
	<-job.Done()
	eventually(t, "the watcher was not told the job was over", func() bool { return last().State == truenasrpc.JobSuccess })

	// a watcher that comes late is told how it ended
	late := make(chan truenasrpc.JobStatus, 1)
	job.OnUpdate(func(status truenasrpc.JobStatus) { late <- status })
	select {
	case status := <-late:
		if status.State != truenasrpc.JobSuccess {
			t.Errorf("a watcher added after the end was told %+v", status)
		}
	case <-time.After(5 * time.Second):
		t.Error("a watcher added after the end was told nothing")
	}
}

// A job that fails has to be as easy to act on as a call that fails: the same error, that the same questions can be asked of.
func TestAJobThatFailsIsTheErrorACallThatFailedTheSameWayWouldBe(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Job("pool.export", func([]json.RawMessage) (any, error) {
		return nil, &truenastest.Error{Errno: 2, ErrName: "ENOENT", Reason: "there is no pool of that number, and the key was " + truenastest.DefaultKey}
	})
	nas.Job("pool.create", func([]json.RawMessage) (any, error) {
		return nil, &truenastest.InvalidError{Reason: "[EINVAL] pool_create.name: taken", Invalid: []truenastest.Invalid{{Attribute: "pool_create.name", Message: "taken", Errno: 17}}}
	})
	conn := connect(t, nas)

	job, err := conn.Start(t.Context(), "pool.export", []any{7})
	if err != nil {
		t.Fatal(err)
	}

	err = job.Wait(t.Context(), nil)
	refusal, ok := errors.AsType[*truenasrpc.Error](err)
	if !ok || !truenasrpc.IsNotFound(err) || refusal.Method != "pool.export" || refusal.Errno != 2 || !strings.Contains(refusal.Reason, "there is no pool of that number") {
		t.Errorf("a job that failed answered %v, want the error a call would get", err)
	}
	if strings.Contains(err.Error(), truenastest.DefaultKey) {
		t.Errorf("the failure quotes the key: %v", err)
	}
	if job.Status().State != truenasrpc.JobFailed {
		t.Errorf("the job ended as %+v, want it failed", job.Status())
	}

	job, err = conn.Start(t.Context(), "pool.create", []any{map[string]any{"name": "tank"}})
	if err != nil {
		t.Fatal(err)
	}

	err = job.Wait(t.Context(), nil)
	refusal, ok = errors.AsType[*truenasrpc.Error](err)
	if !ok || !truenasrpc.IsInvalid(err) || len(refusal.Invalid()) != 1 || refusal.Invalid()[0].Attribute != "pool_create.name" {
		t.Errorf("a job whose arguments were refused answered %v, want each argument and why", err)
	}
}

// A job started by mistake, or one that is taking the pool with it, has to be stoppable; and stopped is not the same as failed.
func TestAJobIsStoppedByAbortAndSaysSo(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("pool.scrub.scrub", work.answering(nil))
	conn := connect(t, nas)

	job, err := conn.Start(t.Context(), "pool.scrub.scrub", []any{"tank"})
	if err != nil {
		t.Fatal(err)
	}
	work.running(t)

	if err := job.Abort(t.Context()); err != nil {
		t.Fatal(err)
	}

	err = job.Wait(t.Context(), nil)
	if !errors.Is(err, truenasrpc.ErrJobAborted) || job.Status().State != truenasrpc.JobAborted {
		t.Errorf("a job that was stopped answered %v and stands at %+v, want it aborted", err, job.Status())
	}
}

// A job outlives the program that started it, and the person asking how it went may be asking from another one: a job is found again by its
// number, running or over, with the answer as it was answered and not the copy a NAS hands out with its secrets removed.
func TestAJobIsFoundAgainByItsNumberFromAnotherConnection(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("cloudsync.sync", work.answering(map[string]any{"transferred": 12}))
	first, second := connect(t, nas), connect(t, nas)

	started, err := first.Start(t.Context(), "cloudsync.sync", []any{3})
	if err != nil {
		t.Fatal(err)
	}
	run := work.running(t)
	run.Progress(40, "copying")

	found, err := second.Job(t.Context(), started.ID())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the job found by number was not seen to be running at 40", func() bool {
		status := found.Status()

		return status.State == truenasrpc.JobRunning && status.Percent == 40 && status.Method == "cloudsync.sync"
	})

	close(work.release)

	var answer struct {
		Transferred int `json:"transferred"`
	}
	if err := found.Wait(t.Context(), &answer); err != nil || answer.Transferred != 12 {
		t.Errorf("the job found by number answered %+v, %v", answer, err)
	}

	// and after it is over, by a connection that never heard a word of it
	third := connect(t, nas)
	over, err := third.Job(t.Context(), started.ID())
	if err != nil {
		t.Fatal(err)
	}
	answer.Transferred = 0
	if err := over.Wait(t.Context(), &answer); err != nil || answer.Transferred != 12 || over.Status().State != truenasrpc.JobSuccess {
		t.Errorf("a job found after it was over answered %+v, %v, %+v", answer, err, over.Status())
	}

	for _, call := range nas.Calls("core.get_jobs") {
		if len(call.Params) != 2 || !strings.Contains(string(call.Params[1]), `"raw_result":true`) {
			t.Errorf("a job was asked after with %s, want its answer asked for as it was answered", call.Params)
		}
	}

	if _, err := third.Job(t.Context(), 4040); !truenasrpc.IsNotFound(err) {
		t.Errorf("a number the NAS has no job for answered %v, want not found", err)
	}
}

// Start is given a method by name, and nothing stops the name being one the NAS answers at once. That call has been made by the time anyone can
// know, so it is not hidden: the error says it was carried out and holds what it answered. A number is no proof of a job either, since a new
// user's number is a number too.
func TestAMethodThatIsNotAJobIsCarriedOutAndSaidNotToBeOne(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("pool.scrub.scrub", work.answering(nil))
	nas.Answer("system.info", map[string]any{"version": "25.10.7"})
	nas.Answer("user.create", 1)
	conn := connect(t, nas, truenasrpc.WithStartGrace(100*time.Millisecond))

	// job number 1 exists, and is not the call that answers 1 below
	scrub, err := conn.Start(t.Context(), "pool.scrub.scrub", []any{"tank"})
	if err != nil || scrub.ID() != 1 {
		t.Fatalf("the scrub started as %v, %v", scrub, err)
	}

	_, err = conn.Start(t.Context(), "system.info", nil)
	notAJob, ok := errors.AsType[*truenasrpc.NotAJobError](err)
	if !ok || !strings.Contains(string(notAJob.Answer), "25.10.7") || !strings.Contains(err.Error(), "was carried out") {
		t.Errorf("starting a method that is not a job answered %v, want it said and its answer kept", err)
	}

	_, err = conn.Start(t.Context(), "user.create", []any{map[string]any{"username": "new"}})
	if notAJob, ok := errors.AsType[*truenasrpc.NotAJobError](err); !ok || string(notAJob.Answer) != "1" {
		t.Errorf("starting a method that answers a number answered %v, want it not taken for job 1", err)
	}

	close(work.release)
}

// A job goes on while the connection that started it comes and goes. Whoever is waiting on it keeps waiting, and is answered when the connection
// is back, whatever was said while it was away.
func TestAJobIsFollowedThroughAConnectionThatGoesAway(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("replication.run", work.answering("copied"))
	conn := connect(t, nas, truenasrpc.WithRedialWait(func(int) time.Duration { return 10 * time.Millisecond }))

	job, err := conn.Start(t.Context(), "replication.run", []any{1})
	if err != nil {
		t.Fatal(err)
	}
	run := work.running(t)

	nas.Drop()
	run.Progress(70, "said while nobody was listening")
	eventually(t, "the job's standing was not asked after once the connection was back", func() bool { return job.Status().Percent == 70 })

	nas.Drop()
	close(work.release)

	var answer string
	if err := job.Wait(t.Context(), &answer); err != nil || answer != "copied" {
		t.Errorf("a job that ended while the connection was away answered %q, %v", answer, err)
	}
}

// A NAS that restarts forgets its jobs. Waiting for ever on one of them would be the worst answer, and saying it failed would be a guess: it is
// said to be lost.
func TestAJobTheNASForgotWhenItRestartedIsSaidToBeLost(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	work := hold()
	nas.LongJob("replication.run", work.answering(nil))
	conn := connect(t, nas, truenasrpc.WithRedialWait(func(int) time.Duration { return 10 * time.Millisecond }))

	job, err := conn.Start(t.Context(), "replication.run", []any{1})
	if err != nil {
		t.Fatal(err)
	}
	work.running(t)

	nas.Restart()

	if err := job.Wait(t.Context(), nil); !errors.Is(err, truenasrpc.ErrJobLost) {
		t.Errorf("a job the NAS forgot answered %v, want it said to be lost", err)
	}
	close(work.release)
}

// What a NAS says of a job carries what the job was called with and what it answered, and it says it unasked, to a trace that hides a call's
// secrets by the call's name. A job's are hidden the same way, by the name of the method the job is carrying out.
func TestAJobsSecretsAreHiddenInATraceAsItsCallsWouldBe(t *testing.T) {
	t.Parallel()

	const (
		bareKey  = "a-dataset-key-that-is-answered-with-no-name-on-it"
		password = "a-password-inside-what-a-job-answers"
	)

	nas := truenastest.New(t)
	nas.Job("pool.dataset.export_key", func([]json.RawMessage) (any, error) { return bareKey, nil })
	nas.Job("cloudsync.sync", func([]json.RawMessage) (any, error) {
		return map[string]any{"password": password, "shown": "what-a-job-answers-that-is-no-secret"}, nil
	})

	log := &written{}
	conn := connect(t, nas, truenasrpc.WithLog(log))

	for _, method := range []string{"pool.dataset.export_key", "cloudsync.sync"} {
		job, err := conn.Start(t.Context(), method, []any{"tank/secret"})
		if err != nil {
			t.Fatal(err)
		}

		var answer any
		if err := job.Wait(t.Context(), &answer); err != nil {
			t.Fatal(err)
		}
		if text, _ := json.Marshal(answer); !strings.Contains(string(text), bareKey) && !strings.Contains(string(text), password) {
			t.Errorf("%s answered %s, want the caller handed what the job answered, secrets and all", method, text)
		}

		// asked after by number, the answer comes a second way
		again, err := connect(t, nas, truenasrpc.WithLog(log)).Job(t.Context(), job.ID())
		if err != nil {
			t.Fatal(err)
		}
		if err := again.Wait(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
	}

	text := log.text()
	if strings.Contains(text, bareKey) || strings.Contains(text, password) {
		t.Error("a secret a job answered is in the log")
	}
	if !strings.Contains(text, "what-a-job-answers-that-is-no-secret") || !strings.Contains(text, "collection_update") {
		t.Errorf("the log does not show what a job said that is no secret:\n%s", text)
	}
}
