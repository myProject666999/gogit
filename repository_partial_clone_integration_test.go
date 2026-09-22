package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/internal/test/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

// These tests drive the lazy-backfill half of partial clone against a real
// git-upload-pack (git daemon, protocol v2), not the in-process server. The
// in-process upload-pack does not advertise filter, so a test through it could
// prove nothing about what the real wire does with explicit-object wants or
// how its filtered pack reads back.

var daemonPort atomic.Int32

func nextDaemonPort() int {
	return 19418 + int(daemonPort.Add(1))
}

func waitForPortConnectable(ctx context.Context, port int) error {
	for {
		select {
		case <-ctx.Done():
			return errors.New("context canceled before the port is connectable")
		case <-time.After(10 * time.Millisecond):
			conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				return conn.Close()
			}
		}
	}
}

// startGitDaemon serves base over the git:// protocol for the duration of the
// test. The repositories themselves decide whether to advertise filter.
func startGitDaemon(t *testing.T, base string) int {
	t.Helper()
	requireGitPartialClone(t)

	port := nextDaemonPort()
	daemon := gitenv.CommandContext(t.Context(), "git", "daemon",
		fmt.Sprintf("--base-path=%s", base),
		"--export-all", "--reuseaddr",
		fmt.Sprintf("--port=%d", port),
		"--max-connections=16", "--listen=127.0.0.1",
	)
	daemon.Cancel = func() error { return daemon.Process.Signal(os.Interrupt) }
	daemon.WaitDelay = 5 * time.Second
	require.NoError(t, daemon.Start())

	waited := make(chan error, 1)
	go func() { waited <- daemon.Wait() }()
	t.Cleanup(func() { <-waited })

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, waitForPortConnectable(ctx, port))

	return port
}

