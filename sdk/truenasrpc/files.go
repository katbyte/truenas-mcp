package truenasrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/katbyte/go-kt/httpc"
)

// A TrueNAS hands a file over, and takes one, outside the connection calls are made on: by an ordinary web request to the same address, let in
// by a token that is good once and for a few minutes. The token is asked for over the connection, so the key itself never rides on a web request.

// tokenLife is how many seconds an upload's token is good for: long enough to open the request, and no use to anyone who finds it later.
const tokenLife = 300

// maxUploads is how many files a NAS takes in one upload.
const maxUploads = 5

// Download has the NAS make a file and copies it to w as it arrives: a saved configuration (config.save), a debug bundle (system.debug), a
// dataset's keys. method is the method that makes the file and params its arguments. It answers how much was copied, and an error when the
// NAS's making of the file failed part way, in which case what was copied is not the whole file.
func (c *Conn) Download(ctx context.Context, method string, params []any, w io.Writer) (int64, error) {
	if method == "" {
		return 0, errors.New("a download needs the name of the method that makes the file")
	}
	if params == nil {
		params = []any{}
	}

	// asking for a file starts the NAS making it, so one that could not be fetched is not asked for
	if _, err := c.web(); err != nil {
		return 0, err
	}

	var handed []json.RawMessage
	if err := c.Call(ctx, "core.download", []any{method, params, "download", false}, &handed); err != nil {
		return 0, err
	}

	var job int64
	var path string
	if len(handed) != 2 || json.Unmarshal(handed[0], &job) != nil || json.Unmarshal(handed[1], &path) != nil {
		return 0, fmt.Errorf("%s: the TrueNAS answered the asking for a download with something other than a job and where to fetch from", method)
	}

	copied, err := c.fetch(ctx, method, path, w)
	if err != nil {
		return copied, err
	}

	// the file is whole only if the job that made it ended well: one that failed part way ends the file early and says why here
	if err := c.Call(ctx, "core.job_wait", []any{job}, nil); err != nil {
		return copied, fmt.Errorf("the file is not whole: %w", err)
	}

	return copied, nil
}

// Logs copies what the job has logged to w, for a job that keeps a log, and answers how much there was.
func (j *Job) Logs(ctx context.Context, w io.Writer) (int64, error) {
	if _, err := j.conn.web(); err != nil {
		return 0, err
	}

	var path string
	if err := j.conn.Call(ctx, "core.job_download_logs", []any{j.id, "logs", false}, &path); err != nil {
		return 0, err
	}

	return j.conn.fetch(ctx, "core.job_download_logs", path, w)
}

// Upload hands the NAS a file for a method that takes one, a configuration to restore (config.upload) or a file to put somewhere
// (filesystem.put), waits for the NAS to deal with it, and reads what the method answers into into, or nothing for nil, as Call does. Some
// methods take more than one file, up to five. It is the form the generated client sends files through.
func (c *Conn) Upload(ctx context.Context, method string, params []any, into any, files ...io.Reader) error {
	job, err := c.StartUpload(ctx, method, params, files...)
	if err != nil {
		return err
	}

	return job.Wait(ctx, into)
}

