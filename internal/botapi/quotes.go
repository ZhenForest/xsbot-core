package botapi

import (
	"context"
	"errors"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/message"
	"github.com/ZhenForest/xsbot-core/sdk"
)

const QuoteTimeout = 5 * time.Second
const MaxQuoteWeight int64 = 1 << 20

// EnrichQuotes returns a private copy. It fetches only direct, valid ID-only
// replies; queried messages are never sent back through Router or enriched again.
func (s *Shared) EnrichQuotes(ctx context.Context, m sdk.Message, generation uint64) sdk.Message {
	return enrichQuotes(ctx, m, QuoteTimeout, func(ctx context.Context, ref sdk.MessageRef) (sdk.Message, error) {
		select {
		case s.quoteSlots <- struct{}{}:
			defer func() { <-s.quoteSlots }()
		default:
			return sdk.Message{}, &sdk.Error{Kind: sdk.Busy}
		}
		ctx, done, err := s.Operations.Begin(ctx, false)
		if err != nil {
			return sdk.Message{}, err
		}
		defer done()
		quoted, err := s.Client.GetMessageAt(ctx, ref, generation)
		if err == nil && generation != s.Client.Generation() {
			return sdk.Message{}, &sdk.Error{Kind: sdk.Expired}
		}
		if err == nil && message.Weight(quoted) > MaxQuoteWeight {
			return sdk.Message{}, &sdk.Error{Kind: sdk.TooLarge}
		}
		if err == nil {
			quoted = s.ResourcesAt(quoted, generation)
		}
		return quoted, err
	})
}

func enrichQuotes(ctx context.Context, original sdk.Message, timeout time.Duration, get func(context.Context, sdk.MessageRef) (sdk.Message, error)) sdk.Message {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	m := sdk.CloneMessage(original)
	var retained int64
	type outcome struct {
		message sdk.Message
		status  sdk.QuoteStatus
	}
	seen := map[sdk.MessageRef]outcome{}
	for i := range m.Segments {
		s := &m.Segments[i]
		if s.Kind != sdk.Reply || s.Unknown != nil {
			continue
		}
		if s.Ref == nil || s.Ref.ID == 0 || s.Ref.Target != m.Ref.Target || s.Ref.Target.Validate() != nil {
			s.Quoted = nil
			s.QuoteStatus = sdk.QuoteInvalid
			continue
		}
		if s.Quoted != nil {
			if s.Quoted.Ref != *s.Ref {
				s.Quoted = nil
				s.QuoteStatus = sdk.QuoteInvalid
			} else {
				weight := message.Weight(*s.Quoted)
				if weight > MaxQuoteWeight-retained {
					s.Quoted = nil
					s.QuoteStatus = sdk.QuoteTooLarge
				} else {
					retained += weight
					s.QuoteStatus = sdk.QuoteReady
				}
			}
			continue
		}
		x, exists := seen[*s.Ref]
		if !exists {
			var err error
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				x.message, err = get(ctx, *s.Ref)
				if ctx.Err() != nil {
					err = ctx.Err()
				}
			}
			x.status = sdk.QuoteReady
			if err != nil {
				x.status = sdk.QuoteFailed
				if ctx.Err() == context.DeadlineExceeded || sdk.HasKind(err, sdk.Timeout) {
					x.status = sdk.QuoteTimeout
				}
				if sdk.HasKind(err, sdk.Expired) {
					x.status = sdk.QuoteExpired
				}
				if sdk.HasKind(err, sdk.Busy) {
					x.status = sdk.QuoteBusy
				}
				if sdk.HasKind(err, sdk.TooLarge) {
					x.status = sdk.QuoteTooLarge
				}
				if sdk.HasKind(err, sdk.Invalid) {
					x.status = sdk.QuoteInvalid
				}
				var failure *sdk.Error
				if errors.As(err, &failure) && failure.Kind == sdk.ProtocolFailure && failure.Code == 0 {
					x.status = sdk.QuoteInvalid
				}
			} else if x.message.Ref != *s.Ref || (m.BotID.State == sdk.Present && x.message.BotID.State == sdk.Present && m.BotID.Value != x.message.BotID.Value) {
				x.status = sdk.QuoteInvalid
			}
			if x.status == sdk.QuoteReady && message.Weight(x.message) > MaxQuoteWeight-retained {
				x.status = sdk.QuoteTooLarge
			}
			if x.status != sdk.QuoteReady {
				x.message = sdk.Message{}
			}
			seen[*s.Ref] = x
		}
		s.QuoteStatus = x.status
		if x.status == sdk.QuoteReady {
			weight := message.Weight(x.message)
			if weight > MaxQuoteWeight-retained {
				s.QuoteStatus = sdk.QuoteTooLarge
			} else {
				retained += weight
				copy := sdk.CloneMessage(x.message)
				s.Quoted = &copy
			}
		}
	}
	return m
}
