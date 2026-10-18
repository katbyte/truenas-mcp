package truenasrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/go-kt/httpc"
)

// A setting holds whatever a person typed: a bare host, a host and port, the https address out of a browser's bar. Each must come to the one address
// the API is at, and a path someone gave to pin a version must be kept.
func TestAnAddressIsWorkedOutFromWhatASettingHolds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ given, want, socket string }{
		{"nas.example.net", "wss://nas.example.net/api/current", ""},
		{" nas.example.net ", "wss://nas.example.net/api/current", ""},
		{"nas.example.net:8443", "wss://nas.example.net:8443/api/current", ""},
		{"https://nas.example.net", "wss://nas.example.net/api/current", ""},
		{"https://nas.example.net/", "wss://nas.example.net/api/current", ""},
		{"HTTPS://nas.example.net/ui/dashboard?x=1#", "wss://nas.example.net/ui/dashboard", ""},
		{"wss://nas.example.net/api/v25.10.5", "wss://nas.example.net/api/v25.10.5", ""},
		{"192.0.2.5", "wss://192.0.2.5/api/current", ""},
		{"[2001:db8::5]:444", "wss://[2001:db8::5]:444/api/current", ""},
		// a program on the NAS itself reaches it by its own socket, the usual one when none is named
		{"unix://", "ws://localhost/api/current", DefaultSocket},
		{"unix:///run/middleware/middlewared.sock", "ws://localhost/api/current", "/run/middleware/middlewared.sock"},
		{"ws+unix:///tmp/another.sock", "ws://localhost/api/current", "/tmp/another.sock"},
	} {
		got, socket, err := endpoint(tc.given)
		if err != nil || got != tc.want || socket != tc.socket {
			t.Errorf("%q came to %q over socket %q (%v), want %q over %q", tc.given, got, socket, err, tc.want, tc.socket)
		}
	}

	if _, _, err := endpoint("unix://run/middleware/middlewared.sock"); err == nil || !strings.Contains(err.Error(), "three slashes") {
		t.Errorf("a socket named by a path that does not start at the root = %v, want it refused with how to write one", err)
	}
}

// A NAS that cannot be reached is asked again soon the first time, since most failures are a blip, and then less and less often up to a limit, so
// that one that is off for the night is not dialled thousands of times.
func TestANASThatKeepsFailingIsLeftAloneForLongerEachTimeUpToALimit(t *testing.T) {
	t.Parallel()

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, MaxRedialWait, MaxRedialWait}
	for i, w := range want {
		if got := redialWait(i + 1); got != w {
			t.Errorf("after %d failures the wait is %s, want %s", i+1, got, w)
		}
	}
	if got := redialWait(10_000); got != MaxRedialWait {
		t.Errorf("after ten thousand failures the wait is %s, want %s", got, MaxRedialWait)
	}
}

