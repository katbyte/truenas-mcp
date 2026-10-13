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

// The kinds of thing a NAS says happened to what it was asked to watch, and one that is this connection's own.
const (
	Added   = "added"
	Changed = "changed"
	Removed = "removed"
	// Resumed is not the NAS's word: it says the connection went away and is back, so whatever happened in between was not heard. Whoever keeps a
	// copy of what is watched asks for the whole of it again.
	Resumed = "resumed"
)

// ErrEnded is a subscription the NAS ended of its own accord without saying anything was wrong: a log that was being followed came to its end.
var ErrEnded = errors.New("the TrueNAS ended the subscription")

// maxQueued is how many events wait for a subscription's function before the oldest are dropped.
const maxQueued = 4096

// Event is one thing a NAS said happened to something it was asked to watch.
type Event struct {
	// Collection is what it happened in: "alert.list", "pool.query".
	Collection string
	// Kind is Added, Changed or Removed, or Resumed.
	Kind string
	// ID is which one of the collection it happened to, as the NAS wrote it, a number or text; empty where the NAS names none.
	ID json.RawMessage
	// Fields is how the one it happened to now stands, for one added or changed.
	Fields json.RawMessage
	// Extra is whatever more the NAS said.
	Extra json.RawMessage
	// Missed is how many events before this one were dropped, because the function they are handed to had fallen too far behind.
	Missed int
}

// Subscription is a NAS saying what happens to one thing, until Close or until the NAS stops.
type Subscription struct {
	conn *Conn
	name string
	fn   func(Event)
	wake chan struct{}
	done chan struct{}

	mu     sync.Mutex
	queue  []Event
	missed int
	// wired says the NAS has been asked once, so that its being asked again after the connection came back is worth telling of
	wired bool
	ended bool
	err   error
}

// watch is one name the NAS has been asked to say of, and everything here that wants to hear.
type watch struct {
	subs []*Subscription
	// jobs says jobs are followed through it, which keeps it asked for with nobody subscribed
	jobs bool
}

// Subscribe has the NAS say whenever something in a collection changes ("alert.list", "pool.query", or "*" for everything it says), and hands
// each event to fn, one at a time and in the order they came. It goes on through a NAS that restarts: the connection is made again and the NAS
// asked again, and fn is handed a Resumed event for whatever was missed in between.
//
// Some of what can be watched takes an argument after a colon, as "reporting.realtime:{\"interval\":2}" does.
func (c *Conn) Subscribe(ctx context.Context, collection string, fn func(Event)) (*Subscription, error) {
	if collection == "" || fn == nil {
		return nil, errors.New("a subscription needs the name of what to watch and a function to hand each event to")
	}
	if _, set := ctx.Deadline(); !set && c.o.callWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.o.callWait)
		defer cancel()
	}

	s := &Subscription{conn: c, name: collection, fn: fn, wake: make(chan struct{}, 1), done: make(chan struct{})}

	// it is listening before the NAS is asked, so that nothing said between the asking and the answer is lost
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil, ErrClosed
	}
	w := c.watched[collection]
	if w == nil {
		w = &watch{}
		c.watched[collection] = w
	}
	w.subs = append(w.subs, s)
	c.mu.Unlock()

	go s.run()

	if err := c.wire(ctx, collection); err != nil {
		s.drop()

		return nil, fmt.Errorf("watching %s: %w", collection, err)
	}

	s.mu.Lock()
	s.wired = true
	s.mu.Unlock()

	return s, nil
}

// Collection is what the subscription watches, as it was asked for.
func (s *Subscription) Collection() string { return s.name }

// Done is closed when the subscription is over: it was closed, the connection was, or the NAS ended it.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Err is why the subscription is over when it was not Close that ended it: what the NAS said, ErrEnded when it said nothing, ErrClosed for a
// connection that was closed, or a login the NAS turned down when the connection had to be made again.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}

// Close stops the subscription. Its function is not called again once a call of it under way has returned.
func (s *Subscription) Close() error {
	if last := s.drop(); last {
		s.conn.unask(s.name)
	}

	return nil
}

