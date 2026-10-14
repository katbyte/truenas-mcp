package client

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// Copy copies a file's body to dst and closes the body whatever happens: the body's own error when the
// server's side broke off, the writer's when dst failed, told apart by WriteError, and the copy's
// error over the close's. A generated response's WriteTo copies with it.
func Copy(dst io.Writer, body io.ReadCloser) (int64, error) {
	w := &failingWriter{w: dst}
	n, err := io.Copy(w, body)
	if closeErr := body.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("closing the file: %w", closeErr)
	}
	switch {
	case err == nil:
		return n, nil
	case w.err != nil:
		return n, &WriteError{Err: w.err}
	default:
		return n, fmt.Errorf("reading the file from the server: %w", err)
	}
}

// WriteError is the error of a Copy whose writer failed, as against the server's side breaking off:
// a caller removing a part-written file tells its caller which it was.
type WriteError struct{ Err error }

func (e *WriteError) Error() string { return "writing the file: " + e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }

// failingWriter remembers the writer's own error, which io.Copy folds in with the reader's.
type failingWriter struct {
	w   io.Writer
	err error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		f.err = err
	}

	return n, err
}

// ReadAtMost reads a file's body whole and closes it, failing when the body is over max bytes rather
// than cutting it short without a word, which a tool showing the last lines of a log must never do.
// most is the most to read.
// A generated response's Bytes reads with it.
func ReadAtMost(body io.ReadCloser, most int64) ([]byte, error) {
	defer func() { _ = body.Close() }()
	if most <= 0 {
		return nil, errors.New("the most to read must be more than 0 bytes")
	}
	data, err := io.ReadAll(io.LimitReader(body, most+1))
	if err != nil {
		return nil, fmt.Errorf("reading the file from the server: %w", err)
	}
	if int64(len(data)) > most {
		return nil, &TooLargeError{Max: most}
	}

	return data, nil
}

// TooLargeError is the error of a ReadAtMost whose body ran past max.
type TooLargeError struct{ Max int64 }

func (e *TooLargeError) Error() string { return fmt.Sprintf("the file is over %d bytes", e.Max) }

// Filename is the name a server gave a file in its Content-Disposition header, in either the plain or the
// encoded form, or "" when it named none. A generated response's Filename reads it.
func Filename(h http.Header) string {
	_, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil {
		return ""
	}

	return params["filename"]
}
