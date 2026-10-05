// Package shard decides two separate things, and keeping them separate is the
// point of the package.
//
// The first is which shard a workflow belongs to. That answer is fixed for the
// life of the workflow and never changes, no matter how many machines are
// running.
//
// The second is which machine currently owns a shard. That answer is allowed to
// change every time a node joins or dies.
//
// Why bother with two steps instead of hashing a workflow straight onto a
// machine? Because a workflow's history is an append-only log, and a log has to
// stay in one place. If workflows were hashed directly to nodes, then adding a
// single machine would recompute the answer for a huge fraction of workflows,
// and each one would have to have its log physically moved to a different
// database. The shard is a stable middleman: the workflow's log lives with its
// shard forever, and a cluster change only reassigns which node reads that log.
// Rebalancing becomes "hand over a lease", not "copy a database".
package shard

import (
	"hash/fnv"
	"strconv"

	"github.com/dharaneswar339/durexec/internal/core"
)

// DefaultShardCount is the number of shards a cluster is deployed with. It is
// fixed at deploy time and must never change afterwards: changing it would
// change the answer ShardFor gives for existing workflows, which would strand
// their history in the wrong log.
const DefaultShardCount int32 = 16

// ShardFor maps a workflow's business key onto its permanent shard.
//
// FNV-1a is used rather than a cryptographic hash because there is no adversary
// here — we only need the output spread evenly and, far more importantly, to be
// identical on every node and in every future version of the binary. FNV-1a is
// a fixed, tiny, standard-library algorithm, so it will still return the same
// number years from now. That stability is the actual requirement.
//
// Panics if n <= 0, because a zero shard count is a deployment mistake that
// would otherwise silently corrupt routing.
func ShardFor(workflowID string, n int32) core.ShardID {
	if n <= 0 {
		panic("shard: shard count must be positive")
	}
	h := fnv.New32a()
	// Write on a hash.Hash never returns an error; the interface only has one
	// for io.Writer compatibility.
	_, _ = h.Write([]byte(workflowID))
	// Modulo on the unsigned sum keeps the result in [0, n).
	return core.ShardID(int32(h.Sum32() % uint32(n)))
}

// Key is the string a shard is looked up by on the hash ring. It is a stable,
// human-readable name rather than the raw integer so that ring positions do not
// accidentally collide with node names that happen to be numeric.
func Key(s core.ShardID) string {
	return "shard-" + strconv.FormatInt(int64(s), 10)
}

// All returns every shard id in a cluster of n shards, in ascending order.
func All(n int32) []core.ShardID {
	out := make([]core.ShardID, 0, n)
	for i := int32(0); i < n; i++ {
		out = append(out, core.ShardID(i))
	}
	return out
}
