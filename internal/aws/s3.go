package aws

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// S3 reads the agent's binary from the deploy bucket, writes backups, and lists the
// reporter's reports for the operator UI.
type S3 struct {
	// Endpoint is the store's URL, used only with PathStyle. Without PathStyle, requests go
	// to https://<bucket>.s3.<region>.amazonaws.com.
	Endpoint string
	// PathStyle addresses buckets as <Endpoint>/<bucket>, for tests and S3-compatible stores.
	PathStyle   bool
	Region      string
	Credentials Provider
	HTTP        *http.Client
	// PartSize is the number of bytes in each multipart part. It defaults to 16 MiB.
	PartSize int
}

const defaultPartSize = 16 << 20

func (s *S3) objectURL(bucket, key string) *url.URL {
	var u *url.URL
	if s.PathStyle {
		u, _ = url.Parse(strings.TrimSuffix(s.Endpoint, "/"))
		u.Path = "/" + bucket + "/" + key
	} else {
		u = &url.URL{Scheme: "https", Host: bucket + ".s3." + s.Region + ".amazonaws.com", Path: "/" + key}
	}
	// Send the path exactly as it is signed.
	u.RawPath = canonicalPath(u.Path)
	return u
}

func (s *S3) do(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, error) {
	creds, err := s.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("s3: credentials: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	hash := HashHex(body)
	req.Header.Set("X-Amz-Content-Sha256", hash)
	Sign(req, hash, creds, s.Region, "s3", time.Now())
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s: %w", method, u.Path, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, parseError(resp.StatusCode, data)
	}
	return resp, nil
}

// Get opens an object. A missing object is ErrNotFound.
func (s *S3) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, s.objectURL(bucket, key), nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Object is a key in a bucket, as a listing gives it.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// List returns every object whose key starts with prefix, in key order, reading the listing
// page by page.
func (s *S3) List(ctx context.Context, bucket, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		u := s.bucketURL(bucket)
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		u.RawQuery = q.Encode()
		resp, err := s.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			IsTruncated bool
			NextToken   string `xml:"NextContinuationToken"`
			Contents    []struct {
				Key          string
				Size         int64
				LastModified time.Time
			}
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3 list %s/%s: %w", bucket, prefix, err)
		}
		for _, c := range page.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size, Modified: c.LastModified})
		}
		if !page.IsTruncated || page.NextToken == "" {
			return out, nil
		}
		token = page.NextToken
	}
}

func (s *S3) bucketURL(bucket string) *url.URL {
	if s.PathStyle {
		u, _ := url.Parse(strings.TrimSuffix(s.Endpoint, "/"))
		u.Path = "/" + bucket
		return u
	}
	return &url.URL{Scheme: "https", Host: bucket + ".s3." + s.Region + ".amazonaws.com", Path: "/"}
}

// Exists reports whether an object exists.
func (s *S3) Exists(ctx context.Context, bucket, key string) (bool, error) {
	resp, err := s.do(ctx, http.MethodHead, s.objectURL(bucket, key), nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return true, nil
}

// Upload writes everything read from r to an object. Nothing is stored unless r is read to
// its end without an error: a failed multipart upload is aborted.
func (s *S3) Upload(ctx context.Context, bucket, key string, r io.Reader) error {
	size := s.PartSize
	if size <= 0 {
		size = defaultPartSize
	}
	first, done, err := readPart(r, size)
	if err != nil {
		return err
	}
	if done {
		return s.put(ctx, bucket, key, first)
	}
	second, done, err := readPart(r, size)
	if err != nil {
		return err
	}
	if done && len(second) == 0 {
		return s.put(ctx, bucket, key, first)
	}
	return s.multipart(ctx, bucket, key, r, size, first, second, done)
}

// readPart reads up to size bytes. done is true once r is exhausted.
func readPart(r io.Reader, size int) (part []byte, done bool, err error) {
	buf := make([]byte, size)
	n, err := io.ReadFull(r, buf)
	switch {
	case err == nil:
		return buf, false, nil
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return buf[:n], true, nil
	default:
		return nil, false, err
	}
}

func (s *S3) put(ctx context.Context, bucket, key string, body []byte) error {
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(bucket, key), body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type completedPart struct {
	PartNumber int
	ETag       string
}

func (s *S3) multipart(ctx context.Context, bucket, key string, r io.Reader, size int, first, second []byte, done bool) (err error) {
	u := s.objectURL(bucket, key)
	u.RawQuery = "uploads"
	resp, err := s.do(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err := xml.Unmarshal(data, &created); err != nil || created.UploadID == "" {
		return fmt.Errorf("s3: starting a multipart upload of %s: %v", key, err)
	}
	uploadID := created.UploadID
	defer func() {
		if err != nil {
			abort := s.objectURL(bucket, key)
			abort.RawQuery = "uploadId=" + url.QueryEscape(uploadID)
			if resp, aerr := s.do(context.WithoutCancel(ctx), http.MethodDelete, abort, nil); aerr == nil {
				resp.Body.Close()
			}
		}
	}()

	var parts []completedPart
	upload := func(body []byte) error {
		n := len(parts) + 1
		pu := s.objectURL(bucket, key)
		pu.RawQuery = "partNumber=" + strconv.Itoa(n) + "&uploadId=" + url.QueryEscape(uploadID)
		resp, err := s.do(ctx, http.MethodPut, pu, body)
		if err != nil {
			return err
		}
		resp.Body.Close()
		parts = append(parts, completedPart{PartNumber: n, ETag: resp.Header.Get("ETag")})
		return nil
	}
	if err := upload(first); err != nil {
		return err
	}
	if err := upload(second); err != nil {
		return err
	}
	for !done {
		var part []byte
		if part, done, err = readPart(r, size); err != nil {
			return err
		}
		if len(part) > 0 {
			if err := upload(part); err != nil {
				return err
			}
		}
	}

	body, _ := xml.Marshal(struct {
		XMLName xml.Name        `xml:"CompleteMultipartUpload"`
		Parts   []completedPart `xml:"Part"`
	}{Parts: parts})
	cu := s.objectURL(bucket, key)
	cu.RawQuery = "uploadId=" + url.QueryEscape(uploadID)
	resp, err = s.do(ctx, http.MethodPost, cu, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// S3 can answer 200 with an error in the body.
	data, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if bytes.Contains(data, []byte("<Error>")) {
		return parseError(resp.StatusCode, data)
	}
	return nil
}
