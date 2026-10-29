// Command extsimd is a TEST-ONLY external-effect simulator.
//
// It exists for one reason: the external-effect ledger has to survive the
// engine dying. internal/extsim's Service is an in-process HTTP handler, and
// every existing test runs it inside the test binary — which is fine for a
// unit test, and useless for the question this phase asks. If the simulator
// lives in the engine's process, then SIGKILLing the engine to test "the
// effect happened but the process died before journaling it" destroys the only
// record of whether the effect happened. The evidence and the thing under test
// would share a fate.
//
// So this is a separate process, in its own pod, holding its own ledger.
// Killing the engine does not touch it, and the ledger can be read afterwards
// to count effects independently of anything the engine wrote.
//
// It is never built into the engine image and is not referenced by engined.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dharaneswar339/durexec/internal/extsim"
)

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	flag.Parse()

	svc := extsim.New(extsim.Behaviour{})

	mux := http.NewServeMux()

	// The effect endpoint itself, delegated unchanged to internal/extsim so
	// that the behaviour measured here is the behaviour the unit tests cover.
	mux.Handle("/payments", svc)

	// ---- ledger inspection -------------------------------------------------
	// Counters separates REQUESTS (invocations) from PERFORMED (effects) from
	// REPLAYED (invocations the service absorbed because it recognised the
	// key). That separation is the whole measurement: at-least-once invocation
	// with effectively-once effect looks like requests > performed, and a
	// duplicate EFFECT looks like performed > the number of distinct keys.
	mux.HandleFunc("GET /counters", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, svc.Counters())
	})

	mux.HandleFunc("GET /effects", func(w http.ResponseWriter, r *http.Request) {
		effects := svc.Effects()
		// Per-key effect count is reported explicitly rather than left for the
		// caller to compute, because "two effects under one key" is the exact
		// failure this whole phase is looking for and it should be impossible
		// to miss in the output.
		// NOTE on reading this: more than one REQUEST under a key is expected
		// and safe — it is the dedup absorbing a retry. A duplicate EFFECT is
		// two ledger rows sharing one key, which is the failure being hunted.
		dup := 0
		byKey := map[string]int{}
		for _, e := range effects {
			byKey[e.Key]++
		}
		for _, n := range byKey {
			if n > 1 {
				dup += n - 1
			}
		}
		writeJSON(w, map[string]any{
			"effects":                effects,
			"distinct_keys":          len(byKey),
			"duplicate_effect_count": dup,
		})
	})

	mux.HandleFunc("GET /effects/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		writeJSON(w, map[string]any{
			"key":       key,
			"effects":   svc.EffectsForKey(key),
			"remembers": svc.Remembers(key),
		})
	})

	// ---- behaviour arming --------------------------------------------------
	// Arming is a POST with a Behaviour body. This is a test binary with no
	// authentication, which is why it must never be deployed anywhere but an
	// isolated lab namespace; it is NOT reachable from the engine's own API
	// surface and adds no endpoint to engined.
	mux.HandleFunc("POST /arm", func(w http.ResponseWriter, r *http.Request) {
		var beh extsim.Behaviour
		if err := json.NewDecoder(r.Body).Decode(&beh); err != nil {
			http.Error(w, "bad behaviour: "+err.Error(), http.StatusBadRequest)
			return
		}
		svc.Arm(beh)
		writeJSON(w, map[string]any{"armed": beh})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"status": "ok", "counters": svc.Counters()})
	})

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// Generous, because HangAfterEffect deliberately holds a response open
		// to simulate a timeout; a short write timeout here would cut the hang
		// short and the engine would see a different failure than intended.
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      0,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		// No grace period worth having: the ledger is in memory and a
		// shutdown loses it either way, which is why tests read the ledger
		// BEFORE restarting this process and never restart it mid-scenario.
		_ = srv.Close()
	}()

	log.Printf("extsimd listening on %s — TEST ONLY, no authentication", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "extsimd:", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
