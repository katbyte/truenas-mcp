// Command testnas makes a throwaway TrueNAS for the live tests out of a virtual machine: it drives TrueNAS's installer with nobody at the keyboard,
// and then sets the installed NAS up with what the tests expect to find. .scripts/testenv.sh runs the machine and calls this for the two things a
// shell cannot do, which are both conversations over a websocket.
//
//	testnas install   -installer ws://127.0.0.1:18080/ws -boot-bytes 17179869184
//	testnas provision -server https://127.0.0.1:18443 -certificate-file nas.pem
//
// The administrator's password is read from TRUENAS_TEST_PASSWORD. provision prints, as export lines, what a test needs to log in.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "testnas:", err)
		os.Exit(1)
	}
}

func logf(format string, args ...any) { fmt.Fprintf(os.Stderr, "==> "+format+"\n", args...) }

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("say what to do: install or provision")
	}

	password := os.Getenv("TRUENAS_TEST_PASSWORD")
	if len(password) < 6 {
		return errors.New("TRUENAS_TEST_PASSWORD must hold the administrator's password, six characters or more")
	}

	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	wait := flags.Duration("timeout", time.Hour, "how long the whole of it may take")

	switch args[0] {
	case "install":
		address := flags.String("installer", "ws://127.0.0.1:18080/ws", "where the installer answers")
		bootBytes := flags.Int64("boot-bytes", 0, "the size in bytes of the one disk to install onto")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *bootBytes <= 0 {
			return errors.New("-boot-bytes must be the size of the disk to install onto")
		}

		ctx, cancel := context.WithTimeout(context.Background(), *wait)
		defer cancel()

		return install(ctx, *address, password, *bootBytes, logf)

	case "provision":
		server := flags.String("server", "https://127.0.0.1:18443", "the address of the NAS that was installed")
		certificate := flags.String("certificate-file", "", "where to write the certificate the NAS shows")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *certificate == "" {
			return errors.New("-certificate-file must say where to write the NAS's certificate")
		}

		ctx, cancel := context.WithTimeout(context.Background(), *wait)
		defer cancel()

		return setUp(ctx, *server, password, *certificate)

	default:
		return fmt.Errorf("%s is neither install nor provision", args[0])
	}
}

// setUp waits for a newly installed NAS to come up, takes its certificate, sets it up, and prints what a test needs to log into it.
func setUp(ctx context.Context, server, password, certificate string) error {
	conn, err := waitFor(ctx, server, password, certificate)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	out, err := provision(ctx, conn, logf)
	if err != nil {
		return err
	}

	for _, line := range exports(server, password, certificate, out) {
		fmt.Println(line)
	}

	return nil
}

// waitFor is a connection to a NAS that has just been started for the first time, logged in as its administrator: it is tried until the NAS is
// far enough up to take the login, which is some minutes after the machine starts.
func waitFor(ctx context.Context, server, password, certificate string) (*truenasrpc.Conn, error) {
	for try := 0; ; try++ {
		conn, err := connect(ctx, server, password, certificate)
		if err == nil {
			return conn, nil
		}

		if try%6 == 0 {
			logf("waiting for the TrueNAS at %s to come up: %v", server, err)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the TrueNAS at %s never came up: %w (last: %w)", server, context.Cause(ctx), err)
		case <-time.After(10 * time.Second):
		}
	}
}

func connect(ctx context.Context, server, password, certificate string) (*truenasrpc.Conn, error) {
	shown, err := certificateOf(ctx, server)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(certificate, shown, 0o600); err != nil { //nolint:gosec // G703: the path is one the person running the program names
		return nil, err
	}

	// a NAS that is not up yet is asked again here, by the loop above, and not left alone for as long as a program in use would leave it
	conn, err := truenasrpc.New(server, truenasrpc.Password(layout.AdminUser, password), truenasrpc.WithCertificateFile(certificate), truenasrpc.WithRedialWait(func(int) time.Duration { return time.Second }))
	if err != nil {
		return nil, err
	}

	dialling, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := conn.Connect(dialling); err != nil {
		_ = conn.Close()

		return nil, err
	}

	return conn, nil
}

// exports is what a test needs to log into the test NAS, as lines a shell takes: this repository's names, and those the truenas-metrics exporter's
// live suite reads, so that both run off the one NAS. The exporter is given the user who may only read.
func exports(server, password, certificate string, out *made) []string {
	values := [][2]string{
		{"TRUENAS_TEST_SERVER", server},
		{"TRUENAS_TEST_VERSION", out.Version},
		{"TRUENAS_TEST_HOSTNAME", layout.Hostname},
		{"TRUENAS_TEST_USERNAME", layout.AdminUser},
		{"TRUENAS_TEST_PASSWORD", password},
		{"TRUENAS_TEST_TOKEN", out.Key},
		{"TRUENAS_TEST_READONLY_USERNAME", layout.ReadOnlyUser},
		{"TRUENAS_TEST_READONLY_TOKEN", out.ReadOnlyKey},
		{"TRUENAS_TEST_CERTIFICATE_FILE", certificate},
		{"TRUENAS_METRICS_TEST_SERVER", server},
		{"TRUENAS_METRICS_TEST_USERNAME", layout.ReadOnlyUser},
		{"TRUENAS_METRICS_TEST_API_KEY", out.ReadOnlyKey},
		{"TRUENAS_METRICS_TEST_CERTIFICATE_FILE", certificate},
	}

	lines := make([]string, 0, len(values))
	for _, v := range values {
		lines = append(lines, "export "+v[0]+"='"+strings.ReplaceAll(v[1], "'", `'\''`)+"'")
	}

	return lines
}
