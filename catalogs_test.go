package main

import "testing"

func TestFixMojibake(t *testing.T) {
	cases := map[string]string{
		"2022â€“":    "2022–",
		"RyÃ»nosuke": "Ryûnosuke",
		"Amélie":     "Amélie",
		"The Thing":  "The Thing",
		"Señor Â":    "Señor Â", // not valid once re-encoded: kept
	}
	for in, want := range cases {
		if got := fixMojibake(in); got != want {
			t.Errorf("fixMojibake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseStreamingCatalog(t *testing.T) {
	body := []byte(`{"metas":[
		{"imdb_id":"tt4406178","name":"The Gilded Age","year":"2022â€“","poster":"https://img/a.jpg","imdbRating":"8.1"},
		{"imdb_id":"tt4406178","name":"Duplicate","poster":"https://img/a.jpg"},
		{"imdb_id":"tt1","name":"Bad id","poster":"https://img/b.jpg"},
		{"imdb_id":"tt0084787","name":"No poster"}
	]}`)
	groups, err := parseStreamingCatalog(body, "series")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(groups), groups)
	}
	m := groups[0].Meta
	if m.IMDb != "tt4406178" || m.Type != "series" || m.Year != "2022–" || m.Rating != "8.1" {
		t.Errorf("unexpected meta %+v", m)
	}
	if groups[0].Results == nil {
		t.Error("results should be an empty list, not null")
	}
}

func TestStreamingCatalogURL(t *testing.T) {
	// Paramount+ is not in the addon's manifest but its catalog is served
	if got := streamingServices["pmp"].catalogURL("movie"); got != streamingCatalogBase+"/catalog/movie/pmp.json" {
		t.Errorf("pmp url = %s", got)
	}
	if streamingServices["pmp"].Label != "Paramount+" {
		t.Errorf("pmp label = %q", streamingServices["pmp"].Label)
	}
	if got := streamingServices["hbm"].catalogURL("series"); got != streamingCatalogBase+"/catalog/series/hbm.json" {
		t.Errorf("hbm url = %s", got)
	}
	// base64("nfx::MX:1762000000000:0:1:MX"), '=' escaped
	want := streamingCatalogBase + "/bmZ4OjpNWDoxNzYyMDAwMDAwMDAwOjA6MTpNWA%3D%3D/catalog/movie/netflix-top10-MX.json"
	if got := streamingServices["nfx-top"].catalogURL("movie"); got != want {
		t.Errorf("top10 url = %s, want %s", got, want)
	}
}
