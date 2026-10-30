package firewall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fakeIptables is an in-memory iptables: tables of chains of rules, with iptables' exit
// codes. Rules are compared as the argument strings they were added with.
type fakeIptables struct {
	tables  map[string]map[string][]string
	sysctls map[string]string
	calls   [][]string
	// fail makes the fake fail any command that contains it.
	fail string
}

func newFake() *fakeIptables {
	return &fakeIptables{
		tables: map[string]map[string][]string{
			"filter": {"INPUT": nil, "FORWARD": nil, "OUTPUT": nil, "DOCKER-USER": {"-j RETURN"}},
			"nat":    {"PREROUTING": nil, "POSTROUTING": nil, "OUTPUT": nil},
		},
		sysctls: map[string]string{"net.ipv4.ip_forward": "0"},
	}
}

func (f *fakeIptables) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.fail != "" && strings.Contains(strings.Join(args, " "), f.fail) {
		return "", &CommandError{Code: 4, Stderr: "resource unavailable"}
	}
	if args[0] != "iptables" {
		return "", fmt.Errorf("unexpected command %v", args)
	}
	args = args[1:]
	if len(args) > 0 && args[0] == "-w" {
		args = args[1:]
	}
	table := "filter"
	if len(args) > 1 && args[0] == "-t" {
		table, args = args[1], args[2:]
	}
	chains := f.tables[table]
	op, chain := args[0], args[1]
	rule := strings.Join(args[2:], " ")
	rules, exists := chains[chain]
	missing := &CommandError{Code: 1, Stderr: "iptables: No chain/target/match by that name."}
	switch op {
	case "-N":
		if exists {
			return "", &CommandError{Code: 1, Stderr: "iptables: Chain already exists."}
		}
		chains[chain] = nil
	case "-S":
		if !exists {
			return "", missing
		}
		out := "-N " + chain + "\n"
		if slices.Contains([]string{"INPUT", "FORWARD", "OUTPUT", "PREROUTING", "POSTROUTING"}, chain) {
			out = "-P " + chain + " ACCEPT\n"
		}
		for _, r := range rules {
			out += "-A " + chain + " " + r + "\n"
		}
		return out, nil
	case "-C":
		if !exists || !slices.Contains(rules, rule) {
			return "", missing
		}
	case "-A":
		if !exists {
			return "", missing
		}
		chains[chain] = append(rules, rule)
	case "-I":
		if !exists {
			return "", missing
		}
		pos := 1
		if len(args) > 2 {
			if n, err := fmt.Sscan(args[2], &pos); n == 1 && err == nil {
				rule = strings.Join(args[3:], " ")
			}
		}
		chains[chain] = slices.Insert(slices.Clone(rules), pos-1, rule)
	case "-D":
		i := slices.Index(rules, rule)
		if !exists || i < 0 {
			return "", missing
		}
		chains[chain] = slices.Delete(slices.Clone(rules), i, i+1)
	case "-F":
		if !exists {
			return "", missing
		}
		chains[chain] = nil
	default:
		return "", fmt.Errorf("unsupported op %s", op)
	}
	return "", nil
}

func (f *fakeIptables) sysctl() Sysctl {
	return Sysctl{
		Get: func(k string) (string, error) { return f.sysctls[k], nil },
		Set: func(k, v string) error { f.sysctls[k] = v; return nil },
	}
}

func testConfig() Config {
	return Config{
		ContainerCIDRs:  []string{"172.17.0.0/24", "172.18.0.0/24", "172.19.0.0/16"},
		VPCCIDR:         "10.20.0.0/16",
		Resolver:        "10.20.0.2",
		ManagedCIDRs:    []string{"10.20.2.0/24"},
		CredentialsPort: 51679,
	}
}

func ensure(t *testing.T, fw *Firewall) []string {
	t.Helper()
	repaired, err := fw.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return repaired
}

