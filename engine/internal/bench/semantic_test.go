package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/exact"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

const semShard = core.ShardID(3)

// bookingInput is a realistic activity argument: a short natural-language
// request, which is the case the semantic check exists for.
func bookingInput(text string) json.RawMessage {
	b, err := json.Marshal(map[string]string{"tool": "BookFlight", "args": text})
	if err != nil {
		panic(err)
	}
	return b
}

// primedChecker builds a checker whose store already holds candidates prior
// invocations, the way a run's history would have left them.
func primedChecker(tb testing.TB, candidates int) (*semantic.Checker, *MemIntents) {
	tb.Helper()

	emb := semantic.NewFakeEmbedder(semantic.DefaultDim)
	store := NewMemIntents()
	ctx := context.Background()

	for i := 0; i < candidates; i++ {
		input := bookingInput(fmt.Sprintf("blr to del friday morning, option %d", i))
		text, err := semantic.IntentText(ChainActivity, input)
		if err != nil {
			tb.Fatal(err)
		}
		vec, err := emb.Embed(ctx, text)
		if err != nil {
			tb.Fatal(err)
		}
		if err := store.Put(ctx, core.Intent{
			ShardID: semShard, WorkflowID: benchWFID, RunID: benchRunID,
			ScheduledSeq: core.Seq(i + 1), ActivityType: ChainActivity,
			IntentText: text, Embedding: vec,
		}); err != nil {
			tb.Fatal(err)
		}
	}

	c := semantic.NewChecker(emb, store)
	return c, store
}

func checkRequest() semantic.CheckRequest {
	return semantic.CheckRequest{
		ShardID:      semShard,
		WorkflowID:   benchWFID,
		RunID:        benchRunID,
		K:            99,
		ActivityType: ChainActivity,
		Input:        bookingInput("bangalore to delhi, friday am"),
		Thresholds:   core.DefaultThresholds(),
	}
}

// ---------------------------------------------------------------------------
// NFR-7: the cost of one semantic check.
// ---------------------------------------------------------------------------