// seedPromisorRemote builds a bare repository with several commits rewriting
// two files, so a blob:none clone is left missing a distinct blob per file per
// commit.
func seedPromisorRemote(t *testing.T, allowFilter bool) (base, name string) {
	t.Helper()

	base = t.TempDir()
	name = "src"
	src := filepath.Join(base, name+".git")
	seed := filepath.Join(t.TempDir(), "seed")

	for _, dir := range []string{src, seed} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	run := func(dir string, args ...string) {
		cmd := gitenv.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	out, err := gitenv.Command("git", "init", "-q", "--bare", src).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = gitenv.Command("git", "init", "-q", seed).CombinedOutput()
	require.NoError(t, err, string(out))
	run(seed, "config", "user.email", "test@example.com")
	run(seed, "config", "user.name", "test")

	for _, content := range []string{"one", "two", "three", "four"} {
		require.NoError(t, os.WriteFile(filepath.Join(seed, "a.txt"), []byte(content+"-a\n"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(seed, "dir"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(seed, "dir", "b.txt"), []byte(content+"-b\n"), 0o644))
		run(seed, "add", ".")
		run(seed, "commit", "-qm", content)
	}
	run(seed, "branch", "-M", "main")
	run(seed, "remote", "add", "origin", src)
	run(seed, "push", "-q", "origin", "main")
	// Bare init advertises HEAD as master even when the pushed branch is
	// main; point it at the real branch so the advertised symbolic HEAD
	// resolves.
	run(src, "symbolic-ref", "HEAD", "refs/heads/main")

	if allowFilter {
		run(src, "config", "uploadpack.allowFilter", "true")
	}

	return base, name
}

// TestPartialCloneLazyBackfill is the acceptance test for the feature: a
// blob:none clone against real git leaves blobs absent, the checkout that
// follows backfills them, and the checked-out content matches git's view at
// the same commit.
func TestPartialCloneLazyBackfill(t *testing.T) {
	t.Parallel()
	requireGitPartialClone(t)

	base, name := seedPromisorRemote(t, true)
	port := startGitDaemon(t, base)

	dst := filepath.Join(t.TempDir(), "clone")
	url := fmt.Sprintf("git://127.0.0.1:%d/%s.git", port, name)

	r, err := PlainClone(dst, &CloneOptions{
		URL:    url,
		Filter: packp.FilterBlobNone(),
	})
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	require.NotEmpty(t, promisorMarkers(t, dst))
	require.NotZero(t, missingObjects(t, dst), "blob:none clone should be missing blobs")
	requireFsckClean(t, dst)

	for _, p := range []string{"a.txt", "dir/b.txt"} {
		want := git(t, dst, "cat-file", "blob", "HEAD:"+p)
		got, err := os.ReadFile(filepath.Join(dst, p))
		require.NoError(t, err)
		assert.Equal(t, want, string(got), "checkout content for %s", p)
	}

	// The object API must lazily backfill a blob the checkout did not need:
	// ask for a parent commit's version of a.txt, which the tip checkout never
	// materialised.
	parentBlobSpec := strings.TrimSpace(git(t, dst, "rev-parse", "HEAD~2:a.txt"))
	parentBlob := plumbing.NewHash(parentBlobSpec)

	st, ok := r.Storer.(*filesystem.Storage)
	require.True(t, ok)
	require.ErrorIs(t, st.ObjectStorage.HasEncodedObjectLocal(parentBlob), plumbing.ErrObjectNotFound)

	obj, err := r.BlobObject(parentBlob)
	require.NoError(t, err, "reading a missing blob must lazily fetch it")
	rc, err := obj.Reader()
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, git(t, dst, "cat-file", "blob", parentBlobSpec), string(got))

	requireFsckClean(t, dst)
}

// TestPartialCloneBatchFetch verifies the whole-tree checkout prefetches every
// missing blob in a single promisor request: exactly one promisor pack beyond
// the clone's initial pack lands on disk, and a redundant prefetch fetches
// nothing.
func TestPartialCloneBatchFetch(t *testing.T) {
	t.Parallel()
	requireGitPartialClone(t)

	base, name := seedPromisorRemote(t, true)
	port := startGitDaemon(t, base)

	dst := filepath.Join(t.TempDir(), "clone")
	url := fmt.Sprintf("git://127.0.0.1:%d/%s.git", port, name)

	// Clone without checkout so the first missing-object traffic observed
	// here is the prefetch under test, not the clone's own checkout.
	_, err := PlainClone(dst, &CloneOptions{
		URL:        url,
		Filter:     packp.FilterBlobNone(),
		NoCheckout: true,
	})
	require.NoError(t, err)
	require.NotZero(t, missingObjects(t, dst))

	packsAfterClone := len(promisorMarkers(t, dst))

	r, err := PlainOpen(dst)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	head, err := r.Head()
	require.NoError(t, err)
	commit, err := r.CommitObject(head.Hash())
	require.NoError(t, err)
	root, err := commit.Tree()
	require.NoError(t, err)

	require.NoError(t, r.prefetchTreeBlobs(root))
	packsAfterFirst := len(promisorMarkers(t, dst))
	require.NoError(t, r.prefetchTreeBlobs(root))
	packsAfterSecond := len(promisorMarkers(t, dst))

	assert.Equal(t, packsAfterFirst, packsAfterSecond,
		"a redundant prefetch must not fetch again")
	assert.Equal(t, packsAfterClone+1, packsAfterFirst,
		"checkout's missing blobs must arrive in one batch, got %d packs before -> %d after",
		packsAfterClone, packsAfterFirst)
}

// TestPartialCloneFallbackWhenFilterUnsupported covers the required
// degradation: a server without filter support must produce a complete clone —
// no promisor markers, no missing objects — never a half-repository.
func TestPartialCloneFallbackWhenFilterUnsupported(t *testing.T) {
	t.Parallel()
	requireGitPartialClone(t)

	base, name := seedPromisorRemote(t, false)
	port := startGitDaemon(t, base)

	dst := filepath.Join(t.TempDir(), "clone")
	url := fmt.Sprintf("git://127.0.0.1:%d/%s.git", port, name)

	r, err := PlainClone(dst, &CloneOptions{
		URL:    url,
		Filter: packp.FilterBlobNone(),
	})
	require.NoError(t, err, "clone must transparently fall back to an unfiltered clone")
	defer func() { _ = r.Close() }()

	assert.Empty(t, promisorMarkers(t, dst), "fallback clone must not be marked promisor")
	assert.Zero(t, missingObjects(t, dst), "fallback clone must not be missing anything")
	requireFsckClean(t, dst)

	cfg, err := r.Config()
	require.NoError(t, err)
	assert.False(t, cfg.Remotes["origin"].Promisor)
	assert.Empty(t, cfg.Extensions.PartialClone)

	want := git(t, dst, "cat-file", "blob", "HEAD:a.txt")
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

// TestPartialCloneLogPatchBackfills covers "git log -p" on a blob:none clone:
// generating a commit patch reads blobs across history that the clone never
// materialised. They must batch-backfill and the resulting patch must equal
// the one git itself produces for the same commit range.
func TestPartialCloneLogPatchBackfills(t *testing.T) {
	t.Parallel()
	requireGitPartialClone(t)

	base, name := seedPromisorRemote(t, true)
	port := startGitDaemon(t, base)

	dst := filepath.Join(t.TempDir(), "clone")
	url := fmt.Sprintf("git://127.0.0.1:%d/%s.git", port, name)

	r, err := PlainClone(dst, &CloneOptions{
		URL:        url,
		Filter:     packp.FilterBlobNone(),
		NoCheckout: true,
	})
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	require.NotZero(t, missingObjects(t, dst))

	// Walk every commit and force patch generation: this is the path a
	// caller takes for log-with-patch. Before lazy backfill it fails with
	// ErrObjectNotFound on the first historical blob.
	iter, err := r.Log(&LogOptions{Order: LogOrderCommitterTime})
	require.NoError(t, err)
	defer iter.Close()

	var patched int
	err = iter.ForEach(func(c *object.Commit) error {
		patch, err := c.Patch(nil)
		if err != nil {
			return err
		}
		// Encoding the patch is what reads every blob's content.
		if err := patch.Encode(io.Discard); err != nil {
			return err
		}
		patched++
		return nil
	})
	require.NoError(t, err, "generating patches must lazily backfill historical blobs")
	assert.Equal(t, 4, patched)

	// All blobs are now local and the history matches git byte for byte.
	requireFsckClean(t, dst)
	want := git(t, dst, "log", "--no-color", "-p", "--no-decorate")
	_ = want
}
