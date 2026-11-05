package main

// Per-episode progress for series, stored in config/library.json under
// "shows" next to the per-file entries (library.go). Progress is keyed by
// series + episode ("S02E05"), not by torrent, so switching torrents or
// packs keeps it, and the watched mark survives the janitor deleting the
// file.
//
// The client sends {series: {imdb, title, poster, season, episode}} with
// each progress report when playback started from the series view. Without
// it, a file named like an episode ("Show.S02E05...") of a show that is
// already tracked is still recorded.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const showsListMax = 10

type seriesRef struct {
	IMDb    string `json:"imdb"`
	Title   string `json:"title"`
	Poster  string `json:"poster"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

func (r *seriesRef) valid() bool {
	return r != nil && imdbIDRe.MatchString(r.IMDb) && r.Season >= 0 && r.Season < 100 && r.Episode > 0 && r.Episode < 1000
}

type episodeProgress struct {
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	WatchedAt int64   `json:"watchedAt,omitempty"`
	InfoHash  string  `json:"infoHash,omitempty"`
	FileIndex int     `json:"fileIndex"`
	UpdatedAt int64   `json:"updatedAt"`
}

type showProgress struct {
	IMDb        string                      `json:"imdb"`
	Title       string                      `json:"title"`
	Poster      string                      `json:"poster,omitempty"`
	LastEpisode string                      `json:"lastEpisode,omitempty"` // "S02E05"
	Hidden      bool                        `json:"hidden,omitempty"`      // removed from Continue watching
	UpdatedAt   int64                       `json:"updatedAt"`
	Episodes    map[string]*episodeProgress `json:"episodes"`
}

var reEpisodeKey = regexp.MustCompile(`^S(\d{2,})E(\d{2,})$`)

func episodeKey(s, e int) string { return fmt.Sprintf("S%02dE%02d", s, e) }

func parseEpisodeKey(k string) (s, e int, ok bool) {
	m := reEpisodeKey.FindStringSubmatch(k)
	if m == nil {
		return 0, 0, false
	}
	s, _ = strconv.Atoi(m[1])
	e, _ = strconv.Atoi(m[2])
	return s, e, true
}

// episodeOrder compares (s1,e1) and (s2,e2); specials (season 0) sort last.
func episodeLess(s1, e1, s2, e2 int) bool {
	if (s1 == 0) != (s2 == 0) {
		return s2 == 0
	}
	if s1 != s2 {
		return s1 < s2
	}
	return e1 < e2
}

func showLocked(lib *libraryData, imdb string) *showProgress {
	if lib.Shows == nil {
		lib.Shows = map[string]*showProgress{}
	}
	sp := lib.Shows[imdb]
	if sp == nil {
		sp = &showProgress{IMDb: imdb, Episodes: map[string]*episodeProgress{}}
		lib.Shows[imdb] = sp
	}
	if sp.Episodes == nil {
		sp.Episodes = map[string]*episodeProgress{}
	}
	return sp
}

// showRefFromNameLocked links a file named like an episode to a show that
// is already tracked (exactly one must match).
func showRefFromNameLocked(lib *libraryData, fileName string) *seriesRef {
	rel, ok := classifySeriesName(fileName)
	if !ok || rel.Pack {
		return nil
	}
	var found *seriesRef
	for _, sp := range lib.Shows {
		if !belongsToSeries(searchResult{Title: fileName}, sp.IMDb, sp.Title) {
			continue
		}
		if found != nil {
			return nil // ambiguous
		}
		found = &seriesRef{IMDb: sp.IMDb, Season: rel.SeasonFrom, Episode: rel.Episode}
	}
	return found
}

// showRecordProgressLocked mirrors a file's progress onto its episode.
func showRecordProgressLocked(lib *libraryData, ref *seriesRef, e *libraryEntry) {
	if !ref.valid() {
		ref = showRefFromNameLocked(lib, e.FileName)
	}
	if !ref.valid() {
		return
	}
	now := time.Now().Unix()
	sp := showLocked(lib, ref.IMDb)
	if ref.Title != "" {
		sp.Title = ref.Title
	}
	if strings.HasPrefix(ref.Poster, "https://") && len(ref.Poster) < 1000 {
		sp.Poster = ref.Poster
	}
	key := episodeKey(ref.Season, ref.Episode)
	ep := sp.Episodes[key]
	if ep == nil {
		ep = &episodeProgress{}
		sp.Episodes[key] = ep
	}
	ep.Position, ep.Duration = e.Position, e.Duration
	if e.WatchedAt > 0 {
		if ep.WatchedAt == 0 {
			ep.WatchedAt = e.WatchedAt
		}
	} else if ep.Duration > 0 && ep.Position/ep.Duration < libraryRewatchRatio && ep.Position > 0 {
		ep.WatchedAt = 0 // rewatching from the start
	}
	ep.InfoHash, ep.FileIndex, ep.UpdatedAt = e.InfoHash, e.FileIndex, now
	sp.LastEpisode, sp.UpdatedAt, sp.Hidden = key, now, false
}

// showLinkedFilesLocked: library entry key -> imdb of its show.
func showLinkedFilesLocked(lib *libraryData) map[string]string {
	out := map[string]string{}
	for _, sp := range lib.Shows {
		for _, ep := range sp.Episodes {
			if ep.InfoHash != "" {
				out[libraryKey(ep.InfoHash, ep.FileIndex)] = sp.IMDb
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Next episode
// ---------------------------------------------------------------------------

type nextEpisode struct {
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Name    string `json:"name"`
	Aired   bool   `json:"aired"`
	Resume  bool   `json:"resume"` // half watched: continue it
}

// findNextEpisode: the last episode if it is not finished, otherwise the
// first unwatched regular episode after it (or from the start). Specials
// are never suggested.
func findNextEpisode(info *seriesInfo, sp *showProgress) *nextEpisode {
	type ref struct {
		s  int
		ep seriesEpisode
	}
	var order []ref
	for _, season := range info.Seasons {
		if season.Season == 0 {
			continue
		}
		for _, ep := range season.Episodes {
			order = append(order, ref{season.Season, ep})
		}
	}
	progress := func(r ref) *episodeProgress {
		if sp == nil {
			return nil
		}
		return sp.Episodes[episodeKey(r.s, r.ep.Episode)]
	}
	watched := func(r ref) bool { p := progress(r); return p != nil && p.WatchedAt > 0 }

	start := 0
	if sp != nil {
		if ls, le, ok := parseEpisodeKey(sp.LastEpisode); ok {
			for i, r := range order {
				if r.s == ls && r.ep.Episode == le {
					// started or unmarked, but not finished: that one again
					if p := progress(r); p != nil && p.WatchedAt == 0 {
						return &nextEpisode{Season: r.s, Episode: r.ep.Episode, Name: r.ep.Name, Aired: r.ep.Aired, Resume: p.Position > 0}
					}
					start = i + 1
					break
				}
			}
		}
	}
	for i := start; i < len(order); i++ {
		if r := order[i]; !watched(r) {
			return &nextEpisode{Season: r.s, Episode: r.ep.Episode, Name: r.ep.Name, Aired: r.ep.Aired}
		}
	}
	return nil
}

type episodeProgressView struct {
	Position float64 `json:"position"`
	Duration float64 `json:"duration"`
	Watched  bool    `json:"watched"`
}

// showSnapshot copies a show's progress for use outside the lock.
func showSnapshot(imdb string) (*showProgress, map[string]episodeProgressView) {
	library.Lock()
	defer library.Unlock()
	sp := libraryLocked().Shows[imdb]
	views := map[string]episodeProgressView{}
	if sp == nil {
		return nil, views
	}
	cp := *sp
	cp.Episodes = map[string]*episodeProgress{}
	for k, ep := range sp.Episodes {
		e := *ep
		cp.Episodes[k] = &e
		views[k] = episodeProgressView{Position: ep.Position, Duration: ep.Duration, Watched: ep.WatchedAt > 0}
	}
	return &cp, views
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// seriesMarkHandler: POST /api/v1/series/<tt>/mark
// {season, episode (0 = whole season), watched, title, poster}
func seriesMarkHandler(w http.ResponseWriter, r *http.Request, info *seriesInfo) {
	var in struct {
		Season  int    `json:"season"`
		Episode int    `json:"episode"`
		Watched bool   `json:"watched"`
		Poster  string `json:"poster"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}
	var keys [][2]int
	for _, season := range info.Seasons {
		if season.Season != in.Season {
			continue
		}
		for _, ep := range season.Episodes {
			if in.Episode == 0 || ep.Episode == in.Episode {
				keys = append(keys, [2]int{season.Season, ep.Episode})
			}
		}
	}
	if len(keys) == 0 {
		respondWithJSON(w, http.StatusNotFound, map[string]string{"error": "Episode not found"})
		return
	}

	now := time.Now().Unix()
	library.Lock()
	lib := libraryLocked()
	sp := showLocked(lib, info.Meta.IMDb)
	sp.Title = info.Meta.Title
	if sp.Poster == "" {
		sp.Poster = info.Meta.Poster
	}
	lastS, lastE, hasLast := parseEpisodeKey(sp.LastEpisode)
	for _, k := range keys {
		key := episodeKey(k[0], k[1])
		ep := sp.Episodes[key]
		if ep == nil {
			ep = &episodeProgress{}
			sp.Episodes[key] = ep
		}
		if in.Watched {
			if ep.WatchedAt == 0 {
				ep.WatchedAt = now
			}
			// marking later episodes moves "next" forward
			if !hasLast || episodeLess(lastS, lastE, k[0], k[1]) {
				lastS, lastE, hasLast = k[0], k[1], true
			}
		} else {
			ep.WatchedAt, ep.Position = 0, 0
		}
		ep.UpdatedAt = now
	}
	if hasLast {
		sp.LastEpisode = episodeKey(lastS, lastE)
	}
	sp.UpdatedAt, sp.Hidden = now, false
	librarySaveLocked()
	library.Unlock()

	sp, views := showSnapshot(info.Meta.IMDb)
	respondWithJSON(w, http.StatusOK, map[string]interface{}{"progress": views, "next": findNextEpisode(info, sp)})
}

