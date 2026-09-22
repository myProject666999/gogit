# Partial clone (filter + promisor lazy fetch)

go-git can clone and fetch with `CloneOptions.Filter` / `FetchOptions.Filter`
(e.g. `blob:none`). This document describes how the repository is marked as
partial, how the withheld objects come back on demand, how several misses are
coalesced into one request, and how the fetched packs stay readable.

## Repository marking

A filtered fetch leaves objects deliberately absent. Three things record that
fact so that both git and go-git treat the absences as *promised* rather than
as corruption:

1. **`.promisor` sidecar.** Every pack a filtered fetch stores is written by
   the `PromisorPackfileWriter` (`storage/filesystem`), which places an empty
   `pack-<hash>.promisor` next to the `.pack`/`.idx`/`.rev`. Presence is all
   git consults (`packfile.c` uses `access(2)`); the marker vouchs for the
   objects the pack references but does not contain. Packs produced by a lazy
   backfill are promisor-marked too, because they can themselves reference
   still-withheld objects.
2. **`remote.<name>.promisor = true`** and
   **`remote.<name>.partialclonefilter = <filter>`**, written by
   `Remote.recordPromisor`. The filter is recorded once (the first one) and is
   what later fetches and lazy requests reapply.
3. **`extensions.partialClone = <name>`** plus
   `core.repositoryFormatVersion = 1`, the same keys canonical git writes in
   `list-objects-filter-options.c partial_clone_register`. On open,
   `promisorRemoteName` reads the extension (falling back to a
   `promisor = true` remote) to find where lazy fetches go.

Memory storage keeps objects individually (there is no pack to mark), so a
filtered fetch into memory is allowed; lazy fetching is only wired for
storers implementing the promisor interfaces, which today is the on-disk
filesystem storer.

## Lazy backfill

`ObjectStorage` lookups — `EncodedObject`, `EncodedObjectSize`,
`HasEncodedObject`, and `DeltaObject` — run the ordinary lookup first. Only
when it returns `plumbing.ErrObjectNotFound` do they ask the
`missingObjectBatcher` for the hash and then retry the lookup once. In a full
clone the batcher has no fetch callback, so the call is a no-op and the
original error is returned unchanged: no new network behaviour appears for
non-partial repositories or storers opened without a repository.

The callback (`promisorFetchFunc` in `promisor.go`) opens a fresh upload-pack
connection to the promisor remote and sends a protocol **v2** fetch command
with the missing hashes as explicit `want`s, no `have`s, and the recorded
filter. The filter is safe on this request: v2 applies a filter only to the
graph the server traverses, never to objects named explicitly as roots
(`list-objects-filter.c` exempts the root set), matching canonical git's
`fetch_objects` reuse of the clone's `filter_options`. The returned pack is
stored through `UpdatePromisorObjectStorage` and therefore lands as another
promisor-marked pack.

The repository wires the callback through
`storer.MissingObjectFetchSetter` in two places: `Open`/`PlainOpen` (so an
already-partial clone heals on read) and inside `clone` right after a
successful filtered fetch but **before** the checkout, which is the first
consumer of withheld blobs.

## Batching

A checkout or diff misses many objects in quick succession; requesting them
one HTTP round trip each would be unusable. Two mechanisms keep the request
count at one per operation:

1. **Explicit prefetch (the primary boundary).**
   - Checkout/reset calls `Repository.prefetchTreeBlobs`, which walks the
     target tree, collects every file entry that is not already local
     (`storer.LocalObjectChecker.HasEncodedObjectLocal` — a probe that does
     *not* itself fetch), and hands the whole set to
     `storer.MissingObjectFetcher.FetchMissingObjects` in one call. This
     mirrors git's prefetch-to-checkout (`builtin/checkout.c` via
     `fetch-object.c`).
   - Patch generation (`object.getPatchContext`, i.e. the `log -p` / diff
     path) prefetches the from/to blob hashes of all changes in the same way,
     before diffing any of them — git diff's prefetch is the analogue.
2. **Coalescing window (the read-path safety net).** Individual lazy reads
   that were not covered by an explicit prefetch go through
   `missingObjectBatcher`: the first miss opens a batch that collects further
   misses for a short 2 ms window and then issues one request. Batches are
   chained serially — a miss arriving while a request is in flight attaches
   to the follow-up batch, which starts only after the fetched pack is stored,
   so retries after an error and concurrent readers never overlap on the
   wire. Hashes within a batch are deduplicated.

## Pack and index handling

Backfilled packs are written by the same `PackWriter` path as ordinary fetches
(`ObjectStorage.packfileWriter`): the close sequence writes `.pack`, `.idx`,
`.rev`, and the `.promisor` sidecar (marker first, then the pack rename, so no
window exists in which the pack is visible without its marker), and publishes
the new index into the in-memory pack index via the writer's `Notify`
callback. The retry lookup therefore sees the object immediately without a
directory rescan; later processes rebuild the index from disk as usual.

go-git does not currently read or write a multi-pack index (`midx`), so there
is no `midx` to update or invalidate: adding a pack is exactly the same
operation a normal fetch performs. If midx support is added later, a promisor
fetch must invalidate/refresh it on the same boundary as any other fetch —
nothing here assumes a single pack.

## Fallback when the server does not support filters

If a filtered **clone** gets `transport.ErrFilterNotSupported` (the capability
was not advertised), `Repository.clone` retries the same fetch once with the
filter cleared, mirroring `fetch-pack.c`. The retry is clone-only: an explicit
`Fetch` with a filter keeps returning the error, as it did before, so callers
opting into filtering are not silently surprised. A successful fallback
produces a complete repository — no promisor config, no `extensions.partialClone`,
no marked pack — and the lazy fetcher is never wired.

## Public API

No new option is required for the common case: `CloneOptions.Filter` and
`FetchOptions.Filter` (both `packp.Filter`, e.g. `packp.FilterBlobNone()`)
drive everything. Storage integrations that provide their own object store
can participate via the optional interfaces:

- `storer.MissingObjectFetchSetter` — receive the fetch callback,
- `storer.MissingObjectFetcher` — serve an explicit batch prefetch,
- `storer.LocalObjectChecker` — answer "already local?" without fetching,
- `storer.PromisorPackfileWriter` / `storer.PromisorObjectStorer` — mark and
  report promisor packs.
