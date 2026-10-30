package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duncan-G/protostrap/internal/configfiles"
)

// configRepo is repo with config files: PostgreSQL's from config/postgres, which holds a
// nested file and an untracked one, and the cache's from the same directory.
func configRepo(t *testing.T) (root, specPath string) {
	t.Helper()
	root = repo(t)
	specPath = filepath.Join(root, "app", "protostrap.yaml")
	src := strings.Replace(testSpec, "    volume: { path: /var/lib/postgresql/data, size: 10Gi }\n",
		"    volume: { path: /var/lib/postgresql/data, size: 10Gi }\n    config: { from: config/postgres, path: /etc/postgresql/conf }\n    on_config_change: reload\n", 1)
	write(t, root, map[string]string{
		"app/protostrap.yaml":                   src,
		"app/config/postgres/pg_hba.conf":       "local all all trust\nhost all all all scram-sha-256\n",
		"app/config/postgres/conf.d/extra.conf": "max_connections = 200\n",
		"app/config/postgres/scratch.tmp":       "untracked\n",
	})
	git(t, root, "add", "app/protostrap.yaml", "app/config/postgres/pg_hba.conf", "app/config/postgres/conf.d")
	git(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "config")
	return root, specPath
}

const testCommit = "9f2c1e7aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAReleaseCarriesItsConfigFiles(t *testing.T) {
	_, specPath := configRepo(t)
	r, err := Make(specPath, testCommit, "123.dkr.ecr.us-east-1.amazonaws.com", "1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	want := configfiles.Files{
		"pg_hba.conf":       []byte("local all all trust\nhost all all all scram-sha-256\n"),
		"conf.d/extra.conf": []byte("max_connections = 200\n"),
	}
	pg := r.Components["postgres"]
	if pg.Config != configfiles.Digest(want) {
		t.Fatalf("postgres's config %q, want the digest of its tracked files %q", pg.Config, configfiles.Digest(want))
	}
	files := r.Config[pg.Config]
	if len(files) != 2 || string(files["pg_hba.conf"]) != string(want["pg_hba.conf"]) || string(files["conf.d/extra.conf"]) != string(want["conf.d/extra.conf"]) {
		t.Errorf("files %v: only the tracked ones, by their path under from", files)
	}
	s, err := r.ParseSpec()
	if err != nil {
		t.Fatal(err)
	}
	if pg.Fingerprint != s.Components["postgres"].Fingerprint(pg.Image, pg.Config) {
		t.Errorf("postgres's fingerprint isn't computed with its config's digest")
	}
	if api := r.Components["api"]; api.Config != "" {
		t.Errorf("api has no config, but got %q", api.Config)
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(back.Config[pg.Config]["conf.d/extra.conf"]) != "max_connections = 200\n" {
		t.Errorf("config files don't survive the round trip: %v", back.Config)
	}
}

func TestComponentsWithTheSameFilesShareThem(t *testing.T) {
	root, specPath := configRepo(t)
	src, _ := os.ReadFile(specPath)
	write(t, root, map[string]string{"app/protostrap.yaml": strings.Replace(string(src), "    volume: { path: /data, size: 5Gi }\n",
		"    volume: { path: /data, size: 5Gi }\n    config: { from: config/postgres, path: /etc/conf }\n", 1)})
	r, err := Make(specPath, testCommit, "r", "1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if r.Components["cache"].Config != r.Components["postgres"].Config || len(r.Config) != 1 {
		t.Errorf("config %v: the same files are kept once", r.Config)
	}
}

func TestConfigIsWhatGitTracks(t *testing.T) {
	root, specPath := configRepo(t)
	// An edit that isn't added doesn't count: a release is a commit.
	write(t, root, map[string]string{"app/config/postgres/pg_hba.conf": "edited\n"})
	r, err := Make(specPath, testCommit, "r", "1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(r.Config[r.Components["postgres"].Config]["pg_hba.conf"]); got == "edited\n" {
		t.Errorf("pg_hba.conf is %q: the release took a file git doesn't have", got)
	}
}

func TestConfigThatCantBeReleased(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root, specPath string)
		want  string
	}{
		{"no tracked files", func(t *testing.T, root, specPath string) {
			src, _ := os.ReadFile(specPath)
			write(t, root, map[string]string{"app/protostrap.yaml": strings.Replace(string(src), "from: config/postgres", "from: config/nothing", 1)})
		}, "config/nothing"},
		{"a file, not a directory", func(t *testing.T, root, specPath string) {
			src, _ := os.ReadFile(specPath)
			write(t, root, map[string]string{"app/protostrap.yaml": strings.Replace(string(src), "from: config/postgres", "from: config/postgres/pg_hba.conf", 1)})
		}, "directory"},
		{"a symlink", func(t *testing.T, root, specPath string) {
			if err := os.Symlink("pg_hba.conf", filepath.Join(root, "app/config/postgres/link.conf")); err != nil {
				t.Skip(err)
			}
			git(t, root, "add", "app/config/postgres/link.conf")
		}, "link.conf"},
		{"too large", func(t *testing.T, root, specPath string) {
			write(t, root, map[string]string{"app/config/postgres/big.bin": strings.Repeat("x", configfiles.MaxBytes+1)})
			git(t, root, "add", "app/config/postgres/big.bin")
		}, "MiB"},
		{"a name the agent keeps for itself", func(t *testing.T, root, specPath string) {
			write(t, root, map[string]string{"app/config/postgres/..data": "x"})
			git(t, root, "add", "app/config/postgres/..data")
		}, "..data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, specPath := configRepo(t)
			tt.setup(t, root, specPath)
			_, err := Make(specPath, testCommit, "r", "1.4.0")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err %v, want one naming %q", err, tt.want)
			}
		})
	}
}

func TestParseRejectsBrokenConfig(t *testing.T) {
	_, specPath := configRepo(t)
	good, err := Make(specPath, testCommit, "r", "1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	digest := good.Components["postgres"].Config
	for name, breakIt := range map[string]func(r *Release){
		"no digest for a component with config": func(r *Release) {
			c := r.Components["postgres"]
			c.Config = ""
			r.Components["postgres"] = c
		},
		"a digest with no files": func(r *Release) { delete(r.Config, digest) },
		"files that don't match their digest": func(r *Release) {
			r.Config[digest] = configfiles.Files{"pg_hba.conf": []byte("changed\n")}
		},
		"config for a component with none": func(r *Release) {
			c := r.Components["api"]
			c.Config = digest
			r.Components["api"] = c
		},
		"a path the agent can't write": func(r *Release) {
			files := configfiles.Files{"../escape": []byte("x")}
			d := configfiles.Digest(files)
			r.Config[d] = files
			c := r.Components["postgres"]
			c.Config = d
			r.Components["postgres"] = c
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(good)
			var r Release
			json.Unmarshal(b, &r)
			breakIt(&r)
			b, _ = json.Marshal(r)
			if _, err := Parse(b); err == nil {
				t.Errorf("accepted")
			}
		})
	}
}

func TestStateKeepsConfigureRuns(t *testing.T) {
	s := State{Target: "abc1234", Configured: map[string]ConfigureState{"idp": {Applied: "k1", Dirty: true}}}
	b, _ := json.Marshal(s)
	var back State
	if err := json.Unmarshal(b, &back); err != nil || back.Configured["idp"] != (ConfigureState{Applied: "k1", Dirty: true}) {
		t.Errorf("state: %+v %v", back, err)
	}
}
