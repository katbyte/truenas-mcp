// Package s3stand is storage that speaks enough S3 for a TrueNAS to copy to, kept in memory, for the live tests: a NAS in a virtual machine
// has no cloud to send a cloud sync or a cloud backup to, and this stands in for one on the machine the tests run on. It takes any key, since
// who may write is not what is being tested, and it can be told to stop answering, which is how such a copy fails in life.
package s3stand

import (
	"bufio"
	"bytes"
	"crypto/md5" //nolint:gosec // G501: S3 names what an object holds by its MD5, which is no secret and guards nothing
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is the storage: buckets of named objects, served over HTTP on this machine.
type Server struct {
	listener net.Listener
	web      *http.Server

	mu      sync.Mutex
	buckets map[string]map[string]object
	uploads map[string]map[int][]byte
	asked   int
	gone    bool
}

// object is one thing kept: what it holds and when it was put.
type object struct {
	data []byte
	put  time.Time
}

// tag is the name S3 gives what an object holds, which whoever sent it checks against its own.
func (o object) tag() string {
	sum := md5.Sum(o.data) //nolint:gosec // G401: as above

	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// Start begins serving on a port of this machine nothing else has.
func Start() (*Server, error) {
	var listening net.ListenConfig
	listener, err := listening.Listen(nil, "tcp", "127.0.0.1:0") //nolint:staticcheck // SA1012: nothing here has a context to give, and listening does not wait
	if err != nil {
		return nil, err
	}

	s := &Server{listener: listener, buckets: map[string]map[string]object{}, uploads: map[string]map[int][]byte{}}
	s.web = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = s.web.Serve(listener) }()

	return s, nil
}

// Port is the port it serves on.
func (s *Server) Port() int {
	if at, ok := s.listener.Addr().(*net.TCPAddr); ok {
		return at.Port
	}

	return 0
}

// Close stops serving.
func (s *Server) Close() { _ = s.web.Close() }

// Make makes a bucket, as whoever looks after the storage would: some senders cannot make one themselves.
func (s *Server) Make(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, there := s.buckets[bucket]; !there {
		s.buckets[bucket] = map[string]object{}
	}
}

// Gone makes the storage refuse everything, as storage that is down does, until it is told otherwise.
func (s *Server) Gone(gone bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.gone = gone
}

// Asked is how many requests it has been sent.
func (s *Server) Asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.asked
}

// Buckets is the names of the buckets it holds.
func (s *Server) Buckets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	names := make([]string, 0, len(s.buckets))
	for name := range s.buckets {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

// Keys is the names of everything in a bucket.
func (s *Server) Keys(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.held(bucket)
}

// held is the names of everything in a bucket, for a caller that already holds the lock.
func (s *Server) held(bucket string) []string {
	keys := make([]string, 0, len(s.buckets[bucket]))
	for key := range s.buckets[bucket] {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}

// Object is what one object holds, and whether there is one.
func (s *Server) Object(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	held, ok := s.buckets[bucket][key]

	return held.data, ok
}

// failure is how S3 says something went wrong.
type failure struct {
	XMLName xml.Name `xml:"Error"`
	Code    string
	Message string
}

func refuse(w http.ResponseWriter, status int, code, message string) {
	answer(w, status, failure{Code: code, Message: message})
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(body)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.asked++
	gone := s.gone
	s.mu.Unlock()

	if gone {
		refuse(w, http.StatusServiceUnavailable, "ServiceUnavailable", "the storage is not there")

		return
	}

	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	query := r.URL.Query()

	switch {
	case bucket == "":
		s.listBuckets(w)
	case key == "":
		s.bucket(w, r, bucket, query)
	case query.Has("uploads") || query.Has("uploadId"):
		s.parts(w, r, bucket, key, query)
	default:
		s.one(w, r, bucket, key)
	}
}

func (s *Server) listBuckets(w http.ResponseWriter) {
	type named struct {
		Name         string
		CreationDate string
	}
	var listed struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Owner   struct{ ID, DisplayName string }
		Buckets struct {
			Bucket []named
		}
	}
	listed.Owner.ID, listed.Owner.DisplayName = "stand-in", "stand-in"
	for _, name := range s.Buckets() {
		listed.Buckets.Bucket = append(listed.Buckets.Bucket, named{Name: name, CreationDate: "2020-01-01T00:00:00.000Z"})
	}

	answer(w, http.StatusOK, listed)
}

// bucket answers what is asked of a bucket itself: made, removed, whether it is there, where it is, and what it holds.
func (s *Server) bucket(w http.ResponseWriter, r *http.Request, bucket string, query url.Values) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, there := s.buckets[bucket]

	switch {
	case r.Method == http.MethodPut:
		if !there {
			s.buckets[bucket] = map[string]object{}
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(http.StatusOK)
	case !there:
		refuse(w, http.StatusNotFound, "NoSuchBucket", "there is no bucket "+bucket)
	case r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		delete(s.buckets, bucket)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && query.Has("delete"):
		s.removeMany(w, r, bucket)
	case r.Method != http.MethodGet:
		refuse(w, http.StatusNotImplemented, "NotImplemented", r.Method+" of a bucket is not something this stands in for")
	case query.Has("location"):
		answer(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
		}{})
	case query.Has("versioning"):
		answer(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"VersioningConfiguration"`
		}{})
	case query.Has("object-lock"):
		refuse(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError", "nothing here is locked")
	case query.Has("uploads"):
		answer(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"ListMultipartUploadsResult"`
			Bucket  string
		}{Bucket: bucket})
	default:
		s.list(w, bucket, query)
	}
}

