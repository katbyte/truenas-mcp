package truenasrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The longest message a TrueNAS reads: one longer ends the connection for every call on it, so it is refused here before it is sent. The few methods
// that take a file's worth of data are allowed more.
const (
	maxMessage     = 64 << 10
	maxFileMessage = 2 << 20
)

// fileMethods are the methods allowed maxFileMessage.
var fileMethods = []string{"filesystem.file_receive", "failover.datastore.sql"}

// writeWait is how long a message has to leave. It is short, and its own: a caller that gives up part way through a write must not cut the socket
// off for everyone else.
const writeWait = 15 * time.Second

// link is one open socket and the calls waiting on it. A Conn has one at a time, and makes another when it goes.
type link struct {
	conn *Conn
	// events says what the NAS sends unasked on this socket is listened to
	events bool
	ws     *websocket.Conn
	// stop ends the goroutines that read the socket and keep it alive
	stop context.CancelFunc
	// gone is closed when the socket is finished with
	gone chan struct{}
	ids  atomic.Int64

	mu sync.Mutex
	// err is why the socket is finished with, nil while it is in use
	err     error
	waiting map[int64]*waiter
	// idents are the names the NAS gave this socket's subscriptions, by what each is subscribed to
	idents map[string]string
}

// waiter is a call that has been sent and not yet answered.
type waiter struct {
	method string
	answer chan reply
	// free gives back the call's place among those the NAS is carrying out, once the NAS is done with it
	free func()
}

type reply struct {
	result json.RawMessage
	err    *Error
}

// request and response are a call and its answer as JSON-RPC 2.0 writes them.
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type response struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int      `json:"code"`
		Message string   `json:"message"`
		Data    *failure `json:"data"`
	} `json:"error"`
}

// failure is what a TrueNAS says of something it could not do: its number and name for what went wrong, a sentence, and whatever more.
type failure struct {
	Errno   int             `json:"error"`
	ErrName string          `json:"errname"`
	Reason  string          `json:"reason"`
	Extra   json.RawMessage `json:"extra"`
}

// call sends one call and waits for its answer, the context or the socket's end, whichever is first. sent says whether the call left: one that did
// may have been carried out even when no answer came. free is called exactly once, when the NAS is done with the call or never had it.
func (l *link) call(ctx context.Context, method string, params []any, into any, free func()) (sent bool, err error) {
	return l.callAs(ctx, l.ids.Add(1), method, params, into, free)
}

// callAs is call under a number taken beforehand with l.ids.Add(1), for a caller that has to know the number before the call leaves.
func (l *link) callAs(ctx context.Context, id int64, method string, params []any, into any, free func()) (sent bool, err error) {
	if params == nil {
		params = []any{}
	}

	frame, err := json.Marshal(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		free()

		return false, fmt.Errorf("%s: its arguments cannot be written as JSON: %w", method, err)
	}

	limit := maxMessage
	if slices.Contains(fileMethods, method) {
		limit = maxFileMessage
	}
	if len(frame) > limit {
		free()

		return false, fmt.Errorf("%s: its arguments come to %d bytes, and a TrueNAS drops the connection for a message over %d", method, len(frame), limit)
	}

	w := &waiter{method: method, answer: make(chan reply, 1), free: free}

	l.mu.Lock()
	if l.err != nil {
		l.mu.Unlock()
		free()

		return false, l.lost(false)
	}
	l.waiting[id] = w
	l.mu.Unlock()

	l.conn.trace("->", method, frame)

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeWait)
	err = l.ws.Write(wctx, websocket.MessageText, frame)
	cancel()
	if err != nil {
		l.end(fmt.Errorf("sending %s: %w", method, err), false)

		return false, l.lost(false)
	}

	select {
	case r := <-w.answer:
		return true, l.read1(method, r, into)
	case <-ctx.Done():
		// the NAS is still carrying the call out, so its place stays taken until the answer comes or the socket goes
		return true, fmt.Errorf("%s: gave up waiting for the TrueNAS to answer: %w", method, context.Cause(ctx))
	case <-l.gone:
		// the answer may have come in the moment before
		select {
		case r := <-w.answer:
			return true, l.read1(method, r, into)
		default:
			return true, l.lost(true)
		}
	}
}

