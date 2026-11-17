package bot

import (
	"context"
	"iter"
	"sync"
	"testing"
	"time"

	disgobot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/sharding"
)

// lockingShardManager mimics disgo's shard manager locking: Shards() holds a
// plain (non-reentrant) mutex while the caller's loop runs, and Shard() takes
// the same mutex. Calling Shard() inside a range over Shards() deadlocks.
type lockingShardManager struct {
	sharding.ShardManager
	mu     sync.Mutex
	shards []gateway.Gateway
}

func (m *lockingShardManager) Shards() iter.Seq[gateway.Gateway] {
	return func(yield func(gateway.Gateway) bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, s := range m.shards {
			if !yield(s) {
				return
			}
		}
	}
}

// Shard returns no shard, so SetPresenceForShard stops before sending.
func (m *lockingShardManager) Shard(int) gateway.Gateway {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil
}

type fakeShard struct {
	gateway.Gateway
	id  int
	lat time.Duration
}

func (s fakeShard) ShardID() int           { return s.id }
func (s fakeShard) Latency() time.Duration { return s.lat }

// Regression: rotateStatus used to call SetPresenceForShard inside a range
// over Shards(), deadlocking on the shard manager's lock, after which /about
// (latency) and shutdown blocked forever.
func TestShardLoopsDoNotDeadlock(t *testing.T) {
	sm := &lockingShardManager{shards: []gateway.Gateway{
		fakeShard{id: 0, lat: 40 * time.Millisecond},
		fakeShard{id: 1, lat: 60 * time.Millisecond},
	}}
	b := &Bot{Client: &disgobot.Client{ShardManager: sm}}
	tr := &Tracking{b: b, ctx: context.Background()}

	done := make(chan time.Duration, 1)
	go func() {
		tr.rotateStatus()
		done <- b.latency()
	}()
	select {
	case lat := <-done:
		if lat != 50*time.Millisecond {
			t.Errorf("latency() = %v, want 50ms (average of the shards)", lat)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: rotateStatus/latency still blocked on the shard manager lock")
	}
}
