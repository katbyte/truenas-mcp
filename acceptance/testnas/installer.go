package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/katbyte/truenas-mcp/acceptance/layout"
)

// installer is a connection to the program that installs TrueNAS. It answers on the installer disc while that sits at its menu, over a websocket
// on port 8080, and speaks JSON-RPC: one call at a time is all an install needs, so that is all this does.
type installer struct {
	ws  *websocket.Conn
	ids int64
}

// installerDisk is a disk as the installer lists one.
type installerDisk struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Model string `json:"model"`
}

// dialInstaller connects to an installer, trying again until it answers or the context ends: a machine started from the installer disc takes a
// minute or more to get as far as answering.
func dialInstaller(ctx context.Context, address string, logf func(string, ...any)) (*installer, error) {
	for try := 0; ; try++ {
		dialling, cancel := context.WithTimeout(ctx, 10*time.Second)
		ws, _, err := websocket.Dial(dialling, address, nil) //nolint:bodyclose // the library closes the body of a refusal, and on success it is the socket
		cancel()
		if err == nil {
			ws.SetReadLimit(4 << 20)

			return &installer{ws: ws}, nil
		}

		if try%6 == 0 {
			logf("waiting for the installer to answer at %s: %v", address, err)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the installer at %s never answered: %w (last: %w)", address, context.Cause(ctx), err)
		case <-time.After(5 * time.Second):
		}
	}
}

func (i *installer) close() { _ = i.ws.Close(websocket.StatusNormalClosure, "") }

// call makes one call and waits for its answer. Whatever the installer says of an install's progress in the meantime is handed to progress.
func (i *installer) call(ctx context.Context, method string, params []any, into any, progress func(done float64, doing string)) error {
	if params == nil {
		params = []any{}
	}
	i.ids++
	id := i.ids

	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := i.ws.Write(ctx, websocket.MessageText, frame); err != nil {
		return fmt.Errorf("%s: sending to the installer: %w", method, err)
	}

	for {
		_, frame, err := i.ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("%s: the installer went away before answering: %w", method, err)
		}

		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(frame, &msg); err != nil {
			return fmt.Errorf("%s: the installer sent something that is not JSON: %w", method, err)
		}

		// what the installer says unasked: how far along an install is
		if msg.Method != "" {
			var said []struct {
				Progress float64 `json:"progress"`
				Message  string  `json:"message"`
			}
			if msg.Method == "installation_progress" && progress != nil && json.Unmarshal(msg.Params, &said) == nil && len(said) == 1 {
				progress(said[0].Progress, said[0].Message)
			}

			continue
		}
		if msg.ID == nil || *msg.ID != id {
			continue
		}

		if msg.Error != nil {
			return fmt.Errorf("%s: the installer refused: %s (%d)", method, msg.Error.Message, msg.Error.Code)
		}
		if into == nil {
			return nil
		}

		return json.Unmarshal(msg.Result, into)
	}
}

// install has the installer at an address put TrueNAS on the one disk of the size given, with layout.AdminUser as its administrator and the password
// given, and then switch the machine off. The disk is picked by its size and not its name, since which name a disk comes up under is not ours to
// say; every other disk the machine has must be of another size.
func install(ctx context.Context, address, password string, bootBytes int64, logf func(string, ...any)) error {
	i, err := dialInstaller(ctx, address, logf)
	if err != nil {
		return err
	}
	defer i.close()

	var info struct {
		Version   string  `json:"version"`
		EFI       bool    `json:"efi"`
		Running   bool    `json:"installation_running"`
		Completed bool    `json:"installation_completed"`
		Failed    *string `json:"installation_error"`
	}
	if err := i.call(ctx, "system_info", nil, &info, nil); err != nil {
		return err
	}
	logf("the installer is for TrueNAS %s", info.Version)

	var disks []installerDisk
	if err := i.call(ctx, "list_disks", nil, &disks, nil); err != nil {
		return err
	}

	var onto []string
	found := make([]string, 0, len(disks))
	for _, d := range disks {
		found = append(found, fmt.Sprintf("%s (%d bytes)", d.Name, d.Size))
		if d.Size == bootBytes {
			onto = append(onto, d.Name)
		}
	}
	if len(onto) != 1 {
		return fmt.Errorf("the installer has %d disks of %d bytes to install onto, and exactly one is wanted: it lists %s", len(onto), bootBytes, strings.Join(found, ", "))
	}
	logf("installing onto %s", onto[0])

	last := ""
	if err := i.call(ctx, "install", []any{installArguments(onto[0], password)}, nil, func(done float64, doing string) {
		if doing != last {
			logf("  %3.0f%% %s", done*100, doing)
			last = doing
		}
	}); err != nil {
		return err
	}
	logf("installed; switching the machine off")

	// the machine goes down under the call, so no answer is the usual one
	down, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := i.call(down, "shutdown", nil, nil, nil); err != nil && !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "went away") {
		return err
	}

	return nil
}

// installArguments is what the installer's install call takes to put TrueNAS on one disk. The machine is one that starts the old way, from the
// disk's first sector, which is how QEMU starts one unless told otherwise.
func installArguments(disk, password string) map[string]any {
	return map[string]any{"disks": []string{disk}, "set_pmbr": false, "authentication": map[string]any{"username": layout.AdminUser, "password": password}}
}
