package main

// Poster / year / IMDb metadata for search results, from Cinemeta (the
// public metadata API Stremio uses, no key needed). Verified 2026-11-04:
//   GET https://v3-cinemeta.strem.io/meta/<movie|series>/<tt id>.json
//       -> {"meta":{"imdb_id","type","name","year","releaseInfo","imdbRating","poster","genres"}}
//   GET https://v3-cinemeta.strem.io/catalog/<movie|series>/top/search=<q>.json
//       -> {"metas":[{"id","type","name","releaseInfo","poster"}]}  (no rating)
//   Unknown ids answer 307 (no JSON), treated as "not found".
//
// Results are grouped by title: an IMDb id from the source (YTS, TPB) is
// used directly, otherwise the torrent name is parsed (parseMediaName) and
// searched by title. Lookups are cached and bounded in time, and the search
// still answers if Cinemeta is down (everything lands in the "other" group).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cinemetaBase        = "https://v3-cinemeta.strem.io"
	metaRequestTimeout  = 5 * time.Second
	metaLookupBudget    = 6 * time.Second
	metaConcurrency     = 6
	metaMaxTitleQueries = 25 // name searches per request (ids are cheap)
	metaCacheTTL        = 24 * time.Hour
	metaMissTTL         = time.Hour
	metaCacheMax        = 5000
)

type mediaMeta struct {
	IMDb   string   `json:"imdb"`
	Type   string   `json:"type"` // movie | series
	Title  string   `json:"title"`
	Year   string   `json:"year,omitempty"`
	Poster string   `json:"poster,omitempty"`
	Rating string   `json:"rating,omitempty"`
	Genres []string `json:"genres,omitempty"`
}

type resultGroup struct {
	Key     string         `json:"key"`  // IMDb id, or "other"
	Meta    *mediaMeta     `json:"meta"` // nil for "other"
	Results []searchResult `json:"results"`
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

type metaCacheEntry struct {
	meta    *mediaMeta // nil = known miss
	expires time.Time
}

var metaCache = struct {
	sync.Mutex
	m map[string]metaCacheEntry
}{m: map[string]metaCacheEntry{}}

func metaCacheGet(key string) (*mediaMeta, bool) {
	metaCache.Lock()
	defer metaCache.Unlock()
	e, ok := metaCache.m[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.meta, true
}

func metaCachePut(key string, m *mediaMeta) {
	ttl := metaCacheTTL
	if m == nil {
		ttl = metaMissTTL
	}
	metaCache.Lock()
	defer metaCache.Unlock()
	if len(metaCache.m) >= metaCacheMax {
		metaCache.m = map[string]metaCacheEntry{} // crude but bounded
	}
	metaCache.m[key] = metaCacheEntry{meta: m, expires: time.Now().Add(ttl)}
}

// ---------------------------------------------------------------------------
// Cinemeta client
// ---------------------------------------------------------------------------

var imdbIDRe = regexp.MustCompile(`^tt\d{5,10}$`)

type cinemetaItem struct {
	ID          string     `json:"id"`
	IMDbID      string     `json:"imdb_id"`
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	Year        flexString `json:"year"`
	ReleaseInfo flexString `json:"releaseInfo"`
	Poster      string     `json:"poster"`
	IMDbRating  flexString `json:"imdbRating"`
	Genres      []string   `json:"genres"`
}

func (c cinemetaItem) toMeta() *mediaMeta {
	id := c.IMDbID
	if !imdbIDRe.MatchString(id) {
		id = c.ID
	}
	if !imdbIDRe.MatchString(id) || c.Name == "" {
		return nil
	}
	m := &mediaMeta{
		IMDb:   id,
		Type:   c.Type,
		Title:  c.Name,
		Year:   string(c.Year),
		Rating: string(c.IMDbRating),
		Genres: c.Genres,
	}
	if m.Year == "" {
		m.Year = string(c.ReleaseInfo)
	}
	if strings.HasPrefix(c.Poster, "https://") {
		m.Poster = c.Poster
	}
	return m
}

func cinemetaTypes(preferred string) []string {
	if preferred == "series" {
		return []string{"series", "movie"}
	}
	return []string{"movie", "series"}
}

// metaByID returns the full metadata of an IMDb id (cached).
func metaByID(ctx context.Context, client *http.Client, id, typ string) *mediaMeta {
	key := "id:" + id
	if m, ok := metaCacheGet(key); ok {
		return m
	}
	for _, t := range cinemetaTypes(typ) {
		body, err := fetchBodyTimeout(ctx, client, fmt.Sprintf("%s/meta/%s/%s.json", cinemetaBase, t, id), metaRequestTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil // out of time: do not cache a miss
			}
			continue
		}
		var resp struct {
			Meta cinemetaItem `json:"meta"`
		}
		if json.Unmarshal(body, &resp) != nil {
			continue
		}
		if m := resp.Meta.toMeta(); m != nil {
			metaCachePut(key, m)
			return m
		}
	}
	metaCachePut(key, nil)
	return nil
}

// releaseYear extracts the first year of "2008–2013" / "1999".
func releaseYear(s string) int {
	if len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil {
			return y
		}
	}
	return 0
}

