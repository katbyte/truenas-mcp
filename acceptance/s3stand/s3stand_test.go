package s3stand_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/truenas-mcp/acceptance/s3stand"
)

// sent makes one request of the storage and answers the status and what came back.
func sent(t *testing.T, s *s3stand.Server, method, path string, body io.Reader, headers ...string) (status int, answered string) {
	t.Helper()

	request, err := http.NewRequestWithContext(context.Background(), method, fmt.Sprintf("http://127.0.0.1:%d%s", s.Port(), path), body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	text, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the answer: %v", method, path, err)
	}

	return response.StatusCode, string(text)
}

func started(t *testing.T) *s3stand.Server {
	t.Helper()

	s, err := s3stand.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	return s
}

// What is put is what comes back, whole or in part, and is listed where it was put: under a prefix, and as a folder when what holds it is
// listed with folders asked for. Removed, it is gone, and asking for it says so.
func TestWhatIsPutIsKeptListedFetchedAndRemoved(t *testing.T) {
	t.Parallel()

	s := started(t)

	if status, _ := sent(t, s, http.MethodPut, "/kept/a/note.txt", strings.NewReader("no bucket yet")); status != http.StatusNotFound {
		t.Errorf("putting into a bucket that was never made answered %d", status)
	}
	if status, _ := sent(t, s, http.MethodPut, "/kept", nil); status != http.StatusOK {
		t.Fatalf("making a bucket answered %d", status)
	}
	if status, _ := sent(t, s, http.MethodHead, "/kept", nil); status != http.StatusOK {
		t.Errorf("asking whether the bucket is there answered %d", status)
	}
	if status, listed := sent(t, s, http.MethodGet, "/", nil); status != http.StatusOK || !strings.Contains(listed, "<Name>kept</Name>") {
		t.Errorf("listing the buckets answered %d: %s", status, listed)
	}

	for _, key := range []string{"a/note.txt", "a/deeper/other.txt", "b.txt"} {
		if status, _ := sent(t, s, http.MethodPut, "/kept/"+key, strings.NewReader("held at "+key)); status != http.StatusOK {
			t.Fatalf("putting %s answered %d", key, status)
		}
	}
	if keys := s.Keys("kept"); !slices.Equal(keys, []string{"a/deeper/other.txt", "a/note.txt", "b.txt"}) {
		t.Errorf("the bucket holds %v", keys)
	}

	if status, back := sent(t, s, http.MethodGet, "/kept/a/note.txt", nil); status != http.StatusOK || back != "held at a/note.txt" {
		t.Errorf("fetched whole, the object is %d %q", status, back)
	}
	if status, part := sent(t, s, http.MethodGet, "/kept/a/note.txt", nil, "Range", "bytes=5-6"); status != http.StatusPartialContent || part != "at" {
		t.Errorf("fetched in part, the object is %d %q", status, part)
	}

	// everything under a prefix, and then only what is right under it, with what lies deeper as a folder
	_, all := sent(t, s, http.MethodGet, "/kept?list-type=2&prefix=a/", nil)
	if !strings.Contains(all, "<Key>a/note.txt</Key>") || !strings.Contains(all, "<Key>a/deeper/other.txt</Key>") || strings.Contains(all, "b.txt") {
		t.Errorf("listed under a prefix: %s", all)
	}
	_, level := sent(t, s, http.MethodGet, "/kept?list-type=2&prefix=a/&delimiter=/", nil)
	if !strings.Contains(level, "<Key>a/note.txt</Key>") || !strings.Contains(level, "<CommonPrefixes><Prefix>a/deeper/</Prefix></CommonPrefixes>") || strings.Contains(level, "other.txt") {
		t.Errorf("listed one level under a prefix: %s", level)
	}

	if status, _ := sent(t, s, http.MethodDelete, "/kept/a/note.txt", nil); status != http.StatusNoContent {
		t.Errorf("removing the object answered %d", status)
	}
	if status, why := sent(t, s, http.MethodGet, "/kept/a/note.txt", nil); status != http.StatusNotFound || !strings.Contains(why, "NoSuchKey") {
		t.Errorf("fetching what was removed answered %d: %s", status, why)
	}
	if _, ok := s.Object("kept", "a/note.txt"); ok {
		t.Error("the object that was removed is still held")
	}
}

