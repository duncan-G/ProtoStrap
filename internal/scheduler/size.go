package scheduler

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/spec"
)

// poolShape is what sizing assumes of a pool: its first VM type, and its settings.
type poolShape struct {
	vm       api.Resources
	reserved api.Resources
	target   float64
	spare    int
	min, max int
}

// size is how many VMs a pool should have for the CPU and memory requested of it
// (README.md#scaling-vms):
//
//	usable per VM = (VM capacity − reserved) × target
//	needed        = max(ceil(CPU requested / usable CPU), ceil(memory requested / usable memory))
//	VMs           = clamp(needed + spare, min, max)
func (p poolShape) size(requested api.Resources) int {
	usableCPU := float64(p.vm.CPU-p.reserved.CPU) * p.target
	usableMem := float64(p.vm.Memory-p.reserved.Memory) * p.target
	need := 0
	if requested.CPU > 0 && usableCPU > 0 {
		need = max(need, ceil(float64(requested.CPU)/usableCPU))
	}
	if requested.Memory > 0 && usableMem > 0 {
		need = max(need, ceil(float64(requested.Memory)/usableMem))
	}
	return min(max(need+p.spare, p.min), p.max)
}

// room is one empty VM's room for replicas.
func (p poolShape) room() api.Resources {
	return api.Resources{CPU: p.vm.CPU - p.reserved.CPU, Memory: p.vm.Memory - p.reserved.Memory}
}

func (s *Scheduler) shape(name string) (poolShape, bool) {
	sp := s.st.target.spec.Pool(name)
	if sp == nil || len(sp.VM) == 0 {
		return poolShape{}, false
	}
	t, err := spec.LookupVM(sp.VM[0])
	if err != nil {
		return poolShape{}, false
	}
	return poolShape{vm: api.Resources{CPU: t.CPU, Memory: t.Memory}, reserved: sp.Reserved.API(),
		target: float64(sp.Target), spare: sp.Spare, min: sp.Min, max: sp.Max}, true
}

// poolState is a pool's size this cycle, for the status.
type poolState struct {
	vms, booting, desired, wanted int
	requested                     api.Resources
}

// size sizes the general and compute pools (docs/vms.md#scaling-with-auto-scaling-groups):
// each runs whichever is larger, the VMs sizing wants or the VMs placing its pending
// replicas needs, counting VMs still booting with all their room. It adds VMs by raising a
// group's desired capacity. It removes them only on sizing, slowly: once sizing has wanted
// fewer for ScaleInAfter, it drains the emptiest VM whose replicas fit on the others, then
// terminates it, one at a time. It also cancels launches that can't be filled, and marks
// VMs whose agents stay silent unhealthy.
func (s *Scheduler) size(now time.Time) []action {
	var acts []action
	if s.st.target != nil {
		for _, pool := range []string{"general", "compute"} {
			acts = append(acts, s.sizePool(pool, now)...)
		}
	}
	return append(acts, s.unhealthy(now)...)
}

func (s *Scheduler) groupsOf(pool string) []*Group {
	var out []*Group
	for i := range s.st.groups {
		if s.st.groups[i].Pool == pool {
			out = append(out, &s.st.groups[i])
		}
	}
	return out
}

