package adapter

import (
	"context"
	"log"
	"time"
)

// Only observation streams are retried. Each attempt invalidates its own state
// on loss and establishes a new snapshot on subscribe; business RPCs never use
// this loop, because a lost reply does not mean a mutation was not executed.
func retrySource(ctx context.Context, name string, watch func(context.Context) error) error {
	delay := 100 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		err := watch(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil // The source explicitly completed; only failures reconnect.
		}
		if time.Since(started) >= 5*time.Second {
			delay = 100 * time.Millisecond
		}
		log.Printf("%s source disconnected: %v; reconnecting in %s", name, err, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}
