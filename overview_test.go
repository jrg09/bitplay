package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTMDBFind(t *testing.T) {
	body := []byte(`{"movie_results":[{"overview":""},{"overview":"  A   movie\nplot. "}],"tv_results":[{"overview":"A show plot."}]}`)
	if got, _ := parseTMDBFind(body, "movie"); got != "A movie plot." {
		t.Errorf("movie = %q", got)
	}
	if got, _ := parseTMDBFind(body, "series"); got != "A show plot." {
		t.Errorf("series = %q", got)
	}
	// the other type is a fallback
	if got, _ := parseTMDBFind([]byte(`{"movie_results":[],"tv_results":[{"overview":"Only a show"}]}`), "movie"); got != "Only a show" {
		t.Errorf("fallback = %q", got)
	}
	if got, err := parseTMDBFind([]byte(`{"movie_results":[],"tv_results":[]}`), "movie"); got != "" || err != nil {
		t.Errorf("empty = %q, %v", got, err)
	}
	if _, err := parseTMDBFind([]byte(`nope`), "movie"); err == nil {
		t.Error("invalid JSON must be an error")
	}
}

func TestParseCinemetaOverview(t *testing.T) {
	if got, _ := parseCinemetaOverview([]byte(`{"meta":{"description":"Plot  here."}}`)); got != "Plot here." {
		t.Errorf("got %q", got)
	}
	if got, _ := parseCinemetaOverview([]byte(`{"meta":{}}`)); got != "" {
		t.Errorf("no description = %q", got)
	}
}

func TestCleanOverviewTruncates(t *testing.T) {
	long := strings.Repeat("á", overviewMaxLen+50)
	got := cleanOverview(long)
	if r := []rune(got); len(r) != overviewMaxLen+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncated to %d runes", len(r))
	}
}

func TestLookupOverviewFallbackChain(t *testing.T) {
	boom := errors.New("boom")
	cin := func(text string, err error) func(context.Context, string, string) (string, error) {
		return func(context.Context, string, string) (string, error) { return text, err }
	}
	var langs []string
	tmdb := func(byLang map[string]string) func(context.Context, string, string, string) (string, error) {
		return func(_ context.Context, _, _, lang string) (string, error) {
			langs = append(langs, lang)
			if v, ok := byLang[lang]; ok {
				return v, nil
			}
			return "", boom
		}
	}
	ctx := context.Background()

	cases := []struct {
		name      string
		src       overviewSources
		want      overviewResult
		wantLangs []string
	}{
		{"wanted language", overviewSources{tmdb: tmdb(map[string]string{"es-MX": "Sinopsis"}), cinemeta: cin("English", nil), lang: "es-MX"},
			overviewResult{"Sinopsis", "es-MX"}, []string{"es-MX"}},
		{"empty in Spanish falls back to TMDB English", overviewSources{tmdb: tmdb(map[string]string{"es-MX": "", "en-US": "TMDB english"}), cinemeta: cin("English", nil), lang: "es-MX"},
			overviewResult{"TMDB english", "en-US"}, []string{"es-MX", "en-US"}},
		{"TMDB down falls back to Cinemeta", overviewSources{tmdb: tmdb(nil), cinemeta: cin("Cinemeta plot", nil), lang: "es-MX"},
			overviewResult{"Cinemeta plot", "en"}, []string{"es-MX", "en-US"}},
		{"no API key uses Cinemeta only", overviewSources{cinemeta: cin("Cinemeta plot", nil), lang: "es-MX"},
			overviewResult{"Cinemeta plot", "en"}, nil},
		{"English wanted does not ask twice", overviewSources{tmdb: tmdb(nil), cinemeta: cin("Cinemeta plot", nil), lang: "en-GB"},
			overviewResult{"Cinemeta plot", "en"}, []string{"en-GB"}},
		{"nothing anywhere", overviewSources{cinemeta: cin("", boom), lang: "es-MX"},
			overviewResult{}, nil},
	}
	for _, c := range cases {
		langs = nil
		got := lookupOverview(ctx, c.src, "tt1", "movie")
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
		if strings.Join(langs, ",") != strings.Join(c.wantLangs, ",") {
			t.Errorf("%s: TMDB asked for %v, want %v", c.name, langs, c.wantLangs)
		}
	}
}

