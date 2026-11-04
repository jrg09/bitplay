package main

import (
	"os"
	"path/filepath"
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
		Entries: map[string]*libraryEntry{
			libraryKey(hash, 0): {InfoHash: hash, FileIndex: 0, RelPath: "Show/S01E01.mkv"},
			libraryKey(hash, 1): {InfoHash: hash, FileIndex: 1, RelPath: "Show/S01E02.mkv"},
		},
	}
	defer func() { library.loaded, library.data = false, nil }()

	libraryRemoveLocked(libraryKey(hash, 0))
	if onDiskSize("Show/S01E01.mkv") != 0 || onDiskSize("Show/S01E02.mkv") == 0 {
		t.Error("first removal must delete only that episode")
	}
	if _, err := os.Stat(libraryMetaPath(hash)); err != nil {
		t.Error("metainfo must stay while other episodes remain")
	}

	libraryRemoveLocked(libraryKey(hash, 1))
	if _, err := os.Stat(filepath.Join(libraryDataDir, "Show")); !os.IsNotExist(err) {
		t.Error("last removal must delete the whole torrent folder")
	}
	if _, err := os.Stat(libraryMetaPath(hash)); !os.IsNotExist(err) {
		t.Error("last removal must delete the saved metainfo")
	}
	if onDiskSize("Other.mkv") == 0 {
		t.Error("unrelated files must be kept")
	}
	if len(library.data.Entries) != 0 || len(library.data.Torrents) != 0 {
		t.Errorf("library not emptied: %+v", library.data)
	}
	library.Unlock()

	if _, err := os.Stat(libraryFile); err != nil {
		t.Errorf("library file not saved: %v", err)
	}
}