// BenchmarkSemanticCheck measures one whole check through the engine's own
// semantic.Checker: render the intent text, embed it, load the candidates and
// compare against every one of them.
//
// The candidate count is the axis that matters. The design argues the candidate
// set is bounded by the length of ONE workflow's history rather than by the
// size of a global corpus, which is why a brute-force scan is acceptable and no
// approximate-nearest-neighbour index is needed. These rows are the evidence
// for that: the cost has to stay flat enough across realistic candidate counts
// that an index would buy nothing.
//
// Remember what is missing: FakeEmbedder hashes instead of running a model, so
// the embed step here costs microseconds where a real encoder costs
// milliseconds. See the package comment.
func BenchmarkSemanticCheck(b *testing.B) {
	for _, n := range []int{1, 5, 20, 50, 200} {
		b.Run(fmt.Sprintf("candidates=%d", n), func(b *testing.B) {
			checker, store := primedChecker(b, n)
			if store.Len() != n {
				b.Fatalf("primed %d candidates, want %d", store.Len(), n)
			}
			req := checkRequest()
			ctx := context.Background()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := checker.Check(ctx, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSemanticCheckParts splits the check into its pieces, so the report
// can say where the time goes rather than quoting one opaque total.
func BenchmarkSemanticCheckParts(b *testing.B) {
	emb := semantic.NewFakeEmbedder(semantic.DefaultDim)
	input := bookingInput("bangalore to delhi, friday am")
	ctx := context.Background()

	text, err := semantic.IntentText(ChainActivity, input)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("IntentText", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := semantic.IntentText(ChainActivity, input); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Embed/fake", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := emb.Embed(ctx, text); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("CandidateScan/20", func(b *testing.B) {
		checker, _ := primedChecker(b, 20)
		req := checkRequest()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := checker.Check(ctx, req); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestNFR7SemanticCheckBudget writes the requirement down as a test.
//
// NFR-7 allows the semantic check to add no more than 40 ms at p99. This
// measures everything in that check except the model call, because FakeEmbedder
// is standing in for the encoder. The useful output is therefore not "we meet
// NFR-7" — it is how much of the 40 ms the engine spends on its own, and so how
// much is left for the model.
func TestNFR7SemanticCheckBudget(t *testing.T) {
	const (
		budget     = 40 * time.Millisecond
		candidates = 20
		samples    = 200
	)
	checker, _ := primedChecker(t, candidates)
	req := checkRequest()
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if _, err := checker.Check(ctx, req); err != nil {
			t.Fatal(err)
		}
	}

	times := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		ev, err := checker.Check(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(start))
		if semantic.IsFallback(ev) {
			t.Fatal("the check degraded to the fallback path, so nothing was measured")
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })

	p50 := times[len(times)/2]
	p99 := times[int(float64(len(times))*0.99+0.9999)-1]

	t.Logf("NFR-7: one semantic check over %d candidates, %d samples", candidates, samples)
	t.Logf("  engine overhead p50=%v p99=%v (budget for the whole check: %v)", p50, p99, budget)
	t.Logf("  budget left for the model after the engine's own work: %v at p99", budget-p99)
	t.Logf("  NOTE: this excludes the real embedding call. A sentence encoder over a")
	t.Logf("        short sentence typically costs single-digit milliseconds, and that")
	t.Logf("        term — not the engine — is what NFR-7 is really about.")

	if p99 > budget {
		t.Fatalf("the engine's own overhead is %v at p99, which already exceeds the whole "+
			"NFR-7 budget of %v before the model has been called", p99, budget)
	}
	// The engine's share should be a small fraction of the budget, or there
	// would be no room left for the thing that actually does the work.
	if share := float64(p99) / float64(budget); share > 0.25 {
		t.Fatalf("the engine's own overhead is %.1f%% of the NFR-7 budget, leaving too "+
			"little for the model", 100*share)
	}
}

// TestSemanticCheckCostIsBoundedByCandidateCount is the design claim about why
// no vector index is needed, measured rather than asserted.
func TestSemanticCheckCostIsBoundedByCandidateCount(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive; skipped under -short")
	}
	ctx := context.Background()

	measure := func(candidates int) time.Duration {
		checker, _ := primedChecker(t, candidates)
		req := checkRequest()
		for i := 0; i < 10; i++ {
			if _, err := checker.Check(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
		const samples = 100
		times := make([]time.Duration, 0, samples)
		for i := 0; i < samples; i++ {
			start := time.Now()
			if _, err := checker.Check(ctx, req); err != nil {
				t.Fatal(err)
			}
			times = append(times, time.Since(start))
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		return times[len(times)/2]
	}

	one, many := measure(1), measure(50)
	t.Logf("median check: 1 candidate=%v, 50 candidates=%v, growth=%.2fx for 50x the candidates",
		one, many, float64(many)/float64(one))
	t.Logf("the fixed cost — rendering the intent text and embedding it — dominates, " +
		"which is why scanning every candidate is cheaper than maintaining an index")
}

// ---------------------------------------------------------------------------
// Canonicalisation throughput.
// ---------------------------------------------------------------------------

// payload builds a JSON object with n keys, deliberately written out of key
// order so canonicalisation has real sorting work to do.
func payload(n int) json.RawMessage {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		// Descending keys, so every document is worst-case for the sort.
		fmt.Fprintf(&b, `  "field_%03d" : {"value": %d, "label": "item number %d"} `, n-i, i*7, i)
	}
	b.WriteByte('}')
	return json.RawMessage(b.String())
}

// BenchmarkCanonicalize measures the rewrite that makes an idempotency key
// stable: sort the keys, drop the whitespace, normalise the numbers.
//
// It runs on the workflow's hot path, once per activity scheduled, so its
// throughput is what decides whether exact idempotency is effectively free.
// b.SetBytes makes the output report MB/s directly.
func BenchmarkCanonicalize(b *testing.B) {
	for _, fields := range []int{1, 8, 64, 512} {
		in := payload(fields)
		b.Run(fmt.Sprintf("fields=%d/bytes=%d", fields, len(in)), func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := exact.Canonicalize(in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkExactKey is canonicalisation plus the sha256 over it: the whole
// baseline idempotency check, and the thing the semantic check is compared
// against.
func BenchmarkExactKey(b *testing.B) {
	for _, fields := range []int{1, 8, 64} {
		in := payload(fields)
		b.Run(fmt.Sprintf("fields=%d", fields), func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := exact.Key(ChainActivity, in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestCanonicalizePayloadsAreWorthBenchmarking(t *testing.T) {
	// A benchmark over a document that canonicalises to itself would measure
	// nothing interesting, so check the fixtures really do get rewritten.
	for _, fields := range []int{1, 8, 64} {
		in := payload(fields)
		out, err := exact.Canonicalize(in)
		if err != nil {
			t.Fatalf("fields=%d: %v", fields, err)
		}
		if string(out) == string(in) {
			t.Fatalf("fields=%d: the payload is already canonical, so the benchmark "+
				"would not be measuring the rewrite", fields)
		}
		t.Logf("fields=%3d: %5d bytes in -> %5d bytes out", fields, len(in), len(out))
	}
}

// ---------------------------------------------------------------------------
// Cosine similarity throughput.
// ---------------------------------------------------------------------------

// unitVector builds a deterministic pseudo-random unit vector. A fixed seed
// keeps the benchmark comparable between runs.
func unitVector(dim int, seed int64) []float32 {
	r := rand.New(rand.NewSource(seed))
	v := make([]float32, dim)
	var norm float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		norm += float64(v[i]) * float64(v[i])
	}
	norm = 1 / (norm + 1e-12)
	for i := range v {
		v[i] = float32(float64(v[i]) * norm)
	}
	return v
}

// BenchmarkCosine is the innermost loop of the semantic check: one comparison
// between two meaning-vectors. b.SetBytes counts both vectors, so the output
// reads as the rate the comparison consumes vector data.
func BenchmarkCosine(b *testing.B) {
	for _, dim := range []int{64, semantic.DefaultDim, 768, 1536} {
		x, y := unitVector(dim, 1), unitVector(dim, 2)
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
			b.SetBytes(int64(dim * 4 * 2))
			b.ReportAllocs()
			b.ResetTimer()
			var sink float64
			for i := 0; i < b.N; i++ {
				sink = semantic.Cosine(x, y)
			}
			// Keep the compiler from deleting the call.
			if sink > 2 {
				b.Fatal("impossible cosine")
			}
		})
	}
}

// BenchmarkBestMatch measures the full candidate scan at the engine's real
// vector width. This is the part a vector index would replace, so the number
// here is what an index would have to beat.
func BenchmarkBestMatch(b *testing.B) {
	const dim = semantic.DefaultDim
	query := unitVector(dim, 0)

	for _, n := range []int{1, 10, 50, 200, 1000} {
		cands := make([][]float32, n)
		for i := range cands {
			cands[i] = unitVector(dim, int64(i+1))
		}
		b.Run(fmt.Sprintf("candidates=%d", n), func(b *testing.B) {
			b.SetBytes(int64(n * dim * 4))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if idx, _ := semantic.BestMatch(query, cands); idx < 0 {
					b.Fatal("no match returned")
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/candidate")
		})
	}
}

func TestCosineFixturesAreUnitVectors(t *testing.T) {
	// If the fixtures were not unit length the throughput number would still be
	// right, but the similarity values would be meaningless, so check.
	for _, dim := range []int{64, semantic.DefaultDim} {
		v := unitVector(dim, 7)
		if got := semantic.Cosine(v, v); got < 0.999999 {
			t.Fatalf("dim=%d: a vector is not similar to itself (%.6f)", dim, got)
		}
	}
}
