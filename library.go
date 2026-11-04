package main

// "Continue watching" library.
//
//   - Torrent data already lives in ./torrent-data (anacrolix file storage +
//     .torrent.bolt.db piece completion), so anything downloaded survives a
//     session teardown. On top of that we:
//   - save each torrent's metainfo, so re-opening it does not need peers just
//     to fetch metadata (a fully downloaded file plays with no peers at all);
//   - download the whole file in the background once playback starts, and
//     keep the session alive until it completes;
//   - remember the playback position per file (config/library.json), so any
//     device can resume where it stopped;
//   - delete watched files after KeepWatchedDays.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

const (
	libraryFile            = "config/library.json"
	libraryDataDir         = "./torrent-data"
	libraryMetaDir         = "./torrent-data/.bitplay-meta"
	libraryWatchedRatio    = 0.95 // past this point a file counts as watched
	libraryRewatchRatio    = 0.90 // going back before this clears "watched"
	defaultKeepWatchedDays = 3
	// a background download that makes no progress for this long no longer
	// keeps its session alive
	libraryStallTimeout = time.Hour
)

type libraryTorrent struct {
	Magnet  string `json:"magnet"`
	Name    string `json:"name"`
	AddedAt int64  `json:"addedAt"`
}

type libraryEntry struct {
	InfoHash    string  `json:"infoHash"`
	FileIndex   int     `json:"fileIndex"`
	Title       string  `json:"title"`
	FileName    string  `json:"fileName"`
	RelPath     string  `json:"relPath"` // path inside torrent-data
	Size        int64   `json:"size"`
	Position    float64 `json:"position"`
	Duration    float64 `json:"duration"`
	Downloaded  int64   `json:"downloaded"` // last known bytes on disk
	CompletedAt int64   `json:"completedAt,omitempty"`
	WatchedAt   int64   `json:"watchedAt,omitempty"`
	UpdatedAt   int64   `json:"updatedAt"`
}

type libraryData struct {
	Torrents map[string]*libraryTorrent `json:"torrents"`
	Entries  map[string]*libraryEntry   `json:"entries"`
}

var library = struct {
	sync.Mutex
	data   *libraryData
	loaded bool
}{}

var hexInfoHashRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func libraryKey(hash string, idx int) string {
	return hash + "/" + strconv.Itoa(idx)
}

// libraryLocked returns the in-memory library; callers hold library.Mutex.
func libraryLocked() *libraryData {
	if !library.loaded {
		library.loaded = true
		library.data = &libraryData{}
		if b, err := os.ReadFile(libraryFile); err == nil {
			if err := json.Unmarshal(b, library.data); err != nil {
				log.Printf("Library: ignoring unreadable %s: %v", libraryFile, err)
			}
		}
		if library.data.Torrents == nil {
			library.data.Torrents = map[string]*libraryTorrent{}
		}
		if library.data.Entries == nil {
			library.data.Entries = map[string]*libraryEntry{}
		}
	}
	return library.data
}

