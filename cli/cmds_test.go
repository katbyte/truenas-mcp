package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenastest"
	"github.com/spf13/viper"
)

// run executes the command tree the binary builds and returns what it printed. viper is global, so none of the tests that use it can run in parallel.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)
	// a developer's own ~/.truenas-mcp must not leak into the run: with a real NAS configured, info would connect to it
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	root, err := Make()
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err = root.Execute()

	return out.String(), err
}

// said is what a stand-in NAS answers when asked what it runs.
var said = map[string]any{"version": "25.10.7", "hostname": "stand-in"}

// standIn is a stand-in NAS that answers info, and the file a command is told of its certificate by.
func standIn(t *testing.T) (nas *truenastest.NAS, caFile string) {
	t.Helper()

	nas = truenastest.New(t)
	nas.Answer("system.info", said)

	caFile = filepath.Join(t.TempDir(), "nas.pem")
	if err := os.WriteFile(caFile, nas.CertificatePEM(), 0o600); err != nil {
		t.Fatal(err)
	}

	return nas, caFile
}

//nolint:paralleltest // viper is global state; Make mutates it
func TestMakeBuildsTheCommands(t *testing.T) {
	out, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, want := range []string{"info", "version"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q is missing from --help:\n%s", want, out)
		}
	}

	out, err = run(t)
	if err != nil || !strings.Contains(out, "truenas-mcp help") {
		t.Errorf("bare invocation = %q, %v", out, err)
	}
}

//nolint:paralleltest // viper is global state; Make mutates it
func TestVersionCommand(t *testing.T) {
	out, err := run(t, "version")
	if err != nil || !strings.HasPrefix(out, "truenas-mcp ") {
		t.Errorf("version = %q, %v", out, err)
	}
}

// A command that talks to the NAS refuses to start without an address and a key, before any connection is attempted.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestInfoNeedsAnAddressAndAKey(t *testing.T) {
	_, err := run(t, "info")
	if err == nil || !strings.Contains(err.Error(), "server") {
		t.Errorf("info with nothing configured = %v, want a complaint about server", err)
	}

	_, err = run(t, "info", "--server", testServer)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("info with no token = %v, want a complaint about token", err)
	}
}

// info is how someone finds out their settings are right, so it has to go the whole way: reach the NAS through the certificate file it was given,
// log in with the key, and print what the NAS says it runs.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestInfoLogsInAndPrintsTheVersionTheNASRuns(t *testing.T) {
	nas, caFile := standIn(t)

	out, err := run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token", nas.Key(), "--ca-file", caFile)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !strings.HasPrefix(out, "TrueNAS 25.10.7 at wss://127.0.0.1:") || !strings.HasSuffix(out, "/api/current\n") {
		t.Errorf("info printed %q, want the version and the address it was read from", out)
	}
	if strings.Contains(out, nas.Key()) {
		t.Errorf("info printed the key: %q", out)
	}
	if len(nas.Calls("auth.login_ex")) != 1 {
		t.Errorf("with a user name the NAS was logged into with %+v, want the login that names the user", nas.Calls())
	}
}

// Without a user name the key is logged in the older way, so that a setup with only an address and a key works on the versions that allow it.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestInfoWithNoUserNameLogsInWithTheKeyAlone(t *testing.T) {
	nas, caFile := standIn(t)

	if _, err := run(t, "info", "--server", nas.Address(), "--token", nas.Key(), "--ca-file", caFile); err != nil {
		t.Fatalf("info: %v", err)
	}
	if len(nas.Calls("auth.login_with_api_key")) != 1 || len(nas.Calls("auth.login_ex")) != 0 {
		t.Errorf("with no user name the NAS was logged into with %+v, want the key alone", nas.Calls())
	}
}

// A NAS's certificate is its own, so the first run of info usually fails on it. The failure says what to do, a wrong key says it is the key, and
// neither prints the key.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestInfoSaysWhatIsWrongWithTheCertificateOrTheKey(t *testing.T) {
	nas, caFile := standIn(t)

	_, err := run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token", nas.Key())
	if err == nil || !strings.Contains(err.Error(), "--certificate-file, --ca-file or --skip-certificate-check") || strings.Contains(err.Error(), nas.Key()) {
		t.Errorf("info with no certificate file = %v, want the flags that answer it and no key", err)
	}

	_, err = run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token", nas.Key(), "--skip-certificate-check")
	if err == nil || !strings.Contains(err.Error(), "--send-key-unchecked") || strings.Contains(err.Error(), nas.Key()) {
		t.Errorf("info with the check switched off, on a NAS that wants the key as it is = %v, want the key kept back and the flag that sends it named", err)
	}

	if _, err := run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token", nas.Key(), "--skip-certificate-check", "--send-key-unchecked"); err != nil {
		t.Errorf("info with the check switched off and the key allowed to go: %v", err)
	}

	const wrong = "9-notTheKeyThatWasMade"
	_, err = run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token", wrong, "--ca-file", caFile)
	if err == nil || !strings.Contains(err.Error(), "refused the API key") || strings.Contains(err.Error(), wrong) {
		t.Errorf("info with a wrong key = %v, want it to say the key was refused without quoting it", err)
	}
}

