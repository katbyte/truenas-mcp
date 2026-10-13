package truenasrpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// line is one socket to the NAS that is kept up: dialled when it is first needed, and again after it goes.
type line struct {
	conn *Conn
	// events says this is the socket jobs are started on and events arrive on, and not the one calls are made on
	events bool
	// slots holds a place for every call the NAS is carrying out on this socket, so that no more are sent at once than it will run
	slots chan struct{}
	// dialling is held by the one caller that is dialling, while the others wait for what it comes back with
	dialling chan struct{}
	// link is the socket itself, nil when there is none, and is guarded by the connection's lock
	link *link
}

// call makes one call on the line's socket, dialling it first if there is none, with the wait a call is given when its context names none.
func (n *line) call(ctx context.Context, method string, params []any, into any) error {
	if method == "" {
		return errors.New("a call needs the name of a method")
	}
	c := n.conn
	if _, set := ctx.Deadline(); !set && c.o.callWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.o.callWait)
		defer cancel()
	}

	for try := 0; ; try++ {
		free, err := n.place(ctx, method)
		if err != nil {
			return err
		}

		l, err := n.connected(ctx)
		if err != nil {
			free()

			return err
		}

		sent, err := l.call(ctx, method, params, into, free)
		if err == nil {
			return nil
		}

		// a NAS that says nobody is logged in has ended the session under us: drop the socket, so that the next call logs in again
		if IsNotLoggedIn(err) {
			l.end(err, false)
		}

		// a socket that had gone before the call left is dialled again and the call made once more, since the NAS never saw it
		if sent || try > 0 || !IsDisconnected(err) || ctx.Err() != nil {
			return err
		}
	}
}

// place takes one of the places the NAS has for this socket's calls, waiting for one if they are all taken, and answers how to give it back.
func (n *line) place(ctx context.Context, method string) (free func(), err error) {
	select {
	case n.slots <- struct{}{}:
		return sync.OnceFunc(func() { <-n.slots }), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: gave up waiting behind the %d calls already with the TrueNAS: %w", method, cap(n.slots), context.Cause(ctx))
	}
}

// connected is the socket to call on, dialled and logged in if there is none.
func (n *line) connected(ctx context.Context) (*link, error) {
	c := n.conn
	if l, err := n.current(); l != nil || err != nil {
		return l, err
	}

	select {
	case n.dialling <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("gave up waiting for the TrueNAS at %s to be dialled: %w", c.address, context.Cause(ctx))
	}
	defer func() { <-n.dialling }()

	// another call may have dialled while this one waited its turn
	if l, err := n.current(); l != nil || err != nil {
		return l, err
	}

	l, err := n.dial(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		// a caller that gave up is not the NAS failing, and is not held against it
		if ctx.Err() == nil {
			c.fails++
			c.failed, c.retryAt = err, time.Now().Add(c.o.redialWait(c.fails))
			c.debugf("the TrueNAS at %s could not be used, and is left alone until %s: %v", c.address, c.retryAt.Format(time.TimeOnly), err)
		}

		return nil, err
	}
	if c.closed {
		l.end(ErrClosed, true)

		return nil, ErrClosed
	}
	n.link, c.fails, c.failed = l, 0, nil

	// whatever is being watched is watched again when this socket goes, without waiting for someone to ask
	if n.events {
		go c.keepWatching(l) //nolint:gosec,contextcheck // G118: the watch outlives the call that happened to dial, so it cannot go by that call's context
	}

	return l, nil
}

// current is the socket in use, or why none may be dialled yet: the connection is closed, or the NAS failed a moment ago and is being left alone.
// Neither means there is no socket and one may be dialled.
func (n *line) current() (*link, error) {
	c := n.conn

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, ErrClosed
	}
	if n.link != nil && n.link.ended() == nil {
		return n.link, nil
	}
	n.link = nil

	if wait := time.Until(c.retryAt); c.failed != nil && wait > 0 {
		return nil, fmt.Errorf("%w (it failed a moment ago, and is not tried again for %s)", c.failed, wait.Round(100*time.Millisecond))
	}

	// no socket, and no reason not to dial one
	return nil, nil
}

// dial opens a socket to the NAS, sets it up for what the line is for, and logs in. The socket calls are made on asks for long calls to be
// answered when they are done; the one events arrive on is subscribed again to everything that was being watched.
func (n *line) dial(ctx context.Context) (*link, error) {
	c := n.conn

	ctx, cancel := context.WithTimeout(ctx, c.o.dialWait)
	defer cancel()

	c.debugf("dialling the TrueNAS at %s", c.address)

	// the certificate the NAS shows is kept, since the newer login is tied to it
	var shown atomic.Pointer[x509.Certificate]
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(got httptrace.GotConnInfo) {
		if secure, ok := got.Conn.(*tls.Conn); ok {
			if certs := secure.ConnectionState().PeerCertificates; len(certs) > 0 {
				shown.Store(certs[0])
			}
		}
	}})

	ws, resp, err := websocket.Dial(ctx, c.url, &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: http.Header{"User-Agent": {c.o.userAgent}}}) //nolint:bodyclose // the library closes the body of a refusal, and on success it is the socket
	if err != nil {
		return nil, &ConnError{Address: c.address, Err: refused(resp, err)}
	}
	ws.SetReadLimit(c.o.maxAnswer)

	// the socket outlives the call that happened to dial it
	run, stop := context.WithCancel(context.WithoutCancel(ctx))
	l := &link{conn: c, events: n.events, ws: ws, stop: stop, gone: make(chan struct{}), waiting: map[int64]*waiter{}, idents: map[string]string{}}
	go l.read(run)
	go l.keepAlive(run)

	// a TrueNAS answers a long call with the number of a job unless told otherwise, which is not what any call's description says it answers: the
	// socket calls are made on asks for the answer, and the one jobs are started on is left answering the number
	options := map[string]any{}
	if !n.events {
		options["legacy_jobs"] = false
	}
	if c.o.private {
		options["private_methods"] = true
	}
	if len(options) > 0 {
		if _, err := l.call(ctx, "core.set_options", []any{options}, nil, func() {}); err != nil {
			l.end(err, false)

			return nil, c.unusable(err)
		}
	}

	if err := c.cred.login(ctx, &session{conn: c, link: l, shown: shown.Load(), checked: c.vouchedFor()}); err != nil {
		l.end(err, false)

		return nil, c.unusable(err)
	}

	if n.events {
		if err := c.rewire(ctx, l); err != nil {
			l.end(err, false)

			return nil, c.unusable(err)
		}
	}

	c.debugf("logged in to the TrueNAS at %s", c.address)

	return l, nil
}
