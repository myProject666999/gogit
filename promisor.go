package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/utils/ioutil"
	xstorage "github.com/go-git/go-git/v6/x/storage"
)

// promisorRemotes returns the configured remotes that promised to serve the
// objects a filtered fetch left out, in configuration order. A repository is
// a partial clone when this is non-empty; git records the same state as
// remote.<name>.promisor plus remote.<name>.partialclonefilter.
func (r *Repository) promisorRemotes() ([]*config.RemoteConfig, error) {
	cfg, err := r.Storer.Config()
	if err != nil {
		return nil, err
	}

	var remotes []*config.RemoteConfig
	for _, rc := range cfg.Remotes {
		if rc.Promisor && len(rc.URLs) > 0 {
			remotes = append(remotes, rc)
		}
	}
	return remotes, nil
}

// enablePartialCloneBackfill wraps the repository storer so that reading an
// object a promisor remote withheld fetches it on demand instead of failing
// with plumbing.ErrObjectNotFound. It is a no-op when the repository is not a
// partial clone or the wrapper is already in place.
func (r *Repository) enablePartialCloneBackfill() error {
	switch r.Storer.(type) {
	case *promisorStorer, *promisorFSStorer:
		return nil
	}

	remotes, err := r.promisorRemotes()
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		return nil
	}

	r.Storer = newPromisorStorer(r.Storer, r)
	return nil
}

// backfill fetches the given objects from a promisor remote in a single
// fetch, storing the result in the local object database. Objects already
// present are dropped from the request, so callers can pass a superset of
// what they need. It is a no-op for repositories that are not partial clones.
//
// One call is one network round trip no matter how many objects are missing,
// mirroring git's promisor_remote_get_direct: bulk users such as checkout
// collect everything they need up front and ask once, rather than fetching
// object by object.
func (r *Repository) backfill(ctx context.Context, hashes []plumbing.Hash) error {
	st := unwrapPromisorStorer(r.Storer)

	seen := make(map[plumbing.Hash]struct{}, len(hashes))
	var missing []plumbing.Hash
	for _, h := range hashes {
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		if st.HasEncodedObject(h) == nil {
			continue
		}
		missing = append(missing, h)
	}
	if len(missing) == 0 {
		return nil
	}

	remotes, err := r.promisorRemotes()
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		return fmt.Errorf("cannot backfill %d objects: no promisor remote configured", len(missing))
	}

	var fetchErr error
	for _, remote := range remotes {
		fetchErr = fetchObjects(ctx, st, remote, missing)
		if fetchErr == nil {
			return nil
		}
	}
	return fetchErr
}

// fetchObjects fetches exactly the given objects from remote in one request.
// The remote's recorded partialclonefilter is reapplied, as git does on
// promisor fetches (promisor-remote.c passes --filter back), so the incoming
// pack is again a promisor pack and is marked accordingly.
func fetchObjects(ctx context.Context, st storage.Storer, remote *config.RemoteConfig, hashes []plumbing.Hash) (err error) {
	cl, req, err := newClient(remote.URLs[0], nil)
	if err != nil {
		return err
	}

	req.Command = transport.UploadPackService
	if cfg, cfgErr := st.Config(); cfgErr == nil && cfg != nil {
		req.Protocol = cfg.Protocol.Version
	}

	sess, err := cl.Handshake(ctx, req)
	if err != nil {
		return err
	}
	defer ioutil.CheckClose(sess, &err)

	return sess.Fetch(ctx, st, &transport.FetchRequest{
		Wants:  hashes,
		Filter: packp.Filter(remote.PartialCloneFilter),
	})
}

// promisorStorer wraps a storage.Storer so that reading an object the

// unwrapPromisorStorer returns the storer behind a backfilling wrapper, or
// its argument unchanged. Reachability walks (repack, prune) use it to read
// only what is local: fetching the objects a promisor remote withheld is
// neither wanted nor always possible there, and the walk already tolerates
// their absence.
func unwrapPromisorStorer(s storage.Storer) storage.Storer {
	switch w := s.(type) {
	case *promisorStorer:
		return w.Storer
	case *promisorFSStorer:
		return w.Storer
	}
	return s
}

// fsBased is implemented by storers that expose their underlying filesystem.
type fsBased interface {
	Filesystem() billy.Filesystem
}

// promisor remote withheld fetches it on demand. Only EncodedObject and
// EncodedObjectSize fetch: presence probes such as HasEncodedObject must
// report what is actually local, the same split git makes with
// OBJECT_INFO_SKIP_FETCH_OBJECT.
//
// The embedded Storer forwards every method not overridden here, and the
// optional storage interfaces are re-implemented below by delegating to the
// wrapped storer, so type assertions on the repository's Storer keep the
// meaning they had before wrapping.
type promisorStorer struct {
	storage.Storer
	repo *Repository
	// mu serializes backfills so concurrent readers of the same missing
	// object collapse into one fetch.
	mu sync.Mutex
}

var _ storage.Storer = (*promisorStorer)(nil)

// newPromisorStorer wraps inner with lazy backfill. The filesystem-specific
// view is only added when inner is filesystem based, mirroring the fsBased
// assertion repository code makes on the Storer.
func newPromisorStorer(inner storage.Storer, repo *Repository) storage.Storer {
	w := &promisorStorer{Storer: inner, repo: repo}
	if _, ok := inner.(fsBased); ok {
		return &promisorFSStorer{w}
	}
	return w
}