// librarySaveLocked writes the library atomically; callers hold the mutex.
func librarySaveLocked() {
	b, err := json.MarshalIndent(library.data, "", "  ")
	if err != nil {
		log.Printf("Library: encode failed: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(libraryFile), 0755); err != nil {
		log.Printf("Library: %v", err)
		return
	}
	tmp := libraryFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		log.Printf("Library: write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, libraryFile); err != nil {
		log.Printf("Library: rename failed: %v", err)
	}
}

func keepWatchedDays() int {
	settingsMutex.RLock()
	defer settingsMutex.RUnlock()
	if currentSettings.KeepWatchedDays > 0 {
		return currentSettings.KeepWatchedDays
	}
	return defaultKeepWatchedDays
}

// ---------------------------------------------------------------------------
// Metainfo cache: re-open torrents without fetching metadata from peers
// ---------------------------------------------------------------------------

func libraryMetaPath(hash string) string {
	return filepath.Join(libraryMetaDir, hash+".torrent")
}

// libraryAddMagnet adds the magnet, using saved metainfo when available.
func libraryAddMagnet(client *torrent.Client, magnet string) (*torrent.Torrent, error) {
	m, err := metainfo.ParseMagnetUri(magnet)
	if err == nil {
		hash := m.InfoHash.HexString()
		if mi, err := metainfo.LoadFromFile(libraryMetaPath(hash)); err == nil {
			t, err := client.AddTorrent(mi)
			if err == nil {
				if len(m.Trackers) > 0 {
					t.AddTrackers([][]string{m.Trackers})
				}
				log.Printf("Library: opened %s from saved metainfo", hash)
				return t, nil
			}
			log.Printf("Library: saved metainfo for %s unusable: %v", hash, err)
		}
	}
	return client.AddMagnet(magnet)
}

// libraryTorrentReady saves the metainfo and remembers the magnet once the
// torrent info is known.
func libraryTorrentReady(t *torrent.Torrent, magnet string) {
	hash := t.InfoHash().HexString()

	path := libraryMetaPath(hash)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(libraryMetaDir, 0755); err == nil {
			if f, err := os.Create(path); err == nil {
				mi := t.Metainfo()
				if err := mi.Write(f); err != nil {
					log.Printf("Library: could not save metainfo: %v", err)
				}
				f.Close()
			}
		}
	}

	library.Lock()
	defer library.Unlock()
	lib := libraryLocked()
	rec := lib.Torrents[hash]
	if rec == nil {
		rec = &libraryTorrent{AddedAt: time.Now().Unix()}
		lib.Torrents[hash] = rec
	}
	rec.Magnet = magnet
	rec.Name = t.Name()
	librarySaveLocked()
}

// ---------------------------------------------------------------------------
// Background full download
// ---------------------------------------------------------------------------

type fullDownload struct {
	session      *TorrentSession
	lastProgress time.Time
	lastBytes    int64
	done         bool
}

var fullDownloads = struct {
	sync.Mutex
	m map[string]*fullDownload // key: libraryKey
}{m: map[string]*fullDownload{}}

// libraryStartFullDownload asks the client for every piece of the file (the
// player's readahead keeps the higher priority) and watches for completion.
func libraryStartFullDownload(hash string, idx int, session *TorrentSession, file *torrent.File) {
	key := libraryKey(hash, idx)
	fullDownloads.Lock()
	if fd, ok := fullDownloads.m[key]; ok && fd.session == session {
		fullDownloads.Unlock()
		return
	}
	fd := &fullDownload{session: session, lastProgress: time.Now(), lastBytes: file.BytesCompleted()}
	fullDownloads.m[key] = fd
	fullDownloads.Unlock()

	file.Download()
	log.Printf("Library: downloading all of %q in the background", file.DisplayPath())

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			current, ok := sessions.Load(hash)
			if !ok || current.(*TorrentSession) != session {
				fullDownloads.Lock()
				if fullDownloads.m[key] == fd {
					delete(fullDownloads.m, key)
				}
				fullDownloads.Unlock()
				return
			}
			done := file.BytesCompleted()
			complete := done >= file.Length()

			fullDownloads.Lock()
			if done > fd.lastBytes {
				fd.lastBytes, fd.lastProgress = done, time.Now()
			}
			fd.done = complete
			fullDownloads.Unlock()

			library.Lock()
			if e := libraryLocked().Entries[key]; e != nil {
				e.Downloaded = done
				if complete && e.CompletedAt == 0 {
					e.CompletedAt = time.Now().Unix()
					librarySaveLocked()
				}
			}
			library.Unlock()

			if complete {
				log.Printf("Library: %q is fully downloaded", file.DisplayPath())
				return
			}
		}
	}()
}

