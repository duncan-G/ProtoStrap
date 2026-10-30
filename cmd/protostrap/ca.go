package main

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/duncan-G/protostrap/internal/aws"
)

// caSource is the scheduler's certificate: the copy on disk, or the one data-1 publishes in
// the deploy bucket, which is then kept on disk.
type caSource struct {
	s3     *aws.S3
	bucket string
	key    string
	file   string
}

func (c caSource) CA(ctx context.Context, refresh bool) ([]byte, error) {
	if !refresh && c.file != "" {
		if b, err := os.ReadFile(c.file); err == nil {
			return b, nil
		}
	}
	body, err := c.s3.Get(ctx, c.bucket, c.key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, err
	}
	if c.file != "" {
		os.MkdirAll(filepath.Dir(c.file), 0o755)
		os.WriteFile(c.file, b, 0o644)
	}
	return b, nil
}
