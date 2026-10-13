package truenasrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// jobsCollection is what a NAS says of its jobs under: each one as it is added, and again whenever it gets further or ends.
const jobsCollection = "core.get_jobs"

// startGrace is how long a call that answered a number is given to turn out to be a job, when the NAS has not yet said so.
const startGrace = 2 * time.Second

// JobState is where a job has got to.
type JobState string

const (
	JobWaiting JobState = "WAITING"
	JobRunning JobState = "RUNNING"
	JobSuccess JobState = "SUCCESS"
	JobFailed  JobState = "FAILED"
	JobAborted JobState = "ABORTED"
)

// Finished reports whether a job in this state is over, one way or another.
func (s JobState) Finished() bool { return s == JobSuccess || s == JobFailed || s == JobAborted }

// ErrJobAborted is a job that was stopped before it was done.
var ErrJobAborted = errors.New("the job was aborted")

// ErrJobLost is a job the NAS no longer knows of: it restarted, which forgets every job, or the job ended long enough ago to be forgotten.
var ErrJobLost = errors.New("the TrueNAS no longer knows of the job")

// NotAJobError is a method given to Start that the NAS does not run as a job: it was carried out, and Answer is what it answered.
type NotAJobError struct {
	Method string
	Answer json.RawMessage
}

func (e *NotAJobError) Error() string {
	return e.Method + ": the TrueNAS does not run this as a job: it was carried out, and answered at once"
}

// JobStatus is how a job stands, as the NAS last said.
type JobStatus struct {
	// ID is the job's number, which is how it is found again (Conn.Job), and Method what it is carrying out.
	ID     int64
	Method string
	State  JobState
	// Percent is how far along the job says it is, from 0 to 100, Description what it says it is doing, and Extra whatever more it says.
	Percent     float64
	Description string
	Extra       json.RawMessage
	// Abortable says the job can be stopped.
	Abortable bool
	// Started and Finished are when it did each, Finished being the zero time for a job that has not.
	Started  time.Time
	Finished time.Time
	// LogsExcerpt is the end of what the job has logged, for one that keeps a log.
	LogsExcerpt string
}

// Job is a long piece of work a NAS is carrying out: a scrub, a replication, an update. Start makes one, and Conn.Job finds one by its number.
type Job struct {
	conn *Conn
	id   int64
	done chan struct{}
	// changed wakes whatever is telling the job's watchers
	changed chan struct{}

	mu       sync.Mutex
	status   JobStatus
	result   json.RawMessage
	failure  error
	finished bool
	// heard says the NAS has said something of the job unasked since its standing was last asked for, which is then the newer of the two
	heard bool
	// version counts how many times the status has changed, so that each watcher is told of each standing once
	version  uint64
	watchers []*jobWatcher
	telling  bool
}

type jobWatcher struct {
	fn   func(JobStatus)
	told uint64
}

// starting is a job that has been asked for and not yet given a number.
type starting struct {
	method string
	bound  chan struct{}
	job    *Job
}

// jobItem is a job as a NAS writes one.
type jobItem struct {
	ID          int64             `json:"id"`
	MessageIDs  []json.RawMessage `json:"message_ids"`
	Method      string            `json:"method"`
	Abortable   bool              `json:"abortable"`
	LogsExcerpt *string           `json:"logs_excerpt"`
	Progress    struct {
		Percent     *float64        `json:"percent"`
		Description *string         `json:"description"`
		Extra       json.RawMessage `json:"extra"`
	} `json:"progress"`
	Result  json.RawMessage `json:"result"`
	Error   *string         `json:"error"`
	ExcInfo *struct {
		Type  *string         `json:"type"`
		Errno *int            `json:"errno"`
		Extra json.RawMessage `json:"extra"`
	} `json:"exc_info"`
	State        JobState `json:"state"`
	TimeStarted  Time     `json:"time_started"`
	TimeFinished Time     `json:"time_finished"`
}

