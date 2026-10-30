// Package firewall keeps the agent's iptables rules in place (docs/network.md#containers-and-their-vm):
//
//   - Off the VM, containers reach only the VPC's resolver and the managed subnets in the
//     VPC. The internet and AWS are unaffected.
//   - On their VM, containers reach only the credentials the agent serves, which requests
//     to 169.254.170.2 are redirected to. Instance metadata is out of reach.
//   - On an ingress VM, the other VMs' outbound traffic is forwarded and masqueraded.
//
// The rules live in the agent's own chains, jumped to first from INPUT, DOCKER-USER,
// PREROUTING and POSTROUTING. Docker rewrites its rules when it restarts, so Ensure checks
// everything and puts back whatever is missing.
package firewall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// Config is what the rules are built from.
type Config struct {
	// ContainerCIDRs are Docker's bridges, which containers' traffic leaves the VM from.
	ContainerCIDRs []string
	VPCCIDR        string
	// Resolver is the VPC's DNS resolver.
	Resolver string
	// ManagedCIDRs are the subnets for RDS, ElastiCache and interface endpoints.
	ManagedCIDRs []string
	// CredentialsPort is where the agent serves credentials.
	CredentialsPort int
	// NAT is set on ingress VMs only.
	NAT *NAT
}

// NAT forwards the other VMs' traffic out of an ingress VM.
type NAT struct {
	Interface string // It is the VM's network interface, such as ens5.
}

func (c Config) Validate() error {
	var errs []error
	if len(c.ContainerCIDRs) == 0 {
		errs = append(errs, errors.New("no container ranges"))
	}
	for _, cidr := range append(append([]string{c.VPCCIDR}, c.ContainerCIDRs...), c.ManagedCIDRs...) {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			errs = append(errs, fmt.Errorf("range %q: %w", cidr, err))
		}
	}
	if _, err := netip.ParseAddr(c.Resolver); err != nil {
		errs = append(errs, fmt.Errorf("resolver %q: %w", c.Resolver, err))
	}
	if c.CredentialsPort < 1 || c.CredentialsPort > 65535 {
		errs = append(errs, fmt.Errorf("credentials port %d", c.CredentialsPort))
	}
	if c.NAT != nil && c.NAT.Interface == "" {
		errs = append(errs, errors.New("NAT needs an interface"))
	}
	return errors.Join(errs...)
}

// Runner runs a command and returns its output. A command that exits non-zero returns a
// *CommandError.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// CommandError is the error from a command that exited non-zero.
type CommandError struct {
	Code   int
	Stderr string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("exit status %d: %s", e.Code, strings.TrimSpace(e.Stderr))
}

// isMissing reports whether iptables said a rule or chain doesn't exist.
func isMissing(err error) bool {
	var cerr *CommandError
	return errors.As(err, &cerr) && cerr.Code == 1
}

// Exec runs commands on the VM.
type Exec struct{}

func (Exec) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), &CommandError{Code: exitErr.ExitCode(), Stderr: stderr.String()}
	}
	return stdout.String(), err
}

// Sysctl reads and writes kernel settings.
type Sysctl struct {
	Get func(key string) (string, error)
	Set func(key, value string) error
}

func procPath(key string) string { return "/proc/sys/" + strings.ReplaceAll(key, ".", "/") }

// ProcSysctl uses /proc/sys.
var ProcSysctl = Sysctl{
	Get: func(key string) (string, error) {
		b, err := os.ReadFile(procPath(key))
		return strings.TrimSpace(string(b)), err
	},
	Set: func(key, value string) error { return os.WriteFile(procPath(key), []byte(value), 0o644) },
}

// Firewall keeps the rules in place.
type Firewall struct {
	cfg    Config
	run    Runner
	sysctl Sysctl
}

func New(cfg Config, run Runner, sysctl Sysctl) *Firewall {
	if sysctl.Get == nil || sysctl.Set == nil {
		sysctl = ProcSysctl
	}
	return &Firewall{cfg: cfg, run: run, sysctl: sysctl}
}