func (s *Scheduler) sizePool(pool string, now time.Time) []action {
	shape, ok := s.shape(pool)
	if !ok {
		return nil
	}
	groups := s.groupsOf(pool)
	if len(groups) == 0 {
		return nil
	}
	var acts []action

	// Launches that have gone unfilled too long are cancelled. Compute grows On-Demand for
	// a while instead.
	for _, g := range groups {
		if g.Desired <= len(g.Instances) {
			delete(s.st.unfilledSince, g.Name)
			continue
		}
		since, ok := s.st.unfilledSince[g.Name]
		if !ok {
			s.st.unfilledSince[g.Name] = now
			continue
		}
		if now.Sub(since) < s.cfg.LaunchTimeout {
			continue
		}
		s.log.Warn("cancelled launches that couldn't be filled", "group", g.Name, "desired", g.Desired, "instances", len(g.Instances), "for", now.Sub(since).Round(time.Second))
		acts = append(acts, action{kind: "desired", group: g.Name, desired: len(g.Instances)})
		g.Desired = len(g.Instances)
		delete(s.st.unfilledSince, g.Name)
		if g.Spot {
			s.st.spotRetryAt = now.Add(s.cfg.SpotRetry)
		}
	}

	members := map[string]bool{}
	desired := 0
	for _, g := range groups {
		desired += g.Desired
		for _, in := range g.Instances {
			members[in.ID] = true
		}
	}
	var live []*VM
	synced := 0
	for _, id := range slices.Sorted(maps.Keys(s.st.vms)) {
		vm := s.st.vms[id]
		if vm.Pool != pool || vm.Lost || vm.Terminated {
			continue
		}
		live = append(live, vm)
		if members[id] {
			synced++
		}
	}
	booting := max(0, desired-synced)

	var requested api.Resources
	var waiting []*Replica
	for _, r := range s.st.replicas {
		if r.Pool != pool || r.Ephemeral || !r.live() {
			continue
		}
		requested.CPU += r.Request.CPU
		requested.Memory += r.Request.Memory
		if r.VM == "" {
			waiting = append(waiting, r)
		}
	}
	sizing := shape.size(requested)
	placing := len(live) + booting + extraVMs(waiting, booting, shape)
	wanted := min(max(sizing, placing, shape.min), shape.max)
	s.st.pools[pool] = poolState{vms: len(live), booting: booting, desired: desired, wanted: wanted, requested: requested}

	switch {
	case wanted > desired:
		g := s.growGroup(pool, groups, now)
		to := min(g.Desired+wanted-desired, g.Max)
		if to > g.Desired {
			why := "sizing"
			if placing > sizing {
				why = "replicas waiting for room"
			}
			s.log.Info("adding VMs", "pool", pool, "group", g.Name, "from", g.Desired, "to", to, "reason", why, "requested_cpu", requested.CPU, "waiting", len(waiting))
			acts = append(acts, action{kind: "desired", group: g.Name, desired: to})
			g.Desired = to
		}
		delete(s.st.wantFewerSince, pool)
	case sizing < len(live) && len(waiting) == 0 && booting == 0:
		since, ok := s.st.wantFewerSince[pool]
		if !ok {
			s.st.wantFewerSince[pool] = now
			break
		}
		if now.Sub(since) < s.cfg.ScaleInAfter || slices.ContainsFunc(live, func(vm *VM) bool { return vm.Cordoned }) {
			break
		}
		if vm := s.emptiestMovable(pool, live, groups); vm != nil {
			vm.Cordoned = true
			s.log.Info("removing a VM: moving its replicas off first", "vm", vm.ID, "pool", pool, "sizing", sizing, "vms", len(live),
				"reason", "sizing has wanted fewer VMs for "+now.Sub(since).Round(time.Second).String())
		}
	default:
		delete(s.st.wantFewerSince, pool)
	}

	// A drained VM is terminated, lowering its group's capacity in the same call.
	for _, vm := range live {
		if !vm.Cordoned || vm.Terminated {
			continue
		}
		empty := true
		for _, r := range s.st.replicas {
			if r.VM == vm.ID {
				empty = false
				break
			}
		}
		if empty {
			vm.Terminated = true
			s.log.Info("VM drained: terminating it", "vm", vm.ID, "pool", pool)
			acts = append(acts, action{kind: "terminate", instance: vm.ID})
		}
	}
	return acts
}

// growGroup is the group a pool grows: general, the zone with the fewest VMs; compute, its
// Spot group, unless Spot recently had no capacity.
func (s *Scheduler) growGroup(pool string, groups []*Group, now time.Time) *Group {
	cands := slices.Clone(groups)
	if pool == "compute" {
		spot := now.After(s.st.spotRetryAt)
		filtered := slices.DeleteFunc(slices.Clone(cands), func(g *Group) bool { return g.Spot != spot })
		if len(filtered) > 0 {
			cands = filtered
		}
	}
	slices.SortFunc(cands, func(a, b *Group) int {
		return cmp.Or(cmp.Compare(a.Desired, b.Desired), strings.Compare(a.Name, b.Name))
	})
	return cands[0]
}

