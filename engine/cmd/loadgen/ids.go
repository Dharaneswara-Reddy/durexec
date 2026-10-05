package main

import (
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/shard"
)

// maxIDAttempts bounds the search for an id that lands on a wanted shard.
//
// The expected cost is one attempt per shard in the cluster — sixteen tries in
// the default deployment — because FNV-1a spreads ids evenly over the residues.
// The bound exists so that a mistake elsewhere (a shard id outside the range,
// a hash that stopped being surjective) surfaces as an error instead of an
// invisible hang inside a benchmark.
const maxIDAttempts = 1 << 20

// IDGen produces workflow ids that are guaranteed to land on chosen shards.
//
// This is the part of the tool that makes "-shards 2" mean anything. A
// workflow's shard is decided by hashing its business key, and the engine does
// not accept a shard id from the client, so the only way to aim load at a
// particular shard is to hand the engine a key that hashes there. The hash
// rule is imported from internal/shard rather than copied: a second
// implementation of FNV-1a-mod-N that drifted by one character would send
// every request to the wrong shard while reporting perfect compliance.
type IDGen struct {
	prefix     string
	shardCount int32
	targets    []core.ShardID

	counter atomic.Uint64
	cursor  atomic.Uint64
}

// NewIDGen builds a generator for the given target shards.
func NewIDGen(prefix string, shardCount int32, targets []core.ShardID) (*IDGen, error) {
	if shardCount <= 0 {
		return nil, fmt.Errorf("shard count must be positive, got %d", shardCount)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one target shard is required")
	}
	for _, t := range targets {
		if t < 0 || int32(t) >= shardCount {
			return nil, fmt.Errorf("target shard %d is outside 0..%d", t, shardCount-1)
		}
	}
	return &IDGen{prefix: prefix, shardCount: shardCount, targets: targets}, nil
}

// Next returns the next id, cycling evenly through the target shards so the
// load is spread over all of them rather than piling onto the first.
func (g *IDGen) Next() (string, core.ShardID, error) {
	t := g.targets[int(g.cursor.Add(1)-1)%len(g.targets)]
	id, err := g.NextFor(t)
	return id, t, err
}

// NextFor returns the next id that hashes to target.
func (g *IDGen) NextFor(target core.ShardID) (string, error) {
	for i := 0; i < maxIDAttempts; i++ {
		n := g.counter.Add(1)
		id := g.prefix + "-" + strconv.FormatUint(n, 10)
		if shard.ShardFor(id, g.shardCount) == target {
			return id, nil
		}
	}
	return "", fmt.Errorf("no id hashing to shard %d found in %d attempts with prefix %q",
		target, maxIDAttempts, g.prefix)
}

// Targets returns the shards this generator aims at.
func (g *IDGen) Targets() []core.ShardID { return g.targets }
