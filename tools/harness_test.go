package tools

import (
	"context"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	mcpregistry "github.com/katbyte/go-kt/mcp/registry"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tools end to end against the stand-in NAS: the calls a handler makes and the answer it makes of them, with no NAS. The live suite under
// acceptance/tools holds the same tools to a real one.

// everything is the options that register every tool: the ones that change something, and the ones that delete.
var everything = Options{EnableWrite: true, EnableDelete: true}

// connected is a connection to the stand-in, closed when the test ends.
func connected(t *testing.T, nas *truenastest.NAS) *truenasrpc.Conn {
	t.Helper()

	conn, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key()), truenasrpc.WithRootCAs(nas.Roots()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// session connects an in-memory MCP client to a server registering every tool against the stand-in, with the given options.
func session(t *testing.T, nas *truenastest.NAS, opts Options) *acctest.Suite {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	opts.Toolsets = []string{mcpregistry.All}
	if _, err := RegisterAll(srv, connected(t, nas), opts); err != nil {
		t.Fatal(err)
	}

	return connect(t, srv)
}

// connect is a client session to a server, closed when the test ends.
func connect(t *testing.T, srv *mcp.Server) *acctest.Suite {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	suite, err := acctest.Connect(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = suite.Session.Close() })

	return suite
}

// unreachable is a connection to a NAS that is never called, for the tests of which tools are registered: nothing is dialled until a call is made.
func unreachable(t *testing.T) *truenasrpc.Conn {
	t.Helper()

	conn, err := truenasrpc.New("127.0.0.1:1", truenasrpc.APIKey("nobody", "1-notakey"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// register is the names RegisterAll registers for the options.
func register(t *testing.T, opts Options) []string {
	t.Helper()

	names, err := RegisterAll(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), unreachable(t), opts)
	if err != nil {
		t.Fatal(err)
	}

	return names
}

// refused is what RegisterAll refuses the options with.
func refused(t *testing.T, opts Options) string {
	t.Helper()

	_, err := RegisterAll(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), unreachable(t), opts)
	if err == nil {
		t.Errorf("%+v was accepted", opts)

		return ""
	}

	return err.Error()
}
