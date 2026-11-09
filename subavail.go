package main

// "Are there Spanish subtitles for this search result?"
//
// A torrent search result is only a release name, not a file, so the exact
// OpenSubtitles file hash is not available. Instead the title (IMDb id +
// season/episode) is looked up once, the Spanish subtitles' release names
// are cached, and each torrent's release name is compared with them:
//
//	exact    a subtitle made for the same release group
//	similar  same source (WEB-DL, BluRay...) and resolution, other group
//	other    Spanish subtitles exist, but for a different version
//	none     no Spanish subtitles for this title/episode
//
// The search endpoint only needs the OpenSubtitles API key; it does not use
// the daily download quota.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	subAvailLanguages   = "es,ea,sp"
	subAvailMaxReleases = 60
	subAvailMaxKeys     = 6 // distinct title/episode lookups per request
	subAvailTTL         = 24 * time.Hour
	subAvailEmptyTTL    = 6 * time.Hour
	subAvailCacheMax    = 2000
	subAvailConcurrency = 3
	subAvailBudget      = 15 * time.Second
)

type subAvailEntry struct {
	releases []string // release names of the Spanish subtitles
	expires  time.Time
}

var subAvail = struct {
	sync.Mutex
	m   map[string]subAvailEntry
	sem chan struct{}
}{m: map[string]subAvailEntry{}, sem: make(chan struct{}, subAvailConcurrency)}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

var (
	reAvailSource = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(web[ ._-]?dl|webrip|web|blu[ ._-]?ray|bdrip|brrip|remux|hdtv|dvdrip|hdrip|hdcam|cam|telesync)(?:[^a-z0-9]|$)`)
	reAvailRes    = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(480p|576p|720p|1080p|2160p|4k)(?:[^a-z0-9]|$)`)
	reAvailGroup  = regexp.MustCompile(`-\s*([A-Za-z0-9]{2,15})(?:\[[^\]]*\])?\s*$`)
	reAvailTok    = regexp.MustCompile(`[a-z0-9]+`)
)

var sourceSeparators = strings.NewReplacer(" ", "", ".", "", "_", "", "-", "")

// groups that are really the tail of "WEB-DL", "BD-Rip"...
var notAGroup = map[string]bool{"dl": true, "rip": true, "ray": true, "hd": true, "dvd": true, "cam": true}

type releaseTraits struct {
	group, source, res string
}

func traitsOf(release string) releaseTraits {
	var t releaseTraits
	if m := reAvailGroup.FindStringSubmatch(strings.TrimSpace(release)); m != nil {
		if g := strings.ToLower(m[1]); !notAGroup[g] {
			t.group = g
		}
	}
	if m := reAvailSource.FindStringSubmatch(release); m != nil {
		switch s := strings.ToLower(sourceSeparators.Replace(m[1])); {
		case strings.HasPrefix(s, "web") && strings.Contains(s, "rip"):
			t.source = "webrip"
		case strings.HasPrefix(s, "web"):
			t.source = "web"
		case strings.Contains(s, "blu") || s == "bdrip" || s == "brrip" || s == "remux":
			t.source = "bluray"
		default:
			t.source = s
		}
	}
	if m := reAvailRes.FindStringSubmatch(release); m != nil {
		r := strings.ToLower(m[1])
		if r == "4k" {
			r = "2160p"
		}
		t.res = r
	}
	return t
}

func hasToken(s, tok string) bool {
	for _, t := range reAvailTok.FindAllString(strings.ToLower(s), -1) {
		if t == tok {
			return true
		}
	}
	return false
}

// subtitleMatchLevel rates how well the Spanish subtitles (release names)
// fit a torrent: "exact", "similar", "other", or "none" for an empty list.
// It also returns the best subtitle release.
func subtitleMatchLevel(torrent string, subReleases []string) (level, best string) {
	if len(subReleases) == 0 {
		return "none", ""
	}
	tt := traitsOf(torrent)
	level, best = "other", subReleases[0]
	for _, sub := range subReleases {
		if tt.group != "" && hasToken(sub, tt.group) {
			return "exact", sub
		}
		if level != "similar" && tt.source != "" && tt.res != "" {
			st := traitsOf(sub)
			if st.source == tt.source && st.res == tt.res {
				level, best = "similar", sub
			}
		}
	}
	return level, best
}

type subAvailKey struct {
	IMDb    string
	Season  int // 0 = whole title (movie or full series)
	Episode int
}

func (k subAvailKey) String() string {
	return k.IMDb + "|" + strconv.Itoa(k.Season) + "|" + strconv.Itoa(k.Episode)
}

// subAvailKeyFor decides which OpenSubtitles lookup covers a release.
func subAvailKeyFor(imdb, typ, release string) (subAvailKey, bool) {
	if typ != "series" {
		return subAvailKey{IMDb: imdb}, true
	}
	rel, ok := classifySeriesName(release)
	if !ok {
		return subAvailKey{}, false
	}
	switch {
	case !rel.Pack:
		return subAvailKey{imdb, rel.SeasonFrom, rel.Episode}, true
	case rel.SeasonFrom > 0 && rel.SeasonFrom == rel.SeasonTo:
		return subAvailKey{imdb, rel.SeasonFrom, 0}, true
	default: // several seasons or the complete series
		return subAvailKey{IMDb: imdb}, true
	}
}