// StartUpload hands the NAS a file as Upload does, and answers the job that deals with it as soon as the NAS has the file, to wait for or
// follow, as Start does for a call.
func (c *Conn) StartUpload(ctx context.Context, method string, params []any, files ...io.Reader) (*Job, error) {
	switch {
	case method == "":
		return nil, errors.New("an upload needs the name of the method that takes the file")
	case len(files) == 0:
		return nil, fmt.Errorf("%s: an upload needs a file", method)
	case len(files) > maxUploads:
		return nil, fmt.Errorf("%s: a TrueNAS takes %d files at once, and this is %d", method, maxUploads, len(files))
	}
	if params == nil {
		params = []any{}
	}

	base, err := c.web()
	if err != nil {
		return nil, err
	}

	what, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("%s: its arguments cannot be written as JSON: %w", method, err)
	}

	// the token is good once, for a few minutes, and only from the address that asked for it
	var token string
	if err := c.Call(ctx, "auth.generate_token", []any{tokenLife, map[string]any{}, true, true}, &token); err != nil {
		return nil, fmt.Errorf("%s: asking the TrueNAS for leave to upload: %w", method, err)
	}

	// the request is written as it is sent, so that a file of any size is never held whole
	body, into := io.Pipe()
	form := multipart.NewWriter(into)
	go func() {
		written := form.WriteField("data", string(what))
		for _, file := range files {
			if written != nil {
				break
			}

			var part io.Writer
			if part, written = form.CreateFormFile("file", "file"); written == nil {
				_, written = io.Copy(part, file)
			}
		}
		if written == nil {
			written = form.Close()
		}
		_ = into.CloseWithError(written)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/_upload", body)
	if err != nil {
		_ = body.Close()

		return nil, err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Token "+token)
	req.Header.Set("User-Agent", c.o.userAgent)

	resp, err := c.files.Do(req)
	if err != nil {
		_ = body.Close()

		return nil, &ConnError{Address: c.address, Sent: true, Err: fmt.Errorf("%s: handing the file over: %w", method, httpc.Explain(httpc.RedactError(err)))}
	}
	defer func() { _ = resp.Body.Close() }()

	said, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, c.turnedAway(method, "take the file", resp.StatusCode, string(said))
	}

	var taken struct {
		Job int64 `json:"job_id"`
	}
	if err := json.Unmarshal(said, &taken); err != nil || taken.Job == 0 {
		return nil, fmt.Errorf("%s: the TrueNAS took the file and did not say which job is dealing with it", method)
	}

	return c.Job(ctx, taken.Job)
}

// fetch copies what the NAS serves at a path it handed out to w, and answers how much there was.
func (c *Conn) fetch(ctx context.Context, method, path string, w io.Writer) (int64, error) {
	base, err := c.web()
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("%s: where the TrueNAS said to fetch the file from is not an address", method)
	}
	req.Header.Set("User-Agent", c.o.userAgent)

	resp, err := c.files.Do(req)
	if err != nil {
		return 0, &ConnError{Address: c.address, Sent: true, Err: fmt.Errorf("%s: fetching the file: %w", method, httpc.Explain(httpc.RedactError(err, "auth_token")))}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		said, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

		return 0, c.turnedAway(method, "hand the file over", resp.StatusCode, string(said))
	}

	copied, err := io.Copy(w, resp.Body)
	if err != nil {
		return copied, fmt.Errorf("%s: the file stopped arriving after %d bytes: %w", method, copied, err)
	}

	return copied, nil
}

// web is the address the NAS's web server answers at, which is where files go to and from: the address calls are made at, without its path.
func (c *Conn) web() (string, error) {
	if c.socket != "" {
		return "", errors.New("a file is handed over by the NAS's web server, which its own socket is not: a program on the NAS reads and writes the file itself, or reaches the NAS by its address")
	}

	u, err := url.Parse(c.url)
	if err != nil {
		return "", err
	}

	return "https://" + u.Host, nil
}

// turnedAway is a NAS's web server refusing to take or hand over a file, in words for what its answer means.
func (c *Conn) turnedAway(method, what string, status int, said string) error {
	said = cut(strings.TrimSpace(c.scrub(said, scrubbed)), 500)

	why := fmt.Sprintf("it answered HTTP %d", status)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		why = "it did not take the leave it had given, which is good once, for a few minutes, and only from the address that asked"
	case http.StatusNotFound:
		why = "it has no such job any more"
	case http.StatusGone:
		why = "the file was not fetched in time: a NAS keeps one waiting for a minute"
	case http.StatusUnprocessableEntity:
		why = "it has no such method, or was not told which"
	case http.StatusPreconditionFailed:
		why = "it could not carry the method out"
	}
	if said != "" {
		why += ": " + said
	}

	return fmt.Errorf("%s: the TrueNAS would not %s: %s", method, what, why)
}
