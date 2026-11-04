package main

// Built-in torrent search over public sources, modelled on
// github.com/baairon/torlink (MIT): every enabled source is queried in
// parallel, results are merged, de-duplicated by info hash (the copy with
// more seeders wins) and sorted by seeders. Prowlarr and Jackett, when
// configured, are queried through their existing handlers and merged too.
//
// Sources (endpoints verified 2026-11-04):
//   - YTS     GET https://<host>/api/v2/list_movies.json?limit=50&query_term=<q>
//             (yts.gg is the live host; yts.mx/yts.am are kept as fallbacks)
//   - TPB     GET https://apibay.org/q.php?q=<q>   (video categories only)
//   - EZTV    GET https://eztvx.to/api/get-torrents?limit=100&page=N[&imdb_id=N]
//             EZTV has no text search: recent pages are indexed and matched
//             by name, and shows are then fetched by IMDb id (ids also come
//             from TV results on TPB, which carry an "imdb" field).

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	publicSearchMaxResults = 200
	publicSearchMaxBody    = 20 * 1024 * 1024
	publicSearchUserAgent  = "BitPlay"
	eztvIndexPages         = 10
	eztvIndexTTL           = 5 * time.Minute
	eztvMaxShows           = 3

	// Connections to these sites occasionally stall (seen with both Go and
	// wget), so every request and every source gets its own deadline: one
	// stuck request must not hold the whole search until searchTimeout().
	publicRequestTimeout   = 10 * time.Second
	eztvIndexPageTimeout   = 6 * time.Second
	publicSourceMaxRuntime = 15 * time.Second
)

// Same default tracker list torlink appends to magnets it builds.
var publicSearchTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.dler.org:6969/announce",
	"http://tracker.opentrackr.org:1337/announce",
	"http://tracker.openbittorrent.com:80/announce",
	"http://tracker.dler.org:6969/announce",
	"https://tracker.tamersunion.org:443/announce",
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type searchResult struct {
	Title        string `json:"title"`
	MagnetURL    string `json:"magnetUrl,omitempty"`
	DownloadURL  string `json:"downloadUrl,omitempty"`
	DirectMagnet bool   `json:"directMagnet"`
	InfoHash     string `json:"infoHash,omitempty"`
	Size         string `json:"size"`
	SizeBytes    int64  `json:"sizeBytes"`
	Seeders      int64  `json:"seeders"`
	Leechers     int64  `json:"leechers"`
	Indexer      string `json:"indexer"`
	Category     string `json:"category,omitempty"` // "movies" | "tv" | ""
	Added        int64  `json:"added,omitempty"`    // unix seconds

	imdb string // only used to cross-reference TPB -> EZTV
}

type sourceStatus struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	OK    bool   `json:"ok"`
	Count int    `json:"count"`
	Error string `json:"error,omitempty"`
}

type publicSource struct {
	ID         string
	Label      string
	Categories []string
}

var publicSources = []publicSource{
	{ID: "yts", Label: "YTS", Categories: []string{"movies"}},
	{ID: "tpb", Label: "The Pirate Bay", Categories: []string{"movies", "tv"}},
	{ID: "eztv", Label: "EZTV", Categories: []string{"tv"}},
}

func (s publicSource) supports(category string) bool {
	return category == "all" || slices.Contains(s.Categories, category)
}

// flexInt decodes numbers that APIs send either as JSON numbers or strings.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	*f = 0
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(n)
	} else if fl, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexInt(fl)
	}
	return nil
}

// flexString decodes a value sent either as a JSON string or a number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	raw := strings.TrimSpace(string(b))
	if raw == "null" {
		raw = ""
	}
	*f = flexString(raw)
	return nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

type publicSearchSettings struct {
	EnablePublicSearch bool     `json:"enablePublicSearch"`
	Sources            []string `json:"sources"`
}

func enabledPublicSources() []publicSource {
	settingsMutex.RLock()
	defer settingsMutex.RUnlock()
	if currentSettings.DisablePublicSearch {
		return nil
	}
	var out []publicSource
	for _, s := range publicSources {
		if !slices.Contains(currentSettings.DisabledSearchSources, s.ID) {
			out = append(out, s)
		}
	}
	return out
}

func savePublicSearchSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in publicSearchSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	disabled := []string{}
	for _, s := range publicSources {
		if !slices.Contains(in.Sources, s.ID) {
			disabled = append(disabled, s.ID)
		}
	}

	settingsMutex.Lock()
	currentSettings.DisablePublicSearch = !in.EnablePublicSearch
	currentSettings.DisabledSearchSources = disabled
	settingsMutex.Unlock()

	if err := saveSettingsToFile(); err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to save settings: " + err.Error()})
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Search settings saved successfully"})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var (
	hexHashRe    = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	base32HashRe = regexp.MustCompile(`^[A-Za-z2-7]{32}$`)
	magnetHashRe = regexp.MustCompile(`(?i)xt=urn:btih:([a-z0-9]{32,40})`)
	zeroHash     = strings.Repeat("0", 40)
)

// normalizeInfoHash returns a lowercase hex info hash, or "" if invalid.
func normalizeInfoHash(h string) string {
	h = strings.TrimSpace(h)
	switch {
	case hexHashRe.MatchString(h):
		h = strings.ToLower(h)
		if h == zeroHash {
			return ""
		}
		return h
	case base32HashRe.MatchString(h):
		raw, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
		if err != nil {
			return ""
		}
		return hex.EncodeToString(raw)
	}
	return ""
}

func infoHashFromMagnet(magnet string) string {
	if m := magnetHashRe.FindStringSubmatch(magnet); m != nil {
		return normalizeInfoHash(m[1])
	}
	return ""
}

func buildMagnet(infoHash, name string) string {
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(infoHash)
	b.WriteString("&dn=")
	b.WriteString(url.QueryEscape(name))
	for _, tr := range publicSearchTrackers {
		b.WriteString("&tr=")
		b.WriteString(url.QueryEscape(tr))
	}
	return b.String()
}

var humanSizeRe = regexp.MustCompile(`(?i)^\s*([\d.]+)\s*(B|KB|MB|GB|TB)\s*$`)

// parseHumanSize reverses formatSize ("1.85 GB" -> bytes) so Prowlarr and
// Jackett results can be sorted by size together with the others.
func parseHumanSize(s string) int64 {
	m := humanSizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	exp := map[string]int{"B": 0, "KB": 1, "MB": 2, "GB": 3, "TB": 4}[strings.ToUpper(m[2])]
	for i := 0; i < exp; i++ {
		n *= 1024
	}
	return int64(n)
}

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeText(s string) string {
	return strings.TrimSpace(nonAlnumRe.ReplaceAllString(strings.ToLower(s), " "))
}

func queryTokens(q string) []string {
	return strings.Fields(normalizeText(q))
}

// matchesAllTokens reports whether every query token appears in the text.
func matchesAllTokens(text string, tokens []string) bool {
	text = " " + normalizeText(text) + " "
	for _, t := range tokens {
		if !strings.Contains(text, t) {
			return false
		}
	}
	return true
}

func fetchBody(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	return fetchBodyTimeout(ctx, client, rawURL, publicRequestTimeout)
}

func fetchBodyTimeout(ctx context.Context, client *http.Client, rawURL string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", publicSearchUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, publicSearchMaxBody))
}

// mergeResults de-duplicates by info hash (more seeders wins) and sorts by
// seeders, then by date, both descending.
func mergeResults(lists ...[]searchResult) []searchResult {
	var out []searchResult
	byHash := map[string]int{}
	for _, list := range lists {
		for _, r := range list {
			if r.InfoHash == "" {
				out = append(out, r)
				continue
			}
			if i, ok := byHash[r.InfoHash]; ok {
				if r.Seeders > out[i].Seeders {
					out[i] = r
				}
				continue
			}
			byHash[r.InfoHash] = len(out)
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Seeders != out[j].Seeders {
			return out[i].Seeders > out[j].Seeders
		}
		return out[i].Added > out[j].Added
	})
	if len(out) > publicSearchMaxResults {
		out = out[:publicSearchMaxResults]
	}
	return out
}

// ---------------------------------------------------------------------------
// YTS
// ---------------------------------------------------------------------------

var ytsHosts = []string{"yts.gg", "yts.mx", "yts.am"}

type ytsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Movies []struct {
			Title     string `json:"title"`
			TitleLong string `json:"title_long"`
			ImdbCode  string `json:"imdb_code"`
			Torrents  []struct {
				Hash             string  `json:"hash"`
				Quality          string  `json:"quality"`
				Type             string  `json:"type"`
				Seeds            flexInt `json:"seeds"`
				Peers            flexInt `json:"peers"`
				SizeBytes        flexInt `json:"size_bytes"`
				DateUploadedUnix flexInt `json:"date_uploaded_unix"`
			} `json:"torrents"`
		} `json:"movies"`
	} `json:"data"`
}

func parseYTS(body []byte) ([]searchResult, error) {
	var resp ytsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if resp.Status != "" && resp.Status != "ok" {
		return nil, fmt.Errorf("status %q", resp.Status)
	}
	var out []searchResult
	for _, m := range resp.Data.Movies {
		name := m.TitleLong
		if name == "" {
			name = m.Title
		}
		if name == "" {
			name = "Unknown"
		}
		for _, t := range m.Torrents {
			hash := normalizeInfoHash(t.Hash)
			if hash == "" {
				continue
			}
			label := strings.TrimSpace(t.Quality + " " + t.Type)
			title := name
			if label != "" {
				title = fmt.Sprintf("%s [%s]", name, label)
			}
			out = append(out, searchResult{
				Title:        title,
				MagnetURL:    buildMagnet(hash, title),
				DirectMagnet: true,
				InfoHash:     hash,
				Size:         formatSize(float64(t.SizeBytes)),
				SizeBytes:    int64(t.SizeBytes),
				Seeders:      int64(t.Seeds),
				Leechers:     int64(t.Peers),
				Indexer:      "YTS",
				Category:     "movies",
				Added:        int64(t.DateUploadedUnix),
				imdb:         m.ImdbCode,
			})
		}
	}
	return out, nil
}

func searchYTS(ctx context.Context, client *http.Client, query string) ([]searchResult, error) {
	var lastErr error
	for _, host := range ytsHosts {
		if ctx.Err() != nil {
			break
		}
		u := "https://" + host + "/api/v2/list_movies.json?limit=50&query_term=" + url.QueryEscape(query)
		body, err := fetchBody(ctx, client, u)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", host, err)
			continue
		}
		results, err := parseYTS(body)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", host, err)
			continue
		}
		return results, nil
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return nil, lastErr
}

// ---------------------------------------------------------------------------
// The Pirate Bay (apibay.org)
// ---------------------------------------------------------------------------

// TPB video categories: 201 Movies, 202 DVDR, 207 HD, 209 3D, 210 CAM/TS,
// 211 UHD/4k; 205 TV, 208 HD TV, 212 UHD/4k TV.
var (
	tpbMovieCats = []int64{201, 202, 207, 209, 210, 211}
	tpbTVCats    = []int64{205, 208, 212}
)

type tpbRow struct {
	ID       flexString `json:"id"`
	Name     string     `json:"name"`
	InfoHash string     `json:"info_hash"`
	Leechers flexInt    `json:"leechers"`
	Seeders  flexInt    `json:"seeders"`
	Size     flexInt    `json:"size"`
	Added    flexInt    `json:"added"`
	Category flexInt    `json:"category"`
	Imdb     string     `json:"imdb"`
}

func parseTPB(body []byte, category string) ([]searchResult, error) {
	var rows []tpbRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var out []searchResult
	for _, row := range rows {
		hash := normalizeInfoHash(row.InfoHash)
		if row.ID == "0" || hash == "" {
			continue // "No results returned" placeholder
		}
		cat := ""
		switch {
		case slices.Contains(tpbMovieCats, int64(row.Category)):
			cat = "movies"
		case slices.Contains(tpbTVCats, int64(row.Category)):
			cat = "tv"
		default:
			continue // not a video category
		}
		if category != "all" && category != cat {
			continue
		}
		name := row.Name
		if name == "" {
			name = "Unknown"
		}
		out = append(out, searchResult{
			Title:        name,
			MagnetURL:    buildMagnet(hash, name),
			DirectMagnet: true,
			InfoHash:     hash,
			Size:         formatSize(float64(row.Size)),
			SizeBytes:    int64(row.Size),
			Seeders:      int64(row.Seeders),
			Leechers:     int64(row.Leechers),
			Indexer:      "The Pirate Bay",
			Category:     cat,
			Added:        int64(row.Added),
			imdb:         row.Imdb,
		})
	}
	return out, nil
}