func subAvailQuery(k subAvailKey, typ string) url.Values {
	id := strings.TrimPrefix(k.IMDb, "tt")
	q := url.Values{}
	q.Set("languages", subAvailLanguages)
	if typ == "series" {
		q.Set("parent_imdb_id", id)
		q.Set("type", "episode")
		if k.Season > 0 {
			q.Set("season_number", strconv.Itoa(k.Season))
		}
		if k.Episode > 0 {
			q.Set("episode_number", strconv.Itoa(k.Episode))
		}
	} else {
		q.Set("imdb_id", id)
		q.Set("type", "movie")
	}
	return q
}

// ---------------------------------------------------------------------------
// Cache and lookup
// ---------------------------------------------------------------------------

func subAvailCacheGet(key string) ([]string, bool) {
	subAvail.Lock()
	defer subAvail.Unlock()
	e, ok := subAvail.m[key]
	if !ok || time.Now().After(e.expires) {
		delete(subAvail.m, key)
		return nil, false
	}
	return e.releases, true
}

func subAvailCachePut(key string, releases []string) {
	ttl := subAvailTTL
	if len(releases) == 0 {
		ttl = subAvailEmptyTTL
	}
	subAvail.Lock()
	defer subAvail.Unlock()
	if len(subAvail.m) >= subAvailCacheMax {
		now := time.Now()
		for k, e := range subAvail.m {
			if now.After(e.expires) {
				delete(subAvail.m, k)
			}
		}
		for k := range subAvail.m { // still full: drop an arbitrary entry
			if len(subAvail.m) < subAvailCacheMax {
				break
			}
			delete(subAvail.m, k)
		}
	}
	subAvail.m[key] = subAvailEntry{releases: releases, expires: time.Now().Add(ttl)}
}

// subAvailFetch returns the Spanish subtitle release names for one lookup,
// from cache or OpenSubtitles (at most subAvailConcurrency at a time).
func subAvailFetch(ctx context.Context, cfg subtitleConfig, k subAvailKey, typ string) ([]string, error) {
	cacheKey := typ + "|" + k.String()
	if rel, ok := subAvailCacheGet(cacheKey); ok {
		return rel, nil
	}
	select {
	case subAvail.sem <- struct{}{}:
		defer func() { <-subAvail.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// another request may have filled it while we waited
	if rel, ok := subAvailCacheGet(cacheKey); ok {
		return rel, nil
	}
	results, err := osSearchOnce(cfg, subAvailQuery(k, typ))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	releases := []string{}
	for _, r := range results {
		name := strings.TrimSpace(r.Attributes.Release)
		if name == "" && len(r.Attributes.Files) > 0 {
			name = strings.TrimSpace(r.Attributes.Files[0].FileName)
		}
		if name != "" && !seen[name] {
			seen[name] = true
			releases = append(releases, name)
		}
	}
	subAvailCachePut(cacheKey, releases)
	return releases, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

type subAvailRequest struct {
	IMDb     string   `json:"imdb"`
	Type     string   `json:"type"` // movie | series
	Releases []string `json:"releases"`
}

type subAvailResult struct {
	Level string `json:"level"`
	Sub   string `json:"sub,omitempty"`
}

// subtitleAvailabilityHandler: POST /api/v1/subtitles/availability
// {imdb, type, releases[]} -> {enabled, results: {release: {level, sub}}}
// Releases that can't be checked (unknown episode, lookup failed) are left
// out so the page shows nothing for them.
func subtitleAvailabilityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := snapshotSubtitleConfig()
	if !cfg.Enabled || cfg.APIKey == "" {
		respondWithJSON(w, http.StatusOK, map[string]interface{}{"enabled": false, "results": map[string]subAvailResult{}})
		return
	}
	var in subAvailRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil || !imdbIDRe.MatchString(in.IMDb) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}
	if in.Type != "series" {
		in.Type = "movie"
	}
	if len(in.Releases) > subAvailMaxReleases {
		in.Releases = in.Releases[:subAvailMaxReleases]
	}

	byKey := map[subAvailKey][]string{}
	var keys []subAvailKey
	for _, rel := range in.Releases {
		if len(rel) > 300 {
			continue
		}
		k, ok := subAvailKeyFor(in.IMDb, in.Type, rel)
		if !ok {
			continue
		}
		if _, seen := byKey[k]; !seen {
			if len(keys) >= subAvailMaxKeys {
				continue
			}
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], rel)
	}

	ctx, cancel := context.WithTimeout(r.Context(), subAvailBudget)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	subs := map[subAvailKey][]string{}
	for _, k := range keys {
		wg.Add(1)
		go func(k subAvailKey) {
			defer wg.Done()
			rel, err := subAvailFetch(ctx, cfg, k, in.Type)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("Subtitle availability: %s: %v", k, err)
				}
				return
			}
			mu.Lock()
			subs[k] = rel
			mu.Unlock()
		}(k)
	}
	wg.Wait()

	results := map[string]subAvailResult{}
	for _, k := range keys {
		rels, ok := subs[k]
		if !ok {
			continue
		}
		for _, torrent := range byKey[k] {
			level, best := subtitleMatchLevel(torrent, rels)
			results[torrent] = subAvailResult{Level: level, Sub: best}
		}
	}
	respondWithJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "results": results})
}
