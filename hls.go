package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// On-demand HLS for WebKit (Safari, every iOS browser). iOS refuses the
// progressive fragmented MP4 the remux endpoint produces (no Range support),
// but plays HLS natively with seeking. The playlist is a complete VOD
// playlist built from the probed duration; each 6s MPEG-TS segment is
// encoded by its own ffmpeg when first requested, so any seek is instant and
// no state is kept between segments except a small in-memory cache.

const (
	hlsSegmentSeconds = 6.0
	hlsCacheMaxBytes  = 256 << 20
	hlsSlotWait       = 60 * time.Second
	hlsEncodeTimeout  = 2 * time.Minute
)

// hlsPlaylist builds the VOD playlist. audio < 0 means the default track.
func hlsPlaylist(duration float64, audio int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(hlsSegmentSeconds)))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	query := ""
	if audio >= 0 {
		query = "?audio=" + strconv.Itoa(audio)
	}
	count := hlsSegmentCount(duration)
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg%d.ts%s\n", hlsSegmentLength(duration, i), i, query)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func hlsSegmentCount(duration float64) int {
	return int(math.Ceil(duration / hlsSegmentSeconds))
}

// hlsSegmentLength is the length of segment n: 6s, except the last one.
func hlsSegmentLength(duration float64, n int) float64 {
	return math.Min(hlsSegmentSeconds, duration-float64(n)*hlsSegmentSeconds)
}

// parseHLSSegmentName extracts n from "seg<n>.ts".
func parseHLSSegmentName(name string) (int, bool) {
	if !strings.HasPrefix(name, "seg") || !strings.HasSuffix(name, ".ts") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg"), ".ts"))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// hlsSegmentArgs encodes [start, start+length) to H.264 8-bit + stereo AAC in
// MPEG-TS with timestamps continuing from the start offset.
func hlsSegmentArgs(inputURL, authHeader string, start, length float64, audio int) []string {
	args := []string{"-v", "error"}
	if authHeader != "" {
		args = append(args, "-headers", "Authorization: "+authHeader+"\r\n")
	}
	args = append(args,
		"-ss", strconv.FormatFloat(start, 'f', 3, 64),
		"-i", inputURL,
		"-t", strconv.FormatFloat(length, 'f', 3, 64),
		"-map", "0:v:0",
	)
	if audio >= 0 {
		args = append(args, "-map", fmt.Sprintf("0:%d", audio))
	} else {
		args = append(args, "-map", "0:a:0?")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-profile:v", "high", "-level", "4.1", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-ac", "2",
		// start audio exactly with the video so segments don't drift
		"-af", "aresample=async=1:first_pts=0",
		"-sn",
		"-muxdelay", "0", "-muxpreload", "0",
		"-output_ts_offset", strconv.FormatFloat(start, 'f', 3, 64),
		"-f", "mpegts", "pipe:1",
	)
	return args
}

type hlsEntry struct {
	done chan struct{}
	data []byte
	err  error
}

// hlsSegmentCache keeps recently encoded segments and shares one encode
// between concurrent requests for the same segment (players retry and
// prefetch aggressively).
type hlsSegmentCache struct {
	mu      sync.Mutex
	entries map[string]*hlsEntry
	order   []string
	bytes   int
}

var hlsCache = &hlsSegmentCache{entries: map[string]*hlsEntry{}}

// get returns the cached entry for key, or registers a new one and reports
// that the caller must produce it.
func (c *hlsSegmentCache) get(key string) (entry *hlsEntry, create bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		return e, false
	}
	e := &hlsEntry{done: make(chan struct{})}
	c.entries[key] = e
	c.order = append(c.order, key)
	return e, true
}

// finish publishes the result; failed encodes are dropped so they can be retried.
func (c *hlsSegmentCache) finish(key string, e *hlsEntry, data []byte, err error) {
	c.mu.Lock()
	e.data, e.err = data, err
	if err != nil {
		delete(c.entries, key)
		c.removeFromOrder(key)
	} else {
		c.bytes += len(data)
		// evict oldest finished segments (never this one, nor in-flight ones)
		kept := make([]string, 0, len(c.order))
		for _, k := range c.order {
			old := c.entries[k]
			finished := false
			if old != nil {
				select {
				case <-old.done:
					finished = true
				default:
				}
			}
			if c.bytes > hlsCacheMaxBytes && finished && k != key {
				c.bytes -= len(old.data)
				delete(c.entries, k)
				continue
			}
			kept = append(kept, k)
		}
		c.order = kept
	}
	c.mu.Unlock()
	close(e.done)
}

