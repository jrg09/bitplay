package main

// Trailer of a movie or series, found automatically.
//
//	GET /api/v1/trailer?imdb=tt0903747&type=series -> {"youtubeId": "abc123def45", "lang": "es", "source": "tmdb"}
//
// With a TMDB key (see overview.go) the videos of the title are listed in
// the configured language and in English; trailers come before teasers,
// the configured language before English before any other, official before
// unofficial, newer before older. Without a key, or when TMDB has none,
// Cinemeta's trailer is used. Candidates are checked against YouTube's
// oEmbed endpoint so a video that can't be embedded is skipped.
// An empty youtubeId means nothing was found.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	trailerTTL        = 7 * 24 * time.Hour
	trailerMissTTL    = 10 * time.Minute
	trailerCacheMax   = 3000
	trailerMaxChecked = 4 // candidates tried against oEmbed
	youtubeOEmbed     = "https://www.youtube.com/oembed"
)

var youtubeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

type trailerResult struct {
	YouTubeID string `json:"youtubeId"`
	Lang      string `json:"lang,omitempty"`
	Source    string `json:"source,omitempty"`
}

type tmdbVideo struct {
	Key         string `json:"key"`
	Site        string `json:"site"`
	Type        string `json:"type"`
	Lang        string `json:"iso_639_1"`
	Official    bool   `json:"official"`
	PublishedAt string `json:"published_at"`
}

