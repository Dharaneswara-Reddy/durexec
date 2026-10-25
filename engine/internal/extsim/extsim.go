// Package extsim is a stand-in for a real external service that honours an
// Idempotency-Key, and a ledger of what that service actually did.
//
// WHY IT EXISTS. The engine's whole "effectively-once" argument rests on one
// mechanism: activity.DedupKey is a pure function of (workflow, run, k), so it
// is byte-identical on every retry and after every crash, and an external
// service that honours it as an Idempotency-Key will perform the effect once
// however many times it is called. The key is derived correctly and it is
// delivered to activity implementations through activity.Info — and until this
// package existed, NO activity in the repository read it. Every shipped
// activity was an in-process simulation that slept and incremented a counter.
// There was no HTTP call and no Idempotency-Key header anywhere in the
// codebase, so the mechanism the central claim depends on had never been
// executed end to end, and no measurement could have detected whether it
// worked.
//
// WHAT MAKES THIS EVIDENCE RATHER THAN DECORATION. The ledger is the service's
// own record, kept entirely SEPARATELY from the engine's event log. Nothing in
// here reads events, executions or tasks, and nothing in the engine writes
// here. So "how many times did the effect really happen" is answered by a
// source the engine cannot influence — which is exactly the question the
// project's existing duplicate-step evidence structurally cannot answer,
// because every retry path in the engine journals nothing at all.
//
// WHAT IT DOES NOT PROVE. This is a simulator. It shows that the engine holds
// up its half of the contract — one stable key per logical effect, forwarded on
// every attempt — and it shows what happens to the effect under each failure.
// It cannot show that any particular real payment provider behaves this way,
// and the limitations that remain even with a perfect provider are written
// down in LIMITATIONS at the bottom of this file rather than glossed over.
package extsim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// HeaderIdempotencyKey is the header the service requires. The name is the de
// facto standard one, so an activity written against this simulator is written
// against the same interface a real provider exposes.
const HeaderIdempotencyKey = "Idempotency-Key"

// Effect is one thing the service did, as the SERVICE recorded it.
type Effect struct {
	// Key is the Idempotency-Key the first accepted request carried.
	Key string
	// EffectID is what the service minted. It is returned unchanged to every
	// later request with the same key, which is how a caller that lost the
	// first response learns the effect's identity rather than creating another.
	EffectID string
	// Amount and Payee are the request's substance, kept so a test can prove
	// the effect happened once with the values the FIRST caller sent.
	Amount int64
	Payee  string
	// At is when the service performed it.
	At time.Time
	// Requests counts every request that arrived with this key, accepted or
	// replayed. Requests > 1 with one Effect is the mechanism working.
	Requests int
}

// Counters are the numbers that make at-least-once invocation visible. The
// engine has no metric for this; the service does.
type Counters struct {
	// Requests is every request that reached the service, including ones it
	// answered from the ledger and ones it deliberately failed.
	Requests int
	// Performed is the number of distinct effects actually carried out.
	Performed int
	// Replayed is the number of requests answered from the ledger because
	// their key had been seen before. This is the duplicate invocations that
	// did NOT become duplicate effects.
	Replayed int
	// Unkeyed is requests that arrived with no Idempotency-Key at all. Each one
	// is performed, because the service has no way to tell it from a new
	// request — which is precisely the failure mode the engine is avoiding.
	Unkeyed int
	// Injected counts requests the configured fault behaviour disrupted.
	Injected int
}

// Behaviour configures the faults the service injects. The zero value is a
// perfectly behaved service.
//
// Every field is deliberate about WHEN the fault happens relative to the
// effect, because that is the only thing that distinguishes these cases:
// a caller cannot tell them apart, and the engine's correctness depends on
// which side of the effect the failure fell.
type Behaviour struct {
	// FailBeforeEffect makes the next n requests fail WITHOUT performing the
	// effect. The honest case: a retry is safe and necessary.
	FailBeforeEffect int

	// LoseResponseAfterEffect makes the next n requests perform the effect and
	// then fail to answer. This is the case the engine cannot resolve on its
	// own and the one the dedup key exists for: the effect happened, the caller
	// does not know, and its retry must not repeat it.
	LoseResponseAfterEffect int

	// HangAfterEffect makes the next n requests perform the effect and then
	// sleep for Hang, so the caller's own timeout fires. Indistinguishable from
	// LoseResponseAfterEffect to the caller, and listed separately because a
	// timeout is the common real-world shape and worth exercising on its own.
	HangAfterEffect int
	Hang            time.Duration

	// KeyTTL expires ledger entries. A real provider keeps idempotency keys for
	// a bounded window — 24 hours is typical — and a durable workflow can
	// easily retry after longer than that. Zero means keys never expire, which
	// no real provider offers.
	KeyTTL time.Duration
}

