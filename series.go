package main

// Series: seasons/episodes from Cinemeta and torrents per episode or season
// pack from EZTV + TPB. Verified 2026-11-04:
//   Cinemeta GET /meta/series/<tt>.json -> meta.videos[]: {season, episode,
//            name|title, released (ISO), thumbnail, overview|description}.
//            Season 0 = specials.
//   EZTV     GET get-torrents?imdb_id=<digits>&limit=100&page=N ->
//            {torrents_count, torrents[]}. The IMDb id is sometimes wrong
//            (0903747 returns "Breaking Brad"), so names are checked too.
//   TPB      q.php?q=<title> S02 | Season 2 | S02E05 | complete -> episodes
//            and packs ("The.Bear.S02.COMPLETE", "Season 2 Complete",
//            "S01-S05"); "imdb" is usually set.
//
// Ranking follows Jorge's choices (SERIES_PLAN.md): torrents with enough
// seeders first, then formats the browser can play (no HEVC/x265), then
// Latino/Dual/Spanish audio, then 1080p > 720p > 2160p, then seeders. A single episode is preferred over a season pack.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	seriesMetaTTL      = 6 * time.Hour
	seriesTorrentsTTL  = 30 * time.Minute
	seriesEZTVMaxPages = 6
	seriesTimeout      = 20 * time.Second
	seriesMinSeeders   = 10 // below this a torrent streams poorly: last resort
	seriesMaxPerList   = 12
)

type seriesEpisode struct {
	Episode   int    `json:"episode"`
	Name      string `json:"name"`
	Released  string `json:"released,omitempty"` // YYYY-MM-DD
	Thumbnail string `json:"thumbnail,omitempty"`
	Overview  string `json:"overview,omitempty"`
	Aired     bool   `json:"aired"`
}

type seriesSeason struct {
	Season   int             `json:"season"`
	Episodes []seriesEpisode `json:"episodes"`
}

type seriesInfo struct {
	Meta    *mediaMeta     `json:"meta"`
	Seasons []seriesSeason `json:"seasons"`
}

// ---------------------------------------------------------------------------
// Seasons and episodes (Cinemeta)
// ---------------------------------------------------------------------------

type cinemetaVideo struct {
	Season      flexInt    `json:"season"`
	Episode     flexInt    `json:"episode"`
	Name        string     `json:"name"`
	Title       string     `json:"title"`
	Released    flexString `json:"released"`
	Thumbnail   string     `json:"thumbnail"`
	Overview    string     `json:"overview"`
	Description string     `json:"description"`
}

