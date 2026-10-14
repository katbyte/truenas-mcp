//go:build acceptance

// The binary, not the package. Everything else in this suite drives tools.RegisterAll in process, which is every line of tool code the binary runs;
// what it never touches is the thin layer around it: stdio framing, the flags and environment reaching the server, the HTTP routes, and what a bad
// start says. Those break silently: a stray line on stdout corrupts the protocol and a client simply fails to connect, with every other test green.
//
// So this is a smoke test: the real binary, built from this checkout, spoken to the way a client speaks to it, against the same NAS.
package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	binOnce   sync.Once
	binPath   string
	binDir    string
	errBinary error
)

// removeBinary deletes the binary the smoke test built, at the end of the run: it is built once for the suite, so no one test can clean it up.
func removeBinary() {
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
}

// builtBinary builds truenas-mcp from this checkout, once per run, so the test speaks to the code in the tree rather than to whatever is installed.
func builtBinary(t *testing.T) string {
	t.Helper()

	if !suite.Ready {
		t.Skip(suite.NotReady)
	}
	binOnce.Do(func() {
		// not t.TempDir: the binary is built once and outlives the test that built it
		binDir, errBinary = os.MkdirTemp("", "truenas-mcp-binary") //nolint:usetesting // removed by removeBinary at the end of the run
		if errBinary != nil {
			return
		}
		binPath = filepath.Join(binDir, "truenas-mcp")
		build := exec.CommandContext(context.Background(), "go", "build", "-o", binPath, ".") //nolint:gosec // G204: go build, with the arguments the test wrote
		build.Dir = "../.."
		if out, err := build.CombinedOutput(); err != nil {
			errBinary = fmt.Errorf("building truenas-mcp: %w\n%s", err, out)
		}
	})
	if errBinary != nil {
		t.Fatal(errBinary)
	}

	return binPath
}

// bareEnv is an environment with nothing of ours in it: a stray TRUENAS_* in the shell running the tests cannot change what is registered, and HOME
// is an empty directory, so a real ~/.truenas-mcp cannot either.
func bareEnv(t *testing.T, extra ...string) []string {
	t.Helper()

	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, extra...)
}

// serverEnv is the environment a client would give the binary: the NAS under test, its certificate, and the administrator's key.
func serverEnv(t *testing.T, extra ...string) []string {
	t.Helper()

	return bareEnv(t, append([]string{"TRUENAS_SERVER=" + nas.server, "TRUENAS_USERNAME=" + nas.username, "TRUENAS_TOKEN=" + nas.made.Key, "TRUENAS_CERTIFICATE_FILE=" + nas.certificate}, extra...)...)
}

// secrets are what the binary must never write: the administrator's password and both keys.
func secrets() []string { return []string{nas.password, nas.made.Key, nas.made.ReadOnlyKey} }

// syncBuffer collects output a process writes while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// teeReadCloser is a reader that also closes the process's stdout.
type teeReadCloser struct {
	io.Reader
	io.Closer
}

// stdio starts "truenas-mcp serve" as a client does, connects an MCP session over its stdin and stdout, and keeps everything the process wrote to
// each stream so a test can read them. dir is the working directory, for the config file.
func stdio(t *testing.T, dir string, env []string, args ...string) (session *mcp.ClientSession, wrote, wroteErr *syncBuffer) {
	t.Helper()

	ctx := context.WithoutCancel(t.Context())
	cmd := exec.CommandContext(ctx, builtBinary(t), append([]string{"serve"}, args...)...) //nolint:gosec // G204: the binary this test built itself, run with the arguments the test wrote
	cmd.Env = env
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := &syncBuffer{}, &syncBuffer{}
	cmd.Stderr = errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	session, err = mcp.NewClient(&mcp.Implementation{Name: "binary-test", Version: "0"}, nil).Connect(ctx, &mcp.IOTransport{Reader: teeReadCloser{Reader: io.TeeReader(stdout, out), Closer: stdout}, Writer: stdin}, nil)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("connecting to the binary over stdio: %v\nstderr: %s", err, errOut)
	}
	t.Cleanup(func() {
		_ = session.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	return session, out, errOut
}

// toolNamesOf lists the tools a session's server registered.
func toolNamesOf(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()

	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		listed = append(listed, tool.Name)
	}
	slices.Sort(listed)

	return listed
}

// through calls a tool that takes no arguments over the session and returns what it answered.
func through(t *testing.T, session *mcp.ClientSession, tool string) map[string]any {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s through the binary: %v", tool, said(res))
	}
	out, isObject := res.StructuredContent.(map[string]any)
	if !isObject {
		t.Fatalf("%s through the binary answered %v", tool, res.StructuredContent)
	}

	return out
}

