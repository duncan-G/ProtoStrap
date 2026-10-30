package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/docker"
	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/scheduler"
	"github.com/duncan-G/protostrap/internal/telemetry"
	"github.com/duncan-G/protostrap/internal/ui"
)

// schedulerConfig is /etc/protostrap/scheduler.json on data-1, which its user data writes
// (infra/platform/data.tf). It holds only what never changes: the scheduler finds Auto
// Scaling groups by tag, and queues and roles by name.
type schedulerConfig struct {
	App          string `json:"app"`
	Environment  string `json:"environment"`
	Region       string `json:"region"`
	DeployBucket string `json:"deploy_bucket"`
	FilesBucket  string `json:"files_bucket"`
	Registry     string `json:"registry"`
	// Listen is where the API listens. It defaults to :7443.
	Listen string `json:"listen"`
	// Address is data-1's fixed address.
	Address string `json:"address"`
	// StateDir is on the control volume and holds the API's TLS key and certificate.
	StateDir string `json:"state_dir"`
	// LogGroup is the CloudWatch Logs group that earlier versions wrote their decisions to.
	// The infra log replaces it, so it is ignored, but still read: data-1's config is written
	// once, when it boots.
	LogGroup     string `json:"log_group"`
	ReportSecret string `json:"report_secret"`
	ReportModel  string `json:"report_model"`
	// IdentityCertificate is AWS's public certificate for the region, a PEM file, for
	// checking instance identity signatures as well as addresses. It is optional.
	IdentityCertificate string `json:"identity_certificate"`
	DockerSocket        string `json:"docker_socket"`
	// UIListen is where the operator UI listens for Platform Admins, and UIAdminListen for
	// Org Admins, on loopback only, for operators' SSM port forwards
	// (docs/protostrap.md#operator-ui). Each role's session document forwards its own port,
	// so the port is the role. An empty value turns that listener off. Neither is written by
	// Terraform, so an earlier scheduler never meets a key it doesn't know.
	UIListen      string `json:"ui_listen"`
	UIAdminListen string `json:"ui_admin_listen"`
	// TelemetryListen is where Grafana is served, from the telemetry replica's socket, on
	// loopback only, for the protostrap-telemetry port forward (docs/telemetry.md). It is a
	// port of its own, apart from the UI's: Developers may forward it, and must never reach
	// the infra log. Terraform doesn't write it either.
	TelemetryListen string `json:"telemetry_listen"`
}

// uiListener is one of the operator UI's listeners, and the role it serves.
type uiListener struct {
	addr string
	role ui.Role
}

func (c schedulerConfig) uiListeners() []uiListener {
	var out []uiListener
	if c.UIListen != "" {
		out = append(out, uiListener{c.UIListen, ui.RolePlatformAdmin})
	}
	if c.UIAdminListen != "" {
		out = append(out, uiListener{c.UIAdminListen, ui.RoleOrgAdmin})
	}
	return out
}

