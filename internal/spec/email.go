package spec

import (
	"fmt"
	"slices"
	"strings"
)

// Email is the domains the application sends mail from with Amazon SES. Terraform verifies
// each one, with DKIM and a MAIL FROM domain, and lets the components that have email in
// their uses send from any address in them (docs/email.md).
type Email struct {
	Domains []string `yaml:"domains"`
}

// emailUse is what a component names in uses to send mail.
const emailUse = "email"

// emailProjection is what Terraform makes for email: an identity per domain.
type emailProjection struct {
	Domains []string `json:"domains"`
}

func (e *Email) projection() *emailProjection {
	if e == nil {
		return nil
	}
	return &emailProjection{Domains: slices.Sorted(slices.Values(e.Domains))}
}

func (c *checker) email() {
	e := c.s.Email
	if e == nil {
		return
	}
	if len(e.Domains) == 0 {
		c.add("email.domains", "list at least one domain to send mail from")
	}
	seen := map[string]bool{}
	for i, d := range e.Domains {
		at := fmt.Sprintf("email.domains[%d]", i)
		switch {
		case strings.HasPrefix(d, "*.") || !hostName.MatchString(d):
			c.add(at, "%q isn't a domain to send mail from, such as example.com", d)
		case seen[d]:
			c.add(at, "%s is listed twice", d)
		}
		seen[d] = true
	}
}