const (
	chainForward     = "PROTOSTRAP-FORWARD"
	chainInput       = "PROTOSTRAP-INPUT"
	chainPrerouting  = "PROTOSTRAP-PREROUTING"
	chainPostrouting = "PROTOSTRAP-POSTROUTING"
	credentialsAddr  = "169.254.170.2/32"
	metadataAddr     = "169.254.169.254/32"
	established      = "-m conntrack --ctstate RELATED,ESTABLISHED"
)

type ownChain struct{ table, name string }

type jump struct{ table, from, to string }

func (f *Firewall) chains() []ownChain {
	c := []ownChain{{"filter", chainForward}, {"filter", chainInput}, {"nat", chainPrerouting}}
	if f.cfg.NAT != nil {
		c = append(c, ownChain{"nat", chainPostrouting})
	}
	return c
}

func (f *Firewall) jumps() []jump {
	j := []jump{{"filter", "DOCKER-USER", chainForward}, {"filter", "INPUT", chainInput}, {"nat", "PREROUTING", chainPrerouting}}
	if f.cfg.NAT != nil {
		j = append(j, jump{"nat", "POSTROUTING", chainPostrouting})
	}
	return j
}

func host(addr string) string { return addr + "/32" }

// chain returns the rules one of the agent's chains should hold, in order.
func (f *Firewall) chain(table, name string) []string {
	cfg := f.cfg
	var rules []string
	add := func(format string, args ...any) { rules = append(rules, fmt.Sprintf(format, args...)) }
	switch table + "/" + name {
	case "filter/" + chainForward:
		if cfg.NAT != nil {
			ifc, vpc := cfg.NAT.Interface, cfg.VPCCIDR
			add("-i %s -o %s -s %s ! -d %s -j ACCEPT", ifc, ifc, vpc, vpc)
			add("-i %s -o %s -d %s %s -j ACCEPT", ifc, ifc, vpc, established)
		}
		add("%s -j RETURN", established)
		for _, c := range cfg.ContainerCIDRs {
			add("-s %s -d %s -p udp --dport 53 -j RETURN", c, host(cfg.Resolver))
			add("-s %s -d %s -p tcp --dport 53 -j RETURN", c, host(cfg.Resolver))
			for _, m := range cfg.ManagedCIDRs {
				add("-s %s -d %s -j RETURN", c, m)
			}
			add("-s %s -d %s -j DROP", c, metadataAddr)
			add("-s %s -d %s -j DROP", c, cfg.VPCCIDR)
		}
	case "filter/" + chainInput:
		add("%s -j RETURN", established)
		for _, c := range cfg.ContainerCIDRs {
			add("-s %s -p tcp --dport %d -j RETURN", c, cfg.CredentialsPort)
		}
		for _, c := range cfg.ContainerCIDRs {
			add("-s %s -j DROP", c)
		}
		add("! -i lo -p tcp --dport %d -j DROP", cfg.CredentialsPort)
	case "nat/" + chainPrerouting:
		for _, c := range cfg.ContainerCIDRs {
			add("-s %s -d %s -p tcp --dport 80 -j REDIRECT --to-ports %d", c, credentialsAddr, cfg.CredentialsPort)
		}
	case "nat/" + chainPostrouting:
		if cfg.NAT != nil {
			add("-s %s ! -d %s -o %s -j MASQUERADE", cfg.VPCCIDR, cfg.VPCCIDR, cfg.NAT.Interface)
		}
	}
	return rules
}

func (f *Firewall) iptables(ctx context.Context, table string, args ...string) (string, error) {
	return f.run.Run(ctx, append([]string{"iptables", "-w", "-t", table}, args...)...)
}

