package main

// Automatic subtitle lookup through the OpenSubtitles REST API (v1).
//
// Flow:
//   1. The client calls GET /api/v1/subtitles/auto?session=<id>&file=<index>
//      once playback starts. The server parses the video file name, tries to
//      compute the OpenSubtitles hash of the (streamed) file, searches the API
//      and returns the best candidate per configured language.
//   2. The client attaches each candidate as a remote text track pointing at
//      GET /api/v1/subtitles/file/<file_id>.vtt. The subtitle is only
//      downloaded (and counted against the OpenSubtitles quota) when the
//      browser actually loads that track, and it is cached in memory.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	osDefaultHost    = "api.opensubtitles.com"
	osUserAgent      = "BitPlay v1.0"
	osHashTimeout    = 8 * time.Second
	osMaxSubtitleLen = 10 * 1024 * 1024
	osCacheEntries   = 40
)

var subtitleHTTPClient = &http.Client{Timeout: 25 * time.Second}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// SubtitleSettings is the payload of the subtitle settings form.
type SubtitleSettings struct {
	EnableSubtitles       bool   `json:"enableSubtitles"`
	OpenSubtitlesApiKey   string `json:"openSubtitlesApiKey"`
	OpenSubtitlesUsername string `json:"openSubtitlesUsername"`
	OpenSubtitlesPassword string `json:"openSubtitlesPassword"`
	SubtitleLanguages     string `json:"subtitleLanguages"`
}

type subtitleConfig struct {
	Enabled   bool
	APIKey    string
	Username  string
	Password  string
	Languages []string
}

