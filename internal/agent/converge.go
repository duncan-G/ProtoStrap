package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
	"github.com/duncan-G/protostrap/internal/credentials"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

type opKind int

const (
	opStart opKind = iota
	opStop
)

// op is a start or a stop running in the background.
type op struct {
	kind      opKind
	component string
	cancel    func()
}

// converge makes the VM run its assignment: everything assigned that isn't running is
// started, and everything running that isn't assigned is stopped. Replicas are never
// compared or changed: a replica's settings never change, so its ID says it all.
func (a *Agent) converge(ctx context.Context, as api.Assignment, containers []container.Container, firewallOK bool) {
	a.writeFiles(as.Files)

	desired := make(map[string]api.ReplicaSpec, len(as.Replicas))
	for _, s := range as.Replicas {
		desired[s.ID] = s
	}
	existing := map[string]container.Container{}
	for _, c := range containers {
		existing[c.Replica()] = c
		if _, ok := desired[c.Replica()]; !ok {
			a.stop(ctx, c, a.stopTime(c))
		}
	}
	a.mu.Lock()
	for id, o := range a.ops {
		if _, ok := desired[id]; !ok && o.kind == opStart {
			o.cancel()
		}
	}
	for id := range a.failed {
		if _, ok := desired[id]; !ok {
			delete(a.failed, id)
		}
	}
	a.mu.Unlock()

	cores := a.coresInUse(containers)
	for _, id := range slices.Sorted(maps.Keys(desired)) {
		s := desired[id]
		a.mu.Lock()
		_, busy := a.ops[id]
		_, failed := a.failed[id]
		a.mu.Unlock()
		if busy || failed {
			continue
		}
		if c, ok := existing[id]; ok {
			if a.term == nil && firewallOK {
				a.restart(ctx, c, s)
			}
			continue
		}
		if a.term != nil || !firewallOK {
			continue // Nothing new starts on a VM on its way out, or without its firewall.
		}
		cpuset := ""
		if s.Kind == api.KindStateful || s.Kind == api.KindTelemetry {
			if a.volumeInUse(s, containers) {
				continue // The old replica still owns the volume.
			}
			dir := filepath.Join(a.cfg.DataDir, s.Component)
			mounted, err := a.volumes.Mounted(dir)
			if err != nil {
				a.log.Error("checking a volume", "replica", s.ID, "dir", dir, "err", err)
				continue
			}
			if !mounted {
				a.markFailed(s, fmt.Sprintf("volume %s is not mounted", dir))
				continue
			}
			if s.Kind == api.KindStateful {
				cpuset = a.allocateCores(cores, s.Request.CPU)
			}
		}
		a.start(ctx, s, cpuset)
	}
}

func (a *Agent) markFailed(s api.ReplicaSpec, reason string) {
	a.log.Error("replica failed", "replica", s.ID, "reason", reason)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failed[s.ID] = failure{component: s.Component, release: s.Release, kind: s.Kind, reason: reason}
}

// restart starts an assigned replica's container again: one created but never started,
// which happens when the agent stops between the two, or a ready one stopped by hand.
// A task never runs twice, and a replica that failed is never restarted.
func (a *Agent) restart(ctx context.Context, c container.Container, s api.ReplicaSpec) {
	a.mu.Lock()
	ready := a.ready[s.ID]
	a.mu.Unlock()
	stopped := c.Status == container.Exited || c.Status == container.Dead
	if c.Status != container.Created && !(stopped && ready && s.Kind != api.KindTask) {
		return
	}
	a.log.Info("starting a replica's container again", "replica", s.ID, "status", c.Status)
	if err := a.rt.Start(ctx, c.ID); err != nil {
		a.log.Error("starting a replica", "replica", s.ID, "err", err)
	}
}

