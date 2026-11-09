package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeDataPath(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, bad := range []string{"", ".", "..", "../etc/passwd", "a/../../x"} {
		if _, err := safeDataPath(bad); err == nil {
			t.Errorf("safeDataPath(%q) should fail", bad)
		}
	}
	for _, good := range []string{"Movie.mkv", "Show/Season 1/ep.mkv", "a/../b.mkv"} {
		if _, err := safeDataPath(good); err != nil {
			t.Errorf("safeDataPath(%q): %v", good, err)
		}
	}
}

func TestLibraryRemove(t *testing.T) {
	t.Chdir(t.TempDir())
	hash := "08ada5a7a6183aae1e09d831df6748d566095a10"
	write := func(rel string) {
		p := filepath.Join(libraryDataDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("Show/S01E01.mkv")
	write("Show/S01E02.mkv")
	write("Show/info.nfo")
	write(".bitplay-meta/" + hash + ".torrent")
	write("Other.mkv")

	library.Lock()
	library.loaded = true
	library.data = &libraryData{
		Torrents: map[string]*libraryTorrent{hash: {Name: "Show"}},
		Users: map[string]*userLibrary{
			"a@x.com": {Entries: map[string]*libraryEntry{
				libraryKey(hash, 0): {InfoHash: hash, FileIndex: 0, RelPath: "Show/S01E01.mkv"},
				libraryKey(hash, 1): {InfoHash: hash, FileIndex: 1, RelPath: "Show/S01E02.mkv"},
			}},
		},
	}
	defer func() { library.loaded, library.data = false, nil }()

	libraryRemoveLocked("a@x.com", libraryKey(hash, 0))
	if onDiskSize("Show/S01E01.mkv") != 0 || onDiskSize("Show/S01E02.mkv") == 0 {
		t.Error("first removal must delete only that episode")
	}
	if _, err := os.Stat(libraryMetaPath(hash)); err != nil {
		t.Error("metainfo must stay while other episodes remain")
	}

	libraryRemoveLocked("a@x.com", libraryKey(hash, 1))
	if _, err := os.Stat(filepath.Join(libraryDataDir, "Show")); !os.IsNotExist(err) {
		t.Error("last removal must delete the whole torrent folder")
	}
	if _, err := os.Stat(libraryMetaPath(hash)); !os.IsNotExist(err) {
		t.Error("last removal must delete the saved metainfo")
	}
	if onDiskSize("Other.mkv") == 0 {
		t.Error("unrelated files must be kept")
	}
	if len(library.data.Users["a@x.com"].Entries) != 0 || len(library.data.Torrents) != 0 {
		t.Errorf("library not emptied: %+v", library.data)
	}
	library.Unlock()

	if _, err := os.Stat(libraryFile); err != nil {
		t.Errorf("library file not saved: %v", err)
	}
}

// A file two users watched stays on disk until both have dropped it.
func TestLibraryRemoveSharedFile(t *testing.T) {
	t.Chdir(t.TempDir())
	hash := "08ada5a7a6183aae1e09d831df6748d566095a10"
	p := filepath.Join(libraryDataDir, "Movie.mkv")
	if err := os.MkdirAll(libraryDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	key := libraryKey(hash, 0)

	library.Lock()
	library.loaded = true
	library.data = &libraryData{
		Torrents: map[string]*libraryTorrent{hash: {Name: "Movie.mkv"}},
		Users: map[string]*userLibrary{
			"a@x.com": {Entries: map[string]*libraryEntry{key: {InfoHash: hash, RelPath: "Movie.mkv"}}},
			"b@x.com": {Entries: map[string]*libraryEntry{key: {InfoHash: hash, RelPath: "Movie.mkv"}}},
		},
	}
	defer func() { library.loaded, library.data = false, nil }()
	defer library.Unlock()

	libraryRemoveLocked("a@x.com", key)
	if onDiskSize("Movie.mkv") == 0 {
		t.Fatal("file deleted while another user still has it")
	}
	if library.data.Users["b@x.com"].Entries[key] == nil {
		t.Fatal("the other user's entry must be untouched")
	}
	if library.data.Torrents[hash] == nil {
		t.Fatal("shared torrent record must stay")
	}

	libraryRemoveLocked("b@x.com", key)
	if onDiskSize("Movie.mkv") != 0 {
		t.Fatal("file must be deleted once nobody has it")
	}
}

// History saved before users existed goes to the first admin.
func TestLibraryMigratesLegacyHistory(t *testing.T) {
	t.Setenv("BITPLAY_ADMINS", "Owner@x.com, other@x.com")
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("config", 0755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"torrents":{"aa":{"magnet":"m","name":"n"}},` +
		`"entries":{"aa/0":{"infoHash":"aa","fileIndex":0,"position":12}},` +
		`"shows":{"tt1":{"imdb":"tt1","title":"S"}}}`
	if err := os.WriteFile(libraryFile, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	library.Lock()
	library.loaded, library.data = false, nil
	defer func() { library.loaded, library.data = false, nil; library.Unlock() }()
	lib := libraryLocked()

	owner := lib.Users["owner@x.com"]
	if owner == nil || owner.Entries["aa/0"] == nil || owner.Entries["aa/0"].Position != 12 || owner.Shows["tt1"] == nil {
		t.Fatalf("history not moved to the first admin: %+v", lib.Users)
	}
	if lib.LegacyEntries != nil || lib.LegacyShows != nil {
		t.Fatal("legacy fields must be cleared")
	}
	if lib.Torrents["aa"] == nil {
		t.Fatal("shared torrents must be kept")
	}
	b, _ := os.ReadFile(libraryFile)
	if strings.Contains(string(b), `"entries":{"aa/0"`) && !strings.Contains(string(b), `"users"`) {
		t.Fatalf("migrated layout not saved: %s", b)
	}
}

// History recorded as the single-user "local" before admins were configured
// is adopted by the first admin, without overwriting what they already have.
func TestLibraryAdoptsLocalHistory(t *testing.T) {
	t.Setenv("BITPLAY_ADMINS", "owner@x.com")
	lib := &libraryData{Users: map[string]*userLibrary{
		localUserID: {
			Entries: map[string]*libraryEntry{"aa/0": {InfoHash: "aa", Position: 5}, "bb/0": {InfoHash: "bb", Position: 9}},
			Shows:   map[string]*showProgress{"tt1": {IMDb: "tt1"}},
		},
		"owner@x.com": {Entries: map[string]*libraryEntry{"bb/0": {InfoHash: "bb", Position: 100}}},
	}}
	if !lib.migrateLegacyLocked() {
		t.Fatal("migration must report a change")
	}
	if lib.Users[localUserID] != nil {
		t.Fatal("local history must be removed once adopted")
	}
	owner := lib.Users["owner@x.com"]
	if owner.Entries["aa/0"] == nil || owner.Shows["tt1"] == nil {
		t.Fatalf("local history not adopted: %+v", owner)
	}
	if owner.Entries["bb/0"].Position != 100 {
		t.Fatal("the admin's own progress must win")
	}
	if lib.migrateLegacyLocked() {
		t.Fatal("a second run must be a no-op")
	}
}
