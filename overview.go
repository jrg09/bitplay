package main

// Synopsis ("overview") of a movie or series, shown when a title is opened.
//
// With BITPLAY_TMDB_API_KEY set (a free themoviedb.org v3 API key, or a v4
// read access token) the synopsis comes from TMDB in BITPLAY_TMDB_LANGUAGE
// (default es-MX), falling back to TMDB in English. Without a key, or when
// TMDB has nothing, Cinemeta's English description is used, so the feature
// works out of the box.
//
//	GET /api/v1/overview?imdb=tt0903747&type=series -> {"overview": "...", "lang": "es-MX"}
//
// An empty overview means no source had one.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	tmdbBase         = "https://api.themoviedb.org/3"
	overviewTimeout  = 8 * time.Second
	overviewTTL      = 7 * 24 * time.Hour
	overviewMissTTL  = 10 * time.Minute // a failure may be transient
	overviewCacheMax = 5000
	overviewMaxLen   = 1500
	defaultTMDBLang  = "es-MX"
)

var tmdbLangRe = regexp.MustCompile(`^[a-z]{2}(-[A-Z]{2})?$`)

func tmdbAPIKey() string {
	return strings.TrimSpace(os.Getenv("BITPLAY_TMDB_API_KEY"))
}

func tmdbLanguage() string {
	if v := strings.TrimSpace(os.Getenv("BITPLAY_TMDB_LANGUAGE")); tmdbLangRe.MatchString(v) {
		return v
	}
	return defaultTMDBLang
}

type overviewResult struct {
	Overview string `json:"overview"`
	Lang     string `json:"lang,omitempty"`
}

func cleanOverview(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if r := []rune(s); len(r) > overviewMaxLen {
		s = string(r[:overviewMaxLen]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// parseTMDBFind picks the overview out of a /find answer, looking first at
// the results of the wanted type.
func parseTMDBFind(body []byte, typ string) (string, error) {
	var data struct {
		Movies []struct {
			Overview string `json:"overview"`
		} `json:"movie_results"`
		TV []struct {
			Overview string `json:"overview"`
		} `json:"tv_results"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", err
	}
	first, second := data.Movies, data.TV
	if typ == "series" {
		first, second = data.TV, data.Movies
	}
	for _, list := range [][]struct {
		Overview string `json:"overview"`
	}{first, second} {
		for _, r := range list {
			if o := cleanOverview(r.Overview); o != "" {
				return o, nil
			}
		}
	}
	return "", nil
}

func parseCinemetaOverview(body []byte) (string, error) {
	var data struct {
		Meta struct {
			Description string `json:"description"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", err
	}
	return cleanOverview(data.Meta.Description), nil
}

func httpGetLimited(ctx context.Context, client *http.Client, u string, header map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, overviewTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// tmdbOverview asks TMDB for the title with the given IMDb id (find endpoint)
// in lang. A v4 read access token (a JWT) goes in the Authorization header,
// a v3 key in the query string.
func tmdbOverview(ctx context.Context, client *http.Client, base, key, imdb, typ, lang string) (string, error) {
	q := url.Values{}
	q.Set("external_source", "imdb_id")
	q.Set("language", lang)
	header := map[string]string{}
	if strings.HasPrefix(key, "eyJ") {
		header["Authorization"] = "Bearer " + key
	} else {
		q.Set("api_key", key)
	}
	body, err := httpGetLimited(ctx, client, base+"/find/"+url.PathEscape(imdb)+"?"+q.Encode(), header)
	if err != nil {
		return "", err
	}
	return parseTMDBFind(body, typ)
}

func cinemetaOverview(ctx context.Context, client *http.Client, base, imdb, typ string) (string, error) {
	t := "movie"
	if typ == "series" {
		t = "series"
	}
	body, err := httpGetLimited(ctx, client, fmt.Sprintf("%s/meta/%s/%s.json", base, t, url.PathEscape(imdb)), nil)
	if err != nil {
		return "", err
	}
	return parseCinemetaOverview(body)
}

// overviewSources are the lookups, injectable for tests. tmdb is nil when
// there is no API key.
type overviewSources struct {
	tmdb     func(ctx context.Context, imdb, typ, lang string) (string, error)
	cinemeta func(ctx context.Context, imdb, typ string) (string, error)
	lang     string
}

// lookupOverview tries TMDB in the wanted language, TMDB in English, then
// Cinemeta (English).
func lookupOverview(ctx context.Context, src overviewSources, imdb, typ string) overviewResult {
	if src.tmdb != nil {
		if o, err := src.tmdb(ctx, imdb, typ, src.lang); err == nil && o != "" {
			return overviewResult{Overview: o, Lang: src.lang}
		}
		if !strings.HasPrefix(src.lang, "en") {
			if o, err := src.tmdb(ctx, imdb, typ, "en-US"); err == nil && o != "" {
				return overviewResult{Overview: o, Lang: "en-US"}
			}
		}
	}
	if src.cinemeta != nil {
		if o, err := src.cinemeta(ctx, imdb, typ); err == nil && o != "" {
			return overviewResult{Overview: o, Lang: "en"}
		}
	}
	return overviewResult{}
}

func liveOverviewSources() overviewSources {
	client := createSelectiveProxyClient()
	src := overviewSources{
		lang: tmdbLanguage(),
		cinemeta: func(ctx context.Context, imdb, typ string) (string, error) {
			return cinemetaOverview(ctx, client, cinemetaBase, imdb, typ)
		},
	}
	if key := tmdbAPIKey(); key != "" {
		src.tmdb = func(ctx context.Context, imdb, typ, lang string) (string, error) {
			return tmdbOverview(ctx, client, tmdbBase, key, imdb, typ, lang)
		}
	}
	return src
}

// ---------------------------------------------------------------------------
// Cache and handler
// ---------------------------------------------------------------------------

type overviewEntry struct {
	res     overviewResult
	expires time.Time
}

var overviewCache = struct {
	sync.Mutex
	m map[string]overviewEntry
}{m: map[string]overviewEntry{}}

func overviewCacheGet(key string) (overviewResult, bool) {
	overviewCache.Lock()
	defer overviewCache.Unlock()
	e, ok := overviewCache.m[key]
	if !ok || time.Now().After(e.expires) {
		delete(overviewCache.m, key)
		return overviewResult{}, false
	}
	return e.res, true
}

func overviewCachePut(key string, res overviewResult) {
	ttl := overviewTTL
	if res.Overview == "" {
		ttl = overviewMissTTL
	}
	overviewCache.Lock()
	defer overviewCache.Unlock()
	if len(overviewCache.m) >= overviewCacheMax {
		for k := range overviewCache.m { // full: drop an arbitrary entry
			delete(overviewCache.m, k)
			break
		}
	}
	overviewCache.m[key] = overviewEntry{res: res, expires: time.Now().Add(ttl)}
}

// overviewHandler: GET /api/v1/overview?imdb=tt...&type=movie|series
func overviewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	imdb := r.URL.Query().Get("imdb")
	if !imdbIDRe.MatchString(imdb) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid IMDb id"})
		return
	}
	typ := "movie"
	if r.URL.Query().Get("type") == "series" {
		typ = "series"
	}
	key := typ + "|" + imdb + "|" + tmdbLanguage()
	if res, ok := overviewCacheGet(key); ok {
		respondWithJSON(w, http.StatusOK, res)
		return
	}
	res := lookupOverview(r.Context(), liveOverviewSources(), imdb, typ)
	if r.Context().Err() == nil {
		overviewCachePut(key, res)
	}
	respondWithJSON(w, http.StatusOK, res)
}