// said is the text of a tool's answer.
func said(res *mcp.CallToolResult) string {
	var text strings.Builder
	for _, content := range res.Content {
		if piece, isText := content.(*mcp.TextContent); isText {
			text.WriteString(piece.Text)
		}
	}

	return text.String()
}

// reachedThrough says whether server_info through a session answers for the test NAS as its administrator, which proves the address, the certificate
// and the key all reached the connection the binary made.
func reachedThrough(t *testing.T, session *mcp.ClientSession) bool {
	t.Helper()

	info := through(t, session, "server_info")

	return info["hostname"] == layout.Hostname && info["user"] == nas.username && info["version"] == nas.made.Version
}

// MCP over stdio uses stdout for the protocol: one line of anything else - a print, a log line, a cobra message - corrupts the stream, and the client
// fails to connect with every test still green. So the binary is run at the most verbose log level and every line it writes to stdout is checked.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheBinarySpeaksOnlyTheProtocolOnStdout(t *testing.T) {
	session, out, errOut := stdio(t, t.TempDir(), serverEnv(t, "TRUENAS_LOG=TRACE"), "--toolsets", "all")

	if !reachedThrough(t, session) {
		t.Error("server_info through the binary does not answer for the test NAS as its administrator")
	}
	if err := session.Close(); err != nil {
		t.Errorf("closing the session: %v", err)
	}

	for i, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.JSONRPC != "2.0" {
			t.Fatalf("stdout line %d is not a protocol message, which breaks the stream: %q", i+1, line)
		}
	}
	// the logging that must not be on stdout does happen, on stderr: the start, and the traffic at trace, with no key in it
	logged := errOut.String()
	if !strings.Contains(logged, "registered") || !strings.Contains(logged, "system.info") {
		t.Errorf("stderr has no start line and no traffic at trace level: %q", logged)
	}
	for _, secret := range secrets() {
		if strings.Contains(logged, secret) || strings.Contains(out.String(), secret) {
			t.Error("a key or the password is in what the binary wrote")
		}
	}
}

// core is the tools every session gets, sorted, and defaults those it gets with nothing asked for: core's and the audits.
var (
	core     = []string{"alert_list", "dataset_get", "dataset_list", "pool_get", "pool_list", "server_info"}
	defaults = []string{"alert_list", "audit_disks", "audit_pools", "audit_space", "audit_tasks", "dataset_get", "dataset_list", "pool_get", "pool_list", "server_info"}
)

// The flags and the environment have to reach the running server, not just parse: what a client lists is what was asked for.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheBinaryRegistersWhatTheFlagsAsk(t *testing.T) {
	for _, c := range []struct {
		name string
		env  []string
		args []string
		want []string
	}{
		{name: "the default sets are core and the audits", want: defaults},
		{name: "a toolset from the environment, with core beside it", env: []string{"TRUENAS_TOOLSETS=disks"}, want: append([]string{"disk_list"}, core...)},
		{name: "--allow-tools alone is only the tools it names", args: []string{"--allow-tools", "server_*"}, want: []string{"server_info"}},
		{name: "--deny-tools takes away", args: []string{"--toolsets", "all", "--deny-tools", "server_*,*_get"}, want: []string{"alert_list", "audit_disks", "audit_pools", "audit_space", "audit_tasks", "dataset_list", "disk_list", "pool_list"}},
		{name: "--deny-tools from the environment", env: []string{"TRUENAS_DENY_TOOLS=*_info,*_get"}, want: []string{"alert_list", "audit_disks", "audit_pools", "audit_space", "audit_tasks", "dataset_list", "pool_list"}},
	} {
		t.Run(c.name, func(t *testing.T) { //nolint:paralleltest // as the test
			// an empty working directory: only the flags and the environment under test decide what is registered
			session, _, _ := stdio(t, t.TempDir(), serverEnv(t, c.env...), c.args...)
			slices.Sort(c.want)
			if listed := toolNamesOf(t, session); !slices.Equal(listed, c.want) {
				t.Errorf("registered %v, want %v", listed, c.want)
			}
		})
	}
}

