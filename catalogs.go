package main

// Streaming catalogs ("what's new on Netflix, HBO Max..."), from the public
// Stremio addon "Streaming Catalogs". Verified 2026-11-04:
//   GET <base>/catalog/<movie|series>/<service>.json
//       -> {"metas":[{"imdb_id","type","name","year","releaseInfo","poster","imdbRating","genres"}]}
//   services: nfx Netflix, hbm HBO Max, dnp Disney+, amp Prime Video, atp Apple TV+
//   (plus Netflix Top 10 by country with a configured URL, see streamingServices)
// Some text fields come double-encoded (UTF-8 read as Windows-1252, e.g.
// "2022â€“"); fixMojibake repairs them.
//
// The answer has the same shape as /api/v1/featured (groups with meta), but
// without torrents: the client searches them when a title is opened.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

const (
	streamingCatalogBase = "https://7a82163c306e-stremio-netflix-catalog-addon.baby-beamup.club"
	streamingCacheTTL    = 6 * time.Hour
	streamingMaxTitles   = 60
	streamingTimeout     = 15 * time.Second
)

type streamingService struct {
	Label   string
	Catalog string // catalog id in the addon
	Config  string // addon config, "" = default
}

// The addon's own "nfx" catalog stopped updating (newest titles from late
// 2024), so the Netflix Top 10 for Mexico is offered too. It needs a
// configured addon URL: base64("<providers>:<rpdbKey>:<country>:<timestamp>:
// <top10Global>:<top10Country>:<top10CountryCode>"), see the /configure page.
var streamingServices = map[string]streamingService{
	"nfx":     {Label: "Netflix", Catalog: "nfx"},
	"nfx-top": {Label: "Netflix Top 10 (MX)", Catalog: "netflix-top10-MX", Config: "nfx::MX:1762000000000:0:1:MX"},
	"hbm":     {Label: "HBO Max", Catalog: "hbm"},
	"dnp":     {Label: "Disney+", Catalog: "dnp"},
	"amp":     {Label: "Prime Video", Catalog: "amp"},
	"atp":     {Label: "Apple TV+", Catalog: "atp"},
}

func (s streamingService) catalogURL(typ string) string {
	u := streamingCatalogBase
	if s.Config != "" {
		u += "/" + url.QueryEscape(base64.StdEncoding.EncodeToString([]byte(s.Config)))
	}
	return u + "/catalog/" + typ + "/" + s.Catalog + ".json"
}

// fixMojibake undoes UTF-8 text that was decoded as Windows-1252. Strings
// that don't look like that (or don't round-trip to valid UTF-8) are kept.
func fixMojibake(s string) string {
	if !strings.ContainsAny(s, "ÃÂâÅÄ") {
		return s
	}
	b, err := charmap.Windows1252.NewEncoder().Bytes([]byte(s))
	if err != nil || !utf8.Valid(b) {
		return s
	}
	return string(b)
}

func parseStreamingCatalog(body []byte, typ string) ([]resultGroup, error) {
	var data struct {
		Metas []cinemetaItem `json:"metas"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	groups := make([]resultGroup, 0, streamingMaxTitles)
	seen := map[string]bool{}
	for _, item := range data.Metas {
		item.Name = fixMojibake(item.Name)
		item.Year = flexString(fixMojibake(string(item.Year)))
		item.ReleaseInfo = flexString(fixMojibake(string(item.ReleaseInfo)))
		if item.Type == "" {
			item.Type = typ
		}
		m := item.toMeta()
		if m == nil || m.Poster == "" || seen[m.IMDb] {
			continue
		}
		seen[m.IMDb] = true
		groups = append(groups, resultGroup{Key: m.IMDb, Meta: m, Results: []searchResult{}})
		if len(groups) == streamingMaxTitles {
			break
		}
	}
	return groups, nil
}

var streamingCache = struct {
	sync.Mutex
	m map[string]featuredCacheEntry
}{m: map[string]featuredCacheEntry{}}

func loadStreamingCatalog(ctx context.Context, service string, svc streamingService, typ string) ([]resultGroup, error) {
	key := service + "/" + typ
	streamingCache.Lock()
	if e, ok := streamingCache.m[key]; ok && time.Now().Before(e.expires) {
		streamingCache.Unlock()
		return e.groups, nil
	}
	streamingCache.Unlock()

	ctx, cancel := context.WithTimeout(ctx, streamingTimeout)
	defer cancel()
	body, err := fetchBody(ctx, createSelectiveProxyClient(), svc.catalogURL(typ))
	if err != nil {
		return nil, err
	}
	groups, err := parseStreamingCatalog(body, typ)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, errors.New("empty catalog")
	}

	streamingCache.Lock()
	streamingCache.m[key] = featuredCacheEntry{groups: groups, expires: time.Now().Add(streamingCacheTTL)}
	streamingCache.Unlock()
	return groups, nil
}

// streamingCatalogHandler: GET /api/v1/catalog?service=nfx|hbm|dnp|amp|atp&type=movie|series
func streamingCatalogHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	service := r.URL.Query().Get("service")
	svc, ok := streamingServices[service]
	if !ok {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Unknown service"})
		return
	}
	typ := r.URL.Query().Get("type")
	if typ != "series" {
		typ = "movie"
	}
	groups, err := loadStreamingCatalog(r.Context(), service, svc, typ)
	if err != nil {
		log.Printf("Catalog: %s/%s failed: %v", service, typ, err)
		respondWithJSON(w, http.StatusBadGateway, map[string]string{"error": svc.Label + " is not available right now"})
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]interface{}{"service": service, "label": svc.Label, "type": typ, "groups": groups})
}