// Start has the NAS begin a method it runs as a job, and answers the job as soon as the NAS has taken it on, without waiting for it to be done:
// Wait waits, OnUpdate follows its progress, Abort stops it. Unlike Call, a job started this way takes none of the places calls are made in,
// however long it runs.
//
// A method the NAS does not run as a job is carried out all the same, and the error is a *NotAJobError holding what it answered.
func (c *Conn) Start(ctx context.Context, method string, params []any) (*Job, error) {
	if method == "" {
		return nil, errors.New("a job needs the name of a method")
	}
	if _, set := ctx.Deadline(); !set && c.o.callWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.o.callWait)
		defer cancel()
	}

	if err := c.watchJobs(ctx); err != nil {
		return nil, err
	}

	free, err := c.events.place(ctx, method)
	if err != nil {
		return nil, err
	}

	l, err := c.events.connected(ctx)
	if err != nil {
		free()

		return nil, err
	}

	// the NAS says a job has been added before it answers the call that added it, naming the message that asked: the message's number is noted
	// before it leaves, so that the job is known for this one's the moment it is heard of
	message := l.ids.Add(1)
	asked := &starting{method: method, bound: make(chan struct{})}

	c.mu.Lock()
	c.starting[message] = asked
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.starting, message)
		c.mu.Unlock()
	}()

	var answer json.RawMessage
	if _, err := l.callAs(ctx, message, method, params, &answer, free); err != nil {
		return nil, err
	}

	select {
	case <-asked.bound:
		return asked.job, nil
	default:
	}

	// a job is answered by its number, and so are other things: a number alone does not make it a job
	var number int64
	if json.Unmarshal(answer, &number) != nil {
		return nil, &NotAJobError{Method: method, Answer: answer}
	}

	items, err := l.jobItems(ctx, []int64{number})
	if err != nil {
		return nil, fmt.Errorf("%s: it answered %d, which is its job's number if it is a job, and the job could not be asked after: %w", method, number, err)
	}
	if len(items) == 1 && items[0].Method == method && items[0].askedBy(message) {
		return c.follow(items[0]), nil
	}

	select {
	case <-asked.bound:
		return asked.job, nil
	case <-time.After(c.o.startGrace):
		return nil, &NotAJobError{Method: method, Answer: answer}
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: gave up waiting to learn whether it is a job: %w", method, context.Cause(ctx))
	}
}

// Job finds a job by its number, one started here or by anyone else the key's user may see the jobs of, running or over. A number the NAS has
// no job for is an error IsNotFound is true of.
func (c *Conn) Job(ctx context.Context, id int64) (*Job, error) {
	if _, set := ctx.Deadline(); !set && c.o.callWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.o.callWait)
		defer cancel()
	}

	if err := c.watchJobs(ctx); err != nil {
		return nil, err
	}

	// the job is followed before the NAS is asked how it stands, so that nothing the NAS says of it in between is missed: the last thing it says
	// of a job is that it is over
	c.mu.Lock()
	j, following := c.jobs[id], true
	if j == nil {
		j, following = c.newJob(id, ""), false
	}
	c.mu.Unlock()

	if following {
		return j, nil
	}

	var items []jobItem
	l, err := c.events.connected(ctx)
	if err == nil {
		items, err = l.jobItems(ctx, []int64{id})
	}
	if err == nil && len(items) == 0 {
		err = &Error{Method: jobsCollection, Code: CodeCallFailed, Message: "Method call error", Errno: 2, ErrName: ErrnoName(2), Reason: fmt.Sprintf("the TrueNAS has no job %d: it never had, or the job ended long enough ago to be forgotten", id)}
	}
	if err != nil {
		c.mu.Lock()
		if c.jobs[id] == j {
			delete(c.jobs, id)
		}
		c.mu.Unlock()
		j.lose(err)

		return nil, err
	}

	j.take(items[0], false)

	return j, nil
}

// ID is the job's number, which finds it again from another program or after this one restarts.
func (j *Job) ID() int64 { return j.id }

// Status is how the job stands, as the NAS last said.
func (j *Job) Status() JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.status
}

// Done is closed when the job is over, or can no longer be followed.
func (j *Job) Done() <-chan struct{} { return j.done }