// A TrueNAS from version 26 on is logged into without the key being sent, so switching the certificate check off needs nothing more; the key can
// come from a file, as a secret is handed to a container; and a NAS behind something that shows its own certificate is told the flag for that.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestInfoLogsIntoANewerNASFromAKeyFileWithoutSendingTheKey(t *testing.T) {
	nas := truenastest.New(t, truenastest.Version26())
	nas.Answer("system.info", said)
	_, secret, _ := strings.Cut(nas.Key(), "-")

	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(nas.Key()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token-file", keyFile, "--skip-certificate-check")
	if err != nil || !strings.HasPrefix(out, "TrueNAS 25.10.7 at ") {
		t.Fatalf("info with a key file and the check switched off = %q, %v", out, err)
	}
	for _, call := range nas.Calls() {
		for _, param := range call.Params {
			if strings.Contains(string(param), secret) {
				t.Errorf("the key was sent in %s", call.Method)
			}
		}
	}

	_, err = run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token", nas.Key(), "--token-file", keyFile, "--skip-certificate-check")
	if err == nil || !strings.Contains(err.Error(), "given twice") {
		t.Errorf("info with the key given twice = %v, want it refused", err)
	}

	middle := truenastest.InTheMiddle(t, nas)
	_, err = run(t, "info", "--server", middle, "--username", nas.Username(), "--token-file", keyFile, "--skip-certificate-check")
	if err == nil || !strings.Contains(err.Error(), "--no-channel-binding") {
		t.Errorf("info through something that shows its own certificate = %v, want the flag for it named", err)
	}

	if _, err := run(t, "info", "--server", middle, "--username", nas.Username(), "--token-file", keyFile, "--skip-certificate-check", "--no-channel-binding"); err != nil {
		t.Errorf("info through the same with the login left untied: %v", err)
	}
}

// A NAS still on the certificate it came with shows one made out to "localhost". Named as an authority it fails on the name, with a word about what
// to do; handed over as the NAS's own certificate it is taken.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestInfoTakesTheCertificateANASComesWithWhenHandedItByFile(t *testing.T) {
	nas := truenastest.New(t, truenastest.WithCertificate(truenastest.SelfSigned(t, nil, "localhost", time.Now().AddDate(1, 0, 0))))
	nas.Answer("system.info", said)

	file := filepath.Join(t.TempDir(), "nas.pem")
	if err := os.WriteFile(file, nas.CertificatePEM(), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token", nas.Key(), "--ca-file", file)
	if err == nil || !strings.Contains(err.Error(), "made out to another name") {
		t.Errorf("info with the certificate named as an authority = %v, want a word about the name on it", err)
	}

	out, err := run(t, "info", "--server", nas.Address(), "--username", nas.Username(), "--token", nas.Key(), "--certificate-file", file)
	if err != nil || !strings.HasPrefix(out, "TrueNAS 25.10.7 at ") {
		t.Errorf("info with the NAS's own certificate = %q, %v, want the version", out, err)
	}
}

// Run on the NAS itself, info needs no key: the address is the NAS's own socket, and the NAS can see who is asking.
//
//nolint:paralleltest // viper is global state; Make mutates it
func TestInfoOverTheNASsOwnSocketNeedsNoKey(t *testing.T) {
	nas := truenastest.New(t, truenastest.OnSocket())
	nas.Answer("system.info", said)

	out, err := run(t, "info", "--server", nas.Address())
	if err != nil || !strings.HasPrefix(out, "TrueNAS 25.10.7 at unix:///") {
		t.Fatalf("info over the socket = %q, %v", out, err)
	}
	if len(nas.Calls("auth.login_ex", "auth.login_with_api_key")) != 0 {
		t.Errorf("info over the socket logged in with %+v, want nothing sent", nas.Calls())
	}
}
