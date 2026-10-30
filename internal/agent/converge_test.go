package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
)

// labelsFor builds the labels an earlier agent would have put on a replica.
func labelsFor(s api.ReplicaSpec) map[string]string {
	l := map[string]string{
		container.LabelReplica: s.ID, container.LabelComponent: s.Component, container.LabelRelease: s.Release,
		container.LabelKind: string(s.Kind), container.LabelStop: "30", container.LabelApp: "shop",
	}
	if s.Stop != 0 {
		l[container.LabelStop] = strconv.Itoa(s.Stop)
	}
	if s.Role != "" {
		l[container.LabelRole] = s.Role
	}
	if s.Ready != "" {
		l[container.LabelReady] = s.Ready
	}
	if s.Port != 0 {
		l[container.LabelPort] = strconv.Itoa(s.Port)
	}
	return l
}

func TestStartsAnAssignedReplica(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	creates, starts, _, _, pulls := h.rt.snapshot()
	if !slices.Equal(pulls, []string{registry + "api:c-4e1a90b2"}) || !slices.Equal(creates, []string{"api-1"}) || !slices.Equal(starts, []string{"api-1"}) {
		t.Fatalf("pulls %v creates %v starts %v", pulls, creates, starts)
	}
	calls := h.rt.calls()
	if slices.Index(calls, "pull "+registry+"api:c-4e1a90b2") > slices.Index(calls, "create api-1") {
		t.Fatalf("the image is pulled before the container is created: %v", calls)
	}
	h.mustTick()
	if got := h.state("api-1"); got != api.StateStarting {
		t.Fatalf("state %s", got)
	}
}

func TestPullsOnlyMissingImages(t *testing.T) {
	h := newHarness(t)
	h.rt.images[registry+"api:c-4e1a90b2"] = true
	h.sched.assign(service("api-1"))
	h.mustTick()
	if _, _, _, _, pulls := h.rt.snapshot(); len(pulls) != 0 {
		t.Fatalf("image tags are content hashes: a present tag needs no pull, pulled %v", pulls)
	}
}

func TestLeavesRunningReplicasAlone(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"), worker("w-1"))
	h.rt.resetCalls()
	for range 3 {
		h.mustTick()
	}
	creates, starts, removes, stops, pulls := h.rt.snapshot()
	if len(creates)+len(starts)+len(removes)+len(stops)+len(pulls) != 0 {
		t.Fatalf("a converged VM must be left alone: %v %v %v %v %v", creates, starts, removes, stops, pulls)
	}
}

func TestTrustsReplicaIDs(t *testing.T) {
	// "A replica's settings never change": a new release gets a new id, so the agent never
	// compares settings or recreates a replica it is already running.
	h := newHarness(t)
	h.running(service("api-1"))
	changed := service("api-1")
	changed.Image = registry + "api:c-different"
	changed.Request.Memory = 4096
	h.sched.assign(changed)
	h.rt.resetCalls()
	h.mustTick()
	if creates, _, _, stops, _ := h.rt.snapshot(); len(creates)+len(stops) != 0 {
		t.Fatal("a replica must never be recreated in place")
	}
}

func TestStopsAReplicaLeftOutOfTheAssignment(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"), worker("w-1"))
	h.sched.assign(service("api-1"))
	h.mustTick()
	_, _, removes, stops, _ := h.rt.snapshot()
	if len(stops) != 1 || stops[0].Name != "w-1" || stops[0].Timeout != 120*time.Second {
		t.Fatalf("stops %+v: SIGTERM, then SIGKILL once its stop time has passed", stops)
	}
	if !slices.Equal(removes, []string{"w-1"}) {
		t.Fatalf("removes %v", removes)
	}
	if _, ok := h.creds.get("w-1"); ok {
		t.Fatal("a removed replica's credentials must go")
	}
	h.mustTick()
	h.noReplica("w-1")
}

func TestAnEmptyAssignmentStopsEverything(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"), worker("w-1"), task("m-1", "migrate"))
	h.sched.assign()
	h.mustTick()
	if names := h.rt.names(); len(names) != 0 {
		t.Fatalf("an assignment is complete: %v still there", names)
	}
}

func TestIgnoresContainersItDoesNotManage(t *testing.T) {
	h := newHarness(t)
	h.rt.addContainer(container.Container{Name: "ssm-helper", Status: container.Running, Labels: map[string]string{"x": "y"}})
	h.sched.assign()
	h.mustTick()
	h.mustTick()
	if _, ok := h.rt.byName("ssm-helper"); !ok {
		t.Fatal("a container without the replica label is not the agent's to stop")
	}
	h.noReplica("ssm-helper")
}

