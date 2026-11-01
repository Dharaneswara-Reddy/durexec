# Generic-v2: a domain-independent duplicate check

**Question.** The hybrid comparator's field reader understood only flights
(`itinerary.go`). Can one comparator work for *any* task?

**Method.** `cmd/genericeval`, model-free comparators only, deterministic.
Development used the flight corpus (213 pairs), the ten-domain corpus
(300 pairs) and 45 real rewordings from a live model. Two further ten-domain
corpora were written and **committed before** the version they test:

| Corpus | Domains | Committed before | Measured with |
|---|---|---|---|
| `heldout-300` | hotel, car rental, trade, insurance claim, password reset, file share, payroll, DNS, prescription, deploy | generic-v2 (`76527e0` < `ff3de42`) | generic-v2, once |
| `heldout2-300` | cab, invoice, leave, bill pay, event tickets, backup, access grant, table booking, repair, funds transfer | generic-v2.1 (`450a593` < `f71acc4`) | generic-v2.1, once |

Each has 150 duplicate and 150 distinct pairs (15 + 15 per domain; most
negatives differ from their partner in one deciding detail). Labels are by
construction, not by human annotators.

**What generic-v2 does** (`internal/idem/semantic/generic2.go`): it compares
the two requests *with each other*. Typed values are normalised by form and
compared by kind (amounts, currencies, counts, measures, dates, weekdays and
"next week" shifts, times, parts of day, durations, ids, addresses, IPs,
versions); ordered "X to Y" pairs keep direction; other words are matched via
WordNet 3.0 synonyms, abbreviations and clipped forms. A repeat may add words
but must not replace any; a value stated on one side only escalates. Optional
place knowledge (IATA airport codes and city names, `airportsdata`) is a
separate arm. No embedding model, no LLM, no per-domain vocabulary.

## Results on data the comparator had never seen

| Corpus | Comparator | Repeats caught | Different actions wrongly suppressed |
|---|---|---|---|
| heldout-300 | generic-v1 (value shapes, gate only) | 128/150 (85.3%) | 41/150 (27.3%) |
| heldout-300 | **generic-v2** | 68/150 (45.3%) | **5/150 (3.3%)** |
| heldout2-300 | generic-v1 | 115/150 (76.7%) | 42/150 (28.0%) |
| heldout2-300 | **generic-v2.1** | 82/150 (54.7%) | **3/150 (2.0%)** |

Development corpora, generic-v2.1: flight-213 56/98 caught, 0/115 wrong
(81/98 with places); ten-domain 68/150, 1/150 (78/150 with places); live
rewordings 35/45.

## Reading

* Wrong suppressions fall from ~28% to 2–3% on unseen tasks: the comparator
  is safe to switch on in domains nobody wrote rules for.
* About half of reworded repeats are caught automatically. The rest are
  paraphrases that swap ordinary words ("pay"/"transfer", "move"/"upgrade",
  "goods"/"order"). WordNet relations do not separate those from real
  changes ("standard"/"express", "growth"/"scale"), so generic-v2 never
  suppresses on them; a model tie-break for that residue is the next step.
* Remaining wrong suppressions on held-out-2 (3), read after the run:
  a ticket tier hidden by the counted-noun rule ("2 **gold** tickets" vs
  "2 **silver** tickets"), and two reversed directions phrased without "to"
  ("out of savings into current"; "pick me up in Whitefield ... drop me at
  the airport"). Both are general and fixable, and are left unfixed here so
  the held-out figure stays the one measured.
* Held-out-1 after the v2.1 fixes (post-hoc, no longer unseen): 66/150, 1/150.

Raw rows: `generic-v2-heldout.csv`, `generic-v2.1-heldout2.csv`.
Reproduce: `cd engine && go run ./cmd/genericeval -corpora heldout2`.
