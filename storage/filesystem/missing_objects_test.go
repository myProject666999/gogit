package filesystem

import (
	"sync"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
)

func testHash(i int) plumbing.Hash {
	return plumbing.NewHash(fmt.Sprintf("%040x", i))
}

// TestMissingObjectBatcherCoalesces pins the requirement behind partial clone:
// objects one high-level operation misses close together must come back in a
// single promisor request, not one connection per object.
func TestMissingObjectBatcherCoalesces(t *testing.T) {
	t.Parallel()

	var (
		calls   atomic.Int32
		gotSize atomic.Int32
		mu      sync.Mutex
		seen    = map[plumbing.Hash]bool{}
	)

	b := newMissingObjectBatcher()
	b.setFetch(func(hashes []plumbing.Hash) error {
		calls.Add(1)
		gotSize.Store(int32(len(hashes)))
		mu.Lock()
		for _, x := range hashes {
			seen[x] = true
		}
		mu.Unlock()
		return nil
	})

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, b.Fetch(testHash(i + 1)))
		}()
	}
	close(start)
	wg.Wait()

	require.EqualValues(t, 1, calls.Load(), "20 misses inside one window must be one request")
	require.EqualValues(t, n, gotSize.Load())
	assert.Len(t, seen, n)
}

// TestMissingObjectBatcherDedupes verifies the same hash requested repeatedly
// in one window is requested once from the remote.
func TestMissingObjectBatcherDedupes(t *testing.T) {
	t.Parallel()

	var got atomic.Int32
	b := newMissingObjectBatcher()
	b.setFetch(func(hashes []plumbing.Hash) error {
		got.Store(int32(len(hashes)))
		return nil
	})

	require.NoError(t, b.FetchMany([]plumbing.Hash{testHash(1), testHash(1), testHash(2), testHash(1), testHash(2)}))
	require.EqualValues(t, 2, got.Load())
}

// TestMissingObjectBatcherSerialRounds verifies misses arriving while a request
// is in flight chain into a second, serial round rather than racing two
// requests for the same follow-up batch.
func TestMissingObjectBatcherSerialRounds(t *testing.T) {
	t.Parallel()

	var (
		calls   atomic.Int32
		mu      sync.Mutex
		sizes   []int
		proceed = make(chan struct{})
	)

	b := newMissingObjectBatcher()
	b.setFetch(func(hashes []plumbing.Hash) error {
		n := calls.Add(1)
		mu.Lock()
		sizes = append(sizes, len(hashes))
		mu.Unlock()
		if n == 1 {
			<-proceed
		}
		return nil
	})

	firstDone := make(chan error, 1)
	go func() { firstDone <- b.Fetch(testHash(1)) }()

	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)

	secondDone := make(chan error, 1)
	go func() { secondDone <- b.Fetch(testHash(2)) }()

	time.Sleep(20 * time.Millisecond)
	require.EqualValues(t, 1, calls.Load(), "follow-up must not overlap the in-flight request")

	close(proceed)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)

	require.EqualValues(t, 2, calls.Load())
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int{1, 1}, sizes)
}

// TestMissingObjectBatcherNoFetcher keeps storage without a wired promisor
// inert: calls succeed as a no-op, so the lookup path keeps treating the object
// as absent rather than triggering a network.
func TestMissingObjectBatcherNoFetcher(t *testing.T) {
	t.Parallel()

	b := newMissingObjectBatcher()
	assert.NoError(t, b.Fetch(testHash(1)))
	assert.NoError(t, b.FetchMany([]plumbing.Hash{testHash(1), testHash(2)}))
}

// TestMissingObjectBatcherRetryAfterError verifies a hash whose first round
// failed can be requested again on a later round and succeeds then.
func TestMissingObjectBatcherRetryAfterError(t *testing.T) {
	t.Parallel()

	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int32
	b := newMissingObjectBatcher()
	b.setFetch(func(hashes []plumbing.Hash) error {
		calls.Add(1)
		if fail.Load() {
			return assert.AnError
		}
		return nil
	})

	require.Error(t, b.Fetch(testHash(7)))
	fail.Store(false)
	require.NoError(t, b.Fetch(testHash(7)))
	require.EqualValues(t, 2, calls.Load())
}
