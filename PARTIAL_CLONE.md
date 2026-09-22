# Partial Clone

go-git supports partial clone: cloning or fetching with an object filter
(`CloneOptions.Filter` / `FetchOptions.Filter`, e.g. `blob:none`), and
backfilling the objects the filter withheld from the promisor remote when
they are actually needed. The wire protocol is protocol v2, matching the
reference `git` implementation's on-disk and on-the-wire behaviour.

## Marking a repository as partial

A filtered fetch that lands on disk records its origin in three places,
mirroring what `git clone --filter` writes:

- `remote.<name>.promisor = true` — the remote promises to serve the
  objects it withheld.
- `remote.<name>.partialclonefilter = <filter>` — the filter to reapply to
  later fetches from this remote, recorded once and then left alone.
- `core.repositoryformatversion = 1` — partial clone is a repository
  format extension, so the format version has to allow extensions.

In the object database, every pack received from a promisor remote gets a
sibling `<pack>.promisor` file (empty; only its presence is consulted, by
go-git and by git alike). This marker is what lets `git fsck`/`git gc`
treat the absent objects as promised rather than as corruption. Storages
that write packfiles must implement `storer.PromisorPackfileWriter` to be
usable for filtered fetches; storages that keep objects loose (memory)
need no marker. Repacking a partial clone folds objects into a new pack
that is promisor-marked in turn, so the markers survive maintenance.

## When objects are backfilled

Reading an object the promisor remote withheld fetches it on demand
instead of failing with `plumbing.ErrObjectNotFound`. When a repository
has a promisor remote configured, `Open`, `Clone` and `Fetch` wrap the
repository's `Storer` in a backfilling layer (`promisorStorer`):

- `EncodedObject` (and `EncodedObjectSize`) on a missing object triggers
  a fetch and retries the read once. A genuine miss still returns
  `ErrObjectNotFound` — a promisor only promises the objects it filtered
  out.
- Presence probes (`HasEncodedObject`) never fetch: they report what is
  actually local, the same split git makes with
  `OBJECT_INFO_SKIP_FETCH_OBJECT`.
- The wrapper delegates every optional storage interface
  (`PackfileWriter`, `PromisorPackfileWriter`, `LooseObjectStorer`,
  `PackedObjectStorer`, `DeltaObjectStorer`, `io.Closer`, the filesystem
  view, ...) to the wrapped storer, so type assertions on
  `Repository.Storer` keep the meaning they had before wrapping.

This is what keeps checkout, diff and `log -p` working on a blobless
clone: any object read that misses goes to the promisor remote instead of
erroring.

## Batching

One backfill call is one network round trip regardless of how many
objects are missing, mirroring git's `promisor_remote_get_direct`:

- Bulk operations collect what they need up front. Checkout/reset
  computes the blob set of the pending changes and fetches every missing
  blob in a single fetch before materialising files, so a checkout
  touching N missing blobs costs one request, not N.
- Point reads (a single `BlobObject`, one commit's patch) fetch just
  what they asked for; concurrent misses on different goroutines are
  serialised so the same object is never fetched twice.

The backfill fetch is a protocol v2 `fetch` command whose wants are the
missing OIDs (no ref advertisement, no negotiation), with the remote's
recorded `partialclonefilter` reapplied — as git's promisor fetch does.
Serving such a request requires the remote to allow fetching objects by
OID (`uploadpack.allowAnySHA1InWant`), which is how promisor services
such as GitHub are configured.

Backfill connects to the promisor remote with its configured URL; it does
not reuse the `ClientOptions` (credentials) of the clone or fetch that
created the partial clone, since those are per-call and git's own model
here is external state (credential helpers). Private promisor remotes
therefore need a URL or environment that authenticates without them.

## Landing in the object database

A backfill pack is stored exactly like the original filtered clone's
pack: through `PromisorPackfileWriter`, which writes the `.pack`, its
`.idx` and the `.promisor` marker together. The objects are therefore
readable by go-git and by real git from that point on, and `git fsck`
stays clean. go-git does not maintain a multi-pack-index (midx); if one
is introduced it must include promisor packs like any other pack and
preserve the `.promisor` markers, since those markers — not the midx —
are what legitimise the absent objects.

## Servers without filter support

If the server does not advertise the `filter` capability, a filtered
clone or fetch falls back to an unfiltered one (with a
`filtering not recognized` warning on the progress stream, as git
prints). The capability check runs before any pack data is written, so
the retry starts clean and the repository is left complete: no promisor
configuration is recorded and no objects are missing. Leaving a
half-filtered repository whose missing objects have nowhere to come from
is exactly what this avoids.

## Tests

`promisor_test.go` covers the contract end to end:

- blobless clone against real `git http-backend`, then checkout
  backfilling every missing blob in one batched fetch, with file contents
  matching `git show` at the same commit;
- lazy backfill of a single blob on first read, exactly one fetch, then
  served locally;
- `log -p`-style patch generation on a partial clone (this is the test
  that catches negotiating a filter without ever backfilling);
- fallback to a full clone when the server cannot honour filters;
- acceptance against GitHub (`TestPartialCloneAgainstGitHub`): blobless
  clone of a public repository, then backfill on first use. It is skipped
  under `-short` and when github.com is unreachable.
