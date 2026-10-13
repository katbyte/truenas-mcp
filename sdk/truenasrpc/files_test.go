package truenasrpc_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/sdk/truenastest"
)

// A saved configuration holds every password the NAS has. It is fetched whole, by a web request let in by a token that is good once, and neither
// the key, nor the token, nor a byte of the file may reach a log on the way.
func TestAFileTheNASMakesIsFetchedWholeAndNothingOfItReachesALog(t *testing.T) {
	t.Parallel()

	contents := strings.Repeat("a line of a saved configuration, with a password in it\n", 4000)

	nas := truenastest.New(t)
	nas.Downloadable("config.save", func([]json.RawMessage) ([]byte, error) { return []byte(contents), nil })

	log := &written{}
	conn := connect(t, nas, truenasrpc.WithLog(log))

	var file bytes.Buffer
	copied, err := conn.Download(t.Context(), "config.save", []any{map[string]any{"secretseed": true}}, &file)
	if err != nil {
		t.Fatal(err)
	}
	if copied != int64(len(contents)) || file.String() != contents {
		t.Errorf("%d bytes were fetched of a file of %d, the same: %v", copied, len(contents), file.String() == contents)
	}

	asked := nas.Calls("core.download")
	if len(asked) != 1 || string(asked[0].Params[0]) != `"config.save"` || !strings.Contains(string(asked[0].Params[1]), `"secretseed":true`) {
		t.Errorf("the download was asked for with %+v, want the method and its arguments", asked)
	}

	text := log.text()
	if strings.Contains(text, "a line of a saved configuration") || strings.Contains(text, nas.Key()) {
		t.Error("the file, or the key, is in the log")
	}
	if at := strings.Index(text, "auth_token="); at < 0 || !strings.HasPrefix(text[at:], "auth_token=REDACTED") {
		t.Errorf("the log does not show the fetch with its token hidden:\n%s", text)
	}
}

// The NAS starts sending a file before it knows it can finish it. One that fails part way is cut short, and a short file that looks whole is the
// worst thing a backup can be: the failure is asked after once the file has stopped, and is the error.
func TestAFileTheNASCouldNotFinishIsSaidNotToBeWhole(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.Downloadable("system.debug", func([]json.RawMessage) ([]byte, error) {
		return nil, &truenastest.Error{Errno: 28, ErrName: "ENOSPC", Reason: "there is no room left to gather it in"}
	})
	nas.Answer("system.info", map[string]any{})
	conn := connect(t, nas)

	var file bytes.Buffer
	_, err := conn.Download(t.Context(), "system.debug", nil, &file)
	if err == nil || !strings.Contains(err.Error(), "the file is not whole") || !strings.Contains(err.Error(), "there is no room left") {
		t.Errorf("a file the NAS could not finish answered %v, want it said not to be whole and why", err)
	}

	// a method that hands no file back is refused by the NAS, in its words
	if _, err := conn.Download(t.Context(), "system.info", nil, &file); err == nil || !strings.Contains(err.Error(), "not downloadable") {
		t.Errorf("fetching a file from a method that makes none answered %v", err)
	}
}

