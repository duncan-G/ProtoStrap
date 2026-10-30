package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"slices"
)

func hash16(v any) string {
	b, err := json.Marshal(v) // json.Marshal sorts map keys, so equal values hash equally.
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Fingerprint is a hash of everything that shapes a component's replicas: its image, its
// config files, named by their digest, and the fields in a replica's assignment. Replicas
// are replaced only when it changes, so a new scale setting, route or permission never
// restarts anything (docs/protostrap-aspire.md, "Component fingerprints"). A follower's
// config files are left out: they are swapped in under its replicas.
//
// Fields added later are left out when unset, so upgrading ProtoStrap changes no
// fingerprint.
func (c *Component) Fingerprint(image, config string) string {
	type change struct {
		Reload  bool     `json:"reload,omitempty"`
		Command []string `json:"command,omitempty"`
	}
	type files struct {
		Path   string  `json:"path"`
		Digest string  `json:"digest,omitempty"`
		Follow *change `json:"follow,omitempty"`
	}
	type shape struct {
		Image          string            `json:"image"`
		Command        []string          `json:"command,omitempty"`
		Request        Resources         `json:"request"`
		Port           int               `json:"port,omitempty"`
		Ready          string            `json:"ready,omitempty"`
		ReadyCommand   []string          `json:"ready_command,omitempty"`
		Stop           int64             `json:"stop"`
		Env            map[string]string `json:"env,omitempty"`
		Secrets        []string          `json:"secrets,omitempty"`
		SecretPrefix   string            `json:"secret_prefix,omitempty"`
		SecretFiles    bool              `json:"secret_files,omitempty"`
		OnSecretChange *change           `json:"on_secret_change,omitempty"`
		Metrics        string            `json:"metrics,omitempty"`
		Volume         string            `json:"volume,omitempty"`
		Backup         []string          `json:"backup,omitempty"`
		Config         *files            `json:"config,omitempty"`
	}
	s := shape{Image: image, Command: c.Command, Request: c.Request, Port: c.Port, Ready: c.Ready, ReadyCommand: c.ReadyCommand,
		Stop: int64(c.Stop.Seconds()), Env: c.Env, Secrets: c.Secrets, SecretPrefix: c.SecretPrefix,
		SecretFiles: c.SecretFiles, Metrics: c.Metrics, Backup: c.Backup}
	if c.Follows() {
		// Restart, said or not, is the default.
		s.OnSecretChange = &change{Reload: c.OnSecretChange.Mode == SecretReload, Command: c.OnSecretChange.Command}
	}
	if c.Volume != nil {
		s.Volume = c.Volume.Path
	}
	if c.Config != nil {
		s.Config = &files{Path: c.Config.Path, Digest: config}
		if c.ConfigFollows() {
			s.Config.Digest = ""
			s.Config.Follow = &change{Reload: c.OnConfigChange.Mode == SecretReload, Command: c.OnConfigChange.Command}
		}
	}
	return hash16(s)
}

// InfraProjection is the part of the spec Terraform reads. The infra workflow only needs to
// run when it changes (docs/protostrap-aspire.md, "Infrastructure projection").
type InfraProjection struct {
	Hosts      map[string]string         `json:"hosts"`
	Pools      map[string]poolProjection `json:"pools"`
	Queues     map[string]Queue          `json:"queues"`
	Buckets    map[string]Bucket         `json:"buckets,omitempty"`
	Email      *emailProjection          `json:"email,omitempty"`
	Components map[string]compProjection `json:"components"`
	Telemetry  telemetryProjection       `json:"telemetry"`
}

// telemetryProjection is what Terraform makes for telemetry: its volume, and its bucket's
// expiry, from the longer of logs and traces.
type telemetryProjection struct {
	Volume Size     `json:"volume"`
	Logs   Duration `json:"logs"`
	Traces Duration `json:"traces"`
}

type poolProjection struct {
	VM    []string `json:"vm"`
	Min   int      `json:"min,omitempty"`
	Max   int      `json:"max,omitempty"`
	Count int      `json:"count,omitempty"`
}

type compProjection struct {
	Kind string   `json:"kind"`
	Uses []string `json:"uses,omitempty"`
	// Queues are the component's queue signals, which its role may receive from.
	Queues []string `json:"queues,omitempty"`
	VM     string   `json:"vm,omitempty"`
	Volume *Volume  `json:"volume,omitempty"`
}

// Projection returns the infra projection.
func (s *Spec) Projection() InfraProjection {
	p := InfraProjection{
		Hosts:      map[string]string(s.Hosts),
		Pools:      map[string]poolProjection{},
		Queues:     map[string]Queue{},
		Components: map[string]compProjection{},
	}
	p.Pools["ingress"] = poolProjection{VM: []string{s.Pools.Ingress.VM}}
	p.Pools["general"] = poolProjection{VM: s.Pools.General.VM, Min: s.Pools.General.Min, Max: s.Pools.General.Max}
	if c := s.Pools.Compute; c != nil {
		p.Pools["compute"] = poolProjection{VM: c.VM, Min: c.Min, Max: c.Max}
	}
	p.Pools["data"] = poolProjection{VM: []string{s.Pools.Data.VM}, Count: s.Pools.Data.Count}
	for name, q := range s.Queues {
		p.Queues[name] = *q
	}
	if len(s.Buckets) > 0 {
		p.Buckets = map[string]Bucket{}
		for name, b := range s.Buckets {
			p.Buckets[name] = *b
		}
	}
	for _, c := range s.Components {
		cp := compProjection{Kind: string(c.Kind), VM: c.VM, Volume: c.Volume}
		cp.Uses = slices.Sorted(slices.Values(c.Uses))
		cp.Queues = slices.Sorted(slices.Values(c.QueueSignals()))
		p.Components[c.Name] = cp
	}
	t := s.Telemetry
	p.Telemetry = telemetryProjection{Volume: t.Volume, Logs: t.Retention.Logs, Traces: t.Retention.Traces}
	p.Email = s.Email.projection()
	return p
}

// InfraHash is a hash of the infra projection.
func (s *Spec) InfraHash() string { return hash16(s.Projection()) }

// TagSources are the paths hashed into a built component's image tag: its build directory,
// or its project's directory, and its sources.
func (c *Component) TagSources() []string {
	dir := c.Build
	if c.Project != "" {
		dir = path.Dir(c.Project)
	}
	srcs := append([]string{dir}, c.Sources...)
	slices.Sort(srcs)
	return slices.Compact(srcs)
}

// Recipe is how an image is built, which goes into its tag beside its sources: a change to
// either is a new image.
func (c *Component) Recipe() string {
	recipe := map[string]string{
		"builder":  "docker",
		"build":    c.Build,
		"context":  c.Context,
		"platform": Platform,
	}
	if c.Project != "" {
		// A project is built with the .NET SDK's container publish. Its SDK is pinned by the
		// global.json plan finds and adds to its sources, and its default base image by the
		// ContainerBaseImage among them.
		recipe = map[string]string{"builder": "dotnet", "project": c.Project, "platform": Platform}
	}
	b, _ := json.Marshal(recipe)
	return string(b)
}

// Tag is an image's tag: "c-" and 16 hex digits of the hash of its sources and recipe.
// sourcesHash is git's hash of the sources' tracked files (git ls-files -s | git
// hash-object --stdin).
func Tag(sourcesHash, recipe string) string {
	sum := sha256.Sum256([]byte(sourcesHash + "\n" + recipe))
	return "c-" + hex.EncodeToString(sum[:8])
}
