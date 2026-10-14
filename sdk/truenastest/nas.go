// Package truenastest is a stand-in TrueNAS for tests: a server that speaks the API's protocol the way a real one does, answering the calls a test
// gives it answers for. What it does unasked is taken from the TrueNAS source: the login, the refusal of a call made before it, the answer of a job's
// number until a connection asks otherwise, the limits on a message's length and on how many calls run at once.
//
//	nas := truenastest.New(t)
//	nas.Answer("system.info", map[string]any{"version": "25.10.7"})
//	conn, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key()), truenasrpc.WithRootCAs(nas.Roots()))
package truenastest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// What the stand-in is logged into with unless a test says otherwise. Neither is anything's name, so a test can look for them in what a program
// writes down.
const (
	DefaultUsername = "stand-in-user"
	DefaultKey      = "7-stAnd1nKeyThatIsNobodysAndIsLongEnoughToLookFor"
)

// The limits a TrueNAS holds a connection to.
const (
	maxMessageBeforeLogin = 8192
	maxMessage            = 65536
	runAtOnce             = 10
	heldAtOnce            = 20
)

// Handler answers one call: what it returns is sent as the answer, and an *Error or *InvalidError as the refusal a NAS would send.
type Handler func(params []json.RawMessage) (any, error)

// Error is a call refused the way a TrueNAS refuses one it could not carry out.
type Error struct {
	Errno   int
	ErrName string
	Reason  string
	Extra   any
}

func (e *Error) Error() string { return e.Reason }

// InvalidError is a call refused for its arguments, each with why.
type InvalidError struct {
	Reason  string
	Invalid []Invalid
}

// Invalid is one argument refused.
type Invalid struct {
	Attribute string
	Message   string
	Errno     int
}

func (e *InvalidError) Error() string { return e.Reason }

// Refusals a test is likely to want, as a NAS words them.
var (
	ErrNotAuthorized = &Error{Errno: 13, ErrName: "EACCES", Reason: "Not authorized"}
	ErrNotFound      = &Error{Errno: 2, ErrName: "ENOENT", Reason: "the thing asked for does not exist"}
	// ErrNoMatch is how a query told to answer the one row it matches is refused when none does: a real NAS names nothing but the
	// exception, and not ENOENT.
	ErrNoMatch = &Error{Errno: 22, ErrName: "EINVAL", Reason: "MatchNotFound()"}
)

// NoSuchInstance is the refusal a TrueNAS gives a call that asks, by id or name, for something it has not got: what is "Pool 7", "Job with id
// 12". A real NAS does not say this as a failure of the call. It refuses the argument, naming none, with the number for "not there" beside it,
// which is what this is.
func NoSuchInstance(what string) *InvalidError {
	return &InvalidError{Reason: "[ENOENT] None: " + what + " does not exist", Invalid: []Invalid{{Message: what + " does not exist", Errno: 2}}}
}

// row is one refused argument as a NAS writes it: an argument with no name is written as nothing, not as an empty name.
func (i Invalid) row() []any {
	if i.Attribute == "" {
		return []any{nil, i.Message, i.Errno}
	}

	return []any{i.Attribute, i.Message, i.Errno}
}

// Call is one call the stand-in was sent.
type Call struct {
	Method string
	Params []json.RawMessage
}

// NAS is a stand-in TrueNAS. New starts one, and it stops when the test ends.
type NAS struct {
	server   *httptest.Server
	username string
	key      string

	mu       sync.Mutex
	handlers map[string]handler
	calls    []Call
	sockets  map[*websocket.Conn]context.CancelFunc
	dials    int
	logins   int
	// sessions are the connections the stand-in has, jobs every job it has taken on since it last restarted, by number, and lastJob the number of
	// the newest: a restart forgets the jobs and not the numbers already given out
	sessions map[*session]struct{}
	jobs     map[int]*job
	lastJob  int
	// keys are the keys added since the stand-in was made, each with the user it is for
	keys map[string]string
	// tokens each let one web request in, to fetch the file of the job they are for or to upload when that is 0, and files are what jobs have
	// made and not yet handed over
	tokens  map[string]int
	files   map[int]*file
	revoked bool
	down    bool
	frozen  chan struct{}

	// password is what the stand-in's user logs in with beside the key, and otp the one-time password it asks for after it, if any
	password string
	otp      string
	// socket is the path the stand-in listens at when it is reached as a NAS's own programs reach it, and stranger says it does not know whoever
	// connects there
	socket   string
	onSocket bool
	stranger bool
	// newer says the stand-in takes the login that never sends the key, as a TrueNAS from version 26 does; impostor that it lets that login in
	// without holding the key, as something answering in a NAS's place would
	newer    bool
	impostor bool
	// salt and rounds are what the key was stretched with, and stored and proving the two parts of it a NAS keeps: one to check a caller's proof
	// against, one to prove itself with
	salt    []byte
	rounds  int
	client  []byte
	stored  []byte
	proving []byte
	// most is the most calls any one connection had with the stand-in at a time, running or waiting to
	most atomic.Int64
}