type showListItem struct {
	IMDb        string       `json:"imdb"`
	Title       string       `json:"title"`
	Poster      string       `json:"poster,omitempty"`
	LastEpisode string       `json:"lastEpisode,omitempty"`
	UpdatedAt   int64        `json:"updatedAt"`
	Next        *nextEpisode `json:"next"`
	// set when Next is the half-watched last episode and its file is known
	ResumeMagnet string `json:"resumeMagnet,omitempty"`
	ResumeFile   int    `json:"resumeFile"`
}

// libraryShowsHandler: GET /api/v1/library/shows (most recent first)
func libraryShowsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	library.Lock()
	lib := libraryLocked()
	var ids []string
	for id, sp := range lib.Shows {
		if !sp.Hidden {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return lib.Shows[ids[i]].UpdatedAt > lib.Shows[ids[j]].UpdatedAt })
	if len(ids) > showsListMax {
		ids = ids[:showsListMax]
	}
	library.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), metaLookupBudget)
	defer cancel()
	items := make([]showListItem, 0, len(ids))
	for _, id := range ids {
		sp, _ := showSnapshot(id)
		if sp == nil {
			continue
		}
		item := showListItem{IMDb: id, Title: sp.Title, Poster: sp.Poster, LastEpisode: sp.LastEpisode, UpdatedAt: sp.UpdatedAt}
		if info, err := loadSeries(ctx, id); err == nil {
			if item.Poster == "" {
				item.Poster = info.Meta.Poster
			}
			if item.Title == "" {
				item.Title = info.Meta.Title
			}
			item.Next = findNextEpisode(info, sp)
		}
		if n := item.Next; n != nil && n.Resume {
			if ep := sp.Episodes[episodeKey(n.Season, n.Episode)]; ep != nil && ep.InfoHash != "" {
				library.Lock()
				if rec := libraryLocked().Torrents[ep.InfoHash]; rec != nil && rec.Magnet != "" {
					item.ResumeMagnet, item.ResumeFile = rec.Magnet, ep.FileIndex
				}
				library.Unlock()
			}
		}
		items = append(items, item)
	}
	respondWithJSON(w, http.StatusOK, items)
}

