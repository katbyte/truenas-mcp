package truenasrpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/katbyte/go-kt/httpc"
)

// Caller is what a call to a TrueNAS goes through: the name of the method, its arguments in order, and where to read the answer into. A Conn is one,
// and so is whatever stands in for a NAS in a test.
type Caller interface {
	Call(ctx context.Context, method string, params []any, into any) error
}

// Conn is a connection to one TrueNAS, safe for any number of calls at once. The zero value is not usable: New makes one.
type Conn struct {
	// address is where the NAS is, as an error or a log names it, and url what is dialled: the same, but for a NAS reached over its own socket,
	// whose path is socket
	address string
	url     string
	socket  string
	cred    Credential
	o       settings
	// http opens the sockets, and files fetches and sends files, with nothing of what is in them written to a trace
	http  *http.Client
	files *http.Client

	// calls is the socket calls are made on. events is a second one, dialled only once something is watched: jobs are started on it and what the
	// NAS says unasked arrives on it, so that a job running for hours takes none of the places calls are made in
	calls  *line
	events *line
	// done is closed when the connection is, to end whatever is waiting to dial again
	done chan struct{}
	// wiring is held while what the events socket is subscribed to is being changed
	wiring chan struct{}

	mu     sync.Mutex
	closed bool
	// fails is how many times running the NAS could not be reached or would not take the login, failed is why the last time, and retryAt is when
	// it may next be tried: both sockets go by the same count, since it is the one NAS that is failing
	fails   int
	failed  error
	retryAt time.Time
	// watched is what the NAS has been asked to say of, by the name it was asked under
	watched map[string]*watch
	// jobs are the jobs being followed, by number, and starting those asked for and not yet numbered, by the number of the message that asked
	jobs     map[int64]*Job
	starting map[int64]*starting
}

// New makes a connection to the TrueNAS at an address: a host, a host and port, or a whole https address. Nothing is dialled until the first call;
// Connect dials at once, for a program that would rather learn of a wrong address or key when it starts.
func New(address string, cred Credential, opts ...Option) (*Conn, error) {
	at, socket, err := endpoint(address)
	if err != nil {
		return nil, err
	}
	if cred.invalid != nil {
		return nil, cred.invalid
	}
	if cred.login == nil {
		return nil, errors.New("a TrueNAS is logged into with an API key or a password: give APIKey, APIKeyFile or Password")
	}
	if cred.local && socket == "" {
		return nil, errors.New("LocalUser logs in over the NAS's own socket, from a program running on the NAS: give the address as unix:// and the socket's path, or an API key")
	}

	o := defaults()
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			return nil, err
		}
	}

	trust, err := o.trust()
	if err != nil {
		return nil, err
	}

	if socket != "" && o.transport != nil {
		return nil, errors.New("a connection over the NAS's own socket has no use for a transport: leave WithTransport out")
	}

	c := &Conn{address: at, url: at, socket: socket, cred: cred, o: o, http: opener(o, trust, socket, false), files: opener(o, trust, socket, true), done: make(chan struct{}), wiring: make(chan struct{}, 1), watched: map[string]*watch{}, jobs: map[int64]*Job{}, starting: map[int64]*starting{}}
	c.calls = &line{conn: c, slots: make(chan struct{}, o.maxCalls), dialling: make(chan struct{}, 1)}
	c.events = &line{conn: c, events: true, slots: make(chan struct{}, o.maxCalls), dialling: make(chan struct{}, 1)}
	if socket != "" {
		c.address = "unix://" + socket
	}

	return c, nil
}

// opener is the client the request that opens the connection is sent with, or the one files are fetched and sent with: traced like any other
// request of go-kt's, and holding the NAS's certificate to what the settings say.
func opener(o settings, trust *tls.Config, socket string, files bool) *http.Client {
	base := o.transport
	if base == nil {
		base = httpc.NewBaseTransport(httpc.Options{})
		if t, ok := base.(*http.Transport); ok {
			t.TLSClientConfig = trust

			// a NAS's own socket is dialled by its path, whatever the request says its address is
			if socket != "" {
				t.Proxy = nil
				t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer

					return d.DialContext(ctx, "unix", socket)
				}
			}
		}
	}

	// a file's contents are a saved configuration or a dataset's keys as often as not, so a trace shows that a file went and not what was in it
	traced := httpc.Options{Name: "TrueNAS", Log: o.log, SecretNames: []string{"auth_token"}}
	if files {
		traced.TraceBody = -1
	}

	return &http.Client{Transport: httpc.NewTransport(traced, base)}
}

