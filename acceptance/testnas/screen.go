package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
)

// screen has the machine save a picture of its screen to file, which is where a TrueNAS that will not start says why. It is asked at the
// machine's console, the way a person at it would ask.
func screen(ctx context.Context, monitor, file string) error {
	var dialer net.Dialer
	console, err := dialer.DialContext(ctx, "unix", monitor)
	if err != nil {
		return fmt.Errorf("reaching the machine's console: %w", err)
	}
	defer func() { _ = console.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = console.SetDeadline(deadline)
	}

	// the console says it is ready for more with a prompt, once when it is reached and once after each thing it is told
	said := bufio.NewReader(console)
	prompted := func() (string, error) {
		var text strings.Builder
		for !strings.HasSuffix(text.String(), "(qemu) ") {
			b, readErr := said.ReadByte()
			if readErr != nil {
				return "", fmt.Errorf("the machine's console went quiet after %q: %w", text.String(), readErr)
			}
			text.WriteByte(b)
		}

		return text.String(), nil
	}

	if _, err := prompted(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(console, "screendump %s\n", file); err != nil {
		return fmt.Errorf("asking the machine for a picture of its screen: %w", err)
	}
	answer, err := prompted()
	if err != nil {
		return err
	}

	// the console answers the same prompt whether the picture was saved or not, saying why when it was not
	if _, err := os.Stat(file); err != nil { //nolint:gosec // G703: the path is one the person running the program names
		return fmt.Errorf("the machine saved no picture of its screen: %q", answer)
	}
	logf("a picture of the machine's screen is in %s", file)

	return nil
}