// What a trace shows of a message, case by case: the rules are easy to get subtly wrong, and a wrong one prints somebody's password.
func TestWhatATraceShowsOfAMessage(t *testing.T) {
	t.Parallel()

	c := &Conn{cred: APIKey("metrics", "3-theConnectionsOwnKeyOfSomeLength"), o: settings{secretNames: []string{"pin"}}}

	for _, tc := range []struct {
		name   string
		method string
		frame  string
		shows  []string
		hides  []string
	}{
		{
			name:   "a login's arguments are left out whole, the user name with them",
			method: "auth.login_ex",
			frame:  `{"jsonrpc":"2.0","id":2,"method":"auth.login_ex","params":[{"mechanism":"API_KEY_PLAIN","username":"metrics","api_key":"3-theConnectionsOwnKeyOfSomeLength"}]}`,
			shows:  []string{`"method":"auth.login_ex"`, `"params":"REDACTED"`},
			hides:  []string{"theConnectionsOwnKey", "metrics", "API_KEY_PLAIN"},
		},
		{
			name:   "a password sent by position, with no name on it",
			method: "auth.login",
			frame:  `{"jsonrpc":"2.0","id":3,"method":"auth.login","params":["admin","positional-password"]}`,
			hides:  []string{"positional-password"},
		},
		{
			name:   "a token answered bare",
			method: "auth.generate_token",
			frame:  `{"jsonrpc":"2.0","id":4,"result":"a-bare-token"}`,
			shows:  []string{`"result":"REDACTED"`},
			hides:  []string{"a-bare-token"},
		},
		{
			name:   "a secret by name, however deep and whatever it holds",
			method: "user.create",
			frame:  `{"jsonrpc":"2.0","id":5,"method":"user.create","params":[{"username":"ada","password":"pw-1","smb":{"smbhash":"hash-1"},"keys":[{"private_key":{"pem":"pem-1"}}],"sshpubkey":"shown-key"}]}`,
			shows:  []string{`"username":"ada"`, "shown-key"},
			hides:  []string{"pw-1", "hash-1", "pem-1"},
		},
		{
			name:   "the names that are TrueNAS's own",
			method: "mail.update",
			frame:  `{"jsonrpc":"2.0","id":6,"result":{"pass":"mail-pw","oauth":{"refresh":"mail-oauth"},"bindpw":"ldap-pw","key":"enc-key","attributes":{"user":"cloud-user"},"values":{"env":"app-secret"},"fromname":"shown-name"}}`,
			shows:  []string{"shown-name"},
			hides:  []string{"mail-pw", "mail-oauth", "ldap-pw", "enc-key", "cloud-user", "app-secret"},
		},
		{
			name:   "a name that is a secret in one part of the API and plain in the rest",
			method: "kerberos.keytab.query",
			frame:  `{"jsonrpc":"2.0","id":11,"result":[{"id":1,"name":"shown-keytab","file":"a2V5dGFiLXNlY3JldA=="}]}`,
			shows:  []string{"shown-keytab"},
			hides:  []string{"a2V5dGFiLXNlY3JldA=="},
		},
		{
			name:   "the same name where it is plain",
			method: "filesystem.stat",
			frame:  `{"jsonrpc":"2.0","id":12,"result":{"file":"shown-path","username":"shown-owner","raw":"shown-raw"}}`,
			shows:  []string{"shown-path", "shown-owner", "shown-raw"},
		},
		{
			name:   "a change the NAS sends unasked is hidden as the list it is about would be",
			method: "",
			frame:  `{"jsonrpc":"2.0","method":"collection_update","params":{"msg":"changed","collection":"kerberos.keytab.query","id":1,"fields":{"name":"shown-event","file":"ZXZlbnQta2V5dGFi"}}}`,
			shows:  []string{"shown-event", "kerberos.keytab.query"},
			hides:  []string{"ZXZlbnQta2V5dGFi"},
		},
		{
			name:   "a name and its value sent side by side",
			method: "app.update",
			frame:  `{"jsonrpc":"2.0","id":7,"method":"app.update","params":["plex",{"env":[{"name":"API_TOKEN","value":"paired-secret"},{"name":"TZ","value":"shown-zone"}]}]}`,
			shows:  []string{"shown-zone", "API_TOKEN"},
			hides:  []string{"paired-secret"},
		},
		{
			name:   "a name the caller added",
			method: "ups.update",
			frame:  `{"jsonrpc":"2.0","id":8,"method":"ups.update","params":[{"pin":"1234-a-pin","port":"shown-port"}]}`,
			shows:  []string{"shown-port"},
			hides:  []string{"1234-a-pin"},
		},
		{
			name:   "the connection's own key, wherever the NAS puts it",
			method: "system.info",
			frame:  `{"jsonrpc":"2.0","id":9,"error":{"code":-32001,"message":"Method call error","data":{"error":22,"errname":"EINVAL","reason":"bad 3-theConnectionsOwnKeyOfSomeLength","trace":{"formatted":"locals: traceback-secret"},"py_exception":"pickled-secret","extra":null}}}`,
			shows:  []string{"EINVAL", "bad REDACTED"},
			hides:  []string{"theConnectionsOwnKey", "traceback-secret", "pickled-secret"},
		},
		{
			name:   "something the NAS sent unasked is treated by the method it names",
			method: "",
			frame:  `{"jsonrpc":"2.0","method":"collection_update","params":{"collection":"user.query","fields":{"username":"ada","password":"event-pw"}}}`,
			shows:  []string{"collection_update", "user.query"},
			hides:  []string{"event-pw"},
		},
		{
			name:   "bytes that are not a message are counted, not printed",
			method: "system.info",
			frame:  `not json, and maybe a secret`,
			shows:  []string{"28 bytes"},
			hides:  []string{"maybe a secret"},
		},
		{
			name:   "numbers come through as they were sent",
			method: "pool.query",
			frame:  `{"jsonrpc":"2.0","id":10,"result":[{"size":98765432109876543210,"free":0.1}]}`,
			shows:  []string{"98765432109876543210", "0.1"},
		},
	} {
		got := c.shown(tc.method, []byte(tc.frame))
		for _, want := range tc.shows {
			if !strings.Contains(got, want) {
				t.Errorf("%s: the trace does not show %s: %s", tc.name, want, got)
			}
		}
		for _, secret := range tc.hides {
			if strings.Contains(got, secret) {
				t.Errorf("%s: the trace shows %s: %s", tc.name, secret, got)
			}
		}
	}
}

