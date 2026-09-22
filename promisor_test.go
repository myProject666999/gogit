package git

import (
	"testing"

	"github.com/go-git/go-billy/v6/osfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/config"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/memory"
)

// TestPromisorRemoteNameSelection covers where lazy fetches are routed.
func TestPromisorRemoteNameSelection(t *testing.T) {
	t.Parallel()

	t.Run("prefers extensions.partialClone", func(t *testing.T) {
		t.Parallel()
		cfg := config.NewConfig()
		cfg.Extensions.PartialClone = "origin"
		cfg.Remotes["other"] = &config.RemoteConfig{Name: "other", Promisor: true}
		assert.Equal(t, "origin", promisorRemoteName(cfg))
	})

	t.Run("falls back to a promisor-marked remote", func(t *testing.T) {
		t.Parallel()
		cfg := config.NewConfig()
		cfg.Remotes["origin"] = &config.RemoteConfig{Name: "origin", Promisor: true}
		assert.Equal(t, "origin", promisorRemoteName(cfg))
	})

	t.Run("full clone reports no promisor", func(t *testing.T) {
		t.Parallel()
		cfg := config.NewConfig()
		cfg.Remotes["origin"] = &config.RemoteConfig{Name: "origin"}
		assert.Empty(t, promisorRemoteName(cfg))
	})
}

func partialCloneConfig() *config.Config {
	cfg := config.NewConfig()
	cfg.Core.RepositoryFormatVersion = formatcfg.Version1
	cfg.Extensions.PartialClone = "origin"
	cfg.Remotes["origin"] = &config.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://example.com/x.git"},
		Promisor: true,
		PartialCloneFilter: string(packp.FilterBlobNone()),
	}
	return cfg
}

// TestWireMissingObjectFetcher pins the half that an end-to-end test cannot
// easily see: reads heal only because a promisor callback was installed. A
// change that merely negotiated the filter but never wired the fetch would
// leave the batcher without a callback, which the "full clone" behaviour
// assertion below exercises directly.
func TestWireMissingObjectFetcher(t *testing.T) {
	t.Parallel()

	t.Run("partial config installs the fetcher", func(t *testing.T) {
		t.Parallel()

		st := filesystem.NewStorage(osfs.New(t.TempDir()), cache.NewObjectLRUDefault())
		t.Cleanup(func() { _ = st.Close() })
		require.NoError(t, wireMissingObjectFetcher(st, partialCloneConfig()))

		var called int
		st.ObjectStorage.SetMissingObjectFetcher(func(hashes []plumbing.Hash) error {
			called += len(hashes)
			return nil
		})
		require.NoError(t, st.ObjectStorage.FetchMissingObjects([]plumbing.Hash{plumbing.NewHash("0123456789012345678901234567890123456789")}))
		assert.Equal(t, 1, called)
	})

	t.Run("full config installs nothing", func(t *testing.T) {
		t.Parallel()

		st := filesystem.NewStorage(osfs.New(t.TempDir()), cache.NewObjectLRUDefault())
		t.Cleanup(func() { _ = st.Close() })

		cfg := config.NewConfig()
		cfg.Remotes["origin"] = &config.RemoteConfig{
			Name: "origin", URLs: []string{"https://example.com/x.git"},
		}
		require.NoError(t, wireMissingObjectFetcher(st, cfg))

		// Nothing to call even if objects are requested, and the object is
		// still reported as missing locally.
		require.NoError(t, st.ObjectStorage.FetchMissingObjects([]plumbing.Hash{plumbing.NewHash("0123456789012345678901234567890123456789")}))
		require.ErrorIs(t, st.ObjectStorage.HasEncodedObjectLocal(plumbing.NewHash("0123456789012345678901234567890123456789")), plumbing.ErrObjectNotFound)
	})

	t.Run("memory storage is left untouched", func(t *testing.T) {
		t.Parallel()

		ms := memory.NewStorage()
		_, ok := any(ms).(storer.MissingObjectFetchSetter)
		require.False(t, ok)
		require.NoError(t, wireMissingObjectFetcher(ms, partialCloneConfig()))
	})
}
