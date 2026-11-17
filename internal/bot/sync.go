package bot

import (
	"sync"
)

// idSet is a concurrent set of IDs (opt-out lists, guilds with watches).
type idSet struct {
	mu sync.RWMutex
	m  map[int64]bool
}

func newIDSet(m map[int64]bool) *idSet {
	if m == nil {
		m = map[int64]bool{}
	}
	return &idSet{m: m}
}

func (s *idSet) Has(id int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[id]
}

func (s *idSet) Add(id int64) {
	s.mu.Lock()
	s.m[id] = true
	s.mu.Unlock()
}

func (s *idSet) Remove(id int64) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

func (s *idSet) Replace(m map[int64]bool) {
	s.mu.Lock()
	s.m = m
	s.mu.Unlock()
}

type memberKey struct{ guild, user int64 }

// memberOrder runs each member's join/leave/update DB work in the order the
// events arrived, one at a time per member (different members run in
// parallel). Events are dispatched in gateway order, so chaining each task
// after the previous one for the same member preserves that order even
// though the work itself runs off the gateway goroutine.
type memberOrder struct {
	mu    sync.Mutex
	tails map[memberKey]chan struct{}
}

func newMemberOrder() *memberOrder { return &memberOrder{tails: map[memberKey]chan struct{}{}} }

// Go runs fn after every earlier task for the same member has finished.
func (o *memberOrder) Go(k memberKey, fn func()) {
	o.mu.Lock()
	prev := o.tails[k]
	done := make(chan struct{})
	o.tails[k] = done
	o.mu.Unlock()
	go func() {
		defer func() {
			close(done)
			o.mu.Lock()
			if o.tails[k] == done {
				delete(o.tails, k)
			}
			o.mu.Unlock()
		}()
		if prev != nil {
			<-prev
		}
		fn()
	}()
}

// Pending reports whether work for the member is queued or running.
func (o *memberOrder) Pending(k memberKey) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.tails[k]
	return ok
}
