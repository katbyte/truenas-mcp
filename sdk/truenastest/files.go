package truenastest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// file is a file a job has made and not yet handed over.
type file struct {
	content []byte
	// fetched is closed when it has been handed over
	fetched chan struct{}
}

// Downloadable has a function make the file a method hands back: the method is then one that is asked for through core.download and fetched from
// the stand-in's web address, as a NAS's saved configuration or debug bundle is. An error from the function is the job failing, with no file.
func (n *NAS) Downloadable(method string, fn func(params []json.RawMessage) ([]byte, error)) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.handlers[method] = handler{job: true, download: fn}
}

// Uploadable has a function take the files a method is handed, each as it arrived: the method is then one that is sent to the stand-in's web
// address with its files, as a configuration to restore is, and what the function returns is what the job that deals with them answers.
func (n *NAS) Uploadable(method string, fn func(params []json.RawMessage, files [][]byte) (any, error)) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.handlers[method] = handler{job: true, upload: fn}
}

// token makes a token that lets one web request in: to fetch the file of the job given, or to upload when the job is 0.
func (n *NAS) token(job int) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)

	n.mu.Lock()
	n.tokens[token] = job
	n.mu.Unlock()

	return token, nil
}

// spend uses a token up, and reports whether it was good for the job given.
func (n *NAS) spend(token string, job int) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	good, ok := n.tokens[token]
	if !ok || good != job {
		return false
	}
	delete(n.tokens, token)

	return true
}

// download answers core.download: it starts the job that makes the file, and says where the file is to be fetched from and with what token.
func (n *NAS) download(message json.RawMessage, params []json.RawMessage) (any, error) {
	var method string
	var args []json.RawMessage
	if len(params) < 3 || json.Unmarshal(params[0], &method) != nil || json.Unmarshal(params[1], &args) != nil {
		return nil, &InvalidError{Reason: "[EINVAL] method: Field required", Invalid: []Invalid{{Attribute: "method", Message: "Field required", Errno: 22}}}
	}

	n.mu.Lock()
	h, known := n.handlers[method]
	n.mu.Unlock()

	if !known || h.download == nil {
		return nil, &Error{Errno: 22, ErrName: "EINVAL", Reason: "Method " + method + " is not downloadable"}
	}

	content, failure := h.download(args)

	id, path, err := n.offer(method, message, args, content, failure)
	if err != nil {
		return nil, err
	}

	return []any{id, path}, nil
}

// jobLogs answers core.job_download_logs: where what a job has logged is to be fetched from, for a job that has logged anything.
func (n *NAS) jobLogs(message json.RawMessage, params []json.RawMessage) (any, error) {
	var id int
	if len(params) < 1 || json.Unmarshal(params[0], &id) != nil {
		return nil, &InvalidError{Reason: "[EINVAL] id: Input should be a valid integer", Invalid: []Invalid{{Attribute: "id", Message: "Input should be a valid integer", Errno: 22}}}
	}

	n.mu.Lock()
	j := n.jobs[id]
	logged := ""
	if j != nil {
		logged = j.logged
	}
	n.mu.Unlock()

	switch {
	case j == nil:
		return nil, NoSuchInstance("Job with id " + strconv.Itoa(id))
	case logged == "":
		return nil, &Error{Errno: 14, ErrName: "EFAULT", Reason: "This job has no logs"}
	}

	_, path, err := n.offer("filesystem.get", message, nil, []byte(logged), nil)

	return path, err
}

// offer starts the job that hands a file over, and answers its number and where the file is to be fetched from, token and all.
func (n *NAS) offer(method string, message json.RawMessage, args []json.RawMessage, content []byte, failure error) (id int, path string, err error) {
	made := &file{content: content, fetched: make(chan struct{})}

	// the job is over when its file has been handed over, or straight away when there is no file to hand over
	j := n.begin(method, message, args, handler{long: func(run *Run, _ []json.RawMessage) (any, error) {
		if failure != nil {
			return nil, failure
		}

		select {
		case <-made.fetched:
		case <-run.Aborted():
		case <-time.After(time.Minute):
		}

		return nil, nil //nolint:nilnil // a job that hands a file over answers nothing
	}})

	n.mu.Lock()
	n.files[j.id] = made
	n.mu.Unlock()

	token, err := n.token(j.id)
	if err != nil {
		return 0, "", err
	}

	return j.id, "/_download/" + strconv.Itoa(j.id) + "?auth_token=" + token, nil
}

// serveDownload hands a job's file over to whoever has the token for it, once.
func (n *NAS) serveDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/_download/"))
	if err != nil {
		http.NotFound(w, r)

		return
	}
	if !n.spend(r.URL.Query().Get("auth_token"), id) {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	n.mu.Lock()
	_, running := n.jobs[id]
	made := n.files[id]
	delete(n.files, id)
	n.mu.Unlock()

	switch {
	case !running:
		http.NotFound(w, r)

		return
	case made == nil:
		w.WriteHeader(http.StatusGone)

		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(made.content)
	close(made.fetched)
}

// serveUpload takes a method and its files in one web request, starts the job that deals with them, and answers the job's number.
func (n *NAS) serveUpload(w http.ResponseWriter, r *http.Request) {
	parts, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "No part found on payload", http.StatusMethodNotAllowed)

		return
	}

	first, err := parts.NextPart()
	if err != nil || first.FormName() != "data" {
		http.Error(w, `"data" part must be the first on payload`, http.StatusMethodNotAllowed)

		return
	}

	var data struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(first).Decode(&data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}
	if data.Method == "" {
		w.WriteHeader(http.StatusUnprocessableEntity)

		return
	}

	// let in by a token asked for over a logged-in connection, or by the key itself, as a NAS lets an upload in
	kind, credential, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	switch {
	case kind == "Token" && n.spend(credential, 0):
	case kind == "Bearer" && credential == n.key:
	default:
		http.Error(w, "Invalid token", http.StatusUnauthorized)

		return
	}

	n.mu.Lock()
	n.calls = append(n.calls, Call{Method: data.Method, Params: data.Params})
	h, known := n.handlers[data.Method]
	n.mu.Unlock()

	if !known || h.upload == nil {
		http.Error(w, "[ENOMETHOD] Method "+data.Method+" not found", http.StatusUnprocessableEntity)

		return
	}

	var files [][]byte
	for {
		part, err := parts.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "file" {
			http.Error(w, "Unknown payload part '"+part.FormName()+"'", http.StatusMethodNotAllowed)

			return
		}
		if len(files) == 5 {
			http.Error(w, "Too many uploaded files", http.StatusMethodNotAllowed)

			return
		}

		content, err := io.ReadAll(part)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		files = append(files, content)
	}
	if len(files) == 0 {
		http.Error(w, `"file" not found as second part on payload`, http.StatusMethodNotAllowed)

		return
	}

	j := n.begin(data.Method, nil, data.Params, handler{fn: func(params []json.RawMessage) (any, error) { return h.upload(params, files) }})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"job_id": j.id})
}