func searchTPB(ctx context.Context, client *http.Client, query, category string) ([]searchResult, error) {
	body, err := fetchBody(ctx, client, "https://apibay.org/q.php?q="+url.QueryEscape(query))
	if err != nil {
		return nil, err
	}
	return parseTPB(body, category)
}

// ---------------------------------------------------------------------------
// EZTV
// ---------------------------------------------------------------------------

const eztvAPI = "https://eztvx.to/api/get-torrents"

type eztvTorrent struct {
	Hash             string     `json:"hash"`
	Filename         string     `json:"filename"`
	Title            string     `json:"title"`
	MagnetURL        string     `json:"magnet_url"`
	Seeds            flexInt    `json:"seeds"`
	Peers            flexInt    `json:"peers"`
	SizeBytes        flexInt    `json:"size_bytes"`
	DateReleasedUnix flexInt    `json:"date_released_unix"`
	ImdbID           flexString `json:"imdb_id"`
}

type eztvResponse struct {
	Torrents []eztvTorrent `json:"torrents"`
}

func parseEZTV(body []byte) ([]eztvTorrent, error) {
	var resp eztvResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return resp.Torrents, nil
}

func (t eztvTorrent) toResult() (searchResult, bool) {
	hash := normalizeInfoHash(t.Hash)
	if hash == "" {
		hash = infoHashFromMagnet(t.MagnetURL)
	}
	if hash == "" {
		return searchResult{}, false
	}
	name := t.Title
	if name == "" {
		name = t.Filename
	}
	if name == "" {
		name = hash
	}
	magnet := t.MagnetURL
	if !strings.HasPrefix(magnet, "magnet:") {
		magnet = buildMagnet(hash, name)
	}
	return searchResult{
		Title:        name,
		MagnetURL:    magnet,
		DirectMagnet: true,
		InfoHash:     hash,
		Size:         formatSize(float64(t.SizeBytes)),
		SizeBytes:    int64(t.SizeBytes),
		Seeders:      int64(t.Seeds),
		Leechers:     int64(t.Peers),
		Indexer:      "EZTV",
		Category:     "tv",
		Added:        int64(t.DateReleasedUnix),
	}, true
}

var eztvIndex struct {
	sync.Mutex
	rows    []eztvTorrent
	fetched time.Time
}

// eztvLoadIndex returns the most recent EZTV torrents (cached for 5 min).
func eztvLoadIndex(ctx context.Context, client *http.Client) []eztvTorrent {
	eztvIndex.Lock()
	defer eztvIndex.Unlock()
	if len(eztvIndex.rows) > 0 && time.Since(eztvIndex.fetched) < eztvIndexTTL {
		return eztvIndex.rows
	}

	pages := make([][]eztvTorrent, eztvIndexPages)
	var wg sync.WaitGroup
	for i := range pages {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, err := fetchBodyTimeout(ctx, client, fmt.Sprintf("%s?limit=100&page=%d", eztvAPI, i+1), eztvIndexPageTimeout)
			if err != nil {
				return // missing pages only make the index smaller
			}
			pages[i], _ = parseEZTV(body)
		}(i)
	}
	wg.Wait()

	var rows []eztvTorrent
	complete := true
	for _, p := range pages {
		complete = complete && len(p) > 0
		rows = append(rows, p...)
	}
	// only cache a complete index, otherwise retry on the next search
	if complete {
		eztvIndex.rows, eztvIndex.fetched = rows, time.Now()
	}
	return rows
}

func eztvImdbID(id string) string {
	id = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "tt")
	if id == "" || strings.Trim(id, "0") == "" {
		return ""
	}
	if _, err := strconv.Atoi(id); err != nil {
		return ""
	}
	return id
}

