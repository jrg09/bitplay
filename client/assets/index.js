const getLanguage = (code) => {
  const lang = new Intl.DisplayNames(["en"], { type: "language" });
  return lang.of(code);
};

let settings = {
  enableProxy: false,
  proxyUrl: "",
  enableProwlarr: false,
  prowlarrHost: "",
  prowlarrApiKey: "",
  enableJackett: false,
  jackettHost: "",
  jackettApiKey: "",
};

const searchWrapper = document.querySelector("#search-wrapper");
var player = null;

function doubleTapFF(options) {
	var videoElement = this
	var videoElementId = this.id();
	document.getElementById(videoElementId).addEventListener("touchstart", tapHandler);
	var tapedTwice = false;
	function tapHandler(e) {
		if (!videoElement.paused()) {

			if (!tapedTwice) {
				tapedTwice = true;
				setTimeout(function () {
					tapedTwice = false;
				}, 300);
				return false;
			}
			e.preventDefault();
			var br = document.getElementById(videoElementId).getBoundingClientRect();


			var x = e.touches[0].clientX - br.left;
			var y = e.touches[0].clientY - br.top;

			if (x <= br.width / 2) {
				videoElement.currentTime(player.currentTime() - 10)
			} else {
				videoElement.currentTime(player.currentTime() + 10)

			}
		}


	}
}
videojs.registerPlugin('doubleTapFF', doubleTapFF);

