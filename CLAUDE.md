# BitPlay

Go web app that streams video from torrents to the browser (anacrolix/torrent). Backend is a single Go package (`module torrent-stream`); the frontend is vanilla JS + video.js + Tailwind served from `client/`.

## Commands

```bash
go run main.go          # serves on http://localhost:3347
go vet ./... && go test ./...
pnpm build              # rebuild client/assets/output.css (Tailwind) after changing classes
docker build -t bitplay . # Alpine image with ffmpeg (needs libx264 for HLS)
```

Tests are plain `go test`, no network or ffmpeg required (ffmpeg arguments are built by pure functions that are unit tested). There is no JS test suite; the client is checked by hand in a browser.

## Layout

| File | Role |
| --- | --- |
| `main.go` | HTTP server, routing, torrent sessions, `/stream`, `/probe`, `/remux`, settings, capabilities |
| `hls.go` | On-demand HLS for Safari/iOS: playlist, per-segment ffmpeg, segment cache |
| `library.go`, `shows.go` | "Continue watching" and per-episode progress (`config/library.json`) |
| `series.go`, `catalogs.go`, `featured.go`, `metadata.go` | Series/episodes, streaming catalogs, home page lists, Cinemeta metadata |
| `torrentsearch.go` | Built-in search over YTS / TPB / EZTV |
| `subtitles.go` | OpenSubtitles lookup |
| `client/assets/index.js` | All UI logic, including playback source selection (`pickSource`) |
| `client/assets/app.css` | Page layout (top bar, rows, poster grid, sheets, player overlay). Plain CSS on the theme variables, no build step |

## Page layout

*   Sticky top bar (logo, search/magnet box, "+" for a `.torrent`, theme, user name, Settings).
*   **Home** (`#home`): "Continue watching" cards, then one poster row per list (`HOME_ROWS` in `index.js`; each row loads when it nears the viewport). **Search** replaces it (`body.bp-searching`) with a poster grid.
*   Clicking a poster opens a **sheet** (`#featured-detail` for torrents, `#series-view` for episodes, `#add-sheet`) over a shared backdrop; Esc or the backdrop closes it. `closeSheets()` closes all of them.
*   Playback runs in the full screen **player overlay** (`#player-overlay`, stage `#player-stage`); "Back" disposes the player (`closePlayerOverlay`). New UI that must appear above the video goes in `#player-stage`.
*   New CSS goes in `app.css` with `bp-*` classes. `output.css` is a prebuilt Tailwind file: a Tailwind utility only works if it is already in it, otherwise rebuild with `pnpm build`.

## Playback pipeline

The server probes (`ffprobe`), the **browser** decides how to play, because only it knows its codecs. `pickSource()` in `index.js` returns one of:

*   `direct`: `/stream/<file>` with HTTP Range (MP4/WebM the browser can decode).
*   `remux`: `/remux/<file>`, ffmpeg stream-copy into progressive fragmented MP4 (MKV, unsupported audio becomes AAC). No Range, so seeks restart ffmpeg with `?t=`. `?transcode=1` re-encodes video to H.264 8-bit.
*   `hls`: `/hls/<file>/index.m3u8`, used on WebKit (Safari, all iOS browsers) when the file is not a plain H.264 8-bit + AAC/MP3 MP4. Full VOD playlist from the probed duration, 6 s MPEG-TS segments each encoded on demand to H.264 + AAC.
*   `unsupported`: shows the "open in VLC" panel.

## Gotchas

*   **iOS rejects the progressive remux stream** (`Accept-Ranges: none`) even when codecs are fine. Do not "fix" Safari by only changing codecs; it needs HLS.
*   `canPlayType` lies on WebKit ("maybe" for HEVC/10-bit), so WebKit detection (`isWebKitOnly`) overrides it. iPadOS reports as a Mac; detect with `maxTouchPoints`.
*   iOS plays in its native player, which only shows **native text tracks**. `nativeTextTracks` is `true` on WebKit and `false` elsewhere (video.js overlay).
*   ffmpeg jobs (remux and HLS segments) share the `remuxSlots` semaphore (`BITPLAY_MAX_REMUX`, default 3). Remux fails fast with 503; HLS segments wait up to 60 s for a slot.
*   ffmpeg/ffprobe read torrent data back through the server's own `/stream` endpoint (`loopbackStreamURL`), so they need `loopbackAuthHeader` when basic auth is on.
*   Request params that reach the ffmpeg argv (`audio`, `t`, segment index) are parsed as numbers and range-checked. Keep it that way.
*   `session.ActiveStreams` must be incremented while ffmpeg runs, or idle cleanup can drop the torrent mid-playback.
*   HEVC copied for Safari needs the `hvc1` tag (`videoArgs`); the default `hev1` is rejected.
*   EAC3 cannot be copied into fragmented MP4; AC3/EAC3/DTS/TrueHD are always re-encoded to stereo AAC.

## Conventions

*   Commit messages: short imperative subject, then a body explaining why.
*   Keep new server logic in small pure functions (arguments, playlists, parsing) so they can be unit tested without ffmpeg.
*   Update `README.md` when behaviour or environment variables change.