// A trace prints no more of a long answer than go-kt's HTTP trace would, says how much it left out, and does not cut a character in half.
func TestALongMessageIsCutShortInATrace(t *testing.T) {
	t.Parallel()

	c := &Conn{}
	long := `{"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("é", httpc.DefaultTraceBody) + `"}`

	got := c.shown("system.info", []byte(long))
	if !strings.Contains(got, "more bytes)") || len(got) > httpc.DefaultTraceBody+40 {
		t.Errorf("a %d byte message is shown as %d bytes ending %q, want it cut near %d with a note of the rest", len(long), len(got), got[len(got)-30:], httpc.DefaultTraceBody)
	}
	if strings.ContainsRune(got, '�') {
		t.Error("the cut split a character in two")
	}
}

// A secret with a name on it is hidden by the name. One sent by position, or answered bare, has none, and is only hidden because its method is on a
// list. This reads the NAS's own description of its API and holds the list to it, so that a method a new version adds cannot quietly print a
// password: every method that takes an argument named like a secret, or answers a bare string its description calls a key, a password or a token.
func TestNoMethodShowsASecretItTakesByPositionOrAnswersBare(t *testing.T) {
	t.Parallel()

	documents, err := filepath.Glob("../../api-defs/truenas-methods-*.json")
	if err != nil || len(documents) == 0 {
		t.Fatalf("no description of the API under api-defs (%v)", err)
	}

	// a bare string these words describe is a secret, but for the ones that are public by nature
	secretWords := regexp.MustCompile(`(?i)password|token|secret|passphrase|\bkey\b`)
	public := map[string]string{"keychaincredential.remote_ssh_host_key_scan": "a host's public key"}

	c := &Conn{}
	const marker = "MARKER-that-must-not-be-shown"

	for _, document := range documents {
		raw, err := os.ReadFile(document) //nolint:gosec // G304: the files are the repository's own, found by the glob above
		if err != nil {
			t.Fatal(err)
		}

		var methods map[string]struct {
			Accepts []struct {
				Name string `json:"_name_"`
			} `json:"accepts"`
			Returns []struct {
				Type        string `json:"type"`
				Description string `json:"description"`
				AnyOf       []struct {
					Type string `json:"type"`
				} `json:"anyOf"`
			} `json:"returns"`
		}
		if err := json.Unmarshal(raw, &methods); err != nil {
			t.Fatalf("%s: %v", document, err)
		}

		checked := 0
		for method, m := range methods {
			params := make([]string, 0, len(m.Accepts))
			secret := false
			for _, p := range m.Accepts {
				value := "shown"
				if c.secretName(p.Name, nil) {
					value, secret = marker, true
				}
				params = append(params, value)
			}
			if secret {
				checked++
				sent, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
				if got := c.shown(method, sent); strings.Contains(got, marker) {
					t.Errorf("%s takes a secret by position and a trace shows it: %s", method, got)
				}
			}

			if len(m.Returns) != 1 || public[method] != "" {
				continue
			}
			r := m.Returns[0]
			bare := r.Type == "string"
			for _, alt := range r.AnyOf {
				bare = bare || alt.Type == "string"
			}
			if bare && secretWords.MatchString(r.Description) {
				checked++
				answered := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, marker)
				if got := c.shown(method, []byte(answered)); strings.Contains(got, marker) {
					t.Errorf("%s answers a bare secret (%q) and a trace shows it: %s", method, r.Description, got)
				}
			}
		}

		// the check is worth nothing if it found nothing to check
		if checked < 8 {
			t.Errorf("%s: only %d methods take or answer a bare secret, where 25.10.7 has eleven: is the description read right?", document, checked)
		}
	}
}