type handler struct {
	fn  Handler
	job bool
	// long is the function of a job that says how far along it is, in place of fn
	long JobHandler
	// download makes the file of a method that hands one back, and upload takes the files of one that is handed them
	download func(params []json.RawMessage) ([]byte, error)
	upload   func(params []json.RawMessage, files [][]byte) (any, error)
}

// Option changes how a stand-in is made.
type Option func(*NAS)

// WithCertificate has the stand-in show this certificate in place of the usual one, which is made out to 127.0.0.1, the address a stand-in is
// dialled at. SelfSigned makes one like the certificate a TrueNAS comes with.
func WithCertificate(cert tls.Certificate) Option {
	return func(n *NAS) {
		n.server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
}

// DefaultPassword is the password of the stand-in's user unless a test says otherwise.
const DefaultPassword = "stAnd1nPasswordThatIsNobodys"

// Version26 has the stand-in take the login a TrueNAS takes from version 26 on, in which the key is proved and never sent, beside the older ones.
// Without it the stand-in is a 25.10: it takes a key only as it is, and cannot even say which logins it takes until someone is logged in.
func Version26() Option {
	return func(n *NAS) { n.newer = true }
}

// Impostor has the stand-in let the newer login in without holding the key, and answer with a proof it made up: what something answering in a
// NAS's place would have to do.
func Impostor() Option {
	return func(n *NAS) { n.newer, n.impostor = true, true }
}

// OnSocket has the stand-in listen on a socket in place of the network, as a NAS does for the programs that run on it: whoever connects is logged
// in as they connect, with nothing sent. Address is then unix:// and the socket's path.
func OnSocket() Option {
	return func(n *NAS) { n.onSocket = true }
}

// Stranger has a stand-in that listens on a socket know nobody who connects to it, as a NAS does not know a user it has no account for.
func Stranger() Option {
	return func(n *NAS) { n.onSocket, n.stranger = true, true }
}

// WithUsername sets the name of the stand-in's user, for a test of something that goes by a particular one.
func WithUsername(username string) Option {
	return func(n *NAS) { n.username = username }
}

// WithPassword sets the password of the stand-in's user.
func WithPassword(password string) Option {
	return func(n *NAS) { n.password = password }
}

// WithOneTimePassword has the stand-in ask for this one-time password after the password, as an account with two-factor login does.
func WithOneTimePassword(token string) Option {
	return func(n *NAS) { n.otp = token }
}

// SelfSigned makes a certificate signed with its own key and made out to one name, good from an hour ago until the time given: the certificate a
// TrueNAS comes with is such a one, made out to "localhost". A nil key makes a new key; the key of an earlier certificate (its PrivateKey) makes a
// renewal of it.
func SelfSigned(tb testing.TB, key *ecdsa.PrivateKey, name string, until time.Time) tls.Certificate {
	tb.Helper()

	if key == nil {
		made, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			tb.Fatal(err)
		}
		key = made
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		tb.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	if !until.After(template.NotBefore) {
		template.NotBefore = until.Add(-time.Hour)
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		tb.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatal(err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// New starts a stand-in NAS that stops when the test ends.
func New(tb testing.TB, opts ...Option) *NAS {
	tb.Helper()

	n := &NAS{username: DefaultUsername, key: DefaultKey, password: DefaultPassword, handlers: map[string]handler{}, sockets: map[*websocket.Conn]context.CancelFunc{}, sessions: map[*session]struct{}{}, jobs: map[int]*job{}, tokens: map[string]int{}, files: map[int]*file{}, keys: map[string]string{}}
	n.server = httptest.NewUnstartedServer(http.HandlerFunc(n.serve))
	for _, opt := range opts {
		opt(n)
	}

	// the key is stretched once, as a NAS does when a key is made, with the fewest rounds a NAS uses so that a test's login is quick
	n.salt, n.rounds = make([]byte, 16), 50_000
	if _, err := rand.Read(n.salt); err != nil {
		tb.Fatal(err)
	}
	_, secret, _ := strings.Cut(n.key, "-")
	salted, err := pbkdf2.Key(sha512.New, secret, n.salt, n.rounds, sha512.Size)
	if err != nil {
		tb.Fatal(err)
	}
	n.client = sum(salted, "Client Key")
	stored := sha512.Sum512(n.client)
	n.stored, n.proving = stored[:], sum(salted, "Server Key")
	// a test of a certificate that is not trusted has the server say so, which is the test passing and not something to print
	n.server.Config.ErrorLog = log.New(io.Discard, "", 0)

	if n.onSocket {
		// a socket's path may only be about a hundred characters, and a test's own temporary folder, which is named after the test, runs longer
		dir, err := os.MkdirTemp("", "nas") //nolint:usetesting // tb.TempDir() is too long a path for a socket
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = os.RemoveAll(dir) })

		n.socket = filepath.Join(dir, "middlewared.sock")
		listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", n.socket)
		if err != nil {
			tb.Fatal(err)
		}
		_ = n.server.Listener.Close()
		n.server.Listener = listener
		n.server.Start()
	} else {
		n.server.StartTLS()
	}
	tb.Cleanup(func() {
		n.Drop()
		n.server.Close()
	})

	return n
}

// InTheMiddle starts something that sits between a caller and the stand-in, and answers its address: it shows a certificate of its own and passes
// every message along, each way, as something that had got onto the network between the two would. It stops when the test ends.
func InTheMiddle(tb testing.TB, nas *NAS) (address string) {
	tb.Helper()

	relay := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		near, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = near.CloseNow() }()

		ctx, stop := context.WithCancel(r.Context())
		defer stop()

		far, _, err := websocket.Dial(ctx, nas.Address()+r.URL.Path, &websocket.DialOptions{HTTPClient: nas.server.Client()}) //nolint:bodyclose // the library closes the body of a refusal, and on success it is the socket
		if err != nil {
			return
		}
		defer func() { _ = far.CloseNow() }()

		near.SetReadLimit(4 << 20)
		far.SetReadLimit(4 << 20)

		pass := func(from, to *websocket.Conn) {
			defer stop()

			for {
				kind, frame, err := from.Read(ctx)
				if err != nil || to.Write(ctx, kind, frame) != nil {
					return
				}
			}
		}
		go pass(far, near)
		pass(near, far)
	}))
	relay.TLS = &tls.Config{Certificates: []tls.Certificate{SelfSigned(tb, nil, "127.0.0.1", time.Now().AddDate(1, 0, 0))}, MinVersion: tls.VersionTLS12}
	relay.Config.ErrorLog = log.New(io.Discard, "", 0)
	relay.StartTLS()
	tb.Cleanup(relay.Close)

	return relay.URL
}