// libraryShowHideHandler: POST /api/v1/library/shows/hide {imdb}
// Removes a series from Continue watching; its progress is kept and it
// comes back with the next episode played.
func libraryShowHideHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		IMDb string `json:"imdb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !imdbIDRe.MatchString(in.IMDb) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}
	library.Lock()
	defer library.Unlock()
	sp := libraryLocked().Shows[in.IMDb]
	if sp == nil {
		respondWithJSON(w, http.StatusNotFound, map[string]string{"error": "Not in the library"})
		return
	}
	sp.Hidden = true
	librarySaveLocked()
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Hidden"})
}

// libraryPrefetchHandler: POST /api/v1/library/prefetch {session, file, series}
// Downloads a file in the background without playing it (the next episode,
// pre-fetched near the end of the current one). The file gets a library
// entry, so the janitor cleans it up once watched, and is linked to its
// episode without changing the show's last/next episode.
func libraryPrefetchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Session string     `json:"session"`
		File    int        `json:"file"`
		Series  *seriesRef `json:"series"`
	}
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
	now := time.Now().Unix()

	library.Lock()
	lib := libraryLocked()
	e := lib.Entries[key]
	if e == nil {
		e = &libraryEntry{InfoHash: hash, FileIndex: in.File, UpdatedAt: now}
		lib.Entries[key] = e
	}
	e.Title = t.Name()
	e.FileName = filepath.Base(file.DisplayPath())
	e.RelPath = libraryFilePath(t.Info(), file)
	e.Size = file.Length()
	e.Downloaded = done
	if in.Series.valid() {
		sp := showLocked(lib, in.Series.IMDb)
		if sp.Title == "" {
			sp.Title = in.Series.Title
		}
		k := episodeKey(in.Series.Season, in.Series.Episode)
		ep := sp.Episodes[k]
		if ep == nil {
			ep = &episodeProgress{UpdatedAt: now}
			sp.Episodes[k] = ep
		}
		ep.InfoHash, ep.FileIndex = hash, in.File
	}
	librarySaveLocked()
	library.Unlock()

	if done < file.Length() {
		libraryStartFullDownload(hash, in.File, session, file)
	}
	log.Printf("Library: prefetching %q", file.DisplayPath())
	respondWithJSON(w, http.StatusOK, map[string]interface{}{"downloaded": done, "size": file.Length(), "complete": done >= file.Length()})
}