func parseTMDBVideos(body []byte) ([]tmdbVideo, error) {
	var data struct {
		Results []tmdbVideo `json:"results"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data.Results, nil
}

// parseTMDBFindID returns "movie" or "tv" and the TMDB id of the first
// result of the wanted type (the other type is a fallback).
func parseTMDBFindID(body []byte, typ string) (kind string, id int, err error) {
	var data struct {
		Movies []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
		TV []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	if err = json.Unmarshal(body, &data); err != nil {
		return "", 0, err
	}
	hasMovie, hasTV := len(data.Movies) > 0, len(data.TV) > 0
	switch {
	case typ == "series" && hasTV, !hasMovie && hasTV:
		return "tv", data.TV[0].ID, nil
	case hasMovie:
		return "movie", data.Movies[0].ID, nil
	}
	return "", 0, nil
}

func parseCinemetaTrailers(body []byte) ([]string, error) {
	var data struct {
		Meta struct {
			Trailers []struct {
				Source string `json:"source"`
				Type   string `json:"type"`
			} `json:"trailers"`
			TrailerStreams []struct {
				YtID string `json:"ytId"`
			} `json:"trailerStreams"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if youtubeIDRe.MatchString(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, t := range data.Meta.Trailers {
		if t.Type == "" || strings.EqualFold(t.Type, "Trailer") {
			add(t.Source)
		}
	}
	for _, t := range data.Meta.TrailerStreams {
		add(t.YtID)
	}
	return ids, nil
}

// rankTrailers keeps the YouTube trailers and teasers and orders them best
// first. prefLang is a language code such as "es" or "es-MX".
func rankTrailers(videos []tmdbVideo, prefLang string) []tmdbVideo {
	pref := strings.ToLower(strings.SplitN(prefLang, "-", 2)[0])
	var out []tmdbVideo
	seen := map[string]bool{}
	for _, v := range videos {
		if v.Site != "YouTube" || !youtubeIDRe.MatchString(v.Key) || seen[v.Key] {
			continue
		}
		if v.Type != "Trailer" && v.Type != "Teaser" {
			continue
		}
		seen[v.Key] = true
		out = append(out, v)
	}
	typeRank := func(v tmdbVideo) int {
		if v.Type == "Trailer" {
			return 0
		}
		return 1
	}
	langRank := func(v tmdbVideo) int {
		switch strings.ToLower(v.Lang) {
		case pref:
			return 0
		case "en":
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if typeRank(a) != typeRank(b) {
			return typeRank(a) < typeRank(b)
		}
		if langRank(a) != langRank(b) {
			return langRank(a) < langRank(b)
		}
		if a.Official != b.Official {
			return a.Official
		}
		return a.PublishedAt > b.PublishedAt
	})
	return out
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// trailerSources are the lookups, injectable for tests. tmdbVideos is nil
// when there is no API key.
type trailerSources struct {
	tmdbVideos func(ctx context.Context, imdb, typ string, langs []string) ([]tmdbVideo, error)
	cinemeta   func(ctx context.Context, imdb, typ string) ([]string, error)
	embeddable func(ctx context.Context, id string) bool
	lang       string
}

func lookupTrailer(ctx context.Context, src trailerSources, imdb, typ string) trailerResult {
	embeddable := src.embeddable
	if embeddable == nil {
		embeddable = func(context.Context, string) bool { return true }
	}

	if src.tmdbVideos != nil {
		langs := []string{src.lang}
		if !strings.HasPrefix(src.lang, "en") {
			langs = append(langs, "en-US")
		}
		if videos, err := src.tmdbVideos(ctx, imdb, typ, langs); err == nil {
			for i, v := range rankTrailers(videos, src.lang) {
				if i >= trailerMaxChecked {
					break
				}
				if embeddable(ctx, v.Key) {
					return trailerResult{YouTubeID: v.Key, Lang: strings.ToLower(v.Lang), Source: "tmdb"}
				}
			}
		}
	}
	if src.cinemeta != nil {
		if ids, err := src.cinemeta(ctx, imdb, typ); err == nil {
			for i, id := range ids {
				if i >= trailerMaxChecked {
					break
				}
				if embeddable(ctx, id) {
					return trailerResult{YouTubeID: id, Source: "cinemeta"}
				}
			}
		}
	}
	return trailerResult{}
}

func tmdbVideosFor(ctx context.Context, client *http.Client, base, key, imdb, typ string, langs []string) ([]tmdbVideo, error) {
	q := url.Values{}
	q.Set("external_source", "imdb_id")
	header := map[string]string{}
	if strings.HasPrefix(key, "eyJ") {
		header["Authorization"] = "Bearer " + key
	} else {
		q.Set("api_key", key)
	}
	body, err := httpGetLimited(ctx, client, base+"/find/"+url.PathEscape(imdb)+"?"+q.Encode(), header)
	if err != nil {
		return nil, err
	}
	kind, id, err := parseTMDBFindID(body, typ)
	if err != nil || id == 0 {
		return nil, err
	}
	var all []tmdbVideo
	var firstErr error
	for _, lang := range langs {
		vq := url.Values{}
		vq.Set("language", lang)
		if header["Authorization"] == "" {
			vq.Set("api_key", key)
		}
		body, err := httpGetLimited(ctx, client, fmt.Sprintf("%s/%s/%d/videos?%s", base, kind, id, vq.Encode()), header)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		videos, err := parseTMDBVideos(body)
		if err != nil {
			continue
		}
		all = append(all, videos...)
	}
	if len(all) == 0 {
		return nil, firstErr
	}
	return all, nil
}

func cinemetaTrailers(ctx context.Context, client *http.Client, base, imdb, typ string) ([]string, error) {
	t := "movie"
	if typ == "series" {
		t = "series"
	}
	body, err := httpGetLimited(ctx, client, fmt.Sprintf("%s/meta/%s/%s.json", base, t, url.PathEscape(imdb)), nil)
	if err != nil {
		return nil, err
	}
	return parseCinemetaTrailers(body)
}

// youtubeEmbeddable asks oEmbed: 200 means the video exists and can be
// embedded, 401 that embedding is disabled, 400/404 that it doesn't exist.
// Network problems count as embeddable so YouTube being unreachable from
// the server doesn't hide a trailer the browser can play.
func youtubeEmbeddable(ctx context.Context, client *http.Client, base, id string) bool {
	u := base + "?format=json&url=" + url.QueryEscape("https://www.youtube.com/watch?v="+id)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return true
	}
	resp, err := client.Do(req)
	if err != nil {
		return true
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func liveTrailerSources() trailerSources {
	client := createSelectiveProxyClient()
	src := trailerSources{
		lang: tmdbLanguage(),
		cinemeta: func(ctx context.Context, imdb, typ string) ([]string, error) {
			return cinemetaTrailers(ctx, client, cinemetaBase, imdb, typ)
		},
		embeddable: func(ctx context.Context, id string) bool {
			return youtubeEmbeddable(ctx, client, youtubeOEmbed, id)
		},
	}
	if key := tmdbAPIKey(); key != "" {
		src.tmdbVideos = func(ctx context.Context, imdb, typ string, langs []string) ([]tmdbVideo, error) {
			return tmdbVideosFor(ctx, client, tmdbBase, key, imdb, typ, langs)
		}
	}
	return src
}

// ---------------------------------------------------------------------------
// Cache and handler
// ---------------------------------------------------------------------------

type trailerEntry struct {
	res     trailerResult
	expires time.Time
}

var trailerCache = struct {
	sync.Mutex
	m map[string]trailerEntry
}{m: map[string]trailerEntry{}}

func trailerCacheGet(key string) (trailerResult, bool) {
	trailerCache.Lock()
	defer trailerCache.Unlock()
	e, ok := trailerCache.m[key]
	if !ok || time.Now().After(e.expires) {
		delete(trailerCache.m, key)
		return trailerResult{}, false
	}
	return e.res, true
}

func trailerCachePut(key string, res trailerResult) {
	ttl := trailerTTL
	if res.YouTubeID == "" {
		ttl = trailerMissTTL
	}
	trailerCache.Lock()
	defer trailerCache.Unlock()
	if len(trailerCache.m) >= trailerCacheMax {
		for k := range trailerCache.m { // full: drop an arbitrary entry
			delete(trailerCache.m, k)
			break
		}
	}
	trailerCache.m[key] = trailerEntry{res: res, expires: time.Now().Add(ttl)}
}

// trailerHandler: GET /api/v1/trailer?imdb=tt...&type=movie|series
func trailerHandler(w http.ResponseWriter, r *http.Request) {
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
	if res, ok := trailerCacheGet(key); ok {
		respondWithJSON(w, http.StatusOK, res)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	res := lookupTrailer(ctx, liveTrailerSources(), imdb, typ)
	if r.Context().Err() == nil {
		trailerCachePut(key, res)
	}
	respondWithJSON(w, http.StatusOK, res)
}