// Address is where the stand-in listens: an https address, or unix:// and a path for one that listens on a socket.
func (n *NAS) Address() string {
	if n.socket != "" {
		return "unix://" + n.socket
	}

	return n.server.URL
}

// Roots is the authority that vouches for the stand-in's certificate: a connection trusts it the way a real one is told of a NAS's own.
func (n *NAS) Roots() *x509.CertPool {
	roots := x509.NewCertPool()
	if cert := n.server.Certificate(); cert != nil {
		roots.AddCert(cert)
	}

	return roots
}

// Certificate is the certificate the stand-in shows, nil for one that listens on a socket and shows none.
func (n *NAS) Certificate() *x509.Certificate { return n.server.Certificate() }

// CertificatePEM is the stand-in's certificate as a PEM file holds one, for a program that is told of a NAS's certificate by file.
func (n *NAS) CertificatePEM() []byte {
	cert := n.server.Certificate()
	if cert == nil {
		return nil
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// KeyFile is the stand-in's key as a key file holds one that does not hold the key itself: its number and the parts worked out of it, which are
// enough for the newer login and no use for the older ones.
func (n *NAS) KeyFile() string {
	number, _, _ := strings.Cut(n.key, "-")
	part := base64.StdEncoding.EncodeToString

	return `{"api_key_id": ` + number + `, "client_key": "` + part(n.client) + `", "stored_key": "` + part(n.stored) + `", "server_key": "` + part(n.proving) + `"}`
}

// Password is the password of the stand-in's user.
func (n *NAS) Password() string { return n.password }

// AddKey has the stand-in take another key, for the user named, beside the one it starts with: as a NAS does once a key is made on it. Such a
// key logs in the older way, by being sent.
func (n *NAS) AddKey(username, key string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.keys[key] = username
}

// Username and Key are what the stand-in is logged into with.
func (n *NAS) Username() string { return n.username }
func (n *NAS) Key() string      { return n.key }

// Handle has a function answer every call of a method.
func (n *NAS) Handle(method string, fn Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.handlers[method] = handler{fn: fn}
}

// Answer has every call of a method answered with a value, written as JSON. A json.RawMessage is sent as it is.
func (n *NAS) Answer(method string, result any) {
	n.Handle(method, func([]json.RawMessage) (any, error) { return result, nil })
}

// Refuse has every call of a method refused with an error, an *Error or an *InvalidError.
func (n *NAS) Refuse(method string, err error) {
	n.Handle(method, func([]json.RawMessage) (any, error) { return nil, err })
}

// Job has a function answer a method that a TrueNAS runs as a job: a connection that has not asked otherwise is answered the job's number at once,
// as a real one is, and one that has is answered what the function returns. Either way the job is carried out, can be asked after by its number,
// and is said of to whoever asked to hear of jobs.
func (n *NAS) Job(method string, fn Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.handlers[method] = handler{fn: fn, job: true}
}

// LongJob is Job for a function that says how far along it is, and can be told it has been stopped.
func (n *NAS) LongJob(method string, fn JobHandler) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.handlers[method] = handler{long: fn, job: true}
}

// Calls is every call the stand-in was sent, in order, or only those of the methods named. The logins are among them, with the key they sent.
func (n *NAS) Calls(methods ...string) []Call {
	n.mu.Lock()
	defer n.mu.Unlock()

	var out []Call
	for _, c := range n.calls {
		if len(methods) == 0 || slices.Contains(methods, c.Method) {
			out = append(out, c)
		}
	}

	return out
}

// Dials is how many times the stand-in was connected to, and Logins how many of those logged in.
func (n *NAS) Dials() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.dials
}

