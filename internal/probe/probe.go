// Package probe runs readiness checks against replicas that are starting.
package probe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// CheckGRPC calls grpc.health.v1.Health/Check for the service.
	CheckGRPC = "grpc"
	// CheckTCP checks that the port accepts connections.
	CheckTCP = "tcp"
	// CheckHTTP is a GET of a path.
	CheckHTTP = "http"
)

// Check is a readiness check, written in a spec's `ready` as "grpc", "grpc:9001", "tcp",
// "tcp:9001", "/healthz" or "9000/health/ready" (docs/protostrap.md#readiness-checks).
type Check struct {
	// Kind is CheckGRPC, CheckTCP or CheckHTTP.
	Kind string
	// Port is the port the check runs on. Zero means the replica's own port.
	Port int
	// Path is an HTTP check's path.
	Path string
}

// ParseCheck reads a check as a spec writes it.
func ParseCheck(s string) (Check, error) {
	bad := fmt.Errorf("%q must be grpc, tcp or an HTTP path such as /healthz, each optionally on a port: grpc:9001, tcp:9001, 9000/healthz", s)
	port := func(p string) (int, error) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return 0, bad
		}
		return n, nil
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return Check{}, bad
	}
	for _, kind := range []string{CheckGRPC, CheckTCP} {
		if s == kind {
			return Check{Kind: kind}, nil
		}
		if p, ok := strings.CutPrefix(s, kind+":"); ok {
			n, err := port(p)
			return Check{Kind: kind, Port: n}, err
		}
	}
	if strings.HasPrefix(s, "/") {
		return Check{Kind: CheckHTTP, Path: s}, nil
	}
	if p, path, ok := strings.Cut(s, "/"); ok {
		n, err := port(p)
		return Check{Kind: CheckHTTP, Port: n, Path: "/" + path}, err
	}
	return Check{}, bad
}

// String is the check as a spec writes it.
func (c Check) String() string {
	switch {
	case c.Kind == CheckHTTP && c.Port != 0:
		return strconv.Itoa(c.Port) + c.Path
	case c.Kind == CheckHTTP:
		return c.Path
	case c.Port != 0:
		return c.Kind + ":" + strconv.Itoa(c.Port)
	}
	return c.Kind
}

// ReadyStatuses are the HTTP answers that mean ready: 2xx, and 3xx, since a redirect is an
// answer and the replica is up. The edge's own check uses the same range, so a replica the
// agent calls ready is never one the edge won't send requests to.
var ReadyStatuses = struct{ Start, End int }{200, 400}

// ReadyStatus reports whether an HTTP answer with this status means ready.
func ReadyStatus(status int) bool {
	return status >= ReadyStatuses.Start && status < ReadyStatuses.End
}

// Target is one check: Check is a check as a spec writes it, such as "grpc" or
// "9000/healthz".
type Target struct {
	Addr string
	// Port is the replica's own port, where a check that names no port runs.
	Port  int
	Check string
	// Service is the component's name, which the replica must answer to.
	Service string
	// H2C says the replica's own port speaks cleartext HTTP/2, so an HTTP check there does
	// too. A check on another port speaks HTTP/1.1.
	H2C bool
}

// Prober runs checks with a timeout each.
type Prober struct {
	timeout time.Duration
	http    *http.Client
	grpc    *http.Client
}

func New(timeout time.Duration) *Prober {
	h2c := new(http.Protocols)
	h2c.SetUnencryptedHTTP2(true)
	return &Prober{
		timeout: timeout,
		http: &http.Client{
			Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
			// A redirect is an answer: the replica is up.
			CheckRedirect: noRedirect,
		},
		grpc: &http.Client{Transport: &http.Transport{Proxy: nil, Protocols: h2c, DisableKeepAlives: true}, CheckRedirect: noRedirect},
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Probe returns nil when the target is ready.
func (p *Prober) Probe(ctx context.Context, t Target) error {
	check, err := ParseCheck(t.Check)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	port, h2c := t.Port, t.H2C
	if check.Port != 0 {
		port, h2c = check.Port, false
	}
	if t.Addr == "" || port <= 0 {
		return errors.New("probe: no address")
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	addr := net.JoinHostPort(t.Addr, strconv.Itoa(port))
	switch check.Kind {
	case CheckTCP:
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	case CheckGRPC:
		return p.grpcHealth(ctx, addr, t.Service)
	default:
		client := p.http
		if h2c {
			client = p.grpc
		}
		return p.httpGet(ctx, client, addr, check.Path, t.Service)
	}
}

func (p *Prober) httpGet(ctx context.Context, client *http.Client, addr, path, service string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	// A replica answers its check only when asked for its own service by name, so an
	// address reused by another service never passes.
	req.Host = service
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if !ReadyStatus(resp.StatusCode) {
		return fmt.Errorf("probe: %s answered %d", path, resp.StatusCode)
	}
	return nil
}

// grpcHealth calls the standard gRPC health check for the service, over cleartext HTTP/2.
func (p *Prober) grpcHealth(ctx context.Context, addr, service string) error {
	// The request is HealthCheckRequest{service: 1} in protobuf, inside a gRPC frame.
	msg := append([]byte{0x0a}, protoVarint(uint64(len(service)))...)
	msg = append(msg, service...)
	frame := make([]byte, 5, 5+len(msg))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(msg)))
	frame = append(frame, msg...)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/grpc.health.v1.Health/Check", bytes.NewReader(frame))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := p.grpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if s := grpcStatus(resp); s != "0" {
		return fmt.Errorf("probe: grpc-status %s", s)
	}
	if len(body) < 5 {
		return errors.New("probe: empty health response")
	}
	status, err := servingStatus(body[5:])
	if err != nil {
		return err
	}
	if status != 1 { // 1 is SERVING.
		return fmt.Errorf("probe: health status %d, not SERVING", status)
	}
	return nil
}

func grpcStatus(resp *http.Response) string {
	if s := resp.Trailer.Get("Grpc-Status"); s != "" {
		return s
	}
	if s := resp.Header.Get("Grpc-Status"); s != "" {
		return s // The answer is trailers-only.
	}
	return "missing"
}

// servingStatus reads field 1 of HealthCheckResponse.
func servingStatus(msg []byte) (uint64, error) {
	for len(msg) > 0 {
		key, n := binary.Uvarint(msg)
		if n <= 0 {
			return 0, errors.New("probe: bad health response")
		}
		msg = msg[n:]
		field, wire := key>>3, key&7
		switch wire {
		case 0:
			v, n := binary.Uvarint(msg)
			if n <= 0 {
				return 0, errors.New("probe: bad health response")
			}
			if field == 1 {
				return v, nil
			}
			msg = msg[n:]
		case 2:
			l, n := binary.Uvarint(msg)
			if n <= 0 || uint64(len(msg)-n) < l {
				return 0, errors.New("probe: bad health response")
			}
			msg = msg[n+int(l):]
		default:
			return 0, errors.New("probe: bad health response")
		}
	}
	return 0, nil // The status is UNKNOWN (0), because a default value isn't sent.
}

func protoVarint(v uint64) []byte {
	return binary.AppendUvarint(nil, v)
}