func TestFirstEnsureInstallsEverything(t *testing.T) {
	f := newFake()
	fw := New(testConfig(), f, f.sysctl())
	repaired := ensure(t, fw)
	if len(repaired) == 0 {
		t.Fatal("the first ensure installs the rules, which counts as a repair")
	}
	filter, nat := f.tables["filter"], f.tables["nat"]
	if filter["DOCKER-USER"][0] != "-j PROTOSTRAP-FORWARD" {
		t.Errorf("DOCKER-USER must jump to our chain first: %v", filter["DOCKER-USER"])
	}
	if filter["INPUT"][0] != "-j PROTOSTRAP-INPUT" {
		t.Errorf("INPUT must jump to our chain first: %v", filter["INPUT"])
	}
	if nat["PREROUTING"][0] != "-j PROTOSTRAP-PREROUTING" {
		t.Errorf("PREROUTING must jump to our chain first: %v", nat["PREROUTING"])
	}
	if _, ok := nat["PROTOSTRAP-POSTROUTING"]; ok {
		t.Error("NAT chains are only for ingress VMs")
	}
}

func TestSecondEnsureChangesNothing(t *testing.T) {
	f := newFake()
	fw := New(testConfig(), f, f.sysctl())
	ensure(t, fw)
	f.calls = nil
	if repaired := ensure(t, fw); len(repaired) != 0 {
		t.Fatalf("nothing was missing, but repaired %v", repaired)
	}
	for _, c := range f.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, " -A ") || strings.Contains(joined, " -I ") || strings.Contains(joined, " -F ") {
			t.Errorf("a check must not change rules: %s", joined)
		}
	}
}

func TestRulesKeepContainersOffTheVPC(t *testing.T) {
	fw := New(testConfig(), newFake(), Sysctl{})
	forward := fw.chain("filter", "PROTOSTRAP-FORWARD")
	for _, want := range []string{
		"-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN",
		"-s 172.18.0.0/24 -d 10.20.0.2/32 -p udp --dport 53 -j RETURN",
		"-s 172.18.0.0/24 -d 10.20.0.2/32 -p tcp --dport 53 -j RETURN",
		"-s 172.18.0.0/24 -d 10.20.2.0/24 -j RETURN",
		"-s 172.18.0.0/24 -d 169.254.169.254/32 -j DROP",
		"-s 172.18.0.0/24 -d 10.20.0.0/16 -j DROP",
	} {
		if !slices.Contains(forward, want) {
			t.Errorf("forward chain is missing %q:\n%s", want, strings.Join(forward, "\n"))
		}
	}
	// Order matters: the resolver and the managed subnets are allowed before the VPC is
	// dropped, and nothing else in the VPC is reachable.
	allowResolver := slices.Index(forward, "-s 172.18.0.0/24 -d 10.20.0.2/32 -p udp --dport 53 -j RETURN")
	allowManaged := slices.Index(forward, "-s 172.18.0.0/24 -d 10.20.2.0/24 -j RETURN")
	dropVPC := slices.Index(forward, "-s 172.18.0.0/24 -d 10.20.0.0/16 -j DROP")
	if !(allowResolver < dropVPC && allowManaged < dropVPC) {
		t.Errorf("allows must come before the VPC drop:\n%s", strings.Join(forward, "\n"))
	}
	for _, r := range forward {
		if strings.Contains(r, "-j ACCEPT") {
			t.Errorf("without NAT, the forward chain only returns or drops: %s", r)
		}
	}
}

func TestRulesKeepContainersOffTheirVM(t *testing.T) {
	fw := New(testConfig(), newFake(), Sysctl{})
	input := fw.chain("filter", "PROTOSTRAP-INPUT")
	allowCreds := slices.Index(input, "-s 172.18.0.0/24 -p tcp --dport 51679 -j RETURN")
	dropRest := slices.Index(input, "-s 172.18.0.0/24 -j DROP")
	if allowCreds < 0 || dropRest < 0 || allowCreds > dropRest {
		t.Fatalf("containers may reach only the credentials port on their VM:\n%s", strings.Join(input, "\n"))
	}
	if input[0] != "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN" {
		t.Errorf("replies to the agent's own probes must be let in first: %v", input)
	}
	if !slices.Contains(input, "! -i lo -p tcp --dport 51679 -j DROP") {
		t.Errorf("only containers and the VM itself may reach the credentials port: %v", input)
	}
}

