package spec

import (
	"strings"
	"testing"
	"time"
)

// withAuthorizer adds an auth service, an upstream on /auth/, to the docs example, and has the
// api's routes authorized by it.
func withAuthorizer(authorize string) func(string) string {
	return func(s string) string {
		s = insertAfter("components:\n", `  auth:
    kind: service
    build: services/auth
    pool: general
    request: { cpu: 250m, memory: 256Mi }
    port: 8080
    ready: /healthz
    stop: 20s
`)(s)
		s = insertAfter("  upstreams:\n", "    auth:\n      routes: [shop/auth/]\n")(s)
		return replace("      routes: [shop/api/]\n", "      routes: [shop/api/]\n      authorize: "+authorize+"\n")(s)
	}
}

// A service's routes can be authorized by another service, which the edge asks about each
// request before sending it on (docs/protostrap.md, "Authorizing requests").
func TestParseAuthorize(t *testing.T) {
	s := mustParse(t, withAuthorizer("{ by: auth, path: /auth/check, headers: [x-identity], forward: [cookie] }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	a := s.Upstream("api").Authorize
	if a == nil || a.By != "auth" || a.Path != "/auth/check" || strings.Join(a.Headers, ",") != "x-identity" || strings.Join(a.Forward, ",") != "cookie" {
		t.Errorf("authorize = %+v", a)
	}
	if got := s.Upstream("auth").Authorize; got != nil {
		t.Errorf("auth's own routes are authorized: %+v", got)
	}

	// The path defaults to /: the edge asks for the request's own path. Nothing of the
	// answer reaches the client unless respond names it.
	s = mustParse(t, withAuthorizer("{ by: auth }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	if got := s.Upstream("api").Authorize.CheckPath(); got != "/" {
		t.Errorf("default path %q", got)
	}
	if got := s.Upstream("api").Authorize.Respond; got != nil {
		t.Errorf("respond = %q without respond", got)
	}
}

// An authorizer's 200 can carry headers back to the client, such as a cookie it renewed with
// the session, besides the headers it adds to the request.
func TestParseAuthorizeRespond(t *testing.T) {
	s := mustParse(t, withAuthorizer("{ by: auth, headers: [x-identity], forward: [cookie], respond: [set-cookie, x-session-expires] }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	a := s.Upstream("api").Authorize
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
	if got := s.Upstream("api").Authorize.Respond; len(got) != 0 {
		t.Errorf("respond = %q", got)
	}
}

// An authorizer can be open, so the edge sends on a request it can't answer for, have a
// timeout of its own, and be skipped for a service's static files.
func TestParseAuthorizeOpenTimeoutAndSkip(t *testing.T) {
	s := mustParse(t, withAuthorizer(`{ by: auth, headers: [x-identity], open: true, timeout: 500ms, skip: [/api/assets/, "*.js", "*.woff2"] }`)(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("check: %v", Problems(problems))
	}
	a := s.Upstream("api").Authorize
	if !a.Open || a.Timeout.Duration != 500*time.Millisecond || strings.Join(a.Skip, ",") != "/api/assets/,*.js,*.woff2" {
		t.Errorf("authorize = %+v", a)
	}
	// A prefix can be a whole route.
	s = mustParse(t, withAuthorizer("{ by: auth, timeout: 5s, skip: [/api/] }")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("check: %v", Problems(problems))
	}
	// Without them the edge asks about every request, waits 2s, and answers 503 when the
	// authorizer can't answer.
	a = mustParse(t, withAuthorizer("{ by: auth }")(docsExample)).Upstream("api").Authorize
	if a.Open || a.Timeout.Duration != 0 || a.Skip != nil {
		t.Errorf("authorize = %+v", a)
	}
}

// A timeout that isn't a duration, or an extension the spec doesn't quote, isn't read at all.
func TestParseAuthorizeRefusesWhatIsntAValue(t *testing.T) {
	for _, tt := range []struct{ authorize, want string }{
		{"{ by: auth, timeout: fast }", `gateway.upstreams.api.authorize.timeout: want a duration like "30s"`},
		{"{ by: auth, timeout: -1s }", `gateway.upstreams.api.authorize.timeout: want a duration like "30s"`},
		{"{ by: auth, open: yes }", "gateway.upstreams.api.authorize.open: want true or false"},
		{"{ by: auth, skip: [*.js] }", `quote a value that starts with *, such as "*.js"`},
	} {
		if _, err := Parse([]byte(withAuthorizer(tt.authorize)(docsExample))); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %s", tt.authorize, err, tt.want)
		}
	}
}

func TestCheckAuthorize(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		path string
		want string
	}{
		{"no authorizer", withAuthorizer("{ path: /check }"), "gateway.upstreams.api.authorize.by", "required"},
		{"unknown authorizer", withAuthorizer("{ by: nobody }"), "gateway.upstreams.api.authorize.by", "no component nobody"},
		{"authorizer isn't a service", withAuthorizer("{ by: postgres }"), "gateway.upstreams.api.authorize.by", "postgres is a stateful component: only a service answers the edge"},
		{"authorizes itself", withAuthorizer("{ by: api }"), "gateway.upstreams.api.authorize.by", "can't authorize its own requests"},
		{"relative path", withAuthorizer("{ by: auth, path: check }"), "gateway.upstreams.api.authorize.path", "must start with /"},
		{"bad header", withAuthorizer("{ by: auth, headers: [X Identity] }"), "gateway.upstreams.api.authorize.headers[0]", "isn't a header name"},
		{"reserved header", withAuthorizer("{ by: auth, headers: [host] }"), "gateway.upstreams.api.authorize.headers[0]", "the edge sets host"},
		{"bad forward", withAuthorizer("{ by: auth, forward: [\"cookie:\"] }"), "gateway.upstreams.api.authorize.forward[0]", "isn't a header name"},
		// respond takes header names as headers and forward do: lower case, as the edge
		// passes them.
		{"bad respond", withAuthorizer("{ by: auth, respond: [Set-Cookie] }"), "gateway.upstreams.api.authorize.respond[0]", "isn't a header name"},
		{"respond with a space", withAuthorizer("{ by: auth, respond: [\"set cookie\"] }"), "gateway.upstreams.api.authorize.respond[0]", "isn't a header name"},
		{"respond is checked at each index", withAuthorizer("{ by: auth, respond: [set-cookie, \"x y\"] }"), "gateway.upstreams.api.authorize.respond[1]", "isn't a header name"},
		// A pseudo-header is the response's status line, not a header the answer can lend
		// another response: the edge would have to replace the service's status with the
		// authorizer's 200.
		{"respond with :status", withAuthorizer("{ by: auth, respond: [\":status\"] }"), "gateway.upstreams.api.authorize.respond[0]", "isn't a header name"},
		// The edge frames each response on its own connection to the client. The
		// authorizer's content-length is its own body's, and added to the service's response
		// it would give the client two lengths, or a wrong one.
		{"respond with content-length", withAuthorizer("{ by: auth, respond: [content-length] }"), "gateway.upstreams.api.authorize.respond[0]", "the edge sets content-length itself"},
		// The rest belong to one connection, and HTTP/2 refuses any message that carries
		// them (RFC 9113, section 8.2.2): the client would get a reset stream, not the
		// service's response.
		{"respond with transfer-encoding", withAuthorizer("{ by: auth, respond: [transfer-encoding] }"), "gateway.upstreams.api.authorize.respond[0]", "the edge sets transfer-encoding itself"},
		{"respond with connection", withAuthorizer("{ by: auth, respond: [connection] }"), "gateway.upstreams.api.authorize.respond[0]", "the edge sets connection itself"},
		{"respond with keep-alive", withAuthorizer("{ by: auth, respond: [keep-alive] }"), "gateway.upstreams.api.authorize.respond[0]", "the edge sets keep-alive itself"},
		{"respond with proxy-connection", withAuthorizer("{ by: auth, respond: [proxy-connection] }"), "gateway.upstreams.api.authorize.respond[0]", "the edge sets proxy-connection itself"},
		{"respond with upgrade", withAuthorizer("{ by: auth, respond: [upgrade] }"), "gateway.upstreams.api.authorize.respond[0]", "the edge sets upgrade itself"},
		{"respond with te", withAuthorizer("{ by: auth, respond: [te] }"), "gateway.upstreams.api.authorize.respond[0]", "the edge sets te itself"},
		{"zero timeout", withAuthorizer("{ by: auth, timeout: 0s }"), "gateway.upstreams.api.authorize.timeout", "must be above 0"},
		// The edge gives the service 120s once the authorizer answers, and Cloudflare gives
		// up at 125s, so a longer wait would have Cloudflare answer instead of the edge.
		{"long timeout", withAuthorizer("{ by: auth, timeout: 5001ms }"), "gateway.upstreams.api.authorize.timeout", "must be at most 5s"},
		{"timeout in days", withAuthorizer("{ by: auth, timeout: 1d }"), "gateway.upstreams.api.authorize.timeout", "must be at most 5s"},
		{"skip outside the routes", withAuthorizer("{ by: auth, skip: [/assets/] }"), "gateway.upstreams.api.authorize.skip[0]", "/assets/ isn't under any of the service's routes"},
		{"skip beside a route", withAuthorizer("{ by: auth, skip: [/apix/] }"), "gateway.upstreams.api.authorize.skip[0]", "isn't under any of the service's routes"},
		{"skip is checked at each index", withAuthorizer("{ by: auth, skip: [/api/a/, /b/] }"), "gateway.upstreams.api.authorize.skip[1]", "isn't under any"},
		{"skip without its last slash", withAuthorizer("{ by: auth, skip: [/api/assets] }"), "gateway.upstreams.api.authorize.skip[0]", "isn't a path prefix that starts and ends with /"},
		{"relative skip", withAuthorizer("{ by: auth, skip: [api/assets/] }"), "gateway.upstreams.api.authorize.skip[0]", "isn't a path prefix that starts and ends with /"},
		{"skip of everything", withAuthorizer(`{ by: auth, skip: ["*"] }`), "gateway.upstreams.api.authorize.skip[0]", "or a file extension, such as *.js"},
		// The edge matches the path as RFC 3986 normalises it, which a prefix with // or a
		// dot segment never is.
		{"skip with //", withAuthorizer("{ by: auth, skip: [/api//assets/] }"), "gateway.upstreams.api.authorize.skip[0]", "write it as /api/assets/"},
		{"skip with ..", withAuthorizer("{ by: auth, skip: [/api/x/../assets/] }"), "gateway.upstreams.api.authorize.skip[0]", "write it as /api/assets/"},
		{"upper-case extension", withAuthorizer(`{ by: auth, skip: ["*.JS"] }`), "gateway.upstreams.api.authorize.skip[0]", "isn't a file extension"},
		{"extension with a dot", withAuthorizer(`{ by: auth, skip: ["*.min.js"] }`), "gateway.upstreams.api.authorize.skip[0]", "isn't a file extension"},
		{"long extension", withAuthorizer(`{ by: auth, skip: ["*.abcdefghijk"] }`), "gateway.upstreams.api.authorize.skip[0]", "up to 10 lower-case letters and digits"},
		{"too many extensions", withAuthorizer("{ by: auth, skip: [" + strings.Repeat(`"*.js", `, 30) + `"*.css"] }`), "gateway.upstreams.api.authorize.skip", "at most 30 file extensions"},
		// The edge asks the authorizer on its own: it needn't be an upstream.
		{"authorizer off the gateway", func(s string) string {
			return replace("    auth:\n      routes: [shop/auth/]\n", "")(withAuthorizer("{ by: auth }")(s))
		}, "", ""},
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
