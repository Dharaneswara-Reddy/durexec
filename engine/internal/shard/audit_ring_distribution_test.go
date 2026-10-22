package shard

// Added by the production-readiness audit (docs/audit/01-correctness-fault-tolerance.md).
// Read-only: it asserts nothing, it prints the ring's actual shard distribution
// and the number of shards that move on a membership change, so the claims in
// docs/results/ can be checked against the code rather than taken on trust.

import (
	"fmt"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

func TestAuditRingDistributionAndMovement(t *testing.T) {
	const n int32 = 16
	three := []string{"node-a", "node-b", "node-c"}
	four := []string{"node-a", "node-b", "node-c", "node-d"}

	r3 := NewRing(three, DefaultVNodes)
	r4 := NewRing(four, DefaultVNodes)

	own3 := map[string]int{}
	own4 := map[string]int{}
	var movedList []int
	for s := int32(0); s < n; s++ {
		a := r3.Lookup(Key(core.ShardID(s)))
		b := r4.Lookup(Key(core.ShardID(s)))
		own3[a]++
		own4[b]++
		if a != b {
			movedList = append(movedList, int(s))
		}
	}
	fmt.Printf("AUDIT 3-node distribution over %d shards: %v\n", n, own3)
	fmt.Printf("AUDIT 4-node distribution over %d shards: %v\n", n, own4)
	fmt.Printf("AUDIT shards that move when node-d is added: %d of %d -> %v\n",
		len(movedList), n, movedList)

	for _, dead := range three {
		rest := make([]string, 0, 2)
		for _, x := range three {
			if x != dead {
				rest = append(rest, x)
			}
		}
		rr := NewRing(rest, DefaultVNodes)
		m := 0
		d2 := map[string]int{}
		for s := int32(0); s < n; s++ {
			if r3.Lookup(Key(core.ShardID(s))) != rr.Lookup(Key(core.ShardID(s))) {
				m++
			}
			d2[rr.Lookup(Key(core.ShardID(s)))]++
		}
		fmt.Printf("AUDIT remove %s: %d of %d shards move, remaining distribution %v\n",
			dead, m, n, d2)
	}
}
