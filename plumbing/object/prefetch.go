package object

import (
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// prefetchChangeBlobs batch-fetches the blobs a patch is about to read from a
// partial clone's promisor remote. Without it each file content is requested
// lazily and separately — the diff loop handles one change before asking for
// the next, so the misses never overlap the storage's coalescing window — and
// a log -p over many commits would pay a network round trip per file.
//
// It mirrors canonical git's diff prefetch (builtin/diff.c diffcore_std ->
// fetch_objects for the prefetch filter), which collects the post-image blobs
// before textconv/diff runs.
//
// Full clones and storers without the promisor-fetch interfaces are left
// untouched: the whole call collapses to a few interface checks.
func prefetchChangeBlobs(changes []*Change) error {
	var (
		fetcher storer.MissingObjectFetcher
		s       storer.EncodedObjectStorer
	)

	var hashes []plumbing.Hash
	seen := make(map[plumbing.Hash]struct{})

	add := func(e ChangeEntry) {
		if e == empty {
			return
		}
		// Gitlinks belong to a different repository.
		if e.TreeEntry.Mode == filemode.Submodule {
			return
		}
		if _, ok := seen[e.TreeEntry.Hash]; ok {
			return
		}
		seen[e.TreeEntry.Hash] = struct{}{}

		// Both sides of a change share one storer; capture it the first time
		// a real entry is seen.
		if s == nil && e.Tree != nil {
			s = e.Tree.s
			if f, ok := e.Tree.s.(storer.MissingObjectFetcher); ok {
				fetcher = f
			}
		}
		hashes = append(hashes, e.TreeEntry.Hash)
	}

	for _, c := range changes {
		add(c.From)
		add(c.To)
	}

	if fetcher == nil || s == nil || len(hashes) == 0 {
		return nil
	}

	// Ask only for what is genuinely absent, without triggering a fetch per
	// probe (HasEncodedObject would itself backfill).
	local, hasLocal := s.(storer.LocalObjectChecker)
	missing := hashes[:0]
	for _, h := range hashes {
		if hasLocal {
			if local.HasEncodedObjectLocal(h) == nil {
				continue
			}
		} else {
			if s.HasEncodedObject(h) == nil {
				continue
			}
		}
		missing = append(missing, h)
	}

	if len(missing) == 0 {
		return nil
	}

	return fetcher.FetchMissingObjects(missing)
}
