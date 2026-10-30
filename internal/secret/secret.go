// Package secret reads the environment's secret: one JSON object of strings, whose keys
// start with a prefix that says who they're for (docs/secrets.md). A component's
// keys are its own, such as Api_ConnectionStrings__shop, and the Shared_ ones it names.
package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// These prefixes are never a component's own: Shared_ keys may be read by every
// component, and Edge_ and Reporter_ are ProtoStrap's.
const (
	Shared   = "Shared_"
	Edge     = "Edge_"
	Reporter = "Reporter_"
)

var (
	prefix  = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*_$`)
	version = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// ValidVersion reports whether v looks like a version ID, which the agent names a directory
// after: letters, digits and dashes.
func ValidVersion(v string) bool { return version.MatchString(v) }

// Name is the environment's secret: <app>/<environment>, as Terraform creates it.
func Name(app, environment string) string { return app + "/" + environment }

// ValidPrefix reports whether p is a capital letter, then letters and digits, then _. One
// _ only, so no prefix starts another.
func ValidPrefix(p string) bool { return prefix.MatchString(p) }

// Reserved reports whether p can't be a component's own prefix.
func Reserved(p string) bool { return p == Shared || p == Edge || p == Reporter }

// Parse reads the secret's value, a JSON object of strings. A secret with no value yet is
// empty. Its errors never quote the value.
func Parse(value string) (map[string]string, error) {
	all := map[string]string{}
	if value == "" {
		return all, nil
	}
	if err := json.Unmarshal([]byte(value), &all); err != nil {
		return nil, errors.New("the secret isn't a JSON object of strings")
	}
	return all, nil
}

// Keys returns a component's keys, without their prefixes: every key with its prefix, and
// the Shared_ ones it names in shared. Its own keys win over Shared_ ones.
func Keys(all map[string]string, prefix string, shared []string) map[string]string {
	own := map[string]string{}
	for _, name := range shared {
		if v, ok := all[Shared+name]; ok && name != "" {
			own[name] = v
		}
	}
	for k, v := range all {
		if name, ok := strings.CutPrefix(k, prefix); ok && name != "" {
			own[name] = v
		}
	}
	return own
}

// Missing returns the names in want that own lacks, in want's order.
func Missing(own map[string]string, want []string) []string {
	var missing []string
	for _, name := range want {
		if _, ok := own[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// Digest is a hash of a component's keys, keyed so that it says nothing about their values
// to anyone without key: equal keys and values give equal digests.
func Digest(key []byte, own map[string]string) string {
	mac := hmac.New(sha256.New, key)
	for _, name := range slices.Sorted(maps.Keys(own)) {
		// Each part is JSON, so a name and value can't be split another way.
		b, _ := json.Marshal([2]string{name, own[name]})
		mac.Write(b)
	}
	return hex.EncodeToString(mac.Sum(nil)[:16])
}
