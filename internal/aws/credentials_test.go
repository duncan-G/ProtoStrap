package aws

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingProvider struct {
	calls atomic.Int32
	next  func(n int32) (Credentials, error)
}

func (p *countingProvider) Retrieve(context.Context) (Credentials, error) {
	return p.next(p.calls.Add(1))
}

func TestCachedProviderReusesValidCredentials(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	src := &countingProvider{next: func(n int32) (Credentials, error) {
		return Credentials{AccessKeyID: "a", Expires: now.Add(time.Hour)}, nil
	}}
	c := &CachedProvider{Source: src, Window: 5 * time.Minute, Now: func() time.Time { return now }}
	for range 5 {
		if _, err := c.Retrieve(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if src.calls.Load() != 1 {
		t.Fatalf("fetched %d times", src.calls.Load())
	}
}

func TestCachedProviderRefreshesBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	src := &countingProvider{next: func(n int32) (Credentials, error) {
		return Credentials{AccessKeyID: string(rune('0' + n)), Expires: now.Add(10 * time.Minute)}, nil
	}}
	clock := now
	c := &CachedProvider{Source: src, Window: 5 * time.Minute, Now: func() time.Time { return clock }}
	c.Retrieve(t.Context())
	clock = now.Add(6 * time.Minute) // Only 4 minutes are left, which is inside the window.
	got, _ := c.Retrieve(t.Context())
	if got.AccessKeyID != "2" {
		t.Fatalf("expected a refresh, got %q", got.AccessKeyID)
	}
}

func TestCachedProviderKeepsUsableCredentialsWhenRefreshFails(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	src := &countingProvider{next: func(n int32) (Credentials, error) {
		if n == 1 {
			return Credentials{AccessKeyID: "first", Expires: now.Add(10 * time.Minute)}, nil
		}
		return Credentials{}, errors.New("throttled")
	}}
	clock := now
	c := &CachedProvider{Source: src, Window: 5 * time.Minute, Now: func() time.Time { return clock }}
	c.Retrieve(t.Context())
	clock = now.Add(7 * time.Minute) // The credentials are in the window but haven't expired.
	got, err := c.Retrieve(t.Context())
	if err != nil || got.AccessKeyID != "first" {
		t.Fatalf("still-valid credentials should be served when a refresh fails: %v %v", got, err)
	}
	clock = now.Add(11 * time.Minute) // The credentials have expired.
	if _, err := c.Retrieve(t.Context()); err == nil {
		t.Fatal("expired credentials must not be served")
	}
}

func TestCachedProviderIsSafeForConcurrentUse(t *testing.T) {
	now := time.Now()
	src := &countingProvider{next: func(int32) (Credentials, error) {
		time.Sleep(10 * time.Millisecond)
		return Credentials{AccessKeyID: "a", Expires: now.Add(time.Hour)}, nil
	}}
	c := &CachedProvider{Source: src, Window: time.Minute}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { c.Retrieve(t.Context()) })
	}
	wg.Wait()
	if src.calls.Load() != 1 {
		t.Fatalf("concurrent callers should share one fetch, got %d", src.calls.Load())
	}
}
