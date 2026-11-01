// Package release is what the deploy workflow hands the scheduler: releases/<commit>.json
// in the deploy bucket, the whole application at one commit (docs/protostrap.md#deploy-workflow).
// A release holds the spec as committed, every component's image and fingerprint, and the
// ProtoStrap version it runs. The package also has the plan the workflow builds from, and
// the scheduler's state.json.
package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/spec"
)

// Release is releases/<commit>.json.
type Release struct {
	Commit     string               `json:"commit"`
	App        string               `json:"app"`
	Version    string               `json:"protostrap"` // It is the ProtoStrap version VMs run.
	Spec       string               `json:"spec"`       // It is protostrap.yaml as committed.
	Components map[string]Component `json:"components"`
	// Edge runs the gateway and the tunnel. It is empty when the spec has no gateway.
	Edge Component `json:"edge,omitzero"`
	// Telemetry is empty when the spec has no telemetry, and in a release made before
	// telemetry, which runs without it.
	Telemetry Component `json:"telemetry,omitzero"`
	Created   time.Time `json:"created"`
	// Config holds the components' config files, each set once, by its digest.
	Config map[string]configfiles.Files `json:"config,omitempty"`
}

// Component is one component's image and fingerprint in a release.
type Component struct {
	Image       string `json:"image"`
	Fingerprint string `json:"fingerprint"`
	// Config is the digest of the component's config files, if it has any.
	Config string `json:"config,omitempty"`
}

var (
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)
)

// ValidCommit reports whether c can name a release.
func ValidCommit(c string) bool { return commitPattern.MatchString(c) }