// planted is a value with a marker at one place and nothing else in it: at "$[1].smb[].hash" it is [null, {"smb": [{"hash": marker}]}].
func planted(path, marker string) any {
	var value any = marker

	steps := regexp.MustCompile(`\[\d*\]|\.\*|\.[A-Za-z0-9_-]+`).FindAllString(path, -1)
	for _, step := range slices.Backward(steps) {
		switch {
		case step == "[]":
			value = []any{value}
		case strings.HasPrefix(step, "["):
			at, _ := strconv.Atoi(strings.Trim(step, "[]"))
			list := make([]any, at+1)
			list[at] = value
			value = list
		case step == ".*":
			value = map[string]any{"anything": value}
		default:
			value = map[string]any{step[1:]: value}
		}
	}

	return value
}

// The TrueNAS source marks which values are secrets, and api-defs holds the place of each one, method by method. A trace hides by a list of names
// and a list of methods, which is only right for as long as it covers every one of those places: this plants a marker at each and looks for it in
// what a trace would print, so that a secret under a name nobody thought of fails here and not in somebody's log. The same is done for the
// secrets the source does not mark, which the list keeps apart: a NAS hands those to anyone who may read, so the trace is all that hides them.
func TestEverySecretTheSourceMarksIsHiddenInATrace(t *testing.T) {
	t.Parallel()

	lists, err := filepath.Glob("../../api-defs/truenas-secrets-*.json")
	if err != nil || len(lists) == 0 {
		t.Fatalf("no list of secrets under api-defs (%v)", err)
	}

	c := &Conn{}
	const marker = "MARKER-that-must-not-be-shown"

	for _, list := range lists {
		raw, err := os.ReadFile(list) //nolint:gosec // G304: the files are the repository's own, found by the glob above
		if err != nil {
			t.Fatal(err)
		}

		type where struct {
			Params []string `json:"params"`
			Result []string `json:"result"`
		}
		var secrets struct {
			Methods  map[string]where `json:"methods"`
			Unmarked map[string]where `json:"unmarked"`
		}
		if err := json.Unmarshal(raw, &secrets); err != nil {
			t.Fatalf("%s: %v", list, err)
		}

		// a method is in one list or both: its own places from each are planted the same way
		type listed struct {
			method string
			where
		}
		all := make([]listed, 0, len(secrets.Methods)+len(secrets.Unmarked))
		for method, m := range secrets.Methods {
			all = append(all, listed{method, m})
		}
		for method, m := range secrets.Unmarked {
			all = append(all, listed{method, m})
		}
		if len(secrets.Unmarked) == 0 {
			t.Errorf("%s: no secrets the source does not mark, where 25.10.7 has the SSH server's own keys and a replication's: is the list read right?", list)
		}

		places := 0
		for _, of := range all {
			method, m := of.method, of.where

			for _, path := range m.Params {
				places++
				sent, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": planted(path, marker)})
				if got := c.shown(method, sent); strings.Contains(got, marker) {
					t.Errorf("%s: the secret sent at %s is shown: %s", method, path, got)
				}
			}
			for _, path := range m.Result {
				places++
				answered, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": planted(path, marker)})
				if got := c.shown(method, answered); strings.Contains(got, marker) {
					t.Errorf("%s: the secret answered at %s is shown: %s", method, path, got)
				}
			}

			// a method the NAS runs as a job has what it was called with and what it answered said again, unasked, in the NAS's account of the
			// job, and again in the answer to anyone asking how the job stands: the same places, under another message's name
			for _, at := range []struct {
				part  string
				paths []string
			}{{"arguments", m.Params}, {"result", m.Result}} {
				for _, path := range at.paths {
					account := map[string]any{"id": 5, "method": method, "state": "SUCCESS", at.part: planted(path, marker), "exception": "Traceback: " + marker}

					said, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "collection_update", "params": map[string]any{"msg": "changed", "collection": jobsCollection, "id": 5, "fields": account}})
					if got := c.shown("collection_update", said); strings.Contains(got, marker) {
						t.Errorf("%s: the secret at %s of its job's %s is shown when the NAS says how the job stands: %s", method, path, at.part, got)
					}

					asked, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": []any{account}})
					if got := c.shown(jobsCollection, asked); strings.Contains(got, marker) {
						t.Errorf("%s: the secret at %s of its job's %s is shown when the job is asked after: %s", method, path, at.part, got)
					}
				}
			}
		}

		// the check is worth nothing if the list was empty or read wrong
		if places < 500 {
			t.Errorf("%s: %d secret places, where 25.10.7 has over a thousand: is the list read right?", list, places)
		}
	}
}