func loadSchedulerConfig(path string) (schedulerConfig, error) {
	c := schedulerConfig{Listen: ":7443", StateDir: "/var/lib/protostrap/control/scheduler", DockerSocket: "/var/run/docker.sock",
		UIListen: "127.0.0.1:7480", UIAdminListen: "127.0.0.1:7481", TelemetryListen: telemetry.ProxyAddr}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	var errs []error
	for name, v := range map[string]string{"app": c.App, "region": c.Region, "deploy_bucket": c.DeployBucket, "registry": c.Registry, "address": c.Address} {
		if v == "" {
			errs = append(errs, fmt.Errorf("no %s", name))
		}
	}
	if c.UIListen != "" && !loopbackListen(c.UIListen) {
		errs = append(errs, fmt.Errorf("ui_listen %q: want a loopback address and port, such as 127.0.0.1:7480: the UI has no sign-in of its own", c.UIListen))
	}
	if c.UIAdminListen != "" && !loopbackListen(c.UIAdminListen) {
		errs = append(errs, fmt.Errorf("ui_admin_listen %q: want a loopback address and port, such as 127.0.0.1:7481: the UI has no sign-in of its own", c.UIAdminListen))
	}
	if c.UIAdminListen != "" && c.UIAdminListen == c.UIListen {
		errs = append(errs, fmt.Errorf("ui_admin_listen %q is ui_listen too: each role needs a port of its own", c.UIAdminListen))
	}
	if c.TelemetryListen != "" && !loopbackListen(c.TelemetryListen) {
		errs = append(errs, fmt.Errorf("telemetry_listen %q: want a loopback address and port, such as 127.0.0.1:7482: Grafana has no sign-in of its own", c.TelemetryListen))
	}
	if c.TelemetryListen != "" && (c.TelemetryListen == c.UIListen || c.TelemetryListen == c.UIAdminListen) {
		errs = append(errs, fmt.Errorf("telemetry_listen %q is the operator UI's too: Developers may forward Grafana's port, and must never reach the UI", c.TelemetryListen))
	}
	if err := errors.Join(errs...); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// loopbackListen reports whether a listen address is on loopback only: 127.0.0.1, ::1 or
// localhost, with a port.
func loopbackListen(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localAPI is the scheduler's API as `protostrap deploy` and `status` reach it, on data-1.
func (c schedulerConfig) localAPI() string {
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil || port == "" {
		port = "7443"
	}
	return "127.0.0.1:" + port
}

func runScheduler(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("scheduler", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", "/etc/protostrap/scheduler.json", "the scheduler's config file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	stderr := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := loadSchedulerConfig(*path)
	if err != nil {
		stderr.Error("loading the config", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := startScheduler(ctx, cfg); err != nil {
		stderr.Error("scheduler stopped", "err", err)
		return 1
	}
	return 0
}

// identityDocument reads the fields of this VM's identity document the scheduler needs.
func identityDocument(ctx context.Context, imds *aws.IMDS) (account, instance string, err error) {
	id, err := imds.Identity(ctx)
	if err != nil {
		return "", "", err
	}
	var doc struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal([]byte(id.Document), &doc); err != nil {
		return "", "", err
	}
	return doc.AccountID, id.InstanceID, nil
}

// startScheduler wires the scheduler to S3, SQS, Auto Scaling, EC2, CloudWatch Logs and the
// local Docker, publishes its certificate, and serves its API until ctx is done.
func startScheduler(ctx context.Context, cfg schedulerConfig) error {
	imds := &aws.IMDS{}
	account, instance, err := identityDocument(ctx, imds)
	if err != nil {
		return fmt.Errorf("instance identity: %w", err)
	}
	creds := &aws.CachedProvider{Source: imds, Window: 5 * time.Minute}
	region := cfg.Region

	// The scheduler's records go to the systemd journal and to the infra log, which keeps the
	// last day, with the agents' records, for the operator UI, and writes every hour to the
	// deploy bucket, for the reporter and for the next run (docs/protostrap.md#infra-log).
	s3 := &aws.S3{Region: region, Credentials: creds}
	infra := &infralog.Log{Archive: &infralog.Archive{Bucket: s3, Name: cfg.DeployBucket, Writer: infraLogWriter(time.Now(), instance)}}
	log := slog.New(infra.Handler(slog.NewJSONHandler(os.Stderr, nil), infralog.SourceScheduler, instance, "data-1"))
	infra.Logger = log
	loading, cancelLoad := context.WithTimeout(ctx, 30*time.Second)
	if err := infra.Load(loading); err != nil {
		log.Warn("the infra log can't read the runs before this one", "err", err)
	}
	cancelLoad()
	logCtx, stopLog := context.WithCancel(ctx)
	written := make(chan struct{})
	go func() { infra.Run(logCtx); close(written) }()
	// The infra log writes what it has before the scheduler exits.
	defer func() { stopLog(); <-written }()

	dockerClient := docker.New(cfg.DockerSocket, nil)
	if err := dockerClient.Ping(ctx); err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	// The key components' keys are hashed with stays on the control volume, so a restarted
	// scheduler knows which keys changed.
	secretKey, err := scheduler.LoadOrCreateSecretKey(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("the secret digest key: %w", err)
	}
	s, err := scheduler.New(scheduler.Config{
		App: cfg.App, Environment: cfg.Environment, Region: region, Account: account, Registry: cfg.Registry,
		FilesBucket: cfg.FilesBucket, Address: cfg.Address, Version: version,
	}, scheduler.Deps{
		Store:     deployStore{s3, cfg.DeployBucket},
		Queues:    queueDepths{&aws.SQS{Region: region, Credentials: creds}},
		Secrets:   envSecret{&aws.SecretsManager{Region: region, Credentials: creds}},
		SecretKey: secretKey,
		Groups:    autoScaling{&aws.AutoScaling{Region: region, Credentials: creds}, cfg.App},
		Swarm: swarm{dockerClient, docker.SwarmSettings{Advertise: cfg.Address, Network: "protostrap",
			Subnet: "10.200.0.0/22", AddressPool: "10.201.0.0/16"}},
		Logger: log,
	})
	if err != nil {
		return err
	}
	if err := s.Start(ctx); err != nil {
		return err
	}

	// The API's certificate lives on the control volume; agents fetch it from the bucket.
	cert, certPEM, err := scheduler.LoadOrCreateCertificate(cfg.StateDir, cfg.Address)
	if err != nil {
		return fmt.Errorf("the API's certificate: %w", err)
	}
	if err := s3.Put(ctx, cfg.DeployBucket, "scheduler/ca.pem", certPEM); err != nil {
		log.Error("publishing the API's certificate", "err", err)
	}
	// New VMs install the version the scheduler runs.
	if err := s3.Put(ctx, cfg.DeployBucket, "protostrap/current", []byte(version+"\n")); err != nil {
		log.Error("publishing the current version", "err", err)
	}

	verifier := &scheduler.IdentityVerifier{Account: account, Region: region,
		Directory: instances{&aws.EC2{Region: region, Credentials: creds}, cfg.App}}
	if cfg.IdentityCertificate != "" {
		pemBytes, err := os.ReadFile(cfg.IdentityCertificate)
		if err != nil {
			return err
		}
		if verifier.Certificate, err = scheduler.ParseCertificate(pemBytes); err != nil {
			return fmt.Errorf("%s: %w", cfg.IdentityCertificate, err)
		}
	}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           &scheduler.Server{Scheduler: s, Verifier: verifier, Logs: infra, Logger: log},
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServeTLS("", "") }()
	go s.Run(ctx)
	log.Info("scheduler listening", "addr", cfg.Listen, "version", version)
	// The UI is a convenience: if it can't listen, the scheduler carries on without it.
	for _, u := range cfg.uiListeners() {
		uiServer := &http.Server{
			Handler: &ui.Server{Status: s.Status, Role: u.role, Logs: infra,
				Reports: reportsBucket{s3: s3, bucket: cfg.DeployBucket}, Logger: log},
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		l, err := net.Listen("tcp", u.addr)
		if err != nil {
			log.Error("the operator UI can't listen", "addr", u.addr, "role", u.role, "err", err)
			continue
		}
		go uiServer.Serve(l)
		defer uiServer.Close()
		log.Info("operator UI listening", "addr", u.addr, "role", u.role)
	}
	// Grafana is served from the telemetry replica's socket, for the protostrap-telemetry
	// forward. Like the UI, if it can't listen, the scheduler carries on without it.
	if cfg.TelemetryListen != "" {
		grafana := &http.Server{
			Handler:           &telemetry.Proxy{Socket: telemetry.GrafanaSocket, Logger: log},
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		if l, err := net.Listen("tcp", cfg.TelemetryListen); err != nil {
			log.Error("Grafana's listener can't listen", "addr", cfg.TelemetryListen, "err", err)
		} else {
			go grafana.Serve(l)
			defer grafana.Close()
			log.Info("Grafana listening", "addr", cfg.TelemetryListen)
		}
	}
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	case err := <-errs:
		return err
	}
}

// infraLogWriter names a scheduler run's objects in the infra log's archive: when it
// started, and on which instance. A run rewrites only its own objects.
func infraLogWriter(started time.Time, instance string) string {
	return started.UTC().Format("20060102T150405Z") + "-" + instance
}

// deployStore is the deploy bucket, as the scheduler uses it.
type deployStore struct {
	s3     *aws.S3
	bucket string
}

func (d deployStore) Release(ctx context.Context, commit string) (release.Release, error) {
	body, err := d.s3.Get(ctx, d.bucket, "releases/"+commit+".json")
	if errors.Is(err, aws.ErrNotFound) {
		return release.Release{}, fmt.Errorf("releases/%s.json: %w", commit, scheduler.ErrNotFound)
	}
	if err != nil {
		return release.Release{}, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		return release.Release{}, err
	}
	return release.Parse(b)
}

func (d deployStore) State(ctx context.Context) (release.State, error) {
	body, err := d.s3.Get(ctx, d.bucket, "state.json")
	if errors.Is(err, aws.ErrNotFound) {
		return release.State{}, nil
	}
	if err != nil {
		return release.State{}, err
	}
	defer body.Close()
	var st release.State
	if err := json.NewDecoder(body).Decode(&st); err != nil {
		return release.State{}, fmt.Errorf("state.json: %w", err)
	}
	return st, nil
}

// Hosts reads hosts.json, which the account's Terraform config writes: each of the spec's
// hosts' hostnames in this account.
func (d deployStore) Hosts(ctx context.Context) (map[string]string, error) {
	body, err := d.s3.Get(ctx, d.bucket, "hosts.json")
	if errors.Is(err, aws.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var hosts map[string]string
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&hosts); err != nil {
		return nil, fmt.Errorf("hosts.json: %w", err)
	}
	return hosts, nil
}

func (d deployStore) SaveState(ctx context.Context, st release.State) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		return err
	}
	return d.s3.Put(ctx, d.bucket, "state.json", b.Bytes())
}

// reportsBucket is the reporter's reports in the deploy bucket, reports/<date>.md, for the
// operator UI.
type reportsBucket struct {
	s3 interface {
		List(ctx context.Context, bucket, prefix string) ([]aws.Object, error)
		Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	}
	bucket string
}

func (r reportsBucket) Reports(ctx context.Context) ([]ui.Report, error) {
	objects, err := r.s3.List(ctx, r.bucket, "reports/")
	if err != nil {
		return nil, err
	}
	var out []ui.Report
	for _, o := range objects {
		date, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, "reports/"), ".md")
		if !ok {
			continue
		}
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			continue
		}
		out = append(out, ui.Report{Date: date, Size: o.Size, Modified: o.Modified})
	}
	return out, nil
}