// Parse reads and checks a release.
func Parse(b []byte) (Release, error) {
	var r Release
	if err := json.Unmarshal(b, &r); err != nil {
		return Release{}, fmt.Errorf("release: %w", err)
	}
	var errs []error
	if !ValidCommit(r.Commit) {
		errs = append(errs, fmt.Errorf("commit %q", r.Commit))
	}
	if r.App == "" {
		errs = append(errs, errors.New("no app"))
	}
	if !versionPattern.MatchString(r.Version) {
		errs = append(errs, fmt.Errorf("protostrap version %q", r.Version))
	}
	if len(errs) == 0 {
		s, err := r.ParseSpec()
		if err != nil {
			errs = append(errs, err)
		} else {
			for name, sc := range s.Components {
				c, ok := r.Components[name]
				if !ok || c.Image == "" || c.Fingerprint == "" {
					errs = append(errs, fmt.Errorf("no image or fingerprint for %s", name))
					continue
				}
				switch files, ok := r.Config[c.Config]; {
				case sc.Config == nil && c.Config != "":
					errs = append(errs, fmt.Errorf("%s has config files, but its spec has no config", name))
				case sc.Config == nil:
				case !ok:
					errs = append(errs, fmt.Errorf("no config files %q for %s", c.Config, name))
				case configfiles.Digest(files) != c.Config:
					errs = append(errs, fmt.Errorf("%s's config files don't match their digest %s", name, c.Config))
				default:
					if err := files.Validate(); err != nil {
						errs = append(errs, fmt.Errorf("%s's config files: %w", name, err))
					}
				}
			}
			if s.Gateway != nil && r.Edge.Image == "" {
				errs = append(errs, errors.New("no edge image"))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Release{}, fmt.Errorf("release %s: %w", r.Commit, err)
	}
	return r, nil
}

// ParseSpec parses the release's spec.
func (r Release) ParseSpec() (*spec.Spec, error) {
	s, err := spec.Parse([]byte(r.Spec))
	if err != nil {
		return nil, fmt.Errorf("the release's spec: %w", err)
	}
	return s, nil
}

// Plan is what a commit needs built and copied before it can be released.
type Plan struct {
	App        string     `json:"app"`
	ProtoStrap string     `json:"protostrap"` // It is the spec's version.
	Version    string     `json:"version"`    // It is the version the release runs.
	InfraHash  string     `json:"infra_hash"`
	Builds     []Build    `json:"builds"`
	Stateful   []Stateful `json:"stateful"`
	// Edge and Telemetry are the images of ProtoStrap's own components the release runs, or
	// empty when the spec doesn't run them.
	Edge      string `json:"edge"`
	Telemetry string `json:"telemetry"`
}

// Build is one image to build, with paths the workflow can use from where it runs: a
// Dockerfile and its context, or a .NET project, which the .NET SDK publishes as an image
// with the SDK its global.json pins.
type Build struct {
	Component  string   `json:"component"`
	Tag        string   `json:"tag"`
	Image      string   `json:"image"`
	Dockerfile string   `json:"dockerfile"`
	Context    string   `json:"context"`
	Project    string   `json:"project,omitempty"`
	GlobalJSON string   `json:"global_json,omitempty"`
	Sources    []string `json:"sources"`
}

// Stateful is an upstream image to copy into ECR.
type Stateful struct {
	Component string `json:"component"`
	Upstream  string `json:"upstream"`
	Image     string `json:"image"`
}

// MakePlan reads the spec at specPath and works out every built component's tag from its
// tracked sources. Paths in the spec are relative to its directory. version overrides the
// spec's ProtoStrap version when set.
func MakePlan(specPath, registry, version string) (Plan, error) {
	s, err := spec.ReadFile(specPath)
	if err != nil {
		return Plan{}, err
	}
	if problems := s.Check(); len(problems) > 0 {
		return Plan{}, fmt.Errorf("%s:\n%w", specPath, spec.Problems(problems))
	}
	if version == "" {
		version = s.ProtoStrap
	}
	dir := filepath.Dir(specPath)
	p := Plan{App: s.App, ProtoStrap: s.ProtoStrap, Version: version, InfraHash: s.InfraHash(),
		Builds: []Build{}, Stateful: []Stateful{}}
	if s.Gateway != nil {
		p.Edge = image(registry, "edge", version)
	}
	if s.Telemetry != nil {
		p.Telemetry = image(registry, "telemetry", version)
	}
	for _, c := range s.Sorted() {
		switch c.Kind {
		case api.KindStateful:
			p.Stateful = append(p.Stateful, Stateful{Component: c.Name, Upstream: c.Image, Image: image(registry, c.Name, upstreamTag(c.Image))})
		default:
			// The build directory holds the Dockerfile and the project is the project file, so
			// whichever the component has must be tracked in the repository.
			from := c.Build
			if c.Project != "" {
				from = c.Project
			}
			if _, err := SourcesHash(dir, []string{from}); err != nil {
				return Plan{}, fmt.Errorf("%s: %w", c.Name, err)
			}
			sources := c.TagSources()
			var sdk string
			if c.Project != "" {
				// The SDK is part of how a project's image is built, so the global.json that
				// pins it is among the tag's sources, whether or not the spec lists it.
				if sdk, err = globalJSON(dir, c.Project); err != nil {
					return Plan{}, fmt.Errorf("%s: %w", c.Name, err)
				}
				sources = append(sources, sdk)
				slices.Sort(sources)
				sources = slices.Compact(sources)
			}
			hash, err := SourcesHash(dir, sources)
			if err != nil {
				return Plan{}, fmt.Errorf("%s: %w", c.Name, err)
			}
			tag := spec.Tag(hash, c.Recipe())
			b := Build{
				Component: c.Name,
				Tag:       tag,
				Image:     image(registry, c.Name, tag),
				Sources:   sources,
			}
			if c.Project != "" {
				b.Project = filepath.Join(dir, c.Project)
				b.GlobalJSON = filepath.Join(dir, sdk)
			} else {
				b.Dockerfile = filepath.Join(dir, c.Build, "Dockerfile")
				b.Context = filepath.Join(dir, c.Context)
			}
			p.Builds = append(p.Builds, b)
		}
	}
	return p, nil
}

// globalJSON finds the global.json that pins the .NET SDK a project builds with, relative to
// dir, the spec's directory. ProtoStrap never picks an SDK, so a new .NET needs no new
// ProtoStrap. It is the nearest global.json at or above the project's directory, which the
// SDK reads when the deploy workflow publishes from there, but no higher than dir, since the
// tag can only hash what is inside it. It must be tracked, since the workflow has only what
// is committed, and must set sdk.version.
func globalJSON(dir, project string) (string, error) {
	start := path.Dir(project)
	for d := start; ; d = path.Dir(d) {
		rel := path.Join(d, "global.json")
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			if d == "." {
				return "", fmt.Errorf("no global.json at or above %s pins the .NET SDK it builds with: add one that sets sdk.version", start)
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := SourcesHash(dir, []string{rel}); err != nil {
			return "", fmt.Errorf("%s pins the .NET SDK it builds with, but isn't tracked in git", rel)
		}
		var pin struct {
			SDK struct {
				Version string `json:"version"`
			} `json:"sdk"`
		}
		if err := json.Unmarshal(withoutComments(bytes.TrimPrefix(b, []byte("\ufeff"))), &pin); err != nil {
			return "", fmt.Errorf("%s isn't JSON the .NET SDK can read: %v", rel, err)
		}
		if pin.SDK.Version == "" {
			return "", fmt.Errorf("%s doesn't pin the .NET SDK it builds with: set sdk.version", rel)
		}
		return rel, nil
	}
}

// withoutComments blanks out the // and /* */ comments the .NET SDK allows in global.json,
// which encoding/json doesn't, leaving strings as they are.
func withoutComments(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '"':
			j := i + 1
			for ; j < len(b) && b[j] != '"'; j++ {
				if b[j] == '\\' {
					j++
				}
			}
			out = append(out, b[i:min(j+1, len(b))]...)
			i = j
		case bytes.HasPrefix(b[i:], []byte("//")):
			for i < len(b) && b[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case bytes.HasPrefix(b[i:], []byte("/*")):
			end := bytes.Index(b[i+2:], []byte("*/"))
			if end < 0 {
				// An unclosed comment is left for encoding/json to refuse.
				return append(out, b[i:]...)
			}
			out = append(out, ' ')
			i += 2 + end + 1
		default:
			out = append(out, b[i])
		}
	}
	return out
}

func image(registry, component, tag string) string {
	return strings.TrimSuffix(registry, "/") + "/protostrap/" + component + ":" + tag
}

// upstreamTag is the tag an upstream image keeps in ECR: its own, or for a digest,
// sha256- and the digest's first 32 hex digits, since ECR tags can't hold a colon.
func upstreamTag(img string) string {
	if _, digest, ok := strings.Cut(img, "@sha256:"); ok {
		return "sha256-" + digest[:min(32, len(digest))]
	}
	name := path.Base(img)
	_, tag, _ := strings.Cut(name, ":")
	return tag
}

// SourcesHash is git's hash of the tracked files under paths, relative to dir: `git
// ls-files -s -- <paths> | git hash-object --stdin`. It lists each file's blob hash, so it
// costs milliseconds, and untracked files such as bin/ and node_modules/ never count.
func SourcesHash(dir string, paths []string) (string, error) {
	ls := exec.Command("git", append([]string{"ls-files", "-s", "--"}, paths...)...)
	ls.Dir = dir
	var stderr bytes.Buffer
	ls.Stderr = &stderr
	listing, err := ls.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-files: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(bytes.TrimSpace(listing)) == 0 {
		return "", fmt.Errorf("no tracked files in %s", strings.Join(paths, ", "))
	}
	hash := exec.Command("git", "hash-object", "--stdin")
	hash.Dir = dir
	hash.Stdin = bytes.NewReader(listing)
	out, err := hash.Output()
	if err != nil {
		return "", fmt.Errorf("git hash-object: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Make builds the release of the spec at specPath for a commit.
func Make(specPath, commit, registry, version string) (Release, error) {
	if !ValidCommit(commit) {
		return Release{}, fmt.Errorf("commit %q isn't a git commit", commit)
	}
	p, err := MakePlan(specPath, registry, version)
	if err != nil {
		return Release{}, err
	}
	src, err := os.ReadFile(specPath)
	if err != nil {
		return Release{}, err
	}
	s, err := spec.Parse(src)
	if err != nil {
		return Release{}, err
	}
	r := Release{Commit: commit, App: s.App, Version: p.Version, Spec: string(src),
		Components: map[string]Component{}, Created: time.Now().UTC().Truncate(time.Second)}
	images := map[string]string{}
	for _, b := range p.Builds {
		images[b.Component] = b.Image
	}
	for _, st := range p.Stateful {
		images[st.Component] = st.Image
	}
	dir := filepath.Dir(specPath)
	for _, c := range s.Sorted() {
		digest := ""
		if c.Config != nil {
			files, err := ReadConfig(dir, c.Config.From)
			if err != nil {
				return Release{}, fmt.Errorf("%s's config: %w", c.Name, err)
			}
			digest = configfiles.Digest(files)
			if r.Config == nil {
				r.Config = map[string]configfiles.Files{}
			}
			r.Config[digest] = files
		}
		r.Components[c.Name] = Component{Image: images[c.Name], Fingerprint: c.Fingerprint(images[c.Name], digest), Config: digest}
	}
	if g := s.Gateway; g != nil {
		r.Edge = Component{Image: p.Edge, Fingerprint: EdgeFingerprint(p.Edge, g.Request, s.Tunnel != nil)}
	}
	if t := s.Telemetry; t != nil {
		r.Telemetry = Component{Image: p.Telemetry, Fingerprint: TelemetryFingerprint(p.Telemetry, *t)}
	}
	return r, nil
}

// ReadConfig reads the files git tracks in from, a directory relative to dir, as the index
// has them: a release is a commit, so an edit that isn't added doesn't count. Each file is
// named by its path under from.
func ReadConfig(dir, from string) (configfiles.Files, error) {
	ls := exec.Command("git", "ls-files", "-s", "-z", "--", from)
	ls.Dir = dir
	var stderr bytes.Buffer
	ls.Stderr = &stderr
	listing, err := ls.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	prefix := strings.TrimSuffix(path.Clean(from), "/") + "/"
	type entry struct{ name, blob string }
	var entries []entry
	for _, line := range strings.Split(string(listing), "\x00") {
		if line == "" {
			continue
		}
		// Each line is "<mode> <blob> <stage>\t<path>".
		meta, p, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("git ls-files printed %q", line)
		}
		if !strings.HasPrefix(p, prefix) {
			return nil, fmt.Errorf("%s must be a directory", from)
		}
		name := strings.TrimPrefix(p, prefix)
		if fields[0] != "100644" && fields[0] != "100755" {
			return nil, fmt.Errorf("%s isn't a regular file (git mode %s): config files are plain files, never links or submodules", p, fields[0])
		}
		entries = append(entries, entry{name, fields[1]})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no tracked files in %s", from)
	}
	var in bytes.Buffer
	for _, e := range entries {
		in.WriteString(e.blob + "\n")
	}
	cat := exec.Command("git", "cat-file", "--batch")
	cat.Dir = dir
	cat.Stdin = &in
	cat.Stderr = &stderr
	out, err := cat.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	files := configfiles.Files{}
	total := 0
	for _, e := range entries {
		// Each object is "<blob> blob <size>\n<content>\n".
		header, rest, ok := bytes.Cut(out, []byte("\n"))
		var blob, kind string
		var size int
		if _, err := fmt.Sscanf(string(header), "%s %s %d", &blob, &kind, &size); !ok || err != nil || blob != e.blob || kind != "blob" || len(rest) < size+1 {
			return nil, fmt.Errorf("git cat-file printed %q for %s", header, e.name)
		}
		total += size
		if total > configfiles.MaxBytes {
			return nil, fmt.Errorf("%s holds more than %d MiB: config is small, so put larger files in the image", from, configfiles.MaxBytes>>20)
		}
		files[e.name] = rest[:size:size]
		out = rest[size+1:]
	}
	if err := files.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", from, err)
	}
	return files, nil
}

// EdgeFingerprint is the edge's fingerprint: its image, the gateway's request, and whether
// it runs the tunnel. An edge with the tunnel hashes as it did before the tunnel was
// optional, so its replicas aren't replaced for an upgrade alone.
func EdgeFingerprint(image string, request spec.Resources, tunnel bool) string {
	b, _ := json.Marshal(struct {
		Image    string         `json:"image"`
		Request  spec.Resources `json:"request"`
		NoTunnel bool           `json:"no_tunnel,omitempty"`
	}{image, request, !tunnel})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// TelemetryFingerprint is the telemetry replica's fingerprint: its image, its request and
// its retention, which its programs read when they start. Its volume's size isn't part of
// it, since Terraform grows the volume under it.
func TelemetryFingerprint(image string, t spec.Telemetry) string {
	days := func(d spec.Duration) int { return int(d.Hours() / 24) }
	b, _ := json.Marshal(struct {
		Image     string         `json:"image"`
		Request   spec.Resources `json:"request"`
		Retention [3]int         `json:"retention"`
	}{image, t.Request, [3]int{days(t.Retention.Logs), days(t.Retention.Traces), days(t.Retention.Metrics)}})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// State is state.json in the deploy bucket: what the scheduler must not lose if data-1 is
// lost. S3 is regional, so the file survives the loss of data-1.
type State struct {
	Target   string         `json:"target"`    // It is the release being rolled out, or running.
	LastGood string         `json:"last_good"` // It is the last release that finished.
	Desired  map[string]int `json:"desired"`   // It is each component's last desired replica count.
	Updated  time.Time      `json:"updated"`

	// Secrets are each component's keys in the environment's secret, by component: the
	// version its replicas should have and a keyed hash of its keys, never their values.
	Secrets map[string]SecretState `json:"secrets,omitempty"`
	// SecretVersion is the secret's current version as the scheduler last saw it, and
	// SecretChanged when it first did.
	SecretVersion string    `json:"secret_version,omitempty"`
	SecretChanged time.Time `json:"secret_changed,omitempty"`

	// Configured is each component's configure task, by component: what it last applied.
	Configured map[string]ConfigureState `json:"configured,omitempty"`

	// Hosts are the account's hostnames as the scheduler last read them from hosts.json,
	// so a restart while it can't be read changes nothing.
	Hosts map[string]string `json:"hosts,omitempty"`
}

// ConfigureState is what a component's configure task has applied.
type ConfigureState struct {
	// Applied names what the last run that succeeded applied: its task, its keys and the
	// component, hashed together.
	Applied string `json:"applied,omitempty"`
	// Dirty is set while a run that started since then hasn't succeeded, so whatever it
	// changed is applied over again.
	Dirty bool `json:"dirty,omitempty"`
}

// SecretState is a component's keys in the environment's secret.
type SecretState struct {
	Version  string `json:"version"`            // It is the version of the component's last change.
	Read     string `json:"read,omitempty"`     // It is the newest version with the same keys, which new replicas read.
	Digest   string `json:"digest"`             // It is the scheduler's keyed hash of the keys.
	Baseline bool   `json:"baseline,omitempty"` // No change has been applied since the scheduler first saw the keys.
}