func TestCredentialsRedirect(t *testing.T) {
	fw := New(testConfig(), newFake(), Sysctl{})
	pre := fw.chain("nat", "PROTOSTRAP-PREROUTING")
	for _, cidr := range testConfig().ContainerCIDRs {
		want := "-s " + cidr + " -d 169.254.170.2/32 -p tcp --dport 80 -j REDIRECT --to-ports 51679"
		if !slices.Contains(pre, want) {
			t.Errorf("missing %q in %v", want, pre)
		}
	}
}

func TestEnsureRepairsADeletedRule(t *testing.T) {
	f := newFake()
	fw := New(testConfig(), f, f.sysctl())
	ensure(t, fw)
	chain := f.tables["filter"]["PROTOSTRAP-FORWARD"]
	f.tables["filter"]["PROTOSTRAP-FORWARD"] = slices.Delete(slices.Clone(chain), 3, 4)
	repaired := ensure(t, fw)
	if !slices.Contains(repaired, "filter/PROTOSTRAP-FORWARD") {
		t.Fatalf("repaired %v", repaired)
	}
	if !slices.Equal(f.tables["filter"]["PROTOSTRAP-FORWARD"], fw.chain("filter", "PROTOSTRAP-FORWARD")) {
		t.Fatal("chain not rebuilt exactly")
	}
}

func TestEnsureRemovesAStrayRule(t *testing.T) {
	f := newFake()
	fw := New(testConfig(), f, f.sysctl())
	ensure(t, fw)
	f.tables["filter"]["PROTOSTRAP-INPUT"] = append(f.tables["filter"]["PROTOSTRAP-INPUT"], "-j ACCEPT")
	if repaired := ensure(t, fw); !slices.Contains(repaired, "filter/PROTOSTRAP-INPUT") {
		t.Fatalf("an extra rule must be removed, repaired %v", repaired)
	}
	if slices.Contains(f.tables["filter"]["PROTOSTRAP-INPUT"], "-j ACCEPT") {
		t.Fatal("stray rule kept")
	}
}

func TestEnsureRestoresAJumpDockerRemoved(t *testing.T) {
	f := newFake()
	fw := New(testConfig(), f, f.sysctl())
	ensure(t, fw)
	// A Docker restart rewrites DOCKER-USER.
	f.tables["filter"]["DOCKER-USER"] = []string{"-j RETURN"}
	repaired := ensure(t, fw)
	if !slices.Contains(repaired, "filter/DOCKER-USER") {
		t.Fatalf("repaired %v", repaired)
	}
	if f.tables["filter"]["DOCKER-USER"][0] != "-j PROTOSTRAP-FORWARD" {
		t.Fatalf("DOCKER-USER = %v", f.tables["filter"]["DOCKER-USER"])
	}
}

func TestEnsureMovesAJumpBackToTheTop(t *testing.T) {
	f := newFake()
	fw := New(testConfig(), f, f.sysctl())
	ensure(t, fw)
	f.tables["filter"]["INPUT"] = []string{"-j ACCEPT", "-j PROTOSTRAP-INPUT"}
	repaired := ensure(t, fw)
	if !slices.Contains(repaired, "filter/INPUT") {
		t.Fatalf("repaired %v", repaired)
	}
	if got := f.tables["filter"]["INPUT"]; !slices.Equal(got, []string{"-j PROTOSTRAP-INPUT", "-j ACCEPT"}) {
		t.Fatalf("INPUT = %v", got)
	}
}

func TestEnsureRecreatesADeletedChain(t *testing.T) {
	f := newFake()
	fw := New(testConfig(), f, f.sysctl())
	ensure(t, fw)
	delete(f.tables["nat"], "PROTOSTRAP-PREROUTING")
	f.tables["nat"]["PREROUTING"] = nil
	repaired := ensure(t, fw)
	if !slices.Contains(repaired, "nat/PROTOSTRAP-PREROUTING") || !slices.Contains(repaired, "nat/PREROUTING") {
		t.Fatalf("repaired %v", repaired)
	}
	if len(f.tables["nat"]["PROTOSTRAP-PREROUTING"]) == 0 {
		t.Fatal("chain not recreated")
	}
}