// Some senders wrap what they send in signed chunks, and some send a large object in pieces: either way what is kept is what was meant.
func TestWhatIsSentInChunksOrInPiecesIsKeptWhole(t *testing.T) {
	t.Parallel()

	s := started(t)
	sent(t, s, http.MethodPut, "/kept", nil)

	wrapped := "5;chunk-signature=0f\r\nhello\r\n7;chunk-signature=1e\r\n, world\r\n0;chunk-signature=2d\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	if status, _ := sent(t, s, http.MethodPut, "/kept/chunked", strings.NewReader(wrapped), "X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"); status != http.StatusOK {
		t.Fatalf("putting an object in signed chunks answered %d", status)
	}
	if held, _ := s.Object("kept", "chunked"); string(held) != "hello, world" {
		t.Errorf("sent in signed chunks, the object is kept as %q", held)
	}

	_, begun := sent(t, s, http.MethodPost, "/kept/pieces?uploads", nil)
	_, after, found := strings.Cut(begun, "<UploadId>")
	id, _, _ := strings.Cut(after, "</UploadId>")
	if !found || id == "" {
		t.Fatalf("beginning an object in pieces answered %s", begun)
	}
	// the pieces are joined in the order of their numbers, not the order they came in
	for number, piece := range map[int]string{2: "second.", 1: "first, "} {
		if status, _ := sent(t, s, http.MethodPut, fmt.Sprintf("/kept/pieces?partNumber=%d&uploadId=%s", number, id), strings.NewReader(piece)); status != http.StatusOK {
			t.Fatalf("putting piece %d answered %d", number, status)
		}
	}
	if status, _ := sent(t, s, http.MethodPost, "/kept/pieces?uploadId="+id, strings.NewReader("<CompleteMultipartUpload/>")); status != http.StatusOK {
		t.Fatalf("joining the pieces answered %d", status)
	}
	if held, _ := s.Object("kept", "pieces"); string(held) != "first, second." {
		t.Errorf("sent in pieces, the object is kept as %q", held)
	}

	if status, _ := sent(t, s, http.MethodPut, "/kept/copy", nil, "X-Amz-Copy-Source", "/kept/pieces"); status != http.StatusOK {
		t.Errorf("copying an object answered %d", status)
	}
	if held, _ := s.Object("kept", "copy"); !bytes.Equal(held, []byte("first, second.")) {
		t.Errorf("the copy is kept as %q", held)
	}
}

// Storage that has gone refuses everything, and takes everything again once it is back, with what it held.
func TestStorageThatIsGoneRefusesEverythingUntilItIsBack(t *testing.T) {
	t.Parallel()

	s := started(t)
	s.Make("kept")
	sent(t, s, http.MethodPut, "/kept/note.txt", strings.NewReader("held"))

	s.Gone(true)
	for _, path := range []string{"/", "/kept", "/kept/note.txt"} {
		if status, why := sent(t, s, http.MethodGet, path, nil); status != http.StatusServiceUnavailable || !strings.Contains(why, "ServiceUnavailable") {
			t.Errorf("with the storage gone, %s answered %d: %s", path, status, why)
		}
	}

	s.Gone(false)
	if status, back := sent(t, s, http.MethodGet, "/kept/note.txt", nil); status != http.StatusOK || back != "held" {
		t.Errorf("with the storage back, the object is %d %q", status, back)
	}
	if asked := s.Asked(); asked != 5 {
		t.Errorf("the storage counts %d requests, and was sent 5", asked)
	}
}