func (r reportsBucket) Report(ctx context.Context, date string) ([]byte, error) {
	body, err := r.s3.Get(ctx, r.bucket, "reports/"+date+".md")
	if errors.Is(err, aws.ErrNotFound) {
		return nil, ui.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 4<<20))
}

// envSecret reads the environment's secret with data-1's role: its version every minute,
// and its value only when the version is new, to hash each component's keys.
type envSecret struct{ sm *aws.SecretsManager }

func (e envSecret) CurrentVersion(ctx context.Context, id string) (string, error) {
	return e.sm.CurrentVersion(ctx, id)
}

func (e envSecret) Read(ctx context.Context, id, version string) (string, error) {
	value, _, err := e.sm.GetSecretValueVersion(ctx, id, version)
	return value, err
}

type queueDepths struct{ sqs *aws.SQS }

func (q queueDepths) Depth(ctx context.Context, _, url string) (scheduler.Depth, error) {
	visible, inFlight, err := q.sqs.Depth(ctx, url)
	return scheduler.Depth{Visible: visible, InFlight: inFlight}, err
}

// autoScaling is the application's groups, found by their protostrap:app tag.
type autoScaling struct {
	as  *aws.AutoScaling
	app string
}

func (a autoScaling) Groups(ctx context.Context) ([]scheduler.Group, error) {
	infos, err := a.as.Groups(ctx, a.app)
	if err != nil {
		return nil, err
	}
	var out []scheduler.Group
	for _, g := range infos {
		pool := g.Tags["protostrap:pool"]
		if pool == "" {
			continue
		}
		sg := scheduler.Group{Name: g.Name, Pool: pool, Zone: g.Tags["protostrap:zone"], Spot: g.Tags["protostrap:capacity"] == "spot",
			Min: g.Min, Max: g.Max, Desired: g.Desired}
		for _, in := range g.Instances {
			sg.Instances = append(sg.Instances, scheduler.Instance{ID: in.ID, State: in.State})
		}
		out = append(out, sg)
	}
	return out, nil
}

