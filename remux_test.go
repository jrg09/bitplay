package main

import (
	"reflect"
	"testing"
)

func TestVideoArgsCopy(t *testing.T) {
	got := videoArgs(false, "h264")
	if !reflect.DeepEqual(got, []string{"-c:v", "copy"}) {
		t.Fatalf("unexpected args: %v", got)
	}
	got = videoArgs(false, "hevc")
	if !reflect.DeepEqual(got, []string{"-c:v", "copy", "-tag:v", "hvc1"}) {
		t.Fatalf("hevc copy must be tagged hvc1: %v", got)
	}
}

func TestVideoArgsTranscode(t *testing.T) {
	for _, codec := range []string{"hevc", "h264", "vp9", "av1"} {
		got := videoArgs(true, codec)
		joined := ""
		for _, a := range got {
			joined += a + " "
		}
		for _, want := range []string{"libx264", "yuv420p", "high"} {
			if !contains(joined, want) {
				t.Fatalf("%s: %q missing from %q", codec, want, joined)
			}
		}
		if contains(joined, "copy") || contains(joined, "hvc1") {
			t.Fatalf("%s: transcode must not copy or tag: %q", codec, joined)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
