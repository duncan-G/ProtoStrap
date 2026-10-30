package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned for a metadata path or an object that doesn't exist.
var ErrNotFound = errors.New("not found")

// APIError is an error answer from an AWS API.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("aws: %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

// Is makes a 404, and the APIs' own "not found" codes, match ErrNotFound.
func (e *APIError) Is(target error) bool {
	if target != ErrNotFound {
		return false
	}
	return e.Status == 404 || e.Code == "NoSuchKey" || strings.HasSuffix(e.Code, "NotFoundException")
}

// Provider gives credentials to sign requests with.
type Provider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// CachedProvider keeps credentials from Source until they are within Window of expiring.
// If a refresh fails, credentials that haven't expired yet are still used.
type CachedProvider struct {
	Source Provider
	Window time.Duration
	Now    func() time.Time

	mu    sync.Mutex
	creds Credentials
}

func (c *CachedProvider) Retrieve(ctx context.Context) (Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	if !c.creds.ExpiresWithin(now, c.Window) {
		return c.creds, nil
	}
	fresh, err := c.Source.Retrieve(ctx)
	if err != nil {
		if !c.creds.ExpiresWithin(now, 0) {
			return c.creds, nil
		}
		return Credentials{}, err
	}
	c.creds = fresh
	return fresh, nil
}