// Address is where the connection reaches the NAS, as it was worked out from what New was given.
func (c *Conn) Address() string { return c.address }

// Connect dials the NAS and logs in now, if that has not been done, and says why not when it cannot.
func (c *Conn) Connect(ctx context.Context) error {
	_, err := c.calls.connected(ctx)

	return err
}

// Ping asks the NAS whether it is there and answering, which it says to anyone, logged in or not.
func (c *Conn) Ping(ctx context.Context) error {
	var answer string
	if err := c.Call(ctx, "core.ping", nil, &answer); err != nil {
		return err
	}
	if answer != "pong" {
		return fmt.Errorf("core.ping: the TrueNAS answered %q, where a TrueNAS answers \"pong\"", answer)
	}

	return nil
}

// Call calls a method on the NAS with its arguments in order and reads the answer into into, which may be nil for an answer nobody wants. It
// waits for the context, or DefaultCallWait when that sets no deadline. An error the NAS answered is an *Error, and one for a NAS that could not be
// reached a *ConnError: the Is functions tell them apart.
//
// A method the NAS runs as a job, a scrub or a replication, answers when the job is done, and takes one of the places calls are made in until
// then: Start is for one that runs longer than a caller wants to wait, or whose progress is wanted.
func (c *Conn) Call(ctx context.Context, method string, params []any, into any) error {
	return c.calls.call(ctx, method, params, into)
}

// Close ends the connection. Calls waiting on an answer fail, everything being watched ends, and every call after it is ErrClosed.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil
	}
	links := []*link{c.calls.link, c.events.link}
	c.calls.link, c.events.link, c.closed = nil, nil, true
	c.mu.Unlock()

	close(c.done)
	for _, l := range links {
		if l != nil {
			l.end(ErrClosed, true)
		}
	}
	c.stopWatching(ErrClosed)

	return nil
}

// vouchedFor reports whether anything checks the certificate the NAS shows: an authority, the certificate the caller handed over, or a transport of
// the caller's own that has not plainly switched its checking off.
func (c *Conn) vouchedFor() bool {
	// the NAS's own socket is reached without leaving the NAS
	if c.socket != "" {
		return true
	}
	if c.o.transport == nil {
		return !c.o.unchecked
	}

	own, ok := c.o.transport.(*http.Transport)
	if !ok || own.TLSClientConfig == nil {
		return true
	}

	return !own.TLSClientConfig.InsecureSkipVerify || own.TLSClientConfig.VerifyPeerCertificate != nil || own.TLSClientConfig.VerifyConnection != nil
}

// unusable is why a socket that was opened could not be made ready: a NAS that answered the first calls with an error is one a call cannot be
// made on, whatever the error. A login the NAS turned down stays what it is.
func (c *Conn) unusable(err error) error {
	if _, refused := errors.AsType[*LoginError](err); refused || IsDisconnected(err) {
		return err
	}

	return &ConnError{Address: c.address, Err: err}
}

// refused is why a NAS would not open a socket, with what to do about it where that is known.
func refused(resp *http.Response, err error) error {
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return fmt.Errorf("%w: its certificate is made out to another name, as the one a TrueNAS comes with is (to localhost): hand over the certificate itself, to have it accepted whatever name is on it", err)
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return fmt.Errorf("%w: its certificate is not one this machine trusts, as a TrueNAS's own is not: hand over the certificate itself, name the authority that signed it, or say the certificate is not to be checked", err)
	}
	if resp == nil {
		return httpc.Explain(httpc.RedactError(err))
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("nothing answers at that path (HTTP 404): TrueNAS serves this API from version 25.04 on: %w", err)
	case http.StatusOK:
		return fmt.Errorf("it answered a web page where the API should be: is the address a TrueNAS's, with nothing in front of it that drops websockets? %w", err)
	default:
		return fmt.Errorf("it answered HTTP %d: %w", resp.StatusCode, err)
	}
}
