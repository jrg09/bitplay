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

  // handle demo button
  const demoButton = document.querySelector("#demo_torrent");
  demoButton.addEventListener("click", async () => {
    document.getElementById("magnet").value =
      "magnet:?xt=urn:btih:08ada5a7a6183aae1e09d831df6748d566095a10&dn=Sintel&tr=udp%3A%2F%2Fexplodie.org%3A6969&tr=udp%3A%2F%2Ftracker.coppersurfer.tk%3A6969&tr=udp%3A%2F%2Ftracker.empire-js.us%3A1337&tr=udp%3A%2F%2Ftracker.leechers-paradise.org%3A6969&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337&tr=wss%3A%2F%2Ftracker.btorrent.xyz&tr=wss%3A%2F%2Ftracker.fastcast.nz&tr=wss%3A%2F%2Ftracker.openwebtorrent.com&ws=https%3A%2F%2Fwebtorrent.io%2Ftorrents%2F&xs=https%3A%2F%2Fwebtorrent.io%2Ftorrents%2Fsintel.torrent";

    startPlayback();
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
  async function startPlayback() {
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
    const chosen = await pickSource(sessionId, videoFiles[0]);

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
          label: videoFiles[0].name,
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
    autoLoadSubtitles(sessionId, videoFiles[0]);

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
        videoSelect.value = String(videoFiles[0].index);
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
          player.src({ src: next.src, type: next.type });
          setupChosenPlayback(next);
          autoLoadSubtitles(sessionId, file);
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
  let searchPageSize = 5;

  const updateSearchResults = () => {
    const searchPagination = document.querySelector("#search-pagination");
    const searchResults = document.querySelector("#search-result");
    searchResults.classList.remove("hidden");
    searchResults.querySelector("tbody").innerHTML = "";
    searchResults.querySelector("tfoot").classList.add("hidden");
    if (searchData.length === 0) {
      searchResults.querySelector("tfoot").classList.remove("hidden");
      return;
    }

    const start = (searchPage - 1) * searchPageSize;
    const end = start + searchPageSize;
    const results = searchData.slice(start, end);
    results.forEach((result) => {
      // indexer data is untrusted, build with textContent
      const row = document.createElement("tr");
      [
        result.title,
        result.indexer,
        result.size,
        `${result.leechers}/${result.seeders}`,
      ].forEach((text) => {
        const td = document.createElement("td");
        td.textContent = text ?? "";
        row.appendChild(td);
      });
      const actionTd = document.createElement("td");
      const watchBtn = document.createElement("button");
      watchBtn.type = "button";
      watchBtn.className = "btn small play-torrent";
      watchBtn.dataset.magnet = result.downloadUrl || result.magnetUrl || "";
      watchBtn.textContent = "Watch";
      actionTd.appendChild(watchBtn);
      row.appendChild(actionTd);
      searchResults.querySelector("tbody").appendChild(row);
    });

    // Generate pagination
    const totalResults = searchData.length;
    const totalPages = Math.ceil(totalResults / searchPageSize);
    generatePagination(
      searchPage,
      searchPageSize,
      totalResults,
      "#search-pagination"
    );

    // Add event listener to each play button
    searchResults.querySelectorAll(".play-torrent").forEach((el) => {
      el.addEventListener("click", async (e) => {
        const magnet = e.target.getAttribute("data-magnet");
        document.querySelector("#magnet").value = magnet;
        startPlayback();
        e.target.setAttribute("disabled", "disabled");
        e.target.innerHTML = "";
        e.target.classList.add("loader");
      });
    });
  };

  document.querySelector("#search-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const query = e.target.querySelector("#search").value;
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

    let apiUrl = "/api/v1/prowlarr/search";

    if (!settings?.enableProwlarr && settings?.enableJackett) {
      apiUrl = "/api/v1/jackett/search";
    }

    const timeoutMs = ((settings?.searchTimeoutSeconds || 30) + 5) * 1000;

    fetch(`${apiUrl}?q=${encodeURIComponent(query)}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      signal: AbortSignal.timeout(timeoutMs),
    })
      .then(async (res) => {
        if (!res.ok) {
          const err = await res.json().catch(() => ({}));
          throw new Error(err.error || "Failed to fetch search results");
        }
        return res.json();
      })
      .then((data) => {
        if (data && typeof data === "object") {
          searchData = data;
        } else {
          searchData = [];
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

        // Check if Prowlarr or Jackett is enabled
        if (body?.enableProwlarr || settings?.enableJackett) {
          searchWrapper.classList.remove("hidden");
        } else {
          searchWrapper.classList.add("hidden");
        }
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

      // Check if Prowlarr or Jackett is enabled
      if (body?.enableJackett || settings?.enableProwlarr) {
        searchWrapper.classList.remove("hidden");
      } else {
        searchWrapper.classList.add("hidden");
      }
    }

    submitButton.removeAttribute("disabled");
    submitButton.classList.remove("loader");
    submitButton.innerHTML = "Save Settings";
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
        data.subtitleLanguages || "es,en";
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

      // Check if Prowlarr or Jackett is enabled
      if (data?.enableProwlarr || data?.enableJackett) {
        searchWrapper.classList.remove("hidden");
      } else {
        searchWrapper.classList.add("hidden");
      }
    })
    .catch((error) => {
      console.error("There was a problem with the fetch operation:", error);
    });
})();
