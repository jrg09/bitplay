package main

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestParseMediaName(t *testing.T) {
	cases := []struct {
		name                  string
		title                 string
		year, season, episode int
	}{
		{"Breaking.Bad.S02E05.720p.HDTV.x264-GRP.mkv", "Breaking Bad", 0, 2, 5},
		{"The.Matrix.1999.1080p.BluRay.x264.mkv", "The Matrix", 1999, 0, 0},
		{"Show 3x07.mkv", "Show", 0, 3, 7},
		{"2001.A.Space.Odyssey.1968.1080p.BluRay.mkv", "2001 A Space Odyssey", 1968, 0, 0},
		{"Breaking Bad/Season 2/Breaking.Bad.S02E05.Breakage.mkv", "Breaking Bad", 0, 2, 5},
		{"Some_Movie_1080p_WEB-DL.mp4", "Some Movie", 0, 0, 0},
	}
	for _, c := range cases {
		got := parseMediaName(c.name)
		if got.Title != c.title || got.Year != c.year || got.Season != c.season || got.Episode != c.episode {
			t.Errorf("parseMediaName(%q) = %+v, want title=%q year=%d S%dE%d",
				c.name, got, c.title, c.year, c.season, c.episode)
		}
	}
}

func TestOpenSubtitlesHash(t *testing.T) {
	const size = 131072
	data := make([]byte, size)
	h, err := openSubtitlesHash(bytes.NewReader(data), size)
	if err != nil || h != "0000000000020000" {
		t.Fatalf("zero file: got %q, %v", h, err)
	}

	// one uint64 in the head chunk and one in the tail chunk are added to the size
	binary.LittleEndian.PutUint64(data[0:], 1)
	binary.LittleEndian.PutUint64(data[size-8:], 0x10)
	h, err = openSubtitlesHash(bytes.NewReader(data), size)
	if err != nil || h != "0000000000020011" {
		t.Fatalf("patterned file: got %q, %v", h, err)
	}

	if _, err := openSubtitlesHash(bytes.NewReader(make([]byte, 1000)), 1000); err == nil {
		t.Fatal("expected error for a file smaller than 128 KiB")
	}
}

func TestParseLanguages(t *testing.T) {
	cases := map[string][]string{
		" ES, en ,es,,": {"es", "en"},
		"ea,pt-br":      {"ea", "pt-br"},
		"spanish,xx_y":  {"es", "ea", "sp", "en"},
		"":              {"es", "ea", "sp", "en"},
	}
	for in, want := range cases {
		if got := parseLanguages(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseLanguages(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEnsureUTF8(t *testing.T) {
	if got := string(ensureUTF8([]byte("canción"))); got != "canción" {
		t.Errorf("valid UTF-8 changed: %q", got)
	}
	latin1 := []byte{'c', 'a', 'n', 'c', 'i', 0xF3, 'n'}
	if got := string(ensureUTF8(latin1)); got != "canción" {
		t.Errorf("Latin-1 not converted: %q", got)
	}
}

func sub(lang string, fileID, downloads int, release string, opts ...func(*osSubtitle)) osSubtitle {
	var s osSubtitle
	s.Attributes.Language = lang
	s.Attributes.DownloadCount = downloads
	s.Attributes.Release = release
	s.Attributes.NbCD = 1
	s.Attributes.Files = append(s.Attributes.Files, struct {
		FileID   int    `json:"file_id"`
		FileName string `json:"file_name"`
	}{FileID: fileID})
	for _, o := range opts {
		o(&s)
	}
	return s
}

func TestPickBest(t *testing.T) {
	file := "Breaking.Bad.S02E05.720p.HDTV.x264-CTU.mkv"
	results := []osSubtitle{
		sub("en", 1, 5000, "Breaking Bad S02E05"),
		sub("es", 2, 100, "Breaking.Bad.S02E05.720p.HDTV.x264-CTU"),
		sub("es", 3, 9000, "Breaking Bad S02E05 WEB", func(s *osSubtitle) { s.Attributes.MachineTranslated = true }),
		sub("es", 4, 99999, "Breaking Bad S02E05 2CD", func(s *osSubtitle) { s.Attributes.NbCD = 2 }),
		sub("en", 5, 10, "whatever", func(s *osSubtitle) { s.Attributes.MovieHashMatch = true }),
	}

	tracks := pickBest(results, []string{"es", "ea", "en"}, file)
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks (es, en), got %+v", tracks)
	}
	if tracks[0].Lang != "es" || tracks[0].ID != "2" || !tracks[0].Default {
		t.Errorf("es track: got %+v, want file 2 as default", tracks[0])
	}
	if tracks[1].Lang != "en" || tracks[1].ID != "5" || tracks[1].Default || !tracks[1].HashMatch {
		t.Errorf("en track: got %+v, want hash-matched file 5", tracks[1])
	}
	if tracks[0].Src != "/api/v1/subtitles/file/2.vtt" {
		t.Errorf("unexpected src %q", tracks[0].Src)
	}

	if got := pickBest(nil, []string{"es"}, file); got != nil {
		t.Errorf("expected no tracks, got %+v", got)
	}
}