// emptiestMovable is the VM to remove: the one with the least requested whose replicas fit
// on the other VMs; on compute, On-Demand VMs before Spot ones.
func (s *Scheduler) emptiestMovable(pool string, live []*VM, groups []*Group) *VM {
	onDemand := map[string]bool{}
	for _, g := range groups {
		if !g.Spot {
			for _, in := range g.Instances {
				onDemand[in.ID] = true
			}
		}
	}
	rooms := s.rooms()
	used := func(vm *VM) int64 {
		var cpu int64
		for _, r := range s.st.replicas {
			if r.VM == vm.ID {
				cpu += r.Request.CPU
			}
		}
		return cpu
	}
	cands := slices.Clone(live)
	slices.SortFunc(cands, func(a, b *VM) int {
		if pool == "compute" && onDemand[a.ID] != onDemand[b.ID] {
			if onDemand[a.ID] {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(used(a), used(b)), strings.Compare(a.ID, b.ID))
	})
	for _, vm := range cands {
		if !placeable(vm) {
			continue
		}
		var mine []*Replica
		for _, r := range s.st.replicas {
			if r.VM == vm.ID && r.live() {
				mine = append(mine, r)
			}
		}
		var others []*room
		for _, o := range live {
			if o != vm && placeable(o) {
				c := *rooms[o.ID]
				c.perComponent = maps.Clone(c.perComponent)
				others = append(others, &c)
			}
		}
		if len(packAll(mine, others)) == len(mine) {
			return vm
		}
	}
	return nil
}

// extraVMs is how many more VMs of the pool's shape it would take to place the waiting
// replicas, after filling the VMs still booting.
func extraVMs(waiting []*Replica, booting int, shape poolShape) int {
	if len(waiting) == 0 {
		return 0
	}
	empty := shape.room()
	var bins []api.Resources
	for range booting {
		bins = append(bins, empty)
	}
	rs := slices.Clone(waiting)
	sortBiggestFirst(rs)
	extra := 0
	for _, r := range rs {
		if !fits(r.Request, empty) {
			continue // The replica fits no VM of this shape, so more VMs won't help.
		}
		placed := false
		for i := range bins {
			if fits(r.Request, bins[i]) {
				bins[i].CPU -= r.Request.CPU
				bins[i].Memory -= r.Request.Memory
				placed = true
				break
			}
		}
		if !placed {
			bins = append(bins, api.Resources{CPU: empty.CPU - r.Request.CPU, Memory: empty.Memory - r.Request.Memory})
			extra++
		}
	}
	return extra
}

// unhealthy marks VMs whose agents have been silent for UnhealthyAfter, so their groups
// replace them (docs/vms.md#health): at most one per pool every UnhealthyEvery, and none
// when more than half a pool went silent together, which points at the scheduler's side.
// A data VM is never marked.
func (s *Scheduler) unhealthy(now time.Time) []action {
	var acts []action
	for _, pool := range []string{"ingress", "general", "compute"} {
		var all, silent []*VM
		for _, id := range slices.Sorted(maps.Keys(s.st.vms)) {
			vm := s.st.vms[id]
			if vm.Pool != pool || vm.Terminated || vm.Unhealthy {
				continue
			}
			all = append(all, vm)
			if vm.Lost {
				silent = append(silent, vm)
			}
		}
		if len(silent) == 0 {
			continue
		}
		if len(silent)*2 > len(all) {
			if key := "half-silent/" + pool; !s.st.reported[key] {
				s.st.reported[key] = true
				s.log.Error("more than half of a pool is silent: not replacing its VMs, since the cause is likely on the scheduler's side", "pool", pool, "silent", len(silent), "vms", len(all))
			}
			continue
		}
		delete(s.st.reported, "half-silent/"+pool)
		if last, ok := s.st.lastUnhealthy[pool]; ok && now.Sub(last) < s.cfg.UnhealthyEvery {
			continue
		}
		slices.SortFunc(silent, func(a, b *VM) int { return a.LastSync.Compare(b.LastSync) })
		vm := silent[0]
		if now.Sub(vm.LastSync) < s.cfg.UnhealthyAfter {
			continue
		}
		vm.Unhealthy = true
		s.st.lastUnhealthy[pool] = now
		s.log.Warn("marking a silent VM unhealthy, so its group replaces it", "vm", vm.ID, "pool", pool, "silent_for", now.Sub(vm.LastSync).Round(time.Second))
		acts = append(acts, action{kind: "unhealthy", instance: vm.ID})
	}
	return acts
}
