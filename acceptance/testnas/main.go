// Command testnas is the part of making a throwaway TrueNAS that a shell cannot do. .scripts/testenv.sh runs the virtual machine and calls this
// for the conversations: with TrueNAS's installer, which is driven with nobody at the keyboard, and with the NAS that results.
//
//	testnas install   -installer ws://127.0.0.1:18080/ws -boot-bytes 17179869184
//	testnas wait      -server https://127.0.0.1:18443 -certificate-file nas.pem
//	testnas provision -server https://127.0.0.1:18443 -certificate-file nas.pem
//	testnas screen    -monitor run/monitor.sock -file run/screen.ppm
//
// wait leaves the NAS as its installer left it, for the live tests to set up themselves, which is the first and hardest thing they test.
// provision sets it up (acceptance/setup), for anything else that wants a NAS that is ready. Both print, as export lines, what is needed to
// log in. The administrator's password is read from TRUENAS_TEST_PASSWORD. With TRUENAS_TEST_FURNISHED=1, provision also gives the NAS one of
// each thing a NAS is watched for (shares, a target, a machine, tasks in each state a task can be left in), for whatever wants more to look
// at than a pool: acceptance/layout says what.
//
// screen has the machine save a picture of its screen, which is where a TrueNAS that will not start says why.
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
	"github.com/katbyte/truenas-mcp/acceptance/setup"
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
		return errors.New("say what to do: install, wait, provision or screen")
	}

	// a picture of the screen is asked of the machine, not the NAS, and needs no password
	password := os.Getenv("TRUENAS_TEST_PASSWORD")
	if len(password) < 6 && args[0] != "screen" {
		return errors.New("TRUENAS_TEST_PASSWORD must hold the administrator's password, six characters or more")
	}

	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	wait := flags.Duration("timeout", time.Hour, "how long the whole of it may take")

	switch args[0] {
	case "install":
		address := flags.String("installer", "ws://127.0.0.1:18080/ws", "where the installer answers")
		bootBytes := flags.Int64("boot-bytes", 0, "the size in bytes of the one disk to install onto")
		answerWithin := flags.Duration("answer-within", 10*time.Minute, "how long the installer has to start answering")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *bootBytes <= 0 {
			return errors.New("-boot-bytes must be the size of the disk to install onto")
		}

		ctx, cancel := context.WithTimeout(context.Background(), *wait)
		defer cancel()

		return install(ctx, *address, password, *bootBytes, *answerWithin, logf)

	case "wait", "provision":
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

		return ready(ctx, *server, password, *certificate, args[0] == "provision")

	case "screen":
		monitor := flags.String("monitor", "", "the machine's console")
		file := flags.String("file", "", "where the machine is to save a picture of its screen")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *monitor == "" || *file == "" {
			return errors.New("-monitor must say where the machine's console is, and -file where to save the picture")
		}

		ctx, cancel := context.WithTimeout(context.Background(), *wait)
		defer cancel()

		return screen(ctx, *monitor, *file)

	default:
		return fmt.Errorf("%s is not install, wait, provision or screen", args[0])
	}
}

// furnished is the variable that asks provision for a furnished NAS.
const furnished = "TRUENAS_TEST_FURNISHED"

// ready waits for a newly installed NAS to come up and takes its certificate, sets it up if asked to, and prints what is needed to log into it.
func ready(ctx context.Context, server, password, certificate string, setUp bool) error {
	conn, err := waitFor(ctx, server, password, certificate)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	n := &setup.NAS{Conn: conn, Logf: logf}
	if err := n.Disposable(ctx); err != nil {
		return err
	}

	var made *setup.Made
	if setUp {
		if made, err = n.All(ctx); err != nil {
			return err
		}

		// asked for, the NAS is also given one of each thing a NAS is watched for
		if os.Getenv(furnished) == "1" {
			if err := n.Furnish(ctx); err != nil {
				return err
			}
		}
	}

	for _, line := range exports(server, password, certificate, made) {
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

// exports is what is needed to log into the test NAS, as lines a shell takes: the address, the certificate and the administrator's password
// always, and for a NAS that was set up, its keys too, under this repository's names and under those the truenas-exporter's live suite reads,
// so that both run off the one NAS. The exporter is given the user who may only read. It is also handed them under the names it read as
// truenas-metrics, until the truenas-mcp version it takes this script from is one that hands it the new ones.
func exports(server, password, certificate string, made *setup.Made) []string {
	values := [][2]string{
		{"TRUENAS_TEST_SERVER", server},
		{"TRUENAS_TEST_USERNAME", layout.AdminUser},
		{"TRUENAS_TEST_PASSWORD", password},
		{"TRUENAS_TEST_CERTIFICATE_FILE", certificate},
	}
	if made != nil {
		values = append(values, [][2]string{
			{"TRUENAS_TEST_VERSION", made.Version},
			{"TRUENAS_TEST_HOSTNAME", layout.Hostname},
			{"TRUENAS_TEST_TOKEN", made.Key},
			{"TRUENAS_TEST_READONLY_USERNAME", layout.ReadOnlyUser},
			{"TRUENAS_TEST_READONLY_TOKEN", made.ReadOnlyKey},
			{"TRUENAS_EXPORTER_TEST_SERVER", server},
			{"TRUENAS_EXPORTER_TEST_USERNAME", layout.ReadOnlyUser},
			{"TRUENAS_EXPORTER_TEST_API_KEY", made.ReadOnlyKey},
			{"TRUENAS_EXPORTER_TEST_CERTIFICATE_FILE", certificate},
			{"TRUENAS_METRICS_TEST_SERVER", server},
			{"TRUENAS_METRICS_TEST_USERNAME", layout.ReadOnlyUser},
			{"TRUENAS_METRICS_TEST_API_KEY", made.ReadOnlyKey},
			{"TRUENAS_METRICS_TEST_CERTIFICATE_FILE", certificate},
		}...)
	}

	lines := make([]string, 0, len(values))
	for _, v := range values {
		lines = append(lines, "export "+v[0]+"='"+strings.ReplaceAll(v[1], "'", `'\''`)+"'")
	}

	return lines
}