// pickCatalogMatch chooses the catalog item whose name equals the title;
// with a year, the closest release year wins (±1 tolerated). No exact name
// match means no match: a wrong poster is worse than none.
func pickCatalogMatch(items []cinemetaItem, title string, year int) *cinemetaItem {
	want := normalizeText(title)
	var best *cinemetaItem
	bestDiff := 1 << 30
	for i := range items {
		it := &items[i]
		if normalizeText(it.Name) != want {
			continue
		}
		if year == 0 {
			return it // catalog order is relevance
		}
		y := releaseYear(string(it.ReleaseInfo))
		if y == 0 {
			y = releaseYear(string(it.Year))
		}
		diff := y - year
		if diff < 0 {
			diff = -diff
		}
		if diff <= 1 && diff < bestDiff {
			best, bestDiff = it, diff
		}
	}
	return best
}

// metaByTitle searches Cinemeta by name (cached), then loads the full meta.
func metaByTitle(ctx context.Context, client *http.Client, title string, year int, typ string) *mediaMeta {
	key := fmt.Sprintf("q:%s:%s:%d", typ, normalizeText(title), year)
	if m, ok := metaCacheGet(key); ok {
		return m
	}
	u := fmt.Sprintf("%s/catalog/%s/top/search=%s.json", cinemetaBase, typ, url.PathEscape(strings.ToLower(title)))
	body, err := fetchBodyTimeout(ctx, client, u, metaRequestTimeout)
	if err != nil {
		if ctx.Err() == nil {
			metaCachePut(key, nil)
		}
		return nil
	}
	var resp struct {
		Metas []cinemetaItem `json:"metas"`
	}
	if json.Unmarshal(body, &resp) != nil {
		metaCachePut(key, nil)
		return nil
	}
	match := pickCatalogMatch(resp.Metas, title, year)
	if match == nil {
		metaCachePut(key, nil)
		return nil
	}
	m := match.toMeta()
	if m != nil {
		if full := metaByID(ctx, client, m.IMDb, typ); full != nil {
			m = full // adds rating and genres
		}
	}
	metaCachePut(key, m)
	return m
}

// ---------------------------------------------------------------------------
// Grouping
// ---------------------------------------------------------------------------

type metaQuery struct {
	id    string
	title string
	year  int
	typ   string
}

func (q metaQuery) key() string {
	if q.id != "" {
		return "id:" + q.id
	}
	return fmt.Sprintf("q:%s:%s:%d", q.typ, normalizeText(q.title), q.year)
}

// queryForResult decides how to identify a torrent: by the IMDb id the
// source gave, or by the title/year parsed from its name.
func queryForResult(r searchResult) (metaQuery, bool) {
	typ := "movie"
	if r.Category == "tv" {
		typ = "series"
	}
	if imdbIDRe.MatchString(r.imdb) {
		return metaQuery{id: r.imdb, typ: typ}, true
	}
	// the fake extension keeps parseMediaName from treating "1.85GB - YIFY"
	// as a file extension
	info := parseMediaName(strings.ReplaceAll(r.Title, "/", " ") + ".mkv")
	if info.Season > 0 {
		typ = "series"
	}
	title := strings.TrimSpace(info.Title)
	if len(normalizeText(title)) < 2 {
		return metaQuery{}, false
	}
	return metaQuery{title: title, year: info.Year, typ: typ}, true
}

// groupResults identifies every result with resolve and groups them by IMDb
// id, keeping the incoming (seeders) order; unidentified results go last.
func groupResults(results []searchResult, resolve func(metaQuery) *mediaMeta) []resultGroup {
	var groups []resultGroup
	index := map[string]int{}
	var other []searchResult
	for _, r := range results {
		var m *mediaMeta
		if q, ok := queryForResult(r); ok {
			m = resolve(q)
		}
		if m == nil {
			other = append(other, r)
			continue
		}
		if i, ok := index[m.IMDb]; ok {
			groups[i].Results = append(groups[i].Results, r)
			continue
		}
		index[m.IMDb] = len(groups)
		groups = append(groups, resultGroup{Key: m.IMDb, Meta: m, Results: []searchResult{r}})
	}
	if len(other) > 0 {
		groups = append(groups, resultGroup{Key: "other", Results: other})
	}
	return groups
}

// enrichResults resolves metadata for all results concurrently within
// metaLookupBudget and returns the grouped results.
func enrichResults(parent context.Context, results []searchResult) []resultGroup {
	ctx, cancel := context.WithTimeout(parent, metaLookupBudget)
	defer cancel()
	client := createSelectiveProxyClient()

	// unique queries, most-seeded first (results are already sorted)
	var queries []metaQuery
	seen := map[string]bool{}
	titleQueries := 0
	for _, r := range results {
		q, ok := queryForResult(r)
		if !ok || seen[q.key()] {
			continue
		}
		if q.id == "" {
			if titleQueries >= metaMaxTitleQueries {
				continue
			}
			titleQueries++
		}
		seen[q.key()] = true
		queries = append(queries, q)
	}

	resolved := map[string]*mediaMeta{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, metaConcurrency)
	for _, q := range queries {
		wg.Add(1)
		go func(q metaQuery) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			var m *mediaMeta
			if q.id != "" {
				m = metaByID(ctx, client, q.id, q.typ)
			} else {
				m = metaByTitle(ctx, client, q.title, q.year, q.typ)
			}
			mu.Lock()
			resolved[q.key()] = m
			mu.Unlock()
		}(q)
	}
	wg.Wait()

	return groupResults(results, func(q metaQuery) *mediaMeta {
		return resolved[q.key()]
	})
}
