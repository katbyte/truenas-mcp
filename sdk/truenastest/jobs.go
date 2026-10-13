package truenastest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// JobHandler carries out a job that says how far along it is.
type JobHandler func(run *Run, params []json.RawMessage) (any, error)

// Run is a job the stand-in is carrying out, as the function carrying it out sees it.
type Run struct {
	nas *NAS
	job *job
}

// ID is the job's number.
func (r *Run) ID() int { return r.job.id }

// Progress says how far along the job is, which whoever asked to hear of jobs is told.
func (r *Run) Progress(percent int, description string) {
	n := r.nas

	n.mu.Lock()
	if r.job.state != "RUNNING" {
		n.mu.Unlock()

		return
	}
	r.job.percent, r.job.description = percent, description
	account := r.job.account()
	n.mu.Unlock()

	n.Say("core.get_jobs", "changed", r.job.id, account)
}

// Log adds a line to what the job has logged, which can be asked for by the job's number.
func (r *Run) Log(line string) {
	r.nas.mu.Lock()
	defer r.nas.mu.Unlock()

	r.job.logged += line + "\n"
}

// Aborted is closed when the job is stopped before it is done: a function that waits on something waits on this too.
func (r *Run) Aborted() <-chan struct{} { return r.job.aborted }

// job is a job the stand-in has taken on.
type job struct {
	id      int
	method  string
	message json.RawMessage
	params  []json.RawMessage
	// done is closed when the job is over, and aborted when it is stopped before then
	done    chan struct{}
	aborted chan struct{}

	// these are guarded by the stand-in's lock
	state       string
	percent     int
	description string
	logged      string
	result      any
	failure     error
	started     time.Time
	finished    time.Time
}

// The things a NAS says of only to the connection that asked, once for each time it asked, where anything else is said once to a connection however
// often it asked.
var eventSources = []string{"reporting.realtime", "filesystem.file_tail_follow", "virt.instance.metrics", "app.container_log_follow", "app.stats"}

// Say has the stand-in say that something in a collection was "added", "changed" or "removed", to every connection that asked to hear of it: id is
// which one, and fields how it now stands.
func (n *NAS) Say(collection, kind string, id, fields any) {
	event := map[string]any{"msg": kind, "collection": collection}
	if id != nil {
		event["id"] = id
	}
	if fields != nil && kind != "removed" {
		event["fields"] = fields
	}
	source, _, _ := strings.Cut(collection, ":")

	for s, times := range n.listening(collection, slices.Contains(eventSources, source)) {
		for range times {
			s.send(s.ctx, map[string]any{"jsonrpc": "2.0", "method": "collection_update", "params": event})
		}
	}
}

// EndSubscriptions has the stand-in stop saying of a collection and tell whoever was listening so, as a NAS does when what was being followed comes
// to an end: with the error given, or with none for an end that is nothing wrong.
func (n *NAS) EndSubscriptions(collection string, why *Error) {
	params := map[string]any{"collection": collection, "error": nil}
	if why != nil {
		params["error"] = map[string]any{"error": why.Errno, "errname": why.ErrName, "reason": why.Reason, "trace": nil, "extra": why.Extra}
	}

	for s := range n.listening(collection, true) {
		s.mu.Lock()
		for ident, name := range s.subs {
			if name == collection {
				delete(s.subs, ident)
			}
		}
		s.mu.Unlock()

		s.send(s.ctx, map[string]any{"jsonrpc": "2.0", "method": "notify_unsubscribed", "params": params})
	}
}

// Subscribed is how many times the stand-in is at present asked to say of a collection, over all its connections.
func (n *NAS) Subscribed(collection string) int {
	total := 0
	for _, times := range n.listening(collection, true) {
		total += times
	}

	return total
}

// listening is the logged-in connections that asked to hear of a collection, each with how many times it is to be told: once for every time it
// asked by that very name when exact is set, and otherwise once if it asked by that name or for everything.
func (n *NAS) listening(collection string, exact bool) map[*session]int {
	n.mu.Lock()
	sessions := make([]*session, 0, len(n.sessions))
	for s := range n.sessions {
		sessions = append(sessions, s)
	}
	n.mu.Unlock()

	out := map[*session]int{}
	for _, s := range sessions {
		if !s.loggedIn.Load() {
			continue
		}

		s.mu.Lock()
		for _, name := range s.subs {
			switch {
			case name == collection && exact:
				out[s]++
			case (name == collection || name == "*") && !exact:
				out[s] = 1
			}
		}
		s.mu.Unlock()
	}

	return out
}

