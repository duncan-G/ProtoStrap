// Package docker is a client for the parts of the Docker Engine API the agent uses, over
// Docker's unix socket.
package docker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/container"
)

// Auth gives credentials for a registry. ok is false when the registry needs none.
type Auth interface {
	Credentials(ctx context.Context, registry string) (user, secret string, ok bool, err error)
}

// Client talks to the Docker daemon.
type Client struct {
	base string
	http *http.Client
	auth Auth

	mu sync.Mutex
	// version is the API version the daemon speaks, which Ping learns.
	version string
}

// quickTimeout bounds calls that should answer at once. Pulls, stops and execs take as
// long as they need, bounded by their context.
const quickTimeout = 30 * time.Second

// New returns a client for the daemon listening on a unix socket.
func New(socket string, auth Auth) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{base: "http://docker", http: &http.Client{Transport: tr}, auth: auth}
}

// NewHTTP returns a client for a daemon at baseURL.
func NewHTTP(baseURL string, hc *http.Client, auth Auth) *Client {
	c := *hc
	c.Timeout = 0 // Each call bounds itself, since a stop may take minutes.
	return &Client{base: strings.TrimSuffix(baseURL, "/"), http: &c, auth: auth}
}

// Error is an error answer from the daemon.
type Error struct {
	Op      string
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("docker %s: %s (HTTP %d)", e.Op, e.Message, e.Status)
}

func statusIs(err error, codes ...int) bool {
	var derr *Error
	if !errors.As(err, &derr) {
		return false
	}
	for _, c := range codes {
		if derr.Status == c {
			return true
		}
	}
	return false
}

func (c *Client) url(path string, query url.Values) string {
	c.mu.Lock()
	v := c.version
	c.mu.Unlock()
	if v != "" {
		path = "/v" + v + path
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// do sends a request. A status of 300 or more is an *Error, unless it is in ok.
func (c *Client) do(ctx context.Context, op, method, path string, query url.Values, body any, header http.Header, ok ...int) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, query), reader)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", op, err)
	}
	if resp.StatusCode < 300 || slices.Contains(ok, resp.StatusCode) {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var msg struct{ Message string }
	if json.Unmarshal(data, &msg) != nil || msg.Message == "" {
		msg.Message = strings.TrimSpace(string(data))
	}
	return nil, &Error{Op: op, Status: resp.StatusCode, Message: msg.Message}
}

// call sends a quick request and decodes its JSON answer into out.
func (c *Client) call(ctx context.Context, op, method, path string, query url.Values, body, out any, ok ...int) error {
	ctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	resp, err := c.do(ctx, op, method, path, query, body, nil, ok...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker %s: decoding the answer: %w", op, err)
	}
	return nil
}

// Ping checks the daemon answers, and learns the API version it speaks.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker ping: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker ping: HTTP %d", resp.StatusCode)
	}
	if v := resp.Header.Get("API-Version"); v != "" {
		c.mu.Lock()
		c.version = v
		c.mu.Unlock()
	}
	return nil
}

// Info describes the VM and its place in the swarm.
func (c *Client) Info(ctx context.Context) (container.Info, error) {
	var info struct {
		NCPU     int
		MemTotal int64
		Swarm    struct {
			LocalNodeState   string
			ControlAvailable bool
			RemoteManagers   []struct{ Addr string }
		}
	}
	if err := c.call(ctx, "info", http.MethodGet, "/info", nil, nil, &info); err != nil {
		return container.Info{}, err
	}
	out := container.Info{CPUs: info.NCPU, MemoryBytes: info.MemTotal,
		Swarm: container.Swarm{State: info.Swarm.LocalNodeState, Manager: info.Swarm.ControlAvailable}}
	for _, m := range info.Swarm.RemoteManagers {
		out.Swarm.Managers = append(out.Swarm.Managers, m.Addr)
	}
	return out, nil
}

