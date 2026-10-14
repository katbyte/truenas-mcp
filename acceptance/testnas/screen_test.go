package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeConsole stands in for the console of the machine a NAS runs in, at a socket: it greets and prompts as QEMU's does, says back what it is
// told, and saves a picture of the screen when asked, unless refuse is set, when it says that instead. It hands over what it was told.
func fakeConsole(t *testing.T, refuse string) (socket string, told <-chan string) {
	t.Helper()

	// the test's name is kept short, as a socket's path may only be so long
	socket = filepath.Join(t.TempDir(), "m")
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	heard := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = conn.Write([]byte("QEMU 9.2.0 monitor - type 'help' for more information\r\n(qemu) "))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return
		}
		command := strings.TrimSpace(line)
		heard <- command

		answer := command + "\r\n"
		if file, ok := strings.CutPrefix(command, "screendump "); ok && refuse == "" {
			_ = os.WriteFile(file, []byte("P6\n1 1\n255\n\x00\x00\x00"), 0o600)
		} else {
			answer += refuse + "\r\n"
		}
		_, _ = conn.Write([]byte(answer + "(qemu) "))
	}()

	return socket, heard
}

// The machine is asked at its console for a picture of its screen, and the picture is said to be there once it is.
func TestTheScreenIsSaved(t *testing.T) {
	t.Parallel()

	socket, told := fakeConsole(t, "")
	file := filepath.Join(t.TempDir(), "screen.ppm")
	if err := screen(t.Context(), socket, file); err != nil {
		t.Fatalf("asking for a picture of the screen: %v", err)
	}

	if command := <-told; command != "screendump "+file {
		t.Errorf("the console was told %q; want it asked to save its screen to %s", command, file)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("no picture of the screen: %v", err)
	}
}

// A console that saves no picture answers with its prompt all the same, so what it said is the error, and no picture is claimed.
func TestAScreenNotSavedSaysWhy(t *testing.T) {
	t.Parallel()

	socket, _ := fakeConsole(t, "could not open 'screen.ppm': Permission denied")
	err := screen(t.Context(), socket, filepath.Join(t.TempDir(), "screen.ppm"))
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("a picture the console would not save is %v; want what the console said", err)
	}
}
