package git

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/internal/test/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

// filterServer serves a bare repository over HTTP with real git, via
// git-http-backend(1). The server side of the wire protocol is genuine
// upload-pack, so capability advertisement, filtering and promisor semantics
// are git's own rather than a reimplementation's idea of them.
type filterServer struct {
	t   *testing.T
	src string // path of the bare repository being served
	srv *httptest.Server

	mu         sync.Mutex
	fetchPosts int
}

// newFilterServer seeds a source repository with one commit per entry in
// commits (each entry rewrites every file) and serves it bare over HTTP.
// allowFilter controls uploadpack.allowFilter on the server, so tests can
// also exercise a server that cannot honour filters at all.
func newFilterServer(t *testing.T, commits []map[string]string, allowFilter bool) *filterServer {
	t.Helper()
	requireGitPartialClone(t)

	base := t.TempDir()
	src := filepath.Join(base, "src.git")
	seed := filepath.Join(base, "seed")

	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.MkdirAll(seed, 0o755))

	initBare := gitenv.Command("git", "init", "-q", "--bare", src)
	out, err := initBare.CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", out)

	// The seed pushes main, so HEAD has to point there for the clone to
	// resolve it; init's default (master, under gitenv) would stay unborn.
	git(t, src, "symbolic-ref", "HEAD", "refs/heads/main")

	if allowFilter {
		git(t, src, "config", "uploadpack.allowFilter", "true")
	}
	// Backfill asks for objects by OID rather than by ref, which a server
	// only accepts with allowAnySHA1InWant, the same setting promisor
	// services such as GitHub run with.
	git(t, src, "config", "uploadpack.allowAnySHA1InWant", "true")

	initSeed := gitenv.Command("git", "init", "-q", seed)
	out, err = initSeed.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	git(t, seed, "config", "user.email", "test@example.com")
	git(t, seed, "config", "user.name", "test")

	for _, files := range commits {
		for name, content := range files {
			p := filepath.Join(seed, name)
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		}
		git(t, seed, "add", ".")
		git(t, seed, "commit", "-qm", "commit")
	}
	git(t, seed, "branch", "-M", "main")
	git(t, seed, "remote", "add", "origin", src)
	git(t, seed, "push", "-q", "origin", "main")

	s := &filterServer{t: t, src: src}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

// ServeHTTP speaks CGI to git http-backend and counts fetch commands, which
// is how the tests tell one batched backfill from one fetch per object.
func (s *filterServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		require.NoError(s.t, err)
	}
	if r.Method == http.MethodPost && bytes.Contains(body, []byte("command=fetch")) {
		s.mu.Lock()
		s.fetchPosts++
		s.mu.Unlock()
	}

	cmd := gitenv.Command("git", "http-backend")
	cmd.Env = append(cmd.Env,
		"GIT_PROJECT_ROOT="+filepath.Dir(s.src),
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO="+r.URL.Path,
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"),
		"REMOTE_ADDR=127.0.0.1",
	)
	cmd.Stdin = bytes.NewReader(body)

	out, err := cmd.Output()
	require.NoError(s.t, err, "git http-backend")

	head, payload, found := bytes.Cut(out, []byte("\r\n\r\n"))
	require.True(s.t, found, "CGI response without header terminator: %q", out)

	status := http.StatusOK
	for line := range strings.Lines(string(head)) {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(k, "Status") {
			_, _ = fmt.Sscanf(strings.TrimSpace(v), "%d", &status)
			continue
		}
		w.Header().Add(k, strings.TrimSpace(v))
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// fetches reports how many fetch commands the server has answered. Each one
// is a network round trip; the batching assertions compare against it.
func (s *filterServer) fetches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetchPosts
}

func (s *filterServer) url() string { return s.srv.URL + "/" + filepath.Base(s.src) }

// show returns file's content at ref in the source repository, the oracle
// for what a backfilled blob must contain.
func (s *filterServer) show(ref, file string) string {
	s.t.Helper()
	return strings.TrimSuffix(git(s.t, s.src, "show", ref+":"+file), "\n")
}

// blobHash returns the OID of file at ref in the source repository.
func (s *filterServer) blobHash(ref, file string) plumbing.Hash {
	s.t.Helper()
	out := git(s.t, s.src, "rev-parse", ref+":"+file)
	return plumbing.NewHash(strings.TrimSpace(out))
}

var partialCloneSeedCommits = []map[string]string{
	{"a.txt": "a1\n", "b.txt": "b1\n", "dir/c.txt": "c1\n"},
	{"a.txt": "a2\n", "b.txt": "b2\n", "dir/c.txt": "c2\n"},
	{"a.txt": "a3\n", "b.txt": "b3\n", "dir/c.txt": "c3\n"},
}