func (n *NAS) Logins() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.logins
}

// MostAtOnce is the most calls a connection had with the stand-in at one time, those being answered and those waiting their turn.
func (n *NAS) MostAtOnce() int { return int(n.most.Load()) }

// Drop cuts every open connection without a word, as a NAS that restarts does. New connections are still taken.
func (n *NAS) Drop() {
	n.mu.Lock()
	sockets := n.sockets
	n.sockets = map[*websocket.Conn]context.CancelFunc{}
	n.mu.Unlock()

	for ws, stop := range sockets {
		stop()
		_ = ws.CloseNow()
	}
}

// Restart cuts every open connection and forgets every job, as a NAS whose middleware restarts does: the jobs it had are gone, and the numbers it
// gave them are not used again.
func (n *NAS) Restart() {
	n.mu.Lock()
	n.jobs = map[int]*job{}
	n.mu.Unlock()

	n.Drop()
}

// Down stops the stand-in taking connections, and cuts those it has, until Up: a NAS that is rebooting.
func (n *NAS) Down() {
	n.mu.Lock()
	n.down = true
	n.mu.Unlock()

	n.Drop()
}

func (n *NAS) Up() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.down = false
}

// Freeze stops the stand-in reading anything it is sent, without closing a connection, until the function it returns is called: a NAS that has
// hung, or a network that has stopped carrying the connection without telling either end.
func (n *NAS) Freeze() (thaw func()) {
	frozen := make(chan struct{})

	n.mu.Lock()
	n.frozen = frozen
	n.mu.Unlock()

	return sync.OnceFunc(func() {
		n.mu.Lock()
		n.frozen = nil
		n.mu.Unlock()

		close(frozen)
	})
}

// Revoke has the stand-in turn the key away from now on, as a NAS does one that has expired or been revoked.
func (n *NAS) Revoke() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.revoked = true
}

