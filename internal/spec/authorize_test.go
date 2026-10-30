package spec

import (
	"strings"
	"testing"
)

// withAuthorizer adds an auth service on /auth/ to the docs example, and has the api's routes
// authorized by it.
func withAuthorizer(authorize string) func(string) string {
	return func(s string) string {
		s = insertAfter("components:\n", `  auth:
    kind: service
    build: services/auth
    pool: general
    request: { cpu: 250m, memory: 256Mi }
    port: 8080
    ready: /healthz
    routes: [shop/auth/]
    stop: 20s
`)(s)
		return replace("    routes: [shop/api/]\n", "    routes: [shop/api/]\n    authorize: "+authorize+"\n")(s)
	}
}

// A service's routes can be authorized by another service, which the edge asks about each
// request before sending it on (docs/protostrap.md, "Authorizing requests").
func TestParseAuthorize(t *testing.T) {
	s := mustParse(t, withAuthorizer("{ by: auth, path: /auth/check, headers: [x-identity], forward: [cookie] }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	a := s.Components["api"].Authorize
	if a == nil || a.By != "auth" || a.Path != "/auth/check" || strings.Join(a.Headers, ",") != "x-identity" || strings.Join(a.Forward, ",") != "cookie" {
		t.Errorf("authorize = %+v", a)
	}
	if got := s.Components["auth"].Authorize; got != nil {
		t.Errorf("auth's own routes are authorized: %+v", got)
	}

	// The path defaults to /: the edge asks for the request's own path.
	s = mustParse(t, withAuthorizer("{ by: auth }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	if got := s.Components["api"].Authorize.CheckPath(); got != "/" {
		t.Errorf("default path %q", got)
	}
}

func TestCheckAuthorize(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		path string
		want string
	}{
		{"no authorizer", withAuthorizer("{ path: /check }"), "components.api.authorize.by", "required"},
		{"unknown authorizer", withAuthorizer("{ by: nobody }"), "components.api.authorize.by", "no component nobody"},
		{"authorizer isn't a service", withAuthorizer("{ by: postgres }"), "components.api.authorize.by", "postgres is a stateful component: only a service answers the edge"},
		{"authorizes itself", withAuthorizer("{ by: api }"), "components.api.authorize.by", "can't authorize its own requests"},
		{"relative path", withAuthorizer("{ by: auth, path: check }"), "components.api.authorize.path", "must start with /"},
		{"bad header", withAuthorizer("{ by: auth, headers: [X Identity] }"), "components.api.authorize.headers[0]", "isn't a header name"},
		{"reserved header", withAuthorizer("{ by: auth, headers: [host] }"), "components.api.authorize.headers[0]", "the edge sets host"},
		{"bad forward", withAuthorizer("{ by: auth, forward: [\"cookie:\"] }"), "components.api.authorize.forward[0]", "isn't a header name"},
		{"unrouted", func(s string) string {
			return replace("    routes: [shop/api/]\n", "")(withAuthorizer("{ by: auth }")(s))
		}, "components.api.authorize", "only a service with routes takes requests through the edge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustParse(t, tt.edit(docsExample))
			for _, p := range s.Check() {
				if p.Path == tt.path && strings.Contains(p.Msg, tt.want) {
					return
				}
			}
			t.Errorf("want %s: %s, got %v", tt.path, tt.want, Problems(s.Check()))
		})
	}
}
