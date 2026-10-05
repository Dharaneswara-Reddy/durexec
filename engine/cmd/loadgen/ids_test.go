package main

import (
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/shard"
)

// TestIDGenHashesToRequestedShards is the test that keeps "-shards 2" honest.
//
// It re-derives the shard for every generated id with the engine's own
// ShardFor and insists it matches what the generator promised. If the
// generator ever drifted from the engine's hash rule, every throughput number
// this tool produced would be attributed to the wrong shards.
func TestIDGenHashesToRequestedShards(t *testing.T) {
	tests := []struct {
		name       string
		shardCount int32
		targets    []core.ShardID
		draws      int
	}{
		{"single shard of sixteen", 16, []core.ShardID{0}, 200},
		{"two shards of sixteen", 16, []core.ShardID{0, 1}, 200},
		{"four shards of sixteen", 16, []core.ShardID{3, 7, 11, 15}, 400},
		{"every shard of four", 4, []core.ShardID{0, 1, 2, 3}, 200},
		{"highest shard only", 16, []core.ShardID{15}, 100},
		{"single shard cluster", 1, []core.ShardID{0}, 50},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gen, err := NewIDGen("t", tc.shardCount, tc.targets)
			if err != nil {
				t.Fatalf("NewIDGen: %v", err)
			}

			hits := map[core.ShardID]int{}
			seen := map[string]bool{}
			for i := 0; i < tc.draws; i++ {
				id, promised, err := gen.Next()
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if seen[id] {
					t.Fatalf("id %q was handed out twice", id)
				}
				seen[id] = true

				actual := shard.ShardFor(id, tc.shardCount)
				if actual != promised {
					t.Fatalf("id %q: generator promised shard %d, ShardFor says %d", id, promised, actual)
				}
				if !contains(tc.targets, actual) {
					t.Fatalf("id %q landed on shard %d, which is not in the target set %v", id, actual, tc.targets)
				}
				hits[actual]++
			}

			// Round-robin means every target must have been used, otherwise
			// "-shards 4" would quietly drive load at fewer than four.
			for _, want := range tc.targets {
				if hits[want] == 0 {
					t.Errorf("shard %d never received an id", want)
				}
			}
		})
	}
}

func TestIDGenNextForPinsOneShard(t *testing.T) {
	gen, err := NewIDGen("probe", 16, []core.ShardID{0, 5, 9})
	if err != nil {
		t.Fatalf("NewIDGen: %v", err)
	}
	for _, want := range []core.ShardID{0, 5, 9} {
		for i := 0; i < 50; i++ {
			id, err := gen.NextFor(want)
			if err != nil {
				t.Fatalf("NextFor(%d): %v", want, err)
			}
			if got := shard.ShardFor(id, 16); got != want {
				t.Fatalf("NextFor(%d) produced %q which hashes to %d", want, id, got)
			}
		}
	}
}

func TestNewIDGenRejectsBadInput(t *testing.T) {
	tests := []struct {
		name       string
		shardCount int32
		targets    []core.ShardID
	}{
		{"zero shard count", 0, []core.ShardID{0}},
		{"negative shard count", -4, []core.ShardID{0}},
		{"no targets", 16, nil},
		{"target above range", 16, []core.ShardID{16}},
		{"negative target", 16, []core.ShardID{-1}},
		{"one good one bad", 16, []core.ShardID{0, 99}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewIDGen("x", tc.shardCount, tc.targets); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// TestSelectTargets covers the choice of which shards to aim at, including the
// spread rule that stops a "2 shard" run from landing on one machine.
func TestSelectTargets(t *testing.T) {
	owners := map[core.ShardID]string{
		0: "node-a", 1: "node-a", 2: "node-a",
		3: "node-b", 4: "node-b",
		5: "node-c",
	}

	tests := []struct {
		name     string
		wanted   int
		explicit string
		owners   map[core.ShardID]string
		spread   bool
		want     []core.ShardID
		wantErr  bool
	}{
		{name: "one shard, no spreading needed", wanted: 1, owners: owners, spread: true, want: []core.ShardID{0}},
		{name: "two shards land on two nodes", wanted: 2, owners: owners, spread: true, want: []core.ShardID{0, 3}},
		{name: "three shards land on three nodes", wanted: 3, owners: owners, spread: true, want: []core.ShardID{0, 3, 5}},
		{name: "more shards than nodes fills from the rest", wanted: 4, owners: owners, spread: true, want: []core.ShardID{0, 3, 5, 1}},
		{name: "spreading off takes the first n", wanted: 2, owners: owners, spread: false, want: []core.ShardID{0, 1}},
		{name: "no ownership known takes the first n", wanted: 2, owners: nil, spread: true, want: []core.ShardID{0, 1}},
		{name: "explicit list wins", wanted: 1, explicit: "4,9", owners: owners, spread: true, want: []core.ShardID{4, 9}},
		{name: "explicit out of range", explicit: "99", owners: owners, wantErr: true},
		{name: "explicit not a number", explicit: "a,b", owners: owners, wantErr: true},
		{name: "more shards than the cluster has", wanted: 99, owners: owners, spread: true, wantErr: true},
		{name: "zero shards with no explicit list", wanted: 0, owners: owners, spread: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectTargets(tc.wanted, tc.explicit, 16, tc.owners, tc.spread)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectTargets: %v", err)
			}
			if !equalShards(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDistinctOwners(t *testing.T) {
	owners := map[core.ShardID]string{0: "node-a", 1: "node-a", 3: "node-b"}
	tests := []struct {
		name    string
		targets []core.ShardID
		want    int
	}{
		{"both on one node", []core.ShardID{0, 1}, 1},
		{"two nodes", []core.ShardID{0, 3}, 2},
		{"unowned shard is not an owner", []core.ShardID{0, 7}, 1},
		{"nothing owned", []core.ShardID{7, 8}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(distinctOwners(tc.targets, owners)); got != tc.want {
				t.Fatalf("got %d distinct owners, want %d", got, tc.want)
			}
		})
	}
}

func contains(ss []core.ShardID, s core.ShardID) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func equalShards(a, b []core.ShardID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