// rulesOf lists a chain's rules as `iptables -S` prints them, without the "-A CHAIN ".
func (f *Firewall) rulesOf(ctx context.Context, table, chain string) ([]string, error) {
	out, err := f.iptables(ctx, table, "-S", chain)
	if err != nil {
		return nil, err
	}
	var rules []string
	for line := range strings.SplitSeq(out, "\n") {
		if r, ok := strings.CutPrefix(strings.TrimSpace(line), "-A "+chain+" "); ok {
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// Ensure puts back any missing rule, and returns what it had to repair.
func (f *Firewall) Ensure(ctx context.Context) ([]string, error) {
	var repaired []string
	for _, c := range f.chains() {
		fixed, err := f.ensureChain(ctx, c.table, c.name, f.chain(c.table, c.name))
		if err != nil {
			return repaired, err
		}
		if fixed {
			repaired = append(repaired, c.table+"/"+c.name)
		}
	}
	for _, j := range f.jumps() {
		fixed, err := f.ensureJump(ctx, j)
		if err != nil {
			return repaired, err
		}
		if fixed {
			repaired = append(repaired, j.table+"/"+j.from)
		}
	}
	if f.cfg.NAT != nil {
		const key = "net.ipv4.ip_forward"
		if v, err := f.sysctl.Get(key); err != nil || v != "1" {
			if err := f.sysctl.Set(key, "1"); err != nil {
				return repaired, fmt.Errorf("sysctl %s: %w", key, err)
			}
			repaired = append(repaired, "sysctl/"+key)
		}
	}
	return repaired, nil
}

// ensureChain makes a chain hold exactly rules. It checks each rule with -C, which
// compares rules as iptables understands them rather than as it prints them, and rebuilds
// the chain if any is missing or there are extras.
func (f *Firewall) ensureChain(ctx context.Context, table, chain string, rules []string) (bool, error) {
	current, err := f.rulesOf(ctx, table, chain)
	if isMissing(err) {
		if _, err := f.iptables(ctx, table, "-N", chain); err != nil {
			return false, fmt.Errorf("creating %s/%s: %w", table, chain, err)
		}
		current, err = nil, nil
	}
	if err != nil {
		return false, fmt.Errorf("listing %s/%s: %w", table, chain, err)
	}
	intact := len(current) == len(rules)
	for _, r := range rules {
		if !intact {
			break
		}
		_, err := f.iptables(ctx, table, append([]string{"-C", chain}, strings.Fields(r)...)...)
		if isMissing(err) {
			intact = false
		} else if err != nil {
			return false, fmt.Errorf("checking %s/%s: %w", table, chain, err)
		}
	}
	if intact {
		return false, nil
	}
	if _, err := f.iptables(ctx, table, "-F", chain); err != nil {
		return false, fmt.Errorf("flushing %s/%s: %w", table, chain, err)
	}
	for _, r := range rules {
		if _, err := f.iptables(ctx, table, append([]string{"-A", chain}, strings.Fields(r)...)...); err != nil {
			return false, fmt.Errorf("adding to %s/%s %q: %w", table, chain, r, err)
		}
	}
	return true, nil
}

// ensureJump makes the jump to one of the agent's chains the first rule of a built-in
// chain, so no rule Docker adds can come before it.
func (f *Firewall) ensureJump(ctx context.Context, j jump) (bool, error) {
	rule := "-j " + j.to
	current, err := f.rulesOf(ctx, j.table, j.from)
	if isMissing(err) && j.from == "DOCKER-USER" {
		// Docker creates DOCKER-USER; make it so the jump has somewhere to go meanwhile.
		if _, err := f.iptables(ctx, j.table, "-N", j.from); err != nil {
			return false, fmt.Errorf("creating %s/%s: %w", j.table, j.from, err)
		}
		current, err = nil, nil
	}
	if err != nil {
		return false, fmt.Errorf("listing %s/%s: %w", j.table, j.from, err)
	}
	if len(current) > 0 && current[0] == rule && slices.Index(current[1:], rule) < 0 {
		return false, nil
	}
	for slices.Contains(current, rule) {
		_, err := f.iptables(ctx, j.table, "-D", j.from, "-j", j.to)
		if isMissing(err) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("removing %s from %s/%s: %w", rule, j.table, j.from, err)
		}
	}
	if _, err := f.iptables(ctx, j.table, "-I", j.from, "1", "-j", j.to); err != nil {
		return false, fmt.Errorf("inserting %s into %s/%s: %w", rule, j.table, j.from, err)
	}
	return true, nil
}
