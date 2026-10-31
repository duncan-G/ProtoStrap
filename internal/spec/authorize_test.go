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

	// The path defaults to /: the edge asks for the request's own path. Nothing of the
	// answer reaches the client unless respond names it.
	s = mustParse(t, withAuthorizer("{ by: auth }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	if got := s.Components["api"].Authorize.CheckPath(); got != "/" {
		t.Errorf("default path %q", got)
	}
	if got := s.Components["api"].Authorize.Respond; got != nil {
		t.Errorf("respond = %q without respond", got)
	}
}

// An authorizer's 2xx can carry headers back to the client, such as a cookie it renewed with
// the session, besides the headers it adds to the request.
func TestParseAuthorizeRespond(t *testing.T) {
	s := mustParse(t, withAuthorizer("{ by: auth, headers: [x-identity], forward: [cookie], respond: [set-cookie, x-session-expires] }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	a := s.Components["api"].Authorize
	if got := strings.Join(a.Respond, ","); got != "set-cookie,x-session-expires" {
		t.Errorf("respond = %q", got)
	}
	// A header can go both ways: to the service, and to the client.
	s = mustParse(t, withAuthorizer("{ by: auth, headers: [x-identity], respond: [x-identity] }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("check: %v", Problems(problems))
	}
	// An empty list is the same as none.
	s = mustParse(t, withAuthorizer("{ by: auth, respond: [] }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("check: %v", Problems(problems))
	}
	if got := s.Components["api"].Authorize.Respond; len(got) != 0 {
		t.Errorf("respond = %q", got)
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
		// respond takes header names as headers and forward do: lower case, as the edge
		// passes them.
		{"bad respond", withAuthorizer("{ by: auth, respond: [Set-Cookie] }"), "components.api.authorize.respond[0]", "isn't a header name"},
		{"respond with a space", withAuthorizer("{ by: auth, respond: [\"set cookie\"] }"), "components.api.authorize.respond[0]", "isn't a header name"},
		{"respond is checked at each index", withAuthorizer("{ by: auth, respond: [set-cookie, \"x y\"] }"), "components.api.authorize.respond[1]", "isn't a header name"},
		// A pseudo-header is the response's status line, not a header the answer can lend
		// another response: the edge would have to replace the service's status with the
		// authorizer's 2xx.
		{"respond with :status", withAuthorizer("{ by: auth, respond: [\":status\"] }"), "components.api.authorize.respond[0]", "isn't a header name"},
		// The edge frames each response on its own connection to the client. The
		// authorizer's content-length is its own body's, and added to the service's response
		// it would give the client two lengths, or a wrong one.
		{"respond with content-length", withAuthorizer("{ by: auth, respond: [content-length] }"), "components.api.authorize.respond[0]", "the edge sets content-length itself"},
		// The rest belong to one connection, and HTTP/2 refuses any message that carries
		// them (RFC 9113, section 8.2.2): the client would get a reset stream, not the
		// service's response.
		{"respond with transfer-encoding", withAuthorizer("{ by: auth, respond: [transfer-encoding] }"), "components.api.authorize.respond[0]", "the edge sets transfer-encoding itself"},
		{"respond with connection", withAuthorizer("{ by: auth, respond: [connection] }"), "components.api.authorize.respond[0]", "the edge sets connection itself"},
		{"respond with keep-alive", withAuthorizer("{ by: auth, respond: [keep-alive] }"), "components.api.authorize.respond[0]", "the edge sets keep-alive itself"},
		{"respond with proxy-connection", withAuthorizer("{ by: auth, respond: [proxy-connection] }"), "components.api.authorize.respond[0]", "the edge sets proxy-connection itself"},
		{"respond with upgrade", withAuthorizer("{ by: auth, respond: [upgrade] }"), "components.api.authorize.respond[0]", "the edge sets upgrade itself"},
		{"respond with te", withAuthorizer("{ by: auth, respond: [te] }"), "components.api.authorize.respond[0]", "the edge sets te itself"},
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