// session is one connection's standing with the stand-in.
type session struct {
	nas *NAS
	ws  *websocket.Conn
	// ctx ends when the connection does
	ctx context.Context //nolint:containedctx // a connection's context is the connection's for as long as it lasts
	// subs is what the connection has asked to hear of, by the name the stand-in gave each asking
	subs map[string]string
	// loggedIn and waitsForJobs are read by every call the connection has running
	loggedIn     atomic.Bool
	waitsForJobs atomic.Bool
	// mu guards a login that takes more than one call: the newer login's exchange, and a password waiting on its one-time password
	mu       sync.Mutex
	exchange *exchange
	wantsOTP bool
	// running holds a place for each call being answered, and held counts those and the ones waiting for a place
	running chan struct{}
	held    atomic.Int64
}

func (n *NAS) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	down := n.down
	n.mu.Unlock()

	if down {
		http.Error(w, "the stand-in NAS is down", http.StatusServiceUnavailable)

		return
	}
	// files go to and from a NAS by ordinary web requests, beside the socket
	if strings.HasPrefix(r.URL.Path, "/_download/") {
		n.serveDownload(w, r)

		return
	}
	if r.URL.Path == "/_upload" {
		n.serveUpload(w, r)

		return
	}
	if r.URL.Path != "/api/current" && !strings.HasPrefix(r.URL.Path, "/api/v") {
		http.NotFound(w, r)

		return
	}

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(4 << 20)

	ctx, stop := context.WithCancel(r.Context())
	defer stop()

	s := &session{nas: n, ws: ws, ctx: ctx, subs: map[string]string{}, running: make(chan struct{}, runAtOnce)}

	// a NAS can see who is at the other end of its own socket, and logs the connection in as it opens
	local := n.socket != "" && !n.stranger
	s.loggedIn.Store(local)

	n.mu.Lock()
	n.dials++
	if local {
		n.logins++
	}
	n.sockets[ws] = stop
	n.sessions[s] = struct{}{}
	n.mu.Unlock()

	s.read(ctx)

	n.mu.Lock()
	delete(n.sockets, ws)
	delete(n.sessions, s)
	n.mu.Unlock()

	_ = ws.CloseNow()
}

// read takes each message a connection sends until it ends, answering every call on its own so that a slow one holds up nothing else.
func (s *session) read(ctx context.Context) {
	for {
		s.nas.mu.Lock()
		frozen := s.nas.frozen
		s.nas.mu.Unlock()

		if frozen != nil {
			select {
			case <-frozen:
			case <-ctx.Done():
				return
			}
		}

		kind, frame, err := s.ws.Read(ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			_ = s.ws.Close(websocket.StatusUnsupportedData, "Invalid websocket message type")

			return
		}

		if !s.loggedIn.Load() && len(frame) > maxMessageBeforeLogin {
			_ = s.ws.Close(websocket.StatusInvalidFramePayloadData, "Anonymous connection max message length is 8 kB")

			return
		}
		if len(frame) > maxMessage {
			_ = s.ws.Close(websocket.StatusMessageTooBig, "Max message length is 64 kB")

			return
		}

		var msg struct {
			JSONRPC string            `json:"jsonrpc"`
			ID      json.RawMessage   `json:"id"`
			Method  string            `json:"method"`
			Params  []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &msg); err != nil {
			s.refuse(ctx, json.RawMessage("null"), -32700, err.Error(), nil)

			continue
		}
		if msg.JSONRPC != "2.0" || msg.Method == "" {
			s.refuse(ctx, msg.ID, -32600, "'jsonrpc' member must be exactly '2.0' and 'method' a string", nil)

			continue
		}

		go s.answer(ctx, msg.ID, msg.Method, msg.Params)
	}
}