// backfillAndRetry fetches h from a promisor remote and retries op. The
// second result of op is returned verbatim when the object is still absent
// after the fetch: a promisor only promises the objects it filtered out, so
// a genuine miss must surface as ErrObjectNotFound, not as another fetch.
func (s *promisorStorer) backfillAndRetry(h plumbing.Hash, op func() (any, error)) (any, error) {
	v, err := op()
	if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return v, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Another reader may have fetched the object while this one waited.
	if s.Storer.HasEncodedObject(h) == nil {
		return op()
	}

	if berr := s.repo.backfill(context.Background(), []plumbing.Hash{h}); berr != nil {
		return nil, berr
	}
	return op()
}

func (s *promisorStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	v, err := s.backfillAndRetry(h, func() (any, error) {
		return s.Storer.EncodedObject(t, h)
	})
	if err != nil {
		return nil, err
	}
	return v.(plumbing.EncodedObject), nil
}

func (s *promisorStorer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	v, err := s.backfillAndRetry(h, func() (any, error) {
		return s.Storer.EncodedObjectSize(h)
	})
	if err != nil {
		return 0, err
	}
	return v.(int64), nil
}

// The optional interfaces below delegate to the wrapped storer and reproduce,
// for a storer that lacks them, the outcome callers would have got from the
// plain storer.

func (s *promisorStorer) PromisorObjectPacks() ([]plumbing.Hash, error) {
	if pos, ok := s.Storer.(storer.PromisorObjectStorer); ok {
		return pos.PromisorObjectPacks()
	}
	return nil, nil
}

func (s *promisorStorer) PromisorPackfileWriter(marker string) (io.WriteCloser, error) {
	if pw, ok := s.Storer.(storer.PromisorPackfileWriter); ok {
		return pw.PromisorPackfileWriter(marker)
	}
	if !packfile.SupportsPromisorPacks(s.Storer) {
		return nil, packfile.ErrPromisorPacksUnsupported
	}
	// The wrapped storer writes no packfiles (memory storage), so the pack
	// is stored as individual objects, where there is no pack to mark.
	pr, pw := io.Pipe()
	w := &loosePromisorWriter{pw: pw, done: make(chan error, 1)}
	go func() {
		w.done <- packfile.UpdateObjectStorage(s.Storer, pr)
	}()
	return w, nil
}

// loosePromisorWriter pipes the incoming pack to a parser goroutine that
// stores the objects individually. Close ends the stream and reports the
// parse result, so a malformed pack does not fail silently.
type loosePromisorWriter struct {
	pw   *io.PipeWriter
	done chan error
}

func (w *loosePromisorWriter) Write(p []byte) (int, error) { return w.pw.Write(p) }

func (w *loosePromisorWriter) Close() error {
	if err := w.pw.Close(); err != nil {
		return err
	}
	return <-w.done
}

func (s *promisorStorer) PackfileWriter() (io.WriteCloser, error) {
	if pw, ok := s.Storer.(storer.PackfileWriter); ok {
		return pw.PackfileWriter()
	}
	return nil, errors.New("Repository storer is not a storer.PackfileWriter")
}

func (s *promisorStorer) ObjectPacks() ([]plumbing.Hash, error) {
	if pos, ok := s.Storer.(storer.PackedObjectStorer); ok {
		return pos.ObjectPacks()
	}
	return nil, ErrPackedObjectsNotSupported
}

func (s *promisorStorer) DeleteOldObjectPackAndIndex(h plumbing.Hash, t time.Time) error {
	if pos, ok := s.Storer.(storer.PackedObjectStorer); ok {
		return pos.DeleteOldObjectPackAndIndex(h, t)
	}
	return ErrPackedObjectsNotSupported
}

func (s *promisorStorer) ForEachObjectHash(fun func(plumbing.Hash) error) error {
	if los, ok := s.Storer.(storer.LooseObjectStorer); ok {
		return los.ForEachObjectHash(fun)
	}
	return ErrLooseObjectsNotSupported
}

func (s *promisorStorer) LooseObjectTime(h plumbing.Hash) (time.Time, error) {
	if los, ok := s.Storer.(storer.LooseObjectStorer); ok {
		return los.LooseObjectTime(h)
	}
	return time.Time{}, ErrLooseObjectsNotSupported
}

func (s *promisorStorer) DeleteLooseObject(h plumbing.Hash) error {
	if los, ok := s.Storer.(storer.LooseObjectStorer); ok {
		return los.DeleteLooseObject(h)
	}
	return ErrLooseObjectsNotSupported
}

func (s *promisorStorer) DeltaObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if dos, ok := s.Storer.(storer.DeltaObjectStorer); ok {
		return dos.DeltaObject(t, h)
	}
	// Callers fall back to a fully resolved object when the storer has no
	// delta view (packfile.DeltaSelector does exactly this).
	return s.EncodedObject(t, h)
}

func (s *promisorStorer) Init() error {
	if i, ok := s.Storer.(storer.Initializer); ok {
		return i.Init()
	}
	return nil
}

func (s *promisorStorer) Close() error {
	if c, ok := s.Storer.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (s *promisorStorer) SupportsExtension(name, value string) bool {
	if ec, ok := s.Storer.(xstorage.ExtensionChecker); ok {
		return ec.SupportsExtension(name, value)
	}
	return false
}

// promisorFSStorer adds the filesystem view to promisorStorer, present only
// when the wrapped storer has one, so the fsBased assertion in repository
// code keeps matching exactly the storages it matched before wrapping.
type promisorFSStorer struct {
	*promisorStorer
}

func (s *promisorFSStorer) Filesystem() billy.Filesystem {
	return s.Storer.(fsBased).Filesystem()
}