func TestTMDBOverviewAuth(t *testing.T) {
	var gotAuth, gotKey, gotLang, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey, gotLang, gotPath = r.Header.Get("Authorization"), r.URL.Query().Get("api_key"), r.URL.Query().Get("language"), r.URL.Path
		w.Write([]byte(`{"movie_results":[{"overview":"Sinopsis"}],"tv_results":[]}`))
	}))
	defer srv.Close()

	got, err := tmdbOverview(context.Background(), srv.Client(), srv.URL, "v3key", "tt0903747", "movie", "es-MX")
	if err != nil || got != "Sinopsis" {
		t.Fatalf("got %q, %v", got, err)
	}
	if gotKey != "v3key" || gotAuth != "" || gotLang != "es-MX" || gotPath != "/find/tt0903747" {
		t.Errorf("v3 key request: key=%q auth=%q lang=%q path=%q", gotKey, gotAuth, gotLang, gotPath)
	}

	if _, err := tmdbOverview(context.Background(), srv.Client(), srv.URL, "eyJhbGciOi.token", "tt1", "movie", "en-US"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer eyJhbGciOi.token" || gotKey != "" {
		t.Errorf("v4 token request: key=%q auth=%q", gotKey, gotAuth)
	}
}

func TestTMDBLanguageSetting(t *testing.T) {
	for env, want := range map[string]string{"": "es-MX", "es-ES": "es-ES", "pt": "pt", "garbage!": "es-MX", "ES-mx": "es-MX"} {
		t.Setenv("BITPLAY_TMDB_LANGUAGE", env)
		if got := tmdbLanguage(); got != want {
			t.Errorf("BITPLAY_TMDB_LANGUAGE=%q -> %q, want %q", env, got, want)
		}
	}
}

func TestOverviewHandler(t *testing.T) {
	t.Setenv("BITPLAY_TMDB_LANGUAGE", "es-MX")
	overviewCachePut("series|tt0903747|es-MX", overviewResult{"Sinopsis cacheada", "es-MX"})
	defer func() {
		overviewCache.Lock()
		overviewCache.m = map[string]overviewEntry{}
		overviewCache.Unlock()
	}()

	do := func(query string) (int, string) {
		rec := httptest.NewRecorder()
		overviewHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview?"+query, nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := do("imdb=tt0903747&type=series"); code != 200 || !strings.Contains(body, "Sinopsis cacheada") || !strings.Contains(body, `"lang":"es-MX"`) {
		t.Errorf("cached: %d %s", code, body)
	}
	for _, bad := range []string{"imdb=nope", "imdb=", "", "imdb=tt1;drop"} {
		if code, _ := do(bad); code != 400 {
			t.Errorf("%q: code %d, want 400", bad, code)
		}
	}
	rec := httptest.NewRecorder()
	overviewHandler(rec, httptest.NewRequest(http.MethodPost, "/api/v1/overview?imdb=tt0903747", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: code %d", rec.Code)
	}
}

func TestParseKeyFile(t *testing.T) {
	for in, want := range map[string]string{
		"0123456789abcdef0123456789abcdef\n":     "0123456789abcdef0123456789abcdef",
		"  abc123  ":                             "abc123",
		"TMDB_KEY=abc123\n":                      "abc123",
		"BITPLAY_TMDB_API_KEY = \"abc123\"\r\n":  "abc123",
		"# my TMDB key\n\nabc123\nsecond line\n": "abc123",
		"eyJhbGciOi.payload.signature\n":         "eyJhbGciOi.payload.signature",
		"":                                       "",
		"\n# only a comment\n":                   "",
	} {
		if got := parseKeyFile(in); got != want {
			t.Errorf("parseKeyFile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTMDBAPIKeySources(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tmdb.key")
	if err := os.WriteFile(file, []byte("filekey123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BITPLAY_TMDB_API_KEY", "")
	t.Setenv("BITPLAY_TMDB_API_KEY_FILE", file)
	if got := tmdbAPIKey(); got != "filekey123" {
		t.Errorf("from file = %q", got)
	}
	// the file is re-read, so a rotated key needs no restart
	if err := os.WriteFile(file, []byte("rotated456\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := tmdbAPIKey(); got != "rotated456" {
		t.Errorf("after rotation = %q", got)
	}
	// an explicit variable wins
	t.Setenv("BITPLAY_TMDB_API_KEY", "envkey")
	if got := tmdbAPIKey(); got != "envkey" {
		t.Errorf("env = %q", got)
	}
	// a missing file means no key, not a crash
	t.Setenv("BITPLAY_TMDB_API_KEY", "")
	t.Setenv("BITPLAY_TMDB_API_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	if got := tmdbAPIKey(); got != "" {
		t.Errorf("missing file = %q", got)
	}
}
