package storer

import "github.com/go-git/go-git/v6/plumbing"

// MissingObjectFetcher is an optional interface for object storers of partial
// clones: repositories whose initial fetch carried a filter and whose
// promisor remote therefore withheld a subset of the objects.
//
// FetchMissingObjects is invoked with the hashes of objects that a lookup
// proved absent from local storage. The implementation fetches them from the
// promisor remote and stores them (in a promisor-marked pack when the storage
// keeps packs) so that a repeated lookup succeeds. Hashes that are already
// local and failures unrelated to their absence are left to the caller to
// observe: the contract only requires that objects present on the remote are
// retrievable afterwards.
//
// The call is synchronous. Storers that implement this interface should
// coalesce concurrent calls into as few network round trips as they can: a
// single high-level operation (checkout, diff) misses many objects in quick
// succession, and one request per object would be a separate connection each.
type MissingObjectFetcher interface {
	FetchMissingObjects(hashes []plumbing.Hash) error
}

// MissingObjectFetchSetter is an optional interface for object storers that
// can be taught where their missing objects come from. The repository wires
// it when it opens or creates a repository that has a promisor remote; storage
// opened on its own, without a repository, simply never gets one and keeps
// returning plumbing.ErrObjectNotFound for absent objects.
type MissingObjectFetchSetter interface {
	SetMissingObjectFetcher(fetch func(hashes []plumbing.Hash) error)
}

// LocalObjectChecker is an optional interface for object storers whose
// HasEncodedObject transparently backfills missing objects. It answers the
// narrower question "is this object already local?" without going to the
// network, which batch prefetch needs to build its request: probing through
// HasEncodedObject would fetch each probed object one at a time before the
// batch could ever be assembled.
type LocalObjectChecker interface {
	HasEncodedObjectLocal(h plumbing.Hash) error
}
