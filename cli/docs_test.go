package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/mcp/acctest"
	"github.com/katbyte/go-kt/mcp/registry"
	"github.com/katbyte/go-kt/pointer"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
	"github.com/katbyte/truenas-mcp/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// updateDocs has the test write docs/tools.md rather than hold the page to the tools.
var updateDocs = flag.Bool("update", false, "write docs/tools.md from the tools as they are registered")

// toolsPagePath is the page every tool is listed on.
var toolsPagePath = filepath.Join("..", "docs", "tools.md")

// tokensOf is what each tool costs a client's context, by name: its name, description and inputs as a real tools/list sends them, at four bytes a
// token.
func tokensOf(t *testing.T) map[string]float64 {
	t.Helper()

	nas := truenastest.New(t)
	conn, err := truenasrpc.New(nas.Address(), truenasrpc.APIKey(nas.Username(), nas.Key()), truenasrpc.WithRootCAs(nas.Roots()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	srv := mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "0"}, nil)
	if _, err := tools.RegisterAll(srv, conn, tools.Options{Toolsets: []string{registry.All}, EnableWrite: true, EnableDelete: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	suite, err := acctest.Connect(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = suite.Session.Close() })

	listed, err := suite.Session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]float64{}
	for _, tool := range listed.Tools {
		seen, err := json.Marshal(struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema any    `json:"inputSchema"`
		}{tool.Name, tool.Description, tool.InputSchema})
		if err != nil {
			t.Fatal(err)
		}
		tokens[tool.Name] = float64(len(seen)) / 4
	}

	return tokens
}

// about is a number of tokens as the page gives it: to the nearest hundred, so that a word changed here or there leaves the page as it is.
func about(tokens float64) string {
	n := int(tokens/100+0.5) * 100
	s := strconv.Itoa(n)
	if n >= 1000 {
		s = s[:len(s)-3] + "," + s[len(s)-3:]
	}

	return s
}

// needs is what a session has to be told before a tool of this kind loads.
func needs(kind string) string {
	switch kind {
	case "write":
		return "`--enable-write`"
	case "delete":
		return "`--enable-delete`"
	}

	return "nothing"
}

// toolsPage is docs/tools.md as the tools say it should be.
func toolsPage(t *testing.T) string {
	t.Helper()

	described, err := tools.Describe(tools.Options{Toolsets: []string{registry.All}, EnableWrite: true, EnableDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	tokens := tokensOf(t)
	kinds := map[string]int{}
	for _, d := range described {
		kinds[d.Kind]++
	}
	cost := func(names []string) (n int, total float64) {
		for _, name := range names {
			n, total = n+1, total+tokens[name]
		}

		return n, total
	}
	core := tools.Toolsets[registry.Core]

	var b strings.Builder
	fmt.Fprint(&b, "# Tools\n\n")
	fmt.Fprintf(&b, "truenas-mcp has %d tools: %d that read, %d that write and %d that delete. Each is named for what it acts on, then what it does: `pool_list`, `dataset_create`, `audit_space`.\n\n", len(described), kinds["read"], kinds["write"], kinds["delete"])
	fmt.Fprint(&b, "A session only reads until it is told it may do more. Tools that write load with `--enable-write`, and tools that delete only with `--enable-delete` as well; a delete still removes nothing without `confirm: true`, and answers what would go. Every write takes `dry_run`.\n\n")
	fmt.Fprint(&b, "`truenas-mcp tools` lists what the flags given would load, and needs no NAS. This page is written from the tools themselves by `go test ./cli/ -run TestTheToolsPageIsWhatIsRegistered -update`, and that test fails while it is out of date.\n\n")

	defaults := slices.Concat(core, tools.Toolsets["audit"])
	n, total := cost(defaults)
	fmt.Fprint(&b, "## Toolsets\n\n")
	fmt.Fprintf(&b, "**The default is `core` and `audit`: %d tools that read, about %s tokens.** Every tool's description takes room in a client's context before anyone asks a question, so `--toolsets` loads only the sets a session needs, and `core` always comes along.\n\n", n, about(total))
	fmt.Fprint(&b, "| toolset | tools | with core | ~tokens with core |\n|---|---|---|---|\n")
	for _, set := range tools.ToolsetOrder {
		members := tools.Toolsets[set]
		with := members
		if set != registry.Core {
			with = slices.Concat(core, members)
		}
		loaded, spent := cost(with)
		fmt.Fprintf(&b, "| [`%s`](#%s) | %d | %d | %s |\n", set, set, len(members), loaded, about(spent))
	}
	all := make([]string, 0, len(described))
	for _, d := range described {
		all = append(all, d.Name)
	}
	n, total = cost(all)
	fmt.Fprintf(&b, "| `all` | %d | %d | %s |\n\n", n, n, about(total))
	fmt.Fprint(&b, "Tokens are what the model sees: each tool's name, description and inputs, measured over a real `tools/list` at four bytes a token, with every tool in the set loaded. A session that only reads loads fewer.\n\n")
	fmt.Fprint(&b, "`--toolsets` also takes a family, every tool whose name starts with it: `pool`, `dataset`, `snapshot`, `sharing` and so on. `--allow-tools` adds single tools by name and `--deny-tools` takes them away.\n")

	byName := map[string]tools.ToolInfo{}
	for _, d := range described {
		byName[d.Name] = d
	}
	for _, set := range tools.ToolsetOrder {
		fmt.Fprintf(&b, "\n## %s\n\n| tool | needs | what it does |\n|---|---|---|\n", set)
		for _, name := range tools.Toolsets[set] {
			d := byName[name]
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", name, needs(d.Kind), strings.ReplaceAll(firstSentence(d.Description), "|", `\|`))
		}
	}

	return b.String()
}

// docs/tools.md lists every tool there is, by toolset, with what it needs to load and what it does, and what each toolset costs a client: written
// from the tools themselves, so that it cannot say one thing while the tools do another.
//
//nolint:paralleltest // -update writes the page, which another test running at once would read half written
func TestTheToolsPageIsWhatIsRegistered(t *testing.T) {
	want := toolsPage(t)
	if pointer.From(updateDocs) {
		if err := os.WriteFile(toolsPagePath, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}

		return
	}

	have, err := os.ReadFile(toolsPagePath) //nolint:gosec // G304: the repository's own page, named above
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != want {
		t.Error("docs/tools.md is not what the tools say: go test ./cli/ -run TestTheToolsPageIsWhatIsRegistered -update writes it again")
	}
}