// answer carries one call out and sends what it came to.
func (s *session) answer(ctx context.Context, id json.RawMessage, method string, params []json.RawMessage) {
	n := s.nas

	n.mu.Lock()
	n.calls = append(n.calls, Call{Method: method, Params: params})
	h, known := n.handlers[method]
	n.mu.Unlock()

	held := s.held.Add(1)
	for {
		most := n.most.Load()
		if held <= most || n.most.CompareAndSwap(most, held) {
			break
		}
	}
	if held > heldAtOnce {
		s.held.Add(-1)
		s.refuse(ctx, id, -32000, "Maximum number of concurrent calls (20) has exceeded", nil)

		return
	}

	select {
	case s.running <- struct{}{}:
	case <-ctx.Done():
		s.held.Add(-1)

		return
	}

	// a NAS gives a call's place back before it sends the answer, so whoever is answered may send the next call at once and find room for it
	done := sync.OnceFunc(func() {
		<-s.running
		s.held.Add(-1)
	})
	defer done()

	var result any
	var err error

	switch {
	case method == "core.ping":
		result = "pong"
	case method == "core.set_options":
		result = s.setOptions(params)
	case method == "auth.mechanism_choices":
		result, err = s.mechanisms()
	case method == "auth.login_ex", method == "auth.login_ex_continue", method == "auth.login_with_api_key":
		result, err = s.login(method, params)
	case !s.loggedIn.Load():
		err = &Error{Errno: 207, ErrName: "ENOTAUTHENTICATED", Reason: "Not authenticated"}
	case !known && method == "auth.me":
		result = map[string]any{"pw_name": n.username, "pw_uid": 950, "privilege": map[string]any{"roles": []string{"FULL_ADMIN"}}}
	case !known && method == "auth.generate_token":
		result, err = n.token(0)
	case !known && method == "core.download":
		result, err = n.download(id, params)
	case !known && method == "core.job_download_logs":
		result, err = n.jobLogs(id, params)
	case !known && method == "core.job_wait" && s.waitsForJobs.Load():
		result, err = n.waitFor(ctx, params)
	case !known && method == "core.job_wait":
		// waiting for a job is a job itself, so a connection that has not asked to wait is answered the number of the waiting
		result = n.begin(method, id, params, handler{fn: func(asked []json.RawMessage) (any, error) { return n.waitFor(s.ctx, asked) }}).id
	case !known && method == "core.subscribe":
		result, err = s.subscribe(params)
	case !known && method == "core.unsubscribe":
		s.unsubscribe(params)
	case !known && method == "core.get_jobs":
		result, err = n.queryJobs(params)
	case !known && method == "core.job_abort":
		err = n.abort(params)
	case !known:
		done()
		s.refuse(ctx, id, -32601, "Method does not exist", nil)

		return
	case h.download != nil || h.upload != nil:
		err = &Error{Errno: 22, ErrName: "EINVAL", Reason: "This method takes or hands back a file, and is called through the upload or download address"}
	case h.job:
		// a job is taken on and carried out whoever is waiting; a connection that has not asked to wait is answered its number, as a NAS answers
		// unless told otherwise
		j := n.begin(method, id, params, h)
		if !s.waitsForJobs.Load() {
			result = j.id

			break
		}

		select {
		case <-j.done:
			result, err = j.outcome()
		case <-ctx.Done():
			return
		}
	default:
		result, err = h.fn(params)
	}
	done()

	if err == nil {
		s.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})

		return
	}

	if invalid, ok := errors.AsType[*InvalidError](err); ok {
		rows := make([][]any, 0, len(invalid.Invalid))
		for _, i := range invalid.Invalid {
			rows = append(rows, i.row())
		}
		s.refuse(ctx, id, -32602, "Invalid params", map[string]any{"error": 22, "errname": "EINVAL", "reason": invalid.Reason, "trace": nil, "extra": rows})

		return
	}

	said := &Error{Errno: 22, ErrName: "EINVAL", Reason: err.Error()}
	if e, ok := errors.AsType[*Error](err); ok {
		said = e
	}
	s.refuse(ctx, id, -32001, "Method call error", map[string]any{"error": said.Errno, "errname": said.ErrName, "reason": said.Reason, "trace": nil, "extra": said.Extra})
}

// setOptions takes the options a connection sets for itself, and answers how it now stands.
func (s *session) setOptions(params []json.RawMessage) any {
	var options struct {
		LegacyJobs *bool `json:"legacy_jobs"`
	}
	if len(params) > 0 && json.Unmarshal(params[0], &options) == nil && options.LegacyJobs != nil {
		s.waitsForJobs.Store(!*options.LegacyJobs)
	}

	return map[string]any{"legacy_jobs": !s.waitsForJobs.Load(), "private_methods": false, "py_exceptions": false}
}

