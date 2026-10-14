//go:build acceptance

package tools_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/go-kt/mcp/registry"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/acceptance/setup"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nas is the test NAS as the environment describes it, and what setting it up made: a key for its administrator and one for the user who may only
// read.
var nas struct {
	server, certificate string
	username, password  string
	made                *setup.Made
}

var (
	// suite is the one client session every test calls the tools through, as the administrator with every tool registered, and the record of what was
	// called
	suite = &acctest.Suite{NotReady: "the live tests need the throwaway NAS: make testacc-tools"}
	// conns are the connections the run made, closed when it ends
	conns []*truenasrpc.Conn
)

// runSuite checks the NAS is the throwaway one, sets it up, runs the tests and holds the run to having called every tool and shown no key, returning
// the code to exit with.
func runSuite(m *testing.M) int {
	for name, into := range map[string]*string{"TRUENAS_TEST_SERVER": &nas.server, "TRUENAS_TEST_CERTIFICATE_FILE": &nas.certificate, "TRUENAS_TEST_USERNAME": &nas.username, "TRUENAS_TEST_PASSWORD": &nas.password} {
		if into == nil {
			continue
		}
		if *into = os.Getenv(name); *into == "" {
			fmt.Fprintf(os.Stderr, "%s is not set: the live tests run against the throwaway NAS that \"make testacc-tools\" makes, or one already up with eval \"$(make testenv-up)\"\n", name)

			return 1
		}
	}
	// the binary is run from other directories, so it is told of the certificate by its whole path
	if abs, err := filepath.Abs(nas.certificate); err == nil {
		nas.certificate = abs
	}

	ctx := context.Background()
	err := start(ctx)
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, "acceptance setup:", err)

		return 1
	}

	code := m.Run()
	removeBinary()

	// every registered tool must have answered something above, not only refused it. Only a whole run can say that, so a -run or -skip filter skips
	// the check
	if acctest.WholeRun() {
		report, err := suite.CoverageReport()
		if err != nil {
			fmt.Fprintln(os.Stderr, "\n"+err.Error())
			code = 1
		}
		if report != "" {
			fmt.Fprint(os.Stderr, report)
			code = 1
		}
	}
	if report := suite.LeakReport(); report != "" {
		fmt.Fprint(os.Stderr, report)
		code = 1
	}

	return code
}

// start refuses a NAS that was not made to be thrown away, sets it up, and connects the suite to an MCP server registering every tool on it as its
// administrator.
func start(ctx context.Context) error {
	// only setting the NAS up is held to a time: the session made after it lives as long as the run
	setupCtx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()

	root, err := connection(truenasrpc.Password(nas.username, nas.password))
	if err != nil {
		return err
	}
	n := &setup.NAS{Conn: root, Logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }}
	if err := n.Disposable(setupCtx); err != nil {
		return fmt.Errorf("refusing to run the live tests against the TrueNAS at %s: %w", nas.server, err)
	}
	if nas.made, err = n.All(setupCtx); err != nil {
		return fmt.Errorf("setting the NAS up: %w", err)
	}

	admin, err := connection(truenasrpc.APIKey(nas.username, nas.made.Key))
	if err != nil {
		return err
	}
	connected, err := served(ctx, admin, tools.Options{Toolsets: []string{registry.All}, EnableWrite: true, EnableDelete: true})
	if err != nil {
		return err
	}
	suite = connected
	for _, secret := range []string{nas.password, nas.made.Key, nas.made.ReadOnlyKey} {
		suite.Secret(secret)
	}

	return nil
}

// connection is a connection to the test NAS, closed when the run ends.
func connection(cred truenasrpc.Credential) (*truenasrpc.Conn, error) {
	conn, err := truenasrpc.New(nas.server, cred, truenasrpc.WithCertificateFile(nas.certificate))
	if err != nil {
		return nil, err
	}
	conns = append(conns, conn)

	return conn, nil
}

// served is a client session to an MCP server registering the tools the options ask for on a connection.
func served(ctx context.Context, conn *truenasrpc.Conn, opts tools.Options) (*acctest.Suite, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "truenas-mcp-acceptance", Version: "test"}, nil)
	if _, err := tools.RegisterAll(srv, conn, opts); err != nil {
		return nil, err
	}

	return acctest.Connect(ctx, srv)
}

// asReader is a client session as the user who may only read, with the tools the options ask for. Its calls are not counted towards the run's: a tool
// is covered by the administrator's session.
func asReader(t *testing.T, opts tools.Options) *acctest.Suite {
	t.Helper()

	if !suite.Ready {
		t.Skip(suite.NotReady)
	}
	conn, err := connection(truenasrpc.APIKey(layout.ReadOnlyUser, nas.made.ReadOnlyKey))
	if err != nil {
		t.Fatal(err)
	}
	opts.Toolsets = []string{registry.All}
	reader, err := served(t.Context(), conn, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Session.Close() })

	return reader
}
