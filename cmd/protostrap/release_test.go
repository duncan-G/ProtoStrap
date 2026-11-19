package main

import (
	"strings"
	"testing"

	"github.com/duncan-G/protostrap/internal/release"
)

// TestTheReleaseNamesItsVersionOnOneLine checks the line the protostrap-deploy document
// reads the release's version from, even with a component named like the field.
func TestTheReleaseNamesItsVersionOnOneLine(t *testing.T) {
	r := release.Release{
		Commit:     "abc1234",
		App:        "shop",
		Version:    "1.4.0",
		Spec:       "app: shop\nprotostrap: 1.4.0\n",
		Components: map[string]release.Component{"protostrap": {Image: "img", Fingerprint: "fp"}},
	}
	got := string(releaseJSON(r))
	if n := strings.Count(got, "\n  \"protostrap\": "); n != 1 || !strings.Contains(got, "\n  \"protostrap\": \"1.4.0\",\n") {
		t.Errorf("the release's version isn't on one top-level line:\n%s", got)
	}
}
