# BitPlay: Torrent Streaming Web App

BitPlay is a web application built with Go that allows you to stream video content directly from torrents in your browser. It features a clean web UI, support for SOCKS5 proxies, and integration with Prowlarr and Jackett for seamless torrent searching.

![Bitplay Home](screenshots/bitplay_home.png)

## Features

*   **Direct Torrent Streaming:** Stream video files from magnet links or torrent files directly without needing to download them completely first.
*   **MKV & Audio Track Support:** MKV files are remuxed on the fly with ffmpeg (no re-encoding), and files with multiple audio tracks get a track picker. If a codec genuinely can't play in your browser, BitPlay tells you which one and gives you the stream URL to open in VLC or mpv instead.
*   **Safari & iOS Playback:** iPhone, iPad and Safari can't play the progressive MP4 used for MKV remuxing, so BitPlay detects WebKit browsers and serves the video as on-demand HLS instead, re-encoding on the fly to H.264 + AAC. Seeking works natively. See [Safari and iOS playback](#safari-and-ios-playback).
*   **Web-Based UI:** Access and control BitPlay through a user-friendly web interface.
*   **Proxy Support:** Configure a SOCKS5 proxy for all torrent-related traffic (fetching metadata, peer connections). (Note: HTTP proxies are not currently supported).
*   **Prowlarr Integration:** Connect to your Prowlarr instance to search across your configured indexers directly within BitPlay.
*   **Jackett Integration:** Connect to your Jackett instance as an alternative search provider.
*   **Subtitles:** Embedded SRT subtitles are converted to VTT on the fly, and you can upload your own `.srt`/`.vtt` file during playback.
*   **Featured & Rich Results:** The home page shows popular, recently added and top movies / TV (YTS and The Pirate Bay) as a poster grid, and search results are grouped by title with poster, year, rating and IMDb link (metadata from Cinemeta, no API key needed).
*   **Continue Watching:** Once a video starts, BitPlay downloads the whole file in the background, remembers where you stopped (on the server, so it follows you across devices) and lists it on the home page. Fully downloaded videos reopen instantly and play without peers. See [Continue watching](#continue-watching).
*   **Built-in Torrent Search:** Search YTS, The Pirate Bay and EZTV directly from the page, no indexer needed. Results are merged with Prowlarr / Jackett (when enabled), de-duplicated by info hash and sorted by seeders. See [Torrent search](#torrent-search).
*   **Automatic Subtitle Search (OpenSubtitles):** When enabled, BitPlay looks up subtitles for the video being played (by OpenSubtitles file hash when the end of the file is available, otherwise by the parsed title / season / episode / year), in the languages you configure (default `es,ea,sp,en`). The best match per language is attached to the player and the first language is shown automatically; the rest stay available in the captions menu. See [Automatic subtitles](#automatic-subtitles).
*   **Optional Basic Auth:** Protect a publicly reachable instance with a username and password via environment variables.
*   **Maintenance Tools:** A settings tab with live session stats, recent server logs, and a cache purge button.
*   **Session Management:** Handles multiple torrent sessions and cleans up inactive ones.

## Getting Started

You can run BitPlay either directly using Go or via Docker Compose.

### Prerequisites

*   **Go:** Requires Go 1.25 or later (if running locally).
*   **ffmpeg:** Optional for local runs; it enables MKV remuxing, audio track selection and Safari/iOS playback (the latter needs an ffmpeg build with `libx264`). The Docker image already includes it.
*   **Docker & Docker Compose:** Required if running with Docker.

### Running Locally with Go

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/aculix/bitplay.git
    cd bitplay
    ```
2.  **Download dependencies:**
    ```bash
    go mod download
    ```
3.  **Run the application:**
    ```bash
    go run main.go
    ```
    By default, the server will start on `http://localhost:3347`.

### Running with Docker Compose

This is the recommended method for deployment.

1.  **Create a `docker-compose.yml` file:**
    ```yaml
    services:
      bitplay:
        image: ghcr.io/aculix/bitplay:latest
        container_name: bitplay
        ports:
          - 3347:3347 # Expose the web UI port
        volumes:
          # Mount the config directory for persistent settings (Optional)
          - ./config:/app/config
          # Mount the download location of the file (Optional)
          - ./torrent-data:/app/torrent-data
        restart: unless-stopped
    ```
    *   **Optional Persistence:** By default, settings (Proxy, Prowlarr/Jackett) are stored inside the container and will be lost if the container is restarted. To make settings persistent across restarts, you can mount a local directory from your host to `/app/config` inside the container using the `volumes` option above. 
    *   If you choose to mount the directory for persistence, you **must** create the directory on your host machine **before** starting the container for the first time: `mkdir -p ./config`. 
    *   If you don't mount this volume, the application will still function correctly, but you will need to re-configure your settings after each container restart. Torrent data itself is always ephemeral.

2.  **Start the container:**
    ```bash
    docker-compose up -d
    ```
3.  **Access the application:** Open your browser to `http://<your-server-ip>:3347`.

### Running with Docker Run

Alternatively, you can run the container directly using `docker run`:

1.  **(Optional) Create the config directory for persistence:** If you want your settings (Proxy, Prowlarr/Jackett) to persist across container restarts, create the configuration directory on your host first:
    ```bash
    mkdir -p ./config
    ```
    If you skip this step and the volume mount below, the application will still work, but settings will be lost on restart.

2.  **Run the container:**
    ```bash
    docker run -d \
      --name bitplay \
      -p 3347:3347 \
      # Add the volume mount below ONLY if you want persistent settings (and created ./config above)
      -v $(pwd)/config:/app/config \
      # The location where the downloaded file is saved
      -v $(pwd)/torrent-data:/app/torrent-data \
      --restart unless-stopped \
      ghcr.io/aculix/bitplay:latest
    ```
    *   `-d`: Run in detached mode (background).
    *   `--name bitplay`: Assign a name to the container.
    *   `-p 3347:3347`: Map port 3347 on the host to port 3347 in the container.
    *   `-v $(pwd)/config:/app/config`: (Optional) Mount the local `./config` directory for persistent settings.
    *   `--restart unless-stopped`: Configure the container to restart automatically unless manually stopped.
    *   `ghcr.io/aculix/bitplay:latest`: The Docker image to use.

3.  **Access the application:** Open your browser to `http://<your-server-ip>:3347`.

## Configuration

BitPlay is configured primarily through its web interface after starting the application.

1.  **Access the Web UI:** Go to `http://localhost:3347` (or your server's address).
2.  **Navigate to Settings:** Find the settings or configuration section within the UI.
3.  **Configure:**
    *   **Proxy:** Enable/disable proxy support and provide the full SOCKS5 proxy URL (e.g., `socks5://user:pass@host:port`). Test the connection using the provided button.
    *   **Prowlarr:** Enable/disable Prowlarr, provide the Prowlarr Host URL (e.g., `http://prowlarr:9696`), and your Prowlarr API Key. Test the connection.
    *   **Jackett:** Enable/disable Jackett, provide the Jackett Host URL (e.g., `http://jackett:9117`), and your Jackett API Key. Test the connection.
    *   **Search Timeout / TLS:** Both indexer tabs let you raise the search timeout (default 30s, useful for Jackett setups with many indexers) and skip TLS certificate verification for self-signed Prowlarr/Jackett instances.
    *   **Maintenance:** The fourth settings tab shows active torrent sessions, recent server logs, and a Purge Cache button that stops all sessions and deletes everything downloaded to disk.

Settings are saved automatically to `/app/config/settings.json` inside the Docker container, which maps to `./config/settings.json` on the host via the mounted volume in the example Docker Compose setup above. API keys are never sent back to the browser once saved.

### Featured titles and posters

*   **Featured** on the home page has four tabs: *Popular* (YTS, most downloaded), *Recently added* (YTS), *Top movies* and *Top TV* (The Pirate Bay top 100 HD). Click a poster to pick one of its torrents. Lists are cached for 30 minutes.
*   Search results are grouped by title: poster, year, rating, genres and IMDb link come from [Cinemeta](https://v3-cinemeta.strem.io) using the IMDb id the source provides, or the title and year parsed from the torrent name (exact title matches only). Torrents that can't be identified are listed under *Other results*. If Cinemeta is unreachable, search still works without posters.

### Continue watching

*   When playback starts, the rest of the file keeps downloading in the background (the part you are watching is still fetched first). The session stays open until the download finishes, even if you close the page; a download that makes no progress for an hour stops holding it open.
*   The playback position is saved every 10 seconds, on pause and when the page is closed, in `config/library.json`. Reopening the video, from any device, resumes there.
*   The home page lists these videos under **Continue watching** with their progress and how much is on disk. *Delete* removes the file from disk.
*   Each torrent's metainfo is saved in `torrent-data/.bitplay-meta/`, so reopening it does not wait for peers to send metadata. A fully downloaded video plays even with no peers at all.
*   A video counts as watched past 95%. Watched videos are deleted after 3 days by default (Settings → Maintenance). *Purge Cache* still wipes everything.

### Torrent search

The search box on the home page queries public sources in parallel, inspired by [torlink](https://github.com/baairon/torlink):

*   **YTS** (movies), **The Pirate Bay** via apibay (movies and TV, video categories only) and **EZTV** (TV). EZTV has no text search, so BitPlay indexes its most recent torrents and then fetches whole shows by IMDb id (also taken from TV results on The Pirate Bay).
*   If Prowlarr and/or Jackett are enabled, their results are merged into the same table.
*   Pick a category (All / Movies / TV) next to the search box and click the *Size* or *Seeders* headers to sort.
*   Settings → **Search** turns the public sources on or off, globally or one by one. They are on by default. Requests go through the SOCKS5 proxy when it is enabled.

Each source has its own deadline (15 s), so a slow site only drops its own results; sources that fail are listed in a warning.

### Automatic subtitles

Open **Settings → Subtitles** and fill in:

*   **API Key:** create a (free) consumer at [opensubtitles.com](https://www.opensubtitles.com/consumers).
*   **Username / Password:** your OpenSubtitles account. The API requires a login to *download* subtitles; searching only needs the API key. The login token is cached in memory for 23 hours.
*   **Languages:** comma-separated codes in priority order, e.g. `es,ea,sp,en`. OpenSubtitles uses `es` (Spanish), `ea` (Spanish, Latin America) and `sp` (Spanish, Spain); most Latin American releases are tagged `es`. The first language that has a result is displayed by default.

Use **Test Connection** to validate the credentials. Notes:

*   Subtitles count against your OpenSubtitles daily download quota. Only the displayed track is downloaded; the other languages are fetched when you select them, and every downloaded subtitle is cached in memory for the life of the process.
*   Ranking prefers hash matches, non machine/AI-translated subtitles, non hearing-impaired ones, trusted uploaders, popular downloads and releases whose name overlaps with the video file name.
*   Credentials are stored in `config/settings.json` (mode `0600`) and are never sent back to the browser.
*   The OpenSubtitles API is reached through the configured SOCKS5 proxy if one is enabled.

### Safari and iOS playback

Every browser on iOS/iPadOS is WebKit, and desktop Safari is too. They report support for codecs they then fail to play, and iOS rejects the progressive fragmented MP4 that BitPlay uses for MKV remuxing (it requires HTTP Range support). BitPlay therefore picks a different path for them:

*   **Detection:** done in the browser (user agent, plus touch points to catch iPadOS, which presents itself as a Mac). Chrome and Firefox on iOS are covered too.
*   **Direct play:** an MP4/M4V with H.264 (Baseline/Main/High, 8-bit) and AAC or MP3 audio is still served as is, with Range support.
*   **HLS otherwise:** `/api/v1/torrent/<session>/hls/<file>/index.m3u8` returns a complete VOD playlist built from the probed duration. Each 6 second MPEG-TS segment (`segN.ts`) is encoded by its own ffmpeg when first requested: H.264 8-bit (`libx264`, `veryfast`) plus stereo AAC, so HEVC, 10-bit, VP9, AV1 and AC3/EAC3/DTS audio all play. `?audio=N` selects an audio track.
*   **Cost:** video is always re-encoded for these clients, so each playback uses CPU on the server (roughly 1 s of ffmpeg per 6 s segment for 1080p on a modern x86 CPU). Encodes share the `BITPLAY_MAX_REMUX` slots and the last 256 MB of segments are cached in memory.
*   **Subtitles:** WebKit uses native text tracks, so subtitles appear in the iOS player's own menu.

### Multiple users (Cloudflare Access)

If BitPlay is published through a Cloudflare Tunnel protected by Cloudflare Access, every visitor can have their own account:

*   **Private history:** "Continue watching", resume positions and series progress are stored per user (by the email Access verified). Torrent files on disk are shared, so something two people watched is downloaded once and only deleted when neither has it any more.
*   **Display name:** shown in the header, derived from the email (`jorge.castro@…` becomes "Jorge Castro") and editable by clicking it.
*   **Admins:** only the emails in `BITPLAY_ADMINS` see Settings, logs and sessions and can change them. Everyone shares those settings (proxy, indexers, OpenSubtitles).
*   **Verification:** BitPlay validates Cloudflare's signed `Cf-Access-Jwt-Assertion` token (signature, issuer, audience, expiry) instead of trusting a header, so it can't be spoofed.
*   **Direct access:** requests that don't come through Cloudflare (for example `http://server:3347` on your LAN, or ffmpeg on loopback) are treated as the **first admin**, with no login. Don't publish the port to the internet except through the tunnel. Anything that did cross Cloudflare but has no valid token is rejected.
*   **Existing history** from before users existed, or recorded while no admin was configured, moves to the first admin the first time the app starts with `BITPLAY_ADMINS` set.

Without `BITPLAY_CF_TEAM_DOMAIN` the app stays single-user, as before.

### Environment Variables

| Variable | Description |
| --- | --- |
| `BITPLAY_AUTH_USERNAME` / `BITPLAY_AUTH_PASSWORD` | Set both to protect the entire app (UI, API, and video streams) with HTTP basic auth. Recommended for instances exposed to the internet. |
| `BITPLAY_CF_TEAM_DOMAIN` / `BITPLAY_CF_AUD` | Turn on per-user accounts: your Access team domain (e.g. `myteam.cloudflareaccess.com`) and the application's AUD tag (Zero Trust → Access → Applications → your app → Overview). Set both. See [Multiple users](#multiple-users-cloudflare-access). |
| `BITPLAY_ADMINS` | Comma separated emails allowed to change settings and see logs. The first one also owns pre-existing history and direct (non-Cloudflare) access. |
| `BITPLAY_MAX_REMUX` | Maximum concurrent ffmpeg jobs: remux streams and Safari/iOS HLS segment encodes (default `3`). |

Example with Docker Compose:

```yaml
services:
  bitplay:
    image: ghcr.io/aculix/bitplay:latest
    ports:
      - 3347:3347
    environment:
      - BITPLAY_AUTH_USERNAME=admin
      - BITPLAY_AUTH_PASSWORD=change-me
    restart: unless-stopped
```

### Routing Through a VPN with Gluetun

If you want all of BitPlay's traffic (not just torrent traffic) forced through a VPN, run it behind [gluetun](https://github.com/qdm12/gluetun) and attach BitPlay to gluetun's network:

```yaml
services:
  gluetun:
    image: qmcgaw/gluetun
    container_name: gluetun
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    ports:
      - 3347:3347 # BitPlay's UI is published via gluetun
    environment:
      - VPN_SERVICE_PROVIDER=your-provider
      # See gluetun's docs for your provider's credentials/variables
    restart: unless-stopped

  bitplay:
    image: ghcr.io/aculix/bitplay:latest
    container_name: bitplay
    network_mode: service:gluetun
    volumes:
      - ./config:/app/config
    restart: unless-stopped
```

With `network_mode: service:gluetun`, everything BitPlay does (tracker announces, peer connections, DHT) goes through the VPN, and you reach the UI through gluetun's published port. The built-in SOCKS5 proxy setting is the lighter option if you only want torrent traffic proxied.

## Usage

1.  **Configure Settings:** Set up your proxy and search providers (Prowlarr/Jackett) as described above.
2.  **Search:** Use the search bar to query Prowlarr or Jackett for torrents.
3.  **Add Torrent:** Paste a magnet link (or a bare info hash) into the search box and it starts playing right away, or search and click a result's *Watch* button.

    > BitPlay also supports opening from another app/program by providing torrent or magnet link as query parameter `torrent`. The link needs to be URL-encoded! 
    >
    > Ex: `http://localhost:3347?torrent=magnet%3A%3Fxt%3Durn%3Abtih%3A08ada5a7a6183aae1e09d831df6748d566095a10%26dn%3DSintel%26tr%3Dudp%253A%252F%252Fexplodie.org%253A6969%26tr%3Dudp%253A%252F%252Ftracker.coppersurfer.tk%253A6969%26tr%3Dudp%253A%252F%252Ftracker.empire-js.us%253A1337%26tr%3Dudp%253A%252F%252Ftracker.leechers-paradise.org%253A6969%26tr%3Dudp%253A%252F%252Ftracker.opentrackr.org%253A1337%26tr%3Dwss%253A%252F%252Ftracker.btorrent.xyz%26tr%3Dwss%253A%252F%252Ftracker.fastcast.nz%26tr%3Dwss%253A%252F%252Ftracker.openwebtorrent.com%26ws%3Dhttps%253A%252F%252Fwebtorrent.io%252Ftorrents%252F%26xs%3Dhttps%253A%252F%252Fwebtorrent.io%252Ftorrents%252Fsintel.torrent`

4.  **Stream:** Once the torrent info is loaded, select the video file you want to watch. BitPlay will start downloading and streaming it directly in the built-in player.

## Contributing

Contributions are welcome! Please feel free to submit pull requests or open issues. (You can add more details here if you have specific contribution guidelines).

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