func parseSeriesMeta(body []byte, now time.Time) (*seriesInfo, error) {
	var resp struct {
		Meta struct {
			cinemetaItem
			Videos []cinemetaVideo `json:"videos"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	meta := resp.Meta.toMeta()
	if meta == nil {
		return nil, errors.New("unknown series")
	}
	meta.Type = "series"

	bySeason := map[int]map[int]seriesEpisode{}
	for _, v := range resp.Meta.Videos {
		s, e := int(v.Season), int(v.Episode)
		if s < 0 || e <= 0 {
			continue
		}
		ep := seriesEpisode{Episode: e, Name: v.Name, Overview: v.Overview}
		if ep.Name == "" {
			ep.Name = v.Title
		}
		if ep.Name == "" {
			ep.Name = fmt.Sprintf("Episode %d", e)
		}
		if ep.Overview == "" {
			ep.Overview = v.Description
		}
		if strings.HasPrefix(v.Thumbnail, "https://") {
			ep.Thumbnail = v.Thumbnail
		}
		if t, err := time.Parse(time.RFC3339, string(v.Released)); err == nil {
			ep.Released = t.Format("2006-01-02")
			ep.Aired = !t.After(now)
		}
		if bySeason[s] == nil {
			bySeason[s] = map[int]seriesEpisode{}
		}
		bySeason[s][e] = ep // duplicates: last one wins
	}

	info := &seriesInfo{Meta: meta, Seasons: []seriesSeason{}}
	for s, eps := range bySeason {
		season := seriesSeason{Season: s}
		for _, ep := range eps {
			season.Episodes = append(season.Episodes, ep)
		}
		sort.Slice(season.Episodes, func(i, j int) bool { return season.Episodes[i].Episode < season.Episodes[j].Episode })
		info.Seasons = append(info.Seasons, season)
	}
	// regular seasons in order, specials (0) last
	sort.Slice(info.Seasons, func(i, j int) bool {
		a, b := info.Seasons[i].Season, info.Seasons[j].Season
		if (a == 0) != (b == 0) {
			return b == 0
		}
		return a < b
	})
	return info, nil
}

type timedCache[T any] struct {
	sync.Mutex
	m map[string]timedEntry[T]
}

type timedEntry[T any] struct {
	v       T
	expires time.Time
}

func (c *timedCache[T]) get(key string) (T, bool) {
	c.Lock()
	defer c.Unlock()
	e, ok := c.m[key]
	if !ok || time.Now().After(e.expires) {
		var zero T
		return zero, false
	}
	return e.v, true
}

func (c *timedCache[T]) put(key string, v T, ttl time.Duration) {
	c.Lock()
	defer c.Unlock()
	if c.m == nil {
		c.m = map[string]timedEntry[T]{}
	}
	now := time.Now()
	for k, e := range c.m { // tiny caches: sweep expired entries on write
		if now.After(e.expires) {
			delete(c.m, k)
		}
	}
	c.m[key] = timedEntry[T]{v: v, expires: now.Add(ttl)}
}

var seriesMetaCache timedCache[*seriesInfo]

func loadSeries(ctx context.Context, id string) (*seriesInfo, error) {
	if info, ok := seriesMetaCache.get(id); ok {
		return info, nil
	}
	body, err := fetchBodyTimeout(ctx, createSelectiveProxyClient(), fmt.Sprintf("%s/meta/series/%s.json", cinemetaBase, id), metaRequestTimeout)
	if err != nil {
		return nil, err
	}
	info, err := parseSeriesMeta(body, time.Now())
	if err != nil {
		return nil, err
	}
	seriesMetaCache.put(id, info, seriesMetaTTL)
	return info, nil
}

// ---------------------------------------------------------------------------
// Torrent names: episode or season pack
// ---------------------------------------------------------------------------

var (
	// S02E05, plus an optional last episode: S01E01-E03, S01E01-03, S01E01E02
	reSeriesEpisode = regexp.MustCompile(`(?i)\bS(\d{1,2})[ -]*E(\d{1,3})(?:(?: *- *E?|E)(\d{1,3}))?\b`)
	reSeasonRange   = regexp.MustCompile(`(?i)\bS(\d{1,2})[ -]*(?:-|to)[ -]*S(\d{1,2})\b`)
	reSeasonWord    = regexp.MustCompile(`(?i)\bSeasons?[ -]*(\d{1,2})(?:[ -]*(?:-|to|&)[ -]*(\d{1,2}))?\b`)
	reSeasonShort   = regexp.MustCompile(`(?i)\bS(\d{1,2})\b`)
	reCompleteShow  = regexp.MustCompile(`(?i)\bcomplete[ -]*(?:series|collection)\b`)
	reLatinoAudio   = regexp.MustCompile(`(?i)\b(latino|lat|dual|dual[ -]?audio|castellano|espanol|español|spanish|esp)\b`)
	reQuality1080   = regexp.MustCompile(`(?i)\b1080p\b`)
	reQuality720    = regexp.MustCompile(`(?i)\b720p\b`)
	reQuality2160   = regexp.MustCompile(`(?i)\b(2160p|4k|uhd)\b`)
	// browsers can't decode HEVC (10-bit even less): such torrents only
	// play in an external player
	reHEVC = regexp.MustCompile(`(?i)\b(x265|h265|h 265|hevc|10bit|10 bit)\b`)
)

// seriesRelease is what a torrent name says it holds.
type seriesRelease struct {
	Pack       bool
	SeasonFrom int
	SeasonTo   int // == SeasonFrom for a single season; 0/0 + Pack = whole show
	Episode    int // single episode (Pack == false)
}

func (r seriesRelease) hasSeason(s int) bool {
	if r.SeasonFrom == 0 && r.SeasonTo == 0 {
		return r.Pack // "complete series"
	}
	return s >= r.SeasonFrom && s <= r.SeasonTo
}

func classifySeriesName(name string) (seriesRelease, bool) {
	clean := tidyTitle(strings.NewReplacer(".", " ", "_", " ").Replace(name))

	if m := reSeriesEpisode.FindStringSubmatch(clean); m != nil {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		if last, _ := strconv.Atoi(m[3]); last > e && last-e < 30 {
			// several episodes of one season
			return seriesRelease{Pack: true, SeasonFrom: s, SeasonTo: s}, true
		}
		return seriesRelease{SeasonFrom: s, SeasonTo: s, Episode: e}, e > 0
	}
	if m := reNxNN.FindStringSubmatch(clean); m != nil {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		return seriesRelease{SeasonFrom: s, SeasonTo: s, Episode: e}, s > 0 && e > 0
	}
	if m := reSeasonRange.FindStringSubmatch(clean); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		if a > 0 && b >= a {
			return seriesRelease{Pack: true, SeasonFrom: a, SeasonTo: b}, true
		}
	}
	if m := reSeasonWord.FindStringSubmatch(clean); m != nil {
		a, _ := strconv.Atoi(m[1])
		b := a
		if m[2] != "" {
			b, _ = strconv.Atoi(m[2])
		}
		if a > 0 && b >= a {
			return seriesRelease{Pack: true, SeasonFrom: a, SeasonTo: b}, true
		}
	}
	if m := reSeasonShort.FindStringSubmatch(clean); m != nil {
		if s, _ := strconv.Atoi(m[1]); s > 0 {
			return seriesRelease{Pack: true, SeasonFrom: s, SeasonTo: s}, true
		}
	}
	if reCompleteShow.MatchString(clean) {
		return seriesRelease{Pack: true}, true
	}
	return seriesRelease{}, false
}

// belongsToSeries: the torrent's IMDb id, when known, must match; otherwise
// the name must start with the series title ("The Bear S02" yes, "BJ and
// The Bear" or "The Yogi Bear Show" no).
func belongsToSeries(r searchResult, imdb, title string) bool {
	if r.imdb != "" && imdbIDRe.MatchString(r.imdb) {
		return r.imdb == imdb
	}
	name := normalizeText(r.Title)
	t := normalizeText(title)
	if t == "" {
		return false
	}
	if strings.HasPrefix(name+" ", t+" ") {
		return true
	}
	// "The Office" is often released as "Office"
	if rest, ok := strings.CutPrefix(t, "the "); ok {
		return strings.HasPrefix(name+" ", rest+" ")
	}
	return false
}

func qualityTier(name string) int {
	switch {
	case reQuality1080.MatchString(name):
		return 0
	case reQuality720.MatchString(name):
		return 1
	case reQuality2160.MatchString(name):
		return 2
	}
	return 3
}

// rankSeriesResults orders torrents by preference (see the file comment).
func rankSeriesResults(list []searchResult) {
	viable := func(r searchResult) bool { return r.Seeders >= seriesMinSeeders }
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if viable(a) != viable(b) {
			return viable(a)
		}
		if ha, hb := reHEVC.MatchString(normalizeText(a.Title)), reHEVC.MatchString(normalizeText(b.Title)); ha != hb {
			return hb
		}
		if la, lb := reLatinoAudio.MatchString(normalizeText(a.Title)), reLatinoAudio.MatchString(normalizeText(b.Title)); la != lb {
			return la
		}
		if qa, qb := qualityTier(a.Title), qualityTier(b.Title); qa != qb {
			return qa < qb
		}
		return a.Seeders > b.Seeders
	})
}

// ---------------------------------------------------------------------------
// Torrent sources
// ---------------------------------------------------------------------------

var seriesEZTVCache timedCache[[]searchResult]

// seriesFromEZTV loads every EZTV torrent of the show (several pages).
func seriesFromEZTV(ctx context.Context, client *http.Client, imdb string) ([]searchResult, error) {
	id := eztvImdbID(imdb)
	if id == "" {
		return nil, nil
	}
	if res, ok := seriesEZTVCache.get(id); ok {
		return res, nil
	}
	page := func(n int) (int, []eztvTorrent, error) {
		body, err := fetchBody(ctx, client, fmt.Sprintf("%s?imdb_id=%s&limit=100&page=%d", eztvAPI, id, n))
		if err != nil {
			return 0, nil, err
		}
		var resp struct {
			Count    flexInt       `json:"torrents_count"`
			Torrents []eztvTorrent `json:"torrents"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return 0, nil, err
		}
		return int(resp.Count), resp.Torrents, nil
	}

	count, first, err := page(1)
	if err != nil {
		return nil, err
	}
	pages := min((count+99)/100, seriesEZTVMaxPages)
	rest := make([][]eztvTorrent, max(pages-1, 0))
	var wg sync.WaitGroup
	for i := range rest {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, rest[i], _ = page(i + 2) // a missing page only means fewer results
		}(i)
	}
	wg.Wait()

	var out []searchResult
	for _, list := range append([][]eztvTorrent{first}, rest...) {
		for _, t := range list {
			if r, ok := t.toResult(); ok {
				out = append(out, r) // no imdb set: EZTV ids are not trusted
			}
		}
	}
	seriesEZTVCache.put(id, out, seriesTorrentsTTL)
	return out, nil
}