// mechanisms answers which logins the stand-in takes. A 25.10 cannot say until someone is logged in: asked before, it fails on the session it has
// not got yet, which is how a real one of that version answers.
func (s *session) mechanisms() (any, error) {
	if s.nas.newer {
		return []string{"API_KEY_PLAIN", "PASSWORD_PLAIN", "TOKEN_PLAIN", "OTP_TOKEN", "SCRAM"}, nil
	}
	if !s.loggedIn.Load() {
		return nil, &Error{Errno: 22, ErrName: "EINVAL", Reason: "'NoneType' object has no attribute 'may_create_auth_token'"}
	}

	return []string{"API_KEY_PLAIN", "PASSWORD_PLAIN", "TOKEN_PLAIN", "OTP_TOKEN"}, nil
}

// login answers each of the ways of logging in, as a NAS does: a word for how it went, or yes or no for the oldest call.
func (s *session) login(method string, params []json.RawMessage) (any, error) {
	n := s.nas

	var sent struct {
		Mechanism string `json:"mechanism"`
		Username  string `json:"username"`
		APIKey    string `json:"api_key"`
		Password  string `json:"password"`
		OTP       string `json:"otp_token"`
		ScramType string `json:"scram_type"`
		RFC       string `json:"rfc_str"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params[0], &sent)
	}

	n.mu.Lock()
	revoked := n.revoked
	n.mu.Unlock()

	taken := func() any {
		n.mu.Lock()
		n.logins++
		n.mu.Unlock()
		s.loggedIn.Store(true)

		return map[string]any{"response_type": "SUCCESS", "user_info": nil, "authenticator": "LEVEL_1"}
	}
	refused := map[string]any{"response_type": "AUTH_ERR"}

	switch {
	case method == "auth.login_with_api_key":
		var key string
		if len(params) > 0 {
			_ = json.Unmarshal(params[0], &key)
		}
		ok := !revoked && (key == n.key || n.added(key) != "")
		if ok {
			taken()
		}

		return ok, nil

	case method == "auth.login_ex_continue":
		s.mu.Lock()
		waiting := s.wantsOTP
		s.wantsOTP = false
		s.mu.Unlock()

		if waiting && sent.Mechanism == "OTP_TOKEN" && sent.OTP == n.otp {
			return taken(), nil
		}

		return refused, nil

	case sent.Mechanism == "API_KEY_PLAIN":
		switch {
		case revoked:
			return map[string]any{"response_type": "EXPIRED"}, nil
		case sent.Username == n.username && sent.APIKey == n.key, sent.Username != "" && n.added(sent.APIKey) == sent.Username:
			return taken(), nil
		default:
			return refused, nil
		}

	case sent.Mechanism == "PASSWORD_PLAIN":
		if sent.Username != n.username || sent.Password != n.password {
			return refused, nil
		}
		if n.otp != "" {
			s.mu.Lock()
			s.wantsOTP = true
			s.mu.Unlock()

			return map[string]any{"response_type": "OTP_REQUIRED", "username": n.username}, nil
		}

		return taken(), nil

	case sent.Mechanism == "SCRAM" && n.newer:
		if sent.ScramType == "CLIENT_FIRST_MESSAGE" {
			return s.challenge(sent.RFC, revoked), nil
		}

		answer, err := s.prove(sent.RFC)
		if err == nil && answer["scram_type"] == "SERVER_FINAL_RESPONSE" {
			taken()
		}

		return answer, err

	default:
		return nil, &InvalidError{Reason: "[EINVAL] login_data.mechanism: Input tag '" + sent.Mechanism + "' found using 'mechanism' does not match any of the expected tags", Invalid: []Invalid{{Attribute: "login_data.mechanism", Message: "Input tag does not match any of the expected tags", Errno: 22}}}
	}
}

// added is the user a key added since the stand-in was made is for, or nothing for a key it has not got.
func (n *NAS) added(key string) string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.keys[key]
}

// exchange is a newer login part way through: what the caller opened with, and what the stand-in answered.
type exchange struct {
	header      string
	bare        string
	serverFirst string
	combined    []byte
}

// challenge answers the message that opens the newer login: the caller's nonce with the stand-in's own half added, and what the key was stretched
// with. A user or a key it does not have is turned away here, as a NAS turns one away.
func (s *session) challenge(first string, revoked bool) map[string]any {
	n := s.nas
	refused := map[string]any{"response_type": "AUTH_ERR"}

	header, bare, ok := strings.Cut(first, ",,")
	if !ok || header != "n" && header != "p=tls-server-end-point" {
		return refused
	}

	var name, nonce string
	for part := range strings.SplitSeq(bare, ",") {
		switch key, value, _ := strings.Cut(part, "="); key {
		case "n":
			name = value
		case "r":
			nonce = value
		}
	}

	number, _, _ := strings.Cut(n.key, "-")
	theirs, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil || len(theirs) != 32 || revoked || name != n.username+":"+number {
		return refused
	}

	ours := make([]byte, 32)
	if _, err := rand.Read(ours); err != nil {
		return refused
	}

	x := &exchange{header: header, bare: bare, combined: append(theirs, ours...)}
	x.serverFirst = "r=" + base64.StdEncoding.EncodeToString(x.combined) + ",s=" + base64.StdEncoding.EncodeToString(n.salt) + ",i=" + strconv.Itoa(n.rounds)

	s.mu.Lock()
	s.exchange = x
	s.mu.Unlock()

	return map[string]any{"response_type": "SCRAM_RESPONSE", "scram_type": "SERVER_FIRST_RESPONSE", "rfc_str": x.serverFirst, "user_info": nil}
}

// prove checks the caller's proof that it holds the key, and that the login is tied to the certificate the stand-in showed where the caller said
// it would be, and answers with the stand-in's own proof.
func (s *session) prove(final string) (map[string]any, error) {
	n := s.nas
	refused := map[string]any{"response_type": "AUTH_ERR"}

	s.mu.Lock()
	x := s.exchange
	s.exchange = nil
	s.mu.Unlock()

	if x == nil {
		return nil, &Error{Errno: 14, ErrName: "EFAULT", Reason: "SCRAM authentication is not in progress"}
	}

	unproven, proof, ok := strings.Cut(final, ",p=")
	if !ok {
		return refused, nil
	}
	// a proof that is not base64 comes out the wrong length, or wrong, and is refused below as any other wrong proof is
	sentProof, _ := base64.StdEncoding.DecodeString(proof)

	tied := []byte(x.header + ",,")
	if x.header != "n" {
		// the hash every certificate a stand-in shows is signed with
		shown := sha256.Sum256(n.server.Certificate().Raw)
		tied = append(tied, shown[:]...)
	}
	if unproven != "c="+base64.StdEncoding.EncodeToString(tied)+",r="+base64.StdEncoding.EncodeToString(x.combined) {
		return refused, nil
	}

	signed := x.bare + "," + x.serverFirst + "," + unproven
	if n.impostor {
		return map[string]any{"response_type": "SCRAM_RESPONSE", "scram_type": "SERVER_FINAL_RESPONSE", "rfc_str": "v=" + base64.StdEncoding.EncodeToString(sum([]byte("not the key"), signed)), "user_info": nil}, nil
	}

	// the proof is the caller's key with a signature folded over it: unfolded, it must be the key the stored part was made from
	signature := sum(n.stored, signed)
	if len(sentProof) != len(signature) {
		return refused, nil
	}
	key := make([]byte, len(signature))
	subtle.XORBytes(key, sentProof, signature)
	if hashed := sha512.Sum512(key); !hmac.Equal(hashed[:], n.stored) {
		return refused, nil
	}

	return map[string]any{"response_type": "SCRAM_RESPONSE", "scram_type": "SERVER_FINAL_RESPONSE", "rfc_str": "v=" + base64.StdEncoding.EncodeToString(sum(n.proving, signed)), "user_info": nil}, nil
}

func sum(key []byte, message string) []byte {
	h := hmac.New(sha512.New, key)
	h.Write([]byte(message))

	return h.Sum(nil)
}

func (s *session) refuse(ctx context.Context, id json.RawMessage, code int, message string, data any) {
	failure := map[string]any{"code": code, "message": message}
	if data != nil {
		failure["data"] = data
	}

	s.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "error": failure})
}

func (s *session) send(ctx context.Context, message any) {
	frame, err := json.Marshal(message)
	if err != nil {
		frame = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Failed to JSON serialize server message"}}`)
	}

	_ = s.ws.Write(ctx, websocket.MessageText, frame)
}
