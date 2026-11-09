package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTMDBFindID(t *testing.T) {
	body := []byte(`{"movie_results":[{"id":11}],"tv_results":[{"id":22}]}`)
	if kind, id, _ := parseTMDBFindID(body, "movie"); kind != "movie" || id != 11 {
		t.Errorf("movie = %s %d", kind, id)
	}
	if kind, id, _ := parseTMDBFindID(body, "series"); kind != "tv" || id != 22 {
		t.Errorf("series = %s %d", kind, id)
	}
	// the other type is a fallback
	if kind, id, _ := parseTMDBFindID([]byte(`{"movie_results":[],"tv_results":[{"id":5}]}`), "movie"); kind != "tv" || id != 5 {
		t.Errorf("fallback = %s %d", kind, id)
	}
	if _, id, err := parseTMDBFindID([]byte(`{"movie_results":[],"tv_results":[]}`), "movie"); id != 0 || err != nil {
		t.Errorf("empty = %d, %v", id, err)
	}
}

func TestParseCinemetaTrailers(t *testing.T) {
	body := []byte(`{"meta":{"trailers":[{"source":"MuhWxdD0qno","type":"Trailer"},{"source":"short","type":"Trailer"},{"source":"LvKvus3vCEY","type":"Clip"}],
		"trailerStreams":[{"ytId":"MuhWxdD0qno"},{"ytId":"zFjaJbihWwc"}]}}`)
	ids, err := parseCinemetaTrailers(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "MuhWxdD0qno,zFjaJbihWwc" {
		t.Errorf("ids = %v (invalid ids, clips and duplicates must go)", ids)
	}
}

func TestRankTrailers(t *testing.T) {
	videos := []tmdbVideo{
		{Key: "clipclip111", Site: "YouTube", Type: "Clip", Lang: "es", Official: true},
		{Key: "vimeoooo111", Site: "Vimeo", Type: "Trailer", Lang: "es", Official: true},
		{Key: "teaserES111", Site: "YouTube", Type: "Teaser", Lang: "es", Official: true},
		{Key: "trailerEN11", Site: "YouTube", Type: "Trailer", Lang: "en", Official: true, PublishedAt: "2026-01-01"},
		{Key: "trailerFR11", Site: "YouTube", Type: "Trailer", Lang: "fr", Official: true},
		{Key: "trailerES11", Site: "YouTube", Type: "Trailer", Lang: "es", Official: false, PublishedAt: "2026-02-01"},
		{Key: "trailerES22", Site: "YouTube", Type: "Trailer", Lang: "es", Official: true, PublishedAt: "2025-01-01"},
		{Key: "trailerES11", Site: "YouTube", Type: "Trailer", Lang: "es"}, // duplicate
		{Key: "bad", Site: "YouTube", Type: "Trailer", Lang: "es"},         // not a YouTube id
	}
	var keys []string
	for _, v := range rankTrailers(videos, "es-MX") {
		keys = append(keys, v.Key)
	}
	// trailers first (Spanish official, Spanish unofficial, English, other),
	// then teasers; clips, other sites and invalid ids are gone
	want := "trailerES22,trailerES11,trailerEN11,trailerFR11,teaserES111"
	if strings.Join(keys, ",") != want {
		t.Errorf("order = %v, want %s", keys, want)
	}
}

func TestLookupTrailer(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	var asked [][]string
	tmdb := func(videos []tmdbVideo, err error) func(context.Context, string, string, []string) ([]tmdbVideo, error) {
		return func(_ context.Context, _, _ string, langs []string) ([]tmdbVideo, error) {
			asked = append(asked, langs)
			return videos, err
		}
	}
	cin := func(ids ...string) func(context.Context, string, string) ([]string, error) {
		return func(context.Context, string, string) ([]string, error) { return ids, nil }
	}
	spanish := []tmdbVideo{{Key: "trailerES11", Site: "YouTube", Type: "Trailer", Lang: "es", Official: true}}
	blocked := func(blockedIDs ...string) func(context.Context, string) bool {
		return func(_ context.Context, id string) bool {
			for _, b := range blockedIDs {
				if b == id {
					return false
				}
			}
			return true
		}
	}

	cases := []struct {
		name  string
		src   trailerSources
		want  trailerResult
		langs string
	}{
		{"TMDB in Spanish", trailerSources{tmdbVideos: tmdb(spanish, nil), cinemeta: cin("cinemeta111"), lang: "es-MX"},
			trailerResult{"trailerES11", "es", "tmdb"}, "es-MX,en-US"},
		{"not embeddable is skipped", trailerSources{tmdbVideos: tmdb(append([]tmdbVideo{{Key: "blockedES11", Site: "YouTube", Type: "Trailer", Lang: "es", Official: true}}, spanish...), nil),
			embeddable: blocked("blockedES11"), lang: "es-MX"},
			trailerResult{"trailerES11", "es", "tmdb"}, "es-MX,en-US"},
		{"TMDB down uses Cinemeta", trailerSources{tmdbVideos: tmdb(nil, boom), cinemeta: cin("cinemeta111"), lang: "es-MX"},
			trailerResult{"cinemeta111", "", "cinemeta"}, "es-MX,en-US"},
		{"TMDB without videos uses Cinemeta", trailerSources{tmdbVideos: tmdb([]tmdbVideo{{Key: "clipclip111", Site: "YouTube", Type: "Clip"}}, nil), cinemeta: cin("cinemeta111"), lang: "es-MX"},
			trailerResult{"cinemeta111", "", "cinemeta"}, "es-MX,en-US"},
		{"no key: Cinemeta only", trailerSources{cinemeta: cin("cinemeta111"), lang: "es-MX"},
			trailerResult{"cinemeta111", "", "cinemeta"}, ""},
		{"English wanted: one language", trailerSources{tmdbVideos: tmdb(nil, nil), lang: "en-GB"}, trailerResult{}, "en-GB"},
		{"Cinemeta blocked too", trailerSources{cinemeta: cin("cinemeta111"), embeddable: blocked("cinemeta111"), lang: "es-MX"}, trailerResult{}, ""},
		{"nothing anywhere", trailerSources{lang: "es-MX"}, trailerResult{}, ""},
	}
	for _, c := range cases {
		asked = nil
		got := lookupTrailer(ctx, c.src, "tt1", "movie")
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
		if len(asked) > 0 && strings.Join(asked[0], ",") != c.langs {
			t.Errorf("%s: TMDB asked for %v, want %s", c.name, asked[0], c.langs)
		}
	}
}

func TestYouTubeEmbeddable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.RawQuery, "okokokokok1"):
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.RawQuery, "noembed1111"):
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	for id, want := range map[string]bool{"okokokokok1": true, "noembed1111": false, "missing1111": false} {
		if got := youtubeEmbeddable(ctx, srv.Client(), srv.URL, id); got != want {
			t.Errorf("%s: got %v, want %v", id, got, want)
		}
	}
	srv.Close()
	if !youtubeEmbeddable(ctx, http.DefaultClient, srv.URL, "okokokokok1") {
		t.Error("an unreachable oEmbed must not hide the trailer")
	}
}

