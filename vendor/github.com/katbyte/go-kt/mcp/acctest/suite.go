// Package acctest drives an MCP server the way a client does, for a suite against a live one: calls counted for coverage, readers for the answers,
// waits on background work, put-backs for what a test changed, and a watch on everything the tools say for a key the suite gave the server. What a
// suite needs of the server itself, its environment and proxy, is in test/env.
package acctest

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Suite is one client session to the server under test, and the record of what it was asked.
type Suite struct {
	// Ctx is the run's context, not a test's, which is cancelled before the clean-ups that put things back run
	Ctx     context.Context //nolint:containedctx // the run's own context, which every call the suite makes is under
	Session *mcp.ClientSession
	// Ready says the server is there; a test that calls when it is not skips, saying NotReady
	Ready    bool
	NotReady string
	// CannotAnswer are the tools a throwaway server cannot answer, and why, so a failed call is all a test can make of them
	CannotAnswer map[string]string

	mu     sync.Mutex
	called map[string]Calls
	// secrets is what every answer is read for; shown where one was found
	secrets []string
	shown   []string
}

// Connect serves the server in this process and returns a suite with a client session to it, so the tools are driven as a client drives them.
func Connect(ctx context.Context, server *mcp.Server) (*Suite, error) {
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		return nil, err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "acctest", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return nil, err
	}

	return &Suite{Ctx: ctx, Session: session, Ready: true}, nil
}

// Calls is how a tool's calls went: answered, or failed.
type Calls struct {
	Answered, Failed int
}

// Calls is how a tool's calls have gone so far.
func (s *Suite) Calls(name string) Calls {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.called[name]
}

// Invoke calls a tool and returns its structured answer. Every call comes through here, so this is where coverage is counted (Uncovered).
func (s *Suite) Invoke(name string, args map[string]any) (map[string]any, error) {
	out, err := s.callTool(name, args)

	s.mu.Lock()
	if s.called == nil {
		s.called = map[string]Calls{}
	}
	c := s.called[name]
	if err != nil {
		c.Failed++
	} else {
		c.Answered++
	}
	s.called[name] = c
	s.mu.Unlock()

	return out, err
}

func (s *Suite) callTool(name string, args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, err := s.Session.CallTool(s.Ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	s.watch(name, res, err)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if res.IsError {
		var msgs []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msgs = append(msgs, tc.Text)
			}
		}

		return nil, fmt.Errorf("%s: %s", name, strings.Join(msgs, "; "))
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: structured content is %T", name, res.StructuredContent)
	}

	return out, nil
}

// Call invokes a tool, skipping the test when the server is not there and failing it on an error.
func (s *Suite) Call(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()

	if !s.Ready {
		t.Skip(s.NotReady)
	}
	out, err := s.Invoke(name, args)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// CallErr invokes a tool expecting it to fail, and returns the error message.
func (s *Suite) CallErr(t *testing.T, name string, args map[string]any) string {
	t.Helper()

	if !s.Ready {
		t.Skip(s.NotReady)
	}
	out, err := s.Invoke(name, args)
	if err == nil {
		t.Fatalf("%s unexpectedly succeeded: %v", name, out)
	}

	return err.Error()
}

// ToolNames lists every tool the server registered, so a test can hold a family complete.
func (s *Suite) ToolNames(t *testing.T) []string {
	t.Helper()

	if !s.Ready {
		t.Skip(s.NotReady)
	}
	res, err := s.Session.ListTools(s.Ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		out = append(out, tool.Name)
	}

	return out
}

// Uncovered names the tools no test saw answer: never called, or only ever failed. A tool only refused proves it checks its input, not that it works,
// so a new tool without a test fails the suite. Those in CannotAnswer still need a call.
func (s *Suite) Uncovered() (never, onlyFailed []string, err error) {
	res, err := s.Session.ListTools(s.Ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, tool := range res.Tools {
		c := s.called[tool.Name]
		switch {
		case c.Answered > 0:
		case c.Failed > 0 && s.CannotAnswer[tool.Name] != "":
		case c.Failed > 0:
			onlyFailed = append(onlyFailed, tool.Name)
		default:
			never = append(never, tool.Name)
		}
	}
	slices.Sort(never)
	slices.Sort(onlyFailed)

	return never, onlyFailed, nil
}

// CoverageReport is Uncovered as TestMain prints it: "" when every tool answered, else the tools never called and those that only failed. Only a
// whole run can say a tool was never called (WholeRun).
func (s *Suite) CoverageReport() (string, error) {
	never, onlyFailed, err := s.Uncovered()
	if err != nil {
		return "", fmt.Errorf("tool coverage: could not list tools: %w", err)
	}
	if len(never)+len(onlyFailed) == 0 {
		return "", nil
	}

	var b strings.Builder
	if len(never) > 0 {
		fmt.Fprintf(&b, "\n%d registered tool(s) are never called by this suite:\n", len(never))
		for _, name := range never {
			fmt.Fprintln(&b, "  "+name)
		}
	}
	if len(onlyFailed) > 0 {
		fmt.Fprintf(&b, "\n%d registered tool(s) only ever failed in this suite, so nothing shows they work:\n", len(onlyFailed))
		for _, name := range onlyFailed {
			fmt.Fprintf(&b, "  %s (%d failed calls)\n", name, s.Calls(name).Failed)
		}
	}
	fmt.Fprintln(&b, "every tool needs a test that it answers; add one or remove the tool")

	return b.String(), nil
}

// Secret has everything a tool says from now on read for a key, and hands the key back, so a test wraps a key where it gives it to the server:
// "apiKey": suite.Secret(key). A server hands a key back where nobody thinks to test, so every call is read and LeakReport says what was shown. An
// empty secret is reported, not ignored. Give secrets to the suite Connect returned, not to a stand-in made before it. A secret must be a string
// that is nothing's name and too long to turn up by chance: a password that is also a token's name is found wherever that name is listed.
func (s *Suite) Secret(secret string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case secret == "":
		s.show("a secret given to watch for was empty, so nothing was looked for in its place")
	case !slices.Contains(s.secrets, secret):
		s.secrets = append(s.secrets, secret)
	}

	return secret
}

// LeakReport is what TestMain prints of the secrets given (Secret): "" when none was shown, else each place one was, with the tool and where in its
// words, the secret itself left out. It holds for a filtered run too. A suite that called tools and was given no secret reports that rather than "",
// since that is what a secret given to a stand-in looks like.
func (s *Suite) LeakReport() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.shown) == 0 {
		if len(s.secrets) == 0 && len(s.called) > 0 {
			return "\nthe suite called tools and was given no secret to watch for, so nothing they said was read: give the suite Connect returned its secrets (Secret)\n"
		}

		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n%d place(s) in what the tools said carried a credential the suite gave the server, or the server's own:\n", len(s.shown))
	for _, place := range s.shown {
		fmt.Fprintln(&b, "  "+place)
	}
	fmt.Fprintln(&b, "no tool may show one: blank it where the tool reads what the server said")

	return b.String()
}

