package client

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/docker/notary/passphrase"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/store"
	"github.com/docker/notary/tuf/testutils"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

func newBlankRepo(t *testing.T, url string) *NotaryRepository {
	// Temporary directory where test files will be created
	tempBaseDir, err := ioutil.TempDir("", "notary-test-")
	require.NoError(t, err, "failed to create a temporary directory: %s", err)

	repo, err := NewNotaryRepository(tempBaseDir, "docker.com/notary", url,
		http.DefaultTransport, passphrase.ConstantRetriever("pass"))
	require.NoError(t, err)
	return repo
}

// If there's no local cache, we go immediately to check the remote server for
// root, and if it doesn't exist, we return ErrRepositoryNotExist. This happens
// with or without a force check (update for write).
func TestUpdateNotExistNoLocalCache(t *testing.T) {
	testUpdateNotExistNoLocalCache(t, false)
	testUpdateNotExistNoLocalCache(t, true)
}

func testUpdateNotExistNoLocalCache(t *testing.T, forWrite bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo := newBlankRepo(t, ts.URL)
	defer os.RemoveAll(repo.baseDir)

	// there is no metadata at all - this is a fresh repo, and the server isn't
	// aware of the root.
	_, err := repo.Update(forWrite)
	require.Error(t, err)
	require.IsType(t, ErrRepositoryNotExist{}, err)
}

// If there is a local cache, we use the local root as the trust anchor and we
// then an update. If the server has no root.json, we return an ErrRepositoryNotExist.
// If we force check (update for write), then it hits the server first, and
// still returns an ErrRepositoryNotExist.
func TestUpdateNotExistWithLocalCache(t *testing.T) {
	testUpdateNotExistWithLocalCache(t, false)
	testUpdateNotExistWithLocalCache(t, true)
}

func testUpdateNotExistWithLocalCache(t *testing.T, forWrite bool) {
	ts, _, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	// the repo has metadata, but the server is unaware of any metadata
	// whatsoever.
	_, err := repo.Update(forWrite)
	require.Error(t, err)
	require.IsType(t, ErrRepositoryNotExist{}, err)
}

// If there is a local cache, we use the local root as the trust anchor and we
// then an update. If the server has a root.json, but is missing other data,
// then we propagate the ErrMetaNotFound.  Same if we force check
// (update for write); the root exists, but other metadata doesn't.
func TestUpdateWithLocalCacheRemoteMissingMetadata(t *testing.T) {
	testUpdateWithLocalCacheRemoteMissingMetadata(t, false)
	testUpdateWithLocalCacheRemoteMissingMetadata(t, true)
}

func testUpdateWithLocalCacheRemoteMissingMetadata(t *testing.T, forWrite bool) {
	ts, m, _ := simpleTestServer(t)
	defer ts.Close()

	repo, _ := initializeRepo(t, data.ECDSAKey, "docker.com/notary", ts.URL, false)
	defer os.RemoveAll(repo.baseDir)

	rootJSON, err := repo.fileStore.GetMeta(data.CanonicalRootRole, maxSize)
	require.NoError(t, err)

	// the server should know about the root.json, and nothing else
	m.HandleFunc(
		fmt.Sprintf("/v2/docker.com/notary/_trust/tuf/root.json"),
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, string(rootJSON))
		})

	// the first thing the client tries to get is the timestamp - so that
	// will be the failed metadata update.
	_, err = repo.Update(forWrite)
	require.Error(t, err)
	require.IsType(t, store.ErrMetaNotFound{}, err)
	metaNotFound, ok := err.(store.ErrMetaNotFound)
	require.True(t, ok)
	require.Equal(t, data.CanonicalTimestampRole, metaNotFound.Resource)
}

// create a server that just serves static metadata files from a metaStore
func readOnlyServer(t *testing.T, cache store.MetadataStore) *httptest.Server {
	m := mux.NewRouter()
	m.HandleFunc("/v2/docker.com/notary/_trust/tuf/{role:.*}.json",
		func(w http.ResponseWriter, r *http.Request) {
			vars := mux.Vars(r)
			metaBytes, err := cache.GetMeta(vars["role"], maxSize)
			require.NoError(t, err)
			w.Write(metaBytes)
		})

	return httptest.NewServer(m)
}

func bumpVersions(t *testing.T, s *testutils.MetadataSwizzler) {
	for _, r := range s.Roles {
		require.NoError(t, s.OffsetMetadataVersion(r, 1))
	}
	require.NoError(t, s.UpdateSnapshotHashes())
	require.NoError(t, s.UpdateTimestampHash())
}

type unwritableStore struct {
	store.MetadataStore
	roleToNotWrite string
}

