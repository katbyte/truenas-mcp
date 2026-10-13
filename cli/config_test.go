package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// viper is global, so none of these can run in parallel and each resets it.
const (
	testServer = "nas.example.net"
	testUser   = "metrics"
	testKey    = "1-aKeyOfSomeLength"
	binary     = "truenas-mcp"
)

// load drives the real flag wiring against a temporary home and working directory.
func load(t *testing.T, home, wd string) *FlagData {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("HOME", home)
	t.Chdir(wd)

	if err := configureFlags(&cobra.Command{Use: binary}); err != nil {
		t.Fatalf("configureFlags: %v", err)
	}

	return GetFlags()
}

func write(t *testing.T, dir, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, ".truenas-mcp"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// on is how a boolean is switched on from the environment.
const on = "true"

// A .truenas-mcp in the working directory is documented as per-project settings, which it only is if it is found before the one in $HOME. viper reads
// the first file it finds.
//
//nolint:paralleltest // viper is global state; these mutate it
func TestAConfigFileInTheWorkingDirectoryWinsOverTheOneAtHome(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()

	t.Run("project wins over home", func(t *testing.T) {
		write(t, home, "SERVER=from-home\n")
		write(t, project, "SERVER=from-project\n")
		if got := load(t, home, project).Server; got != "from-project" {
			t.Errorf("server = %q, want the project config", got)
		}
	})

	t.Run("home is used when there is no project config", func(t *testing.T) {
		empty := t.TempDir()
		if got := load(t, home, empty).Server; got != "from-home" {
			t.Errorf("server = %q, want the home config", got)
		}
	})

	t.Run("no config at all is not an error", func(t *testing.T) {
		if got := load(t, t.TempDir(), t.TempDir()).Server; got != "" {
			t.Errorf("server = %q, want empty", got)
		}
	})
}

// The environment beats a config file, so a container can override what is baked into an image.
func TestTheEnvironmentOverridesAConfigFile(t *testing.T) {
	home := t.TempDir()
	write(t, home, "SERVER=from-home\nTOKEN=from-home\n")

	t.Setenv("TRUENAS_SERVER", "from-env")
	f := load(t, home, t.TempDir())

	if f.Server != "from-env" {
		t.Errorf("server = %q, want the environment", f.Server)
	}
	// and a key the environment did not set still comes from the file
	if f.Token != "from-home" {
		t.Errorf("token = %q, want the config file", f.Token)
	}
}

// Every documented TRUENAS_* variable has to actually reach its field; a typo in the binding map is invisible until someone sets the variable and
// nothing happens.
//
//nolint:paralleltest // viper is global state; these mutate it
func TestEveryEnvironmentVariableReachesItsSetting(t *testing.T) {
	dir := t.TempDir()
	for _, env := range []struct {
		key, value string
		check      func(*FlagData) bool
	}{
		{"TRUENAS_SERVER", testServer, func(f *FlagData) bool { return f.Server == testServer }},
		{"TRUENAS_USERNAME", testUser, func(f *FlagData) bool { return f.Username == testUser }},
		{"TRUENAS_TOKEN", "tok", func(f *FlagData) bool { return f.Token == "tok" }},
		{"TRUENAS_CERTIFICATE_FILE", "/etc/nas.pem", func(f *FlagData) bool { return f.CertificateFile == "/etc/nas.pem" }},
		{"TRUENAS_CA_FILE", "/etc/ca.pem", func(f *FlagData) bool { return f.CAFile == "/etc/ca.pem" }},
		{"TRUENAS_SKIP_CERTIFICATE_CHECK", on, func(f *FlagData) bool { return f.SkipCertificateCheck }},
	} {
		t.Setenv(env.key, env.value)
		if f := load(t, dir, dir); !env.check(f) {
			t.Errorf("%s=%s did not reach its field: %+v", env.key, env.value, f)
		}
		_ = os.Unsetenv(env.key)
	}
}

// NewConn is where a missing address or API key is caught, and an address that would get the key revoked, before anything tries to talk to the NAS.
func TestAConnectionThatCannotWorkIsRefusedBeforeAnythingIsDialled(t *testing.T) {
	t.Parallel()

	if _, err := (&FlagData{}).NewConn(); err == nil {
		t.Error("a connection with no server should be refused")
	}
	if _, err := (&FlagData{Server: testServer}).NewConn(); err == nil {
		t.Error("a connection with no API key should be refused")
	}
	if _, err := (&FlagData{Server: "http://" + testServer, Token: testKey}).NewConn(); err == nil {
		t.Error("an address with no TLS should be refused: TrueNAS revokes a key sent over one")
	}
	if _, err := (&FlagData{Server: testServer, Token: testKey, CAFile: "no-such-file.pem"}).NewConn(); err == nil {
		t.Error("a certificate authority file that is not there should be refused")
	}

	for _, f := range []*FlagData{
		{Server: testServer, Token: testKey},
		{Server: testServer, Username: testUser, Token: testKey},
		{Server: "https://" + testServer + ":8443", Username: testUser, Token: testKey, SkipCertificateCheck: true},
	} {
		conn, err := f.NewConn()
		if err != nil {
			t.Errorf("a complete configuration was refused: %v", err)

			continue
		}
		_ = conn.Close()
	}
}