// cloneFiltered clones the served repository with the given filter and no
// checkout, so the tests start from a partial clone that is genuinely
// missing objects, and returns the repository and its path.
func cloneFiltered(t *testing.T, s *filterServer, filter packp.Filter) (*Repository, string) {
	t.Helper()

	dst := filepath.Join(t.TempDir(), "clone")
	r, err := PlainClone(dst, &CloneOptions{
		URL:        s.url(),
		Filter:     filter,
		NoCheckout: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	cfg, err := r.Config()
	require.NoError(t, err)
	origin := cfg.Remotes[DefaultRemoteName]
	require.NotNil(t, origin)
	assert.True(t, origin.Promisor, "a filtered clone must record its promisor remote")
	assert.Equal(t, string(filter), origin.PartialCloneFilter)

	assert.NotEmpty(t, promisorMarkers(t, dst), "filtered packs must be promisor-marked")
	requireFsckClean(t, dst)
	return r, dst
}

// TestPartialCloneCheckoutBackfillsBlobs is the end-to-end contract: a
// blobless clone can check out its worktree, with the missing blobs fetched
// from the promisor remote in one batch, and the result matches what git
// has at the same commit.
func TestPartialCloneCheckoutBackfillsBlobs(t *testing.T) {
	t.Parallel()

	s := newFilterServer(t, partialCloneSeedCommits, true)
	r, dst := cloneFiltered(t, s, packp.FilterBlobNone())

	// The blobs of HEAD are genuinely absent before the checkout, or the
	// rest of the test proves nothing.
	for _, f := range []string{"a.txt", "b.txt", "dir/c.txt"} {
		h := s.blobHash("HEAD", f)
		assert.Error(t, r.Storer.HasEncodedObject(h), "%s should be missing before checkout", f)
	}

	before := s.fetches()
	w, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, w.Checkout(&CheckoutOptions{Branch: "refs/heads/main"}))

	assert.Equal(t, 1, s.fetches()-before,
		"checkout must fetch its missing blobs in one batch, not one fetch per blob")

	for _, f := range []string{"a.txt", "b.txt", "dir/c.txt"} {
		content, err := os.ReadFile(filepath.Join(dst, f))
		require.NoError(t, err)
		assert.Equal(t, s.show("HEAD", f), strings.TrimSuffix(string(content), "\n"),
			"checked-out %s must match git's content at HEAD", f)
	}

	requireFsckClean(t, dst)
}

// TestPartialCloneBackfillsBlobOnDemand covers the lazy path: reading a
// single missing object fetches it from the promisor remote, once.
func TestPartialCloneBackfillsBlobOnDemand(t *testing.T) {
	t.Parallel()

	s := newFilterServer(t, partialCloneSeedCommits, true)
	r, dst := cloneFiltered(t, s, packp.FilterBlobNone())

	h := s.blobHash("HEAD", "a.txt")
	require.Error(t, r.Storer.HasEncodedObject(h))

	before := s.fetches()
	b, err := r.BlobObject(h)
	require.NoError(t, err)
	assert.Equal(t, 1, s.fetches()-before, "one missing blob must cost exactly one fetch")

	rd, err := b.Reader()
	require.NoError(t, err)
	content, err := io.ReadAll(rd)
	require.NoError(t, err)
	assert.Equal(t, s.show("HEAD", "a.txt"), strings.TrimSuffix(string(content), "\n"))

	// The object is local now: reading it again must not go back to the
	// remote, and a presence probe must see it.
	require.NoError(t, r.Storer.HasEncodedObject(h))
	_, err = r.BlobObject(h)
	require.NoError(t, err)
	assert.Equal(t, 1, s.fetches()-before, "a backfilled object must be served locally")

	requireFsckClean(t, dst)
}

// TestPartialCloneLogPatchBackfills covers history inspection on a partial
// clone: a commit's patch reads blobs from both sides of the diff, all of
// which the filter withheld. This is the regression test for negotiating a
// filter without ever backfilling: the patch cannot be produced without the
// missing objects.
func TestPartialCloneLogPatchBackfills(t *testing.T) {
	t.Parallel()

	s := newFilterServer(t, partialCloneSeedCommits, true)
	r, _ := cloneFiltered(t, s, packp.FilterBlobNone())

	head, err := r.Head()
	require.NoError(t, err)
	c, err := r.CommitObject(head.Hash())
	require.NoError(t, err)

	parent, err := c.Parent(0)
	require.NoError(t, err)

	patch, err := parent.Patch(c)
	require.NoError(t, err)
	assert.Contains(t, patch.String(), "+a3\n", "patch must contain the backfilled blob content")
}

// TestPartialCloneFilterFallback covers a server that cannot honour filters:
// the clone must fall back to a full fetch, leaving a complete repository
// with no promisor state behind.
func TestPartialCloneFilterFallback(t *testing.T) {
	t.Parallel()

	s := newFilterServer(t, partialCloneSeedCommits, false)

	var progress bytes.Buffer
	dst := filepath.Join(t.TempDir(), "clone")
	r, err := PlainClone(dst, &CloneOptions{
		URL:      s.url(),
		Filter:   packp.FilterBlobNone(),
		Progress: &progress,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	assert.Contains(t, progress.String(), "filtering not recognized",
		"the fallback must be announced, as git does")

	cfg, err := r.Config()
	require.NoError(t, err)
	origin := cfg.Remotes[DefaultRemoteName]
	require.NotNil(t, origin)
	assert.False(t, origin.Promisor, "a server without filter support must not be recorded as promisor")
	assert.Empty(t, origin.PartialCloneFilter)
	assert.Empty(t, promisorMarkers(t, dst), "an unfiltered clone has no promisor packs")

	// The clone is complete: every object git lists is present locally, and
	// the checkout needed no backfill.
	out := git(t, s.src, "rev-list", "--objects", "--all")
	for line := range strings.Lines(out) {
		oid, _, _ := strings.Cut(line, " ")
		oid = strings.TrimSpace(oid)
		if oid == "" {
			continue
		}
		assert.NoError(t, r.Storer.HasEncodedObject(plumbing.NewHash(oid)),
			"object %s must be local after the fallback to a full clone", oid)
	}

	for _, f := range []string{"a.txt", "b.txt", "dir/c.txt"} {
		content, err := os.ReadFile(filepath.Join(dst, f))
		require.NoError(t, err)
		assert.Equal(t, s.show("HEAD", f), strings.TrimSuffix(string(content), "\n"))
	}

	requireFsckClean(t, dst)
}

// TestPartialCloneAgainstGitHub is the acceptance check against a real
// hosting service: blobless clone of a small public repository, then
// backfill on first use. It is skipped in -short mode and whenever GitHub
// is unreachable.
func TestPartialCloneAgainstGitHub(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("acceptance test disabled: -short")
	}
	if !githubReachable() {
		t.Skip("acceptance test disabled: github.com unreachable")
	}

	const repoURL = "https://github.com/git-fixtures/basic.git"

	dst := filepath.Join(t.TempDir(), "clone")
	r, err := PlainClone(dst, &CloneOptions{
		URL:        repoURL,
		Filter:     packp.FilterBlobNone(),
		NoCheckout: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	cfg, err := r.Config()
	require.NoError(t, err)
	origin := cfg.Remotes[DefaultRemoteName]
	require.NotNil(t, origin)
	assert.True(t, origin.Promisor)
	assert.Equal(t, "blob:none", origin.PartialCloneFilter)

	head, err := r.Head()
	require.NoError(t, err)

	// Collect the blobs of the whole history. Hosting services may send
	// some blobs anyway (git's own blobless clone of this repository keeps
	// the HEAD blobs), so the assertion is not that every blob is absent
	// but that the filter left genuinely missing ones — and that those are
	// fetched on first use, with content that hashes back to its OID.
	//
	// The walk reads tree entries only: iterating files would open every
	// blob and answer the missing question by fetching them.
	iter, err := r.Log(&LogOptions{From: head.Hash()})
	require.NoError(t, err)

	var missing []plumbing.Hash
	var walkTree func(t *object.Tree) error
	walkTree = func(t *object.Tree) error {
		for _, e := range t.Entries {
			if e.Mode == filemode.Dir {
				sub, err := t.Tree(e.Name)
				if err != nil {
					return err
				}
				if err := walkTree(sub); err != nil {
					return err
				}
				continue
			}
			if e.Mode == filemode.Submodule {
				continue
			}
			if r.Storer.HasEncodedObject(e.Hash) != nil {
				missing = append(missing, e.Hash)
			}
		}
		return nil
	}

	err = iter.ForEach(func(c *object.Commit) error {
		tree, err := c.Tree()
		if err != nil {
			return err
		}
		return walkTree(tree)
	})
	require.NoError(t, err)
	require.NotEmpty(t, missing,
		"a blob:none clone must be missing blobs, or the filter did nothing")

	for _, h := range missing {
		b, err := r.BlobObject(h)
		require.NoError(t, err)
		rd, err := b.Reader()
		require.NoError(t, err)
		content, err := io.ReadAll(rd)
		require.NoError(t, err)
		require.NoError(t, rd.Close())

		obj := r.Storer.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		obj.SetSize(int64(len(content)))
		w, err := obj.Writer()
		require.NoError(t, err)
		_, err = w.Write(content)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		assert.Equal(t, h, obj.Hash(), "backfilled content must hash to its OID")
	}
}

func githubReachable() bool {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Head("https://github.com")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}
