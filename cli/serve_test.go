package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/server"
	"github.com/katbyte/truenas-mcp/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testToken  = "s3cret"
	testListen = ":8080"
	coreTool   = "server_info"
)

// Serving itself is go-kt's, tested there. What is this tool's own is what it hands over: the flags, and the names an operator and a client are told.
func TestServeHandsOverTheFlagsAndThisToolsOwnNames(t *testing.T) {
	t.Parallel()

	f := &FlagData{Listen: testListen, AuthToken: testToken, AllowNoAuth: true}
	if got, want := f.serveOptions(), (server.Options{Listen: testListen, AuthToken: testToken, AllowNoAuth: true, Name: "truenas-mcp", EnvPrefix: "TRUENAS"}); got != want {
		t.Errorf("serveOptions = %+v, want %+v", got, want)
	}
}

// The health probe has to stay outside the auth check, or a container with a token configured never becomes healthy; the MCP endpoint is behind it,
// and a client sent away is told whose token it lacks.
func TestTheHealthProbeIsOpenAndTheEndpointIsBehindTheToken(t *testing.T) {
	t.Parallel()

	srv := mcp.NewServer(&mcp.Implementation{Name: "truenas-mcp", Version: "test"}, nil)
	routes := server.Handler(srv, (&FlagData{AuthToken: testToken}).serveOptions())

	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, server.HealthPath, http.NoBody))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("%s with a token configured = %d %q, want 200 ok", server.HealthPath, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, server.Path, strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("%s without a token = %d, want 401", server.Path, rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") || !strings.Contains(got, "truenas-mcp") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge naming truenas-mcp", got)
	}
}

// --listen with no bearer token is an open port, so serve refuses it unless the operator said so in as many words, and the refusal names this tool's
// own variables. It is refused before the NAS is asked anything.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestServeRefusesAnOpenPortAndAContradiction(t *testing.T) {
	if _, err := run(t, "serve", "--server", testServer, "--token", testKey, "--listen", "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "TRUENAS_AUTH_TOKEN") || !strings.Contains(err.Error(), "TRUENAS_ALLOW_NO_AUTH") {
		t.Errorf("--listen with no token = %v, want a refusal naming both ways out", err)
	}
	if _, err := run(t, "serve", "--server", testServer, "--token", testKey, "--read-only", "--enable-write"); err == nil || !strings.Contains(err.Error(), "contradict") {
		t.Errorf("--read-only beside --enable-write = %v", err)
	}
	if _, err := run(t, "serve", "--server", testServer, "--token", testKey, "--enable-delete"); err == nil || !strings.Contains(err.Error(), "--enable-delete needs --enable-write") {
		t.Errorf("--enable-delete alone = %v", err)
	}
	if _, err := run(t, "serve"); err == nil || !strings.Contains(err.Error(), "server") {
		t.Errorf("serve with no NAS = %v", err)
	}
}

// `truenas-mcp tools` needs no NAS, so it has to work with nothing configured.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestToolsListsWhatWouldBeRegisteredWithNothingConfigured(t *testing.T) {
	out, err := run(t, "tools")
	if err != nil {
		t.Fatalf("tools: %v", err)
	}

	// the default is core and the audits, so their tools are listed, every one a read, and counted
	core, audits := tools.Toolsets["core"], tools.Toolsets["audit"]
	listed := len(core) + len(audits)
	for _, want := range slices.Concat([]string{fmt.Sprintf("core (%d)", len(core)), fmt.Sprintf("audit (%d)", len(audits)), fmt.Sprintf("%d tools: %d read, 0 write, 0 delete", listed, listed)}, core, audits) {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from `tools`:\n%s", want, out)
		}
	}
	// and it says how to get more
	for _, want := range []string{"toolsets: all, ", "families: alert, app, audit, cloud, cloudsync, dataset, disk, group, interface, iscsi, network, pool, replication, rsynctask, server, service, sharing, snapshot, snapshottask, user, vm", "--toolsets all", "only tools that read are listed; --enable-write registers the ones that change something", "--enable-delete", "TRUENAS_TOOLSETS"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from the footer:\n%s", want, out)
		}
	}
	// one line a tool: its name, its kind and its first sentence
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, coreTool) && (!strings.Contains(line, " read ") || strings.Count(line, ". ") > 0) {
			t.Errorf("the line for %s is not its name, kind and one sentence: %q", coreTool, line)
		}
	}
}

// What the listing holds and what it says is held back follow the flags, and a choice that cannot be made is refused as serve refuses it.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestToolsFollowsTheFlags(t *testing.T) {
	if out, err := run(t, "tools", "--allow-tools", "server_*", "-q"); err != nil || strings.TrimSpace(out) != coreTool {
		t.Errorf("tools --allow-tools server_* -q = %q, %v", out, err)
	}
	if out, err := run(t, "tools", "--toolsets", "all", "--deny-tools", "*_info,*_list,audit_*", "-q"); err != nil || !slices.Equal(strings.Fields(out), []string{"dataset_get", "network_configuration_get", "pool_get"}) {
		t.Errorf("tools --toolsets all --deny-tools *_info,*_list,audit_* -q = %q, %v", out, err)
	}

	out, err := run(t, "tools", "--toolsets", "all", "--enable-write")
	if err != nil || !strings.Contains(out, "delete tools are hidden; --enable-delete registers them") || strings.Contains(out, "only tools that read") {
		t.Errorf("with --enable-write: %v\n%s", err, out)
	}
	out, err = run(t, "tools", "--toolsets", "all", "--enable-write", "--enable-delete")
	if err != nil || strings.Contains(out, "tools are hidden") || strings.Contains(out, "only tools that read") {
		t.Errorf("with everything enabled the listing still says something is held back: %v\n%s", err, out)
	}

	if _, err = run(t, "tools", "--enable-delete"); err == nil || !strings.Contains(err.Error(), "--enable-delete needs --enable-write") {
		t.Errorf("tools --enable-delete alone = %v", err)
	}
	if _, err = run(t, "tools", "--read-only", "--enable-write"); err == nil || !strings.Contains(err.Error(), "contradict") {
		t.Errorf("tools --read-only --enable-write = %v", err)
	}
	if _, err = run(t, "tools", "--toolsets", "nope"); err == nil || !strings.Contains(err.Error(), "core") || !strings.Contains(err.Error(), "server") {
		t.Errorf("an unknown toolset = %v, want an error naming the sets and families", err)
	}
}

func TestFirstSentenceStopsAtTheFirstFullStopThatEndsOne(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ in, want string }{
		{"One thing. And then another.", "One thing."},
		{"No full stop at all", "No full stop at all"},
		{"Trailing full stop.", "Trailing full stop."},
		{"A kind, e.g. socket. Then more.", "A kind, e.g. socket."},
		{"Ends in etc. and goes on", "Ends in etc. and goes on"},
		{"", ""},
	} {
		if got := firstSentence(c.in); got != c.want {
			t.Errorf("firstSentence(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