func (u *unwritableStore) SetMeta(role string, serverMeta []byte) error {
	if role == u.roleToNotWrite {
		return fmt.Errorf("Non-writable")
	}
	return u.MetadataStore.SetMeta(role, serverMeta)
}

// Update can succeed even if we cannot write any metadata to the repo (assuming
// no data in the repo)
func TestUpdateSucceedsEvenIfCannotWriteNewRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	serverMeta, _, err := testutils.NewRepoMetadata("docker.com/notary", "targets/a", "targets/a/b", "targets/a/b/c")
	require.NoError(t, err)

	ts := readOnlyServer(t, store.NewMemoryStore(serverMeta, nil))
	defer ts.Close()

	for role := range serverMeta {
		repo := newBlankRepo(t, ts.URL)
		repo.fileStore = &unwritableStore{MetadataStore: repo.fileStore, roleToNotWrite: role}
		_, err := repo.Update(false)

		if role == data.CanonicalRootRole {
			require.Error(t, err) // because checkRoot loads root from cache to check hashes
			continue
		} else {
			require.NoError(t, err)
		}

		for r, expected := range serverMeta {
			actual, err := repo.fileStore.GetMeta(r, maxSize)
			if r == role {
				require.Error(t, err)
				require.IsType(t, store.ErrMetaNotFound{}, err,
					"expected no data because unable to write for %s", role)
			} else {
				require.NoError(t, err, "problem getting repo metadata for %s", r)
				require.True(t, bytes.Equal(expected, actual),
					"%s: expected to update since only %s was unwritable", r, role)
			}
		}

		os.RemoveAll(repo.baseDir)
	}
}

// Update can succeed even if we cannot write any metadata to the repo (assuming
// existing data in the repo)
func TestUpdateSucceedsEvenIfCannotWriteExistingRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	serverMeta, cs, err := testutils.NewRepoMetadata("docker.com/notary", "targets/a", "targets/a/b", "targets/a/b/c")
	require.NoError(t, err)

	serverSwizzler := testutils.NewMetadataSwizzler("docker.com/notary", serverMeta, cs)
	require.NoError(t, err)

	ts := readOnlyServer(t, serverSwizzler.MetadataCache)
	defer ts.Close()

	// download existing metadata
	repo := newBlankRepo(t, ts.URL)
	defer os.RemoveAll(repo.baseDir)

	_, err = repo.Update(false)
	require.NoError(t, err)

	origFileStore := repo.fileStore

	for role := range serverMeta {
		for _, forWrite := range []bool{true, false} {
			// bump versions of everything on the server, to force everything to update
			bumpVersions(t, serverSwizzler)

			// update fileStore
			repo.fileStore = &unwritableStore{MetadataStore: origFileStore, roleToNotWrite: role}
			_, err := repo.Update(forWrite)

			if role == data.CanonicalRootRole {
				require.Error(t, err) // because checkRoot loads root from cache to check hashes
				continue
			}
			require.NoError(t, err)

			for r, expected := range serverMeta {
				actual, err := repo.fileStore.GetMeta(r, maxSize)
				require.NoError(t, err, "problem getting repo metadata for %s", r)
				if role == r {
					require.False(t, bytes.Equal(expected, actual),
						"%s: expected to not update because %s was unwritable", r, role)
				} else {
					require.True(t, bytes.Equal(expected, actual),
						"%s: expected to update since only %s was unwritable", r, role)
				}
			}
		}
	}
}

type messUpMetadata func(role string) error

func waysToMessUpLocalMetadata(repoSwizzler *testutils.MetadataSwizzler) map[string]messUpMetadata {
	return map[string]messUpMetadata{
		// for instance if the metadata got truncated or otherwise block corrupted
		"invalid JSON": repoSwizzler.SetInvalidJSON,
		// if the metadata was accidentally deleted
		"missing metadata": repoSwizzler.RemoveMetadata,
		// if the signature was invalid - maybe the user tried to modify something manually
		// that they forgot (add a key, or something)
		"signed with right key but wrong hash": repoSwizzler.InvalidateMetadataSignatures,
		// if the user copied the wrong root.json over it by accident or something
		"signed with wrong key": repoSwizzler.SignMetadataWithInvalidKey,
		// self explanatory
		"expired": repoSwizzler.ExpireMetadata,

		// Not trying any of the other repoSwizzler methods, because those involve modifying
		// and re-serializing, and that means a user has the root and other keys and was trying to
		// actively sabotage and break their own local repo (particularly the root.json)
	}
}

