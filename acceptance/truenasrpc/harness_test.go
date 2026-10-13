//go:build acceptance

// Package truenasrpc_test is the connection run against a real TrueNAS: the throwaway one .scripts/testenv.sh makes in a virtual machine and sets up
// with what acceptance/layout says. It is the only place a claim about what a real NAS does is tested and not read out of its source. It runs
// with "make testacc", which makes the NAS, or "make test-live" against one already up.
package truenasrpc_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// nas is the test NAS as the environment describes it.
var nas struct {
	server, version, certificate string
	username, token, password    string
	readOnlyUser, readOnlyToken  string
}

func TestMain(m *testing.M) {
	for name, into := range map[string]*string{
		"TRUENAS_TEST_SERVER": &nas.server, "TRUENAS_TEST_VERSION": &nas.version, "TRUENAS_TEST_CERTIFICATE_FILE": &nas.certificate,
		"TRUENAS_TEST_USERNAME": &nas.username, "TRUENAS_TEST_TOKEN": &nas.token, "TRUENAS_TEST_PASSWORD": &nas.password,
		"TRUENAS_TEST_READONLY_USERNAME": &nas.readOnlyUser, "TRUENAS_TEST_READONLY_TOKEN": &nas.readOnlyToken,
	} {
		if into == nil {
			continue
		}
		if *into = os.Getenv(name); *into == "" {
			fmt.Fprintf(os.Stderr, "%s is not set: the live tests run against the throwaway NAS that \"make testacc\" makes, or one already up with eval \"$(make testenv-up)\"\n", name)
			os.Exit(1)
		}
	}

	// these tests make and remove things. They run against the NAS that was made to be thrown away and no other, and the name it was given when
	// it was set up is how they know it
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	conn, err := truenasrpc.New(nas.server, truenasrpc.APIKey(nas.username, nas.token), truenasrpc.WithCertificateFile(nas.certificate))
	if err == nil {
		var info struct {
			Hostname string `json:"hostname"`
		}
		if err = conn.Call(ctx, "system.info", nil, &info); err == nil && info.Hostname != layout.Hostname {
			err = fmt.Errorf("it is named %q, and the live tests only run against a NAS named %q, which is what the test environment names the one it makes", info.Hostname, layout.Hostname)
		}
		_ = conn.Close()
	}
	cancel()

	if err != nil {
		fmt.Fprintf(os.Stderr, "refusing to run the live tests against the TrueNAS at %s: %v\n", nas.server, err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// written is a log that keeps what it is sent, for a test to read.
type written struct {
	mu    sync.Mutex
	lines []string
}

func (w *written) Debugf(format string, args ...any) { w.add(format, args...) }
func (w *written) Tracef(format string, args ...any) { w.add(format, args...) }

func (w *written) add(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *written) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return strings.Join(w.lines, "\n")
}

// as is a connection to the test NAS with the credential given, which closes when the test ends.
func as(t *testing.T, cred truenasrpc.Credential, opts ...truenasrpc.Option) *truenasrpc.Conn {
	t.Helper()

	conn, err := truenasrpc.New(nas.server, cred, append([]truenasrpc.Option{truenasrpc.WithCertificateFile(nas.certificate)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// admin is a connection as the administrator, by key, and reader one as the user who may only read.
func admin(t *testing.T, opts ...truenasrpc.Option) *truenasrpc.Conn {
	t.Helper()

	return as(t, truenasrpc.APIKey(nas.username, nas.token), opts...)
}

func reader(t *testing.T) *truenasrpc.Conn {
	t.Helper()

	return as(t, truenasrpc.APIKey(nas.readOnlyUser, nas.readOnlyToken))
}

// within is the test's context with a limit on it, so that a NAS that stops answering fails the one test and not the whole run's time.
func within(t *testing.T, limit time.Duration) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), limit)
	t.Cleanup(cancel)

	return ctx
}