// searchEZTV matches the query against recent torrents, then fetches every
// episode of up to eztvMaxShows shows by IMDb id. imdbHints may block until
// other sources (TPB) have answered.
func searchEZTV(ctx context.Context, client *http.Client, query string, imdbHints func() []string) ([]searchResult, error) {
	tokens := queryTokens(query)
	if len(tokens) == 0 {
		return nil, nil
	}

	indexCh := make(chan []eztvTorrent, 1)
	go func() { indexCh <- eztvLoadIndex(ctx, client) }()

	var hints []string
	if imdbHints != nil {
		hints = imdbHints()
	}
	var index []eztvTorrent
	select {
	case index = <-indexCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var matches []eztvTorrent
	var ids []string
	addID := func(id string) {
		if id = eztvImdbID(id); id != "" && !slices.Contains(ids, id) && len(ids) < eztvMaxShows {
			ids = append(ids, id)
		}
	}
	for _, t := range index {
		if matchesAllTokens(t.Title+" "+t.Filename, tokens) {
			matches = append(matches, t)
			addID(string(t.ImdbID))
		}
	}
	for _, h := range hints {
		addID(h)
	}

	shows := make([][]eztvTorrent, len(ids))
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			body, err := fetchBody(ctx, client, fmt.Sprintf("%s?limit=100&page=1&imdb_id=%s", eztvAPI, id))
			if err == nil {
				shows[i], err = parseEZTV(body)
			}
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(i, id)
	}
	wg.Wait()

	var results []searchResult
	for _, list := range append(shows, matches) {
		for _, t := range list {
			if !matchesAllTokens(t.Title+" "+t.Filename, tokens) {
				continue
			}
			if r, ok := t.toResult(); ok {
				results = append(results, r)
			}
		}
	}
	if len(index) == 0 && len(results) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, errors.New("EZTV is not responding")
	}
	return mergeResults(results), nil
}

// ---------------------------------------------------------------------------
// Prowlarr / Jackett (through their existing handlers)
// ---------------------------------------------------------------------------