var languageCodeRe = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,4})?$`)

// defaultSubtitleLanguages is the priority order used when none is configured.
// OpenSubtitles language codes (verified against GET /infos/languages):
// "es" = Spanish, "ea" = Spanish (LA), "sp" = Spanish (EU). Most Latin
// American releases are still tagged "es", so it goes first.
const defaultSubtitleLanguages = "es,ea,sp,en"

// parseLanguages turns "es, en" into ["es", "en"], keeping the order (it is
// the priority order). Falls back to defaultSubtitleLanguages.
func parseLanguages(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		code := strings.ToLower(strings.TrimSpace(part))
		if !languageCodeRe.MatchString(code) || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
		if len(out) == 5 {
			break
		}
	}
	if len(out) == 0 {
		return strings.Split(defaultSubtitleLanguages, ",")
	}
	return out
}

func snapshotSubtitleConfig() subtitleConfig {
	settingsMutex.RLock()
	defer settingsMutex.RUnlock()
	return subtitleConfig{
		Enabled:   currentSettings.EnableSubtitles,
		APIKey:    currentSettings.OpenSubtitlesApiKey,
		Username:  currentSettings.OpenSubtitlesUsername,
		Password:  currentSettings.OpenSubtitlesPassword,
		Languages: parseLanguages(currentSettings.SubtitleLanguages),
	}
}

func saveSubtitleSettingsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var in SubtitleSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	settingsMutex.Lock()
	currentSettings.EnableSubtitles = in.EnableSubtitles
	currentSettings.OpenSubtitlesUsername = strings.TrimSpace(in.OpenSubtitlesUsername)
	// Empty key/password means "keep the stored one" (the UI never sees them)
	if in.OpenSubtitlesApiKey != "" {
		currentSettings.OpenSubtitlesApiKey = strings.TrimSpace(in.OpenSubtitlesApiKey)
	}
	if in.OpenSubtitlesPassword != "" {
		currentSettings.OpenSubtitlesPassword = in.OpenSubtitlesPassword
	}
	currentSettings.SubtitleLanguages = strings.Join(parseLanguages(in.SubtitleLanguages), ",")
	settingsMutex.Unlock()

	// credentials may have changed: drop the cached login token
	osSession.reset()

	if err := saveSettingsToFile(); err != nil {
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to save settings: " + err.Error()})
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Subtitle settings saved successfully"})
}

// testSubtitleConnectionHandler logs in with the submitted (or stored)
// credentials to validate API key, username and password.
func testSubtitleConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var in SubtitleSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	cfg := snapshotSubtitleConfig()
	if in.OpenSubtitlesApiKey != "" {
		cfg.APIKey = strings.TrimSpace(in.OpenSubtitlesApiKey)
	}
	if in.OpenSubtitlesUsername != "" {
		cfg.Username = strings.TrimSpace(in.OpenSubtitlesUsername)
	}
	if in.OpenSubtitlesPassword != "" {
		cfg.Password = in.OpenSubtitlesPassword
	}
	if cfg.APIKey == "" || cfg.Username == "" || cfg.Password == "" {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "API key, username and password are required"})
		return
	}

	if _, _, err := osLogin(cfg); err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Connected to OpenSubtitles as " + cfg.Username})
}

// ---------------------------------------------------------------------------
// OpenSubtitles HTTP client
// ---------------------------------------------------------------------------

type osLoginState struct {
	mu      sync.Mutex
	token   string
	host    string
	expires time.Time
	owner   string // api key + username the token was issued for
}

var osSession osLoginState

func (s *osLoginState) reset() {
	s.mu.Lock()
	s.token, s.host, s.owner = "", "", ""
	s.expires = time.Time{}
	s.mu.Unlock()
}

func osBaseURL(host string) string {
	return "https://" + host + "/api/v1"
}

// osDo performs a request against the OpenSubtitles API and returns the body
// and status code. bearer may be empty.
func osDo(cfg subtitleConfig, method, rawURL string, body []byte, bearer string) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Api-Key", cfg.APIKey)
	req.Header.Set("User-Agent", osUserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := subtitleHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, osMaxSubtitleLen))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// osAPIError extracts a readable message from an API error body.
func osAPIError(prefix string, status int, data []byte) error {
	var e struct {
		Message string   `json:"message"`
		Errors  []string `json:"errors"`
	}
	_ = json.Unmarshal(data, &e)
	msg := e.Message
	if msg == "" && len(e.Errors) > 0 {
		msg = strings.Join(e.Errors, "; ")
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("%s (HTTP %d): %s", prefix, status, msg)
}

// osLogin authenticates and returns the bearer token and the API host to use.
func osLogin(cfg subtitleConfig) (token, host string, err error) {
	payload, _ := json.Marshal(map[string]string{
		"username": cfg.Username,
		"password": cfg.Password,
	})
	data, status, err := osDo(cfg, http.MethodPost, osBaseURL(osDefaultHost)+"/login", payload, "")
	if err != nil {
		return "", "", fmt.Errorf("OpenSubtitles login failed: %w", err)
	}
	if status != http.StatusOK {
		return "", "", osAPIError("OpenSubtitles login failed", status, data)
	}

	var resp struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || resp.Token == "" {
		return "", "", errors.New("OpenSubtitles login returned no token")
	}

	host = strings.TrimSpace(resp.BaseURL)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	// never send the token anywhere that is not an OpenSubtitles host
	if host == "" || !(host == "opensubtitles.com" || strings.HasSuffix(host, ".opensubtitles.com")) {
		host = osDefaultHost
	}
	return resp.Token, host, nil
}

// osEnsureLogin returns a cached token or logs in again (tokens last 24h).
func osEnsureLogin(cfg subtitleConfig) (string, string, error) {
	if cfg.APIKey == "" || cfg.Username == "" || cfg.Password == "" {
		return "", "", errors.New("OpenSubtitles API key, username and password must be configured to download subtitles")
	}
	owner := cfg.APIKey + "|" + cfg.Username

	osSession.mu.Lock()
	defer osSession.mu.Unlock()
	if osSession.token != "" && osSession.owner == owner && time.Now().Before(osSession.expires) {
		return osSession.token, osSession.host, nil
	}

	token, host, err := osLogin(cfg)
	if err != nil {
		return "", "", err
	}
	osSession.token = token
	osSession.host = host
	osSession.owner = owner
	osSession.expires = time.Now().Add(23 * time.Hour)
	return token, host, nil
}

// ---------------------------------------------------------------------------
// Media name parsing
// ---------------------------------------------------------------------------

type mediaInfo struct {
	Title   string
	Year    int
	Season  int
	Episode int
	Clean   string // whole file name, separators normalised
}

var (
	reSxxEyy = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]*E(\d{1,3})\b`)
	reNxNN   = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{2,3})\b`)
	reYear   = regexp.MustCompile(`(?:^|[ (\[])((?:19|20)\d{2})(?:$|[ )\]])`)
	reJunk   = regexp.MustCompile(`(?i)\b(480p|576p|720p|1080p|2160p|4k|bluray|blu-ray|brrip|bdrip|webrip|web-dl|webdl|hdtv|dvdrip|x264|x265|h264|h265|hevc|aac|ac3|dts|remux|proper|repack|extended|unrated)\b.*$`)
	reSpaces = regexp.MustCompile(`\s+`)
)

func tidyTitle(s string) string {
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.Trim(s, " -([._")
}

func parseMediaName(fileName string) mediaInfo {
	base := filepath.Base(fileName)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	clean := tidyTitle(strings.NewReplacer(".", " ", "_", " ").Replace(base))
	info := mediaInfo{Clean: clean, Title: clean}

	for _, re := range []*regexp.Regexp{reSxxEyy, reNxNN} {
		if m := re.FindStringSubmatchIndex(clean); m != nil {
			season, _ := strconv.Atoi(clean[m[2]:m[3]])
			episode, _ := strconv.Atoi(clean[m[4]:m[5]])
			if title := tidyTitle(clean[:m[0]]); title != "" && season > 0 && episode > 0 {
				info.Title, info.Season, info.Episode = title, season, episode
				return info
			}
		}
	}

	// the release year is the last year-looking token that still leaves a title
	// before it ("2001 A Space Odyssey 1968" -> year 1968)
	yearMatches := reYear.FindAllStringSubmatchIndex(clean, -1)
	for i := len(yearMatches) - 1; i >= 0; i-- {
		m := yearMatches[i]
		year, _ := strconv.Atoi(clean[m[2]:m[3]])
		if title := tidyTitle(clean[:m[2]]); title != "" {
			info.Title, info.Year = title, year
			return info
		}
	}

	if title := tidyTitle(reJunk.ReplaceAllString(clean, "")); title != "" {
		info.Title = title
	}
	return info
}

// ---------------------------------------------------------------------------
// OpenSubtitles hash (size + first/last 64 KiB), computed over the torrent stream
// ---------------------------------------------------------------------------

func openSubtitlesHash(r io.ReadSeeker, size int64) (string, error) {
	const chunk = 65536
	if size < chunk*2 {
		return "", errors.New("file too small to hash")
	}
	hash := uint64(size)
	buf := make([]byte, chunk)
	for _, offset := range []int64{0, size - chunk} {
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		for i := 0; i < chunk; i += 8 {
			hash += binary.LittleEndian.Uint64(buf[i : i+8])
		}
	}
	return fmt.Sprintf("%016x", hash), nil
}

// hashWithTimeout hashes the stream but gives up after d: the end of the file
// may not have been downloaded yet, in which case we fall back to name search.
// The reader is always closed.
func hashWithTimeout(r io.ReadSeekCloser, size int64, d time.Duration) string {
	type result struct {
		hash string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		h, err := openSubtitlesHash(r, size)
		ch <- result{h, err}
	}()

	select {
	case res := <-ch:
		r.Close()
		if res.err != nil {
			log.Printf("Subtitles: could not hash file: %v", res.err)
			return ""
		}
		return res.hash
	case <-time.After(d):
		r.Close() // unblocks the pending read
		log.Printf("Subtitles: hashing timed out after %s, searching by name only", d)
		return ""
	}
}

// ---------------------------------------------------------------------------
// Search & ranking
// ---------------------------------------------------------------------------

type osSearchResponse struct {
	Data []osSubtitle `json:"data"`
}

// Fields verified against live GET /subtitles responses (2026-11-04).
// moviehash_match is only present when the request includes moviehash.
type osSubtitle struct {
	ID         string `json:"id"`
	Attributes struct {
		Language          string `json:"language"`
		NbCD              int    `json:"nb_cd"`
		DownloadCount     int    `json:"download_count"`
		HearingImpaired   bool   `json:"hearing_impaired"`
		AITranslated      bool   `json:"ai_translated"`
		MachineTranslated bool   `json:"machine_translated"`
		FromTrusted       bool   `json:"from_trusted"`
		MovieHashMatch    bool   `json:"moviehash_match"`
		Release           string `json:"release"`
		Files             []struct {
			FileID   int    `json:"file_id"`
			FileName string `json:"file_name"`
		} `json:"files"`
	} `json:"attributes"`
}

func osSearchOnce(cfg subtitleConfig, q url.Values) ([]osSubtitle, error) {
	// The API answers 301 to non-canonical URLs (unsorted keys, uppercase
	// values). url.Values.Encode sorts keys; values are lowercased by callers.
	// "%2C"-encoded commas and "+" for spaces are accepted as-is (verified).
	data, status, err := osDo(cfg, http.MethodGet, osBaseURL(osDefaultHost)+"/subtitles?"+q.Encode(), nil, "")
	if err != nil {
		return nil, fmt.Errorf("OpenSubtitles search failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, osAPIError("OpenSubtitles search failed", status, data)
	}
	var resp osSearchResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("OpenSubtitles search returned invalid JSON: %w", err)
	}
	return resp.Data, nil
}

func osSearch(cfg subtitleConfig, info mediaInfo, hash string) ([]osSubtitle, error) {
	langs := strings.Join(cfg.Languages, ",")

	q := url.Values{}
	q.Set("languages", langs)
	q.Set("query", strings.ToLower(info.Title))
	if info.Season > 0 && info.Episode > 0 {
		q.Set("type", "episode")
		q.Set("season_number", strconv.Itoa(info.Season))
		q.Set("episode_number", strconv.Itoa(info.Episode))
	} else {
		q.Set("type", "movie")
		if info.Year > 0 {
			q.Set("year", strconv.Itoa(info.Year))
		}
	}
	if hash != "" {
		q.Set("moviehash", hash)
	}

	results, err := osSearchOnce(cfg, q)
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		return results, nil
	}

	// Second attempt: the whole cleaned file name, without structured filters
	fallback := url.Values{}
	fallback.Set("languages", langs)
	fallback.Set("query", strings.ToLower(info.Clean))
	if hash != "" {
		fallback.Set("moviehash", hash)
	}
	return osSearchOnce(cfg, fallback)
}

var tokenRe = regexp.MustCompile(`[a-z0-9]+`)

func nameTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range tokenRe.FindAllString(strings.ToLower(s), -1) {
		if len(tok) >= 3 {
			out[tok] = true
		}
	}
	return out
}

func scoreSubtitle(sub osSubtitle, fileTokens map[string]bool) float64 {
	a := sub.Attributes
	score := 0.0
	if a.MovieHashMatch {
		score += 1000
	}
	if a.AITranslated || a.MachineTranslated {
		score -= 300
	}
	if a.HearingImpaired {
		score -= 50
	}
	if a.FromTrusted {
		score += 30
	}
	score += math.Min(math.Log10(float64(a.DownloadCount)+1)*20, 100)

	overlap := 0
	for tok := range nameTokens(a.Release) {
		if fileTokens[tok] {
			overlap++
		}
	}
	score += math.Min(float64(overlap)*15, 150)
	return score
}

type subtitleTrack struct {
	ID        string `json:"id"`
	Lang      string `json:"lang"`
	Label     string `json:"label"`
	Release   string `json:"release,omitempty"`
	HashMatch bool   `json:"hashMatch"`
	Default   bool   `json:"default"`
	Src       string `json:"src"`
}

var languageNames = map[string]string{
	"es": "Español", "ea": "Español (Latinoamérica)", "sp": "Español (España)", "en": "English", "pt": "Português", "pt-br": "Português (Brasil)",
	"fr": "Français", "it": "Italiano", "de": "Deutsch", "ja": "日本語", "ko": "한국어",
}

func languageLabel(code string) string {
	if name, ok := languageNames[strings.ToLower(code)]; ok {
		return name
	}
	return strings.ToUpper(code)
}

// pickBest returns the best candidate per language, in the configured
// priority order. The first one is flagged as the default track.
func pickBest(results []osSubtitle, langs []string, fileName string) []subtitleTrack {
	fileTokens := nameTokens(fileName)
	var tracks []subtitleTrack
	used := map[int]bool{}

	for _, lang := range langs {
		bestIdx, bestScore := -1, math.Inf(-1)
		for i, sub := range results {
			if !strings.EqualFold(sub.Attributes.Language, lang) || len(sub.Attributes.Files) == 0 {
				continue
			}
			// multi-CD subtitles would only cover part of a single video file
			if sub.Attributes.NbCD > 1 || len(sub.Attributes.Files) > 1 {
				continue
			}
			if used[sub.Attributes.Files[0].FileID] {
				continue
			}
			if s := scoreSubtitle(sub, fileTokens); s > bestScore {
				bestIdx, bestScore = i, s
			}
		}
		if bestIdx < 0 {
			continue
		}
		best := results[bestIdx]
		fileID := best.Attributes.Files[0].FileID
		used[fileID] = true
		id := strconv.Itoa(fileID)
		tracks = append(tracks, subtitleTrack{
			ID:        id,
			Lang:      strings.ToLower(lang),
			Label:     languageLabel(lang),
			Release:   best.Attributes.Release,
			HashMatch: best.Attributes.MovieHashMatch,
			Src:       "/api/v1/subtitles/file/" + id + ".vtt",
		})
	}
	if len(tracks) > 0 {
		tracks[0].Default = true
	}
	return tracks
}

// subtitlesAutoHandler: GET /api/v1/subtitles/auto?session=<id>&file=<index>
func subtitlesAutoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := snapshotSubtitleConfig()
	if !cfg.Enabled || cfg.APIKey == "" {
		respondWithJSON(w, http.StatusOK, map[string]interface{}{"tracks": []subtitleTrack{}, "reason": "disabled"})
		return
	}

	sessionID := r.URL.Query().Get("session")
	fileIndex, err := strconv.Atoi(r.URL.Query().Get("file"))
	if err != nil {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid file index"})
		return
	}
	sessionValue, ok := sessions.Load(sessionID)
	if !ok {
		respondWithJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	session := sessionValue.(*TorrentSession)
	session.LastUsed = time.Now()
	if session.Torrent.Info() == nil {
		respondWithJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Torrent metadata not available yet"})
		return
	}
	files := session.Torrent.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "File index out of range"})
		return
	}
	file := files[fileIndex]

	name := filepath.Base(file.DisplayPath())
	info := parseMediaName(name)
	hash := hashWithTimeout(file.NewReader(), file.Length(), osHashTimeout)
	log.Printf("Subtitles: searching %q (title=%q season=%d episode=%d year=%d hash=%q) languages=%v",
		name, info.Title, info.Season, info.Episode, info.Year, hash, cfg.Languages)

	results, err := osSearch(cfg, info, hash)
	if err != nil {
		log.Printf("Subtitles: %v", err)
		respondWithJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	tracks := pickBest(results, cfg.Languages, name)
	if tracks == nil {
		tracks = []subtitleTrack{}
	}
	log.Printf("Subtitles: %d result(s), %d track(s) selected", len(results), len(tracks))
	respondWithJSON(w, http.StatusOK, map[string]interface{}{"tracks": tracks, "title": info.Title})
}

// ---------------------------------------------------------------------------
// Download, conversion and cache
// ---------------------------------------------------------------------------

var (
	subtitleCacheMu    sync.Mutex
	subtitleCache      = map[string][]byte{}
	subtitleCacheOrder []string
	subtitleDownloadMu sync.Mutex // serialises downloads so the quota is not wasted twice
)

func subtitleCacheGet(id string) ([]byte, bool) {
	subtitleCacheMu.Lock()
	defer subtitleCacheMu.Unlock()
	v, ok := subtitleCache[id]
	return v, ok
}

func subtitleCachePut(id string, vtt []byte) {
	subtitleCacheMu.Lock()
	defer subtitleCacheMu.Unlock()
	if _, exists := subtitleCache[id]; !exists {
		subtitleCacheOrder = append(subtitleCacheOrder, id)
		if len(subtitleCacheOrder) > osCacheEntries {
			delete(subtitleCache, subtitleCacheOrder[0])
			subtitleCacheOrder = subtitleCacheOrder[1:]
		}
	}
	subtitleCache[id] = vtt
}

// ensureUTF8 passes valid UTF-8 through and otherwise assumes Latin-1.
func ensureUTF8(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return []byte(string(runes))
}

func fetchSubtitleVTT(cfg subtitleConfig, fileID string) ([]byte, error) {
	if v, ok := subtitleCacheGet(fileID); ok {
		return v, nil
	}

	subtitleDownloadMu.Lock()
	defer subtitleDownloadMu.Unlock()
	if v, ok := subtitleCacheGet(fileID); ok {
		return v, nil
	}

	id, err := strconv.Atoi(fileID)
	if err != nil {
		return nil, errors.New("invalid subtitle id")
	}
	payload, _ := json.Marshal(map[string]interface{}{"file_id": id, "sub_format": "srt"})

	var data []byte
	var status int
	for attempt := 0; attempt < 2; attempt++ {
		token, host, err := osEnsureLogin(cfg)
		if err != nil {
			return nil, err
		}
		data, status, err = osDo(cfg, http.MethodPost, osBaseURL(host)+"/download", payload, token)
		if err != nil {
			return nil, fmt.Errorf("OpenSubtitles download failed: %w", err)
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			osSession.reset() // expired token: log in again once
			continue
		}
		break
	}
	if status == http.StatusNotAcceptable {
		return nil, osAPIError("OpenSubtitles daily download quota reached", status, data)
	}
	if status != http.StatusOK {
		return nil, osAPIError("OpenSubtitles download failed", status, data)
	}

	var dl struct {
		Link      string `json:"link"`
		Remaining int    `json:"remaining"`
	}
	if err := json.Unmarshal(data, &dl); err != nil || dl.Link == "" {
		return nil, errors.New("OpenSubtitles download returned no link")
	}
	if !strings.HasPrefix(dl.Link, "https://") {
		return nil, errors.New("OpenSubtitles download link is not HTTPS")
	}
	log.Printf("Subtitles: downloading file %s (%d downloads remaining today)", fileID, dl.Remaining)

	req, err := http.NewRequest(http.MethodGet, dl.Link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", osUserAgent)
	resp, err := subtitleHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subtitle file download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subtitle file download failed (HTTP %d)", resp.StatusCode)
	}
	srt, err := io.ReadAll(io.LimitReader(resp.Body, osMaxSubtitleLen))
	if err != nil {
		return nil, fmt.Errorf("subtitle file download failed: %w", err)
	}

	vtt := convertSRTtoVTT(ensureUTF8(srt))
	subtitleCachePut(fileID, vtt)
	return vtt, nil
}

// subtitleFileHandler: GET /api/v1/subtitles/file/<file_id>.vtt
func subtitleFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := snapshotSubtitleConfig()
	if !cfg.Enabled {
		http.Error(w, "Subtitle search is disabled", http.StatusForbidden)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/v1/subtitles/file/")
	id = strings.TrimSuffix(id, ".vtt")
	if _, err := strconv.Atoi(id); err != nil {
		http.Error(w, "Invalid subtitle id", http.StatusBadRequest)
		return
	}

	vtt, err := fetchSubtitleVTT(cfg, id)
	if err != nil {
		log.Printf("Subtitles: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Write(vtt)
}
