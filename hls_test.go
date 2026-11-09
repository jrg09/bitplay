package main

import (
	"strings"
	"testing"
)

func TestHLSPlaylist(t *testing.T) {
	pl := hlsPlaylist(14.5, 3)
	for _, want := range []string{
		"#EXT-X-PLAYLIST-TYPE:VOD", "#EXT-X-ENDLIST",
		"#EXTINF:6.000,\nseg0.ts?audio=3\n",
		"#EXTINF:6.000,\nseg1.ts?audio=3\n",
		"#EXTINF:2.500,\nseg2.ts?audio=3\n",
	} {
		if !strings.Contains(pl, want) {
			t.Fatalf("playlist missing %q:\n%s", want, pl)
		}
	}
	if strings.Contains(pl, "seg3.ts") {
		t.Fatalf("too many segments:\n%s", pl)
	}
}

func TestHLSPlaylistDefaultAudio(t *testing.T) {
	pl := hlsPlaylist(6, -1)
	if !strings.Contains(pl, "seg0.ts\n") || strings.Contains(pl, "audio=") {
		t.Fatalf("unexpected playlist:\n%s", pl)
	}
	if hlsSegmentCount(6) != 1 || hlsSegmentCount(6.001) != 2 {
		t.Fatalf("bad segment count")
	}
}

func TestParseHLSSegmentName(t *testing.T) {
	for name, want := range map[string]int{"seg0.ts": 0, "seg42.ts": 42} {
		if n, ok := parseHLSSegmentName(name); !ok || n != want {
			t.Fatalf("%s: got %d,%v", name, n, ok)
		}
	}
	for _, bad := range []string{"seg.ts", "seg-1.ts", "segx.ts", "seg1.mp4", "../seg1.ts", "index.m3u8"} {
		if _, ok := parseHLSSegmentName(bad); ok {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestHLSSegmentArgs(t *testing.T) {
	args := strings.Join(hlsSegmentArgs("http://x/stream/0", "", 12, 6, 2), " ")
	for _, want := range []string{"-ss 12.000", "-t 6.000", "-map 0:2", "libx264", "yuv420p", "-c:a aac", "-output_ts_offset 12.000", "-f mpegts"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q: %s", want, args)
		}
	}
	if !strings.Contains(strings.Join(hlsSegmentArgs("u", "", 0, 6, -1), " "), "-map 0:a:0?") {
		t.Fatal("default audio map missing")
	}
}

func TestHLSCacheDedupesAndRetriesFailures(t *testing.T) {
	c := &hlsSegmentCache{entries: map[string]*hlsEntry{}}
	e1, create := c.get("a")
	if !create {
		t.Fatal("first get must create")
	}
	if e2, create := c.get("a"); create || e2 != e1 {
		t.Fatal("second get must reuse the in-flight entry")
	}
	c.finish("a", e1, nil, errTest)
	if _, create := c.get("a"); !create {
		t.Fatal("failed segments must be retried")
	}
}

var errTest = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "boom" }