var seriesTPBCache timedCache[[]searchResult]

func seriesFromTPB(ctx context.Context, client *http.Client, query string) ([]searchResult, error) {
	if res, ok := seriesTPBCache.get(query); ok {
		return res, nil
	}
	res, err := searchTPB(ctx, client, query, "tv")
	if err != nil {
		return nil, err
	}
	seriesTPBCache.put(query, res, seriesTorrentsTTL)
	return res, nil
}

type seriesTorrentList struct {
	Season   int                       `json:"season"`
	Episodes map[string][]searchResult `json:"episodes"` // episode number -> torrents
	Packs    []searchResult            `json:"packs"`
	Best     map[string]*seriesPick    `json:"best"` // episode number -> automatic choice
	Sources  []sourceStatus            `json:"sources"`
}

type seriesPick struct {
	Pack    bool         `json:"pack"`
	Torrent searchResult `json:"torrent"`
}

// buildSeriesTorrentList sorts candidates into episodes / packs of a season.
// episode > 0 limits the answer to that episode (packs are still included).
func buildSeriesTorrentList(candidates []searchResult, imdb, title string, season, episode int) *seriesTorrentList {
	out := &seriesTorrentList{Season: season, Episodes: map[string][]searchResult{}, Packs: []searchResult{}, Best: map[string]*seriesPick{}}
	for _, r := range mergeResults(candidates) {
		if !belongsToSeries(r, imdb, title) {
			continue
		}
		rel, ok := classifySeriesName(r.Title)
		if !ok || !rel.hasSeason(season) {
			continue
		}
		if rel.Pack {
			out.Packs = append(out.Packs, r)
			continue
		}
		if episode > 0 && rel.Episode != episode {
			continue
		}
		key := strconv.Itoa(rel.Episode)
		out.Episodes[key] = append(out.Episodes[key], r)
	}

	rankSeriesResults(out.Packs)
	if len(out.Packs) > seriesMaxPerList {
		out.Packs = out.Packs[:seriesMaxPerList]
	}
	var bestPack *searchResult
	if len(out.Packs) > 0 && out.Packs[0].Seeders >= seriesMinSeeders {
		bestPack = &out.Packs[0]
	}
	for key, list := range out.Episodes {
		rankSeriesResults(list)
		if len(list) > seriesMaxPerList {
			list = list[:seriesMaxPerList]
		}
		out.Episodes[key] = list
		// a single episode first; a season pack only when the episode has
		// no torrent with enough seeders
		switch {
		case list[0].Seeders >= seriesMinSeeders:
			out.Best[key] = &seriesPick{Torrent: list[0]}
		case bestPack != nil:
			out.Best[key] = &seriesPick{Pack: true, Torrent: *bestPack}
		default:
			out.Best[key] = &seriesPick{Torrent: list[0]}
		}
	}
	if episode > 0 && out.Best[strconv.Itoa(episode)] == nil && len(out.Packs) > 0 {
		out.Best[strconv.Itoa(episode)] = &seriesPick{Pack: true, Torrent: out.Packs[0]}
	}
	return out
}