// Wait waits for the job to be over and reads what it answered into into, which may be nil. A job that failed is an *Error, as a call that
// failed is; one that was stopped is ErrJobAborted, and one the NAS forgot is ErrJobLost. It waits as long as the context says, through a NAS
// that restarts its connection, and giving up on the wait leaves the job running.
func (j *Job) Wait(ctx context.Context, into any) error {
	select {
	case <-j.done:
	case <-ctx.Done():
		return fmt.Errorf("gave up waiting for job %d (%s), which is still running: %w", j.id, j.Status().Method, context.Cause(ctx))
	}

	j.mu.Lock()
	failure, result, method := j.failure, j.result, j.status.Method
	j.mu.Unlock()

	if failure != nil {
		return failure
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(result, into); err != nil {
		return fmt.Errorf("%s: what the job answered is not what was expected: %w", method, err)
	}

	return nil
}

// OnUpdate has fn called with how the job stands now, and again each time the NAS says it has got further, the last time when it is over. A
// standing that is overtaken before fn has returned from the one before is skipped: fn is always told the latest, not every one.
func (j *Job) OnUpdate(fn func(JobStatus)) {
	j.mu.Lock()
	j.watchers = append(j.watchers, &jobWatcher{fn: fn})
	begin := !j.telling
	j.telling = true
	j.mu.Unlock()

	if begin {
		go j.tell()

		return
	}
	j.nudge()
}

// Abort asks the NAS to stop the job. It answers once the NAS has taken the request: the job is over when Done is closed.
func (j *Job) Abort(ctx context.Context) error {
	return j.conn.Call(ctx, "core.job_abort", []any{j.id}, nil)
}

// tell calls each watcher with every standing it has not been told of, until the job is over and they all have been.
func (j *Job) tell() {
	for {
		j.mu.Lock()
		status, version := j.status, j.version
		var due []func(JobStatus)
		for _, w := range j.watchers {
			if w.told != version {
				w.told = version
				due = append(due, w.fn)
			}
		}
		if len(due) == 0 && j.finished {
			j.telling = false
			j.mu.Unlock()

			return
		}
		j.mu.Unlock()

		for _, fn := range due {
			fn(status)
		}
		if len(due) > 0 {
			continue
		}

		select {
		case <-j.changed:
		case <-j.conn.done:
			return
		}
	}
}

func (j *Job) nudge() {
	select {
	case j.changed <- struct{}{}:
	default:
	}
}

// take takes in how the NAS says the job stands. said says the NAS said it unasked, and not in answer to being asked: of the two, what it said
// unasked is the newer, so an answer that arrives after it does not put the job back.
func (j *Job) take(item jobItem, said bool) {
	c := j.conn

	j.mu.Lock()
	if j.finished || !said && j.heard && !item.State.Finished() {
		j.mu.Unlock()

		return
	}
	j.heard = j.heard || said

	j.status.Method, j.status.State, j.status.Abortable = item.Method, item.State, item.Abortable
	j.status.Started, j.status.Finished, j.status.Extra = item.TimeStarted.Time, item.TimeFinished.Time, item.Progress.Extra
	if item.Progress.Percent != nil {
		j.status.Percent = *item.Progress.Percent
	}
	if item.Progress.Description != nil {
		j.status.Description = *item.Progress.Description
	}
	if item.LogsExcerpt != nil {
		j.status.LogsExcerpt = *item.LogsExcerpt
	}
	j.version++

	over := item.State.Finished()
	if over {
		j.finished, j.result = true, item.Result

		switch item.State {
		case JobFailed:
			j.failure = c.jobFailure(item)
		case JobAborted:
			j.failure = fmt.Errorf("%s (job %d): %w", item.Method, item.ID, ErrJobAborted)
		case JobWaiting, JobRunning, JobSuccess:
		}
		close(j.done)
	}
	j.mu.Unlock()

	j.nudge()
	if over {
		c.mu.Lock()
		if c.jobs[j.id] == j {
			delete(c.jobs, j.id)
		}
		c.mu.Unlock()
	}
}

// lose ends the following of a job that can no longer be followed, for the reason given. The job itself may well be running still.
func (j *Job) lose(why error) {
	j.mu.Lock()
	if j.finished {
		j.mu.Unlock()

		return
	}
	j.finished, j.failure = true, fmt.Errorf("%s (job %d): %w", j.status.Method, j.id, why)
	j.version++
	close(j.done)
	j.mu.Unlock()

	j.nudge()
}

// askedBy reports whether a job was started by the message of this number.
func (i jobItem) askedBy(message int64) bool {
	return slices.ContainsFunc(i.MessageIDs, func(raw json.RawMessage) bool {
		var number int64

		return json.Unmarshal(raw, &number) == nil && number == message
	})
}

// jobFailure is a job that failed, as the error a call that failed the same way would be.
func (c *Conn) jobFailure(item jobItem) error {
	e := &Error{Method: item.Method, Code: CodeCallFailed, Message: "Method call error"}
	if item.Error != nil {
		e.Reason = c.scrub(*item.Error, scrubbed)
	}

	if exc := item.ExcInfo; exc != nil {
		if exc.Type != nil && *exc.Type == "VALIDATION" {
			e.Code, e.Message, e.Errno = CodeInvalidParams, "Invalid params", 22
		}
		if exc.Errno != nil {
			e.Errno = *exc.Errno
		}
		if string(exc.Extra) != "null" {
			e.Extra = json.RawMessage(c.scrub(string(exc.Extra), scrubbed))
		}
	}
	if e.Errno != 0 {
		e.ErrName = ErrnoName(e.Errno)
	}

	return e
}

// watchJobs has the NAS say of its jobs from now on, on the socket jobs are started on.
func (c *Conn) watchJobs(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return ErrClosed
	}
	w := c.watched[jobsCollection]
	if w == nil {
		w = &watch{}
		c.watched[jobsCollection] = w
	}
	w.jobs = true
	c.mu.Unlock()

	return c.wire(ctx, jobsCollection)
}