// A listener slower than the NAS cannot be allowed to hold up everything behind it, nor to grow without end. Past a point the oldest of what it
// has not been handed is dropped, and the next thing it is handed says how much it missed, so that it knows to ask afresh.
func TestAListenerThatFallsBehindMissesTheOldestAndIsToldHowMany(t *testing.T) {
	t.Parallel()

	var got []Event
	s := &Subscription{name: "alert.list", wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.fn = func(e Event) {
		got = append(got, e)
		if len(got) == maxQueued {
			s.end(nil)
		}
	}

	for i := range maxQueued + 10 {
		s.push(Event{Collection: "alert.list", Kind: Added, ID: json.RawMessage(strconv.Itoa(i))})
	}
	s.run()

	if len(got) != maxQueued || string(got[0].ID) != "10" || got[0].Missed != 10 || got[1].Missed != 0 || string(got[len(got)-1].ID) != strconv.Itoa(maxQueued+9) {
		t.Errorf("a listener %d behind was handed %d events, the first number %s saying %d were missed, the last number %s", maxQueued+10, len(got), got[0].ID, got[0].Missed, got[len(got)-1].ID)
	}

	s.push(Event{Kind: Added})
	if len(s.queue) != 0 {
		t.Error("a subscription that is over still takes events in")
	}
}

// Four calls are a way of making another call: they take its arguments, or answer what it answered, whichever method it is, and two answer an
// address that hands a file to whoever holds it. None of that can be hidden by name, so nothing of what they take or answer is shown.
func TestACallThatCarriesAnotherCallsArgumentsOrAnswerShowsNeither(t *testing.T) {
	t.Parallel()

	c := &Conn{}
	const marker = "MARKER-that-must-not-be-shown"

	for method, params := range map[string][]any{
		"core.bulk":              {"user.set_password", []any{[]any{"admin", marker}}},
		"core.job_wait":          {12},
		"core.download":          {"pool.dataset.export_key", []any{"tank", marker}, "key.json"},
		"core.job_download_logs": {12, "logs.txt"},
	} {
		sent, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if got := c.shown(method, sent); strings.Contains(got, marker) || !strings.Contains(got, method) {
			t.Errorf("%s shows what it was sent: %s", method, got)
		}

		for _, answer := range []any{marker, []any{12, "/_download/12?auth_token=" + marker}, []any{map[string]any{"result": marker, "error": nil}}} {
			answered, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": answer})
			if got := c.shown(method, answered); strings.Contains(got, marker) {
				t.Errorf("%s shows what it answered: %s", method, got)
			}
		}
	}
}

// A socket turned away before TrueNAS's API is reached says so, and where to look: read in a log, a proxy's refusal must not look like the NAS
// turning the key down.
func TestARefusedSocketSaysWhereTheRefusalCameFrom(t *testing.T) {
	t.Parallel()

	dial := errors.New("failed to WebSocket dial: expected handshake response status code 101")
	for status, want := range map[int]string{
		http.StatusForbidden:         "before TrueNAS's API was reached, so the key was never looked at: a proxy or firewall",
		http.StatusUnauthorized:      "before TrueNAS's API was reached",
		http.StatusProxyAuthRequired: "before TrueNAS's API was reached",
		http.StatusBadGateway:        "could not reach the API behind it, as when the NAS is still starting",
		http.StatusGatewayTimeout:    "could not reach the API behind it",
		http.StatusNotFound:          "nothing answers at that path",
		http.StatusTeapot:            "it answered HTTP 418",
	} {
		err := refused(&http.Response{StatusCode: status}, dial)
		if !strings.Contains(err.Error(), want) || !errors.Is(err, dial) {
			t.Errorf("HTTP %d is said as %q, want it to say %q", status, err, want)
		}
	}
}