// drop takes the subscription out of those listening and ends it, and reports whether it was the last listening for what it watched.
func (s *Subscription) drop() (last bool) {
	c := s.conn

	c.mu.Lock()
	if w := c.watched[s.name]; w != nil {
		w.subs = slices.DeleteFunc(slices.Clone(w.subs), func(have *Subscription) bool { return have == s })
		if len(w.subs) == 0 && !w.jobs {
			delete(c.watched, s.name)
			last = true
		}
	}
	c.mu.Unlock()

	s.end(nil)

	return last
}

// end finishes the subscription, for the reason given. Only the first end counts.
func (s *Subscription) end(why error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ended {
		return
	}
	s.ended, s.err, s.queue = true, why, nil
	close(s.done)
}

// push adds an event to those waiting for the function, dropping the oldest when it has fallen too far behind to keep them all.
func (s *Subscription) push(e Event) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()

		return
	}
	if len(s.queue) >= maxQueued {
		s.queue = s.queue[1:]
		s.missed++
	}
	s.queue = append(s.queue, e)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run hands each waiting event to the function, in order, until the subscription is over.
func (s *Subscription) run() {
	for {
		s.mu.Lock()
		queue, missed := s.queue, s.missed
		s.queue, s.missed = nil, 0
		s.mu.Unlock()

		for i, e := range queue {
			select {
			case <-s.done:
				return
			default:
			}

			if i == 0 {
				e.Missed = missed
			}
			s.fn(e)
		}
		if len(queue) > 0 {
			continue
		}

		select {
		case <-s.wake:
		case <-s.done:
			return
		}
	}
}

// wire makes sure the socket events arrive on is up and has asked the NAS to say of a name.
func (c *Conn) wire(ctx context.Context, name string) error {
	for try := 0; ; try++ {
		l, err := c.events.connected(ctx)
		if err != nil {
			return err
		}

		select {
		case c.wiring <- struct{}{}:
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting to ask the TrueNAS: %w", context.Cause(ctx))
		}
		err = l.ask(ctx, name)
		<-c.wiring

		// a socket that went in the asking is dialled again, and the dialling asks for everything watched
		if err == nil || try > 0 || !IsDisconnected(err) || ctx.Err() != nil {
			return err
		}
	}
}

// ask has the socket ask the NAS to say of a name, unless it has already.
func (l *link) ask(ctx context.Context, name string) error {
	l.mu.Lock()
	_, asked := l.idents[name]
	l.mu.Unlock()

	if asked {
		return nil
	}

	var ident string
	if _, err := l.call(ctx, "core.subscribe", []any{name}, &ident, func() {}); err != nil {
		return err
	}

	l.mu.Lock()
	l.idents[name] = ident
	l.mu.Unlock()

	return nil
}

// unask tells the NAS to stop saying of a name nothing here listens for any more. It is a kindness to the NAS and nothing to fail over: a socket
// that goes takes what it asked for with it.
func (c *Conn) unask(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	select {
	case c.wiring <- struct{}{}:
	case <-ctx.Done():
		return
	case <-c.done:
		return
	}
	defer func() { <-c.wiring }()

	// someone may have asked to watch it again in the meantime
	c.mu.Lock()
	_, again := c.watched[name]
	l := c.events.link
	c.mu.Unlock()

	if again || l == nil {
		return
	}

	l.mu.Lock()
	ident, asked := l.idents[name]
	delete(l.idents, name)
	l.mu.Unlock()

	if asked {
		_, _ = l.call(ctx, "core.unsubscribe", []any{ident}, nil, func() {})
	}
}

// rewire has a newly dialled socket ask the NAS for everything that is being watched, tells whoever was listening before that there was a gap, and
// finds out how the jobs being followed now stand.
func (c *Conn) rewire(ctx context.Context, l *link) error {
	c.mu.Lock()
	names := slices.Sorted(maps.Keys(c.watched))
	var again []*Subscription
	for _, w := range c.watched {
		for _, s := range w.subs {
			s.mu.Lock()
			if s.wired {
				again = append(again, s)
			}
			s.mu.Unlock()
		}
	}
	c.mu.Unlock()

	// the gap is told of before the NAS is asked again, so that it comes before anything the NAS then says
	for _, s := range again {
		s.push(Event{Collection: s.name, Kind: Resumed})
	}

	for _, name := range names {
		err := l.ask(ctx, name)
		if err == nil {
			continue
		}

		// a NAS that will no longer say of something, a right taken away while the connection was down, ends that and the rest carry on
		refusal, ok := errors.AsType[*Error](err)
		if !ok {
			return err
		}
		c.unwatch(l, name, refusal)
	}

	return c.catchUp(ctx, l)
}

