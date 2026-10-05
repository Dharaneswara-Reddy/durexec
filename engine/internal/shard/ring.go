package shard

import (
	"hash/fnv"
	"sort"
	"strconv"
	"sync"
)

// DefaultVNodes is how many points each node gets on the ring.
//
// Why not one point per node? With one point each, the gaps between nodes are
// wildly uneven — one node might land next to another and own almost nothing,
// while a third owns a third of the ring. Scattering each node across many
// points averages those gaps out, so every node ends up with roughly the same
// share. 150 is the usual figure: it brings the imbalance down to a few percent
// while keeping the sorted point list small enough to binary-search instantly.
const DefaultVNodes = 150

// Ring is a consistent hash ring over node names.
//
// The ordinary way to spread keys over machines is "hash the key, then take the
// remainder after dividing by the number of machines". That works until the
// number of machines changes, at which point almost every key gets a different
// answer.
//
// A consistent hash ring avoids that. Imagine a clock face. Every node is
// placed at several positions around the face. To find the owner of a key, you
// hash the key to a position on the face and then walk clockwise until you meet
// a node. Removing a node only affects the keys that sat immediately behind it;
// everything else still meets the same node it met before. Adding a node only
// steals the keys immediately behind its new positions. So a cluster change
// moves roughly one node's worth of keys instead of nearly all of them.
//
// A Ring is a pure function of the set of node names it holds. Two rings built
// from the same names with the same vnode count are byte-for-byte identical
// regardless of the order the names were added, which is what makes it safe for
// two nodes to compute ownership independently and still agree.
//
// It is also safe for a Ring to be out of date. A node acting on a stale ring
// can only ever try to take a lease it is not entitled to, and the lease table
// will refuse it. The ring is an optimisation for who should try; the lease is
// the authority on who actually may.
type Ring struct {
	mu     sync.RWMutex
	vnodes int
	// members is the authoritative set. points is derived from it.
	members map[string]struct{}
	points  []point
}

// point is one virtual position of one node on the ring.
type point struct {
	hash uint64
	node string
}

// NewRing builds a ring over nodes. Duplicate names are collapsed. A vnodes
// value of zero or less means DefaultVNodes.
func NewRing(nodes []string, vnodes int) *Ring {
	if vnodes <= 0 {
		vnodes = DefaultVNodes
	}
	r := &Ring{vnodes: vnodes, members: make(map[string]struct{}, len(nodes))}
	for _, n := range nodes {
		r.members[n] = struct{}{}
	}
	r.rebuild()
	return r
}

// Add puts a node into the ring. Adding a node that is already present is a
// no-op, so callers can hand the ring a full membership list repeatedly.
func (r *Ring) Add(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.members[node]; ok {
		return
	}
	r.members[node] = struct{}{}
	r.rebuild()
}

// Remove takes a node out of the ring. Removing an absent node is a no-op.
func (r *Ring) Remove(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.members[node]; !ok {
		return
	}
	delete(r.members, node)
	r.rebuild()
}

// rebuild regenerates the whole point list from the membership set. It is
// deliberately not an incremental insert: recomputing from scratch is what
// guarantees the ring depends only on *which* nodes are present and not on the
// order they arrived in. The cost is trivial — a few thousand hashes — and it
// is paid only on a cluster membership change, which is a rare event.
//
// Caller must hold the write lock.
func (r *Ring) rebuild() {
	names := make([]string, 0, len(r.members))
	for n := range r.members {
		names = append(names, n)
	}
	sort.Strings(names)

	pts := make([]point, 0, len(names)*r.vnodes)
	for _, n := range names {
		for i := 0; i < r.vnodes; i++ {
			pts = append(pts, point{hash: hashRing(n + "#" + strconv.Itoa(i)), node: n})
		}
	}
	sort.Slice(pts, func(i, j int) bool {
		if pts[i].hash != pts[j].hash {
			return pts[i].hash < pts[j].hash
		}
		// Ties are broken by name so the ordering is total and reproducible.
		// Without this, two nodes could sort an identical collision differently
		// and disagree about ownership.
		return pts[i].node < pts[j].node
	})
	r.points = pts
}

// Lookup returns the node that owns key, or "" if the ring is empty.
//
// This is the clockwise walk: find the first ring point whose hash is greater
// than or equal to the key's hash, and if the key hashes past the very last
// point, wrap around to the first one. The wrap is what makes it a ring rather
// than a line, and it is why the highest-hashing keys belong to the
// lowest-positioned node.
func (r *Ring) Lookup(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.points) == 0 {
		return ""
	}
	h := hashRing(key)
	// sort.Search returns the index of the first point with hash >= h, or
	// len(points) when every point is below h.
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i].hash >= h })
	if i == len(r.points) {
		i = 0 // wrapped past the top of the ring
	}
	return r.points[i].node
}

// Nodes returns the ring's membership in sorted order. The slice is a copy, so
// the caller can keep or mutate it freely.
func (r *Ring) Nodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.members))
	for n := range r.members {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Len reports the number of nodes in the ring.
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.members)
}

// hashRing places a name on the ring. A 64-bit hash is used here even though
// ShardFor uses 32 bits: the ring holds hundreds of points per node, and a
// wider space makes accidental collisions between two points vanishingly rare.
func hashRing(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return avalanche(h.Sum64())
}

// avalanche spreads a one-bit change in the input across all 64 output bits.
//
// It exists because raw FNV-1a is the wrong shape for a hash ring. FNV-1a mixes
// upward only: each byte is XORed into the low bits and then multiplied, so the
// high bits are decided almost entirely by the length of the string and its
// first few characters. Ring position is ordered by the whole 64-bit value,
// which those high bits dominate.
//
// The consequence, measured on this engine's own keys before the fix: the
// sixteen shard names "shard-0".."shard-15" landed in exactly two tight
// clusters — seven-character names at 0x8ae4…, eight-character names at
// 0x6749… — each spanning about 1e10 in a space of 1.8e19. For ring purposes
// that is two points, not sixteen. Only two nodes ever owned anything, and
// adding a fourth node to a three-node cluster moved zero shards: the precise
// opposite of the property a consistent hash ring exists to provide.
//
// These are splitmix64's finalising constants. Applying them costs a handful of
// nanoseconds per ring point, built once at construction, and restores the
// avalanche property the ring assumes it is being given.
func avalanche(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