func TestStoppingIsReportedAndNeverRepeated(t *testing.T) {
	h := newHarness(t)
	h.running(worker("w-1"))
	gate := make(chan struct{})
	h.rt.mu.Lock()
	h.rt.stopGate = gate
	h.rt.mu.Unlock()
	h.sched.assign()
	start := time.Now()
	if err := h.a.Tick(t.Context()); err != nil { // Tick begins the stop and returns.
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a slow stop must not hold up the cycle")
	}
	if err := h.a.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.state("w-1"); got != api.StateStopping {
		t.Fatalf("state %s", got)
	}
	close(gate)
	h.a.Wait()
	_, _, _, stops, _ := h.rt.snapshot()
	if len(stops) != 1 {
		t.Fatalf("stopped %d times", len(stops))
	}
}

func TestPullFailureIsReportedAndNotRetried(t *testing.T) {
	h := newHarness(t)
	h.rt.pullErr[registry+"api:c-4e1a90b2"] = errors.New("manifest unknown")
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.mustTick()
	r := h.replica("api-1")
	if r.State != api.StateFailed || !strings.Contains(r.Reason, "manifest unknown") || r.Component != "api" {
		t.Fatalf("got %+v", r)
	}
	h.mustTick()
	if _, _, _, _, pulls := h.rt.snapshot(); len(pulls) != 1 {
		t.Fatalf("a failed replica is replaced by the scheduler under a new id, not retried: pulled %d times", len(pulls))
	}
	h.sched.assign()
	h.mustTick()
	h.mustTick()
	h.noReplica("api-1")
}

func TestCreateFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.rt.createErr = errors.New("no space left on device")
	h.sched.assign(worker("w-1"))
	h.mustTick()
	h.mustTick()
	if r := h.replica("w-1"); r.State != api.StateFailed || !strings.Contains(r.Reason, "no space left") {
		t.Fatalf("got %+v", r)
	}
	if _, ok := h.creds.get("w-1"); ok {
		t.Fatal("credentials of a replica that never ran must go")
	}
}

func TestStartFailureRemovesTheContainer(t *testing.T) {
	// The swarm manager is down: new replicas can't join the overlay.
	h := newHarness(t)
	h.rt.startErr = errors.New("attaching to network: manager unreachable")
	h.sched.assign(worker("w-1"))
	h.mustTick()
	if _, ok := h.rt.byName("w-1"); ok {
		t.Fatal("a container that failed to start must not be left behind")
	}
	h.mustTick()
	if r := h.replica("w-1"); r.State != api.StateFailed || !strings.Contains(r.Reason, "manager unreachable") {
		t.Fatalf("got %+v", r)
	}
}

func TestReplicaRemovedWhilePullingIsCancelled(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.rt.pullGate = gate
	defer close(gate)
	h.sched.assign(service("api-1"))
	if err := h.a.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.sched.assign()
	h.mustTick() // This cycle cancels the pull and waits for it.
	creates, _, _, _, _ := h.rt.snapshot()
	if len(creates) != 0 {
		t.Fatalf("created %v after being unassigned", creates)
	}
	if _, ok := h.creds.get("api-1"); ok {
		t.Fatal("credentials left behind")
	}
	h.mustTick()
	h.noReplica("api-1")
}

func TestReplicaBeingPulledIsNotReported(t *testing.T) {
	// The scheduler counts a replica as assigned until the agent has started it.
	h := newHarness(t)
	gate := make(chan struct{})
	h.rt.pullGate = gate
	h.sched.assign(service("api-1"))
	h.a.Tick(t.Context())
	h.a.Tick(t.Context())
	h.noReplica("api-1")
	close(gate)
	h.a.Wait()
	h.mustTick()
	if h.state("api-1") != api.StateStarting {
		t.Fatalf("state %s", h.state("api-1"))
	}
}

func TestSchedulerDownChangesNothing(t *testing.T) {
	// "Down means frozen, not broken."
	h := newHarness(t)
	h.running(service("api-1"), worker("w-1"))
	h.sched.syncErr = errors.New("connection refused")
	h.rt.resetCalls()
	for range 10 {
		if err := h.tick(); err == nil {
			t.Fatal("a failed sync is reported to the caller")
		}
		h.clock.advance(5 * time.Second)
	}
	creates, starts, removes, stops, _ := h.rt.snapshot()
	if len(creates)+len(starts)+len(removes)+len(stops) != 0 {
		t.Fatal("nothing may change while the scheduler is down")
	}
	if names := h.rt.names(); !slices.Equal(names, []string{"api-1", "w-1"}) {
		t.Fatalf("replicas %v", names)
	}
	if len(h.creds.Replicas()) != 2 {
		t.Fatal("credentials must keep being served")
	}
}