(async function ($) {
  // toggle dark mode button
  const toggleDarkMode = () => {
    const html = document.querySelector("html");
    html.classList.toggle("dark");
    localStorage.setItem(
      "theme",
      html.classList.contains("dark") ? "dark" : "light"
    );
  };
  const toggleDarkModeButton = document.querySelector("#toggle_theme");
  toggleDarkModeButton.addEventListener("click", toggleDarkMode);

  // handle past button
  const pastButton = document.querySelector("#copy_magnet");
  pastButton.addEventListener("click", async () => {
    navigator.clipboard.readText().then((text) => {
      document.getElementById("magnet").value = text;
    });
  });

  // A magnet link or a bare info hash typed/pasted in the search box is
  // played instead of searched
  const asMagnet = (text) => {
    const t = (text || "").trim();
    if (/^magnet:\?/i.test(t)) {
      return t;
    }
    if (/^[0-9a-f]{40}$/i.test(t) || /^[a-z2-7]{32}$/i.test(t)) {
      return "magnet:?xt=urn:btih:" + t;
    }
    return null;
  };

  // pasting a magnet starts playback right away
  document.querySelector("#search").addEventListener("paste", (e) => {
    const pasted = e.clipboardData?.getData("text") || "";
    if (asMagnet(pasted)) {
      // let the paste land in the box, then submit through the normal
      // (cancelable) path
      setTimeout(() => {
        e.target.value = pasted.trim(); // replace whatever was typed before
        document.querySelector("#search-form").requestSubmit();
      }, 0);
    }
  });

  const form = document.querySelector("#torrent-form");
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    startPlayback();
  });

  // Playback compatibility: the server probes and remuxes, the browser
  // decides direct vs remux vs unsupported since only it knows its codecs
  const MIME_BY_EXT = {
    mp4: "video/mp4",
    m4v: "video/mp4",
    webm: "video/webm",
    mkv: "video/x-matroska",
    avi: "video/x-msvideo",
  };
  const guessMime = (name) =>
    MIME_BY_EXT[name.split(".").pop().toLowerCase()] || "video/mp4";

  let capabilitiesCache = null;
  const getCapabilities = async () => {
    if (!capabilitiesCache) {
      capabilitiesCache = await fetch("/api/v1/capabilities")
        .then((r) => r.json())
        .catch(() => ({ ffmpeg: false }));
    }
    return capabilitiesCache;
  };

  // codecs missing here are treated as undecodable
  const CODEC_TYPES = {
    h264: 'video/mp4; codecs="avc1.640029"',
    hevc: 'video/mp4; codecs="hvc1.1.6.L123.B0"',
    av1: 'video/mp4; codecs="av01.0.08M.08"',
    vp8: 'video/webm; codecs="vp8"',
    vp9: 'video/webm; codecs="vp09.00.40.08"',
    aac: 'audio/mp4; codecs="mp4a.40.2"',
    mp3: "audio/mpeg",
    flac: 'audio/mp4; codecs="flac"',
    opus: 'audio/webm; codecs="opus"',
    vorbis: 'audio/webm; codecs="vorbis"',
    ac3: 'audio/mp4; codecs="ac-3"',
    eac3: 'audio/mp4; codecs="ec-3"',
  };
  const codecProbeEl = document.createElement("video");
  const canDecode = (codec) => {
    const type = CODEC_TYPES[codec];
    return type ? codecProbeEl.canPlayType(type) !== "" : false;
  };

  const pickSource = async (sessionId, file) => {
    const directUrl = "/api/v1/torrent/" + sessionId + "/stream/" + file.index;
    const ext = file.name.split(".").pop().toLowerCase();
    const fallback = {
      mode: "direct",
      src: directUrl,
      type: guessMime(file.name),
      directUrl,
      probe: null,
    };

    const caps = await getCapabilities();
    if (!caps.ffmpeg) {
      return fallback;
    }

    let probe = null;
    for (let attempt = 0; attempt < 2; attempt++) {
      const res = await fetch(
        `/api/v1/torrent/${sessionId}/probe/${file.index}`
      ).catch(() => null);
      if (res && res.ok) {
        probe = await res.json();
        break;
      }
      if (!res || res.status !== 504) {
        break;
      }
    }
    if (!probe) {
      return fallback;
    }

    const video = probe.streams.find((s) => s.type === "video");
    const audios = probe.streams.filter((s) => s.type === "audio");
    const defaultAudio = audios.find((a) => a.default) || audios[0];
    if (!video) {
      return fallback;
    }

    const videoOk = canDecode(video.codec);
    const audioOk = !defaultAudio || canDecode(defaultAudio.codec);
    const nativeContainer = ext === "mp4" || ext === "m4v" || ext === "webm";

    if (!videoOk) {
      return { mode: "unsupported", probe, video, directUrl };
    }
    if (nativeContainer && audioOk) {
      return { ...fallback, probe };
    }

    const remuxUrl = (opts = {}) => {
      const params = new URLSearchParams();
      if (opts.audio != null) params.set("audio", opts.audio);
      if (opts.t) params.set("t", opts.t.toFixed(3));
      const qs = params.toString();
      return (
        "/api/v1/torrent/" +
        sessionId +
        "/remux/" +
        file.index +
        (qs ? "?" + qs : "")
      );
    };
    const currentAudio = defaultAudio ? defaultAudio.index : null;
    return {
      mode: "remux",
      src: remuxUrl({ audio: currentAudio }),
      type: "video/mp4",
      probe,
      video,
      audios,
      remuxUrl,
      directUrl,
      currentAudio,
      audioWarning: !audioOk,
      audioCodec: defaultAudio ? defaultAudio.codec : null,
    };
  };

  const removeExternalPanel = () => {
    const panel = document.querySelector("#external-player-panel");
    if (panel) {
      panel.remove();
    }
  };

  const showExternalPanel = (chosen) => {
    removeExternalPanel();
    const absUrl = new URL(chosen.directUrl, location.href).href;

    const panel = document.createElement("div");
    panel.id = "external-player-panel";
    panel.className = "w-full mt-6 border rounded-lg p-4 flex flex-col gap-3";

    const msg = document.createElement("p");
    msg.className = "text-sm text-muted-foreground";
    const codec = chosen.video ? chosen.video.codec.toUpperCase() : "an unknown codec";
    msg.textContent =
      `This video is ${codec}` +
      `${chosen.video && chosen.video.profile ? " (" + chosen.video.profile + ")" : ""}` +
      ", which this browser can't decode. Use an external player like VLC or mpv.";

    const row = document.createElement("div");
    row.className = "flex flex-wrap gap-2";

    const copyBtn = document.createElement("button");
    copyBtn.type = "button";
    copyBtn.className = "btn small";
    copyBtn.textContent = "Copy stream URL";
    copyBtn.addEventListener("click", () => {
      navigator.clipboard.writeText(absUrl).then(() => {
        butterup.toast({
          message: "Stream URL copied. In VLC: Media > Open Network Stream",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "success",
        });
      });
    });

    const vlcLink = document.createElement("a");
    vlcLink.className = "btn small";
    vlcLink.textContent = "Open in VLC";
    vlcLink.href = "vlc://" + absUrl;

    row.append(copyBtn, vlcLink);
    panel.append(msg, row);
    document.querySelector("main").appendChild(panel);
    panel.scrollIntoView({ behavior: "smooth" });
  };

  // Remux streams can't byte-range seek; out-of-buffer seeks restart
  // ffmpeg at the target time
  const attachRemuxPlayback = (chosen) => {
    if (!player.__origCurrentTime) {
      player.__origCurrentTime = player.currentTime.bind(player);
      player.__origDuration = player.duration.bind(player);
    }
    let offset = 0;
    let seekTimer = null;
    const total = (chosen.probe && chosen.probe.duration) || 0;

    const reload = (t) => {
      offset = t || 0;
      player.src({
        src: chosen.remuxUrl({
          audio: chosen.currentAudio,
          t: offset || undefined,
        }),
        type: "video/mp4",
      });
      player.one("loadedmetadata", () => player.trigger("durationchange"));
      player.play();
    };
    chosen.reload = reload;
    chosen.getAbsoluteTime = () => offset + (player.__origCurrentTime() || 0);

    player.duration = () => total || player.__origDuration();
    player.currentTime = (t) => {
      if (t === undefined) {
        return offset + (player.__origCurrentTime() || 0);
      }
      const target = Math.max(0, total ? Math.min(t, total - 0.5) : t);
      const local = target - offset;
      const buffered = player.buffered();
      const bufferedEnd =
        buffered && buffered.length ? buffered.end(buffered.length - 1) : 0;
      if (local >= 0 && local <= bufferedEnd) {
        player.__origCurrentTime(local);
      } else {
        clearTimeout(seekTimer);
        seekTimer = setTimeout(() => reload(target), 400);
      }
      return player;
    };
  };

  const buildAudioSelect = (chosen) => {
    const select = document.createElement("select");
    select.id = "audio-select";
    select.className = "video-select audio-select";
    select.setAttribute("aria-label", "Select audio track");
    chosen.audios.forEach((a) => {
      const option = document.createElement("option");
      option.value = String(a.index);
      const lang =
        a.language && a.language !== "und" ? getLanguage(a.language) : null;
      const name = a.title || lang || `Track ${a.index}`;
      const detail = a.codec + (a.channels ? ` ${a.channels}ch` : "");
      option.textContent = `${name} (${detail})`;
      select.appendChild(option);
    });
    select.value = String(chosen.currentAudio);
    select.addEventListener("change", () => {
      const resumeAt = chosen.getAbsoluteTime ? chosen.getAbsoluteTime() : 0;
      chosen.currentAudio = parseInt(select.value, 10);
      if (chosen.reload) {
        chosen.reload(resumeAt);
      }
    });
    document.querySelector("#video-player").appendChild(select);
  };

  const setupChosenPlayback = (chosen) => {
    const oldAudioSelect = document.querySelector("#audio-select");
    if (oldAudioSelect) {
      oldAudioSelect.remove();
    }
    removeExternalPanel();

    if (chosen.mode === "remux") {
      attachRemuxPlayback(chosen);
      if (chosen.audios && chosen.audios.length > 1) {
        buildAudioSelect(chosen);
      }
      if (chosen.audioWarning) {
        butterup.toast({
          message: `The ${(chosen.audioCodec || "").toUpperCase()} audio track can't be decoded by this browser, so the video may play silently. Use an external player for sound.`,
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "info",
        });
      }
    } else if (player.__origCurrentTime) {
      // back on a direct source, restore the original methods
      delete player.currentTime;
      delete player.duration;
    }
  };

  // Automatic subtitles (OpenSubtitles). Tracks are added as remote text
  // tracks; the server only downloads a subtitle when its track is loaded.
  let autoSubtitleState = { player: null, tracks: [] };

  const clearAutoSubtitles = () => {
    if (player && autoSubtitleState.player === player) {
      autoSubtitleState.tracks.forEach((el) => {
        try {
          player.removeRemoteTextTrack(el);
        } catch (err) {
          console.warn("Could not remove subtitle track:", err);
        }
      });
    }
    autoSubtitleState = { player: null, tracks: [] };
  };

  const autoLoadSubtitles = async (sessionId, file) => {
    if (!settings?.enableSubtitles || !settings?.openSubtitlesApiKeySet) {
      return;
    }
    const currentPlayer = player;
    if (!currentPlayer) {
      return;
    }
    clearAutoSubtitles();

    try {
      const res = await fetch(
        "/api/v1/subtitles/auto?session=" +
          encodeURIComponent(sessionId) +
          "&file=" +
          file.index
      );
      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || "Subtitle search failed");
      }
      // the user may have started another video while we were searching
      if (player !== currentPlayer) {
        return;
      }

      if (!data.tracks || !data.tracks.length) {
        butterup.toast({
          message: "No subtitles found for this video",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "info",
        });
        return;
      }

      // Download the default track first so failures (missing credentials,
      // daily quota reached, ...) are reported instead of a silent empty
      // track. The server caches it, so the player's own request is free.
      const first = data.tracks.find((t) => t.default) || data.tracks[0];
      const probe = await fetch(first.src);
      if (!probe.ok) {
        const text = (await probe.text()).trim();
        throw new Error(text || "Could not download subtitle");
      }
      if (player !== currentPlayer) {
        return;
      }

      const added = [];
      let defaultEl = null;
      data.tracks.forEach((t) => {
        const el = currentPlayer.addRemoteTextTrack(
          {
            kind: "subtitles",
            src: t.src,
            srclang: t.lang,
            label: t.label + " (OpenSubtitles)",
          },
          false
        );
        added.push(el);
        if (t.default) {
          defaultEl = el;
        }
      });
      autoSubtitleState = { player: currentPlayer, tracks: added };

      // show the best match, keep the rest available in the captions menu
      const tracks = currentPlayer.textTracks();
      for (let i = 0; i < tracks.length; i++) {
        if (!added.some((el) => el.track === tracks[i])) {
          continue;
        }
        tracks[i].mode =
          defaultEl && tracks[i] === defaultEl.track ? "showing" : "disabled";
      }

      butterup.toast({
        message: `Subtitles loaded: ${data.tracks[0].label}`,
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "success",
      });
    } catch (err) {
      console.error("Automatic subtitles failed:", err);
      butterup.toast({
        message: err.message || "Could not fetch subtitles",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
    }
  };

  // Never dispatch synthetic submit events at this form: they're
  // non-cancelable, so Firefox runs a real submission and reloads the page
  // "Continue watching" (server side: library.go). While a video plays we
  // report the position every few seconds; the first report also makes the
  // server download the whole file in the background. On the next visit the
  // saved position is restored and the video is listed on the home page.
  const PROGRESS_INTERVAL_MS = 10000;
  const RESUME_MIN_SECONDS = 30;
  let playbackTracker = null;

  const formatClock = (seconds) => {
    const s = Math.max(0, Math.floor(seconds || 0));
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = String(s % 60).padStart(2, "0");
    return h ? `${h}:${String(m).padStart(2, "0")}:${sec}` : `${m}:${sec}`;
  };

  const downloadStatusEl = () => {
    let el = document.querySelector("#download-status");
    if (!el) {
      el = document.createElement("p");
      el.id = "download-status";
      el.className = "w-full text-sm text-muted-foreground mt-4";
    }
    // the player is re-created per playback, keep this right under it
    document.querySelector("main").appendChild(el);
    return el;
  };

  const stopTracking = () => {
    if (playbackTracker) {
      playbackTracker.stop();
      playbackTracker = null;
    }
  };

  const trackPlayback = async (sessionId, file) => {
    stopTracking();
    const currentPlayer = player;
    if (!currentPlayer) {
      return;
    }
    const statusEl = downloadStatusEl();
    statusEl.textContent = "";
    let stopped = false;
    // until the saved position has been restored, report -1 so the server
    // keeps it instead of overwriting it with 0
    let resumed = false;
    let sawIncomplete = false;
    let listRefreshed = false;

    const send = () => {
      if (stopped || player !== currentPlayer) {
        return;
      }
      let position = -1;
      let duration = 0;
      try {
        if (resumed) {
          position = currentPlayer.currentTime() || 0;
        }
        const d = currentPlayer.duration();
        duration = Number.isFinite(d) ? d : 0;
      } catch (err) {
        return; // player disposed
      }
      fetch("/api/v1/library/progress", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          session: sessionId,
          file: file.index,
          position,
          duration,
        }),
        keepalive: true, // still delivered when the page is closing
      })
        .then((r) => (r.ok ? r.json() : null))
        .then((d) => {
          if (!d || stopped) {
            return;
          }
          const pct = d.size ? Math.floor((d.downloaded / d.size) * 100) : 0;
          statusEl.textContent = d.complete
            ? "Fully downloaded: you can close this page and resume later, even without peers."
            : `Downloading the whole video in the background: ${pct}%. You can close this page and resume later.`;
          if (!d.complete) {
            sawIncomplete = true;
          } else if (sawIncomplete) {
            sawIncomplete = false;
            butterup.toast({
              message: "Video fully downloaded",
              location: "top-right",
              icon: true,
              dismissable: true,
              type: "success",
            });
          }
          if (!listRefreshed) {
            listRefreshed = true;
            renderLibrary();
          }
        })
        .catch(() => {});
    };

    const timer = setInterval(() => {
      try {
        if (!currentPlayer.paused()) {
          send();
        }
      } catch (err) {
        // player disposed
      }
    }, PROGRESS_INTERVAL_MS);
    const onHidden = () => {
      if (document.visibilityState === "hidden") {
        send();
      }
    };
    document.addEventListener("visibilitychange", onHidden);
    window.addEventListener("pagehide", send);
    currentPlayer.on("pause", send);
    currentPlayer.on("ended", send);
    currentPlayer.one("playing", send); // starts the background download

    playbackTracker = {
      stop: () => {
        send(); // last position before switching video
        stopped = true;
        clearInterval(timer);
        document.removeEventListener("visibilitychange", onHidden);
        window.removeEventListener("pagehide", send);
        try {
          currentPlayer.off("pause", send);
          currentPlayer.off("ended", send);
        } catch (err) {
          // player disposed
        }
      },
    };

    // restore the saved position
    try {
      const res = await fetch(
        `/api/v1/library/entry?session=${encodeURIComponent(sessionId)}&file=${file.index}`
      );
      const entry = res.ok ? await res.json() : null;
      const pos = entry?.position || 0;
      const dur = entry?.duration || 0;
      const nearEnd = dur && pos > dur * 0.95;
      if (
        !stopped &&
        player === currentPlayer &&
        pos > RESUME_MIN_SECONDS &&
        !nearEnd
      ) {
        const seek = () => {
          if (stopped) {
            return;
          }
          currentPlayer.currentTime(pos);
          resumed = true;
          butterup.toast({
            message: `Resuming at ${formatClock(pos)}`,
            location: "top-right",
            icon: true,
            dismissable: true,
            type: "info",
          });
        };
        if (currentPlayer.readyState() >= 1) {
          seek();
        } else {
          currentPlayer.one("loadedmetadata", seek);
        }
        return;
      }
    } catch (err) {
      console.warn("Could not load the saved position:", err);
    }
    resumed = true;
  };

  const renderLibrary = async () => {
    const wrapper = document.querySelector("#library-wrapper");
    const list = document.querySelector("#library-list");
    let items = [];
    try {
      const res = await fetch("/api/v1/library");
      items = res.ok ? await res.json() : [];
    } catch (err) {
      items = [];
    }
    list.innerHTML = "";
    if (!Array.isArray(items) || !items.length) {
      wrapper.classList.add("hidden");
      return;
    }
    wrapper.classList.remove("hidden");

    items.forEach((it, i) => {
      // names come from torrents: untrusted, so textContent only
      const row = document.createElement("div");
      row.className =
        "flex items-center gap-3 p-3" + (i < items.length - 1 ? " border-b" : "");

      const info = document.createElement("div");
      info.className = "flex flex-col gap-1 flex-1 min-w-0";
      const title = document.createElement("span");
      title.className = "text-sm font-semibold";
      title.style.cssText =
        "overflow:hidden;text-overflow:ellipsis;white-space:nowrap";
      title.textContent = it.fileName || it.title;
      title.title = it.title;

      const pct = it.size ? Math.floor((it.downloaded / it.size) * 100) : 0;
      const parts = [];
      if (it.watched) {
        parts.push("Watched");
      } else if (it.duration) {
        parts.push(`${formatClock(it.position)} / ${formatClock(it.duration)}`);
      }
      if (pct >= 100) {
        parts.push("Downloaded");
      } else if (it.active) {
        parts.push(`Downloading ${pct}%`);
      } else {
        parts.push(pct ? `${pct}% on disk` : "Not on disk");
      }
      const meta = document.createElement("span");
      meta.className = "text-xs text-muted-foreground";
      meta.textContent = parts.join(" · ");

      const bar = document.createElement("div");
      bar.className = "bg-muted rounded-full overflow-hidden";
      bar.style.height = "4px";
      const fill = document.createElement("div");
      fill.className = "bg-primary";
      fill.style.height = "100%";
      fill.style.width =
        (it.duration ? Math.min(100, (it.position / it.duration) * 100) : 0) +
        "%";
      bar.appendChild(fill);
      info.append(title, meta, bar);

      const playBtn = document.createElement("button");
      playBtn.type = "button";
      playBtn.className = "btn small shrink-0";
      playBtn.textContent = it.watched ? "Watch again" : "Continue";
      playBtn.addEventListener("click", () => {
        document.querySelector("#magnet").value = it.magnet;
        startPlayback({ fileIndex: it.fileIndex });
      });

      // two clicks to delete, no browser confirm() dialog
      const delBtn = document.createElement("button");
      delBtn.type = "button";
      delBtn.className = "text-sm text-primary cursor-pointer shrink-0";
      delBtn.textContent = "Delete";
      let armed = null;
      delBtn.addEventListener("click", async () => {
        if (!armed) {
          delBtn.textContent = "Sure?";
          armed = setTimeout(() => {
            armed = null;
            delBtn.textContent = "Delete";
          }, 3000);
          return;
        }
        clearTimeout(armed);
        const res = await fetch("/api/v1/library/delete", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ infoHash: it.infoHash, fileIndex: it.fileIndex }),
        }).catch(() => null);
        butterup.toast({
          message: res && res.ok ? "Deleted from disk" : "Could not delete",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: res && res.ok ? "success" : "error",
        });
        renderLibrary();
      });

      row.append(info, playBtn, delBtn);
      list.appendChild(row);
    });
  };
  renderLibrary();

  document
    .querySelector("#save-library-settings")
    .addEventListener("click", async () => {
      const days = parseInt(
        document.querySelector("#keepWatchedDays").value,
        10
      );
      const res = await fetch("/api/v1/settings/library", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ keepWatchedDays: days }),
      }).catch(() => null);
      const data = res ? await res.json().catch(() => ({})) : {};
      butterup.toast({
        message:
          res && res.ok ? "Library settings saved" : data.error || "Could not save",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: res && res.ok ? "success" : "error",
      });
    });

  // opts.fileIndex picks the video to open (used by "Continue watching");
  // opts.episode = {imdb, title, season, episode} picks that episode's file
  // (needed for season packs)
  async function startPlayback(opts = {}) {
    const magnet = document.querySelector("#magnet").value;

    if (!magnet) {
      butterup.toast({
        message: "Please enter a magnet link",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return;
    }

    // clean up previous player
    if (player) {
      stopTracking();
      player.dispose();
      player = null;
      const vidElm = document.createElement("video");
      vidElm.setAttribute("id", "video-player");
      vidElm.setAttribute("class", "video-js mt-10 w-full");

      document.querySelector("main").appendChild(vidElm);
    }

    form
      .querySelector("button[type=submit]")
      .setAttribute("disabled", "disabled");
    form.querySelector("button[type=submit]").innerHTML = "";
    form.querySelector("button[type=submit]").classList.add("loader");

    const res = await fetch("/api/v1/torrent/add", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ magnet }),
    });

    if (!res.ok) {
      const err = await res.json();
      butterup.toast({
        message: err.error || "Something went wrong",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      form.querySelector("button[type=submit]").removeAttribute("disabled");
      form.querySelector("button[type=submit]").innerHTML = "Play Now";
      form.querySelector("button[type=submit]").classList.remove("loader");
      document.querySelectorAll(".play-torrent").forEach((el) => {
        el.removeAttribute("disabled");
        el.innerHTML = "Watch";
        el.classList.remove("loader");
      });
      return;
    }

    const { sessionId } = await res.json();
    const filesRes = await fetch("/api/v1/torrent/" + sessionId);

    if (!filesRes.ok) {
      const err = await filesRes.json();
      butterup.toast({
        message: err.error || "Something went wrong",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      form.querySelector("button[type=submit]").removeAttribute("disabled");
      form.querySelector("button[type=submit]").innerHTML = "Play Now";
      form.querySelector("button[type=submit]").classList.remove("loader");
      document.querySelectorAll(".play-torrent").forEach((el) => {
        el.removeAttribute("disabled");
        el.innerHTML = "Watch";
        el.classList.remove("loader");
      });
      return;
    }

    const files = await filesRes.json();

    // Find video file
    const videoFiles = files.filter((f) =>
      f.name.match(/\.(mp4|mkv|webm|avi)$/i)
    );

    if (!videoFiles.length) {
      butterup.toast({
        message: "No video file found",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      form.querySelector("button[type=submit]").removeAttribute("disabled");
      form.querySelector("button[type=submit]").innerHTML = "Play Now";
      form.querySelector("button[type=submit]").classList.remove("loader");
      document.querySelectorAll(".play-torrent").forEach((el) => {
        el.removeAttribute("disabled");
        el.innerHTML = "Watch";
        el.classList.remove("loader");
      });
      return;
    }

    let initialFile =
      videoFiles.find((f) => f.index === opts.fileIndex) || videoFiles[0];
    if (opts.episode) {
      const { season, episode } = opts.episode;
      const match = findEpisodeFile(videoFiles, season, episode);
      if (!match) {
        butterup.toast({
          message: `${episodeCode(season, episode)} was not found in this torrent, try another one`,
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
        form.querySelector("button[type=submit]").removeAttribute("disabled");
        form.querySelector("button[type=submit]").innerHTML = "Play Now";
        form.querySelector("button[type=submit]").classList.remove("loader");
        document.querySelectorAll(".play-torrent").forEach((el) => {
          el.removeAttribute("disabled");
          el.innerHTML = "Watch";
          el.classList.remove("loader");
        });
        return;
      }
      initialFile = match;
    }

    const subtitleFiles = files.filter((f) =>
      f.name.match(/\.(srt|vtt|sub)$/i)
    );

    let subtitles = [];
    if (subtitleFiles.length) {
      subtitles = subtitleFiles.map((subFile) => {
        let language = "en";
        let langName = "English";

        // Try to extract language code from filename
        const langMatch = subFile.name.match(/\.([a-z]{2,3})\.(srt|vtt|sub)$/i);
        if (langMatch) {
          language = langMatch[1];
          langName = getLanguage(language);
        }

        return {
          src:
            "/api/v1/torrent/" +
            sessionId +
            "/stream/" +
            subFile.index +
            ".vtt?format=vtt",
          srclang: language,
          label: langName,
          kind: "subtitles",
          type: "vtt",
        };
      });
    }
    const chosen = await pickSource(sessionId, initialFile);

    if (chosen.mode === "unsupported") {
      showExternalPanel(chosen);
      form.querySelector("button[type=submit]").removeAttribute("disabled");
      form.querySelector("button[type=submit]").innerHTML = "Play Now";
      form.querySelector("button[type=submit]").classList.remove("loader");
      document.querySelectorAll(".play-torrent").forEach((el) => {
        el.removeAttribute("disabled");
        el.innerHTML = "Watch";
        el.classList.remove("loader");
      });
      return;
    }

    player = videojs(
      "video-player",
      {
        fluid: true,
        controls: true,
        autoplay: true,
        preload: "auto",
        sources: [{
          src: chosen.src,
          type: chosen.type,
          label: initialFile.name,
        }],
        tracks: subtitles,
        html5: {
          nativeTextTracks: false
        },
        plugins: {
          hotkeys: {
            volumeStep: 0.1,
            seekStep: 5,
            enableModifiersForNumbers: false,
            enableVolumeScroll: false,
          },
        },
      },
      function () {
        player = this;
        player.on("error", () => {
          const mediaError = player.error();
          console.error(mediaError);
          const codecName = chosen.video
            ? chosen.video.codec.toUpperCase() + " video"
            : "this video's codec";
          const messages = {
            1: "Video loading was aborted",
            2: "Network error while fetching the video stream",
            3: `Your browser could not decode ${codecName}`,
            4: "This video format is not supported by your browser",
          };
          butterup.toast({
            message:
              (mediaError && messages[mediaError.code]) ||
              mediaError?.message ||
              "Something went wrong",
            location: "top-right",
            icon: true,
            dismissable: true,
            type: "error",
          });
          if (mediaError && (mediaError.code === 3 || mediaError.code === 4)) {
            showExternalPanel(chosen);
          }
        });
      }
    );
    player.doubleTapFF();
    setupChosenPlayback(chosen);
    // fire and forget: playback must not wait for the subtitle search
    autoLoadSubtitles(sessionId, initialFile);
    trackPlayback(sessionId, initialFile);

    document.querySelector("#video-player").style.display = "block";

    // the video element is re-created per playback, move the control with it
    const subtitleUploadWrapper = document.querySelector(
      "#subtitle-upload-wrapper"
    );
    document.querySelector("main").appendChild(subtitleUploadWrapper);
    subtitleUploadWrapper.classList.remove("hidden");
    // scroll to video player
    setTimeout(() => {
      window.scrollTo({
        top: document.body.scrollHeight,
        behavior: "smooth",
      });

      if (videoFiles.length > 1) {
        const videoSelect = document.createElement("select");
        videoSelect.setAttribute("id", "video-select");
        videoSelect.setAttribute("class", "video-select");
        videoSelect.setAttribute("aria-label", "Select video");
        videoFiles.forEach((file) => {
          const option = document.createElement("option");
          option.setAttribute("value", String(file.index));
          option.textContent = file.name;
          videoSelect.appendChild(option);
        });
        videoSelect.value = String(initialFile.index);
        videoSelect.addEventListener("change", async (e) => {
          const file = videoFiles.find(
            (f) => f.index === parseInt(e.target.value, 10)
          );
          const next = await pickSource(sessionId, file);
          if (next.mode === "unsupported") {
            player.pause();
            showExternalPanel(next);
            return;
          }
          stopTracking();
          player.src({ src: next.src, type: next.type });
          setupChosenPlayback(next);
          autoLoadSubtitles(sessionId, file);
          trackPlayback(sessionId, file);
          player.play();
        });
        document.querySelector("#video-player").appendChild(videoSelect);
      }
      player.play()
    }, 300);

    form.querySelector("button[type=submit]").removeAttribute("disabled");
    form.querySelector("button[type=submit]").innerHTML = "Play Now";
    form.querySelector("button[type=submit]").classList.remove("loader");
    document.querySelectorAll(".play-torrent").forEach((el) => {
      el.removeAttribute("disabled");
      el.innerHTML = "Watch";
      el.classList.remove("loader");
    });
  }

  // create switch button
  const switchInputs = document.querySelectorAll(".switchInput");
  switchInputs.forEach((input) => {
    input.querySelector("input").addEventListener("change", (e) => {
      const dot = e.target.parentElement.querySelector(".dot");
      const wrapper = e.target.parentElement.querySelector(".switch-wrapper");
      if (e.target.checked) {
        dot.classList.add("translate-x-full", "!bg-muted");
        wrapper.classList.add("bg-primary");
      } else {
        dot.classList.remove("translate-x-full", "!bg-muted");
        wrapper.classList.remove("bg-primary");
      }
    });
  });

  document.querySelector("#settings-btn").addEventListener("click", () => {
    document.querySelector("#settings-model").classList.toggle("hidden");
  });

  document.querySelectorAll(".close-settings").forEach((el) => {
    el.addEventListener("click", () => {
      document.querySelector("#settings-model").classList.toggle("hidden");
      document.querySelector("#proxy-result").classList.remove("flex");
    document.querySelector("#proxy-result").classList.add("hidden");
    });
  });

  document.querySelectorAll(".tab-btn").forEach((el) => {
    el.addEventListener("click", () => {
      const tabIndex = el.getAttribute("data-index");
      document.querySelectorAll(".tab").forEach((tab) => {
        const index = tab.getAttribute("data-tab");
        if (index === tabIndex) {
          tab.classList.remove("hidden");
          document.querySelectorAll(".tab-btn").forEach((el) => {
            el.classList.remove("bg-primary", "text-primary-foreground");
            el.classList.add("bg-muted");
          });
          el.classList.add("bg-primary", "text-primary-foreground");
        } else {
          tab.classList.add("hidden");
        }
      });
    });
  });

  const formatBytes = (n) => {
    if (!n) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    const i = Math.min(
      Math.floor(Math.log(n) / Math.log(1024)),
      units.length - 1
    );
    return `${(n / 1024 ** i).toFixed(i ? 2 : 0)} ${units[i]}`;
  };

  const refreshMaintenance = async () => {
    try {
      const [sessionList, logs] = await Promise.all([
        fetch("/api/v1/sessions").then((r) => r.json()),
        fetch("/api/v1/logs").then((r) => r.json()),
      ]);

      const statsEl = document.querySelector("#session-stats");
      statsEl.textContent = "";
      if (!sessionList.length) {
        statsEl.textContent = "No active sessions";
      } else {
        sessionList.forEach((s) => {
          const line = document.createElement("div");
          const pct = s.size ? Math.round((s.complete / s.size) * 100) : 0;
          line.textContent = `${s.name}: ${pct}% of ${formatBytes(
            s.size
          )}, ${s.peers} peers`;
          statsEl.appendChild(line);
        });
      }

      const logEl = document.querySelector("#log-output");
      logEl.textContent = (logs.lines || []).join("\n") || "No logs yet";
      logEl.scrollTop = logEl.scrollHeight;
    } catch (err) {
      console.error("Failed to refresh maintenance info:", err);
    }
  };

  document
    .querySelector('.tab-btn[data-index="3"]')
    .addEventListener("click", refreshMaintenance);
  document
    .querySelector("#refresh-maintenance")
    .addEventListener("click", refreshMaintenance);

  document.querySelector("#copy-logs").addEventListener("click", () => {
    navigator.clipboard
      .writeText(document.querySelector("#log-output").textContent)
      .then(() => {
        butterup.toast({
          message: "Logs copied to clipboard",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "success",
        });
      });
  });

  document.querySelector("#purge-cache").addEventListener("click", async () => {
    if (!confirm("Stop all torrent sessions and delete all downloaded data?")) {
      return;
    }
    const btn = document.querySelector("#purge-cache");
    btn.setAttribute("disabled", "disabled");
    try {
      const res = await fetch("/api/v1/cache/purge", { method: "POST" });
      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || "Failed to purge cache");
      }
      butterup.toast({
        message: `Cache purged, freed ${data.freed}`,
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "success",
      });
      refreshMaintenance();
    } catch (err) {
      butterup.toast({
        message: err.message || "Failed to purge cache",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
    } finally {
      btn.removeAttribute("disabled");
    }
  });

  function generatePagination(currentPage, pageSize, total, target) {
    const pagination = document.querySelector(target);
    if (!pagination) return;
    pagination.classList.remove("hidden");
    pagination.innerHTML = "";
    const totalPages = Math.ceil(total / pageSize);
    const startPage = Math.max(1, currentPage - 2);
    const endPage = Math.min(totalPages, currentPage + 2);

    for (let i = startPage; i <= endPage; i++) {
      const pageButton = document.createElement("button");
      pageButton.textContent = i;
      pageButton.classList.add("page-button");
      if (i === currentPage) {
        pageButton.classList.add("active");
      }
      pageButton.addEventListener("click", () => {
        searchPage = i;
        updateSearchResults();
      });
      pagination.appendChild(pageButton);
    }
    const prevButton = document.createElement("button");
    prevButton.innerHTML = `<svg stroke="currentColor" fill="currentColor" stroke-width="0" viewBox="0 0 20 20" aria-hidden="true" height="1em" width="1em" xmlns="http://www.w3.org/2000/svg"><path fill-rule="evenodd" d="M4.72 9.47a.75.75 0 0 0 0 1.06l4.25 4.25a.75.75 0 1 0 1.06-1.06L6.31 10l3.72-3.72a.75.75 0 1 0-1.06-1.06L4.72 9.47Zm9.25-4.25L9.72 9.47a.75.75 0 0 0 0 1.06l4.25 4.25a.75.75 0 1 0 1.06-1.06L11.31 10l3.72-3.72a.75.75 0 0 0-1.06-1.06Z" clip-rule="evenodd"></path></svg>`;
    prevButton.classList.add("page-button");
    prevButton.disabled = currentPage === 1;
    prevButton.addEventListener("click", () => {
      if (currentPage > 1) {
        searchPage--;
        updateSearchResults();
      }
    });
    pagination.prepend(prevButton);
    const nextButton = document.createElement("button");
    nextButton.innerHTML = `<svg stroke="currentColor" fill="currentColor" stroke-width="0" viewBox="0 0 20 20" aria-hidden="true" height="1em" width="1em" xmlns="http://www.w3.org/2000/svg"><path fill-rule="evenodd" d="M15.28 9.47a.75.75 0 0 1 0 1.06l-4.25 4.25a.75.75 0 1 1-1.06-1.06L13.69 10 9.97 6.28a.75.75 0 0 1 1.06-1.06l4.25 4.25ZM6.03 5.22l4.25 4.25a.75.75 0 0 1 0 1.06l-4.25 4.25a.75.75 0 0 1-1.06-1.06L8.69 10 4.97 6.28a.75.75 0 0 1 1.06-1.06Z" clip-rule="evenodd"></path></svg>`;
    nextButton.classList.add("page-button");
    nextButton.disabled = currentPage === totalPages;
    nextButton.addEventListener("click", () => {
      if (currentPage < totalPages) {
        searchPage++;
        updateSearchResults();
      }
    });
    pagination.appendChild(nextButton);
  }

  let searchData = [];
  let searchPage = 1;
  let searchPageSize = 5; // title groups per page
  // the search box doubles as the magnet box, so it is always visible
  const updateSearchVisibility = () => {
    searchWrapper.classList.remove("hidden");
  };

  // Results arrive grouped by title (server: metadata.go). Torrent names and
  // metadata are untrusted: DOM is built with textContent, posters only
  // from https URLs.
  const GROUP_TORRENTS_SHOWN = 4;

  const playFromButton = (btn, magnet, opts = {}) => {
    document.querySelector("#magnet").value = magnet;
    startPlayback(opts);
    btn.setAttribute("disabled", "disabled");
    btn.innerHTML = "";
    btn.classList.add("loader");
  };

  const ellipsis = "overflow:hidden;text-overflow:ellipsis;white-space:nowrap";

  const buildPoster = (meta, width = 70, height = 104) => {
    const box = document.createElement("div");
    box.className = "bg-muted rounded-md overflow-hidden shrink-0";
    box.style.cssText = `width:${width}px;height:${height}px`;
    if (meta?.poster && meta.poster.startsWith("https://")) {
      const img = document.createElement("img");
      img.src = meta.poster;
      img.alt = meta.title || "";
      img.loading = "lazy";
      img.referrerPolicy = "no-referrer";
      img.style.cssText = "width:100%;height:100%;object-fit:cover";
      img.addEventListener("error", () => img.remove());
      box.appendChild(img);
    }
    return box;
  };

  const buildTorrentRow = (result, opts = {}) => {
    const row = document.createElement("div");
    row.className = "flex items-center gap-3";

    const text = document.createElement("div");
    text.className = "flex flex-col flex-1 min-w-0";
    const name = document.createElement("span");
    name.className = "text-sm";
    name.style.cssText = ellipsis;
    name.textContent = result.title;
    name.title = result.title;
    const info = document.createElement("span");
    info.className = "text-xs text-muted-foreground";
    info.style.cssText = ellipsis;
    info.textContent = [
      result.size,
      `${result.seeders ?? 0} seeders`,
      result.indexer,
    ]
      .filter(Boolean)
      .join(" · ");
    text.append(name, info);

    const watchBtn = document.createElement("button");
    watchBtn.type = "button";
    watchBtn.className = "btn small shrink-0 play-torrent"; // reset by startPlayback on failure
    watchBtn.textContent = "Watch";
    watchBtn.addEventListener("click", () =>
      playFromButton(watchBtn, result.downloadUrl || result.magnetUrl || "", opts)
    );

    row.append(text, watchBtn);
    return row;
  };

  // title, year, rating, type, IMDb link and genres of a title; series get
  // an "Episodes" button unless opts.episodes === false
  const buildMetaHeader = (meta, opts = {}) => {
    const wrap = document.createElement("div");
    wrap.className = "flex flex-col";
    wrap.style.gap = "4px"; // gap-1 is not in the compiled CSS
    const header = document.createElement("div");
    header.className = "flex items-center gap-2";
    header.style.flexWrap = "wrap";
    const title = document.createElement("span");
    title.className = "font-semibold";
    title.textContent = meta ? meta.title : "Other results";
    header.appendChild(title);
    if (meta) {
      const details = [];
      if (meta.year) details.push(meta.year);
      if (meta.rating) details.push(`★ ${meta.rating}`);
      details.push(meta.type === "series" ? "Series" : "Movie");
      const d = document.createElement("span");
      d.className = "text-sm text-muted-foreground";
      d.textContent = details.join(" · ");
      header.appendChild(d);
      if (/^tt\d+$/.test(meta.imdb)) {
        const link = document.createElement("a");
        link.className = "text-xs text-primary";
        link.href = `https://www.imdb.com/title/${meta.imdb}/`;
        link.target = "_blank";
        link.rel = "noopener noreferrer";
        link.textContent = `IMDb ${meta.imdb}`;
        header.appendChild(link);
        if (meta.type === "series" && opts.episodes !== false) {
          const eps = document.createElement("button");
          eps.type = "button";
          eps.className = "btn small shrink-0";
          eps.textContent = "Episodes";
          eps.addEventListener("click", () => openSeries(meta));
          header.appendChild(eps);
        }
      }
    }
    wrap.appendChild(header);
    if (meta?.genres?.length) {
      const genres = document.createElement("span");
      genres.className = "text-xs text-muted-foreground";
      genres.textContent = meta.genres.slice(0, 4).join(", ");
      wrap.appendChild(genres);
    }
    return wrap;
  };

  // torrent rows with a "Show N more" toggle
  const buildTorrentList = (results, shownByDefault = GROUP_TORRENTS_SHOWN, opts = {}) => {
    const list = document.createElement("div");
    list.className = "flex flex-col gap-2";
    let expanded = false;
    const renderRows = () => {
      list.innerHTML = "";
      const shown = expanded ? results : results.slice(0, shownByDefault);
      shown.forEach((r) => list.appendChild(buildTorrentRow(r, opts)));
      if (results.length > shownByDefault) {
        const more = document.createElement("button");
        more.type = "button";
        more.className = "text-xs text-primary cursor-pointer";
        more.style.alignSelf = "flex-start";
        more.textContent = expanded
          ? "Show less"
          : `Show ${results.length - shownByDefault} more`;
        more.addEventListener("click", () => {
          expanded = !expanded;
          renderRows();
        });
        list.appendChild(more);
      }
    };
    renderRows();
    return list;
  };

  const buildGroupCard = (group, isLast) => {
    const meta = group.meta;
    const card = document.createElement("div");
    card.className = "flex gap-3 p-3" + (isLast ? "" : " border-b");
    if (meta) {
      card.appendChild(buildPoster(meta));
    }
    const body = document.createElement("div");
    body.className = "flex flex-col gap-2 flex-1 min-w-0";
    body.append(buildMetaHeader(meta), buildTorrentList(group.results || []));
    card.appendChild(body);
    return card;
  };

  const updateSearchResults = () => {
    const searchResults = document.querySelector("#search-result");
    const container = document.querySelector("#search-groups");
    const empty = document.querySelector("#search-empty");
    searchResults.classList.remove("hidden");
    container.innerHTML = "";
    empty.classList.toggle("hidden", searchData.length > 0);
    if (searchData.length === 0) {
      document.querySelector("#search-pagination").classList.add("hidden");
      return;
    }

    const start = (searchPage - 1) * searchPageSize;
    const page = searchData.slice(start, start + searchPageSize);
    page.forEach((group, i) =>
      container.appendChild(buildGroupCard(group, i === page.length - 1))
    );
    searchResults.scrollTop = 0;

    generatePagination(
      searchPage,
      searchPageSize,
      searchData.length,
      "#search-pagination"
    );
  };

  // Series view (server: series.go): seasons, episodes and an automatic
  // torrent per episode, with the other candidates (episode torrents and
  // season packs) one click away. startPlayback({episode}) picks the right
  // file inside a pack.
  const seriesView = { meta: null, info: null, season: null, torrents: {}, token: 0 };

  const pad2 = (n) => String(n).padStart(2, "0");
  const episodeCode = (s, e) => `S${pad2(s)}E${pad2(e)}`;
  const qualityLabel = (name) => (name.match(/\b(2160p|1080p|720p|480p)\b/i) || [])[1] || "";

  // the video of season s / episode e inside a torrent's file list, or null
  const findEpisodeFile = (files, s, e) => {
    const usable = files.filter((f) => !/(^|[^a-z])sample([^a-z]|$)/i.test(f.name));
    const sxe = new RegExp(`(^|[^a-z0-9])s0*${s}[ ._-]*e0*${e}(?![0-9])`, "i");
    const nxn = new RegExp(`(^|[^0-9])${s}x0*${e}(?![0-9])`, "i");
    let hits = usable.filter((f) => sxe.test(f.name) || nxn.test(f.name));
    if (!hits.length) {
      // "Season 2/05 - Name.mkv"
      const seasonDir = new RegExp(`(season|s)[ ._-]*0*${s}(?![0-9])[^/]*/`, "i");
      const lead = new RegExp(`^(e|ep|episode)?[ ._-]*0*${e}(?![0-9])`, "i");
      hits = usable.filter(
        (f) => seasonDir.test(f.name) && lead.test(f.name.split("/").pop())
      );
    }
    if (!hits.length && usable.length === 1) {
      hits = usable; // a single-episode torrent named differently
    }
    return hits.sort((a, b) => (b.size || 0) - (a.size || 0))[0] || null;
  };

  const searchManually = (query) => {
    document.querySelector("#search").value = query;
    document.querySelector("#search-category").value = "tv";
    document.querySelector("#search-form").requestSubmit();
    window.scrollTo({ top: 0, behavior: "smooth" });
  };

  const buildEpisodeRow = (season, ep) => {
    const meta = seriesView.meta;
    const row = document.createElement("div");
    row.className = "flex gap-3 p-3 border-b";
    if (!ep.aired) {
      row.style.opacity = "0.6";
    }

    const thumb = document.createElement("div");
    thumb.className = "bg-muted rounded-md overflow-hidden shrink-0";
    thumb.style.cssText = "width:128px;height:72px";
    if (ep.thumbnail && ep.thumbnail.startsWith("https://")) {
      const img = document.createElement("img");
      img.src = ep.thumbnail;
      img.alt = "";
      img.loading = "lazy";
      img.referrerPolicy = "no-referrer";
      img.style.cssText = "width:100%;height:100%;object-fit:cover";
      img.addEventListener("error", () => img.remove());
      thumb.appendChild(img);
    }

    const body = document.createElement("div");
    body.className = "flex flex-col flex-1 min-w-0";
    body.style.gap = "4px";
    const title = document.createElement("span");
    title.className = "text-sm font-semibold";
    title.style.cssText = ellipsis;
    title.textContent = `E${pad2(ep.episode)} · ${ep.name}`;
    title.title = ep.name;
    const date = document.createElement("span");
    date.className = "text-xs text-muted-foreground";
    date.textContent = [ep.released, ep.aired ? "" : "Not aired yet"]
      .filter(Boolean)
      .join(" · ");
    body.append(title, date);
    if (ep.overview) {
      const overview = document.createElement("span");
      overview.className = "text-xs text-muted-foreground";
      overview.style.cssText =
        "display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden";
      overview.textContent = ep.overview;
      overview.title = ep.overview;
      body.appendChild(overview);
    }

    const actions = document.createElement("div");
    actions.className = "flex items-center gap-2";
    actions.style.flexWrap = "wrap";
    const watch = document.createElement("button");
    watch.type = "button";
    watch.className = "btn small shrink-0";
    watch.textContent = "Watch";
    watch.setAttribute("disabled", "disabled"); // until torrents arrive
    const note = document.createElement("span");
    note.className = "text-xs text-muted-foreground";
    note.style.cssText = ellipsis;
    actions.append(watch, note);
    body.appendChild(actions);

    const others = document.createElement("div");
    others.className = "hidden flex flex-col gap-2";
    others.style.marginTop = "6px";
    body.appendChild(others);

    row.append(thumb, body);

    const episodeOpts = {
      episode: { imdb: meta.imdb, title: meta.title, season, episode: ep.episode },
    };
    const searchLink = (label) => {
      const a = document.createElement("button");
      a.type = "button";
      a.className = "text-xs text-primary cursor-pointer";
      a.style.alignSelf = "flex-start";
      a.textContent = label;
      a.addEventListener("click", () =>
        searchManually(`${meta.title} ${episodeCode(season, ep.episode)}`)
      );
      return a;
    };

    const setTorrents = (list) => {
      const key = String(ep.episode);
      const episodeTorrents = list?.episodes?.[key] || [];
      const packs = list?.packs || [];
      // no torrent of its own: the best season pack (ranked by the server)
      const best =
        list?.best?.[key] || (packs[0] ? { pack: true, torrent: packs[0] } : null);
      if (!best) {
        watch.remove(); // keeps startPlayback from re-enabling it
        note.textContent = list ? "No torrents found" : "Torrents unavailable";
        actions.appendChild(searchLink("Search"));
        return;
      }
      watch.classList.add("play-torrent"); // reset by startPlayback
      watch.removeAttribute("disabled");
      watch.addEventListener("click", () =>
        playFromButton(
          watch,
          best.torrent.downloadUrl || best.torrent.magnetUrl || "",
          episodeOpts
        )
      );
      note.textContent = [
        best.pack ? "Season pack" : "",
        qualityLabel(best.torrent.title),
        `${best.torrent.seeders ?? 0} seeders`,
      ]
        .filter(Boolean)
        .join(" · ");
      note.title = best.torrent.title;

      const toggle = document.createElement("button");
      toggle.type = "button";
      toggle.className = "text-xs text-primary cursor-pointer";
      toggle.textContent = "Other torrents";
      toggle.addEventListener("click", () => {
        if (others.childElementCount === 0) {
          const section = (label, results) => {
            const h = document.createElement("span");
            h.className = "text-xs font-semibold text-muted-foreground";
            h.textContent = label;
            others.append(h, buildTorrentList(results, 4, episodeOpts));
          };
          if (episodeTorrents.length) section("This episode", episodeTorrents);
          if (packs.length) section("Season packs", packs);
          others.appendChild(searchLink("Search for more"));
        }
        others.classList.toggle("hidden");
      });
      actions.appendChild(toggle);
    };

    return { el: row, setTorrents };
  };

  const selectSeason = async (n) => {
    const token = seriesView.token;
    seriesView.season = n;
    document.querySelectorAll(".series-season").forEach((b) => {
      const on = b.dataset.season === String(n);
      b.classList.toggle("bg-primary", on);
      b.classList.toggle("text-primary-foreground", on);
      b.classList.toggle("bg-muted", !on);
    });
    const list = document.querySelector("#series-episodes");
    const status = document.querySelector("#series-status");
    list.innerHTML = "";
    const season = seriesView.info.seasons.find((s) => s.season === n);
    const rows = (season?.episodes || []).map((ep) => buildEpisodeRow(n, ep));
    rows.forEach((r) => list.appendChild(r.el));
    list.lastElementChild?.classList.remove("border-b");

    let torrents = seriesView.torrents[n];
    if (!torrents) {
      status.textContent = "Looking for torrents…";
      try {
        const res = await fetch(
          `/api/v1/series/${seriesView.meta.imdb}/torrents?season=${n}`
        );
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          throw new Error(data.error || "Could not load torrents");
        }
        torrents = data;
        if ((data.sources || []).some((s) => s.ok)) {
          seriesView.torrents[n] = data; // don't cache a total failure
        }
      } catch (err) {
        if (token === seriesView.token && seriesView.season === n) {
          status.textContent = err.message;
          rows.forEach((r) => r.setTorrents(null));
        }
        return;
      }
    }
    if (token !== seriesView.token || seriesView.season !== n) {
      return; // another season or series was opened meanwhile
    }
    const failed = (torrents.sources || []).filter((s) => !s.ok);
    status.textContent = failed.length
      ? "Some sources failed: " + failed.map((s) => s.label).join(", ")
      : "";
    rows.forEach((r) => r.setTorrents(torrents));
  };

  const closeSeries = () => {
    seriesView.token++;
    document.querySelector("#series-view").classList.add("hidden");
  };

  const openSeries = async (meta) => {
    const token = ++seriesView.token;
    Object.assign(seriesView, { meta, info: null, season: null, torrents: {} });
    const view = document.querySelector("#series-view");
    view.innerHTML = "";

    const close = document.createElement("button");
    close.type = "button";
    close.className = "text-sm text-primary cursor-pointer";
    close.style.cssText = "display:block;margin-left:auto";
    close.textContent = "Close";
    close.addEventListener("click", closeSeries);

    const head = document.createElement("div");
    head.className = "flex gap-3";
    const headBody = document.createElement("div");
    headBody.className = "flex flex-col gap-2 flex-1 min-w-0";
    const seasons = document.createElement("div");
    seasons.className = "flex items-center gap-2";
    seasons.style.flexWrap = "wrap";
    const status = document.createElement("p");
    status.id = "series-status";
    status.className = "text-sm text-muted-foreground";
    status.textContent = "Loading episodes…";
    headBody.append(buildMetaHeader(meta, { episodes: false }), seasons, status);
    head.append(buildPoster(meta, 100, 148), headBody);

    const list = document.createElement("div");
    list.id = "series-episodes";
    list.className = "flex flex-col border rounded-lg";
    list.style.cssText = "max-height:70vh;overflow:auto";

    view.append(close, head, list);
    view.classList.remove("hidden");
    view.scrollIntoView({ behavior: "smooth", block: "start" });

    try {
      const res = await fetch(`/api/v1/series/${meta.imdb}`);
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        throw new Error(data.error || "Could not load this series");
      }
      if (token !== seriesView.token) {
        return;
      }
      seriesView.info = data;
    } catch (err) {
      if (token === seriesView.token) {
        status.textContent = err.message;
      }
      return;
    }

    const all = seriesView.info.seasons || [];
    if (!all.length) {
      status.textContent = "No episodes listed for this series";
      return;
    }
    all.forEach((s) => {
      const b = document.createElement("button");
      b.type = "button";
      b.className =
        "series-season px-3 py-1 rounded-lg border bg-muted text-sm cursor-pointer";
      b.dataset.season = String(s.season);
      b.textContent = s.season === 0 ? "Specials" : `Season ${s.season}`;
      b.addEventListener("click", () => selectSeason(s.season));
      seasons.appendChild(b);
    });
    // Phase 3 will open the season of the next episode to watch
    selectSeason((all.find((s) => s.season > 0) || all[0]).season);
  };

  // Featured titles (server: featured.go) and streaming catalogs (server:
  // catalogs.go): tabs + poster grid; clicking a poster opens its torrents to
  // pick one. Catalog titles come without torrents, so they are searched on
  // open. Lists are cached per page load.
  const featuredLoaded = {};
  let featuredActive = null;
  let streamType = "movie";
  let featuredDetailToken = 0;

  const torrentsForTitle = async (meta) => {
    const isSeries = meta.type === "series";
    const year = (meta.year || "").slice(0, 4);
    const q = isSeries || !year ? meta.title : `${meta.title} ${year}`;
    const res = await fetch(
      `/api/v1/search?q=${encodeURIComponent(q)}&category=${isSeries ? "tv" : "movies"}`,
      { signal: AbortSignal.timeout(((settings?.searchTimeoutSeconds || 30) + 5) * 1000) }
    );
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(data.error || "Search failed");
    }
    const match = (data.groups || []).find((g) => g.key === meta.imdb);
    return match ? match.results || [] : [];
  };

  const showFeaturedDetail = async (group) => {
    const token = ++featuredDetailToken;
    const detail = document.querySelector("#featured-detail");
    detail.innerHTML = "";

    const close = document.createElement("button");
    close.type = "button";
    close.className = "text-sm text-primary cursor-pointer";
    close.style.cssText = "display:block;margin-left:auto";
    close.textContent = "Close";
    close.addEventListener("click", () => {
      featuredDetailToken++;
      detail.classList.add("hidden");
    });

    const row = document.createElement("div");
    row.className = "flex gap-3";
    const body = document.createElement("div");
    body.className = "flex flex-col gap-2 flex-1 min-w-0";
    const list = document.createElement("div");
    body.append(buildMetaHeader(group.meta), list);
    row.append(buildPoster(group.meta, 100, 148), body);

    detail.append(close, row);
    detail.classList.remove("hidden");
    detail.scrollIntoView({ behavior: "smooth", block: "nearest" });

    if (!group.results?.length && !group.searched && group.meta?.imdb) {
      const note = document.createElement("p");
      note.className = "text-sm text-muted-foreground";
      note.textContent = "Searching torrents…";
      list.append(note);
      try {
        group.results = await torrentsForTitle(group.meta);
        group.searched = true;
      } catch (err) {
        if (token === featuredDetailToken) {
          note.textContent = err.message;
        }
        return;
      }
      if (token !== featuredDetailToken) {
        return; // closed, or another title was opened meanwhile
      }
      list.innerHTML = "";
    }
    if (!group.results?.length) {
      const none = document.createElement("p");
      none.className = "text-sm text-muted-foreground";
      none.textContent = "No torrents found for this title";
      list.append(none);
      return;
    }
    list.append(buildTorrentList(group.results, 6));
  };

  const buildFeaturedTile = (group) => {
    const meta = group.meta || {};
    const tile = document.createElement("button");
    tile.type = "button";
    tile.className = "flex flex-col gap-2 cursor-pointer";
    tile.style.cssText =
      "text-align:left;min-width:0;background:none;border:0;padding:0";
    tile.title = meta.title || "";

    const poster = buildPoster(meta);
    poster.style.cssText = "width:100%;aspect-ratio:2/3";

    const name = document.createElement("span");
    name.className = "text-sm font-semibold";
    name.style.cssText = ellipsis;
    name.textContent = meta.title || "";

    const sub = document.createElement("span");
    sub.className = "text-xs text-muted-foreground";
    sub.textContent = [meta.year, meta.rating ? `★ ${meta.rating}` : ""]
      .filter(Boolean)
      .join(" · ");

    tile.append(poster, name, sub);
    tile.addEventListener("click", () =>
      meta.type === "series" && /^tt\d+$/.test(meta.imdb || "")
        ? openSeries(meta)
        : showFeaturedDetail(group)
    );
    return tile;
  };

  const markActive = (selector, attr, value) =>
    document.querySelectorAll(selector).forEach((b) => {
      const on = b.dataset[attr] === value;
      b.classList.toggle("bg-primary", on);
      b.classList.toggle("text-primary-foreground", on);
      b.classList.toggle("bg-muted", !on);
    });

  // id: a featured list ("popular"...) or "stream:<service>" (uses streamType)
  const loadFeaturedList = async (id) => {
    const isStream = id.startsWith("stream:");
    const key = isStream ? `${id}/${streamType}` : id;
    featuredActive = key;
    featuredDetailToken++;
    markActive(".featured-tab", "list", id);
    markActive(".stream-type", "type", isStream ? streamType : "");
    document.querySelector("#featured-detail").classList.add("hidden");
    const grid = document.querySelector("#featured-grid");
    const status = document.querySelector("#featured-status");
    grid.innerHTML = "";

    let groups = featuredLoaded[key];
    if (!groups) {
      status.textContent = "Loading…";
      const url = isStream
        ? `/api/v1/catalog?service=${encodeURIComponent(id.slice(7))}&type=${streamType}`
        : `/api/v1/featured?list=${encodeURIComponent(id)}`;
      try {
        const res = await fetch(url);
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          throw new Error(data.error || "Could not load this list");
        }
        groups = Array.isArray(data.groups) ? data.groups : [];
        featuredLoaded[key] = groups;
      } catch (err) {
        if (featuredActive === key) {
          status.textContent = err.message;
        }
        return;
      }
    }
    if (featuredActive !== key) {
      return; // another tab was picked while this one loaded
    }
    status.textContent = groups.length ? "" : "Nothing to show right now";
    groups.forEach((g) => grid.appendChild(buildFeaturedTile(g)));
  };

  document.querySelectorAll(".featured-tab").forEach((b) =>
    b.addEventListener("click", () => loadFeaturedList(b.dataset.list))
  );
  document.querySelectorAll(".stream-type").forEach((b) =>
    b.addEventListener("click", () => {
      streamType = b.dataset.type;
      // keep the current service, or default to Netflix
      const active = featuredActive || "";
      loadFeaturedList(
        active.startsWith("stream:") ? active.split("/")[0] : "stream:nfx"
      );
    })
  );
  loadFeaturedList("popular");

  document.querySelector("#search-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const query = e.target.querySelector("#search").value;

    const magnet = asMagnet(query);
    if (magnet) {
      const btn = e.target.querySelector("button[type=submit]");
      btn.setAttribute("disabled", "disabled");
      btn.classList.add("loader");
      btn.innerHTML = "";
      document.querySelector("#magnet").value = magnet;
      try {
        await startPlayback();
      } finally {
        btn.removeAttribute("disabled");
        btn.classList.remove("loader");
        btn.innerHTML = "Search";
      }
      return;
    }

    if (!query) {
      butterup.toast({
        message: "Please enter a search query",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return;
    }

    searchData = [];
    searchPage = 1;

    e.target
      .querySelector("button[type=submit]")
      .setAttribute("disabled", "disabled");
    e.target.querySelector("button[type=submit]").classList.add("loader");
    e.target.querySelector("button[type=submit]").innerHTML = "";
    const searchResults = document.querySelector("#search-result");

    searchResults.classList.add("hidden");
    document.querySelector("#search-pagination").classList.add("hidden");

    // public sources + Prowlarr/Jackett, merged server side
    const category = document.querySelector("#search-category").value;
    const timeoutMs = ((settings?.searchTimeoutSeconds || 30) + 5) * 1000;

    fetch(
      `/api/v1/search?q=${encodeURIComponent(query)}&category=${encodeURIComponent(category)}`,
      { signal: AbortSignal.timeout(timeoutMs) }
    )
      .then(async (res) => {
        if (!res.ok) {
          const err = await res.json().catch(() => ({}));
          throw new Error(err.error || "Failed to fetch search results");
        }
        return res.json();
      })
      .then((data) => {
        searchData = Array.isArray(data?.groups) ? data.groups : [];

        const failed = (data?.sources || []).filter((s) => !s.ok);
        if (failed.length) {
          const allFailed = failed.length === data.sources.length;
          butterup.toast({
            message:
              (allFailed ? "Search failed: " : "Some sources failed: ") +
              failed.map((s) => `${s.label} (${s.error})`).join(", "),
            location: "top-right",
            icon: true,
            dismissable: true,
            type: allFailed ? "error" : "warning",
          });
        }

        updateSearchResults();
      })
      .catch((error) => {
        console.error("There was a problem with the fetch operation:", error);
        butterup.toast({
          message: error.message || "Failed to fetch search results",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      })
      .finally(() => {
        e.target
          .querySelector("button[type=submit]")
          .removeAttribute("disabled");
        e.target
          .querySelector("button[type=submit]")
          .classList.remove("loader");
        e.target.querySelector("button[type=submit]").innerHTML = "Search";
      });
  });

  const testProwlarrConfig = async () => {
    const prowlarrHost = document.querySelector("#prowlarrHost").value;
    const prowlarrApiKey = document.querySelector("#prowlarrApiKey").value;
    const prowlarrTestBtn = document.querySelector("#test-prowlarr");

    if (!prowlarrHost || (!prowlarrApiKey && !settings?.prowlarrApiKeySet)) {
      butterup.toast({
        message: "Please enter Prowlarr host and API key",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return false;
    }

    prowlarrTestBtn.setAttribute("disabled", "disabled");
    prowlarrTestBtn.querySelector("span").innerHTML = "Testing...";
    
    const response = await fetch("/api/v1/prowlarr/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prowlarrHost, prowlarrApiKey }),
    });

    const data = await response.json();
    if (!response.ok) {
      butterup.toast({
        message: data.error || "Failed to test Prowlarr connection",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      prowlarrTestBtn.removeAttribute("disabled");
      prowlarrTestBtn.querySelector("span").innerHTML = "Test Connection";
      return false;
    }

    butterup.toast({
      message: "Prowlarr settings are valid",
      location: "top-right",
      icon: true,
      dismissable: true,
      type: "success",
    });

    prowlarrTestBtn.removeAttribute("disabled");
    prowlarrTestBtn.querySelector("span").innerHTML = "Test Connection";

    return true;
  }

  document.querySelector("#test-prowlarr").addEventListener("click", (e) => {
    testProwlarrConfig();
  });

  const testJackettConfig = async () => {
    const jackettHost = document.querySelector("#jackettHost").value;
    const jackettApiKey = document.querySelector("#jackettApiKey").value;
    const jackettTestBtn = document.querySelector("#test-jackett");

    if (!jackettHost || (!jackettApiKey && !settings?.jackettApiKeySet)) {
      butterup.toast({
        message: "Please enter Jackett host and API key",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return false;
    }

    jackettTestBtn.setAttribute("disabled", "disabled");
    jackettTestBtn.querySelector("span").innerHTML = "Testing...";
    
    const response = await fetch("/api/v1/jackett/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ jackettHost, jackettApiKey }),
    });

    const data = await response.json();
    if (!response.ok) {
      butterup.toast({
        message: data.error || "Failed to test Jackett connection",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      jackettTestBtn.removeAttribute("disabled");
      jackettTestBtn.querySelector("span").innerHTML = "Test Connection";
      return false;
    }

    butterup.toast({
      message: "Jackett settings are valid",
      location: "top-right",
      icon: true,
      dismissable: true,
      type: "success",
    });

    jackettTestBtn.removeAttribute("disabled");
    jackettTestBtn.querySelector("span").innerHTML = "Test Connection";

    return true;
  }

  document.querySelector("#test-jackett").addEventListener("click", (e) => {
    testJackettConfig();
  });

  const testProxy = async () => {
    const proxyUrl = document.querySelector("#proxyUrl").value;
    const proxyBtn = document.querySelector("#test-proxy");

    if (!proxyUrl) {
      butterup.toast({
        message: "Please enter a proxy URL",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      return false;
    }

    proxyBtn.setAttribute("disabled", "disabled");
    proxyBtn.querySelector("span").innerHTML = "Testing...";

    const response = await fetch("/api/v1/proxy/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ proxyUrl }),
    });

    const data = await response.json();

    if (!response.ok) {
      butterup.toast({
        message: data.error || "Failed to test Proxy connection",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
      proxyBtn.removeAttribute("disabled");
      proxyBtn.querySelector("span").innerHTML = "Test Proxy";
      return false;
    }

    butterup.toast({
      message: "Proxy url is valid",
      location: "top-right",
      icon: true,
      dismissable: true,
      type: "success",
    });

    proxyBtn.removeAttribute("disabled");
    proxyBtn.querySelector("span").innerHTML = "Test Proxy";

    if (data?.origin) {
      document.querySelector("#proxy-result").classList.remove("hidden");
      document.querySelector("#proxy-result").classList.add("flex");
      document.querySelector("#proxy-result .output-ip").innerHTML = data?.origin
    }

    return true;
  }

  document.querySelector("#test-proxy").addEventListener("click", () => {
    testProxy();
  });

  document
    .querySelector("#proxy-settings-form")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const enableProxy = e.target.querySelector("#enableProxy").checked;
      const proxyUrl = e.target.querySelector("#proxyUrl").value;
      const submitButton = e.target.querySelector("button[type=submit]");

      submitButton.setAttribute("disabled", "disabled");

      if (enableProxy) {
        const isValid = await testProxy();
        if (!isValid) {
          submitButton.removeAttribute("disabled");
          return;
        }
      }

      submitButton.classList.add("loader");
      submitButton.innerHTML = "Saving...";

      const body = {
        enableProxy,
        proxyUrl,
      };

      const response = await fetch("/api/v1/settings/proxy", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })

      const data = await response.json();

      if (!response.ok) {
        butterup.toast({
          message: data.error || "Failed to save settings",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      } else {
        butterup.toast({
          message: "Proxy settings saved successfully",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "success",
        });

        settings = {
          ...settings,
          enableProxy: body.enableProxy,
          proxyUrl: body.proxyUrl,
        };
      }

      submitButton.removeAttribute("disabled");
      submitButton.classList.remove("loader");
      submitButton.innerHTML = "Save Settings";
    });

  document
    .querySelector("#prowlarr-settings-form")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const enableProwlarr = e.target.querySelector("#enableProwlarr").checked;
      const prowlarrHost = e.target.querySelector("#prowlarrHost").value;
      const prowlarrApiKey = e.target.querySelector("#prowlarrApiKey").value;
      const submitButton = e.target.querySelector("button[type=submit]");

      submitButton.setAttribute("disabled", "disabled");

      if (enableProwlarr) {
        const isValid = await testProwlarrConfig();
        if (!isValid) {
          submitButton.removeAttribute("disabled");
          return;
        }
      }

      submitButton.classList.add("loader");
      submitButton.innerHTML = "Saving...";

      const body = {
        enableProwlarr,
        prowlarrHost,
        prowlarrApiKey,
        searchTimeoutSeconds:
          parseInt(e.target.querySelector("#prowlarrTimeout").value, 10) || 0,
        skipTlsVerify: e.target.querySelector("#prowlarrSkipTls").checked,
      };

      const response = await fetch("/api/v1/settings/prowlarr", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })

      const data = await response.json();
      if (!response.ok) {
        butterup.toast({
          message: data.error || "Failed to save settings",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      } else {
        butterup.toast({
          message: "Prowlarr settings saved successfully",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "success",
        });

        settings = {
          ...settings,
          enableProwlarr: body.enableProwlarr,
          prowlarrHost: body.prowlarrHost,
          prowlarrApiKeySet:
            !!body.prowlarrApiKey || settings?.prowlarrApiKeySet,
          searchTimeoutSeconds: body.searchTimeoutSeconds,
          skipTlsVerify: body.skipTlsVerify,
        };

        const prowlarrKeyInput = document.querySelector("#prowlarrApiKey");
        prowlarrKeyInput.value = "";
        if (settings.prowlarrApiKeySet) {
          prowlarrKeyInput.placeholder = "Saved (leave blank to keep)";
        }

        document.querySelector("#jackettTimeout").value =
          body.searchTimeoutSeconds || "";
        document.querySelector("#jackettSkipTls").checked = body.skipTlsVerify;

        updateSearchVisibility();
      }

      submitButton.removeAttribute("disabled");
      submitButton.classList.remove("loader");
      submitButton.innerHTML = "Save Settings";
    });

  document
  .querySelector("#jackett-settings-form")
  .addEventListener("submit", async (e) => {
    e.preventDefault();
    const enableJackett = e.target.querySelector("#enableJackett").checked;
    const jackettHost = e.target.querySelector("#jackettHost").value;
    const jackettApiKey = e.target.querySelector("#jackettApiKey").value;
    const submitButton = e.target.querySelector("button[type=submit]");

    submitButton.setAttribute("disabled", "disabled");

    if (enableJackett) {
      const isValid = await testJackettConfig();
      if (!isValid) {
        submitButton.removeAttribute("disabled");
        return;
      }
    }

    submitButton.classList.add("loader");
    submitButton.innerHTML = "Saving...";

    const body = {
      enableJackett,
      jackettHost,
      jackettApiKey,
      searchTimeoutSeconds:
        parseInt(e.target.querySelector("#jackettTimeout").value, 10) || 0,
      skipTlsVerify: e.target.querySelector("#jackettSkipTls").checked,
    };

    const response = await fetch("/api/v1/settings/jackett", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })

    const data = await response.json();
    if (!response.ok) {
      butterup.toast({
        message: data.error || "Failed to save settings",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "error",
      });
    } else {
      butterup.toast({
        message: "Jackett settings saved successfully",
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "success",
      });

      settings = {
        ...settings,
        enableJackett: body.enableJackett,
        jackettHost: body.jackettHost,
        jackettApiKeySet: !!body.jackettApiKey || settings?.jackettApiKeySet,
        searchTimeoutSeconds: body.searchTimeoutSeconds,
        skipTlsVerify: body.skipTlsVerify,
      };

      const jackettKeyInput = document.querySelector("#jackettApiKey");
      jackettKeyInput.value = "";
      if (settings.jackettApiKeySet) {
        jackettKeyInput.placeholder = "Saved (leave blank to keep)";
      }

      document.querySelector("#prowlarrTimeout").value =
        body.searchTimeoutSeconds || "";
      document.querySelector("#prowlarrSkipTls").checked = body.skipTlsVerify;

      updateSearchVisibility();
    }

    submitButton.removeAttribute("disabled");
    submitButton.classList.remove("loader");
    submitButton.innerHTML = "Save Settings";
  });

  document
    .querySelector("#search-settings-form")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const body = {
        enablePublicSearch: e.target.querySelector("#enablePublicSearch")
          .checked,
        sources: [...e.target.querySelectorAll(".search-source:checked")].map(
          (el) => el.value
        ),
      };
      try {
        const response = await fetch("/api/v1/settings/search", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        });
        const data = await response.json().catch(() => ({}));
        if (!response.ok) {
          throw new Error(data.error || "Failed to save search settings");
        }
        settings = {
          ...settings,
          disablePublicSearch: !body.enablePublicSearch,
          disabledSearchSources: ["yts", "tpb", "eztv"].filter(
            (id) => !body.sources.includes(id)
          ),
        };
        updateSearchVisibility();
        butterup.toast({
          message: "Search settings saved successfully",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "success",
        });
      } catch (err) {
        butterup.toast({
          message: err.message,
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      }
    });

  const subtitleFormBody = (form) => ({
    enableSubtitles: form.querySelector("#enableSubtitles").checked,
    openSubtitlesApiKey: form.querySelector("#osApiKey").value.trim(),
    openSubtitlesUsername: form.querySelector("#osUsername").value.trim(),
    openSubtitlesPassword: form.querySelector("#osPassword").value,
    subtitleLanguages: form.querySelector("#osLanguages").value.trim(),
  });

  document
    .querySelector("#test-subtitles")
    .addEventListener("click", async (e) => {
      const button = e.currentTarget;
      const form = document.querySelector("#subtitle-settings-form");
      button.setAttribute("disabled", "disabled");
      try {
        const response = await fetch("/api/v1/subtitles/test", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(subtitleFormBody(form)),
        });
        const data = await response.json();
        butterup.toast({
          message: response.ok
            ? data.message
            : data.error || "Connection test failed",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: response.ok ? "success" : "error",
        });
      } catch (err) {
        butterup.toast({
          message: "Connection test failed: " + err.message,
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      }
      button.removeAttribute("disabled");
    });

  document
    .querySelector("#subtitle-settings-form")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const submitButton = e.target.querySelector("button[type=submit]");
      submitButton.setAttribute("disabled", "disabled");
      submitButton.classList.add("loader");
      submitButton.innerHTML = "Saving...";

      const body = subtitleFormBody(e.target);
      try {
        const response = await fetch("/api/v1/settings/subtitles", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        });
        const data = await response.json();
        if (!response.ok) {
          butterup.toast({
            message: data.error || "Failed to save settings",
            location: "top-right",
            icon: true,
            dismissable: true,
            type: "error",
          });
        } else {
          butterup.toast({
            message: "Subtitle settings saved successfully",
            location: "top-right",
            icon: true,
            dismissable: true,
            type: "success",
          });

          settings = {
            ...settings,
            enableSubtitles: body.enableSubtitles,
            openSubtitlesUsername: body.openSubtitlesUsername,
            subtitleLanguages: body.subtitleLanguages,
            openSubtitlesApiKeySet:
              !!body.openSubtitlesApiKey || settings?.openSubtitlesApiKeySet,
            openSubtitlesPasswordSet:
              !!body.openSubtitlesPassword ||
              settings?.openSubtitlesPasswordSet,
          };

          const keyInput = document.querySelector("#osApiKey");
          keyInput.value = "";
          if (settings.openSubtitlesApiKeySet) {
            keyInput.placeholder = "Saved (leave blank to keep)";
          }
          const passwordInput = document.querySelector("#osPassword");
          passwordInput.value = "";
          if (settings.openSubtitlesPasswordSet) {
            passwordInput.placeholder = "Saved (leave blank to keep)";
          }
        }
      } catch (err) {
        butterup.toast({
          message: "Failed to save settings: " + err.message,
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      }

      submitButton.removeAttribute("disabled");
      submitButton.classList.remove("loader");
      submitButton.innerHTML = "Save Settings";
    });

  const playTorrentFile = (file) => {
    const formData = new FormData();
    formData.append("torrent", file);

    fetch("/api/v1/torrent/convert", {
      method: "POST",
      body: formData,
    })
      .then(async (res) => {
        if (!res.ok) {
          const err = await res.json();
          throw new Error(err.error || "Failed to upload torrent file");
        }
        return res.json();
      })
      .then((data) => {
        document.querySelector("#magnet").value = data.magnet;
        startPlayback();
      })
      .catch((error) => {
        console.error("There was a problem with the fetch operation:", error);
        butterup.toast({
          message: error.message || "Failed to upload torrent file",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      });
  };

  document.querySelector("#torrent_file").addEventListener("change", (e) => {
    const file = e.target.files[0];
    if (file) {
      playTorrentFile(file);
    }
  });

  // subtitle upload stays entirely in the browser
  const srtToVtt = (srt) => {
    const text = srt.replace(/^\uFEFF/, "").replace(/\r\n/g, "\n");
    let vtt = "WEBVTT\n\n";
    text.split("\n\n").forEach((block) => {
      const lines = block.trim().split("\n");
      if (!lines.length || !lines[0]) return;
      if (/^\d+$/.test(lines[0].trim())) lines.shift();
      if (!lines.length || !lines[0].includes("-->")) return;
      vtt +=
        lines[0].replace(/,/g, ".") + "\n" + lines.slice(1).join("\n") + "\n\n";
    });
    return vtt;
  };

  document
    .querySelector("#subtitle-file")
    .addEventListener("change", async (e) => {
      const file = e.target.files[0];
      e.target.value = "";
      if (!file || !player) {
        return;
      }

      const text = await file.text();
      const vtt = file.name.toLowerCase().endsWith(".vtt")
        ? text
        : srtToVtt(text);
      const blobUrl = URL.createObjectURL(
        new Blob([vtt], { type: "text/vtt" })
      );

      const langMatch = file.name.match(/\.([a-z]{2,3})\.(srt|vtt)$/i);
      const label = file.name.replace(/\.(srt|vtt)$/i, "") || "Uploaded";
      const added = player.addRemoteTextTrack(
        {
          kind: "subtitles",
          src: blobUrl,
          srclang: langMatch ? langMatch[1] : "en",
          label: label,
        },
        false
      );

      const tracks = player.textTracks();
      for (let i = 0; i < tracks.length; i++) {
        tracks[i].mode = tracks[i] === added.track ? "showing" : "disabled";
      }

      butterup.toast({
        message: `Subtitle "${label}" added`,
        location: "top-right",
        icon: true,
        dismissable: true,
        type: "success",
      });
    });

  const torrentFileWrapper = document.querySelector("#torrent_file_wrapper");
  torrentFileWrapper.addEventListener("dragenter", (e) => {
    e.preventDefault();
    e.stopPropagation();
    torrentFileWrapper.classList.add("drag-over");
  });
  torrentFileWrapper.addEventListener("dragover", (e) => {
    e.preventDefault();
    e.stopPropagation();
  });
  torrentFileWrapper.addEventListener("dragleave", (e) => {
    e.preventDefault();
    e.stopPropagation();
    torrentFileWrapper.classList.remove("drag-over");
  });
  torrentFileWrapper.addEventListener("drop", (e) => {
    e.preventDefault();
    e.stopPropagation();
    torrentFileWrapper.classList.remove("drag-over");
    const files = e.dataTransfer.files;
    if (files.length > 0) {
      const file = files[0];
      if (file.name.endsWith(".torrent")) {
        playTorrentFile(file);
      } else {
        butterup.toast({
          message: "Please drop a valid torrent file",
          location: "top-right",
          icon: true,
          dismissable: true,
          type: "error",
        });
      }
    }
  });

  // fetch settings
  fetch("/api/v1/settings")
    .then((res) => {
      if (!res.ok) {
        throw new Error("Network response was not ok");
      }
      return res.json();
    })
    .then((data) => {
      settings = data;
      document.querySelector("#keepWatchedDays").value =
        data.keepWatchedDays || 3;
      document.querySelector("#enablePublicSearch").checked =
        !data.disablePublicSearch;
      document.querySelectorAll(".search-source").forEach((el) => {
        el.checked = !(data.disabledSearchSources || []).includes(el.value);
      });
      document.querySelector("#enableProxy").checked = data.enableProxy;
      document.querySelector("#proxyUrl").value = data.proxyUrl || "";
      document.querySelector("#enableProwlarr").checked =
        data.enableProwlarr || false;
      document.querySelector("#prowlarrHost").value = data.prowlarrHost || "";
      document.querySelector("#enableJackett").checked =
        data.enableJackett || false;
      document.querySelector("#jackettHost").value = data.jackettHost || "";

      document.querySelector("#enableSubtitles").checked =
        data.enableSubtitles || false;
      document.querySelector("#osUsername").value =
        data.openSubtitlesUsername || "";
      document.querySelector("#osLanguages").value =
        data.subtitleLanguages || "es,ea,sp,en";
      const osKeyInput = document.querySelector("#osApiKey");
      osKeyInput.value = "";
      osKeyInput.placeholder = data.openSubtitlesApiKeySet
        ? "Saved (leave blank to keep)"
        : "Your OpenSubtitles API key";
      const osPasswordInput = document.querySelector("#osPassword");
      osPasswordInput.value = "";
      osPasswordInput.placeholder = data.openSubtitlesPasswordSet
        ? "Saved (leave blank to keep)"
        : "OpenSubtitles password";

      // a blank key field means keep the saved one
      const prowlarrKeyInput = document.querySelector("#prowlarrApiKey");
      prowlarrKeyInput.value = "";
      prowlarrKeyInput.placeholder = data.prowlarrApiKeySet
        ? "Saved (leave blank to keep)"
        : "Your Prowlarr API key";
      const jackettKeyInput = document.querySelector("#jackettApiKey");
      jackettKeyInput.value = "";
      jackettKeyInput.placeholder = data.jackettApiKeySet
        ? "Saved (leave blank to keep)"
        : "Your Jackett API key";

      const timeoutValue = data.searchTimeoutSeconds || "";
      document.querySelector("#prowlarrTimeout").value = timeoutValue;
      document.querySelector("#jackettTimeout").value = timeoutValue;
      document.querySelector("#prowlarrSkipTls").checked =
        data.skipTlsVerify || false;
      document.querySelector("#jackettSkipTls").checked =
        data.skipTlsVerify || false;

      // Set switch button state
      const switchInputs = document.querySelectorAll(".switchInput");
      switchInputs.forEach((input) => {
        const dot = input.querySelector(".dot");
        const wrapper = input.querySelector(".switch-wrapper");
        if (input.querySelector("input").checked) {
          dot.classList.add("translate-x-full", "!bg-muted");
          wrapper.classList.add("bg-primary");
        } else {
          dot.classList.remove("translate-x-full", "!bg-muted");
          wrapper.classList.remove("bg-primary");
        }
      });

      updateSearchVisibility();
    })
    .catch((error) => {
      console.error("There was a problem with the fetch operation:", error);
    });
})();