// hidden stands in a report for the secret that was found.
const hidden = "[the secret]"

// said is a piece of text a tool said, and where.
type said struct {
	where, text string
}

// watch reads a call's answer or refusal for each secret and keeps the first place each was found.
func (s *Suite) watch(name string, res *mcp.CallToolResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.secrets) == 0 {
		return
	}

	var all []said
	if res != nil {
		all = everyText(res.StructuredContent, "", all)

		where := "in its text"
		if res.IsError {
			where = "in its refusal"
		}
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				all = append(all, said{where, tc.Text})
			}
		}
	}
	if err != nil {
		all = append(all, said{"in its failure", err.Error()})
	}

	for _, secret := range s.secrets {
		for _, piece := range all {
			at := strings.Index(piece.text, secret)
			if at < 0 {
				continue
			}
			s.show(name + ", " + piece.where + ": ..." + strings.ToValidUTF8(piece.text[max(0, at-120):at]+hidden+piece.text[at+len(secret):min(len(piece.text), at+len(secret)+60)], "") + "...")

			break
		}
	}
}

// show keeps a place a secret was found, once.
func (s *Suite) show(place string) {
	if !slices.Contains(s.shown, place) {
		s.shown = append(s.shown, place)
	}
}

// everyText adds to into every piece of text in a decoded answer, a map's names included, each with the names it is under; a list's rows share one
// name so the same field of two rows is one place.
func everyText(v any, under string, into []said) []said {
	switch v := v.(type) {
	case string:
		into = append(into, said{"under " + under, v})
	case []any:
		for _, held := range v {
			into = everyText(held, under+"[]", into)
		}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(v)) {
			below := name
			if under != "" {
				below = under + "." + name
			}
			into = everyText(v[name], below, append(into, said{"as a name under " + below, name}))
		}
	}

	return into
}

// WholeRun reports whether every test was asked for: under -run or -skip an uncalled tool says nothing about coverage.
func WholeRun() bool {
	return unfiltered(func(name string) string {
		if f := flag.Lookup(name); f != nil {
			return f.Value.String()
		}

		return ""
	})
}

// unfiltered reports whether neither flag that narrows a run is set.
func unfiltered(flagValue func(name string) string) bool {
	return flagValue("test.run") == "" && flagValue("test.skip") == ""
}

// retryWaits is how long Retried waits before each try, about a minute in all: one server refused a removal for over ten seconds after a scan.
var retryWaits = []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second, 15 * time.Second}

// Retried calls a tool until it succeeds, for about a minute: a server can refuse a write while its own work holds the record. It returns the last
// error.
func (s *Suite) Retried(tool string, args map[string]any) error {
	return s.retryWith(retryWaits, tool, args)
}

func (s *Suite) retryWith(waits []time.Duration, tool string, args map[string]any) error {
	var err error
	for _, wait := range waits {
		time.Sleep(wait)
		if _, err = s.Invoke(tool, args); err == nil {
			return nil
		}
	}

	return err
}

// PutBack calls a tool once the test ends to undo what it changed, and reports one that never succeeds.
func (s *Suite) PutBack(t *testing.T, tool string, args map[string]any) {
	t.Helper()

	t.Cleanup(func() { s.Undo(t, tool, args) })
}

// Undo is PutBack for a call made in a clean-up already under way.
func (s *Suite) Undo(t *testing.T, tool string, args map[string]any) {
	t.Helper()

	if err := s.Retried(tool, args); err != nil {
		t.Errorf("putting back with %s %v: %v", tool, args, err)
	}
}

// DeleteLaterIfThere deletes what a test made once it ends, unless the test already did: an answer containing gone ("no collection named") is fine,
// any other failure is reported.
func (s *Suite) DeleteLaterIfThere(t *testing.T, tool string, args map[string]any, gone string) {
	t.Helper()

	t.Cleanup(func() {
		if _, err := s.Invoke(tool, args); err == nil || strings.Contains(err.Error(), gone) {
			return
		}
		s.Undo(t, tool, args)
	})
}