// heard takes something the NAS said unasked: that something watched changed, or that it will say no more of something.
func (c *Conn) heard(l *link, method string, params json.RawMessage) {
	switch method {
	case "collection_update":
		var said struct {
			Kind       string          `json:"msg"`
			Collection string          `json:"collection"`
			ID         json.RawMessage `json:"id"`
			Fields     json.RawMessage `json:"fields"`
			Extra      json.RawMessage `json:"extra"`
		}
		if json.Unmarshal(params, &said) != nil || said.Collection == "" {
			return
		}

		// a job's standing is taken in here and now, in the order the NAS said it, since a job that was just started is told apart from any
		// other by what the NAS says of it before it answers
		if said.Collection == jobsCollection {
			c.jobHeard(said.Fields)
		}

		c.mu.Lock()
		var to []*Subscription
		for _, name := range []string{said.Collection, "*"} {
			if w := c.watched[name]; w != nil {
				to = append(to, w.subs...)
			}
		}
		c.mu.Unlock()

		for _, s := range to {
			s.push(Event{Collection: said.Collection, Kind: said.Kind, ID: said.ID, Fields: said.Fields, Extra: said.Extra})
		}

	case "notify_unsubscribed":
		var said struct {
			Collection string   `json:"collection"`
			Error      *failure `json:"error"`
		}
		if json.Unmarshal(params, &said) != nil || said.Collection == "" {
			return
		}

		why := ErrEnded
		if said.Error != nil {
			why = &Error{Method: said.Collection, Code: CodeCallFailed, Message: "Method call error", Errno: said.Error.Errno, ErrName: said.Error.ErrName, Reason: c.scrub(said.Error.Reason, scrubbed), Extra: json.RawMessage(c.scrub(string(said.Error.Extra), scrubbed))}
		}
		c.unwatch(l, said.Collection, why)
	}
}

// unwatch ends everything listening for a name, for the reason given, and forgets the socket ever asked.
func (c *Conn) unwatch(l *link, name string, why error) {
	c.mu.Lock()
	w := c.watched[name]
	delete(c.watched, name)
	c.mu.Unlock()

	l.mu.Lock()
	delete(l.idents, name)
	l.mu.Unlock()

	if w == nil {
		return
	}
	for _, s := range w.subs {
		s.end(why)
	}
	if w.jobs {
		c.loseJobs(why)
	}
}

// keepWatching waits for the socket events arrive on to go, and dials another while anything is still being watched, leaving a NAS that cannot
// be reached alone for as long between tries as a call would.
func (c *Conn) keepWatching(l *link) {
	select {
	case <-l.gone:
	case <-c.done:
		return
	}

	for {
		c.mu.Lock()
		idle := c.closed || len(c.watched) == 0
		c.mu.Unlock()

		if idle {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), c.o.dialWait+time.Second)
		_, err := c.events.connected(ctx)
		cancel()

		// the socket that was dialled has a wait of its own for when it goes
		if err == nil || errors.Is(err, ErrClosed) {
			return
		}

		// a login the NAS turns down is not tried over and over, since a NAS locks out an address that keeps failing: whoever was listening is
		// told why it stopped
		if _, refused := errors.AsType[*LoginError](err); refused {
			c.stopWatching(err)

			return
		}

		c.mu.Lock()
		wait := max(time.Until(c.retryAt), 50*time.Millisecond)
		c.mu.Unlock()

		select {
		case <-time.After(wait):
		case <-c.done:
			return
		}
	}
}

// stopWatching ends everything that is being watched and every job being followed, for the reason given.
func (c *Conn) stopWatching(why error) {
	c.mu.Lock()
	watched := c.watched
	c.watched = map[string]*watch{}
	c.mu.Unlock()

	for _, w := range watched {
		for _, s := range w.subs {
			s.end(why)
		}
	}
	c.loseJobs(why)
}