// Service is the simulated external system. It is safe for concurrent use,
// which matters: concurrent invocations of one key are one of the cases under
// test.
type Service struct {
	mu sync.Mutex

	// ledger is the idempotency CACHE: which keys the service still remembers.
	// Entries leave it when they expire, exactly as a real provider's do.
	ledger map[string]*Effect

	// performed is the service's APPEND-ONLY record of what it actually did,
	// and the two are deliberately separate. A real provider's payment history
	// is not its idempotency cache: forgetting a key does not un-charge the
	// card. Keeping one list that expiry can never shorten is what lets a test
	// prove a duplicate effect happened rather than merely that a key was
	// forgotten.
	performed []Effect

	counters Counters
	beh      Behaviour
	nextID   int

	// now is injectable so key expiry can be tested without sleeping.
	now func() time.Time
}

// New returns a service with the given behaviour.
func New(beh Behaviour) *Service {
	return &Service{
		ledger: make(map[string]*Effect),
		beh:    beh,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// SetClock replaces the service's clock, so a test can move past KeyTTL
// without waiting.
func (s *Service) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// PaymentRequest is the request body.
type PaymentRequest struct {
	Amount int64  `json:"amount"`
	Payee  string `json:"payee"`
}

// PaymentResponse is the response body. Replayed reports that the service
// answered from its ledger instead of performing the effect, which a real
// provider also tells you and which a caller should log rather than ignore.
type PaymentResponse struct {
	EffectID string `json:"effect_id"`
	Amount   int64  `json:"amount"`
	Payee    string `json:"payee"`
	Replayed bool   `json:"replayed"`
}

// ServeHTTP implements the service. POST /payments.
//
// The ordering inside is the whole point of the simulator, so it is spelled
// out: look the key up; if it is known, answer from the ledger WITHOUT
// performing anything; otherwise apply any pre-effect fault, perform the
// effect, record it, then apply any post-effect fault. A fault that fires after
// the record is what models "the effect happened and you will never hear about
// it".
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/payments" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req PaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key := r.Header.Get(HeaderIdempotencyKey)

	resp, hang, status := s.handle(key, req)
	if hang > 0 {
		// The effect is already recorded; the caller will time out and never
		// see this response.
		time.Sleep(hang)
	}
	if status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handle holds the lock for the whole decision, which is what makes two
// concurrent requests with the same key resolve to one effect. A real provider
// does the same thing with a unique index on the key; the lock stands in for it.
func (s *Service) handle(key string, req PaymentRequest) (PaymentResponse, time.Duration, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.Requests++

	if key == "" {
		// No key, so the service cannot possibly tell a retry from a new
		// request, and it performs the effect. Every such request is a
		// potential duplicate effect, and the counter says so.
		s.counters.Unkeyed++
		return s.performLocked("", req), 0, http.StatusOK
	}

	if e, ok := s.ledger[key]; ok {
		if s.beh.KeyTTL > 0 && s.now().Sub(e.At) > s.beh.KeyTTL {
			// The key expired. The service has forgotten the effect, so it will
			// do it AGAIN — and this is not a defect in the simulator, it is
			// what a real provider does. See LIMITATIONS.
			delete(s.ledger, key)
		} else {
			e.Requests++
			s.counters.Replayed++
			return PaymentResponse{
				EffectID: e.EffectID, Amount: e.Amount, Payee: e.Payee, Replayed: true,
			}, 0, http.StatusOK
		}
	}

	if s.beh.FailBeforeEffect > 0 {
		s.beh.FailBeforeEffect--
		s.counters.Injected++
		// Nothing was performed and nothing was recorded, so the key is still
		// unused and a retry is both safe and necessary.
		return PaymentResponse{}, 0, http.StatusServiceUnavailable
	}

	resp := s.performLocked(key, req)

	if s.beh.LoseResponseAfterEffect > 0 {
		s.beh.LoseResponseAfterEffect--
		s.counters.Injected++
		// THE case the dedup key exists for: recorded, then unanswerable.
		return PaymentResponse{}, 0, http.StatusBadGateway
	}
	if s.beh.HangAfterEffect > 0 {
		s.beh.HangAfterEffect--
		s.counters.Injected++
		return resp, s.beh.Hang, http.StatusOK
	}
	return resp, 0, http.StatusOK
}

// performLocked records the effect. Callers must hold s.mu.
func (s *Service) performLocked(key string, req PaymentRequest) PaymentResponse {
	s.nextID++
	// Deterministic ids, so a test can assert on them and a failure is
	// reproducible rather than "some uuid".
	id := fmt.Sprintf("eff_%04d", s.nextID)
	e := &Effect{
		Key: key, EffectID: id, Amount: req.Amount, Payee: req.Payee,
		At: s.now(), Requests: 1,
	}
	if key != "" {
		s.ledger[key] = e
	}
	// The permanent record, which nothing removes.
	s.performed = append(s.performed, *e)
	s.counters.Performed++
	return PaymentResponse{EffectID: id, Amount: req.Amount, Payee: req.Payee}
}

// Counters returns a snapshot.
func (s *Service) Counters() Counters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counters
}

// Effects returns every effect the service performed, in order. This is the
// record the engine cannot see or influence, and it is the authority on what
// happened in the world.
func (s *Service) Effects() []Effect {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Effect(nil), s.performed...)
}