// A file is handed to the NAS the same way round: a web request let in by a token asked for over the connection, so that the key is not sent,
// and answered with the job that deals with the file, which is waited for like any other.
func TestAFileIsHandedToTheNASAndTheJobThatDealsWithItAnswered(t *testing.T) {
	t.Parallel()

	contents := strings.Repeat("a line of a configuration to restore\n", 4000)

	nas := truenastest.New(t)
	var got [][]byte
	var with []json.RawMessage
	nas.Uploadable("config.upload", func(params []json.RawMessage, files [][]byte) (any, error) {
		got, with = files, params

		return map[string]any{"restored": true}, nil
	})

	log := &written{}
	conn := connect(t, nas, truenasrpc.WithLog(log))

	job, err := conn.Upload(t.Context(), "config.upload", []any{map[string]any{"keep": "settings"}}, strings.NewReader(contents), strings.NewReader("a second file"))
	if err != nil {
		t.Fatal(err)
	}

	var answer struct {
		Restored bool `json:"restored"`
	}
	if err := job.Wait(t.Context(), &answer); err != nil || !answer.Restored {
		t.Fatalf("the job that dealt with the file answered %+v, %v", answer, err)
	}
	if len(got) != 2 || string(got[0]) != contents || string(got[1]) != "a second file" || len(with) != 1 || !strings.Contains(string(with[0]), `"keep":"settings"`) {
		t.Errorf("the NAS was handed %d files with %s, want both files as sent and the arguments", len(got), with)
	}

	if text := log.text(); strings.Contains(text, "a line of a configuration to restore") || strings.Contains(text, nas.Key()) {
		t.Error("the file, or the key, is in the log")
	}
	if tokens := nas.Calls("auth.generate_token"); len(tokens) != 1 || string(tokens[0].Params[3]) != "true" || string(tokens[0].Params[2]) != "true" {
		t.Errorf("leave to upload was asked for with %+v, want a token good once and only from this address", tokens)
	}

	// what cannot work is refused before a file leaves, and what the NAS refuses is said in words
	six := []string{"1", "2", "3", "4", "5", "6"}
	if _, err := conn.Upload(t.Context(), "config.upload", nil, strings.NewReader(six[0]), strings.NewReader(six[1]), strings.NewReader(six[2]), strings.NewReader(six[3]), strings.NewReader(six[4]), strings.NewReader(six[5])); err == nil || !strings.Contains(err.Error(), "5 files at once") {
		t.Errorf("six files at once answered %v", err)
	}
	if _, err := conn.Upload(t.Context(), "config.upload", nil); err == nil || !strings.Contains(err.Error(), "needs a file") {
		t.Errorf("an upload of no file answered %v", err)
	}
	if _, err := conn.Upload(t.Context(), "pool.query", nil, strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "no such method") {
		t.Errorf("a file handed to a method that takes none answered %v", err)
	}
}

// A job that keeps a log can be asked for it, which is where the reason for a failure usually is; and a program on the NAS itself is told, in
// words, that files are not handed over through the NAS's socket.
func TestAJobsLogIsFetchedAndFilesAreNotHandedOverThroughTheSocket(t *testing.T) {
	t.Parallel()

	nas := truenastest.New(t)
	nas.LongJob("replication.run", func(run *truenastest.Run, _ []json.RawMessage) (any, error) {
		run.Log("sending tank/data@monday")
		run.Log("sent 12 GiB")

		return nil, nil //nolint:nilnil // the job answers nothing
	})
	nas.Job("pool.scrub.scrub", func([]json.RawMessage) (any, error) { return nil, nil }) //nolint:nilnil // the job answers nothing
	conn := connect(t, nas)

	job, err := conn.Start(t.Context(), "replication.run", []any{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Wait(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	if _, err := job.Logs(t.Context(), &logged); err != nil || logged.String() != "sending tank/data@monday\nsent 12 GiB\n" {
		t.Errorf("the job's log was fetched as %q, %v", logged.String(), err)
	}

	quiet, err := conn.Start(t.Context(), "pool.scrub.scrub", []any{"tank"})
	if err != nil {
		t.Fatal(err)
	}
	if err := quiet.Wait(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := quiet.Logs(t.Context(), &logged); err == nil || !strings.Contains(err.Error(), "has no logs") {
		t.Errorf("the log of a job that keeps none answered %v", err)
	}

	local := truenastest.New(t, truenastest.OnSocket())
	onNAS, err := truenasrpc.New(local.Address(), truenasrpc.LocalUser())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = onNAS.Close() })

	if _, err := onNAS.Download(t.Context(), "config.save", nil, &logged); err == nil || !strings.Contains(err.Error(), "web server") {
		t.Errorf("a download through the socket answered %v, want it said that files go by the web server", err)
	}
	if _, err := onNAS.Upload(t.Context(), "config.upload", nil, strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "web server") {
		t.Errorf("an upload through the socket answered %v, want it said that files go by the web server", err)
	}
}
