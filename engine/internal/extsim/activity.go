package extsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
)

// PayActivityName is the registered name of the one activity in this
// repository that performs a real external effect over HTTP.
const PayActivityName = "ExternalPayment"

// ErrNoDedupKey is returned when the activity cannot find its dedup key.
//
// It REFUSES TO CALL rather than calling without a key, and that choice is the
// whole point of the activity. A call with no Idempotency-Key is a call the
// downstream service cannot deduplicate, so a retry after a lost response
// would charge twice. Failing loudly turns a silent double-charge into a
// workflow failure an operator can see, and the failure is non-retryable
// because retrying would not conjure a key.
var ErrNoDedupKey = errors.New("extsim: no dedup key in the activity context, refusing to perform an external effect")

// PayInput is what the workflow asks for.
type PayInput struct {
	Amount int64  `json:"amount"`
	Payee  string `json:"payee"`
}

// PayOutput is what the workflow records. ServiceReplayed is carried into
// history deliberately: it is how an operator reading the log afterwards can
// see that this step was invoked more than once and that the external service
// absorbed the repeat. Nothing else in the engine records that.
type PayOutput struct {
	EffectID        string `json:"effect_id"`
	Amount          int64  `json:"amount"`
	Payee           string `json:"payee"`
	ServiceReplayed bool   `json:"service_replayed"`
	DedupKeyUsed    string `json:"dedup_key_used"`
}

// PayDefinition builds the activity, pointed at baseURL.
//
// This is the first activity in the project that reads activity.Info. Until it
// existed, the dedup key was derived correctly, delivered correctly, and read
// by nobody: a grep for InfoFromContext across app/ returned nothing, and every
// shipped activity was an in-process sleep. So the mechanism the central claim
// rests on had never once run end to end.
//
// Compensation is left empty on purpose. A real payment would name a refund
// here, and a refund is itself an external effect needing its own key. The
// relationship between an effect's key and its compensation's key is a design
// question this engine has not answered, and inventing an answer in a
// simulator would misrepresent what is built.
func PayDefinition(client *http.Client, baseURL string) activity.Definition {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return activity.Definition{
		Name: PayActivityName,
		Mode: core.ModeExact,
		Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			info, ok := activity.InfoFromContext(ctx)
			if !ok || info.DedupKey == "" {
				return nil, &core.NonRetryableError{Cause: ErrNoDedupKey}
			}

			var req PayInput
			if err := json.Unmarshal(in, &req); err != nil {
				// Malformed input is the workflow's bug and no retry fixes it.
				return nil, &core.NonRetryableError{
					Cause: fmt.Errorf("extsim: undecodable payment input: %w", err),
				}
			}

			body, err := json.Marshal(PaymentRequest{Amount: req.Amount, Payee: req.Payee})
			if err != nil {
				return nil, &core.NonRetryableError{Cause: err}
			}
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
				baseURL+"/payments", bytes.NewReader(body))
			if err != nil {
				return nil, &core.NonRetryableError{Cause: err}
			}
			httpReq.Header.Set("Content-Type", "application/json")
			// The one line this whole package exists to execute. The key is a
			// pure function of (workflow, run, k), so it is byte-identical on
			// every attempt and after every crash — which is exactly the
			// property an Idempotency-Key needs and the reason the retry of a
			// call that already landed does not land twice.
			httpReq.Header.Set(HeaderIdempotencyKey, info.DedupKey)

			resp, err := client.Do(httpReq)
			if err != nil {
				// A transport failure is AMBIGUOUS: the effect may or may not
				// have happened. Retryable is the right answer precisely
				// because the key makes a retry safe — without the key this
				// would be the point at which a second charge became likely.
				return nil, &core.RetryableError{
					Cause: fmt.Errorf("extsim: %s: %w", info.DedupKey, err),
				}
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

			switch {
			case resp.StatusCode == http.StatusOK:
			case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests:
				// Same ambiguity, stated by the server. Retry under the same
				// key.
				return nil, &core.RetryableError{
					Cause: fmt.Errorf("extsim: %s: service returned %d: %s",
						info.DedupKey, resp.StatusCode, bytes.TrimSpace(raw)),
				}
			default:
				// A 4xx is the service saying the request itself is wrong, and
				// repeating it unchanged cannot help.
				return nil, &core.NonRetryableError{
					Cause: fmt.Errorf("extsim: %s: service rejected the request with %d: %s",
						info.DedupKey, resp.StatusCode, bytes.TrimSpace(raw)),
				}
			}

			var out PaymentResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				// The effect probably happened and the answer is unreadable, so
				// this is ambiguous too, not a validation error.
				return nil, &core.RetryableError{
					Cause: fmt.Errorf("extsim: %s: undecodable response: %w", info.DedupKey, err),
				}
			}
			return json.Marshal(PayOutput{
				EffectID:        out.EffectID,
				Amount:          out.Amount,
				Payee:           out.Payee,
				ServiceReplayed: out.Replayed,
				DedupKeyUsed:    info.DedupKey,
			})
		},
	}
}
