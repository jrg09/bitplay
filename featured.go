package main

// "Featured" lists for the home page, grouped by title like search results
// (metadata.go). Sources verified 2026-11-04:
//   popular     YTS  list_movies.json?sort_by=download_count
//   recent      YTS  list_movies.json?sort_by=date_added
//   top-movies  TPB  https://apibay.org/precompiled/data_top100_207.json (HD movies)
//   top-tv      TPB  https://apibay.org/precompiled/data_top100_208.json (HD TV)
// Lists are cached for featuredCacheTTL; the source order (popularity, date
// or seeders) is kept.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	featuredCacheTTL  = 30 * time.Minute
	featuredMaxTitles = 30
	featuredTimeout   = 15 * time.Second
)

type featuredList struct {
	ID    string
	Label string
	fetch func(ctx context.Context, client *http.Client) ([]searchResult, error)
}

func ytsList(sortBy string) func(ctx context.Context, client *http.Client) ([]searchResult, error) {
	return func(ctx context.Context, client *http.Client) ([]searchResult, error) {
		var lastErr error
		for _, host := range ytsHosts {
			if ctx.Err() != nil {
				break
			}
			u := fmt.Sprintf("https://%s/api/v2/list_movies.json?limit=%d&sort_by=%s", host, featuredMaxTitles, sortBy)
			body, err := fetchBody(ctx, client, u)
			if err == nil {
				var res []searchResult
				if res, err = parseYTS(body); err == nil {
					return res, nil
				}
			}
			lastErr = fmt.Errorf("%s: %w", host, err)
		}
		if lastErr == nil {
			lastErr = ctx.Err()
		}
		return nil, lastErr
	}
}

func tpbTop(category string) func(ctx context.Context, client *http.Client) ([]searchResult, error) {
	return func(ctx context.Context, client *http.Client) ([]searchResult, error) {
		body, err := fetchBody(ctx, client, "https://apibay.org/precompiled/data_top100_"+category+".json")
		if err != nil {
			return nil, err
		}
		return parseTPB(body, "all")
	}
}

var featuredLists = []featuredList{
	{ID: "popular", Label: "Popular", fetch: ytsList("download_count")},
	{ID: "recent", Label: "Recently added", fetch: ytsList("date_added")},
	{ID: "top-movies", Label: "Top movies", fetch: tpbTop("207")},
	{ID: "top-tv", Label: "Top TV", fetch: tpbTop("208")},
}

type featuredCacheEntry struct {
	groups  []resultGroup
	expires time.Time
}

var featuredCache = struct {
	sync.Mutex
	m map[string]featuredCacheEntry
}{m: map[string]featuredCacheEntry{}}

// featuredGroups keeps only identified titles (a tile needs a poster and a
// name), at most featuredMaxTitles.
func featuredGroups(groups []resultGroup) []resultGroup {
	out := make([]resultGroup, 0, featuredMaxTitles)
	for _, g := range groups {
		if g.Meta == nil {
			continue
		}
		out = append(out, g)
		if len(out) == featuredMaxTitles {
			break
		}
	}
	return out
}

func loadFeatured(ctx context.Context, list featuredList) ([]resultGroup, error) {
	featuredCache.Lock()
	if e, ok := featuredCache.m[list.ID]; ok && time.Now().Before(e.expires) {
		featuredCache.Unlock()
		return e.groups, nil
	}
	featuredCache.Unlock()

	ctx, cancel := context.WithTimeout(ctx, featuredTimeout)
	defer cancel()
	results, err := list.fetch(ctx, createSelectiveProxyClient())
	if err != nil {
		return nil, err
	}
	groups := featuredGroups(enrichResults(ctx, results))
	if len(groups) == 0 {
		return nil, errors.New("no titles could be identified")
	}

	featuredCache.Lock()
	featuredCache.m[list.ID] = featuredCacheEntry{groups: groups, expires: time.Now().Add(featuredCacheTTL)}
	featuredCache.Unlock()
	return groups, nil
}

// featuredHandler: GET /api/v1/featured?list=popular|recent|top-movies|top-tv
func featuredHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("list")
	if id == "" {
		id = featuredLists[0].ID
	}
	for _, list := range featuredLists {
		if list.ID != id {
			continue
		}
		groups, err := loadFeatured(r.Context(), list)
		if err != nil {
			log.Printf("Featured: %s failed: %v", list.ID, err)
			respondWithJSON(w, http.StatusBadGateway, map[string]string{"error": list.Label + " is not available right now"})
			return
		}
		respondWithJSON(w, http.StatusOK, map[string]interface{}{"list": list.ID, "label": list.Label, "groups": groups})
		return
	}
	respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Unknown list"})
}
