package spec

import (
	"strings"
	"testing"
	"time"
)

// These tests cover the cdn section (docs/protostrap.md#cdn): Cloudflare in front of the
// tunnel, with the application's own WAF rules, cache rules and per-client rate limit, each
// rule scoped to the application's hosts.

// withCDN adds a cdn section, whose lines follow it, before the docs example's tunnel.
func withCDN(lines string) func(string) string {
	return insertAfter("  data: { vm: m7g.xlarge, count: 1 }\n", "\ncdn:\n"+lines)
}

const cdnExample = `  rate_limit: { requests: 300, period: 1m }
  waf:
    - { name: block-wp, paths: [/wp-admin/, /wp-login.php], action: block }
    - name: shop-geo
      hosts: [shop]
      not_countries: [US, CA]
      methods: [POST, PUT]
      action: managed_challenge
    - { name: office, ips: [203.0.113.0/24, "2001:db8::/32"], action: skip }
    - { name: bots, expression: 'http.user_agent contains "sqlmap"', not_ips: [198.51.100.7], action: js_challenge }
  cache:
    - { name: assets, hosts: [shop], paths: [/assets/], extensions: [js, css], edge_ttl: 30d, browser_ttl: 1d }
    - { name: api, paths: [/api/], cache: false }
    - { name: pages, countries: [GB] }
`