func TestAdoptsReplicasAfterAnAgentRestart(t *testing.T) {
	h := newHarness(t)
	svc, w := service("api-1"), worker("w-1")
	h.rt.addContainer(container.Container{Name: "api-1", Status: container.Running, Labels: labelsFor(svc),
		Env: map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v1/credentials/kept-token"}})
	h.rt.addContainer(container.Container{Name: "w-1", Status: container.Running, Labels: labelsFor(w), Restarts: 3})
	h.prober.setReady("api", true)
	h.sched.syncErr = errors.New("scheduler restarting")
	h.tick()
	r := h.report()
	if len(r.Replicas) != 2 {
		t.Fatalf("the first report must carry what runs: %+v", r.Replicas)
	}
	if h.state("api-1") != api.StateReady || h.state("w-1") != api.StateReady {
		t.Fatalf("states %+v", r.Replicas)
	}
	got := h.replica("w-1")
	if got.Component != "orders-worker" || got.Release != "9f2c1e7" || got.Kind != api.KindWorker || got.Restarts != 3 {
		t.Fatalf("the scheduler adopts replicas from these labels: %+v", got)
	}
	e, ok := h.creds.get("api-1")
	if !ok || e.Token != "kept-token" || e.Role != svc.Role {
		t.Fatalf("credentials must be served again from the container's own token: %+v", e)
	}
	if _, _, _, stops, _ := h.rt.snapshot(); len(stops) != 0 {
		t.Fatal("nothing may be stopped before the scheduler has answered")
	}
}

func TestCreatedButNeverStartedIsStarted(t *testing.T) {
	// The agent crashed between creating and starting a replica.
	h := newHarness(t)
	s := worker("w-1")
	h.rt.addContainer(container.Container{Name: "w-1", Status: container.Created, Labels: labelsFor(s)})
	h.sched.assign(s)
	h.mustTick()
	_, starts, _, _, _ := h.rt.snapshot()
	if !slices.Equal(starts, []string{"w-1"}) {
		t.Fatalf("starts %v", starts)
	}
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Fatal("must not create it again")
	}
}

func TestAReadyReplicaStoppedByHandIsStartedAgain(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"))
	h.rt.set("api-1", func(c *container.Container) { c.Status = container.Exited; c.ExitCode = 0 })
	h.rt.resetCalls()
	h.mustTick()
	if _, starts, _, _, _ := h.rt.snapshot(); !slices.Equal(starts, []string{"api-1"}) {
		t.Fatalf("starts %v", starts)
	}
}

func TestAFailedReplicaIsNotRestarted(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.rt.set("api-1", func(c *container.Container) { c.Status = container.Exited; c.ExitCode = 1 })
	h.mustTick()
	h.mustTick()
	if got := h.replica("api-1"); got.State != api.StateFailed || !strings.Contains(got.Reason, "before it was ready") {
		t.Fatalf("got %+v", got)
	}
	if _, starts, _, _, _ := h.rt.snapshot(); len(starts) != 1 {
		t.Fatalf("a failed replica is never started again: %v", starts)
	}
}

func TestAFinishedTaskIsNeverRunAgain(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(task("m-1", "migrate"))
	h.mustTick()
	h.rt.set("m-1", func(c *container.Container) { c.Status = container.Exited; c.ExitCode = 0 })
	for range 3 {
		h.mustTick()
	}
	if _, starts, _, _, _ := h.rt.snapshot(); len(starts) != 1 {
		t.Fatalf("starts %v", starts)
	}
	if got := h.state("m-1"); got != api.StateSucceeded {
		t.Fatalf("state %s", got)
	}
}

func TestStatefulStopsBeforeItsReplacementStarts(t *testing.T) {
	// Only one replica can own the volume.
	h := newHarness(t, dataVM)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	old := stateful("postgres-a1", "postgres", 1000, "/data")
	h.running(old)
	gate := make(chan struct{})
	h.rt.mu.Lock()
	h.rt.stopGate = gate
	h.rt.mu.Unlock()
	h.rt.resetCalls()
	h.sched.assign(stateful("postgres-b2", "postgres", 1000, "/data"))
	h.a.Tick(t.Context())
	h.a.Tick(t.Context())
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Fatalf("the new replica started while the old one still held the volume: %v", creates)
	}
	close(gate)
	h.a.Wait()
	h.mustTick()
	calls := h.rt.calls()
	if slices.Index(calls, "remove postgres-a1") < 0 || slices.Index(calls, "create postgres-b2") < slices.Index(calls, "remove postgres-a1") {
		t.Fatalf("order %v", calls)
	}
}