// libraryKeepsSessionAlive reports whether a background download that is
// still making progress should keep an idle session from being reaped.
func libraryKeepsSessionAlive(hash string, session *TorrentSession) bool {
	fullDownloads.Lock()
	defer fullDownloads.Unlock()
	for key, fd := range fullDownloads.m {
		if strings.HasPrefix(key, hash+"/") && fd.session == session && !fd.done &&
			time.Since(fd.lastProgress) < libraryStallTimeout {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Files on disk
// ---------------------------------------------------------------------------

// libraryFilePath mirrors anacrolix's default file storage layout:
// <data dir>/<torrent name>/<file path...> (just <torrent name> for a
// single-file torrent).
func libraryFilePath(info *metainfo.Info, file *torrent.File) string {
	fi := file.FileInfo()
	parts := append([]string{info.BestName()}, fi.BestPath()...)
	return filepath.Join(parts...)
}

// safeDataPath resolves rel inside the data dir, refusing anything outside.
func safeDataPath(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty path")
	}
	base, err := filepath.Abs(libraryDataDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(base, rel)
	r, err := filepath.Rel(base, full)
	if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the data directory", rel)
	}
	return full, nil
}

func onDiskSize(rel string) int64 {
	full, err := safeDataPath(rel)
	if err != nil {
		return 0
	}
	if st, err := os.Stat(full); err == nil && !st.IsDir() {
		return st.Size()
	}
	return 0
}

// libraryRemove deletes the entry and its file. When it was the last entry
// of its torrent, the whole torrent folder and its saved metainfo go too.
// Callers hold library.Mutex.
func libraryRemoveLocked(key string) {
	lib := libraryLocked()
	e := lib.Entries[key]
	if e == nil {
		return
	}
	delete(lib.Entries, key)

	// stop the session first so nothing is writing to the files
	if v, ok := sessions.Load(e.InfoHash); ok {
		teardownSession(e.InfoHash, v.(*TorrentSession))
	}

	remaining := false
	for _, other := range lib.Entries {
		if other.InfoHash == e.InfoHash {
			remaining = true
			break
		}
	}

	target := e.RelPath
	if !remaining {
		if rec := lib.Torrents[e.InfoHash]; rec != nil && rec.Name != "" {
			target = rec.Name // top-level file or folder of the torrent
		}
		delete(lib.Torrents, e.InfoHash)
		os.Remove(libraryMetaPath(e.InfoHash))
	}
	if full, err := safeDataPath(target); err == nil {
		if err := os.RemoveAll(full); err != nil {
			log.Printf("Library: could not delete %s: %v", full, err)
		} else {
			log.Printf("Library: deleted %s", target)
		}
		// drop now-empty parent folders
		base, _ := filepath.Abs(libraryDataDir)
		for dir := filepath.Dir(full); dir != base && strings.HasPrefix(dir, base); dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil {
				break
			}
		}
	} else {
		log.Printf("Library: %v", err)
	}
	librarySaveLocked()
}

// libraryJanitor deletes watched files older than KeepWatchedDays.
func libraryJanitor() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for ; ; <-ticker.C {
		cutoff := time.Now().Add(-time.Duration(keepWatchedDays()) * 24 * time.Hour).Unix()
		library.Lock()
		for key, e := range libraryLocked().Entries {
			if e.WatchedAt > 0 && e.WatchedAt < cutoff {
				log.Printf("Library: %q was watched more than %d day(s) ago, deleting", e.FileName, keepWatchedDays())
				libraryRemoveLocked(key)
			}
		}
		library.Unlock()
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

type libraryProgressRequest struct {
	Session  string  `json:"session"`
	File     int     `json:"file"`
	Position float64 `json:"position"`
	Duration float64 `json:"duration"`
}

// libraryProgressHandler: POST /api/v1/library/progress
// Stores the playback position and starts the background download.
func libraryProgressHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in libraryProgressRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}
	v, ok := sessions.Load(in.Session)
	if !ok {
		respondWithJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	session := v.(*TorrentSession)
	t := session.Torrent
	if t == nil || t.Info() == nil {
		respondWithJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Torrent metadata not available yet"})
		return
	}
	files := t.Files()
	if in.File < 0 || in.File >= len(files) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "File index out of range"})
		return
	}
	session.LastUsed = time.Now()
	file := files[in.File]
	hash := t.InfoHash().HexString()
	key := libraryKey(hash, in.File)
	done := file.BytesCompleted()

	library.Lock()
	lib := libraryLocked()
	e := lib.Entries[key]
	if e == nil {
		e = &libraryEntry{InfoHash: hash, FileIndex: in.File}
		lib.Entries[key] = e
	}
	e.Title = t.Name()
	e.FileName = filepath.Base(file.DisplayPath())
	e.RelPath = libraryFilePath(t.Info(), file)
	e.Size = file.Length()
	e.Downloaded = done
	if in.Position >= 0 {
		e.Position = in.Position
	}
	if in.Duration > 0 {
		e.Duration = in.Duration
	}
	if e.Duration > 0 {
		ratio := e.Position / e.Duration
		if ratio >= libraryWatchedRatio && e.WatchedAt == 0 {
			e.WatchedAt = time.Now().Unix()
		} else if ratio < libraryRewatchRatio {
			e.WatchedAt = 0
		}
	}
	if done >= e.Size && e.CompletedAt == 0 {
		e.CompletedAt = time.Now().Unix()
	}
	e.UpdatedAt = time.Now().Unix()
	resp := map[string]interface{}{
		"downloaded": done,
		"size":       e.Size,
		"complete":   done >= e.Size,
		"watched":    e.WatchedAt > 0,
	}
	librarySaveLocked()
	library.Unlock()

	if done < file.Length() {
		libraryStartFullDownload(hash, in.File, session, file)
	}
	respondWithJSON(w, http.StatusOK, resp)
}

