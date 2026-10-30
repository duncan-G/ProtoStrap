package scheduler

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// room is a VM's free CPU and memory, and how many replicas of each component it runs.
type room struct {
	vm           *VM
	free         api.Resources
	perComponent map[string]int
}

func fits(r, free api.Resources) bool { return r.CPU <= free.CPU && r.Memory <= free.Memory }

func (rm *room) take(r *Replica) {
	rm.free.CPU -= r.Request.CPU
	rm.free.Memory -= r.Request.Memory
	rm.perComponent[r.Component]++
}

func (rm *room) give(r *Replica) {
	rm.free.CPU += r.Request.CPU
	rm.free.Memory += r.Request.Memory
	rm.perComponent[r.Component]--
}

// reserve is what a pool's VMs keep back for the OS, the agent and Docker.
func (s *Scheduler) reserve(pool string) api.Resources {
	if s.st.target == nil {
		return api.Resources{}
	}
	p := s.st.target.spec.Pools
	switch pool {
	case "ingress":
		return p.Ingress.Reserved.API()
	case "general":
		return p.General.Reserved.API()
	case "compute":
		if p.Compute != nil {
			return p.Compute.Reserved.API()
		}
	case "data":
		return p.Data.Reserved.API()
	}
	return api.Resources{}
}

// rooms is every VM's room: its capacity, less its pool's reserve and the requests of
// every replica on it, those being stopped included until they have gone.
func (s *Scheduler) rooms() map[string]*room {
	out := map[string]*room{}
	for id, vm := range s.st.vms {
		res := s.reserve(vm.Pool)
		out[id] = &room{vm: vm, free: api.Resources{CPU: vm.Capacity.CPU - res.CPU, Memory: vm.Capacity.Memory - res.Memory}, perComponent: map[string]int{}}
	}
	for _, r := range s.st.replicas {
		rm := out[r.VM]
		if rm == nil {
			continue
		}
		rm.free.CPU -= r.Request.CPU
		rm.free.Memory -= r.Request.Memory
		if r.live() {
			rm.perComponent[r.Component]++
		}
	}
	return out
}

// placeable reports whether new replicas may go on a VM.
func placeable(vm *VM) bool {
	return !vm.Lost && !vm.Cordoned && !vm.Terminated && vm.Terminating == nil && !vm.Unhealthy
}

// place gives every pending replica a VM (README.md#placing-replicas), biggest first, so
// they find room before small ones fragment the space. A worker of the compute pool goes
// to a general VM with room, as an ephemeral replica, when no compute VM has room. A
// service that fits nowhere on general takes the room of ephemeral workers there, which go
// back to waiting for compute.
func (s *Scheduler) place(now time.Time) {
	if s.st.target == nil {
		return
	}
	rooms := s.rooms()
	pool := func(name string) []*room {
		var out []*room
		for _, id := range slices.Sorted(maps.Keys(rooms)) {
			if rm := rooms[id]; rm.vm.Pool == name && placeable(rm.vm) {
				out = append(out, rm)
			}
		}
		return out
	}
	for _, r := range s.pending() {
		switch r.Pool {
		case "data":
			for _, vm := range s.st.vms {
				if vm.Pool == "data" && vm.Name == r.Pin && !vm.Lost {
					s.assign(r, rooms[vm.ID])
				}
			}
		case "ingress":
		default:
			rm := choose(pool(r.Pool), r)
			if rm == nil && r.Pool == "compute" {
				rm = choose(pool("general"), r)
			}
			if rm == nil && r.Pool == "general" && r.Kind != api.KindWorker {
				rm = s.preempt(pool("general"), r)
			}
			if rm != nil {
				s.assign(r, rm)
			}
		}
	}
}

// pending lists the replicas waiting for a VM, biggest request first.
func (s *Scheduler) pending() []*Replica {
	var out []*Replica
	for _, r := range s.st.replicas {
		if r.VM == "" && !r.Stop {
			out = append(out, r)
		}
	}
	sortBiggestFirst(out)
	return out
}

func sortBiggestFirst(rs []*Replica) {
	slices.SortFunc(rs, func(a, b *Replica) int {
		return cmp.Or(cmp.Compare(b.Request.CPU, a.Request.CPU), cmp.Compare(b.Request.Memory, a.Request.Memory), strings.Compare(a.ID, b.ID))
	})
}