// follow is the job a NAS's account of one belongs to, made and kept if it is not yet followed, with the account taken in.
func (c *Conn) follow(item jobItem) *Job {
	c.mu.Lock()
	j := c.jobs[item.ID]
	if j == nil {
		j = c.newJob(item.ID, item.Method)
	}
	c.mu.Unlock()

	j.take(item, false)

	return j
}

// newJob makes a job and keeps it among those followed. The connection's lock is held by the caller.
func (c *Conn) newJob(id int64, method string) *Job {
	j := &Job{conn: c, id: id, done: make(chan struct{}), changed: make(chan struct{}, 1), status: JobStatus{ID: id, Method: method, State: JobWaiting}}
	c.jobs[id] = j

	return j
}

// jobHeard takes in what the NAS said unasked of a job: one being followed, or one just asked for that this is the first word of.
func (c *Conn) jobHeard(fields json.RawMessage) {
	var item jobItem
	if json.Unmarshal(fields, &item) != nil || item.ID == 0 {
		return
	}

	c.mu.Lock()
	j := c.jobs[item.ID]
	for message, asked := range c.starting {
		if asked.job != nil || asked.method != item.Method || !item.askedBy(message) {
			continue
		}
		if j == nil {
			j = c.newJob(item.ID, item.Method)
		}
		asked.job = j
		close(asked.bound)
	}
	c.mu.Unlock()

	if j != nil {
		j.take(item, true)
	}
}

// jobItems asks the NAS how the jobs of these numbers stand, with what each answered as it was answered and not with its secrets taken out, which
// is how a NAS otherwise answers anyone but itself.
func (l *link) jobItems(ctx context.Context, ids []int64) ([]jobItem, error) {
	var items []jobItem
	if _, err := l.call(ctx, jobsCollection, []any{[]any{[]any{"id", "in", ids}}, map[string]any{"extra": map[string]any{"raw_result": true}}}, &items, func() {}); err != nil {
		return nil, err
	}

	return items, nil
}

// catchUp finds out how the jobs being followed stand, on a socket dialled after the one they were followed on went: whatever the NAS said in
// between was not heard. A job the NAS no longer has is lost, which is every job when it was the NAS that restarted.
func (c *Conn) catchUp(ctx context.Context, l *link) error {
	c.mu.Lock()
	following := maps.Clone(c.jobs)
	c.mu.Unlock()

	if len(following) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(following))
	for id, j := range following {
		ids = append(ids, id)

		// what was heard on the old socket is older than what this one is about to be told
		j.mu.Lock()
		j.heard = false
		j.mu.Unlock()
	}
	slices.Sort(ids)

	items, err := l.jobItems(ctx, ids)
	if err != nil {
		return err
	}

	for _, item := range items {
		if j := following[item.ID]; j != nil {
			j.take(item, false)
			delete(following, item.ID)
		}
	}
	for id, j := range following {
		j.lose(ErrJobLost)

		c.mu.Lock()
		if c.jobs[id] == j {
			delete(c.jobs, id)
		}
		c.mu.Unlock()
	}

	return nil
}

// loseJobs ends the following of every job, for the reason given.
func (c *Conn) loseJobs(why error) {
	c.mu.Lock()
	jobs := c.jobs
	c.jobs = map[int64]*Job{}
	c.mu.Unlock()

	for _, j := range jobs {
		j.lose(why)
	}
}
