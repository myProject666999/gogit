package filesystem

import (
	"sync"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// missingFetchWindow bounds how long the first waiter for a missing object
// holds off the network request. Callers that miss more objects during the
// window — the typical shape of a checkout or diff, which walks a tree and
// then reads a run of blobs — join the same request, so one high-level
// operation costs one round trip even though each object is requested
// independently on the read path.
//
// The window is deliberately short: a lone miss pays this much latency and no
// more. A larger window batches more aggressively but stalls every cold read;
// canonical git avoids the trade-off with prefetch at its call sites, and the
// explicit prefetch path in the git package mirrors that for checkout.
const missingFetchWindow = 2 * time.Millisecond

type batchState uint8

const (
	// collecting is the assembly window: misses arriving now join the batch.
	batchCollecting batchState = iota
	// closing is the locked instant between the window ending and the request
	// leaving. New misses attach the follow-up.
	batchClosing
	// fetching means the request is on the wire and the batch is closed. A
	// miss arriving now joins (or opens) the follow-up batch instead.
	batchFetching
)

// missingBatch is one coalesced promisor request.
type missingBatch struct {
	state batchState

	seen   map[plumbing.Hash]struct{}
	hashes []plumbing.Hash

	// waiters is one buffered channel per blocked caller.
	waiters []chan error

	// started records that collect was scheduled for this batch.
	started bool

	// next is the batch that opened while this one was closing or fetching.
	// It collects immediately and is fetched once this batch finishes.
	next *missingBatch
}

// missingObjectBatcher merges the misses that arrive close together into one
// promisor fetch and serialises batches so failed rounds can be retried.
//
// A batch collects misses for missingFetchWindow, then fetches. Misses that
// arrive while a batch is fetching open a single follow-up batch, so even an
// operation that keeps discovering missing objects during a round still pays
// at most one request per round rather than one per object.
type missingObjectBatcher struct {
	mu sync.Mutex

	// fetch talks to the promisor remote. nil means this storage is not wired
	// to one (constructed outside a repository, or a full clone), so lookups
	// keep returning plumbing.ErrObjectNotFound as they always did.
	fetch func(hashes []plumbing.Hash) error

	// current is the collecting or in-flight batch; nil when the chain is
	// empty. Follow-up batches hang off current.next.
	current *missingBatch
}

func newMissingObjectBatcher() *missingObjectBatcher {
	return &missingObjectBatcher{}
}

func (b *missingObjectBatcher) setFetch(fetch func(hashes []plumbing.Hash) error) {
	b.mu.Lock()
	b.fetch = fetch
	b.mu.Unlock()
}

// Fetch enqueues h for a promisor fetch and blocks until its batch finished.
// Misses arriving within the same assembly window share one request.
func (b *missingObjectBatcher) Fetch(h plumbing.Hash) error {
	b.mu.Lock()
	if b.fetch == nil {
		b.mu.Unlock()
		return nil
	}
	_, ch := b.enqueue(h)
	b.mu.Unlock()

	return <-ch
}

// FetchMany enqueues every hash and waits for all of them. Explicit prefetch
// call sites use this so an operation that knows the objects it is about to
// read never fragments them across assembly windows. Hashes are deduplicated.
func (b *missingObjectBatcher) FetchMany(hashes []plumbing.Hash) error {
	b.mu.Lock()
	if b.fetch == nil {
		b.mu.Unlock()
		return nil
	}

	chs := make([]chan error, 0, len(hashes))
	for _, h := range hashes {
		_, ch := b.enqueue(h)
		chs = append(chs, ch)
	}
	b.mu.Unlock()

	var firstErr error
	for _, ch := range chs {
		if err := <-ch; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// enqueue joins (or opens) the live batch for h and returns the channel its
// result arrives on. Callers must hold b.mu.
func (b *missingObjectBatcher) enqueue(h plumbing.Hash) (*missingBatch, chan error) {
	batch := b.current
	if batch == nil {
		batch = &missingBatch{state: batchCollecting, seen: make(map[plumbing.Hash]struct{})}
		b.current = batch
	} else if batch.state != batchCollecting {
		// The live batch is closing or fetching: extend its follow-up,
		// opening one on first demand.
		batch = batch.next
		if batch == nil {
			batch = &missingBatch{state: batchCollecting, seen: make(map[plumbing.Hash]struct{}), started: true}
			b.current.next = batch
		}
	}

	if _, ok := batch.seen[h]; !ok {
		batch.seen[h] = struct{}{}
		batch.hashes = append(batch.hashes, h)
	}
	ch := make(chan error, 1)
	batch.waiters = append(batch.waiters, ch)

	if !batch.started && batch == b.current {
		batch.started = true
		go b.collect(batch)
	}

	return batch, ch
}

// collect owns one batch through its assembly window, request, and handoff.
func (b *missingObjectBatcher) collect(batch *missingBatch) {
	timer := time.NewTimer(missingFetchWindow)
	defer timer.Stop()
	<-timer.C

	b.mu.Lock()
	batch.state = batchClosing
	hashes := append([]plumbing.Hash(nil), batch.hashes...)
	waiters := batch.waiters
	fetch := b.fetch
	b.mu.Unlock()

	var err error
	if fetch != nil {
		err = fetch(hashes)
	}

	b.mu.Lock()
	batch.state = batchFetching
	next := batch.next
	// The follow-up becomes current only now: its goroutine starts after the
	// fetched objects are stored, so its request never races the storage
	// update that made this round's waiters' objects local.
	if next != nil {
		b.current = next
	} else if b.current == batch {
		b.current = nil
	}
	b.mu.Unlock()

	for _, ch := range waiters {
		ch <- err
	}

	if next != nil {
		go b.collect(next)
	}
}