func (a autoScaling) SetDesired(ctx context.Context, group string, desired int) error {
	return a.as.SetDesiredCapacity(ctx, group, desired)
}

func (a autoScaling) Terminate(ctx context.Context, instance string) error {
	return a.as.TerminateInstance(ctx, instance, true)
}

func (a autoScaling) SetUnhealthy(ctx context.Context, instance string) error {
	return a.as.SetUnhealthy(ctx, instance)
}

type swarm struct {
	docker   *docker.Client
	settings docker.SwarmSettings
}

func (s swarm) Join(ctx context.Context) (api.Join, error) {
	return s.docker.ManageSwarm(ctx, s.settings)
}

func (s swarm) Nodes(ctx context.Context) ([]scheduler.Node, error) {
	nodes, err := s.docker.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, scheduler.Node{ID: n.ID, Addr: n.Addr, State: n.State})
	}
	return out, nil
}

func (s swarm) RemoveNode(ctx context.Context, id string) error { return s.docker.RemoveNode(ctx, id) }

// instances lists the application's VMs for the API's identity checks.
type instances struct {
	ec2 *aws.EC2
	app string
}

func (i instances) Instances(ctx context.Context) (map[string]string, error) {
	all, err := i.ec2.InstancesByTag(ctx, "protostrap:app", i.app)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(all))
	for id, in := range all {
		out[id] = in.PrivateIP
	}
	return out, nil
}