// If a repo has corrupt metadata (in that the hash doesn't match the snapshot) or
// missing metadata, an update will replace all of it
func TestUpdateReplacesCorruptOrMissingMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	serverMeta, cs, err := testutils.NewRepoMetadata("docker.com/notary", "targets/a", "targets/a/b", "targets/a/b/c")
	require.NoError(t, err)

	ts := readOnlyServer(t, store.NewMemoryStore(serverMeta, nil))
	defer ts.Close()

	repo := newBlankRepo(t, ts.URL)
	defer os.RemoveAll(repo.baseDir)

	_, err = repo.Update(false) // ensure we have all metadata to start with
	require.NoError(t, err)

	// we want to swizzle the local cache, not the server, so create a new one
	repoSwizzler := testutils.NewMetadataSwizzler("docker.com/notary", serverMeta, cs)
	repoSwizzler.MetadataCache = repo.fileStore

	for _, role := range repoSwizzler.Roles {
		for text, messItUp := range waysToMessUpLocalMetadata(repoSwizzler) {
			for _, forWrite := range []bool{true, false} {
				require.NoError(t, messItUp(role), "could not fuzz %s (%s)", role, text)
				_, err := repo.Update(forWrite)
				require.NoError(t, err)
				for r, expected := range serverMeta {
					actual, err := repo.fileStore.GetMeta(r, maxSize)
					require.NoError(t, err, "problem getting repo metadata for %s", role)
					require.True(t, bytes.Equal(expected, actual),
						"%s for %s: expected to recover after update", text, role)
				}
			}
		}
	}
}

// If a repo has an invalid root (signed by wrong key, expired, invalid version,
// invalid number of signatures, etc.), the repo will just get the new root from
// the server, whether or not the update is for writing (forced update), but
// it will refuse to update if the root key has changed and the new root is
// not signed by the old and new key
func TestUpdateFailsIfServerRootKeyChangedWithoutMultiSign(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	serverMeta, cs, err := testutils.NewRepoMetadata("docker.com/notary", "targets/a", "targets/a/b", "targets/a/b/c")
	require.NoError(t, err)

	serverSwizzler := testutils.NewMetadataSwizzler("docker.com/notary", serverMeta, cs)
	require.NoError(t, err)

	origMeta := testutils.CopyRepoMetadata(serverMeta)

	ts := readOnlyServer(t, serverSwizzler.MetadataCache)
	defer ts.Close()

	repo := newBlankRepo(t, ts.URL)
	defer os.RemoveAll(repo.baseDir)

	_, err = repo.Update(false) // ensure we have all metadata to start with
	require.NoError(t, err)
	ts.Close()

	// rotate the server's root.json root key so that they no longer match trust anchors
	require.NoError(t, serverSwizzler.ChangeRootKey())
	// bump versions, update snapshot and timestamp too so it will not fail on a hash
	bumpVersions(t, serverSwizzler)

	// we want to swizzle the local cache, not the server, so create a new one
	repoSwizzler := &testutils.MetadataSwizzler{
		MetadataCache: repo.fileStore,
		CryptoService: serverSwizzler.CryptoService,
		Roles:         serverSwizzler.Roles,
	}

	for text, messItUp := range waysToMessUpLocalMetadata(repoSwizzler) {
		for _, forWrite := range []bool{true, false} {
			require.NoError(t, messItUp(data.CanonicalRootRole), "could not fuzz root (%s)", text)
			messedUpMeta, err := repo.fileStore.GetMeta(data.CanonicalRootRole, maxSize)

			if _, ok := err.(store.ErrMetaNotFound); ok { // one of the ways to mess up is to delete metadata

				_, err = repo.Update(forWrite)
				require.Error(t, err) // the new server has a different root key, update fails

			} else {

				require.NoError(t, err)

				_, err = repo.Update(forWrite)
				require.Error(t, err) // the new server has a different root, update fails

				// we can't test that all the metadata is the same, because we probably would
				// have downloaded a new timestamp and maybe snapshot.  But the root should be the
				// same because it has failed to update.
				for role, expected := range origMeta {
					if role != data.CanonicalTimestampRole && role != data.CanonicalSnapshotRole {
						actual, err := repo.fileStore.GetMeta(role, maxSize)
						require.NoError(t, err, "problem getting repo metadata for %s", role)

						if role == data.CanonicalRootRole {
							expected = messedUpMeta
						}
						require.True(t, bytes.Equal(expected, actual),
							"%s for %s: expected to not have updated", text, role)
					}
				}

			}

			// revert our original root metadata
			require.NoError(t,
				repo.fileStore.SetMeta(data.CanonicalRootRole, origMeta[data.CanonicalRootRole]))
		}
	}
}