// EffectsForKey counts the effects performed under one key. One is the
// mechanism working; more than one means the same logical action happened twice
// in the world, which is the thing the whole argument is about. It reads the
// permanent record, not the cache, so an expired key cannot hide a duplicate.
func (s *Service) EffectsForKey(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.performed {
		if s.performed[i].Key == key {
			n++
		}
	}
	return n
}

// Remembers reports whether the service still holds an idempotency entry for
// key. A test can then distinguish "the key worked" from "the key was
// forgotten and the effect was repeated".
func (s *Service) Remembers(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ledger[key]
	return ok
}

// Arm replaces the behaviour mid-run, so one test can take a service from
// faulty to healthy without rebuilding it.
func (s *Service) Arm(beh Behaviour) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// KeyTTL is part of the service's nature rather than of an injected fault,
	// so it is preserved unless the caller sets one.
	if beh.KeyTTL == 0 {
		beh.KeyTTL = s.beh.KeyTTL
	}
	s.beh = beh
}

// LIMITATIONS — what this simulator shows, and what no amount of it can.
//
//  1. IT CANNOT MAKE EXTERNAL EXECUTION EXACTLY-ONCE, and nothing in this
//     package should be read as claiming so. The engine's ceiling is
//     at-least-once invocation with a stable key. Whether that becomes a
//     single effect is entirely the downstream service's decision, and this
//     package demonstrates the decision rather than making it.
//
//  2. A UNIQUE JOURNAL RECORD IS NOT A UNIQUE EFFECT. The project's existing
//     evidence — zero duplicate completed steps across thousands of activities
//     — is a statement about the event log's primary key, and the log cannot
//     record a duplicate INVOCATION at all: every retry path in the engine
//     (release-for-retry, claim lost, fenced, lapsed lock) returns without
//     journaling anything. These counters are the only place the gap between
//     invocations and effects is visible.
//
//  3. KEY EXPIRY BREAKS THE GUARANTEE OUTRIGHT, and a durable execution engine
//     is the system most likely to meet it. Providers keep idempotency keys for
//     a bounded window; a workflow can retry a step after a multi-day timer or
//     a long outage. Past the window the key is unknown and the effect happens
//     again. The engine CANNOT fix this on its own — the activity has to
//     reconcile by querying the service for the effect, which requires the
//     service to be queryable by the caller's own reference. Nothing in this
//     engine does that today, and the behaviour is tested here so the gap is
//     measured rather than assumed away.
//
//  4. AN UNKEYED REQUEST IS ALWAYS A POSSIBLE DUPLICATE. Counters.Unkeyed is
//     therefore a defect counter for the activities, not a property of the
//     service: any activity performing an effect without forwarding
//     Info.DedupKey has opted out of the whole mechanism.
//
//  5. THE EFFECT'S CONTENT IS NOT CHECKED AGAINST THE KEY. This service
//     answers a replayed key from its ledger and ignores the new body, which
//     is what real providers do. So an activity that sends a DIFFERENT amount
//     under the same key gets the FIRST amount's result back and no error. The
//     engine's protection against that is the replay signature now including a
//     digest of the activity input (see internal/workflow/canonical.go); the
//     service offers none.
