package git

import (
	"errors"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// prefetchTreeBlobs asks the promisor remote, in a single request, for every
// blob reachable from root that is not already local. It is the batch
// boundary for operations that materialise or compare a whole tree — checkout
// and reset — mirroring canonical git's prefetch-to-checkout step
// (fetch-object.c prefetch_to_remote, driven from builtin/checkout.c).
//
// Without it the lazy per-object path would still make checkout correct, but
// it would emit one request per blob: each miss is handled before the next
// file is read, so misses never overlap the batching window. Walking the tree
// up front collects the whole set and coalesces it.
//
// Repositories without a promisor remote, and storers that cannot fetch, are
// left to the ordinary lazy path: prefetchTreeBlobs is then a no-op.
func (r *Repository) prefetchTreeBlobs(root *object.Tree) error {
	fetcher, ok := r.Storer.(storer.MissingObjectFetcher)
	if !ok {
		return nil
	}

	// Prefer a network-free local check so probing does not defeat the
	// batching by fetching each blob as it is considered.
	local, hasLocal := r.Storer.(storer.LocalObjectChecker)

	var missing []plumbing.Hash

	treeIter := object.NewTreeWalker(root, true, nil)
	defer treeIter.Close()

	for {
		_, entry, err := treeIter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A tree withheld by a tree-depth filter is discovered here; leave
			// it to the operation's own lazy reads, which fetch it on demand,
			// rather than failing the whole-tree walk. Any other error is a
			// real failure to read the tree we were given.
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				continue
			}
			return err
		}

		// Submodule entries are gitlinks: their objects belong to another
		// repository and must never be requested from this remote.
		if !entry.Mode.IsFile() {
			continue
		}

		var probeErr error
		if hasLocal {
			probeErr = local.HasEncodedObjectLocal(entry.Hash)
		} else {
			probeErr = r.Storer.HasEncodedObject(entry.Hash)
		}
		if probeErr != nil {
			missing = append(missing, entry.Hash)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	return fetcher.FetchMissingObjects(missing)
}
