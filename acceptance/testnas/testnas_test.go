package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

const bootBytes = 16 << 30

// lines collects what a function logs, for a test to read.
type lines struct {
	mu   sync.Mutex
	said []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.said = append(l.said, fmt.Sprintf(format, args...))
}

func (l *lines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return strings.Join(l.said, "\n")
}

// fakeInstaller stands in for TrueNAS's installer: it speaks the installer's protocol as its source has it, lists the disks it is given, and
// records what it was asked to install.
type fakeInstaller struct {
	disks  []installerDisk
	refuse string

	mu        sync.Mutex
	installed []json.RawMessage
	shutDown  bool
}

// send writes one message of the installer's to whoever is connected.
func send(ctx context.Context, ws *websocket.Conn, message map[string]any) {
	out, err := json.Marshal(message)
	if err == nil {
		_ = ws.Write(ctx, websocket.MessageText, out)
	}
}

// asked is what the fake was asked to install, and whether it was told to switch the machine off.
func (f *fakeInstaller) asked() (installed []json.RawMessage, shutDown bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.installed), f.shutDown
}

func (f *fakeInstaller) serve(t *testing.T) (address string) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.NotFound(w, r)

			return
		}

		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()

		for {
			_, frame, err := ws.Read(r.Context())
			if err != nil {
				return
			}

			var call struct {
				ID     int64             `json:"id"`
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if json.Unmarshal(frame, &call) != nil {
				return
			}

			switch call.Method {
			case "system_info":
				send(r.Context(), ws, map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{"version": "25.10.7", "efi": false, "installation_running": false, "installation_completed": false, "installation_error": nil}})
			case "list_disks":
				send(r.Context(), ws, map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": f.disks})
			case "install":
				f.mu.Lock()
				f.installed = append(f.installed, call.Params...)
				f.mu.Unlock()

				if f.refuse != "" {
					send(r.Context(), ws, map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32001, "message": f.refuse}})

					continue
				}

				// the installer says how far along it is as a call of its own, with no number on it, before it answers
				for _, said := range []map[string]any{{"progress": 0, "message": "Creating boot pool"}, {"progress": 0.5, "message": "Extracting"}} {
					send(r.Context(), ws, map[string]any{"jsonrpc": "2.0", "method": "installation_progress", "params": []any{said}})
				}
				send(r.Context(), ws, map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": nil})
			case "shutdown":
				// the machine goes down under the call, and nothing is answered
				f.mu.Lock()
				f.shutDown = true
				f.mu.Unlock()

				return
			}
		}
	}))
	t.Cleanup(server.Close)

	return "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
}

