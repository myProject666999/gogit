package git

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage"
)

// promisorRemoteName returns the name of the remote that promises the objects
// this partial clone is missing, or "" when the repository is not a partial
// clone. Git records that name in extensions.partialClone; repositories go-git
// created itself additionally mark the remote with remote.<name>.promisor,
// which is read here as a fallback for clones produced before that extension
// was recorded (or by another tool).
func promisorRemoteName(cfg *config.Config) string {
	if cfg.Extensions.PartialClone != "" {
		return cfg.Extensions.PartialClone
	}
	for name, remote := range cfg.Remotes {
		if remote.Promisor {
			return name
		}
	}
	return ""
}

// promisorFetchFunc builds the function the object storage calls when a lookup
// proves an object absent. The returned function must fetch exactly the
// requested hashes from the promisor remote and store them before returning;
// coalescing several lookups into one call is the storage's responsibility.
//
// It returns nil when the storer cannot wire a promisor fetch: storage that
// keeps no remote config (bare in-memory stores opened for reading), or a full
// clone, simply keep returning plumbing.ErrObjectNotFound for absent objects.
func promisorFetchFunc(s storage.Storer, cfg *config.Config, remoteCfg *config.RemoteConfig) func([]plumbing.Hash) error {
	if remoteCfg == nil || len(remoteCfg.URLs) == 0 {
		return nil
	}

	url0 := remoteCfg.URLs[0]
	filter := packp.Filter(remoteCfg.PartialCloneFilter)

	return func(hashes []plumbing.Hash) error {
		return fetchPromisorObjects(s, url0, filter, hashes)
	}
}

// fetchPromisorObjects asks the promisor remote for a batch of objects using
// the same filter the clone was made with.
//
// The filter is safe to resend: protocol v2 applies it only to the graph the
// server traverses, never to objects named explicitly as wants
// (list-objects-filter.c filter_list_objects: the root set is exempted). The
// explicitly wanted objects therefore come back complete, while their
// neighbourhood stays filtered, which is exactly canonical git's promisor
// fetch (fetch-pack.c fetch_objects reuses filter_options).
//
// The pack the server returns is stored as a promisor pack in turn: it can
// reference objects still withheld, so an unmarked pack would reintroduce the
// broken links the initial fetch's marker prevents.
func fetchPromisorObjects(s storage.Storer, rawURL string, filter packp.Filter, hashes []plumbing.Hash) error {
	if len(hashes) == 0 {
		return nil
	}

	cl, req, err := newClient(rawURL, nil)
	if err != nil {
		return err
	}

	req.Command = transport.UploadPackService
	// Promisor fetches require protocol v2: arbitrary wants with no want-refs
	// and the filter argument are v2 fetch-command features. The configured
	// default is v2 already; pin it so a local override of protocol.version
	// cannot make the lazy fetch speak v0 and mis-handle the wants.
	req.Protocol = protocol.V2

	ctx := context.Background()
	sess, err := cl.Handshake(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	fetchReq := &transport.FetchRequest{
		Wants: hashes,
		// No haves and no negotiation: these objects are missing locally by
		// definition. Git's promisor fetch sends the oids the same way.
		Haves:  nil,
		Filter: filter,
	}

	if err := sess.Fetch(ctx, s, fetchReq); err != nil {
		// Defensive: a v2 request for explicit wants should always produce a
		// pack, but transport implementations may surface no-change on an
		// unexpected shortcut. A successful no-change leaves the object
		// absent, so normalise it into an error the caller retries visibly.
		if errors.Is(err, transport.ErrNoChange) {
			return fmt.Errorf("promisor remote sent no objects for %d missing hashes", len(hashes))
		}
		return err
	}

	return nil
}

// wireMissingObjectFetcher connects a storer that accepts a promisor fetch
// callback to the repository's promisor remote, if there is one. Storers that
// do not implement storer.MissingObjectFetchSetter (memory storage, custom
// stores) are left untouched.
func wireMissingObjectFetcher(s storage.Storer, cfg *config.Config) error {
	setter, ok := s.(storer.MissingObjectFetchSetter)
	if !ok {
		return nil
	}

	name := promisorRemoteName(cfg)
	if name == "" {
		return nil
	}

	remoteCfg, ok := cfg.Remotes[name]
	if !ok {
		return nil
	}

	setter.SetMissingObjectFetcher(promisorFetchFunc(s, cfg, remoteCfg))
	return nil
}