// volumeInUse reports whether another replica of a stateful component still has a
// container or is being started. Only one replica can own the volume, so a new one
// starts only once the old one is gone.
func (a *Agent) volumeInUse(s api.ReplicaSpec, containers []container.Container) bool {
	for _, c := range containers {
		if c.Label(container.LabelComponent) == s.Component && c.Replica() != s.ID {
			return true
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, o := range a.ops {
		if o.component == s.Component && id != s.ID {
			return true
		}
	}
	return false
}

// coresInUse are the cores stateful replicas have been given.
func (a *Agent) coresInUse(containers []container.Container) map[int]bool {
	used := map[int]bool{}
	for _, c := range containers {
		cores, err := container.ParseCPUSet(c.CPUSet)
		if err != nil {
			continue
		}
		for _, core := range cores {
			used[core] = true
		}
	}
	return used
}

// allocateCores gives a stateful replica cores of its own, as many as its request rounds
// up to, so a heavy query can't stall Redis's one thread (docs/vms.md#sharing-the-vm).
// Without enough free cores it gets a CPU share like any replica.
func (a *Agent) allocateCores(used map[int]bool, cpu int64) string {
	want := int((cpu + 999) / 1000)
	if want < 1 {
		want = 1
	}
	var picked []int
	for core := 0; core < int(a.capacity.CPU/1000) && len(picked) < want; core++ {
		if !used[core] {
			picked = append(picked, core)
		}
	}
	if len(picked) < want {
		a.log.Warn("not enough free cores for a stateful replica; it shares the VM's", "want", want, "free", len(picked))
		return ""
	}
	for _, core := range picked {
		used[core] = true
	}
	return container.FormatCPUSet(picked)
}

// spec is the container a replica runs in (docs/protostrap.md#agent, "How it runs a
// replica").
func (a *Agent) spec(s api.ReplicaSpec, token, cpuset string) container.Spec {
	stop := s.Stop
	if stop <= 0 {
		stop = int(defaultStop.Seconds())
	}
	labels := map[string]string{
		container.LabelReplica:   s.ID,
		container.LabelComponent: s.Component,
		container.LabelRelease:   s.Release,
		container.LabelKind:      string(s.Kind),
		container.LabelApp:       a.cfg.App,
		container.LabelStop:      strconv.Itoa(stop),
	}
	optional := map[string]string{
		container.LabelFingerprint:   s.Fingerprint,
		container.LabelRole:          s.Role,
		container.LabelReady:         s.Ready,
		container.LabelSecretVersion: s.SecretVersion,
		container.LabelSecretDigest:  s.SecretDigest,
	}
	if s.Port > 0 {
		optional[container.LabelPort] = strconv.Itoa(s.Port)
	}
	if s.Metrics != nil {
		optional[container.LabelMetrics] = s.Metrics.Endpoint
	}
	if len(s.Backup) > 0 {
		b, _ := json.Marshal(s.Backup)
		optional[container.LabelBackup] = string(b)
	}
	if len(s.ReadyCommand) > 0 {
		b, _ := json.Marshal(s.ReadyCommand)
		optional[container.LabelReadyCommand] = string(b)
	}
	if s.H2C {
		optional[container.LabelH2C] = "true"
	}
	if len(s.SecretFiles) > 0 {
		b, _ := json.Marshal(s.SecretFiles)
		optional[container.LabelSecretFiles] = string(b)
	}
	if s.SecretFollows {
		optional[container.LabelSecretFollows] = "true"
	}
	if s.ConfigPath != "" {
		optional[container.LabelConfigDigest] = s.ConfigDigest
	}
	if s.ConfigFollows {
		optional[container.LabelConfigFollows] = "true"
	}
	for k, v := range optional {
		if v != "" {
			labels[k] = v
		}
	}

	// The environment is the spec's env plus ProtoStrap's variables, which the spec can't
	// override.
	env := maps.Clone(s.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["PROTOSTRAP_APP"] = a.cfg.App
	env["PROTOSTRAP_COMPONENT"] = s.Component
	env["PROTOSTRAP_REPLICA"] = s.ID
	env["PROTOSTRAP_RELEASE"] = s.Release
	env["SECRET_ID"] = a.secretID(s.Component)
	delete(env, "SECRET_PREFIX")
	if s.SecretPrefix != "" {
		env["SECRET_PREFIX"] = s.SecretPrefix
	}
	env["AWS_REGION"] = a.cfg.Region
	delete(env, envCredentialsURI)
	if token != "" {
		env[envCredentialsURI] = a.cfg.CredentialsPath + token
	}
	envList := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		envList = append(envList, k+"="+env[k])
	}

	restart := container.RestartUnlessStopped
	if s.Kind == api.KindTask {
		restart = container.RestartNever
	}
	spec := container.Spec{
		Name: s.ID, Image: s.Image, Cmd: s.Command, Env: envList, Labels: labels,
		Network: a.cfg.Network, Aliases: aliases(s),
		MemoryBytes: s.Request.Memory << 20, CPUShares: max(2, s.Request.CPU*1024/1000), CPUSet: cpuset,
		StopTimeout: time.Duration(stop) * time.Second, Restart: restart,
		LogDriver: "fluentd", LogOptions: a.telemetryLogs(),
	}
	if s.LogGroup != "" {
		spec.LogDriver, spec.LogOptions = "awslogs", a.cloudWatchLogs(s.LogGroup)
	}
	switch s.Kind {
	case api.KindStateful:
		spec.Mounts = []container.Mount{{Source: filepath.Join(a.cfg.DataDir, s.Component), Target: s.Volume}}
	case api.KindEdge:
		spec.Mounts = []container.Mount{{Source: a.edgeFiles(), Target: a.cfg.EdgeFilesMount, ReadOnly: true}}
	case api.KindTelemetry:
		// Its own output stays on data-1: sent to itself, a fault in the pipeline would feed
		// on itself.
		spec.LogDriver, spec.LogOptions = "local", map[string]string{"max-size": "20m", "max-file": "5"}
		spec.Mounts = []container.Mount{
			{Source: filepath.Join(a.cfg.DataDir, s.Component), Target: s.Volume},
			{Source: a.telemetryFiles(), Target: a.cfg.EdgeFilesMount, ReadOnly: true},
			{Source: a.cfg.TelemetrySocketDir, Target: telemetry.SocketDir},
		}
		// Docker on every VM sends replicas' output to data-1's own address, and only there:
		// on every address, a container could reach it.
		spec.Publish = []container.Port{{HostIP: a.cfg.PrivateIP, HostPort: telemetry.ForwardPort, ContainerPort: telemetry.ForwardPort, Protocol: "tcp"}}
	}
	if len(s.SecretFiles) > 0 {
		// The directory, never single files: a file rewritten in place by a rename isn't seen
		// through a mount of the file.
		spec.Mounts = append(spec.Mounts, container.Mount{Source: a.secretDir(s.ID), Target: a.cfg.SecretsMount, ReadOnly: true})
	}
	if s.ConfigPath != "" {
		// The directory, as for secret files, so a swap is seen.
		spec.Mounts = append(spec.Mounts, container.Mount{Source: a.configDir(s.ID), Target: s.ConfigPath, ReadOnly: true})
	}
	return spec
}

func (a *Agent) edgeFiles() string { return filepath.Join(a.cfg.FilesDir, "edge") }

func (a *Agent) telemetryFiles() string { return filepath.Join(a.cfg.FilesDir, telemetry.Name) }

// telemetryLogs are the options of Docker's fluentd driver, which sends a replica's output to
// the telemetry collector on data-1 (docs/telemetry.md#what-is-collected). Each line carries
// the replica's component, replica and release labels. While the collector can't be
// reached, Docker holds up to 10,000 lines and drops the rest, and never blocks the
// replica; `docker logs` keeps its own copy.
func (a *Agent) telemetryLogs() map[string]string {
	return map[string]string{
		"fluentd-address":              "tcp://" + a.cfg.TelemetryAddress,
		"fluentd-async":                "true",
		"fluentd-buffer-limit":         "10000",
		"fluentd-sub-second-precision": "true",
		"tag":                          "{{.Name}}",
		"labels":                       container.LabelComponent + "," + container.LabelReplica + "," + container.LabelRelease,
		"mode":                         "non-blocking",
		"max-buffer-size":              "4m",
	}
}

// cloudWatchLogs are the options of Docker's awslogs driver, which sends a replica's output
// to its component's log group when the application runs no telemetry. Terraform makes the
// group, and the VM's role may write to it. Docker holds up to 4 MB while CloudWatch can't
// be reached, and never blocks the replica; each replica writes a stream of its own name.
func (a *Agent) cloudWatchLogs(group string) map[string]string {
	return map[string]string{
		"awslogs-region":       a.cfg.Region,
		"awslogs-group":        group,
		"awslogs-create-group": "false",
		"tag":                  "{{.Name}}",
		"mode":                 "non-blocking",
		"max-buffer-size":      "4m",
	}
}

// start pulls, creates and starts a replica in the background. Its credentials are served,
// and its secret files written, before it starts, as it reads its keys at once.
func (a *Agent) start(ctx context.Context, s api.ReplicaSpec, cpuset string) {
	token := ""
	if s.Role != "" {
		token = a.newToken()
	}
	spec := a.spec(s, token, cpuset)
	secretID, version := a.secretID(s.Component), s.SecretVersion
	if s.SecretFollows {
		version = a.last.SecretVersions[s.Component]
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o := &op{kind: opStart, component: s.Component, cancel: cancel}
	a.mu.Lock()
	a.ops[s.ID] = o
	a.mu.Unlock()
	a.log.Info("starting a replica", "replica", s.ID, "component", s.Component, "image", s.Image)

	a.wg.Go(func() {
		defer cancel()
		err := a.create(ctx, s, spec, token, secretID, version)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.ops[s.ID] == o {
			delete(a.ops, s.ID)
		}
		switch {
		case ctx.Err() != nil:
			a.log.Info("start cancelled", "replica", s.ID)
		case err != nil:
			a.log.Error("starting a replica", "replica", s.ID, "err", err)
			a.failed[s.ID] = failure{component: s.Component, release: s.Release, kind: s.Kind, reason: err.Error()}
		default:
			a.seen[s.ID] = sighting{at: a.clock.Now()}
		}
	})
}

func (a *Agent) create(ctx context.Context, s api.ReplicaSpec, spec container.Spec, token, secretID, version string) error {
	if len(s.SecretFiles) > 0 {
		written, err := a.prepareSecretFiles(ctx, s, secretID, version)
		if err != nil {
			return err
		}
		spec.Labels[container.LabelSecretVersion] = written
	}
	if s.ConfigPath != "" {
		if err := a.configPathClear(s); err != nil {
			return err
		}
		written, err := a.prepareConfig(ctx, s)
		if err != nil {
			return err
		}
		spec.Labels[container.LabelConfigDigest] = written
	}
	if s.Kind == api.KindEdge {
		if err := os.MkdirAll(a.edgeFiles(), 0o755); err != nil {
			return err
		}
	}
	if s.Kind == api.KindTelemetry {
		// The bind mounts' sources must exist. The socket's directory is for root and the
		// telemetry replica alone, which takes it over for Grafana.
		if err := os.MkdirAll(a.telemetryFiles(), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(a.cfg.TelemetrySocketDir, 0o700); err != nil {
			return err
		}
	}
	present, err := a.rt.HasImage(ctx, s.Image)
	if err != nil {
		return fmt.Errorf("checking the image: %w", err)
	}
	if !present {
		if err := a.rt.Pull(ctx, s.Image); err != nil {
			return fmt.Errorf("pull: %w", err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	id, err := a.rt.Create(ctx, spec)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if token != "" {
		a.creds.Put(credentials.Entry{Token: token, Replica: s.ID, Component: s.Component, Role: s.Role})
	}
	err = ctx.Err()
	if err == nil {
		if err = a.rt.Start(ctx, id); err != nil {
			err = fmt.Errorf("start: %w", err)
		}
	}
	if err != nil {
		// Don't leave a container behind that would never be started.
		a.rt.Remove(context.WithoutCancel(ctx), id)
		a.creds.Remove(s.ID)
	}
	return err
}

// stop stops a replica in the background: SIGTERM, then SIGKILL once its stop time has
// passed, then removes its container. A replica is only stopped once.
func (a *Agent) stop(ctx context.Context, c container.Container, timeout time.Duration) {
	id := c.Replica()
	a.mu.Lock()
	if prev := a.ops[id]; prev != nil {
		if prev.kind == opStop {
			a.mu.Unlock()
			return
		}
		prev.cancel()
	}
	o := &op{kind: opStop, component: c.Label(container.LabelComponent), cancel: func() {}}
	a.ops[id] = o
	a.mu.Unlock()
	a.log.Info("stopping a replica", "replica", id, "timeout", timeout)

	ctx = context.WithoutCancel(ctx)
	a.wg.Go(func() {
		if err := a.rt.Stop(ctx, c.ID, timeout); err != nil {
			a.log.Error("stopping a replica", "replica", id, "err", err)
		}
		if err := a.rt.Remove(ctx, c.ID); err != nil {
			a.log.Error("removing a replica", "replica", id, "err", err)
		}
		a.creds.Remove(id)
		a.removeSecretFiles(id)
		a.removeConfig(id)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.ops[id] == o {
			delete(a.ops, id)
		}
		delete(a.ready, id)
		delete(a.seen, id)
		delete(a.failed, id)
	})
}

// writeFiles writes the assignment's files, such as the edge's routes, and removes any
// it no longer has. A file is only rewritten when it changes, atomically, since Envoy
// reloads its routes when they change.
func (a *Agent) writeFiles(files map[string]string) {
	want := map[string]bool{}
	for key, content := range files {
		if !safeKey(key) {
			a.log.Error("refusing a file outside the files directory", "file", key)
			continue
		}
		p := filepath.Join(a.cfg.FilesDir, filepath.FromSlash(key))
		want[p] = true
		if current, err := os.ReadFile(p); err == nil && string(current) == content {
			continue
		}
		if err := writeAtomic(p, content); err != nil {
			a.log.Error("writing a file", "file", key, "err", err)
			continue
		}
		a.log.Info("file written", "file", key)
	}
	filepath.WalkDir(a.cfg.FilesDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !want[p] {
			if err := os.Remove(p); err == nil {
				a.log.Info("file removed", "file", p)
			}
		}
		return nil
	})
}

func safeKey(key string) bool {
	return key != "" && !strings.HasPrefix(key, "/") && !strings.Contains(key, `\`) &&
		path.Clean(key) == key && key != ".." && !strings.HasPrefix(key, "../")
}

func writeAtomic(p, content string) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// aliases are the names a replica answers to on the overlay: its component's, so the name
// resolves to every replica. A task answers to none, so a migration run in a service's
// image never gets the service's requests.
func aliases(s api.ReplicaSpec) []string {
	if s.Kind == api.KindTask {
		return nil
	}
	return []string{s.Component}
}