func TestStatefulNeverRunsTwiceEvenIfAssignedTwice(t *testing.T) {
	h := newHarness(t, dataVM)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	h.sched.assign(stateful("postgres-a1", "postgres", 1000, "/data"), stateful("postgres-b2", "postgres", 1000, "/data"))
	h.mustTick()
	h.mustTick()
	creates, _, _, _, _ := h.rt.snapshot()
	if len(creates) != 1 {
		t.Fatalf("two replicas of one stateful component must never share a volume: %v", creates)
	}
}

func TestTwoEdgesRunSideBySideDuringAnUpgrade(t *testing.T) {
	h := newHarness(t)
	h.running(edge("edge-old"))
	h.sched.assign(edge("edge-old"), edge("edge-new"))
	h.mustTick()
	if names := h.rt.names(); !slices.Equal(names, []string{"edge-new", "edge-old"}) {
		t.Fatalf("the new edge starts beside the old one: %v", names)
	}
}

func TestNoNewReplicasWithoutTheFirewall(t *testing.T) {
	h := newHarness(t)
	h.running(worker("w-1"))
	h.fw.err = errors.New("iptables: resource unavailable")
	h.sched.assign(worker("w-1"), service("api-1"), worker("w-2"))
	h.mustTick()
	creates, _, _, stops, _ := h.rt.snapshot()
	if slices.Contains(creates, "api-1") || slices.Contains(creates, "w-2") {
		t.Fatal("nothing new may start while the container firewall is unchecked")
	}
	if len(stops) != 0 {
		t.Fatal("running replicas are kept")
	}
	h.fw.err = nil
	h.mustTick()
	if creates, _, _, _, _ := h.rt.snapshot(); !slices.Contains(creates, "api-1") {
		t.Fatal("replicas start once the firewall is back")
	}
}

func TestFirewallRepairsAreReported(t *testing.T) {
	h := newHarness(t)
	h.fw.repaired = []string{"filter/DOCKER-USER"}
	h.mustTick()
	if r := h.report(); !slices.Equal(r.Repairs, []string{"filter/DOCKER-USER"}) {
		t.Fatalf("repairs %v", r.Repairs)
	}
	h.mustTick()
	if r := h.report(); len(r.Repairs) != 0 {
		t.Fatalf("repairs are reported once: %v", r.Repairs)
	}
}

func TestFirewallCheckedEveryCycle(t *testing.T) {
	h := newHarness(t)
	h.sched.syncErr = errors.New("down")
	for range 4 {
		h.tick()
	}
	if h.fw.calls != 4 {
		t.Fatalf("firewall checked %d times in 4 cycles", h.fw.calls)
	}
}

func TestAVMItCannotSeeIsNotReported(t *testing.T) {
	// An empty report would tell the scheduler nothing runs here. It is better to stay silent.
	h := newHarness(t)
	h.running(service("api-1"))
	syncs := h.sched.syncs()
	h.rt.listErr = errors.New("docker: connection refused")
	if err := h.tick(); err == nil {
		t.Fatal("expected an error")
	}
	if h.sched.syncs() != syncs {
		t.Fatal("must not sync without seeing its containers")
	}
}

func TestWatchdogIsPettedEveryCycle(t *testing.T) {
	h := newHarness(t)
	h.mustTick()
	h.sched.syncErr = errors.New("down")
	h.tick()
	h.sched.syncErr = nil
	h.rt.listErr = errors.New("docker down")
	h.tick()
	if got := h.notify.count("WATCHDOG=1"); got != 3 {
		t.Fatalf("watchdog notified %d times in 3 cycles", got)
	}
}

func TestReportDescribesTheVM(t *testing.T) {
	h := newHarness(t)
	h.mustTick()
	r := h.report()
	if r.VM != "i-0a1b2c3d" || r.Name != "general-3" || r.Pool != "general" || r.Zone != "eu-west-1a" || r.Version != "1.4.0" {
		t.Fatalf("got %+v", r)
	}
	if r.Capacity.CPU != 4000 || r.Capacity.Memory != 16384 {
		t.Fatalf("capacity %+v", r.Capacity)
	}
	if r.Replicas == nil {
		t.Fatal("replicas must be an empty list, not null")
	}
	if r.Terminating != nil {
		t.Fatal("not terminating")
	}
}