func (s *Scheduler) assign(r *Replica, rm *room) {
	r.VM, r.State = rm.vm.ID, StateAssigned
	r.Ephemeral = r.Pool == "compute" && rm.vm.Pool == "general"
	rm.take(r)
	s.log.Info("replica placed", "replica", r.ID, "component", r.Component, "vm", rm.vm.ID, "pool", rm.vm.Pool, "ephemeral", r.Ephemeral)
}

// choose is the VM for a replica (README.md#placing-replicas): of those with room for its
// request, the one with the fewest replicas of its component, then the most free CPU.
func choose(rooms []*room, r *Replica) *room {
	var best *room
	for _, rm := range rooms {
		if !fits(r.Request, rm.free) {
			continue
		}
		if best == nil || better(rm, best, r.Component) {
			best = rm
		}
	}
	return best
}

func better(a, b *room, component string) bool {
	if ca, cb := a.perComponent[component], b.perComponent[component]; ca != cb {
		return ca < cb
	}
	if a.free.CPU != b.free.CPU {
		return a.free.CPU > b.free.CPU
	}
	return a.vm.ID < b.vm.ID
}

// packAll places replicas into rooms, biggest first, and returns where each went.
func packAll(rs []*Replica, rooms []*room) map[string]*room {
	rs = slices.Clone(rs)
	sortBiggestFirst(rs)
	out := map[string]*room{}
	for _, r := range rs {
		if rm := choose(rooms, r); rm != nil {
			rm.take(r)
			out[r.ID] = rm
		}
	}
	return out
}

// preempt finds a general VM where stopping ephemeral workers makes room for r, stops as
// few as it can, and returns the VM.
func (s *Scheduler) preempt(rooms []*room, r *Replica) *room {
	var best *room
	var bestEvict []*Replica
	for _, rm := range rooms {
		var workers []*Replica
		for _, w := range s.st.replicas {
			if w.VM == rm.vm.ID && w.Ephemeral && w.live() {
				workers = append(workers, w)
			}
		}
		sortBiggestFirst(workers)
		free := rm.free
		var evict []*Replica
		for _, w := range workers {
			if fits(r.Request, free) {
				break
			}
			free.CPU += w.Request.CPU
			free.Memory += w.Request.Memory
			evict = append(evict, w)
		}
		if !fits(r.Request, free) || len(evict) == 0 {
			continue
		}
		if best == nil || len(evict) < len(bestEvict) {
			best, bestEvict = rm, evict
		}
	}
	if best == nil {
		return nil
	}
	for _, w := range bestEvict {
		s.stop(w, "its room on a general VM is needed by "+r.Component+": it goes back to waiting for compute")
		best.give(w)
	}
	return best
}

// pickToStop chooses n replicas of one component to stop (README.md#removing-replicas):
// those not serving yet first, pending then starting; workers on compute VMs before those
// on general, so compute can return to zero; then from the VM with the most replicas of the
// component, so the rest stay spread; then from the emptiest VM, so it can be removed.
func pickToStop(cands []*Replica, rooms map[string]*room, n int) []*Replica {
	left := slices.Clone(cands)
	var out []*Replica
	stage := func(r *Replica) int {
		switch r.State {
		case StatePending:
			return 0
		case StateAssigned, api.StateStarting:
			return 1
		}
		return 2
	}
	for range n {
		if len(left) == 0 {
			break
		}
		best := 0
		for i := 1; i < len(left); i++ {
			a, b := left[i], left[best]
			if c := cmp.Compare(stage(a), stage(b)); c != 0 {
				if c < 0 {
					best = i
				}
				continue
			}
			if a.Ephemeral != b.Ephemeral {
				if !a.Ephemeral {
					best = i
				}
				continue
			}
			ra, rb := rooms[a.VM], rooms[b.VM]
			ca, cb, fa, fb := 0, 0, int64(0), int64(0)
			if ra != nil {
				ca, fa = ra.perComponent[a.Component], ra.free.CPU
			}
			if rb != nil {
				cb, fb = rb.perComponent[b.Component], rb.free.CPU
			}
			switch {
			case ca != cb:
				if ca > cb {
					best = i
				}
			case fa != fb:
				if fa > fb {
					best = i
				}
			case a.ID < b.ID:
				best = i
			}
		}
		r := left[best]
		left = slices.Delete(left, best, best+1)
		if rm := rooms[r.VM]; rm != nil {
			rm.give(r)
		}
		out = append(out, r)
	}
	return out
}
