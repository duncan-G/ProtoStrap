package spec

import (
	"slices"
	"strings"
	"testing"
)

// configExample is the docs example with config files and a configure task: PostgreSQL
// takes its pg_hba.conf as config and reloads it with a command, idp is a service
// configured through its API by the idp-config task, whose files are config too.
var configExample = func() string {
	s := docsExample
	s = replace("    backup: [pg_dumpall, -U, postgres]\n",
		"    backup: [pg_dumpall, -U, postgres]\n    config: { from: config/postgres, path: /etc/postgresql/conf }\n    on_config_change: [psql, -U, postgres, -c, \"SELECT pg_reload_conf()\"]\n")(s)
	s = insertAfter("components:\n", `  idp:
    kind: service
    build: services/idp
    pool: general
    request: { cpu: 500m, memory: 1Gi }
    port: 8080
    ready: /health/ready
    depends_on: [postgres]
    configure: idp-config
    scale:
      min: 1
      max: 2
      demand: [{ cpu: 70% }]

  idp-config:
    kind: task
    build: services/idp-config
    pool: general
    request: { cpu: 250m, memory: 512Mi }
    depends_on: [idp]
    config: { from: config/idp, path: /config }

`)(s)
	return s
}()

func TestConfigParses(t *testing.T) {
	s := mustParse(t, configExample)
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("the config example has problems:\n%v", Problems(problems))
	}
	pg := s.Components["postgres"]
	if pg.Config == nil || pg.Config.From != "config/postgres" || pg.Config.Path != "/etc/postgresql/conf" {
		t.Errorf("postgres config %+v", pg.Config)
	}
	if pg.OnConfigChange == nil || !slices.Equal(pg.OnConfigChange.Command, []string{"psql", "-U", "postgres", "-c", "SELECT pg_reload_conf()"}) {
		t.Errorf("postgres on_config_change %+v", pg.OnConfigChange)
	}
	if !pg.ConfigFollows() {
		t.Errorf("a command takes new config without replacing the replica")
	}
	idp := s.Components["idp"]
	if idp.Configure != "idp-config" || idp.Config != nil || idp.ConfigFollows() {
		t.Errorf("idp: configure %q config %+v", idp.Configure, idp.Config)
	}
	task := s.Components["idp-config"]
	if task.Config == nil || task.Config.From != "config/idp" || task.Config.Path != "/config" || task.ConfigFollows() {
		t.Errorf("idp-config config %+v", task.Config)
	}
	if got := s.ConfiguredBy("idp-config"); got == nil || got.Name != "idp" {
		t.Errorf("ConfiguredBy(idp-config) = %v, want idp", got)
	}
	if got := s.ConfiguredBy("api"); got != nil {
		t.Errorf("ConfiguredBy(api) = %v: api configures nothing", got.Name)
	}

	for _, tt := range []struct {
		value   string
		follows bool
	}{
		{"restart", false},
		{"reload", true},
		{"[nginx, -s, reload]", true},
	} {
		c := mustParse(t, strings.Replace(configExample, `[psql, -U, postgres, -c, "SELECT pg_reload_conf()"]`, tt.value, 1)).Components["postgres"]
		if c.ConfigFollows() != tt.follows {
			t.Errorf("%s: follows %v, want %v", tt.value, c.ConfigFollows(), tt.follows)
		}
	}
	// Config without on_config_change restarts, as a new image would.
	c := mustParse(t, strings.Replace(configExample, "    on_config_change: [psql, -U, postgres, -c, \"SELECT pg_reload_conf()\"]\n", "", 1)).Components["postgres"]
	if c.OnConfigChange != nil || c.ConfigFollows() {
		t.Errorf("left out: %+v, follows %v", c.OnConfigChange, c.ConfigFollows())
	}
}

