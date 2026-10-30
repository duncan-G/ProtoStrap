package spec

import (
	"strings"
	"testing"
)

// withEmail adds an email section after the docs example's queues, whose domains are the
// given flow list, and has api send mail.
func withEmail(domains string) func(string) string {
	return func(s string) string {
		s = insertAfter("queues:\n  orders: {}\n", "\nemail:\n  domains: "+domains+"\n")(s)
		return replace("    uses: [orders]\n", "    uses: [orders, email]\n")(s)
	}
}

func TestParseEmail(t *testing.T) {
	s := mustParse(t, withEmail("[shop.example.com, mail.example.org]")(docsExample))
	if s.Email == nil || strings.Join(s.Email.Domains, ",") != "shop.example.com,mail.example.org" {
		t.Fatalf("email: %+v", s.Email)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("problems: %v", Problems(problems))
	}
	if !strings.Contains(strings.Join(s.Components["api"].Uses, ","), "email") {
		t.Errorf("api's uses %v", s.Components["api"].Uses)
	}
}

// Without an email section, nothing sends mail and the spec has no Email.
func TestNoEmail(t *testing.T) {
	if s := mustParse(t, docsExample); s.Email != nil {
		t.Errorf("email: %+v", s.Email)
	}
}

func TestCheckEmail(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		path string // It is the problem's path.
		want string // It is a substring of the problem's message.
	}{
		{"no domains", withEmail("[]"), "email.domains", "at least one"},
		{"not a host name", withEmail("[shop..example.com]"), "email.domains[0]", "domain"},
		{"a wildcard", withEmail("[\"*.example.com\"]"), "email.domains[0]", "domain"},
		{"listed twice", withEmail("[shop.example.com, shop.example.com]"), "email.domains[1]", "twice"},
		{"uses email without an email section", replace("    uses: [orders]\n", "    uses: [orders, email]\n"),
			"components.api.uses[1]", "no email section"},
		{"a queue called email", func(s string) string {
			return replace("queues:\n  orders: {}\n", "queues:\n  orders: {}\n  email: {}\n")(withEmail("[shop.example.com]")(s))
		}, "queues.email", "reserved"},
		{"a bucket called email", func(s string) string {
			return withBuckets("  email: {}\n")(withEmail("[shop.example.com]")(s))
		}, "buckets.email", "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Parse([]byte(tt.edit(docsExample)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			problems := s.Check()
			for _, p := range problems {
				if p.Path == tt.path && strings.Contains(p.Msg, tt.want) {
					return
				}
			}
			t.Errorf("no problem at %s containing %q; got:\n%v", tt.path, tt.want, Problems(problems))
		})
	}
}

// An unknown key under email is refused when parsing, like everywhere else in the spec.
func TestEmailUnknownField(t *testing.T) {
	src := insertAfter("queues:\n  orders: {}\n", "\nemail:\n  domains: [shop.example.com]\n  from: [a@shop.example.com]\n")(docsExample)
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("Parse: %v, want an unknown field", err)
	}
}

// Terraform makes the identities and the send permission, so the email section and who uses
// it are in the infra projection. A spec without one hashes as it did before email existed,
// so upgrading ProtoStrap runs no infra.
func TestEmailIsInTheInfraProjection(t *testing.T) {
	base := mustParse(t, docsExample).InfraHash()
	if base != mustParse(t, docsExample).InfraHash() {
		t.Fatal("the infra hash isn't stable")
	}
	one := mustParse(t, withEmail("[shop.example.com]")(docsExample)).InfraHash()
	two := mustParse(t, withEmail("[shop.example.com, mail.example.org]")(docsExample)).InfraHash()
	reordered := mustParse(t, withEmail("[mail.example.org, shop.example.com]")(docsExample)).InfraHash()
	if one == base || two == one {
		t.Errorf("adding email or a domain doesn't change the infra hash")
	}
	if reordered != two {
		t.Errorf("the domains' order changes the infra hash")
	}
	p := mustParse(t, withEmail("[shop.example.com]")(docsExample)).Projection()
	if p.Email == nil || strings.Join(p.Email.Domains, ",") != "shop.example.com" {
		t.Errorf("projection's email: %+v", p.Email)
	}
	if p := mustParse(t, docsExample).Projection(); p.Email != nil {
		t.Errorf("a spec without email projects %+v", p.Email)
	}
}