type inspect struct {
	ID           string `json:"Id"`
	Name         string
	Created      time.Time
	RestartCount int
	State        struct {
		Status   string
		ExitCode int
	}
	Config struct {
		Image  string
		Labels map[string]string
		Env    []string
	}
	HostConfig struct {
		CpusetCpus string
	}
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string
		}
	}
}

// Containers lists every container, running or not, labelled as a replica.
func (c *Client) Containers(ctx context.Context) ([]container.Container, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {container.LabelReplica}})
	var list []struct {
		ID string `json:"Id"`
	}
	if err := c.call(ctx, "list", http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, nil, &list); err != nil {
		return nil, err
	}
	gateway, err := c.gatewayAddrs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]container.Container, 0, len(list))
	for _, item := range list {
		var in inspect
		err := c.call(ctx, "inspect", http.MethodGet, "/containers/"+item.ID+"/json", nil, nil, &in)
		if statusIs(err, http.StatusNotFound) {
			continue // The container was removed after the list was made.
		}
		if err != nil {
			return nil, err
		}
		out = append(out, toContainer(in, gateway))
	}
	return out, nil
}

// gatewayAddrs maps container IDs to their address on docker_gwbridge, the bridge that
// carries an overlay container's traffic off its VM. That is the address the VM reaches
// the container at.
func (c *Client) gatewayAddrs(ctx context.Context) (map[string]string, error) {
	var network struct {
		Containers map[string]struct{ IPv4Address string }
	}
	err := c.call(ctx, "network", http.MethodGet, "/networks/docker_gwbridge", nil, nil, &network)
	if statusIs(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	addrs := map[string]string{}
	for id, ep := range network.Containers {
		ip, _, _ := strings.Cut(ep.IPv4Address, "/")
		addrs[id] = ip
	}
	return addrs, nil
}

func toContainer(in inspect, gateway map[string]string) container.Container {
	env := make(map[string]string, len(in.Config.Env))
	for _, kv := range in.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	var addrs []string
	if ip := gateway[in.ID]; ip != "" {
		addrs = append(addrs, ip)
	}
	if n, ok := in.NetworkSettings.Networks["bridge"]; ok && n.IPAddress != "" {
		addrs = append(addrs, n.IPAddress)
	}
	return container.Container{
		ID: in.ID, Name: strings.TrimPrefix(in.Name, "/"), Image: in.Config.Image,
		Labels: in.Config.Labels, Env: env,
		Status: container.Status(in.State.Status), ExitCode: in.State.ExitCode, Restarts: in.RestartCount,
		Created: in.Created, CPUSet: in.HostConfig.CpusetCpus, Addrs: addrs,
	}
}

// Stats reads a container's CPU time and memory once.
func (c *Client) Stats(ctx context.Context, id string) (container.Stats, error) {
	var s struct {
		Read     time.Time
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage int64
			Stats map[string]int64
		} `json:"memory_stats"`
	}
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	if err := c.call(ctx, "stats", http.MethodGet, "/containers/"+id+"/stats", q, nil, &s); err != nil {
		return container.Stats{}, err
	}
	// As in `docker stats`, page cache that can be reclaimed isn't counted.
	mem := s.MemoryStats.Usage
	inactive, ok := s.MemoryStats.Stats["inactive_file"] // cgroup v2 reports this.
	if !ok {
		inactive = s.MemoryStats.Stats["total_inactive_file"] // cgroup v1 reports this.
	}
	if inactive > 0 && inactive <= mem {
		mem -= inactive
	}
	return container.Stats{CPUNanos: s.CPUStats.CPUUsage.TotalUsage, MemoryBytes: mem, Read: s.Read}, nil
}