func TestConfigChecks(t *testing.T) {
	pgConfig := "    config: { from: config/postgres, path: /etc/postgresql/conf }\n"
	pgHook := "    on_config_change: [psql, -U, postgres, -c, \"SELECT pg_reload_conf()\"]\n"
	tests := []struct {
		name string
		edit func(string) string
		path string
		want string
	}{
		{"config with no from", replace(pgConfig, "    config: { path: /etc/postgresql/conf }\n"), "components.postgres.config.from", "required"},
		{"config from outside the repository", replace("from: config/postgres", "from: ../config"), "components.postgres.config.from", "inside the repository"},
		{"config from an absolute path", replace("from: config/postgres", "from: /etc/postgresql"), "components.postgres.config.from", "inside the repository"},
		{"config from a pathspec", replace("from: config/postgres", `from: ":(glob)config/**"`), "components.postgres.config.from", "directory"},
		{"config from the repository's root", replace("from: config/postgres", "from: ."), "components.postgres.config.from", "directory"},
		{"config with no path", replace(pgConfig, "    config: { from: config/postgres }\n"), "components.postgres.config.path", "required"},
		{"relative config path", replace("path: /etc/postgresql/conf", "path: etc/postgresql"), "components.postgres.config.path", "absolute"},
		{"config at the root", replace("path: /etc/postgresql/conf", "path: /"), "components.postgres.config.path", "root"},
		{"untidy config path", replace("path: /etc/postgresql/conf", "path: /etc//postgresql/"), "components.postgres.config.path", "/etc/postgresql"},
		{"config over the secret files", replace("path: /etc/postgresql/conf", "path: /run/secrets"), "components.postgres.config.path", "secret files"},
		{"config inside the secret files", replace("path: /etc/postgresql/conf", "path: /run/secrets/conf"), "components.postgres.config.path", "secret files"},
		{"config around the secret files", replace("path: /etc/postgresql/conf", "path: /run"), "components.postgres.config.path", "secret files"},
		{"config over the volume", replace("path: /etc/postgresql/conf", "path: /var/lib/postgresql/data"), "components.postgres.config.path", "volume"},
		{"config inside the volume", replace("path: /etc/postgresql/conf", "path: /var/lib/postgresql/data/conf"), "components.postgres.config.path", "volume"},
		{"on_config_change with no config", replace(pgConfig, ""), "components.postgres.on_config_change", "config"},
		{"bad on_config_change", replace(`[psql, -U, postgres, -c, "SELECT pg_reload_conf()"]`, "sometimes"), "components.postgres.on_config_change", "restart, reload or a command"},
		{"on_config_change on a task", insertAfter("    config: { from: config/idp, path: /config }\n", "    on_config_change: reload\n"), "components.idp-config.on_config_change", "each time it runs"},
		{"configure names nothing", replace("configure: idp-config", "configure: nope"), "components.idp.configure", "no task nope"},
		{"configure names a service", replace("configure: idp-config", "configure: api"), "components.idp.configure", "api is a service, not a task"},
		{"configure on a task", insertAfter("    config: { from: config/idp, path: /config }\n", "    configure: idp-config\n"), "components.idp-config.configure", "services, workers and stateful"},
		{"configure task is also a migration", replace("migrate: [dotnet, Api.dll, migrate]", "migrate: idp-config"), "components.api.migrate", "idp's configure task"},
		{"two components share a configure task", insertAfter(pgHook, "    configure: idp-config\n"), "components.postgres.configure", "already idp's"},
		{"a dependent the configure task waits for", func(s string) string {
			// api waits for idp's configure task, which waits for api: neither ever starts.
			s = replace("    depends_on: [idp]\n", "    depends_on: [idp, api]\n")(s)
			return replace("depends_on: [postgres, redis]", "depends_on: [postgres, redis, idp]")(s)
		}, "components.idp-config.depends_on", "cycle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Parse([]byte(tt.edit(configExample)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			problems := s.Check()
			for _, p := range problems {
				if p.Path == tt.path && strings.Contains(p.Msg, tt.want) {
					return
				}
			}
			t.Errorf("no problem at %s containing %q; got:\n%v", tt.path, tt.want, Problems(problems))
		})
	}
}

func TestConfigFingerprint(t *testing.T) {
	s := mustParse(t, configExample)
	// A component that restarts for new config is replaced when its files change.
	restart := *s.Components["postgres"]
	restart.OnConfigChange = nil
	fp := restart.Fingerprint("img:c-1", "files-1")
	if restart.Fingerprint("img:c-1", "files-2") == fp {
		t.Errorf("restart: new files kept the fingerprint, so its replicas would never read them")
	}
	moved := restart
	moved.Config = &Config{From: restart.Config.From, Path: "/etc/elsewhere"}
	if moved.Fingerprint("img:c-1", "files-1") == fp {
		t.Errorf("restart: a new path kept the fingerprint")
	}

	// A follower keeps its replicas: new files are swapped in under them.
	for name, change := range map[string]*Change{"reload": {Mode: "reload"}, "command": {Command: []string{"x"}}} {
		c := restart
		c.OnConfigChange = change
		ffp := c.Fingerprint("img:c-1", "files-1")
		if c.Fingerprint("img:c-1", "files-2") != ffp {
			t.Errorf("%s: new files changed the fingerprint, which replaces its replicas", name)
		}
		if ffp == fp {
			t.Errorf("%s: becoming a follower kept the fingerprint, so running replicas never learn to follow", name)
		}
		m := c
		m.Config = &Config{From: c.Config.From, Path: "/etc/elsewhere"}
		if m.Fingerprint("img:c-1", "files-1") == ffp {
			t.Errorf("%s: a new path kept the fingerprint, but the mount is fixed when a replica is made", name)
		}
	}
	explicit := restart
	explicit.OnConfigChange = &Change{Mode: "restart"}
	if explicit.Fingerprint("img:c-1", "files-1") != fp {
		t.Errorf("restart, said, changed the fingerprint")
	}

	// Configure and the files from which config is read never shape a replica.
	idp := *s.Components["idp"]
	ifp := idp.Fingerprint("img:c-1", "")
	idp.Configure = ""
	if idp.Fingerprint("img:c-1", "") != ifp {
		t.Errorf("configure changed the fingerprint")
	}
	from := restart
	from.Config = &Config{From: "elsewhere", Path: restart.Config.Path}
	if from.Fingerprint("img:c-1", "files-1") != fp {
		t.Errorf("config's from changed the fingerprint: only the files themselves count")
	}

	// A task's files go into its fingerprint: a configure task runs again when they change.
	task := *s.Components["idp-config"]
	if task.Fingerprint("img:c-1", "files-1") == task.Fingerprint("img:c-1", "files-2") {
		t.Errorf("a task's new files kept its fingerprint")
	}
}