func loadSeriesTorrents(ctx context.Context, info *seriesInfo, season, episode int) *seriesTorrentList {
	client := createSelectiveProxyClient()
	title := info.Meta.Title
	sources := enabledPublicSources()
	enabled := func(id string) bool {
		for _, s := range sources {
			if s.ID == id {
				return true
			}
		}
		return false
	}

	var jobs []searchJob
	if enabled("eztv") {
		jobs = append(jobs, searchJob{"eztv", "EZTV", func(ctx context.Context) ([]searchResult, error) {
			return seriesFromEZTV(ctx, client, info.Meta.IMDb)
		}})
	}
	if enabled("tpb") {
		queries := []string{
			fmt.Sprintf("%s S%02d", title, season),
			fmt.Sprintf("%s Season %d", title, season),
			title + " complete",
		}
		if episode > 0 {
			queries = append(queries, fmt.Sprintf("%s S%02dE%02d", title, season, episode))
		}
		jobs = append(jobs, searchJob{"tpb", "The Pirate Bay", func(ctx context.Context) ([]searchResult, error) {
			lists := make([][]searchResult, len(queries))
			errs := make([]error, len(queries))
			var wg sync.WaitGroup
			for i, q := range queries {
				wg.Add(1)
				go func(i int, q string) {
					defer wg.Done()
					lists[i], errs[i] = seriesFromTPB(ctx, client, q)
				}(i, q)
			}
			wg.Wait()
			var all []searchResult
			for _, l := range lists {
				all = append(all, l...)
			}
			if len(all) == 0 {
				return nil, errors.Join(errs...)
			}
			return all, nil
		}})
	}

	ctx, cancel := context.WithTimeout(ctx, seriesTimeout)
	defer cancel()
	results := make([][]searchResult, len(jobs))
	statuses := make([]sourceStatus, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func(i int, job searchJob) {
			defer wg.Done()
			res, err := job.run(ctx)
			st := sourceStatus{ID: job.id, Label: job.label, OK: err == nil, Count: len(res)}
			if ctx.Err() != nil && err != nil {
				st.Error = "timed out"
			} else if err != nil {
				st.Error = err.Error()
			}
			results[i], statuses[i] = res, st
		}(i, job)
	}
	wg.Wait()

	var all []searchResult
	for _, r := range results {
		all = append(all, r...)
	}
	list := buildSeriesTorrentList(all, info.Meta.IMDb, title, season, episode)
	list.Sources = statuses
	return list
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// seriesHandler:
//
//	GET /api/v1/series/<tt>                               seasons + episodes
//	GET /api/v1/series/<tt>/torrents?season=N[&episode=M] episode torrents + packs
func seriesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/series/"), "/"), "/")
	id := parts[0]
	if !imdbIDRe.MatchString(id) || len(parts) > 2 || (len(parts) == 2 && parts[1] != "torrents") {
		respondWithJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
		return
	}

	info, err := loadSeries(r.Context(), id)
	if err != nil {
		log.Printf("Series: %s failed: %v", id, err)
		respondWithJSON(w, http.StatusBadGateway, map[string]string{"error": "Could not load this series"})
		return
	}
	if len(parts) == 1 {
		respondWithJSON(w, http.StatusOK, info)
		return
	}

	season, err := strconv.Atoi(r.URL.Query().Get("season"))
	if err != nil || season < 0 || season > 99 {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid season"})
		return
	}
	episode := 0
	if e := r.URL.Query().Get("episode"); e != "" {
		if episode, err = strconv.Atoi(e); err != nil || episode <= 0 {
			respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid episode"})
			return
		}
	}
	if len(enabledPublicSources()) == 0 {
		respondWithJSON(w, http.StatusConflict, map[string]string{"error": "Public torrent search is disabled in Settings"})
		return
	}
	list := loadSeriesTorrents(r.Context(), info, season, episode)
	log.Printf("Series: %s S%02d (episode %d) -> %d episode(s), %d pack(s)", id, season, episode, len(list.Episodes), len(list.Packs))
	respondWithJSON(w, http.StatusOK, list)
}
