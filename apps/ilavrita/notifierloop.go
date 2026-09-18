package main

import (
	"context"
	"time"
)

// notifyEvery is how often the notifier looks for work.
//
// It polls rather than being woken by the write, because a write and a
// notification are deliberately not the same transaction: the write must not
// wait on anybody's subscriber, and a notification must survive this process
// going away between the two.
const notifyEvery = 5 * time.Second

// runNotifier works the queue until the context is done.
func runNotifier(ctx context.Context, held *notifier) {
	ticker := time.NewTicker(notifyEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			held.pass(ctx)
		}
	}
}

// pass is one look at the queue. A failure is reported and the next pass tries
// again: there is nobody to answer to here, and a worker that stopped on the
// first error would leave every later notification unsent.
func (n *notifier) pass(ctx context.Context) {
	if err := n.fanOut(ctx); err != nil {
		report(err)
	}

	if err := n.send(ctx); err != nil {
		report(err)
	}
}