func searchViaHandler(ctx context.Context, handler http.HandlerFunc, label, query string) ([]searchResult, error) {
	req := httptest.NewRequest(http.MethodPost, "/?q="+url.QueryEscape(query), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	body := rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("%s returned HTTP %d", label, rec.Code)
	}

	var rows []struct {
		Title        string  `json:"title"`
		MagnetURL    string  `json:"magnetUrl"`
		DownloadURL  string  `json:"downloadUrl"`
		DirectMagnet bool    `json:"directMagnet"`
		Size         string  `json:"size"`
		Seeders      float64 `json:"seeders"`
		Leechers     float64 `json:"leechers"`
		Indexer      string  `json:"indexer"`
		PublishDate  string  `json:"publishDate"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("invalid %s response: %w", label, err)
	}
	out := make([]searchResult, 0, len(rows))
	for _, row := range rows {
		indexer := label
		if row.Indexer != "" {
			indexer = row.Indexer + " (" + label + ")"
		}
		r := searchResult{
			Title:        row.Title,
			MagnetURL:    row.MagnetURL,
			DownloadURL:  row.DownloadURL,
			DirectMagnet: row.DirectMagnet,
			InfoHash:     infoHashFromMagnet(row.MagnetURL),
			Size:         row.Size,
			SizeBytes:    parseHumanSize(row.Size),
			Seeders:      int64(row.Seeders),
			Leechers:     int64(row.Leechers),
			Indexer:      indexer,
		}
		if t, err := time.Parse(time.RFC3339, row.PublishDate); err == nil {
			r.Added = t.Unix()
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Orchestration
// ---------------------------------------------------------------------------

type searchJob struct {
	id, label string
	run       func(ctx context.Context) ([]searchResult, error)
}

func runTorrentSearch(ctx context.Context, query, category string) ([]searchResult, []sourceStatus) {
	client := createSelectiveProxyClient() // honours the SOCKS5 proxy setting

	var jobs []searchJob
	sources := enabledPublicSources()
	enabled := func(id string) bool {
		return slices.ContainsFunc(sources, func(s publicSource) bool { return s.ID == id && s.supports(category) })
	}

	// TPB TV results feed IMDb ids to EZTV
	tpbDone := make(chan struct{})
	var tpbResults []searchResult
	tpbRuns := enabled("tpb")
	if !tpbRuns {
		close(tpbDone)
	}

	if enabled("yts") {
		jobs = append(jobs, searchJob{"yts", "YTS", func(ctx context.Context) ([]searchResult, error) {
			return searchYTS(ctx, client, query)
		}})
	}
	if tpbRuns {
		jobs = append(jobs, searchJob{"tpb", "The Pirate Bay", func(ctx context.Context) ([]searchResult, error) {
			defer close(tpbDone)
			res, err := searchTPB(ctx, client, query, category)
			tpbResults = res
			return res, err
		}})
	}
	if enabled("eztv") {
		hints := func() []string {
			select {
			case <-tpbDone:
			case <-ctx.Done():
				return nil
			}
			var ids []string
			for _, r := range tpbResults {
				if r.Category == "tv" && r.imdb != "" && !slices.Contains(ids, r.imdb) {
					ids = append(ids, r.imdb)
				}
			}
			return ids
		}
		jobs = append(jobs, searchJob{"eztv", "EZTV", func(ctx context.Context) ([]searchResult, error) {
			return searchEZTV(ctx, client, query, hints)
		}})
	}

	settingsMutex.RLock()
	useProwlarr := currentSettings.EnableProwlarr && currentSettings.ProwlarrHost != "" && currentSettings.ProwlarrApiKey != ""
	useJackett := currentSettings.EnableJackett && currentSettings.JackettHost != "" && currentSettings.JackettApiKey != ""
	settingsMutex.RUnlock()
	if useProwlarr {
		jobs = append(jobs, searchJob{"prowlarr", "Prowlarr", func(ctx context.Context) ([]searchResult, error) {
			return searchViaHandler(ctx, searchFromProwlarr, "Prowlarr", query)
		}})
	}
	if useJackett {
		jobs = append(jobs, searchJob{"jackett", "Jackett", func(ctx context.Context) ([]searchResult, error) {
			return searchViaHandler(ctx, searchFromJackett, "Jackett", query)
		}})
	}

	type outcome struct {
		idx     int
		results []searchResult
		err     error
	}
	statuses := make([]sourceStatus, len(jobs))
	ch := make(chan outcome, len(jobs))
	for i, job := range jobs {
		statuses[i] = sourceStatus{ID: job.id, Label: job.label, Error: "timed out"}
		go func(i int, job searchJob) {
			defer func() {
				if p := recover(); p != nil {
					ch <- outcome{idx: i, err: fmt.Errorf("internal error: %v", p)}
				}
			}()
			jobCtx := ctx
			if job.id != "prowlarr" && job.id != "jackett" {
				var cancel context.CancelFunc
				jobCtx, cancel = context.WithTimeout(ctx, publicSourceMaxRuntime)
				defer cancel()
			}
			res, err := job.run(jobCtx)
			ch <- outcome{idx: i, results: res, err: err}
		}(i, job)
	}

	lists := make([][]searchResult, len(jobs))
	deadline := ctx.Done()
	var grace <-chan time.Time
	for received := 0; received < len(jobs); {
		select {
		case o := <-ch:
			received++
			lists[o.idx] = o.results
			st := &statuses[o.idx]
			st.Count = len(o.results)
			if o.err != nil {
				st.Error = o.err.Error()
				if errors.Is(o.err, context.DeadlineExceeded) {
					st.Error = "timed out"
				}
				log.Printf("Search: %s failed: %v", st.Label, o.err)
			} else {
				st.OK, st.Error = true, ""
			}
		case <-deadline:
			deadline = nil
			grace = time.After(time.Second) // let cancelled requests report
		case <-grace:
			received = len(jobs)
		}
	}

	return mergeResults(lists...), statuses
}

// torrentSearchHandler: GET /api/v1/search?q=<query>&category=all|movies|tv
func torrentSearchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "No search query provided"})
		return
	}
	category := r.URL.Query().Get("category")
	if category != "movies" && category != "tv" {
		category = "all"
	}

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout())
	defer cancel()
	results, statuses := runTorrentSearch(ctx, query, category)
	if len(statuses) == 0 {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "No search source is enabled. Enable one in Settings → Search."})
		return
	}
	if results == nil {
		results = []searchResult{}
	}
	log.Printf("Search: %q (%s) -> %d result(s)", query, category, len(results))
	respondWithJSON(w, http.StatusOK, map[string]interface{}{
		"query":    query,
		"category": category,
		"results":  results,
		"sources":  statuses,
	})
}