func (c *hlsSegmentCache) removeFromOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func handleHLS(w http.ResponseWriter, r *http.Request, session *TorrentSession, sessionID, indexStr, name string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if !ffmpegAvailable {
		respondWithJSON(w, http.StatusNotImplemented, map[string]string{"error": "ffmpeg is not installed on the server"})
		return
	}
	fileIndex, err := strconv.Atoi(indexStr)
	if err != nil || fileIndex < 0 || fileIndex >= len(session.Torrent.Files()) {
		respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid file index"})
		return
	}
	// params end up in the ffmpeg argv, numeric only
	audio := -1
	if a := r.URL.Query().Get("audio"); a != "" {
		audio, err = strconv.Atoi(a)
		if err != nil || audio < 0 || audio > 512 {
			respondWithJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid audio stream index"})
			return
		}
	}

	duration, err := hlsDuration(r.Context(), session, sessionID, fileIndex)
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("HLS: no duration for session %s file %d: %v", sessionID, fileIndex, err)
		}
		respondWithJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Could not read the video duration yet - retry shortly"})
		return
	}

	if name == "index.m3u8" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, hlsPlaylist(duration, audio))
		return
	}

	n, ok := parseHLSSegmentName(name)
	if !ok || n >= hlsSegmentCount(duration) {
		http.NotFound(w, r)
		return
	}

	key := fmt.Sprintf("%s/%d/%d/%d", sessionID, fileIndex, audio, n)
	entry, create := hlsCache.get(key)
	if create {
		go produceHLSSegment(key, entry, session, sessionID, fileIndex, audio, n, duration)
	}
	select {
	case <-entry.done:
	case <-r.Context().Done():
		return
	}
	if entry.err != nil {
		log.Printf("HLS: segment %d of session %s file %d failed: %v", n, sessionID, fileIndex, entry.err)
		respondWithJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to encode segment"})
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.Itoa(len(entry.data)))
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Write(entry.data)
}

// hlsDuration uses the cached probe, probing first if the client skipped it.
func hlsDuration(ctx context.Context, session *TorrentSession, sessionID string, fileIndex int) (float64, error) {
	session.ProbeMu.Lock()
	probe := session.ProbeCache[fileIndex]
	session.ProbeMu.Unlock()
	if probe == nil {
		var err error
		probe, err = runFFprobe(ctx, loopbackStreamURL(sessionID, fileIndex))
		if err != nil {
			return 0, err
		}
		session.ProbeMu.Lock()
		if session.ProbeCache == nil {
			session.ProbeCache = map[int]*ProbeResult{}
		}
		session.ProbeCache[fileIndex] = probe
		session.ProbeMu.Unlock()
	}
	if probe.Duration <= 0 {
		return 0, fmt.Errorf("unknown duration")
	}
	return probe.Duration, nil
}

// produceHLSSegment runs detached from the request that triggered it so a
// player aborting its request doesn't fail the segment for everyone else.
func produceHLSSegment(key string, entry *hlsEntry, session *TorrentSession, sessionID string, fileIndex, audio, n int, duration float64) {
	ctx, cancel := context.WithTimeout(context.Background(), hlsEncodeTimeout)
	defer cancel()

	select {
	case remuxSlots <- struct{}{}:
		defer func() { <-remuxSlots }()
	case <-time.After(hlsSlotWait):
		hlsCache.finish(key, entry, nil, fmt.Errorf("no free encode slot"))
		return
	}

	atomic.AddInt32(&session.ActiveStreams, 1)
	defer atomic.AddInt32(&session.ActiveStreams, -1)

	start := float64(n) * hlsSegmentSeconds
	args := hlsSegmentArgs(loopbackStreamURL(sessionID, fileIndex), loopbackAuthHeader, start, hlsSegmentLength(duration, n), audio)
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var out bytes.Buffer
	stderr := &limitedWriter{max: 4096}
	cmd.Stdout = &out
	cmd.Stderr = stderr
	began := time.Now()
	if err := cmd.Run(); err != nil {
		hlsCache.finish(key, entry, nil, fmt.Errorf("%v (%s)", err, strings.TrimSpace(stderr.buf.String())))
		return
	}
	log.Printf("HLS: session %s file %d segment %d encoded in %.1fs (%d KB)", sessionID, fileIndex, n, time.Since(began).Seconds(), out.Len()/1024)
	hlsCache.finish(key, entry, out.Bytes(), nil)
}