func TestReportIsSortedByReplica(t *testing.T) {
	h := newHarness(t)
	h.running(worker("w-3"), worker("w-1"), worker("w-2"))
	var ids []string
	for _, r := range h.report().Replicas {
		ids = append(ids, r.ID)
	}
	if !slices.IsSorted(ids) {
		t.Fatalf("ids %v", ids)
	}
}

func TestSelfUpdate(t *testing.T) {
	h := newHarness(t)
	h.sched.assignment.Version = "1.4.1"
	h.sched.assign(service("api-1"))
	err := h.tick()
	if !errors.Is(err, ErrUpdated) {
		t.Fatalf("got %v", err)
	}
	if !slices.Equal(h.updater.calls, []string{"1.4.1"}) {
		t.Fatalf("updates %v", h.updater.calls)
	}
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Fatal("the new version converges, not the old one")
	}
}

func TestSelfUpdateFailureCarriesOn(t *testing.T) {
	h := newHarness(t)
	h.updater.err = errors.New("checksum mismatch")
	h.sched.assignment.Version = "1.4.1"
	h.sched.assign(service("api-1"))
	h.mustTick()
	if creates, _, _, _, _ := h.rt.snapshot(); !slices.Equal(creates, []string{"api-1"}) {
		t.Fatal("a failed update must not stop the agent doing its job")
	}
	h.mustTick()
	if len(h.updater.calls) != 2 {
		t.Fatalf("the update is retried on the next sync: %v", h.updater.calls)
	}
}

func TestNoUpdateForTheSameOrNoVersion(t *testing.T) {
	h := newHarness(t)
	h.mustTick()
	h.sched.assignment.Version = ""
	h.mustTick()
	if len(h.updater.calls) != 0 {
		t.Fatalf("updates %v", h.updater.calls)
	}
}

func TestFilesAreWrittenWhenTheyChange(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.cfg.FilesDir, "edge", "routes.yaml")
	h.sched.assignment.Files = map[string]string{"edge/routes.yaml": "v1"}
	h.mustTick()
	if b, _ := os.ReadFile(path); string(b) != "v1" {
		t.Fatalf("got %q", b)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	h.mustTick()
	if info, _ := os.Stat(path); !info.ModTime().Equal(old) {
		t.Fatal("an unchanged file must not be rewritten: Envoy would reload it")
	}
	h.sched.assignment.Files = map[string]string{"edge/routes.yaml": "v2"}
	h.mustTick()
	if b, _ := os.ReadFile(path); string(b) != "v2" {
		t.Fatalf("got %q", b)
	}
	if leftovers := fileList(t, h.cfg.FilesDir); !slices.Equal(leftovers, []string{"edge/routes.yaml"}) {
		t.Fatalf("files %v: writes must be atomic, with no temporary files left", leftovers)
	}
}

func TestFilesLeftOutAreRemoved(t *testing.T) {
	h := newHarness(t)
	h.sched.assignment.Files = map[string]string{"edge/routes.yaml": "r", "edge/extra.yaml": "x"}
	h.mustTick()
	h.sched.assignment.Files = map[string]string{"edge/routes.yaml": "r"}
	h.mustTick()
	if got := fileList(t, h.cfg.FilesDir); !slices.Equal(got, []string{"edge/routes.yaml"}) {
		t.Fatalf("files %v", got)
	}
}

func TestFilesCannotEscapeTheirDirectory(t *testing.T) {
	h := newHarness(t)
	outside := filepath.Join(filepath.Dir(h.cfg.FilesDir), "escaped")
	h.sched.assignment.Files = map[string]string{
		"../escaped": "x", "/etc/passwd": "x", "edge/../../escaped": "x", "edge/ok.yaml": "fine",
	}
	h.mustTick()
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a file escaped the files directory")
	}
	if got := fileList(t, h.cfg.FilesDir); !slices.Equal(got, []string{"edge/ok.yaml"}) {
		t.Fatalf("files %v", got)
	}
}

func TestFilesAreNotTouchedWhenTheSchedulerIsDown(t *testing.T) {
	h := newHarness(t)
	h.sched.assignment.Files = map[string]string{"edge/routes.yaml": "r"}
	h.mustTick()
	h.sched.syncErr = errors.New("down")
	h.tick()
	if got := fileList(t, h.cfg.FilesDir); !slices.Equal(got, []string{"edge/routes.yaml"}) {
		t.Fatalf("the routes must survive the scheduler being down: %v", got)
	}
}
