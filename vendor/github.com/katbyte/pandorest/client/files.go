package client

import (
	"context"
	"errors"
	"io"
)

// Downloader is what a connection offers for a method whose answer is a file: it runs the method with its arguments and writes
// the file to w, answering how many bytes it wrote. How the file travels is the connection's own (TrueNAS runs the method as a
// job and hands out a one-time address to fetch from).
type Downloader interface {
	Download(ctx context.Context, method string, params []any, w io.Writer) (int64, error)
}

// Uploader is what a connection offers for a method that takes a file beside its arguments: it sends the files with the call
// and decodes what the method answers into into, nil for an answer nobody wants.
type Uploader interface {
	Upload(ctx context.Context, method string, params []any, into any, files ...io.Reader) error
}

// ErrCannotCarryFiles is the error for a connection that offers neither Download nor Upload, asked for one.
var ErrCannotCarryFiles = errors.New("the connection cannot carry a file")

// DownloadWith fetches the file a method answers through the connection, or fails with ErrCannotCarryFiles for one that
// cannot.
func DownloadWith(ctx context.Context, caller Caller, method string, params []any, w io.Writer) (int64, error) {
	d, ok := caller.(Downloader)
	if !ok {
		return 0, ErrCannotCarryFiles
	}

	return d.Download(ctx, method, params, w)
}

// UploadWith sends files beside a call through the connection, or fails with ErrCannotCarryFiles for one that cannot.
func UploadWith(ctx context.Context, caller Caller, method string, params []any, into any, files ...io.Reader) error {
	u, ok := caller.(Uploader)
	if !ok {
		return ErrCannotCarryFiles
	}

	return u.Upload(ctx, method, params, into, files...)
}