func TestParseCDN(t *testing.T) {
	s := mustParse(t, withCDN(cdnExample)(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	c := s.CDN
	if c == nil || c.RateLimit == nil || c.RateLimit.Requests != 300 || c.RateLimit.Period.Duration != time.Minute {
		t.Fatalf("cdn %+v", c)
	}
	if len(c.WAF) != 4 || len(c.Cache) != 3 {
		t.Fatalf("waf %d, cache %d", len(c.WAF), len(c.Cache))
	}
	geo := c.WAF[1]
	if geo.Name != "shop-geo" || strings.Join(geo.Hosts, ",") != "shop" || strings.Join(geo.NotCountries, ",") != "US,CA" ||
		strings.Join(geo.Methods, ",") != "POST,PUT" || geo.Action != "managed_challenge" {
		t.Errorf("shop-geo %+v", geo)
	}
	if c.WAF[3].Expression != `http.user_agent contains "sqlmap"` {
		t.Errorf("bots' expression %q", c.WAF[3].Expression)
	}
	assets := c.Cache[0]
	if assets.Cache != nil || assets.EdgeTTL.Duration != 30*24*time.Hour || assets.BrowserTTL.Duration != 24*time.Hour ||
		strings.Join(assets.Extensions, ",") != "js,css" {
		t.Errorf("assets %+v", assets)
	}
	if api := c.Cache[1]; api.Cache == nil || *api.Cache {
		t.Errorf("api %+v: cache: false bypasses the cache", api)
	}
}

func TestTheRateLimitsPeriodDefaultsToTenSeconds(t *testing.T) {
	s := mustParse(t, withCDN("  rate_limit: { requests: 100 }\n")(docsExample))
	if s.CDN.RateLimit.Period.Duration != 10*time.Second {
		t.Errorf("period %v, want 10s", s.CDN.RateLimit.Period)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("check: %v", Problems(problems))
	}
}

// TestAnEmptyCDNIsCloudflaresDefaults checks that cdn: {} runs Cloudflare in front of the
// tunnel with ProtoStrap's settings alone.
func TestAnEmptyCDNIsCloudflaresDefaults(t *testing.T) {
	s := mustParse(t, insertAfter("  data: { vm: m7g.xlarge, count: 1 }\n", "\ncdn: {}\n")(docsExample))
	if s.CDN == nil || s.CDN.RateLimit != nil || len(s.CDN.WAF) > 0 || len(s.CDN.Cache) > 0 {
		t.Errorf("cdn %+v", s.CDN)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("check: %v", Problems(problems))
	}
}

func TestCheckCDN(t *testing.T) {
	rule := func(list, r string) func(string) string { return withCDN("  " + list + ":\n    - " + r + "\n") }
	tests := []struct {
		name string
		edit func(string) string
		path string
		want string
	}{
		{"no tunnel", func(s string) string {
			return withCDN("  waf: [{ name: a, action: block }]\n")(replace("tunnel: {}\n\n", "")(s))
		},
			"cdn", "the CDN is Cloudflare in front of the tunnel: add tunnel: {}"},
		{"empty", insertAfter("  data: { vm: m7g.xlarge, count: 1 }\n", "\ncdn:\n"), "cdn", "write cdn: {}"},
		{"no requests", withCDN("  rate_limit: { requests: 0 }\n"), "cdn.rate_limit.requests", "at least 1"},
		{"odd period", withCDN("  rate_limit: { requests: 10, period: 30s }\n"), "cdn.rate_limit.period", "10s, 1m, 2m, 5m, 10m or 1h"},
		{"no name", rule("waf", "{ action: block }"), "cdn.waf[0].name", "required"},
		{"bad name", rule("waf", "{ name: Block_WP, action: block }"), "cdn.waf[0].name", "lowercase letters, digits and dashes"},
		{"name twice", withCDN("  waf:\n    - { name: a, action: block }\n    - { name: a, action: skip }\n"), "cdn.waf[1].name", "also cdn.waf[0]"},
		{"the same name in both lists", withCDN("  waf: [{ name: a, action: block }]\n  cache: [{ name: a }]\n"), "", ""},
		{"unknown host", rule("waf", "{ name: a, hosts: [admin], action: block }"), "cdn.waf[0].hosts[0]", "no host admin in gateway.hosts"},
		{"a hostname for a host", rule("cache", "{ name: a, hosts: [shop.example.com] }"), "cdn.cache[0].hosts[0]", "names a host by its name in gateway.hosts, such as shop"},
		{"relative path", rule("waf", "{ name: a, paths: [admin/], action: block }"), "cdn.waf[0].paths[0]", "isn't a path prefix that starts with /"},
		{"quoted path", rule("waf", `{ name: a, paths: ['/a"b'], action: block }`), "cdn.waf[0].paths[0]", "isn't a path prefix"},
		{"extension with a dot", rule("cache", "{ name: a, extensions: [.js] }"), "cdn.cache[0].extensions[0]", "up to 10 lower-case letters and digits, such as js"},
		{"lower-case method", rule("waf", "{ name: a, methods: [post], action: block }"), "cdn.waf[0].methods[0]", "an HTTP method in upper case, such as POST"},
		{"bad country", rule("waf", "{ name: a, countries: [USA], action: block }"), "cdn.waf[0].countries[0]", "two-letter country code, such as US"},
		{"bad excluded country", rule("waf", "{ name: a, not_countries: [us], action: block }"), "cdn.waf[0].not_countries[0]", "two-letter country code"},
		{"bad IP", rule("waf", "{ name: a, ips: [10.0.0.300], action: block }"), "cdn.waf[0].ips[0]", "isn't an IP address or range"},
		{"bad excluded IP", rule("waf", "{ name: a, not_ips: [example.com], action: block }"), "cdn.waf[0].not_ips[0]", "isn't an IP address or range"},
		{"blank expression", rule("waf", "{ name: a, expression: ' ', action: block }"), "cdn.waf[0].expression", "empty"},
		{"no action", rule("waf", "{ name: a }"), "cdn.waf[0].action", "required: block, managed_challenge, js_challenge, challenge or skip"},
		{"unknown action", rule("waf", "{ name: a, action: log }"), "cdn.waf[0].action", "block, managed_challenge, js_challenge, challenge or skip"},
		{"cache settings on a WAF rule", rule("waf", "{ name: a, action: block, edge_ttl: 1d }"), "cdn.waf[0].edge_ttl", "goes in a cache rule"},
		{"an action on a cache rule", rule("cache", "{ name: a, action: block }"), "cdn.cache[0].action", "goes in a WAF rule"},
		{"TTLs on a bypass", rule("cache", "{ name: a, cache: false, edge_ttl: 1d }"), "cdn.cache[0].edge_ttl", "a rule that bypasses the cache has no TTLs"},
		{"zero TTL", rule("cache", "{ name: a, browser_ttl: 0s }"), "cdn.cache[0].browser_ttl", "at least 1s"},
		{"TTL under a second", rule("cache", "{ name: a, edge_ttl: 1500ms }"), "cdn.cache[0].edge_ttl", "whole number of seconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustParse(t, tt.edit(docsExample))
			problems := s.Check()
			if tt.path == "" {
				if len(problems) > 0 {
					t.Errorf("want no problems, got %v", Problems(problems))
				}
				return
			}
			for _, p := range problems {
				if p.Path == tt.path && strings.Contains(p.Msg, tt.want) {
					return
				}
			}
			t.Errorf("want %s: %s, got %v", tt.path, tt.want, Problems(problems))
		})
	}
}

// TestTheCDNIsInfrastructure checks that every CDN setting changes the infra hash: Terraform
// makes all of it, and the scheduler none.
func TestTheCDNIsInfrastructure(t *testing.T) {
	base := mustParse(t, withCDN(cdnExample)(docsExample)).InfraHash()
	for _, edit := range []func(string) string{
		func(s string) string { return s },
		replace("requests: 300", "requests: 301"),
		replace("action: block }", "action: challenge }"),
		replace("edge_ttl: 30d", "edge_ttl: 7d"),
		replace("  rate_limit: { requests: 300, period: 1m }\n", ""),
	} {
		src := edit(withCDN(cdnExample)(docsExample))
		got := mustParse(t, src).InfraHash()
		if same := src == withCDN(cdnExample)(docsExample); (got == base) != same {
			t.Errorf("infra hash changed = %v for an edit that changes the spec = %v", got != base, !same)
		}
	}
	if mustParse(t, docsExample).InfraHash() == base {
		t.Errorf("adding the cdn left the infra hash unchanged")
	}
}