// TrueNAS is only documented as installed by someone at a keyboard, and the machine the live tests make one on has nobody at it. The installer
// answers a small API of its own while it waits at its menu, and that is driven: the right disk picked, the administrator's password set, the
// machine switched off after.
func TestTheInstallerIsDrivenWithNobodyAtTheKeyboard(t *testing.T) {
	t.Parallel()

	data := installerDisk{Size: 10 << 30, Model: "QEMU HARDDISK"}
	fake := &fakeInstaller{disks: []installerDisk{{Name: "vdb", Size: data.Size}, {Name: "vda", Size: bootBytes}, {Name: "vdc", Size: data.Size}}}

	log := &lines{}
	if err := install(t.Context(), fake.serve(t), "a-password-for-the-test", bootBytes, log.logf); err != nil {
		t.Fatal(err)
	}

	installed, shutDown := fake.asked()
	if len(installed) != 1 || !shutDown {
		t.Fatalf("the installer was asked to install %d times and switched off: %v", len(installed), shutDown)
	}

	var asked struct {
		Disks          []string `json:"disks"`
		SetPMBR        *bool    `json:"set_pmbr"`
		Authentication struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"authentication"`
	}
	if err := json.Unmarshal(installed[0], &asked); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked.Disks, []string{"vda"}) || asked.SetPMBR == nil || *asked.SetPMBR || asked.Authentication.Username != "truenas_admin" || asked.Authentication.Password != "a-password-for-the-test" {
		t.Errorf("the install was asked for as %s, want the one disk of the boot disk's size and the administrator with the password", installed[0])
	}
	if said := log.text(); !strings.Contains(said, "installing onto vda") || !strings.Contains(said, "50% Extracting") {
		t.Errorf("what the install said of itself was not passed on:\n%s", said)
	}
}

// A disk is installed onto by being wiped. When it is not plain which disk is meant, nothing is installed and the disks are named; and what the
// installer refuses is said in its words.
func TestAnInstallThatCannotBeSureOfItsDiskOrIsRefusedSaysSo(t *testing.T) {
	t.Parallel()

	two := &fakeInstaller{disks: []installerDisk{{Name: "vda", Size: bootBytes}, {Name: "vdb", Size: bootBytes}}}
	err := install(t.Context(), two.serve(t), "a-password-for-the-test", bootBytes, (&lines{}).logf)
	if installed, _ := two.asked(); err == nil || !strings.Contains(err.Error(), "vda") || !strings.Contains(err.Error(), "vdb") || len(installed) != 0 {
		t.Errorf("with two disks it could be, the install answered %v after asking %d times, want both named and nothing installed", err, len(installed))
	}

	refusing := &fakeInstaller{disks: []installerDisk{{Name: "vda", Size: bootBytes}}, refuse: "Disk 'vda' is in use"}
	if err := install(t.Context(), refusing.serve(t), "a-password-for-the-test", bootBytes, (&lines{}).logf); err == nil || !strings.Contains(err.Error(), "is in use") {
		t.Errorf("an install the installer refused answered %v", err)
	}
}

// freshNAS is a stand-in for a NAS straight out of its installer, with the four disks the test machine is given, that takes everything the setup
// asks of it.
func freshNAS(t *testing.T, pools []any) *truenastest.NAS {
	t.Helper()

	nas := truenastest.New(t)
	nas.Answer("system.info", map[string]any{"version": "25.10.7", "hostname": "truenas"})
	nas.Answer("pool.query", pools)
	nas.Answer("network.configuration.update", map[string]any{"hostname": layout.Hostname})
	nas.Answer("disk.details", []any{map[string]any{"name": "vde", "serial": layout.DiskSerial + "4"}, map[string]any{"name": "vdc", "serial": layout.DiskSerial + "2"}, map[string]any{"name": "vdb", "serial": layout.DiskSerial + "1"}, map[string]any{"name": "vdd", "serial": layout.DiskSerial + "3"}})
	nas.Job("pool.create", func([]json.RawMessage) (any, error) { return map[string]any{"name": layout.Pool}, nil })
	nas.Answer("pool.dataset.create", map[string]any{})
	nas.Answer("group.query", []any{map[string]any{"id": 44, "group": readOnlyGroup}})
	nas.Answer("user.create", 71)
	nas.Handle("api_key.create", func(params []json.RawMessage) (any, error) {
		var asked struct {
			Username string `json:"username"`
		}
		_ = json.Unmarshal(params[0], &asked)

		return map[string]any{"key": "5-aKeyMadeFor" + asked.Username}, nil
	})

	return nas
}

func administrator(t *testing.T, nas *truenastest.NAS) *truenasrpc.Conn {
	t.Helper()

	conn, err := truenasrpc.New(nas.Address(), truenasrpc.Password(nas.Username(), nas.Password()), truenasrpc.WithRootCAs(nas.Roots()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// The live tests count on finding the same NAS every time: its name, its pool of two particular disks, its datasets, and two keys, one of them
// for a user who may only read. This is the setup run against a stand-in, so that what it asks for is known before a real NAS is asked.
func TestATestNASIsSetUpFromOneFreshOutOfItsInstaller(t *testing.T) {
	t.Parallel()

	nas := freshNAS(t, []any{})

	out, err := provision(t.Context(), administrator(t, nas), (&lines{}).logf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Key != "5-aKeyMadeFor"+layout.AdminUser || out.ReadOnlyKey != "5-aKeyMadeFor"+layout.ReadOnlyUser || out.Version != "25.10.7" {
		t.Errorf("the setup came to %+v, want a key for each of the two users", out)
	}

	for method, want := range map[string]string{
		"network.configuration.update": `"hostname":"` + layout.Hostname + `"`,
		"pool.create":                  `"data":[{"disks":["vdb","vdc"],"type":"MIRROR"}]`,
		"user.create":                  `"groups":[44]`,
	} {
		calls := nas.Calls(method)
		if len(calls) != 1 || !strings.Contains(string(calls[0].Params[0]), want) {
			t.Errorf("%s was called with %+v, want once with %s", method, calls, want)
		}
	}
	if made := nas.Calls("pool.dataset.create"); len(made) != len(layout.Datasets) {
		t.Errorf("%d datasets were made, want %d", len(made), len(layout.Datasets))
	}
}

// The setup makes a pool and a user and renames the NAS. Pointed by mistake at a NAS that is in use, it must change nothing: a NAS with a pool is
// not one fresh from its installer, and is refused before anything is asked of it but what it has.
func TestANASThatAlreadyHasAPoolIsLeftAlone(t *testing.T) {
	t.Parallel()

	nas := freshNAS(t, []any{map[string]any{"name": "somebodys-pool"}})

	_, err := provision(t.Context(), administrator(t, nas), (&lines{}).logf)
	if err == nil || !strings.Contains(err.Error(), "already has 1 pools") {
		t.Fatalf("a NAS with a pool was answered %v, want it refused", err)
	}
	if changed := nas.Calls("network.configuration.update", "pool.create", "pool.dataset.create", "user.create", "api_key.create"); len(changed) != 0 {
		t.Errorf("a NAS with a pool was changed: %+v", changed)
	}
}

// The setup is first run for real on a machine nobody is watching, against an API whose argument names are easy to get one letter wrong. Every
// call it makes is held to the NAS's own description: that the method is there, that it is a job where the setup waits for one, and that no
// argument is given a name the method does not take or left without one it needs.
func TestEveryCallTheSetupMakesIsOneTheNASDescribes(t *testing.T) {
	t.Parallel()

	documents, err := filepath.Glob("../../api-defs/truenas-methods-*.json")
	if err != nil || len(documents) == 0 {
		t.Fatalf("no description of the API under api-defs (%v)", err)
	}

	calls := []setupCall{
		callSystemInfo(), callPools(), callSetHostname(), callUnusedDisks(), callCreatePool([]string{"vdb", "vdc"}), callCreateDataset(layout.Datasets[0]),
		callCreateKey("a-key", layout.AdminUser), callFindGroup(readOnlyGroup), callCreateUser(layout.ReadOnlyUser, 44, "a-password"),
	}

	for _, document := range documents {
		raw, err := os.ReadFile(document) //nolint:gosec // G304: the files are the repository's own, found by the glob above
		if err != nil {
			t.Fatal(err)
		}

		var methods map[string]struct {
			Job     bool              `json:"job"`
			Accepts []json.RawMessage `json:"accepts"`
		}
		if err := json.Unmarshal(raw, &methods); err != nil {
			t.Fatalf("%s: %v", document, err)
		}

		for _, call := range calls {
			described, ok := methods[call.Method]
			if !ok {
				t.Errorf("%s: the NAS has no method %s", document, call.Method)

				continue
			}
			if described.Job != call.Job {
				t.Errorf("%s: %s is a job: %v, and the setup treats it as one: %v", document, call.Method, described.Job, call.Job)
			}
			if len(call.Params) > len(described.Accepts) {
				t.Errorf("%s: %s is given %d arguments and takes %d", document, call.Method, len(call.Params), len(described.Accepts))

				continue
			}

			for at, param := range call.Params {
				given, isObject := param.(map[string]any)
				if !isObject {
					continue
				}
				if problem := fits(given, described.Accepts[at]); problem != "" {
					t.Errorf("%s: %s, argument %d: %s", document, call.Method, at+1, problem)
				}
			}
		}
	}
}

// fits says what is wrong with an object given for an argument, by the description of what the argument takes, or nothing when it fits one of the
// shapes described: a name the shape has not got, or one it needs that is missing.
func fits(given map[string]any, described json.RawMessage) string {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		AnyOf      []json.RawMessage          `json:"anyOf"`
		OneOf      []json.RawMessage          `json:"oneOf"`
	}
	if json.Unmarshal(described, &schema) != nil {
		return "its description cannot be read"
	}

	if schema.Properties == nil {
		problems := []string{}
		for _, option := range append(schema.AnyOf, schema.OneOf...) {
			problem := fits(given, option)
			if problem == "" {
				return ""
			}
			problems = append(problems, problem)
		}

		return "it fits none of the shapes described: " + strings.Join(problems, "; ")
	}

	for name := range given {
		if _, known := schema.Properties[name]; !known {
			return "it has no " + name
		}
	}
	for _, name := range schema.Required {
		if _, has := given[name]; !has {
			return "it needs " + name
		}
	}

	return ""
}

// What a test needs to log in is handed on as lines a shell reads, to this repository's tests and to the exporter's, and a value with a quote in
// it must not break the line or run as a command.
func TestWhatATestNeedsIsHandedOnAsLinesAShellReads(t *testing.T) {
	t.Parallel()

	got := exports("https://127.0.0.1:18443", "it's a password", "/tmp/nas.pem", &made{Version: "25.10.7", Key: "5-admin", ReadOnlyKey: "6-readonly"})

	for _, want := range []string{
		`export TRUENAS_TEST_SERVER='https://127.0.0.1:18443'`, `export TRUENAS_TEST_PASSWORD='it'\''s a password'`, `export TRUENAS_TEST_TOKEN='5-admin'`,
		`export TRUENAS_TEST_HOSTNAME='` + layout.Hostname + `'`, `export TRUENAS_TEST_READONLY_TOKEN='6-readonly'`, `export TRUENAS_TEST_CERTIFICATE_FILE='/tmp/nas.pem'`,
		`export TRUENAS_METRICS_TEST_USERNAME='` + layout.ReadOnlyUser + `'`, `export TRUENAS_METRICS_TEST_API_KEY='6-readonly'`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the lines handed on lack %s:\n%s", want, strings.Join(got, "\n"))
		}
	}
}

// The NAS's certificate is taken on first sight and then held to, so what is taken has to be the certificate the NAS shows.
func TestTheCertificateTakenIsTheOneTheNASShows(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)

	got, err := certificateOf(context.Background(), nas.Address())
	if err != nil || !bytes.Equal(got, nas.CertificatePEM()) {
		t.Errorf("the certificate taken is not the one shown (%v)", err)
	}
	if _, err := certificateOf(t.Context(), "not an address"); err == nil {
		t.Error("something that is not an address was taken for a NAS")
	}
}