// libraryEntryHandler: GET /api/v1/library/entry?session=<hash>&file=<idx>
func libraryEntryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	hash := strings.ToLower(r.URL.Query().Get("session"))
	idx, err := strconv.Atoi(r.URL.Query().Get("file"))
	if err != nil || !hexInfoHashRe.MatchString(hash) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid session or file"})
		return
	}
	library.Lock()
	defer library.Unlock()
	if e := libraryLocked().Entries[libraryKey(hash, idx)]; e != nil {
		respondWithJSON(w, http.StatusOK, e)
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]interface{}{"position": 0})
}

type libraryListItem struct {
	libraryEntry
	Magnet  string `json:"magnet"`
	OnDisk  int64  `json:"onDisk"` // bytes of the file present on disk
	Active  bool   `json:"active"` // a session is open right now
	Watched bool   `json:"watched"`
}

// libraryListHandler: GET /api/v1/library (most recent first)
func libraryListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	library.Lock()
	lib := libraryLocked()
	items := make([]libraryListItem, 0, len(lib.Entries))
	for _, e := range lib.Entries {
		item := libraryListItem{libraryEntry: *e, Watched: e.WatchedAt > 0}
		if rec := lib.Torrents[e.InfoHash]; rec != nil {
			item.Magnet = rec.Magnet
		}
		if item.Magnet == "" {
			item.Magnet = "magnet:?xt=urn:btih:" + e.InfoHash
		}
		items = append(items, item)
	}
	library.Unlock()

	for i := range items {
		it := &items[i]
		// a deleted or purged file means nothing is left on disk
		it.OnDisk = onDiskSize(it.RelPath)
		if it.OnDisk == 0 {
			it.Downloaded = 0
		}
		if v, ok := sessions.Load(it.InfoHash); ok {
			s := v.(*TorrentSession)
			if s.Torrent != nil && s.Torrent.Info() != nil && it.FileIndex < len(s.Torrent.Files()) {
				it.Active = true
				it.Downloaded = s.Torrent.Files()[it.FileIndex].BytesCompleted()
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	respondWithJSON(w, http.StatusOK, items)
}

// libraryDeleteHandler: POST /api/v1/library/delete {infoHash, fileIndex}
func libraryDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		InfoHash  string `json:"infoHash"`
		FileIndex int    `json:"fileIndex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !hexInfoHashRe.MatchString(in.InfoHash) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}
	key := libraryKey(in.InfoHash, in.FileIndex)
	library.Lock()
	defer library.Unlock()
	if libraryLocked().Entries[key] == nil {
		respondWithJSON(w, http.StatusNotFound, map[string]string{"error": "Not in the library"})
		return
	}
	libraryRemoveLocked(key)
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Deleted"})
}

// saveLibrarySettingsHandler: POST /api/v1/settings/library {keepWatchedDays}
func saveLibrarySettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		KeepWatchedDays int `json:"keepWatchedDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.KeepWatchedDays < 1 || in.KeepWatchedDays > 365 {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Days must be between 1 and 365"})
		return
	}
	settingsMutex.Lock()
	currentSettings.KeepWatchedDays = in.KeepWatchedDays
	settingsMutex.Unlock()
	if err := saveSettingsToFile(); err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to save settings: " + err.Error()})
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Library settings saved successfully"})
}