func (s *session) subscribe(params []json.RawMessage) (any, error) {
	var name string
	if len(params) != 1 || json.Unmarshal(params[0], &name) != nil || name == "" {
		return nil, &InvalidError{Reason: "[EINVAL] event: Field required", Invalid: []Invalid{{Attribute: "event", Message: "Field required", Errno: 22}}}
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	ident := hex.EncodeToString(raw)

	s.mu.Lock()
	s.subs[ident] = name
	s.mu.Unlock()

	return ident, nil
}

func (s *session) unsubscribe(params []json.RawMessage) {
	var ident string
	if len(params) == 1 && json.Unmarshal(params[0], &ident) == nil {
		s.mu.Lock()
		delete(s.subs, ident)
		s.mu.Unlock()
	}
}

// begin takes a job on and starts carrying it out, saying so to whoever asked to hear of jobs.
func (n *NAS) begin(method string, message json.RawMessage, params []json.RawMessage, h handler) *job {
	n.mu.Lock()
	n.lastJob++
	j := &job{id: n.lastJob, method: method, message: message, params: params, done: make(chan struct{}), aborted: make(chan struct{}), state: "WAITING", started: time.Now()}
	n.jobs[j.id] = j
	account := j.account()
	n.mu.Unlock()

	n.Say("core.get_jobs", "added", j.id, account)
	go n.carryOut(j, h)

	return j
}

// carryOut runs a job's function and records how it came out, unless the job was stopped first.
func (n *NAS) carryOut(j *job, h handler) {
	if !n.move(j, "RUNNING", nil, nil) {
		return
	}

	var result any
	var err error
	if h.long != nil {
		result, err = h.long(&Run{nas: n, job: j}, j.params)
	} else {
		result, err = h.fn(j.params)
	}

	if err != nil {
		n.move(j, "FAILED", nil, err)

		return
	}
	n.move(j, "SUCCESS", result, nil)
}

// move takes a job to another state and says so, unless it is already over, which it reports.
func (n *NAS) move(j *job, state string, result any, failure error) bool {
	n.mu.Lock()
	if j.state == "SUCCESS" || j.state == "FAILED" || j.state == "ABORTED" {
		n.mu.Unlock()

		return false
	}

	j.state, j.result, j.failure = state, result, failure
	over := state != "RUNNING"
	if over {
		j.finished = time.Now()
		if state == "SUCCESS" {
			j.percent = 100
		}
	}
	account := j.account()
	n.mu.Unlock()

	if state == "ABORTED" {
		close(j.aborted)
	}
	if over {
		close(j.done)
	}
	n.Say("core.get_jobs", "changed", j.id, account)

	return true
}

// outcome is what a call that waited for the job is answered.
func (j *job) outcome() (any, error) {
	if j.state == "ABORTED" {
		return nil, &Error{Errno: 14, ErrName: "EFAULT", Reason: "Job was aborted"}
	}

	return j.result, j.failure
}

// account is the job as a NAS writes one. The stand-in's lock is held by the caller.
func (j *job) account() map[string]any {
	asked := []any{}
	if j.message != nil {
		asked = append(asked, j.message)
	}

	account := map[string]any{
		"id": j.id, "message_ids": asked, "method": j.method, "arguments": j.params, "transient": false, "description": nil, "abortable": true,
		"logs_path": nil, "logs_excerpt": nil, "progress": map[string]any{"percent": j.percent, "description": j.description, "extra": nil},
		"result": nil, "result_encoding_error": nil, "error": nil, "exception": nil, "exc_info": nil, "state": j.state,
		"time_started": map[string]any{"$date": j.started.UnixMilli()}, "time_finished": nil,
		"credentials": map[string]any{"type": "API_KEY", "data": map[string]any{"username": DefaultUsername}},
	}
	if !j.finished.IsZero() {
		account["time_finished"] = map[string]any{"$date": j.finished.UnixMilli()}
	}

	switch {
	case j.state == "SUCCESS":
		account["result"] = j.result
	case j.failure != nil:
		account["error"] = j.failure.Error()
		account["exception"] = "Traceback (most recent call last):\n  the stand-in keeps no record of where a job failed\n"
		account["exc_info"] = map[string]any{"repr": j.failure.Error(), "type": "Exception", "errno": nil, "extra": nil}

		if said, ok := errors.AsType[*Error](j.failure); ok {
			account["error"] = "[" + said.ErrName + "] " + said.Reason
			account["exc_info"] = map[string]any{"repr": "CallError('" + said.Reason + "')", "type": "CallError", "errno": said.Errno, "extra": said.Extra}
		}
		if invalid, ok := errors.AsType[*InvalidError](j.failure); ok {
			rows := make([][]any, 0, len(invalid.Invalid))
			for _, i := range invalid.Invalid {
				rows = append(rows, i.row())
			}
			account["error"] = invalid.Reason
			account["exc_info"] = map[string]any{"repr": "ValidationErrors()", "type": "VALIDATION", "errno": nil, "extra": rows}
		}
	}

	return account
}

// queryJobs answers core.get_jobs: every job, or those a filter on the number picks out, which is as much of a NAS's filtering as the stand-in has.
func (n *NAS) queryJobs(params []json.RawMessage) (any, error) {
	var filters [][]json.RawMessage
	if len(params) > 0 && json.Unmarshal(params[0], &filters) != nil {
		return nil, &InvalidError{Reason: "[EINVAL] filters: Input should be a valid list", Invalid: []Invalid{{Attribute: "filters", Message: "Input should be a valid list", Errno: 22}}}
	}

	wanted := func(int) bool { return true }
	for _, filter := range filters {
		var field, op string
		if len(filter) != 3 || json.Unmarshal(filter[0], &field) != nil || json.Unmarshal(filter[1], &op) != nil || field != "id" {
			return nil, &Error{Errno: 22, ErrName: "EINVAL", Reason: "the stand-in filters jobs by their number only"}
		}

		var one int
		var several []int
		switch {
		case op == "=" && json.Unmarshal(filter[2], &one) == nil:
			several = []int{one}
		case op == "in" && json.Unmarshal(filter[2], &several) == nil:
		default:
			return nil, &Error{Errno: 22, ErrName: "EINVAL", Reason: "the stand-in filters jobs by = and in only"}
		}

		earlier := wanted
		wanted = func(id int) bool { return earlier(id) && slices.Contains(several, id) }
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	ids := make([]int, 0, len(n.jobs))
	for id := range n.jobs {
		if wanted(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)

	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, n.jobs[id].account())
	}

	return out, nil
}

// waitFor answers core.job_wait: what the job of a number answered, once it is over.
func (n *NAS) waitFor(ctx context.Context, params []json.RawMessage) (any, error) {
	var id int
	if len(params) != 1 || json.Unmarshal(params[0], &id) != nil {
		return nil, &InvalidError{Reason: "[EINVAL] id: Input should be a valid integer", Invalid: []Invalid{{Attribute: "id", Message: "Input should be a valid integer", Errno: 22}}}
	}

	n.mu.Lock()
	j := n.jobs[id]
	n.mu.Unlock()

	if j == nil {
		return nil, NoSuchInstance("Job with id " + strconv.Itoa(id))
	}

	select {
	case <-j.done:
		return j.outcome()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// abort stops a job that is not yet over.
func (n *NAS) abort(params []json.RawMessage) error {
	var id int
	if len(params) != 1 || json.Unmarshal(params[0], &id) != nil {
		return &InvalidError{Reason: "[EINVAL] id: Input should be a valid integer", Invalid: []Invalid{{Attribute: "id", Message: "Input should be a valid integer", Errno: 22}}}
	}

	n.mu.Lock()
	j := n.jobs[id]
	n.mu.Unlock()

	if j == nil {
		return NoSuchInstance("Job with id " + strconv.Itoa(id))
	}
	n.move(j, "ABORTED", nil, nil)

	return nil
}
