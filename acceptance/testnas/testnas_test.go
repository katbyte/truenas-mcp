package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/acceptance/setup"
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
	if err := install(t.Context(), fake.serve(t), "a-password-for-the-test", bootBytes, time.Minute, log.logf); err != nil {
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
	err := install(t.Context(), two.serve(t), "a-password-for-the-test", bootBytes, time.Minute, (&lines{}).logf)
	if installed, _ := two.asked(); err == nil || !strings.Contains(err.Error(), "vda") || !strings.Contains(err.Error(), "vdb") || len(installed) != 0 {
		t.Errorf("with two disks it could be, the install answered %v after asking %d times, want both named and nothing installed", err, len(installed))
	}

	refusing := &fakeInstaller{disks: []installerDisk{{Name: "vda", Size: bootBytes}}, refuse: "Disk 'vda' is in use"}
	if err := install(t.Context(), refusing.serve(t), "a-password-for-the-test", bootBytes, time.Minute, (&lines{}).logf); err == nil || !strings.Contains(err.Error(), "is in use") {
		t.Errorf("an install the installer refused answered %v", err)
	}
}

// A machine that never gets as far as answering is found out in minutes and said to be that, not waited on for as long as an install may take.
func TestAnInstallerThatNeverAnswersIsGivenUpOnSoon(t *testing.T) {
	t.Parallel()

	err := install(t.Context(), "ws://127.0.0.1:1/ws", "a-password-for-the-test", bootBytes, 200*time.Millisecond, (&lines{}).logf)
	if err == nil || !strings.Contains(err.Error(), "never answered") || !strings.Contains(err.Error(), "200ms to start") {
		t.Errorf("an installer that is not there answered %v, want it said never to have answered in the time it had", err)
	}
}

// What a test needs to log in is handed on as lines a shell reads, to this repository's tests and to the exporter's, and a value with a quote in
// it must not break the line or run as a command.
func TestWhatATestNeedsIsHandedOnAsLinesAShellReads(t *testing.T) {
	t.Parallel()

	got := exports("https://127.0.0.1:18443", "it's a password", "/tmp/nas.pem", &setup.Made{Version: "25.10.7", Key: "5-admin", ReadOnlyKey: "6-readonly"})

	for _, want := range []string{
		`export TRUENAS_TEST_SERVER='https://127.0.0.1:18443'`, `export TRUENAS_TEST_PASSWORD='it'\''s a password'`, `export TRUENAS_TEST_TOKEN='5-admin'`,
		`export TRUENAS_TEST_HOSTNAME='` + layout.Hostname + `'`, `export TRUENAS_TEST_READONLY_TOKEN='6-readonly'`, `export TRUENAS_TEST_CERTIFICATE_FILE='/tmp/nas.pem'`,
		`export TRUENAS_EXPORTER_TEST_SERVER='https://127.0.0.1:18443'`, `export TRUENAS_EXPORTER_TEST_USERNAME='` + layout.ReadOnlyUser + `'`, `export TRUENAS_EXPORTER_TEST_API_KEY='6-readonly'`, `export TRUENAS_EXPORTER_TEST_CERTIFICATE_FILE='/tmp/nas.pem'`,
		`export TRUENAS_METRICS_TEST_USERNAME='` + layout.ReadOnlyUser + `'`, `export TRUENAS_METRICS_TEST_API_KEY='6-readonly'`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the lines handed on lack %s:\n%s", want, strings.Join(got, "\n"))
		}
	}

	// a NAS left as its installer left it has no keys to hand on: only what its administrator logs in with
	fresh := strings.Join(exports("https://127.0.0.1:18443", "a-password", "/tmp/nas.pem", nil), "\n")
	if strings.Contains(fresh, "TOKEN") || strings.Contains(fresh, "METRICS") || !strings.Contains(fresh, "export TRUENAS_TEST_PASSWORD='a-password'") {
		t.Errorf("a NAS that was not set up hands on:\n%s", fresh)
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
