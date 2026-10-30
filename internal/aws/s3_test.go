package aws

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a path-style S3 that keeps objects in memory and checks every request is signed
// over the body it carries.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte // It is keyed by "bucket/key".
	uploads   map[string]map[int][]byte
	aborted   []string
	completed []string
	puts      int
	nextID    int
	// listPage is the number of keys per ListObjectsV2 page. It defaults to 1000.
	listPage int
	lists    int
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		s3Error(w, 403, "AccessDenied")
		return
	}
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		s3Error(w, 400, "XAmzContentSHA256Mismatch")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		f.list(w, path, q.Get("prefix"), q.Get("continuation-token"))
	case r.Method == http.MethodGet:
		obj, ok := f.objects[path]
		if !ok {
			s3Error(w, 404, "NoSuchKey")
			return
		}
		w.Write(obj)
	case r.Method == http.MethodHead:
		if _, ok := f.objects[path]; !ok {
			w.WriteHeader(404)
		}
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.nextID++
		id := fmt.Sprintf("upload-%d", f.nextID)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		parts, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			s3Error(w, 404, "NoSuchUpload")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		parts[n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		id := q.Get("uploadId")
		parts := f.uploads[id]
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) != len(parts) {
			s3Error(w, 400, "InvalidPart")
			return
		}
		var all []byte
		for i, p := range req.Parts {
			if p.PartNumber != i+1 || p.ETag != fmt.Sprintf(`"etag-%d"`, i+1) {
				s3Error(w, 400, "InvalidPartOrder")
				return
			}
			all = append(all, parts[p.PartNumber]...)
		}
		f.objects[path] = all
		delete(f.uploads, id)
		f.completed = append(f.completed, path)
		w.Write([]byte(`<CompleteMultipartUploadResult/>`))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		f.aborted = append(f.aborted, path)
		w.WriteHeader(204)
	case r.Method == http.MethodPut:
		f.puts++
		f.objects[path] = body
	default:
		s3Error(w, 400, "Unsupported")
	}
}