// list answers what a bucket holds under a prefix, the old way or the new, whole: nothing a test sends is too much for one answer.
func (s *Server) list(w http.ResponseWriter, bucket string, query url.Values) {
	type kept struct {
		Key          string
		LastModified string
		ETag         string
		Size         int
		StorageClass string
	}
	type under struct{ Prefix string }

	listed := struct {
		XMLName        xml.Name `xml:"ListBucketResult"`
		Name           string
		Prefix         string
		Delimiter      string `xml:",omitempty"`
		Marker         string `xml:",omitempty"`
		MaxKeys        int
		KeyCount       int
		IsTruncated    bool
		Contents       []kept
		CommonPrefixes []under
	}{Name: bucket, Prefix: query.Get("prefix"), Delimiter: query.Get("delimiter"), Marker: query.Get("marker"), MaxKeys: 1000}

	after := query.Get("start-after")
	if listed.Marker != "" {
		after = listed.Marker
	}

	var folders []string
	for _, key := range s.held(bucket) {
		if !strings.HasPrefix(key, listed.Prefix) || after != "" && key <= after {
			continue
		}

		// with a delimiter, what lies deeper is answered as the one folder it is in
		if rest := key[len(listed.Prefix):]; listed.Delimiter != "" && strings.Contains(rest, listed.Delimiter) {
			folder := listed.Prefix + rest[:strings.Index(rest, listed.Delimiter)+len(listed.Delimiter)]
			if !slices.Contains(folders, folder) {
				folders = append(folders, folder)
			}

			continue
		}

		held := s.buckets[bucket][key]
		listed.Contents = append(listed.Contents, kept{Key: key, LastModified: held.put.UTC().Format("2006-01-02T15:04:05.000Z"), ETag: held.tag(), Size: len(held.data), StorageClass: "STANDARD"})
	}
	for _, folder := range folders {
		listed.CommonPrefixes = append(listed.CommonPrefixes, under{Prefix: folder})
	}
	listed.KeyCount = len(listed.Contents) + len(listed.CommonPrefixes)

	answer(w, http.StatusOK, listed)
}

// removeMany removes the objects a list names, and says each is gone.
func (s *Server) removeMany(w http.ResponseWriter, r *http.Request, bucket string) {
	var asked struct {
		Object []struct{ Key string }
	}
	if err := xml.NewDecoder(r.Body).Decode(&asked); err != nil {
		refuse(w, http.StatusBadRequest, "MalformedXML", err.Error())

		return
	}

	type removed struct{ Key string }
	var done struct {
		XMLName xml.Name `xml:"DeleteResult"`
		Deleted []removed
	}
	for _, one := range asked.Object {
		delete(s.buckets[bucket], one.Key)
		done.Deleted = append(done.Deleted, removed(one))
	}

	answer(w, http.StatusOK, done)
}

