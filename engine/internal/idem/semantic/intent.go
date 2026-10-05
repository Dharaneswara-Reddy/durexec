package semantic

import (
	"encoding/json"
	"fmt"

	"github.com/dharaneswar339/durexec/internal/idem/exact"
)

// IntentText builds the one string that gets embedded: the activity type
// followed by its canonical, key-sorted arguments, as `type(args)`.
//
// How this string is built matters more than which model embeds it.
//
// The canonical form comes from the exact package, so two calls whose
// arguments differ only in key order or whitespace produce the identical
// intent text and therefore the identical vector, before any model is
// involved. The model is then only asked to judge differences that actually
// are differences.
//
// The activity type is inside the embedded text rather than only used as a
// filter, so that the type contributes meaning too: "search_flights" and
// "cancel_flight" pull apart in vector space even when their arguments look
// similar.
//
// The honest limitation is what the arguments are. For
//
//	search_flights({"q":"BLR to DEL next Friday"})
//
// the argument IS natural language, so the embedding carries real meaning and
// a near-duplicate phrasing lands nearby. For
//
//	charge_payment({"amount":450})
//
// it is not. The number 450 has no linguistic neighbourhood; 450 and 460 embed
// close together while meaning two different amounts of money, and treating
// them as the same charge would be a serious bug. That asymmetry — the failure
// mode is a silently skipped irreversible action — is exactly why
// core.IdempotencyMode is opt-in per activity and defaults to ModeExact. An
// activity author has to state that their arguments carry meaning before the
// engine will judge them by meaning.
func IntentText(activityType string, input json.RawMessage) (string, error) {
	canon, err := exact.Canonicalize(input)
	if err != nil {
		return "", fmt.Errorf("semantic: build intent text: %w", err)
	}
	return activityType + "(" + string(canon) + ")", nil
}