func TestTMDBVideosForRequests(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?language="+r.URL.Query().Get("language")+"&key="+r.URL.Query().Get("api_key"))
		switch {
		case strings.HasPrefix(r.URL.Path, "/find/"):
			w.Write([]byte(`{"movie_results":[],"tv_results":[{"id":99}]}`))
		case r.URL.Query().Get("language") == "es-MX":
			w.Write([]byte(`{"results":[{"key":"trailerES11","site":"YouTube","type":"Trailer","iso_639_1":"es"}]}`))
		default:
			w.Write([]byte(`{"results":[{"key":"trailerEN11","site":"YouTube","type":"Trailer","iso_639_1":"en"}]}`))
		}
	}))
	defer srv.Close()

	videos, err := tmdbVideosFor(context.Background(), srv.Client(), srv.URL, "v3key", "tt1", "series", []string{"es-MX", "en-US"})
	if err != nil || len(videos) != 2 {
		t.Fatalf("videos = %v, %v", videos, err)
	}
	want := []string{"/find/tt1?language=&key=v3key", "/tv/99/videos?language=es-MX&key=v3key", "/tv/99/videos?language=en-US&key=v3key"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("requests = %v, want %v", paths, want)
	}
}

func TestTrailerHandler(t *testing.T) {
	t.Setenv("BITPLAY_TMDB_LANGUAGE", "es-MX")
	trailerCachePut("movie|tt0829482|es-MX", trailerResult{YouTubeID: "trailerES11", Lang: "es", Source: "tmdb"})
	defer func() {
		trailerCache.Lock()
		trailerCache.m = map[string]trailerEntry{}
		trailerCache.Unlock()
	}()
	do := func(method, query string) (int, string) {
		rec := httptest.NewRecorder()
		trailerHandler(rec, httptest.NewRequest(method, "/api/v1/trailer?"+query, nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := do(http.MethodGet, "imdb=tt0829482&type=movie"); code != 200 || !strings.Contains(body, `"youtubeId":"trailerES11"`) {
		t.Errorf("cached: %d %s", code, body)
	}
	for _, bad := range []string{"imdb=nope", "", "imdb=tt1;x"} {
		if code, _ := do(http.MethodGet, bad); code != 400 {
			t.Errorf("%q: code %d, want 400", bad, code)
		}
	}
	if code, _ := do(http.MethodPost, "imdb=tt0829482"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: code %d", code)
	}
}
