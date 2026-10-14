//go:build acceptance

// Package truenasrpc_test is the SDK run against a real TrueNAS: the throwaway one .scripts/testenv.sh installs in a virtual machine. The tests
// are handed the NAS as its installer left it, with nothing on it, and the first of them makes everything acceptance/layout describes, a step at a
// time, reading the NAS back after each: setting a NAS up from nothing is the hardest thing the SDK is asked to do, so it is done here and not
// in a script. It is the only place a claim about what a real NAS does is tested and not read out of its source. It runs with "make testacc",
// which makes the NAS, or "make test-live" against one already up.
package truenasrpc_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/described"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/acceptance/setup"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// nas is the test NAS as the environment describes it: where it is, the certificate it shows, and its administrator's password, which is all a
// NAS has when it comes out of its installer. Everything else the tests make.
var nas struct {
	server, certificate string
	username, password  string
}

// held is what the journey made of the NAS, for the tests after it: a key for its administrator and one for the user who may only read.
var held struct {
	mu   sync.Mutex
	made *setup.Made
}

func TestMain(m *testing.M) {
	for name, into := range map[string]*string{"TRUENAS_TEST_SERVER": &nas.server, "TRUENAS_TEST_CERTIFICATE_FILE": &nas.certificate, "TRUENAS_TEST_USERNAME": &nas.username, "TRUENAS_TEST_PASSWORD": &nas.password} {
		if into == nil {
			continue
		}
		if *into = os.Getenv(name); *into == "" {
			fmt.Fprintf(os.Stderr, "%s is not set: the live tests run against the throwaway NAS that \"make testacc\" makes, or one already up with eval \"$(make testenv-up)\"\n", name)
			os.Exit(1)
		}
	}

	// these tests make and remove things. They run against a NAS that was made to be thrown away and no other: one that already goes by a test
	// NAS's name, or one with nothing on it
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	conn, err := truenasrpc.New(nas.server, truenasrpc.Password(nas.username, nas.password), truenasrpc.WithCertificateFile(nas.certificate))
	if err == nil {
		err = (&setup.NAS{Conn: conn}).Disposable(ctx)
		_ = conn.Close()
	}
	cancel()

	if err != nil {
		fmt.Fprintf(os.Stderr, "refusing to run the live tests against the TrueNAS at %s: %v\n", nas.server, err)
		os.Exit(1)
	}

	os.Exit(run(m))
}

// run runs the tests, and closes the connections they shared when the last is done.
func run(m *testing.M) int {
	defer func() {
		shared.mu.Lock()
		defer shared.mu.Unlock()

		for _, conn := range shared.conns {
			_ = conn.Close()
		}
	}()

	return m.Run()
}

// written is a log that keeps what it is sent, for a test to read.
type written struct {
	mu    sync.Mutex
	lines []string
}

func (w *written) Debugf(format string, args ...any) { w.add(format, args...) }
func (w *written) Tracef(format string, args ...any) { w.add(format, args...) }

func (w *written) add(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	ledger.note(line)

	w.mu.Lock()
	defer w.mu.Unlock()

	w.lines = append(w.lines, line)
}

// ledger is what the whole run did to the NAS: every method called, and how often. Every connection the tests make writes its trace here, so
// how much of the API the tests reach is counted and not claimed, and every answer a real NAS gives goes through the code that hides secrets:
// a key the tests made that turns up in a line of trace is kept, and fails the run.
var ledger = &kept{called: map[string]int{}}

type kept struct {
	mu      sync.Mutex
	called  map[string]int
	secrets []string
	// showed is the start of each line of trace a secret was in, with the secret taken out
	showed []string
}

// sent finds the method in the trace of a call going out.
var sent = regexp.MustCompile(`"method":"([^"]+)"`)

func (*kept) Debugf(string, ...any) {}

func (k *kept) Tracef(format string, args ...any) { k.note(fmt.Sprintf(format, args...)) }

func (k *kept) note(line string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if call, out := strings.CutPrefix(line, "TrueNAS -> "); out {
		if method := sent.FindStringSubmatch(call); method != nil {
			k.called[method[1]]++
		}
	}

	for _, secret := range k.secrets {
		if strings.Contains(line, secret) {
			shown := strings.ReplaceAll(line, secret, "(the secret)")
			k.showed = append(k.showed, shown[:min(len(shown), 120)])
		}
	}
}

// secret names a key the tests made, which no line of trace may hold from here on. A key is a number, a dash and the secret; the number is no
// secret and turns up in every list of keys.
func (k *kept) secret(keys ...string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	for _, key := range keys {
		if _, secret, ok := strings.Cut(key, "-"); ok && secret != "" {
			k.secrets = append(k.secrets, secret)
		}
	}
}

// hide names a secret the tests were handed whole, which no line of trace may hold from here on.
func (k *kept) hide(secret string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.secrets = append(k.secrets, secret)
}

// count notes a call the trace cannot name: a file is fetched and sent by a web request, which says nothing of the method it is for.
func (k *kept) count(method string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.called[method]++
}

// took reports how many times a method was called.
func (k *kept) took(method string) int {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.called[method]
}