func TestNATOnIngressVMs(t *testing.T) {
	cfg := testConfig()
	cfg.NAT = &NAT{Interface: "ens5"}
	f := newFake()
	fw := New(cfg, f, f.sysctl())
	repaired := ensure(t, fw)
	if !slices.Contains(repaired, "sysctl/net.ipv4.ip_forward") || f.sysctls["net.ipv4.ip_forward"] != "1" {
		t.Fatalf("forwarding must be on: %v %v", repaired, f.sysctls)
	}
	post := f.tables["nat"]["PROTOSTRAP-POSTROUTING"]
	if !slices.Contains(post, "-s 10.20.0.0/16 ! -d 10.20.0.0/16 -o ens5 -j MASQUERADE") {
		t.Fatalf("masquerade missing: %v", post)
	}
	if f.tables["nat"]["POSTROUTING"][0] != "-j PROTOSTRAP-POSTROUTING" {
		t.Fatalf("POSTROUTING = %v", f.tables["nat"]["POSTROUTING"])
	}
	forward := f.tables["filter"]["PROTOSTRAP-FORWARD"]
	accept := slices.Index(forward, "-i ens5 -o ens5 -s 10.20.0.0/16 ! -d 10.20.0.0/16 -j ACCEPT")
	replies := slices.Index(forward, "-i ens5 -o ens5 -d 10.20.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT")
	returns := slices.Index(forward, "-m conntrack --ctstate RELATED,ESTABLISHED -j RETURN")
	if accept < 0 || replies < 0 {
		t.Fatalf("NAT forwarding rules missing:\n%s", strings.Join(forward, "\n"))
	}
	if accept > returns || replies > returns {
		t.Fatalf("NAT replies must be accepted before established traffic returns to Docker's chains:\n%s", strings.Join(forward, "\n"))
	}
}

func TestNATForwardingTurnedOffIsRepaired(t *testing.T) {
	cfg := testConfig()
	cfg.NAT = &NAT{Interface: "ens5"}
	f := newFake()
	fw := New(cfg, f, f.sysctl())
	ensure(t, fw)
	f.sysctls["net.ipv4.ip_forward"] = "0"
	if repaired := ensure(t, fw); !slices.Equal(repaired, []string{"sysctl/net.ipv4.ip_forward"}) {
		t.Fatalf("repaired %v", repaired)
	}
}

func TestEnsureReportsFailures(t *testing.T) {
	f := newFake()
	f.fail = "-t nat"
	fw := New(testConfig(), f, f.sysctl())
	if _, err := fw.Ensure(t.Context()); err == nil {
		t.Fatal("a failing iptables must be reported")
	}
}

func TestEnsureWaitsForTheXtablesLock(t *testing.T) {
	f := newFake()
	ensure(t, New(testConfig(), f, f.sysctl()))
	for _, c := range f.calls {
		if c[0] == "iptables" && c[1] != "-w" {
			t.Fatalf("every iptables call must wait for the lock: %v", c)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Config){
		func(c *Config) { c.ContainerCIDRs = nil },
		func(c *Config) { c.ContainerCIDRs = []string{"nope"} },
		func(c *Config) { c.VPCCIDR = "10.20.0.0" },
		func(c *Config) { c.Resolver = "x" },
		func(c *Config) { c.ManagedCIDRs = []string{"10.20.2.0/33"} },
		func(c *Config) { c.CredentialsPort = 0 },
		func(c *Config) { c.NAT = &NAT{} },
	}
	for i, mutate := range bad {
		c := testConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
}

func TestCommandErrorCode(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &CommandError{Code: 1})
	if !isMissing(err) {
		t.Fatal("exit status 1 means the rule or chain is missing")
	}
	if isMissing(errors.New("other")) || isMissing(&CommandError{Code: 4}) {
		t.Fatal("other failures are real errors")
	}
}
