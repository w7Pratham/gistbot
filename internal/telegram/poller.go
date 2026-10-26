package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gistbot/internal/state"
)

const (
	pollTimeout    = 30 * time.Second
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// Handler processes an update that passed the owner gate.
type Handler interface {
	Handle(context.Context, Update)
}

// Updates is the long-polling surface a Poller consumes. *Client implements it.
type Updates interface {
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
}

// Poller long-polls Telegram and dispatches owner messages to a Handler.
//
// The stored update offset is advanced before the owner gate and before the
// handler runs, which gives at-most-once dispatch: a crash after the write
// drops the update instead of replaying it.
type Poller struct {
	client  Updates
	state   *state.Store
	ownerID int64
	handler Handler
	log     *slog.Logger
	sleep   func(context.Context, time.Duration) error
}

// NewPoller returns a poller that reads and persists its offset in store. A nil
// logger falls back to slog.Default. sleep is injected by tests.
func NewPoller(client Updates, store *state.Store, ownerID int64, handler Handler, log *slog.Logger) *Poller {
	if log == nil {
		log = slog.Default()
	}
	return &Poller{
		client:  client,
		state:   store,
		ownerID: ownerID,
		handler: handler,
		log:     log,
		sleep:   sleepContext,
	}
}

// Run polls until ctx is cancelled. It returns nil on cancellation and only
// returns an error when the offset cannot be persisted or the backoff wait is
// interrupted unexpectedly; main treats that as a restart trigger.
func (p *Poller) Run(ctx context.Context) error {
	backoff := initialBackoff

	for {
		if ctx.Err() != nil {
			return nil
		}

		updates, err := p.client.GetUpdates(ctx, p.state.Snapshot().UpdateOffset, pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			p.log.Warn("telegram poll failed", "error", err, "backoff", backoff)
			if sleepErr := p.sleep(ctx, backoff); sleepErr != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("telegram poller: wait before retry: %w", sleepErr)
			}
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = initialBackoff

		for _, update := range updates {
			// Persist first: every update advances the offset, including ones
			// the gate rejects. A failed write must stop dispatch so that a
			// restart cannot replay an update that may already have run.
			if err := p.state.SetOffset(update.UpdateID + 1); err != nil {
				return fmt.Errorf("telegram poller: persist offset after update %d: %w", update.UpdateID, err)
			}
			if !p.isOwnerMessage(update) {
				continue
			}
			if p.handler != nil {
				p.handler.Handle(ctx, update)
			}
		}
	}
}

// isOwnerMessage reports whether update is a private message from the owner.
func (p *Poller) isOwnerMessage(update Update) bool {
	message := update.Message
	if message == nil || message.Chat.Type != "private" {
		return false
	}
	if message.From == nil || message.From.IsBot {
		return false
	}
	return message.From.ID == p.ownerID
}

// nextBackoff doubles the wait, capped at maxBackoff.
func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}