// A .truenas-mcp in the working directory is how a project configures the binary without putting a key in .mcp.json. A setting of two words is
// spelled there as the environment spells it: the certificate is one, and without it the NAS's own certificate would not be trusted.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheBinaryReadsTheConfigFileInTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	config := "SERVER=" + nas.server + "\nUSERNAME=" + nas.username + "\nTOKEN=" + nas.made.Key + "\nCERTIFICATE_FILE=" + nas.certificate + "\nTRUENAS_TOOLSETS=disks\n"
	if err := os.WriteFile(filepath.Join(dir, ".truenas-mcp"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	// nothing in the environment: the file is all it has
	session, _, _ := stdio(t, dir, bareEnv(t))
	if listed := toolNamesOf(t, session); !slices.Contains(listed, "disk_list") || len(listed) != len(core)+1 {
		t.Errorf("the toolset in the config file was not used: %v", listed)
	}
	if !reachedThrough(t, session) {
		t.Error("the address, the certificate and the key in the config file did not reach the connection")
	}
}

// --listen has to serve the protocol at /mcp behind the bearer check, answer the health probe a container watches, stay up, and shut down when asked.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheBinaryServesHTTP(t *testing.T) {
	const token = "zzyzx-binary-token" //nolint:gosec // G101: a made-up value for the throwaway server, which is no credential of anything real

	ctx := context.WithoutCancel(t.Context())
	addr := freePort(t)
	cmd := exec.CommandContext(ctx, builtBinary(t), "serve", "--listen", addr, "--auth-token", token, "--toolsets", "all") //nolint:gosec // G204: the binary this test built itself, run with the arguments the test wrote
	cmd.Env = serverEnv(t)
	errOut := &syncBuffer{}
	cmd.Stderr, cmd.Stdout = errOut, errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	base := "http://" + addr
	waitHealthy(t, base, exited, errOut)

	// get answers the status and the challenge, if any
	get := func(path, auth string) (status int, challenge string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}

		return res.StatusCode, res.Header.Get("WWW-Authenticate")
	}

	t.Run("the endpoint is behind the bearer check", func(t *testing.T) { //nolint:paralleltest // each step needs the one before it
		if status, challenge := get("/mcp", ""); status != http.StatusUnauthorized {
			t.Errorf("GET /mcp with no token = %d, want 401", status)
		} else if !strings.Contains(challenge, "Bearer") {
			t.Errorf("no Bearer challenge: %q", challenge)
		}
		if status, _ := get("/mcp", "Bearer nope"); status != http.StatusUnauthorized {
			t.Errorf("GET /mcp with the wrong token = %d, want 401", status)
		}
		// the NAS's own key is not the one this endpoint takes
		if status, _ := get("/mcp", "Bearer "+nas.made.Key); status != http.StatusUnauthorized {
			t.Errorf("GET /mcp with the NAS's key = %d, want 401", status)
		}
		if status, _ := get("/nope", "Bearer "+token); status != http.StatusNotFound {
			t.Errorf("GET /nope = %d, want 404", status)
		}
	})

	// the session stays open through the shutdown below, as a client that is still connected when the container is stopped would
	var session *mcp.ClientSession
	t.Run("and serves the protocol with it", func(t *testing.T) { //nolint:paralleltest // each step needs the one before it
		var err error
		session, err = mcp.NewClient(&mcp.Implementation{Name: "binary-test", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: &http.Client{Transport: bearer(token)}}, nil)
		if err != nil {
			t.Fatalf("connecting over HTTP: %v\n%s", err, errOut)
		}
		t.Cleanup(func() { _ = session.Close() })

		if !reachedThrough(t, session) { //nolint:contextcheck // the helper asks with the test's own context, which outlives this one's deadline for starting the binary
			t.Error("server_info over HTTP does not answer for the test NAS as its administrator")
		}
	})

	t.Run("and shuts down when asked, with a client still connected", func(t *testing.T) { //nolint:paralleltest // each step needs the one before it
		if session == nil {
			t.Skip("no session to hold open")
		}
		asked := time.Now()
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("exit after SIGTERM: %v\n%s", err, errOut)
			}
			// a container gets ten seconds to stop before it is killed
			if took := time.Since(asked); took > 5*time.Second {
				t.Errorf("took %s to stop with a client connected", took.Round(time.Millisecond))
			}
		case <-time.After(shutdownGrace):
			t.Errorf("still running %s after SIGTERM", shutdownGrace)
		}
	})
}

// shutdownGrace is how long the binary gets to drain and exit.
const shutdownGrace = 15 * time.Second

