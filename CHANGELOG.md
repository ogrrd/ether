# Changelog

## 0.3.7 (2026-11-09)

- **Quality:** client rules that pointed at custom or imported quality profiles
  removed during the `0.3.6` profile reset now repoint automatically on startup
  to the media's At home default profile while keeping device matchers and
  limits intact, instead of blocking console saves and profile imports. Saving
  or removing a profile or client rule in the console also settles any
  unrelated dangling rules.
- **Remote access, shared users and catalogue sync:** background catalogue
  index, popularity-rank and planner-statistics builds now queue in-process
  SQLite writers (including `DROP`/`ALTER` DDL) instead of timing them out at 10
  seconds; `plex.tv` registration starts after library sections are registered
  and the listener opens; `plex.tv` connectivity logs include the probed inbound
  address and port; shared-user sync retries promptly on write contention or
  transient errors and can be triggered on demand from **Viewers** or **Apps ›
  Plex › Remote access**; and catalogue sync progress keeps the library title
  count anchored instead of replacing it with scanned row counts (#407).

See the [release notes](release-notes/v0.3.7.md) for upgrade guidance.

## 0.3.6 (2026-11-09)

- **Decade channels:** CHROMA 80, RIOT 90, ORBIT 00 and SHIFT 10 ship on every
  install, with weekly schedules built from your catalogue, rebuilt era
  graphics and their own Apple TV logos; hidden until the catalogue can fill
  them, and a deleted one stays deleted.
- **Live TV:** commercials and bumpers from a link (one-click yt-dlp install),
  **Show in apps** per channel, stability presets with recovery and backup
  failover, IPTV refresh controls, guide improvements and Apple TV logos with
  remote access (#383, #391).
- **Quality:** profiles ranked the Radarr/Sonarr way with custom formats
  (builds on #355); optional alternatives at every resolution. Profiles reset
  to the new defaults on upgrade.
- **Collections:** none created on fresh installs; genres, networks and
  studios become templates; more than 200 templates in starter bundles,
  translated into all 20 languages.
- **Sources:** Statistics and Usage tabs with per-source drawers; Usenet season
  packs (#378) and disc images (#393); archive, Usenet and anime fixes; a dead
  debrid CDN node gets a fresh link (#396).
- **Playback speed:** detail-page warm-up on Jellyfin, Emby and Silo, Stremio
  stream-list warm-up, faster debrid starts, HDR/HEVC for native Stremio and
  Nuvio players, native handling and hedged starts for cached add-on streams.
- **Apps:** Android phones convert Dolby/DTS when streamed (#395), Plex for
  Roku 4K (#394), Infuse Direct Mode rows and Watchlist, Plex for LG (#375),
  Plex desktop downloads (#365), Plex account watch sync, Shield, Google TV
  and Mac device entries.
- **Database and console:** faster browsing on large libraries, a leaner
  catalogue tidied in the background, maintenance that no longer blocks writes,
  SQLite to PostgreSQL fix (#398), Overview and Activity agree, no false
  plex.direct warning, automatic updates on by default for new installs.

See the [release notes](release-notes/v0.3.6.md) for upgrade guidance and limits.

## 0.3.5 (2026-11-07)

- **On Air:** virtual channels rebuilt as continuous channels, with styles and
  Style Studio, break reels, personal channels and plugin segments.
- **Xtream and IPTV:** Xtream sources for Live TV, movies and shows; the
  **IPTV apps (Xtream)** app for IPTV players; per-viewer compatibility
  transcoding; AI guide matching; contrast-aware channel artwork.
- **Server roles on Kubernetes:** Helm chart with external PostgreSQL; full
  nodes plus source, transcode (CPU or NVIDIA), server and delivery roles, with
  playback recovery when a node is lost.
- **Quality:** separate movie and TV show profiles, Radarr/Sonarr imports with
  optional sync, burned-in subtitle handling, more release names recognised.
- **Apps:** closer Plex Media Server behaviour for playlists, play queues,
  downloads, tracks and live updates; fixes for #285, #315, #326, #331, #339,
  #341, #342, #347, #351, #354 and more.
- **Catalogue and console:** AI catalogue search, per-viewer catalogue rules,
  bulk Home row editing, poster badge fixes, database check, Cluster page.

See the [release notes](release-notes/v0.3.5.md) for upgrade guidance and limits.

## 0.3.1 (2026-11-01)

- **Bundled TMDB key and fallback guidance (`internal/tmdb`, `Dockerfile`, `.github/workflows/release.yml`, `web/`):** Official release builds and Docker images now bake in `internal/tmdb.BundledKey` so fresh installs populate a catalogue immediately. For custom builds without a bundled key, the setup finish screen, **Overview › Next steps**, and **Catalog** surface an **Add a TMDB key** link to **Server › Integrations › TMDB**.
- **Connect apps in Docker bridge mode and behind reverse proxies (`internal/app/frontend_portal.go`, `internal/admin/connect_apps.go`, `web/`):** Address resolution for `/ether/me › Connect apps` now checks configured custom/public URLs first, then the request `Host` header (or `X-Forwarded-Host` / `X-Forwarded-Proto` from trusted proxies) with the app port, before local interface subnet matching.
- **Accurate Usenet provider health (`internal/usenet`, `web/`):** Warmup dial and auth failures are recorded on the provider circuit breaker, and `lastError` / `Needs attention` persist across cooldowns until a probe or connection succeeds, with plain-language reasons for DNS, connection, TLS, timeout, and login errors.
- **12-character minimum for new local passwords (`internal/identity`, `internal/admin`, `web/`):** New local owner and viewer passwords require at least 12 characters across setup, **Sign-in methods**, **Viewers**, and the API, matching backup encryption requirements while leaving existing shorter passwords working.
- **Automatic update check toggle and `plex.tv` gating (`internal/updater`, `internal/plextv`, `internal/app`, `web/`):** Added an owner toggle under **Server › General › Updates** (`updates.enabled`, default `true`) to disable background update checks, and gated `plex.tv` publishing, friend sync, and metadata refreshes on the Plex app being enabled.
- **Startup next-step log and documentation (`internal/app/startup_guidance.go`, `README.md`, `docs/install.md`):** Logs one `INFO` line at startup pointing to the next setup step when incomplete, and updates `README.md` and `docs/install.md` with **Things to know first**, exact outbound network behaviour, consistent port `8096` and Docker Desktop guidance, Stremio HTTPS notes, and a troubleshooting table.

## 0.3.0 (2026-11-01)

### Summary

- **Multi-app server:** Ether now serves Plex, Jellyfin, Emby (including Infuse, Swiftfin, and Findroid), Silo, Stremio, read-only WebDAV, DLNA, and M3U/XMLTV from one install, with cross-app watch progress, shared subtitle preferences, personalized views, and Watch Party / SyncPlay rooms.
- **Session broker and capability-based playback:** One playback broker (`internal/sessions`) and client capability registry manage planning, stream grants, concurrent limits, bandwidth budgets, and owner Stop across all protocols, alongside faster Play start and mid-file range seeks.
- **Redesigned owner console in 19 languages:** Rebuilt around Library, Streaming, and Server sidebar groups, a single-level Sources view (Debrid, Usenet, Indexers, Stats), a first-run setup wizard with a language step and finish animation, an Overview Next steps bar, and 19 translated locales alongside English.
- **Releases and quality ranking ladder:** **Releases › Profiles** replaces separate scoring tabs with a single drawer ordered by how Ether picks releases: **Never play** filters, a draggable **What matters most** ranking ladder, and a **Tie-breaker** control, plus **Device rules** and **Searching** tabs.
- **Three new-release modes and unified age ratings:** **Catalog › Settings** replaces the availability table with three modes (*When they can play*, *On digital release*, *Right away*), and content ratings use one age-level equivalence model (`PG` = `TV-PG`) across visibility rules, collections, and viewer caps.
- **Paintable virtual Live TV channels and DASH:** The virtual channel drawer adds a weekly timezone-aware schedule grid, playout modes, repeat windows, bumpers, and DASH playback in Plex Web alongside HLS.
- **On-pick subtitle timing alignment, mojibake repair, and upload:** Picked external text subtitles are aligned against embedded container subtitle tracks or speech audio energy (fitting offset and frame-rate ratio), Windows-1252/Latin-1 mojibake is repaired, and Plex Web subtitle uploads are supported.
- **Plex protocol additions:** Full filter negation and string/numeric operators, secondary sorts and per-viewer `lastViewedAt`, indexed `guid=` lookups, 17-hub search with actor and director hubs, the Plex Web server Dashboard (`/statistics/*`), Plex for Mac HTTP Matroska conversion, converting offline downloads, and an optional **Use Plex's metadata for Plex apps** toggle.
- **Large-library performance architecture:** SQLite FTS5 trigram and people indexes, PostgreSQL `pg_trgm`, narrow covering indexes for browse counts, separate read/writer pools, generation-stamped caches, and a 540,000-title CI gate ensure no request path scales with library size.
- **Guided backup restore and startup recovery:** One-click snapshot restore with automatic rollback, in-app service restart across systemd, launchd, and Windows SCM, SQLite `quick_check` startup verification, and a startup status and recovery page.

---

### Full changelog

#### Apps and protocols

- **Shared frontend runtime (`internal/frontend`, `internal/experience`):** Protocol adapters mount independently through `frontend.Registry` and share one policy-filtered experience layer for home rows, continue watching, next up, search, item details, versions, and track selection. The owner console at `/ether/` stays available independently of protocol listeners.
- **Jellyfin (target 12.4) and Emby (target 4.10.2) frontends (`internal/frontends/mediabrowser`):** Native support for Infuse, Swiftfin, Findroid, Streamyfin, Jellyfin for Android TV, JellyCon, Jellyfin for Roku, and Emby native apps. Includes `/System/Info`, `/Users`, Quick Connect, `/UserViews`, `/Items`, `/Items/Latest`, `/UserItems/Resume`, `/Shows/NextUp` (with `seriesId`), `/Search/Hints`, `/PlaybackInfo` (estimate on detail pages, source resolution on play), direct byte-range streaming, HLS transcoding, `/MediaSegments` skip markers, WebSocket live updates (`/socket`), and UDP `7359` LAN discovery. Default ports are `8096` (Jellyfin) and `8097` (Emby), plus `/jellyfin` and `/emby` on the main port.
- **Personalized Home in Jellyfin and Emby:** Publishes each viewer's top personalized rows as virtual libraries in `/UserViews` (*For You*, *Because you watched…*, *Trending*), populates Jellyfin's *Suggestions* tab (`/Movies/Recommendations`), and sets default home section ordering for new viewers without overriding client-saved preferences. Configurable under **Apps › Jellyfin** and **Apps › Emby**.
- **Silo frontend (`internal/frontends/silo`):** Native `/api/v1` and `/api/v2` REST and WebSocket support for Silo on Apple TV, iOS, macOS, Android, and Android TV on default port `8090` (and `/silo`), including device code pairing, server-driven Home sections, version selection, HLS and direct playback, and real-time watch-state sync.
- **Watch Party and SyncPlay (`internal/watchparty`):** Shared room coordinator bridging Silo Watch Together (`/api/v2/watch-together`, WebSocket room sync) and Jellyfin `SyncPlay` (`/SyncPlay/*`, ` jellyfin-web`). Members open independent playback sessions through the broker with their own transcode decisions and stream limits while sharing one latency-compensated playhead clock.
- **Stremio addon (`internal/frontends/stremio`):** Viewers can mint a private HTTPS Stremio addon link in `/ether/me` to browse and stream their permitted catalogue and collections in Stremio.
- **Read-only WebDAV (`internal/frontends/webdav`):** Exposes `/Movies`, `/TV`, and `/Collections` as read-only virtual folders authenticated by per-viewer app passwords for Infuse, VLC, Kodi, and rclone.
- **DLNA (`internal/frontends/dlna`) and M3U/XMLTV (`internal/frontends/m3u`):** Local-network UPnP/DLNA `ContentDirectory` browsing governed by an owner-selected guest viewer profile (defaulting to a Kids `PG` preset), and per-viewer `playlist.m3u` and `guide.xml` links for IPTV clients such as Channels DVR and TiviMate.
- **Plex browse filters, sorts, and `guid=` lookups:** Added filter negation (`!=`, `!==`) and string/numeric operators (`<=`, `>=`, contains, begins with, ends with) for all advertised section filters including `originallyAvailableAt`, `inProgress`, and `audienceRating`. Added per-viewer `lastViewedAt` sorting, secondary sort keys, and indexed `guid=` lookups (`plex://`, `tmdb://`, `imdb://`, `tvdb://`) on `/library/all` and `/library/sections/{id}/all`.
- **Plex 17-hub search and people hubs:** `/hubs/search` and `/hubs/search/voice` return the full 17-hub set and ordering recorded from Plex Media Server fixtures, including indexed actor and director tag hubs and title `reason` attributes.
- **Plex Web server Dashboard:** Implemented owner-only `/statistics/bandwidth`, `/statistics/resources`, `/statistics/media`, and `/devices` routes backed by a bounded in-memory ring buffer of host/process CPU and memory and per-account, per-device LAN/WAN byte counters.
- **Optional Plex metadata for Plex apps:** Claimed servers can enable **Use Plex's metadata for Plex apps** under **Apps › Plex › Account** (off by default). When enabled, a background worker caches Plex metadata-provider fields (`metadata.provider.plex.tv`) for matched titles and serves them to Plex apps while keeping TMDB person IDs, artwork fallbacks, and TMDB metadata on all other protocols.
- **`plex.tv` account hygiene:** Derived a per-install metadata client identifier instead of a shared static string, paged `plexguid` season and episode children 50 at a time, subscribed to `notifySharingChange` and `notifyHomeChange` pubsub events for immediate friend/token sync (raising polling from 1 minute to 30 minutes), and filtered `100.64.0.0/10` (CGNAT/Tailscale) addresses from `plex.tv` connection publishing so LAN connections are not dropped.
- **Protocol conformance harnesses (`internal/conform`, `internal/pmsconform`):** Added `cmd/conformdiff` with recorded corpora and OpenAPI validation for Jellyfin, Emby, and Silo, a redacted client decision corpus (`Profile-Extra` verified), and CI failure gates on load-bearing Plex Media Server fields.

#### Playback

- **Session broker and delivery authority (`internal/sessions`):** Unified planning, opening, progress reporting, stopping, and cluster session state across all frontends. `sessions/9` tracks delivery authority per viewer, item, surface, and device across restarts, and staged verified encoder input leases prevent premature source retirement during transcode handoffs.
- **Unified client capability registry (`internal/clientcaps`):** Driven from one registry across Plex, Jellyfin, Emby, Silo, Stremio, and WebDAV, combining client profiles, runtime `Profile-Extra` declarations, and owner device rules.
- **Faster Play start and range seeks:** Play starts on the first verified healthy source instead of waiting for every indexer to finish, avoids waiting on unproven Usenet warm-up checks, and yields background debrid prefetch immediately when a player issues a mid-file HTTP `Range` seek.
- **DASH start buffer:** Reduced static and live DASH `minBufferTime` to one segment duration so Plex Web's Shaka player starts playback after a single segment.
- **Plex for Mac HTTP Matroska conversion:** Added `/video/:/transcode/universal/start` with `protocol=http`, streaming chunked Matroska from FFmpeg with offset-based seeks and full session lifecycle tracking.
- **Converting offline downloads:** When a client's download profile cannot play a plain MP4 remux (such as AV1, VP9, TrueHD, DTS, or image subtitles), Ether transcodes the download to an H.264/HEVC + AAC/AC3 MP4 using the hardware pipeline, bounded to one encode per viewer at a time.
- **FFmpeg filter probing and `zscale`-free fallback:** Startup probes record the exact filters and encoders supported by the installed `ffmpeg`. Builds without `libzimg` (`zscale`) use `colorspace`/`scale` for SDR colour-space conversions and `libplacebo`, OpenCL, or hardware tone mapping for HDR-to-SDR conversions without blind retries.
- **Subtitle timing auto-correction, mojibake repair, and upload:** On explicit subtitle selection, Ether aligns external text subtitles against embedded Matroska/MP4 text tracks via range reads or speech-energy cross-correlation over short audio windows, fitting constant offset and frame-rate ratio (`23.976` / `24` / `25` fps). Repairs UTF-8 mojibake mis-decoded through Windows-1252/Latin-1 and supports `POST /library/metadata/{id}/subtitles` uploads from Plex Web.
- **Cross-app subtitle preferences (`internal/trackprefs`):** Viewer subtitle mode (*Only for foreign audio*, *Always on*, *Off*), language order, and appearance sync across Silo, Jellyfin, Emby, Plex, and the owner console, preferring the viewer's language over a default-flagged dub.
- **Remux and segment resilience:** Resumes remux playback cleanly after a mid-title FFmpeg EOF, prevents empty remux restart loops at GOP boundaries, keeps the transcode window anchored on the segment the client is fetching, and stabilizes replay duration across sessions.
- **Surround audio conversion rule:** Added a per-device rule toggle under **Releases › Device rules** to convert Dolby (AC3/EAC3/TrueHD) and DTS audio tracks to AAC for clients or setups that need stereo/AAC output.

#### Sources and releases

- **Quality profile ranking ladder (`internal/resolver`, `internal/releasepolicy`):** Added an ordered `Ranking` ladder to `QualityProfile` covering *Prefer* rules, audio language, connection fit, resolution, release type, HDR, recently verified playback, video codec, audio format, and file size. Existing profiles preserve their exact ordering by default.
- **Release selection accuracy:** Enforced runtime-proportional 4K size floors so low-bitrate re-encodes do not outrank true 4K or high-bitrate 1080p releases, matched alternative and romanized original titles, and restricted the **Play Version** list to the active profile's resolution, HDR, and size ceiling unless *Offer higher versions* is enabled.
- **Named versions across apps:** **Play Version** (and the version picker in Jellyfin, Emby, and Silo) labels **Auto** with the release it picked and shows each alternative's resolution, release type, audio, size, and release group (#188).
- **Usenet spillover, learned limits, and live hedging (`internal/usenet`, `internal/stream`):** Busy primary Usenet providers spill over to the next primary provider with free connections without spending backfill block data. Ether learns server connection limits automatically from refusal responses, hedges stalled live article reads on spare capacity (#196), keeps NZB repost message-IDs as per-segment alternates (#198), coalesces concurrent `FetchNZB` requests, and falls back to the best timed-out candidate when nothing verifies in time.
- **Deep Matroska gap concealment and breaker accuracy:** Matroska damage concealment skips unaffected clusters so mid-file gaps deep into a movie are concealed within budget, background `STAT` checks run at prefetch priority instead of live priority, and the Usenet circuit breaker resets only on `BODY` success while allowing healthy idle sockets during dial cooldowns (#189).
- **Prowlarr tag sync (`internal/prowlarr`):** **Sources › Indexers › Import from Prowlarr** can filter by one or more Prowlarr tags, syncing every 15 minutes and retaining the last working set if Prowlarr is unreachable (#200).
- **Optional outbound proxy (`internal/egress`):** **Server › General › How Ether reaches the internet** routes indexer, Stremio addon, debrid, and Usenet traffic (via HTTP CONNECT or SOCKS5 for NNTP) through a configured proxy while keeping Plex, local discovery, and update checks on the direct connection.

#### Library and catalog

- **Three New releases modes:** **Catalog › Settings › New releases** replaces the availability settings table with three modes: **When they can play** (recommended), **On digital release**, and **Right away**, with an optional custom daily request ceiling under *Advanced* (#208).
- **Unified age-level content ratings (`internal/contentrating`):** Normalizes TMDB and IMDb/OMDb/MDBList certifications into structured country labels and numeric age levels so `PG`, `TV-PG`, and regional equivalents (`6+`/`7+`) match together across **Catalog › Visibility**, collections, and viewer rating caps. Unrated titles are controlled explicitly via *Not rated*, and foreign ratings remain region-qualified (`ca/G`) instead of masquerading as US ratings.
- **Historical and movie release dates:** Pre-1970 release timestamps are treated as known so classic movies and shows remain browsable as aired, and a movie's year stays on TMDB's primary release date when `addedAt` reflects a later theatrical release.
- **Live TV weekly virtual channel creator:** **Live TV** adds a single-drawer virtual channel editor with a paintable 7-day grid in the owner's timezone, smart filter and weighted-mix sources, per-block playout modes (in order, shuffle, random, marathon, newest first), repeat-avoidance windows, hour/half-hour clock alignment (cut or pad with bumpers and off-air slates), and live preview.

#### Owner console

- **Sidebar and information architecture:** Reorganized navigation into top-level **Overview / Activity / Viewers**, **Library** (*Catalog*, *Collections*, *Home*, *Live TV*, *Calendar*, *Metadata*), **Streaming** (*Sources*, *Releases*, *Cache*, *Playback*), and **Server** (*General*, *Apps*, *Integrations*, *Notifications*, *Cluster*, *Maintenance*).
- **Single-level Sources page:** Consolidated **Sources** into four tabs (**Debrid**, **Usenet**, **Indexers**, **Stats**) with logo cards, inline health and quota badges, drag-to-reorder priority, and progressive disclosure under *More options*.
- **Redesigned Releases tabs:** Split **Releases** into **Profiles**, **Device rules**, **Searching**, **Blocklist**, **Pinned**, and **History**, with *Default at home* and *Default away* badges directly on profile cards.
- **First-run setup wizard and Overview Next steps:** Added a 7-step setup flow (*Language*, *Apps*, *Account/Plex*, *Sources*, *Indexers*, *Cache*, *Catalogue*) that works with or without a Plex account, explains blocked macOS service starts or busy ports in plain language, preserves finished steps on reopen, plays a full-viewport Continuum assembly animation on finish, and shows a slim **Next steps** bar on **Overview**.
- **AI language features:** Rebuilt **Integrations › Language model** with status-first cards and switches for on-request subtitle translation, speech-to-text subtitle generation, and overview/tagline translation.
- **19 translated languages:** Full UI localization in Czech, Danish, German, Spanish, Finnish, French, Italian, Japanese, Korean, Norwegian Bokmål, Dutch, Polish, Brazilian Portuguese, Russian, Swedish, Turkish, Ukrainian, Simplified Chinese, and Traditional Chinese alongside English.

#### Viewers and sign-in

- **Protocol-neutral viewer identity (`internal/identity`):** Household viewer accounts own watch history, ratings, watchlists, and preferences across all apps. Surface-bound device grants can be inspected and revoked per device.
- **Viewer portal (`/ether/me`) and Sign-in methods:** Viewers can sign in with a local password or linked Plex account to approve Jellyfin Quick Connect or Silo device codes, set a local username and password, manage active devices, and generate Stremio, WebDAV, or M3U connection credentials.
- **Add viewer drawer and invite links:** Owners can create local viewer accounts with a username and password or mint one-time invite links from **Viewers › Add viewer**.
- **Viewer defaults from `plex.tv`:** Syncs each Plex viewer's `defaultAudioLanguage`, `autoSelectAudio`, `defaultSubtitleLanguage`, `autoSelectSubtitle`, share filters (`filterMovies`, `filterTelevision`), and download permission (`allowSync`) from `plex.tv` as the default layer beneath owner rules and explicit viewer choices, displayed read-only as **From Plex** on the viewer's **Access** and **Playback** tabs.
- **Reconciled outbound `plex.tv` watch sync (`internal/plexwatch`):** Separates send admission from delivery receipt and reconciles uncertain sends against `GET /state/{account}` on backoff, reconnect, or manual **Resolve**, preventing duplicate scrobbles (#207).

#### Performance and scale

- **Real-data harness and 540k-title CI gate (`cmd/etherperf`, `internal/perfharness`):** Replays recorded client request sequences against a 264k-title lab database and gates CI against a 540k-title synthetic database so no endpoint scales with library size (`docs/performance.md`).
- **Indexed search and browse totals:** Added SQLite FTS5 trigram indexes (`catalog_titles_fts`, `catalog_alt_titles_fts`), an FTS5 word-prefix people index (`catalog_person_names`), PostgreSQL `pg_trgm` GIN indexes, and narrow covering indexes for movie/show browse counts and newest-first episode/series walks.
- **Zero-copy settings reads and request memoization:** `config.Store.Read` serves an immutable settings snapshot on request paths (cutting per-call reads from 36 µs / 97 allocations to 97 ns / 0 allocations, and per-title section checks to 27 ns / 1 allocation, #203, #206), memoizes per-item point reads within a request, and batches watched-episode counts.
- **SQLite WAL checkpointer and split pools:** Separate read and writer SQLite connection pools with background WAL checkpointing prevent concurrent reads from queuing on macOS and Windows.

#### Cluster and databases

- **PostgreSQL parity for multi-app and catalog indexes:** All new identity, session broker, watch-party, Plex metadata cache, content-rating, and trigram search tables register through `internal/store` with tested SQLite and PostgreSQL migrations.
- **Guided backup restore, rollback, and in-app restart (`internal/backup`, `internal/service`):** Restoring a snapshot or encrypted `.etherbackup` previews the impact, journals every file move with `fsync`, restarts the service in place (systemd, launchd, Windows SCM, or container supervisor), and offers **Undo restore** on the Backups page (#193, #202).
- **Startup status page and SQLite integrity check:** Displays live database open and migration progress during startup, runs `PRAGMA quick_check`, and provides an owner password / recovery-code rescue screen if startup fails.

#### Security

- **Device-bound transcode session authentication:** Transcode playlist and segment routes (`/video/:/transcode/universal/session/...`) accept >=128-bit random session capabilities bound to the initiating device/client identifier and scoped strictly to segment/playlist routes, refusing encoded path traversal (`6b44ea51f`) and honoring valid stream grants even when an accompanying `X-Plex-Token` has expired.
- **Revalidated viewer policy on media reads:** Scoped media, subtitle, and segment requests recheck the viewer's current policy epoch and active grant before serving bytes, cutting off access when a viewer is revoked or restricted.
- **Scoped encoder loopback input leases:** FFmpeg loopback inputs use opaque single-claim `PlaybackEncoderInputs` leases validated against the exact loopback origin, session, and source lifetime, with control headers redacted from logs and diagnostics.
- **No owner-token claim fallback:** If exchanging a `plex.tv` claim token fails during setup, Ether returns a clear error instead of falling back to the owner's personal account token as the server token.

#### Fixes

- **#141:** Added native Jellyfin client support alongside Emby, Silo, Stremio, WebDAV, DLNA, and M3U/XMLTV.
- **#180:** Fixed empty HTTP `200` responses when encoding console JSON fails, isolated broken collection definitions so they cannot stall Plex Home or break the Collections and Live TV pages, and bounded cached collection hubs.
- **#184:** Added native Infuse support over Jellyfin, Emby, and WebDAV, including file-shaped `MediaSources`, `Static=true` direct playback and resume without a `PlaySessionId`, short stable image tags, and post-retirement progress recording.
- **#188:** Named the chosen release on **Auto** and added resolution, release type, audio, size, and release group labels to every **Play Version** entry across Plex, Jellyfin, Emby, and Silo.
- **#189:** Fixed Matroska gap concealment past the first cluster, demoted background Usenet `STAT` checks to prefetch priority, and stopped false Usenet circuit-breaker trips on stale idle sockets.
- **#190:** Promoted Plex Web HDR remuxes only when `Profile-Extra` affirmatively reports 10-bit colour depth and PQ transfer support.
- **#192:** Published plain LAN and WAN addresses to `plex.tv` in every secure-connections mode while keeping `httpsRequired=1`, so `plex.tv` marks LAN connections `local: true` instead of showing the server as *Remote*.
- **#193, #202:** Added startup SQLite integrity checks, a live startup status and recovery page, in-app service restarts, and guided one-click backup restores with automatic rollback.
- **#196, #197, #198:** Added Usenet cross-tier busy spillover, automatic server connection-limit learning, live article hedging on spare capacity, repost message-ID alternates, `FetchNZB` coalescing, and timed-out candidate fallback.
- **#199:** Cleared Safari HDR10 promotion when a client sends a PQ denial via `list=`.
- **#200:** Added Prowlarr tag filtering and periodic tag sync in **Sources › Indexers › Import from Prowlarr**.
- **#201:** Probed FFmpeg filters at startup and added `colorspace`/`scale` and `libplacebo`/OpenCL/hardware tone-map paths so builds without `libzimg` (`zscale`) do not fail on colour conversion or source swaps.
- **#203, #206:** Eliminated repeated `config.clone` allocations on Plex Home and browse paths via `config.Store.Read` snapshots and settings-free section ID checks, and batched watched-episode counts.
- **#204:** Added DASH streaming for virtual Live TV channels in Plex Web across programme boundaries, permitted WebVTT subtitles in live IPTV inputs, and fixed early virtual-channel drops on Apple TV.
- **#205:** Capped **Play Version** alternatives to the active quality profile's resolution, HDR, and size limits by default, and recorded in Release History whether Ether or the viewer/client selected the version.
- **#207:** Held uncertain outbound `plex.tv` watch-state sends behind an admission fence and reconciled them against `GET /state/{account}`.
- **#208:** Replaced the raw feed-request availability configuration with three **New releases** modes in **Catalog › Settings**, automatic per-indexer request budgets, and stored allowance tracking.
- **#209:** Implemented paged library-wide episode (`/library/sections/{id}/allLeaves`, `all?type=4`) and season (`all?type=3`) listings with `addedAt`/`updatedAt` filtering so Tracearr completes its initial TV sync and uses incremental updates.
- **#210:** Omitted empty or background-art-less items from the iOS Plex Home hero spotlight hub so force-reopened apps do not render a blank top banner.

#### Removed or changed behaviour

- **Console navigation and routes:** **Sources** sub-tabs are now **Debrid**, **Usenet**, **Indexers**, and **Stats** (`/sources/debrid`, `/sources/usenet`, `/sources/indexers`, `/sources/stats`). **Sources › Availability** moved to **Catalog › Settings › New releases** and **Releases › Searching**. **Releases** tabs are now **Profiles**, **Device rules**, **Searching**, **Blocklist**, **Pinned**, and **History**.
- **Viewers › Defaults page removed:** Household audio and subtitle defaults now live under **Playback › Audio & subtitles**, and watchlist monitoring alerts live under **Notifications › Watchlist alerts**. Per-viewer `plex.tv` defaults appear on each viewer's **Access** and **Playback** tabs.
- **Global `resolver.allowCam` removed:** CAM filtering is now controlled solely by each quality profile's **Allow CAM** switch under **Releases › Profiles › Never play**.
- **Database schema version:** Upgrading to `0.3.0` runs automatic migrations (`auth`, `sessions`, `catalog`, `plexmeta`, `watchparty`, and FTS5/trigram indexes) after taking an automatic pre-migration snapshot. Older `0.2.x` binaries cannot open a `0.3.0` database; roll back by restoring the pre-migration snapshot first.
- **Deferred to 0.3.1:** The multi-step hardware transcode fallback ladder (`full GPU -> encode-only -> CPU`) and automatic deinterlacing (`bwdif`), the deeper cold-start resolver rewrite for duplicate-named torrent hashes and obfuscated Usenet header probes, remembering the Play Version list for Watch Version (#214), and configurable indexer User-Agent presets (#213).

#### Credits

Thanks to **FarkasLabs** (#206, #207), **AlexBlomkvist (`bmkhib`)** (#203, #209), **TheIndieArmy** (#190), **mvanbaak** (#201), **XenoUniv3rse** (#192), and **evulhotdog** (#172, #213) for their code contributions, profiling, and detailed bug reports.
