package main

import (
	"encoding/json"
	"testing"
)

func TestCinemetaItemToMeta(t *testing.T) {
	var resp struct {
		Meta cinemetaItem `json:"meta"`
	}
	body := `{"meta":{"id":"tt0133093","imdb_id":"tt0133093","type":"movie","name":"The Matrix","year":"1999","imdbRating":"8.7","poster":"https://images.metahub.space/poster/small/tt0133093/img","genres":["Action","Sci-Fi"]}}`
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	m := resp.Meta.toMeta()
	if m == nil || m.IMDb != "tt0133093" || m.Title != "The Matrix" || m.Year != "1999" || m.Rating != "8.7" || len(m.Genres) != 2 {
		t.Fatalf("unexpected meta %+v", m)
	}

	insecure := cinemetaItem{ID: "tt0133093", Name: "X", Poster: "http://evil/x.jpg", ReleaseInfo: "2001"}
	if m := insecure.toMeta(); m.Poster != "" || m.Year != "2001" {
		t.Errorf("non-https poster must be dropped and releaseInfo used: %+v", m)
	}
	if (cinemetaItem{ID: "kitsu:1", Name: "X"}).toMeta() != nil {
		t.Error("non-IMDb ids must be ignored")
	}
}

func TestPickCatalogMatch(t *testing.T) {
	items := []cinemetaItem{
		{ID: "tt0422720", Name: "Marie Antoinette", ReleaseInfo: "2006"},
		{ID: "tt0030418", Name: "Marie Antoinette", ReleaseInfo: "1938"},
		{ID: "tt43656794", Name: "Making Marie Antoinette", ReleaseInfo: "2026"},
	}
	if m := pickCatalogMatch(items, "Marie Antoinette", 1938); m == nil || m.ID != "tt0030418" {
		t.Errorf("year 1938: got %+v", m)
	}
	if m := pickCatalogMatch(items, "marie antoinette", 2007); m == nil || m.ID != "tt0422720" {
		t.Errorf("year 2007 (±1): got %+v", m)
	}
	if m := pickCatalogMatch(items, "Marie Antoinette", 0); m == nil || m.ID != "tt0422720" {
		t.Errorf("no year: got %+v", m)
	}
	if m := pickCatalogMatch(items, "Marie Antoinette", 1980); m != nil {
		t.Errorf("no year within ±1 must not match: %+v", m)
	}
	if m := pickCatalogMatch(items, "Antoinette", 0); m != nil {
		t.Errorf("partial names must not match: %+v", m)
	}
}

func TestQueryForResult(t *testing.T) {
	cases := []struct {
		r    searchResult
		want metaQuery
		ok   bool
	}{
		{searchResult{Title: "The Matrix (1999) [1080p bluray]", imdb: "tt0133093"}, metaQuery{id: "tt0133093", typ: "movie"}, true},
		{searchResult{Title: "Breaking Bad S02E05 Breakage 2160p NF WEB-DL", Category: "tv"}, metaQuery{title: "Breaking Bad", typ: "series"}, true},
		{searchResult{Title: "The Matrix (1999) 1080p BrRip x264 - 1.85GB - YIFY", Category: "movies"}, metaQuery{title: "The Matrix", year: 1999, typ: "movie"}, true},
		{searchResult{Title: "Severance.S02E10.1080p.WEB.H264-SuccessfulCrab"}, metaQuery{title: "Severance", typ: "series"}, true},
		{searchResult{Title: "---"}, metaQuery{}, false},
	}
	for _, c := range cases {
		got, ok := queryForResult(c.r)
		if ok != c.ok || got != c.want {
			t.Errorf("queryForResult(%q) = %+v,%v want %+v,%v", c.r.Title, got, ok, c.want, c.ok)
		}
	}
}

func TestGroupResults(t *testing.T) {
	matrix := &mediaMeta{IMDb: "tt0133093", Title: "The Matrix"}
	results := []searchResult{
		{Title: "The Matrix 1999 2160p", Seeders: 900},
		{Title: "Unknown thing", Seeders: 500},
		{Title: "The Matrix (1999) [1080p]", imdb: "tt0133093", Seeders: 100},
	}
	groups := groupResults(results, func(q metaQuery) *mediaMeta {
		if q.id == "tt0133093" || normalizeText(q.title) == "the matrix" {
			return matrix
		}
		return nil
	})
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %+v", groups)
	}
	if groups[0].Key != "tt0133093" || len(groups[0].Results) != 2 || groups[0].Results[0].Seeders != 900 {
		t.Errorf("matrix group: %+v", groups[0])
	}
	if groups[1].Key != "other" || groups[1].Meta != nil || len(groups[1].Results) != 1 {
		t.Errorf("other group: %+v", groups[1])
	}
}