// read1 turns one answer into what the caller asked for.
func (*link) read1(method string, r reply, into any) error {
	if r.err != nil {
		return r.err
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(r.result, into); err != nil {
		return fmt.Errorf("%s: the TrueNAS's answer is not what was expected: %w", method, err)
	}

	return nil
}

// read hands every answer that arrives to the call waiting on it, until the socket ends.
func (l *link) read(ctx context.Context) {
	for {
		kind, frame, err := l.ws.Read(ctx)
		if err != nil {
			l.end(err, false)

			return
		}
		if kind != websocket.MessageText {
			continue
		}

		var msg response
		if err := json.Unmarshal(frame, &msg); err != nil || msg.ID == nil {
			// something the NAS sent unasked, or its refusal of a message it could not read: no call is waiting on either
			l.conn.trace("<-", msg.Method, frame)
			if err == nil && l.events {
				l.conn.heard(l, msg.Method, msg.Params)
			}

			continue
		}

		l.mu.Lock()
		w := l.waiting[*msg.ID]
		delete(l.waiting, *msg.ID)
		l.mu.Unlock()

		if w == nil {
			continue
		}

		l.conn.trace("<-", w.method, frame)
		w.free()

		r := reply{result: msg.Result}
		if msg.Error != nil {
			r.err = &Error{Method: w.method, Code: msg.Error.Code, Message: l.conn.scrub(msg.Error.Message, scrubbed)}
			if d := msg.Error.Data; d != nil {
				r.err.Errno, r.err.ErrName, r.err.Reason = d.Errno, d.ErrName, l.conn.scrub(d.Reason, scrubbed)
				r.err.Extra = json.RawMessage(l.conn.scrub(string(d.Extra), scrubbed))
			}
		}
		w.answer <- r
	}
}

// keepAlive asks an idle socket whether it is still there, and ends it when it does not say so: a NAS that was switched off, or a network that
// dropped the connection without telling either end, is otherwise only found out by the next call hanging.
func (l *link) keepAlive(ctx context.Context) {
	tick := time.NewTicker(l.conn.o.pingEvery)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		pctx, cancel := context.WithTimeout(ctx, l.conn.o.pingWait)
		err := l.ws.Ping(pctx)
		cancel()
		if err != nil {
			l.end(fmt.Errorf("it stopped answering: %w", err), false)

			return
		}
	}
}

// end finishes with the socket, for the reason given: every call waiting on it is told, and its place given back. A polite end says goodbye to the
// NAS first; any other just hangs up. Only the first end counts.
func (l *link) end(why error, polite bool) {
	l.mu.Lock()
	if l.err != nil {
		l.mu.Unlock()

		return
	}
	l.err = why
	waiting := l.waiting
	l.waiting = nil
	l.mu.Unlock()

	if !errors.Is(why, ErrClosed) {
		l.conn.debugf("the connection to the TrueNAS at %s ended: %v", l.conn.address, why)
	}

	if polite {
		_ = l.ws.Close(websocket.StatusNormalClosure, "")
	}
	l.stop()
	_ = l.ws.CloseNow()

	for _, w := range waiting {
		w.free()
	}
	close(l.gone)
}

// ended is why the socket is finished with, or nil while it is in use.
func (l *link) ended() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.err
}

// lost is the error for a call that got no answer because the socket ended.
func (l *link) lost(sent bool) error {
	why := l.ended()
	if errors.Is(why, ErrClosed) {
		return ErrClosed
	}
	if _, ok := errors.AsType[*LoginError](why); ok {
		return why
	}

	return &ConnError{Address: l.conn.address, Sent: sent, Err: fmt.Errorf("the connection went away: %w", why)}
}