// bearer adds the HTTP endpoint's token to every request.
func bearer(token string) http.RoundTripper {
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("Authorization", "Bearer "+token)

		return http.DefaultTransport.RoundTrip(r)
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// freePort is an address nothing is listening on, for --listen.
func freePort(t *testing.T) string {
	t.Helper()

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	return l.Addr().String()
}

// waitHealthy polls the health probe until the server answers it, failing with whatever the process said if it exits first.
func waitHealthy(t *testing.T, base string, exited <-chan error, out *syncBuffer) {
	t.Helper()

	for range 100 {
		select {
		case err := <-exited:
			t.Fatalf("the server exited before serving: %v\n%s", err, out)
		default:
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/healthz", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := http.DefaultClient.Do(req); err == nil {
			body, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no health response from the server\n%s", out)
}

// A start that cannot work has to say so and stop. A client waiting on stdio sees nothing at all otherwise, and a container restarts in a loop.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheBinaryRefusesAStartItCannotMake(t *testing.T) {
	for _, c := range []struct {
		name string
		env  []string
		args []string
		says string
	}{
		{name: "no NAS", env: bareEnv(t), says: "server parameter can't be empty"},
		{name: "no key", env: bareEnv(t, "TRUENAS_SERVER="+nas.server), says: "token parameter can't be empty"},
		{name: "an address with no TLS, which would have the key revoked", env: bareEnv(t, "TRUENAS_SERVER=http://"+strings.TrimPrefix(nas.server, "https://"), "TRUENAS_TOKEN="+nas.made.Key), says: "TLS"},
		{name: "--listen with no auth token", args: []string{"--listen", "127.0.0.1:0"}, says: "--auth-token"},
		{name: "a toolset that does not exist", args: []string{"--toolsets", "zzyzx"}, says: "zzyzx"},
		{name: "an allow pattern matching nothing", args: []string{"--allow-tools", "zzyzx_*"}, says: "zzyzx_*"},
		{name: "deleting with no leave to change", args: []string{"--enable-delete"}, says: "--enable-delete needs --enable-write"},
		{name: "reading only and changing at once", args: []string{"--read-only", "--enable-write"}, says: "contradict each other"},
		{name: "a flag there is none of", args: []string{"--zzyzx"}, says: "zzyzx"},
	} {
		t.Run(c.name, func(t *testing.T) { //nolint:paralleltest // as the test
			env := c.env
			if env == nil {
				env = serverEnv(t)
			}
			cmd := exec.CommandContext(context.WithoutCancel(t.Context()), builtBinary(t), append([]string{"serve"}, c.args...)...) //nolint:gosec // G204: the binary this test built itself, run with the arguments the test wrote
			cmd.Env = env
			cmd.Dir = t.TempDir() // no .truenas-mcp to fall back on
			out := &syncBuffer{}
			cmd.Stdout, cmd.Stderr = out, out

			done := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() { done <- cmd.Wait() }()

			select {
			case err := <-done:
				if err == nil {
					t.Errorf("exited 0, want a refusal: %s", out)
				}
			case <-time.After(shutdownGrace):
				_ = cmd.Process.Kill()
				t.Fatalf("still running: a bad start has to fail, not hang\n%s", out)
			}
			if got := out.String(); !strings.Contains(got, c.says) {
				t.Errorf("said %q, want it to name %q", got, c.says)
			}
			for _, secret := range secrets() {
				if strings.Contains(out.String(), secret) {
					t.Error("the refusal repeats a key or the password")
				}
			}
		})
	}
}

// The NAS is asked nothing at start, so a key that is wrong is not a start the binary refuses: it is what the first tool says, in words a person can
// act on and with no key in them. The key is made up, numbered as no key on the NAS is.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheBinarySaysTheKeyIsWrongWhenAToolIsCalled(t *testing.T) {
	const wrong = "999-zzyzxzzyzxzzyzxzzyzxzzyzxzzyzxzzyzxzzyzxzzyzxzzyzxzzyzxzzyzxzzyzx"

	session, out, errOut := stdio(t, t.TempDir(), bareEnv(t, "TRUENAS_SERVER="+nas.server, "TRUENAS_USERNAME="+nas.username, "TRUENAS_TOKEN="+wrong, "TRUENAS_CERTIFICATE_FILE="+nas.certificate))
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "server_info"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("server_info with a wrong key answered %v", res.StructuredContent)
	}
	if text := said(res); !strings.Contains(text, "refused the API key or password") || !strings.Contains(text, "could not be asked") {
		t.Errorf("with a wrong key the binary says %q", text)
	}
	if strings.Contains(said(res)+out.String()+errOut.String(), wrong) {
		t.Error("the key that was refused is repeated in what the binary wrote")
	}
}

// The commands a person runs to check an install, rather than a client.
//
//nolint:paralleltest // the tests share one NAS, which takes only twenty logins a minute from one address
func TestTheBinarysCommandsForAPerson(t *testing.T) {
	run := func(t *testing.T, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(context.WithoutCancel(t.Context()), builtBinary(t), args...) //nolint:gosec // G204: the binary this test built itself, run with the arguments the test wrote
		cmd.Env = serverEnv(t)
		cmd.Dir = t.TempDir()
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}

		return string(out)
	}

	if got := run(t, "version"); !strings.HasPrefix(got, "truenas-mcp ") {
		t.Errorf("version = %q", got)
	}
	// tools needs no NAS, and says what it would register
	if got := run(t, "tools", "-q", "--toolsets", "all", "--enable-write", "--enable-delete"); len(strings.Fields(got)) != len(suite.ToolNames(t)) {
		t.Errorf("tools -q listed %d tools, and the server registers %d", len(strings.Fields(got)), len(suite.ToolNames(t)))
	}
	if got := run(t, "info"); !strings.Contains(got, "TrueNAS "+nas.made.Version+" at ") {
		t.Errorf("info = %q", got)
	}
}
