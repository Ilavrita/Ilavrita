package main

import (
	"context"
	"time"

	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

const (
	// reindexEvery is how often a replica looks for a type to walk. A parameter
	// somebody just defined is worth having soon; it is not worth spinning for.
	reindexEvery = 5 * time.Second

	// reindexLease is how long a claim holds. It bounds how long a type waits
	// after the replica walking it dies, so it is generous enough that a large
	// type finishes inside one and short enough that nobody waits an hour for a
	// process that is gone.
	reindexLease = 5 * time.Minute

	// reindexAtOnce is how many types one pass takes. One at a time would make
	// a Project defining parameters on six types wait six passes; taking them
	// all would hold this process's one connection for as long as that takes.
	reindexAtOnce = 2
)

// reindexer walks the types whose index no longer matches their parameters.
//
// It is a worker outside every request for the same reason the notifier is: the
// work is a whole type's rows, and a request that did it would hold this
// process's single pooled connection for as long as it took.
type reindexer struct {
	store  *sqlite.ResourceStore
	worker string
	now    func() time.Time
}

// runReindexer works the backlog until the context is done.
func runReindexer(ctx context.Context, held *reindexer) {
	ticker := time.NewTicker(reindexEvery)
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

// pass is one look at the backlog. A failure is reported and the next pass tries
// again: the claim expires, so work a failed pass was holding is taken up rather
// than stranded.
func (r *reindexer) pass(ctx context.Context) {
	if r.store == nil {
		return
	}

	at := r.clock()

	claimed, err := r.store.ClaimReindex(ctx, r.worker, at.Add(reindexLease), at, reindexAtOnce)
	if err != nil {
		report(err)

		return
	}

	for _, work := range claimed {
		if err := r.store.Reindex(ctx, work); err != nil {
			report(err)
		}
	}
}

// clock is the time this worker reads, so a test can hold it still.
func (r *reindexer) clock() time.Time {
	if r.now == nil {
		return time.Now().UTC()
	}

	return r.now().UTC()
}