// HasImage reports whether an image is on the VM.
func (c *Client) HasImage(ctx context.Context, image string) (bool, error) {
	err := c.call(ctx, "image", http.MethodGet, "/images/"+image+"/json", nil, nil, nil)
	if statusIs(err, http.StatusNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Pull pulls an image, with credentials for its registry.
func (c *Client) Pull(ctx context.Context, image string) error {
	header := http.Header{}
	if c.auth != nil {
		registry := registryOf(image)
		user, secret, ok, err := c.auth.Credentials(ctx, registry)
		if err != nil {
			return fmt.Errorf("docker pull %s: credentials: %w", image, err)
		}
		if ok {
			auth, _ := json.Marshal(map[string]string{"username": user, "password": secret, "serveraddress": registry})
			header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(auth))
		}
	}
	resp, err := c.do(ctx, "pull", http.MethodPost, "/images/create", url.Values{"fromImage": {image}}, nil, header)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The pull happens while the progress streams; an error arrives as a message in it.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error       string
			ErrorDetail struct{ Message string }
		}
		if err := dec.Decode(&msg); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("docker pull %s: %w", image, err)
		}
		if msg.Error != "" || msg.ErrorDetail.Message != "" {
			return fmt.Errorf("docker pull %s: %s", image, firstOf(msg.ErrorDetail.Message, msg.Error))
		}
	}
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type createBody struct {
	Image            string              `json:"Image"`
	Cmd              []string            `json:"Cmd,omitempty"`
	Env              []string            `json:"Env,omitempty"`
	Labels           map[string]string   `json:"Labels,omitempty"`
	StopTimeout      *int                `json:"StopTimeout,omitempty"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig       hostConfig          `json:"HostConfig"`
	NetworkingConfig *networking         `json:"NetworkingConfig,omitempty"`
}

type hostConfig struct {
	NetworkMode   string                   `json:"NetworkMode,omitempty"`
	Memory        int64                    `json:"Memory,omitempty"`
	MemorySwap    int64                    `json:"MemorySwap,omitempty"`
	CPUShares     int64                    `json:"CpuShares,omitempty"`
	CPUSetCPUs    string                   `json:"CpusetCpus,omitempty"`
	RestartPolicy *restartPolicy           `json:"RestartPolicy,omitempty"`
	LogConfig     *logConfig               `json:"LogConfig,omitempty"`
	Mounts        []mount                  `json:"Mounts,omitempty"`
	PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type restartPolicy struct {
	Name string `json:"Name"`
}

type logConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

type mount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

type networking struct {
	EndpointsConfig map[string]endpoint `json:"EndpointsConfig"`
}

type endpoint struct {
	Aliases []string `json:"Aliases,omitempty"`
}

// Create creates a container from a spec, and returns its ID.
func (c *Client) Create(ctx context.Context, s container.Spec) (string, error) {
	body := createBody{
		Image: s.Image, Cmd: s.Cmd, Env: s.Env, Labels: s.Labels,
		HostConfig: hostConfig{
			NetworkMode: s.Network, Memory: s.MemoryBytes, CPUShares: s.CPUShares, CPUSetCPUs: s.CPUSet,
		},
	}
	if s.MemoryBytes > 0 {
		body.HostConfig.MemorySwap = s.MemoryBytes // A swap limit equal to Memory allows no swap.
	}
	if s.StopTimeout > 0 {
		t := int(s.StopTimeout.Seconds())
		body.StopTimeout = &t
	}
	if s.Restart != "" {
		body.HostConfig.RestartPolicy = &restartPolicy{Name: string(s.Restart)}
	}
	if s.LogDriver != "" {
		body.HostConfig.LogConfig = &logConfig{Type: s.LogDriver, Config: s.LogOptions}
	}
	for _, m := range s.Mounts {
		body.HostConfig.Mounts = append(body.HostConfig.Mounts, mount{Type: "bind", Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	for _, p := range s.Publish {
		key := fmt.Sprintf("%d/%s", p.ContainerPort, cmp.Or(p.Protocol, "tcp"))
		if body.ExposedPorts == nil {
			body.ExposedPorts, body.HostConfig.PortBindings = map[string]struct{}{}, map[string][]portBinding{}
		}
		body.ExposedPorts[key] = struct{}{}
		body.HostConfig.PortBindings[key] = append(body.HostConfig.PortBindings[key], portBinding{HostIP: p.HostIP, HostPort: strconv.Itoa(p.HostPort)})
	}
	if s.Network != "" {
		body.NetworkingConfig = &networking{EndpointsConfig: map[string]endpoint{s.Network: {Aliases: s.Aliases}}}
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.call(ctx, "create "+s.Name, http.MethodPost, "/containers/create", url.Values{"name": {s.Name}}, body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Start starts a container. Starting a running one is not an error.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, "start", http.MethodPost, "/containers/"+id+"/start", nil, nil, nil, http.StatusNotModified)
}

// Stop sends SIGTERM, then SIGKILL once timeout has passed. A container that is stopped
// or gone already is not an error.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+quickTimeout)
	defer cancel()
	q := url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}
	resp, err := c.do(ctx, "stop", http.MethodPost, "/containers/"+id+"/stop", q, nil, nil, http.StatusNotModified, http.StatusNotFound)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Remove removes a container. A missing container is not an error.
func (c *Client) Remove(ctx context.Context, id string) error {
	return c.call(ctx, "remove", http.MethodDelete, "/containers/"+id, url.Values{"force": {"true"}}, nil, nil, http.StatusNotFound)
}

// Exec runs a command in a running container, streaming its output, and returns its exit
// code.
func (c *Client) Exec(ctx context.Context, id string, cmd []string, stdout, stderr io.Writer) (int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{"AttachStdout": true, "AttachStderr": true, "Cmd": cmd}
	if err := c.call(ctx, "exec", http.MethodPost, "/containers/"+id+"/exec", nil, body, &created); err != nil {
		return -1, err
	}
	resp, err := c.do(ctx, "exec start", http.MethodPost, "/exec/"+created.ID+"/start", nil, map[string]bool{"Detach": false, "Tty": false}, nil)
	if err != nil {
		return -1, err
	}
	err = demux(resp.Body, stdout, stderr)
	resp.Body.Close()
	if err != nil {
		return -1, fmt.Errorf("docker exec: %w", err)
	}
	var result struct {
		Running  bool
		ExitCode int
	}
	if err := c.call(ctx, "exec inspect", http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &result); err != nil {
		return -1, err
	}
	return result.ExitCode, nil
}

// demux splits Docker's multiplexed stream, whose frames are each an 8-byte header (the
// stream, 3 zero bytes and a big-endian length) followed by a payload.
func demux(r io.Reader, stdout, stderr io.Writer) error {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("reading a frame header: %w", err)
		}
		var w io.Writer
		switch header[0] {
		case 1:
			w = stdout
		case 2:
			w = stderr
		default:
			return fmt.Errorf("unknown stream %d", header[0])
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		if n, err := io.CopyN(w, r, size); err != nil {
			if n < size && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				return fmt.Errorf("truncated frame: %w", err)
			}
			return err
		}
	}
}

// JoinSwarm joins the swarm as a worker.
func (c *Client) JoinSwarm(ctx context.Context, j container.Join) error {
	body := map[string]any{
		"ListenAddr": "0.0.0.0:2377", "AdvertiseAddr": j.AdvertiseAddr, "RemoteAddrs": j.Managers, "JoinToken": j.Token,
	}
	return c.call(ctx, "swarm join", http.MethodPost, "/swarm/join", nil, body, nil)
}

// LeaveSwarm leaves the swarm. Not being in one is not an error.
func (c *Client) LeaveSwarm(ctx context.Context) error {
	err := c.call(ctx, "swarm leave", http.MethodPost, "/swarm/leave", nil, nil, nil)
	var derr *Error
	if errors.As(err, &derr) && derr.Status == http.StatusServiceUnavailable && strings.Contains(derr.Message, "not part of a swarm") {
		return nil
	}
	return err
}