// list answers ListObjectsV2 over one bucket. It gives the keys in order, a page at a time,
// and the continuation token is the last key of the page before.
func (f *fakeS3) list(w http.ResponseWriter, bucket, prefix, after string) {
	f.lists++
	page := f.listPage
	if page <= 0 {
		page = 1000
	}
	var keys []string
	for k := range f.objects {
		b, key, _ := strings.Cut(k, "/")
		if b == bucket && strings.HasPrefix(key, prefix) && key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	truncated := len(keys) > page
	if truncated {
		keys = keys[:page]
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&b, "<Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><IsTruncated>%v</IsTruncated>", bucket, prefix, len(keys), truncated)
	if truncated {
		fmt.Fprintf(&b, "<NextContinuationToken>%s</NextContinuationToken>", keys[len(keys)-1])
	}
	for _, k := range keys {
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><LastModified>2026-10-25T06:00:00.000Z</LastModified><Size>%d</Size></Contents>", k, len(f.objects[bucket+"/"+k]))
	}
	b.WriteString("</ListBucketResult>")
	w.Write([]byte(b.String()))
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `<Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func newS3(t *testing.T, f *fakeS3) *S3 {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &S3{Endpoint: srv.URL, PathStyle: true, Region: "eu-west-1", Credentials: testCreds, PartSize: 5}
}

func TestS3Get(t *testing.T) {
	f := newFakeS3()
	f.objects["deploy/protostrap/1.4.1/protostrap-linux-amd64"] = []byte("binary")
	s3 := newS3(t, f)
	rc, err := s3.Get(t.Context(), "deploy", "protostrap/1.4.1/protostrap-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "binary" {
		t.Fatalf("got %q", b)
	}
}

func TestS3GetMissing(t *testing.T) {
	s3 := newS3(t, newFakeS3())
	_, err := s3.Get(t.Context(), "deploy", "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestS3Exists(t *testing.T) {
	f := newFakeS3()
	f.objects["backups/postgres/2026-10-24.gz"] = []byte("x")
	s3 := newS3(t, f)
	if ok, err := s3.Exists(t.Context(), "backups", "postgres/2026-10-24.gz"); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, err := s3.Exists(t.Context(), "backups", "postgres/2026-10-25.gz"); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestS3ListByPrefix(t *testing.T) {
	f := newFakeS3()
	f.objects["deploy/reports/2026-10-23.md"] = []byte("# 23")
	f.objects["deploy/reports/2026-10-24.md"] = []byte("# the 24th")
	f.objects["deploy/releases/9f2c1e7.json"] = []byte("{}")
	f.objects["files/reports/2026-10-24.md"] = []byte("another bucket")
	s3 := newS3(t, f)
	got, err := s3.List(t.Context(), "deploy", "reports/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "reports/2026-10-23.md" || got[1].Key != "reports/2026-10-24.md" {
		t.Fatalf("got %+v, want the two reports in key order", got)
	}
	if got[1].Size != int64(len("# the 24th")) {
		t.Errorf("size %d", got[1].Size)
	}
	if want := time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC); !got[1].Modified.Equal(want) {
		t.Errorf("modified %v, want %v", got[1].Modified, want)
	}
}

func TestS3ListFollowsContinuationTokens(t *testing.T) {
	f := newFakeS3()
	f.listPage = 2
	for d := 1; d <= 5; d++ {
		f.objects[fmt.Sprintf("deploy/reports/2026-10-%02d.md", d)] = []byte("r")
	}
	s3 := newS3(t, f)
	got, err := s3.List(t.Context(), "deploy", "reports/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[4].Key != "reports/2026-10-05.md" {
		t.Fatalf("got %d objects (%+v), want all 5", len(got), got)
	}
	if f.lists != 3 {
		t.Errorf("%d list calls, want 3 pages", f.lists)
	}
}

func TestS3ListEmpty(t *testing.T) {
	s3 := newS3(t, newFakeS3())
	got, err := s3.List(t.Context(), "deploy", "reports/")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want nothing and no error", got, err)
	}
}

func TestS3UploadSmallObjectInOnePut(t *testing.T) {
	f := newFakeS3()
	s3 := newS3(t, f)
	if err := s3.Upload(t.Context(), "backups", "redis/2026-10-24.gz", strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if string(f.objects["backups/redis/2026-10-24.gz"]) != "abc" || f.puts != 1 || len(f.completed) != 0 {
		t.Fatalf("objects=%v puts=%d completed=%v", f.objects, f.puts, f.completed)
	}
}

func TestS3UploadEmptyObject(t *testing.T) {
	f := newFakeS3()
	s3 := newS3(t, f)
	if err := s3.Upload(t.Context(), "b", "k", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if obj, ok := f.objects["b/k"]; !ok || len(obj) != 0 {
		t.Fatal("empty object not written")
	}
}

func TestS3UploadLargeObjectInParts(t *testing.T) {
	f := newFakeS3()
	s3 := newS3(t, f) // newS3 sets 5-byte parts.
	data := "0123456789abcdefghij-tail"
	// This reader returns short reads, as a pipe from a backup does.
	r := io.MultiReader(strings.NewReader(data[:3]), strings.NewReader(data[3:11]), strings.NewReader(data[11:]))
	if err := s3.Upload(t.Context(), "backups", "postgres/2026-10-24.gz", r); err != nil {
		t.Fatal(err)
	}
	if got := string(f.objects["backups/postgres/2026-10-24.gz"]); got != data {
		t.Fatalf("got %q", got)
	}
	if len(f.completed) != 1 || f.puts != 0 {
		t.Fatalf("expected one multipart upload, completed=%v puts=%d", f.completed, f.puts)
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestS3UploadAbortsWhenTheReaderFails(t *testing.T) {
	f := newFakeS3()
	s3 := newS3(t, f)
	boom := errors.New("pg_dumpall exited with 1")
	err := s3.Upload(t.Context(), "backups", "postgres/2026-10-24.gz", &failingReader{data: bytes.Repeat([]byte("x"), 12), err: boom})
	if !errors.Is(err, boom) {
		t.Fatalf("want the reader's error, got %v", err)
	}
	if _, ok := f.objects["backups/postgres/2026-10-24.gz"]; ok {
		t.Fatal("a failed backup must not leave an object at its key")
	}
	if len(f.aborted) != 1 || len(f.uploads) != 0 {
		t.Fatalf("the multipart upload must be aborted: aborted=%v open=%d", f.aborted, len(f.uploads))
	}
}

func TestS3UploadSmallFailureWritesNothing(t *testing.T) {
	f := newFakeS3()
	s3 := newS3(t, f)
	err := s3.Upload(t.Context(), "b", "k", &failingReader{data: []byte("ab"), err: errors.New("boom")})
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := f.objects["b/k"]; ok {
		t.Fatal("nothing should be written")
	}
}

func TestS3ErrorIsParsed(t *testing.T) {
	s3 := &S3{Endpoint: "http://unused", PathStyle: true, Region: "eu-west-1", Credentials: staticCreds{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s3Error(w, 403, "AccessDenied")
	}))
	defer srv.Close()
	s3.Endpoint = srv.URL
	_, err := s3.Get(t.Context(), "b", "k")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "AccessDenied" || apiErr.Status != 403 {
		t.Fatalf("got %v", err)
	}
}

func TestS3VirtualHostedURL(t *testing.T) {
	s3 := &S3{Region: "eu-west-1"}
	u := s3.objectURL("shop-backups", "postgres/2026-10-24.gz")
	if u.String() != "https://shop-backups.s3.eu-west-1.amazonaws.com/postgres/2026-10-24.gz" {
		t.Fatal(u.String())
	}
}

func TestS3KeysAreEscaped(t *testing.T) {
	s3 := &S3{Endpoint: "http://localhost:9000", PathStyle: true}
	u := s3.objectURL("b", "a b/c+d")
	if !strings.HasSuffix(u.EscapedPath(), "/b/a%20b/c+d") && !strings.HasSuffix(u.EscapedPath(), "/b/a%20b/c%2Bd") {
		t.Fatal(u.EscapedPath())
	}
}

func TestPartNumbersAreSequential(t *testing.T) {
	f := newFakeS3()
	s3 := newS3(t, f)
	s3.PartSize = 1
	if err := s3.Upload(t.Context(), "b", "k", strings.NewReader("abcdefghijkl")); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if string(f.objects["b/k"]) != "abcdefghijkl" {
		t.Fatalf("objects %v", keys)
	}
}