func (w *written) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return strings.Join(w.lines, "\n")
}

// as is a connection of the test's own to the test NAS, with the credential given, which closes when the test ends. It is for a test of
// logging in itself: a NAS takes only twenty logins a minute from one address, so everything else calls on a connection the run shares.
func as(t *testing.T, cred truenasrpc.Credential, opts ...truenasrpc.Option) *truenasrpc.Conn {
	t.Helper()

	conn, err := truenasrpc.New(nas.server, cred, append([]truenasrpc.Option{truenasrpc.WithCertificateFile(nas.certificate), truenasrpc.WithLog(ledger)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// shared are the connections the tests call on, one as each user for the whole run: logged in once and shared by everything, which is how a
// program is meant to use the SDK, and what keeps the run under the logins a NAS takes in a minute.
var shared struct {
	mu    sync.Mutex
	conns map[string]*truenasrpc.Conn
}

// sharedAs is the run's one connection as a user, made the first time it is asked for.
func sharedAs(t *testing.T, who string, cred truenasrpc.Credential) *truenasrpc.Conn {
	t.Helper()

	shared.mu.Lock()
	defer shared.mu.Unlock()

	if conn, ok := shared.conns[who]; ok {
		return conn
	}

	conn, err := truenasrpc.New(nas.server, cred, truenasrpc.WithCertificateFile(nas.certificate), truenasrpc.WithLog(ledger))
	if err != nil {
		t.Fatal(err)
	}
	if shared.conns == nil {
		shared.conns = map[string]*truenasrpc.Conn{}
	}
	shared.conns[who] = conn

	return conn
}

// root is a connection as the administrator by password, which is the only way into a NAS nothing has been made on yet.
func root(t *testing.T) *truenasrpc.Conn {
	t.Helper()

	return sharedAs(t, "the administrator, by password", truenasrpc.Password(nas.username, nas.password))
}

// made is what the journey made of the NAS. A test run by itself, with no journey before it, has the NAS set up the plain way first: the steps
// make only what is missing, so a NAS that is set up already is left as it is and given two more keys.
func made(t *testing.T) *setup.Made {
	t.Helper()

	held.mu.Lock()
	defer held.mu.Unlock()

	if held.made == nil {
		out, err := (&setup.NAS{Conn: root(t), Logf: t.Logf}).All(within(t, 20*time.Minute))
		if err != nil {
			t.Fatalf("setting the NAS up: %v", err)
		}
		held.made = out
		ledger.secret(out.Key, out.ReadOnlyKey)
	}

	return held.made
}

// furnishing is done once for the run, however many tests ask for it.
var furnishing struct {
	once sync.Once
	err  error
}

// furnished gives the NAS one of each thing a NAS is watched for (acceptance/layout says what), the first time a test asks, and leaves it
// there: a test that reads what a furnished NAS has asks for it here, so that it reads the same whether it runs alone or after the others.
func furnished(t *testing.T) {
	t.Helper()

	conn := admin(t)
	furnishing.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()

		furnishing.err = (&setup.NAS{Conn: conn, Logf: t.Logf}).Furnish(ctx)
	})
	if furnishing.err != nil {
		t.Fatalf("furnishing the NAS: %v", furnishing.err)
	}
}

// release is the version the NAS runs, as api-defs names what it keeps for it. A NAS of a version nothing is kept for cannot be held to
// anything, so the test asking stops.
func release(t *testing.T) string {
	t.Helper()

	documents, err := filepath.Glob("../../api-defs/truenas-methods-*.json")
	if err != nil || len(documents) == 0 {
		t.Fatalf("no description of the API under api-defs (%v)", err)
	}

	for _, document := range documents {
		if release := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(document), "truenas-methods-"), ".json"); strings.Contains(made(t).Version, release) {
			return release
		}
	}

	t.Fatalf("the NAS runs %s, and api-defs holds no description of that version", made(t).Version)

	return ""
}

// description is what the NAS's own version says of its API, as api-defs keeps it.
func description(t *testing.T) map[string]described.Method {
	t.Helper()

	methods, err := described.Load("../../api-defs", release(t))
	if err != nil {
		t.Fatal(err)
	}

	return methods
}

// admin is a connection as the administrator, by key, and reader one as the user who may only read. A test that wants the connection
// made its own way is given one of its own.
func admin(t *testing.T, opts ...truenasrpc.Option) *truenasrpc.Conn {
	t.Helper()

	if len(opts) > 0 {
		return as(t, truenasrpc.APIKey(nas.username, made(t).Key), opts...)
	}

	return sharedAs(t, "the administrator, by key", truenasrpc.APIKey(nas.username, made(t).Key))
}

func reader(t *testing.T) *truenasrpc.Conn {
	t.Helper()

	return sharedAs(t, "the user who may only read", truenasrpc.APIKey(layout.ReadOnlyUser, made(t).ReadOnlyKey))
}

// within is the test's context with a limit on it, so that a NAS that stops answering fails the one test and not the whole run's time.
func within(t *testing.T, limit time.Duration) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), limit)
	t.Cleanup(cancel)

	return ctx
}