// one answers what is asked of one object: put, copied from another, fetched whole or in part, asked after, removed.
func (s *Server) one(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// what is sent is read before anything is held, so that a slow sender holds nobody else up
	var sent []byte
	if r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") == "" {
		var err error
		if sent, err = body(r); err != nil {
			refuse(w, http.StatusBadRequest, "IncompleteBody", err.Error())

			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	in, there := s.buckets[bucket]
	if !there {
		refuse(w, http.StatusNotFound, "NoSuchBucket", "there is no bucket "+bucket)

		return
	}

	switch r.Method {
	case http.MethodPut:
		if from := r.Header.Get("X-Amz-Copy-Source"); from != "" {
			source, err := url.PathUnescape(strings.TrimPrefix(from, "/"))
			fromBucket, fromKey, _ := strings.Cut(source, "/")
			copied, ok := s.buckets[fromBucket][fromKey]
			if err != nil || !ok {
				refuse(w, http.StatusNotFound, "NoSuchKey", "there is nothing at "+from+" to copy")

				return
			}

			in[key] = object{data: copied.data, put: time.Now()}
			answer(w, http.StatusOK, struct {
				XMLName      xml.Name `xml:"CopyObjectResult"`
				ETag         string
				LastModified string
			}{ETag: copied.tag(), LastModified: time.Now().UTC().Format(time.RFC3339)})

			return
		}

		in[key] = object{data: sent, put: time.Now()}
		w.Header().Set("ETag", in[key].tag())
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		held, ok := in[key]
		if !ok {
			refuse(w, http.StatusNotFound, "NoSuchKey", "there is nothing at "+key)

			return
		}

		w.Header().Set("ETag", held.tag())
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", held.put, bytes.NewReader(held.data))
	case http.MethodDelete:
		delete(in, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		refuse(w, http.StatusNotImplemented, "NotImplemented", r.Method+" of an object is not something this stands in for")
	}
}

// parts takes an object sent in pieces: begun, each piece put, and the pieces joined or thrown away.
func (s *Server) parts(w http.ResponseWriter, r *http.Request, bucket, key string, query url.Values) {
	var sent []byte
	if r.Method == http.MethodPut {
		var err error
		if sent, err = body(r); err != nil {
			refuse(w, http.StatusBadRequest, "IncompleteBody", err.Error())

			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, there := s.buckets[bucket]; !there {
		refuse(w, http.StatusNotFound, "NoSuchBucket", "there is no bucket "+bucket)

		return
	}

	id := query.Get("uploadId")
	pieces, begun := s.uploads[id]

	switch {
	case r.Method == http.MethodPost && query.Has("uploads"):
		id = "upload-" + strconv.Itoa(len(s.uploads)+1)
		s.uploads[id] = map[int][]byte{}
		answer(w, http.StatusOK, struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			Bucket   string
			Key      string
			UploadID string `xml:"UploadId"`
		}{Bucket: bucket, Key: key, UploadID: id})
	case !begun:
		refuse(w, http.StatusNotFound, "NoSuchUpload", "nothing was begun under "+id)
	case r.Method == http.MethodPut:
		number, err := strconv.Atoi(query.Get("partNumber"))
		if err != nil {
			refuse(w, http.StatusBadRequest, "InvalidArgument", "a piece has to have a number")

			return
		}

		pieces[number] = sent
		w.Header().Set("ETag", object{data: sent}.tag())
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost:
		numbers := make([]int, 0, len(pieces))
		for number := range pieces {
			numbers = append(numbers, number)
		}
		slices.Sort(numbers)

		var whole []byte
		for _, number := range numbers {
			whole = append(whole, pieces[number]...)
		}
		s.buckets[bucket][key] = object{data: whole, put: time.Now()}
		delete(s.uploads, id)

		answer(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
			Bucket  string
			Key     string
			ETag    string
		}{Bucket: bucket, Key: key, ETag: s.buckets[bucket][key].tag()})
	case r.Method == http.MethodDelete:
		delete(s.uploads, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		refuse(w, http.StatusNotImplemented, "NotImplemented", r.Method+" of an object sent in pieces is not something this stands in for")
	}
}

// body is what a request sends, with the wrapping taken off when it is sent in signed chunks, as some senders do over a connection without TLS.
func body(r *http.Request) ([]byte, error) {
	chunked := strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") || strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked")
	if !chunked {
		return io.ReadAll(r.Body)
	}

	var whole []byte
	from := bufio.NewReader(r.Body)
	for {
		// each chunk starts with a line saying how long it is, in hexadecimal, and what signs it
		line, err := from.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("a chunk with no length: %w", err)
		}
		length, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		size, err := strconv.ParseInt(length, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("a chunk whose length is %q", length)
		}

		// the last chunk is empty, and whatever follows it is checksums nobody here reads
		if size == 0 {
			_, err := io.Copy(io.Discard, from)

			return whole, err
		}

		chunk := make([]byte, size)
		if _, err := io.ReadFull(from, chunk); err != nil {
			return nil, fmt.Errorf("a chunk cut short: %w", err)
		}
		whole = append(whole, chunk...)

		if _, err := from.Discard(2); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
}
