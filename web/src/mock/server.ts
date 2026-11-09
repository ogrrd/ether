import { interpretDemo } from "./catalogInterpret";
import { indexerUsage, infrastructure, saveCost, sourceHistory } from "./source-stats";
import { matchesCatalogFilter } from "./catalogFilter";
import { createOnAirMock } from "./onair";
/**
 * In-memory fake of the Ether admin API. Used by `vite --mode mock` and by the
 * tests; never shipped. It evaluates collection rules against a generated
 * catalogue and builds context-aware Home rows so every screen behaves
 * realistically offline.
 */
import { createPrerollsMock } from "./prerolls";
import type {
  ActivityDay,
  CollectionSpec,
  Device,
  HomeLayout,
  HomeRow,
  LayoutPage,
  LayoutSection,
  Rule,
  Source,
  TasteOverride,
  TasteProfile,
  TitleCard,
  Viewer,
} from "../lib/contract";
import {
  collectionIdOf,
  daypartOf,
  overrideCount,
  pageOf,
  tidySchedule,
  whenMatches,
} from "../lib/layout";
import { assemble } from "./assemble";
import { normalizeSpec } from "../lib/rules";
import { resolutionGroups } from "../lib/qualityRanking";
import type {
  Bootstrap,
  LLMProvider,
  LoadSample,
  SystemLoad,
  ClusterNode,
  ClusterControl,
  ClusterControlRequest,
  ClusterStatus,
  EtherConfig,
  EtherUser,
  IdentityPreset,
  JoinTokenInfo,
  LogEntry,
} from "../lib/types";
import {
  COLLECTIONS,
  GENRES,
  KEYWORDS,
  NETWORKS,
  PEOPLE,
  PROVIDERS,
  STUDIOS,
  avatarFor,
  buildCatalog,
  card,
  hash,
  posterFor,
  type FakeTitle,
} from "./data";
import { createMetadataMock } from "./metadata";
import { createExperienceMock } from "./experience";
import { createMaintenance } from "./maintenance";
import { createOpsMock, UNREACHABLE } from "./ops";
import { createReleaseConsoleMock } from "./releases";
import { createQualityMock } from "./quality";
import { createSitesMock } from "./sites";
import { createTrashMock } from "./trash";
import { createCacheMock } from "./cache";
import { createCalendarMock } from "./calendar";
import { createLiveTVMock } from "./livetv";
import { createExtendMock } from "./extend";
import { TEMPLATE_BUNDLES, TEMPLATES } from "./templates";
import { NOT_RATED, RATING_LEVELS, canonicalRating } from "../lib/contentRatings";

/** A few other boards' labels per level, as the server lists them. */
const MOCK_EQUIVALENTS: Record<string, string[]> = {
  G: ["gb/U", "ca/G", "de/0", "br/L"],
  PG: ["gb/PG", "ca/PG", "de/6", "se/7", "br/10"],
  "PG-13": ["gb/12A", "ca/14A", "de/12"],
  R: ["gb/15", "ca/18A", "de/16"],
  "NC-17": ["gb/18", "de/18"],
};

const NOW = () => new Date();

export interface FakeOptions {
  liveTVEmpty?: boolean;
  onAirLayout?: string;
  capabilityOverrides?: Array<Record<string, unknown>>;
  bootstrap?: Partial<Bootstrap>;
  /** Artificial latency for realism in the browser; 0 in tests. */
  latencyMs?: number;
  /** Serve as a node of a three-node PostgreSQL cluster instead of a standalone server. */
  cluster?: boolean;
  /** Pin the server in a startup state (see mock/ops.ts), e.g. "migrating" or "failed:database_corrupt". */
  startup?: string;
  /** Milliseconds per step of a simulated restart or restore. */
  opsPace?: number;
  /** Viewers watching, for the restart and restore drawers. */
  viewers?: number;
  /** The cache storage changed and needs a restart. */
  cacheRestart?: boolean;
  /** Report a damaged SQLite database in the health checklist. */
  damagedDatabase?: boolean;
  /** Start with a restore report ("restored", "failed") or a scheduled restore ("pending"). */
  restore?: string;
  /** Household audio and subtitle choices, for the Playback screen in the browser mock. */
  householdAudio?: boolean;
  /** Seed pre-rolls, their recent plays and a video still converting. */
  prerolls?: boolean;
}

function json(body: unknown, status = 200): Response {
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}
function err(status: number, code: string, message: string): Response {
  return json({ error: { code, message } }, status);
}

/** Mirrors internal/config/clientid.go identityPresets. */
const IDENTITY_PRESETS: IdentityPreset[] = [
  { key: "ether", userAgent: "Ether/1.0" },
  {
    key: "sonarr",
    userAgent: "Sonarr/4.0.20.3014 (ubuntu 26.04)",
    headers: { Accept: "application/rss+xml, text/rss+xml, application/xml, text/xml" },
  },
  {
    key: "radarr",
    userAgent: "Radarr/6.4.4.10685 (ubuntu 26.04)",
    headers: { Accept: "application/rss+xml, text/rss+xml, application/xml, text/xml" },
  },
  {
    key: "prowlarr",
    userAgent: "Prowlarr/2.7.0.5723 (ubuntu 26.04)",
    headers: { Accept: "application/rss+xml, text/rss+xml, application/xml, text/xml" },
  },
  {
    key: "nzbhydra2",
    userAgent: "NZBHydra2 9.2.0",
    headers: { Accept: "application/xml", "Content-Type": "application/xml" },
  },
  { key: "sabnzbd", userAgent: "SABnzbd/5.1.3" },
  { key: "nzbget", userAgent: "nzbget/26.3", headers: { Accept: "*/*" } },
];

function defaultConfig(): EtherConfig {
  return {
    posters: {
      version: 1,
      enabled: false,
      source: "tmdb",
      baseUrl: "",
      apiKey: "",
      urlTemplate: "",
      accounts: {
        rpdb: {
          enabled: true,
          source: "rpdb",
          baseUrl: "",
          apiKey: "[configured]",
          urlTemplate: "",
        },
      },
    },
    availability: {
      mode: "playable",
      monitor: true,
      verify: false,
      preferVerified: true,
      newlyAvailable: true,
      indexerIds: [],
      debrid: true,
      pollMinutes: 30,
      maxPages: 3,
      feedRequests: 100,
      checkRequests: 100,
      grabs: 20,
      checks: 20,
      checkBytes: 536870912,
      reservePercent: 80,
    },
    server: {
      startupAnimation: true,
      port: 32400,
      bindAddress: "0.0.0.0",
      friendlyName: "Living Room Ether",
      moviesLibraryName: "Movies",
      tvShowsLibraryName: "TV Shows",
      machineIdentifier: "mock-machine",
      logLevel: "info",
      defaultStreamLimit: 2,
      maxStreams: 0,
    },
    plex: {
      token: "••••",
      clientIdentifier: "mock-client",
      ownerPlexId: 1,
      ownerUsername: "Braedon",
      claimed: true,
      remoteAccess: true,
      useUpnp: true,
      publicPort: 32400,
      publicHost: "",
      customUrls: null,
      certFile: "",
      keyFile: "",
      certHash: "abc",
      secureConnections: "preferred",
    },
    usenet: {
      providers: [
        {
          id: "p1",
          name: "Primary news",
          host: "news.example.com",
          port: 563,
          tls: true,
          username: "me",
          password: "••••",
          connections: 20,
          priority: 0,
          backfill: false,
          enabled: true,
        },
        {
          id: "p2",
          name: "Block account",
          host: "block.example.net",
          port: 563,
          tls: true,
          username: "me",
          password: "••••",
          connections: 8,
          priority: 1,
          backfill: true,
          enabled: true,
        },
      ],
    },
    indexers: [
      {
        id: "i1",
        name: "Indexer One",
        url: "https://indexer-one.example.com",
        apiKey: "••••",
        dailyLimit: 300,
        dailyGrabs: 50,
        priority: 0,
        enabled: true,
        movieCategories: null,
        tvCategories: null,
      },
      {
        id: "i2",
        name: "Indexer Two",
        url: "https://indexer-two.example.com",
        apiKey: "••••",
        dailyLimit: 25,
        dailyGrabs: 10,
        priority: 1,
        enabled: true,
        movieCategories: null,
        tvCategories: null,
      },
      {
        id: "i3",
        name: "Torrent Tracker",
        url: "https://tracker.example.org/api/v2.0/indexers/example/results/torznab",
        apiKey: "••••",
        dailyLimit: 0,
        dailyGrabs: 0,
        priority: 2,
        enabled: true,
        movieCategories: null,
        tvCategories: null,
        protocol: "torrent",
      },
      {
        id: "i4",
        name: "Stream Addon",
        url: "https://addon.example/manifest.json",
        apiKey: "",
        dailyLimit: 0,
        dailyGrabs: 0,
        priority: 3,
        enabled: true,
        movieCategories: null,
        tvCategories: null,
        protocol: "torrent",
        kind: "stremio",
      },
    ],
    debrid: {
      providers: [
        {
          id: "rd-1",
          name: "Real-Debrid",
          kind: "realdebrid",
          apiKey: "••••",
          priority: 0,
          enabled: true,
        },
        {
          id: "tb-1",
          name: "TorBox backup",
          kind: "torbox",
          apiKey: "••••",
          priority: 1,
          enabled: true,
        },
      ],
      cachedOnly: true,
      removeUncached: true,
    },
    cache: {
      dir: "/var/lib/ether/cache",
      budgetBytes: 200 * 1024 ** 3,
      prefetchNextEpisode: true,
      prefetchBytes: 64 * 1024 ** 2,
      keepInProgressWarm: true,
      nextEpisodeAt: 0.8,
    },
    catalog: {
      tmdbKey: "",
      language: "en-US",
      region: "US",
      minPopularity: 5,
      minVotes: 100,
      includeAdult: false,
      refreshInterval: "24h0m0s",
    },
    resolver: {
      healthSamples: 12,
      searchCacheTtl: "6h0m0s",
      unavailableCooloff: "24h0m0s",
      maxCandidates: 3,
      sameQualityAlternatives: 1,
      offerHigherVersions: false,
      blockedGroups: null,
      blockedTerms: null,
    },
    quality: {
      profiles: [
        {
          name: "4K",
          mediaType: "movie",
          maxHeight: 2160,
          minHeight: 1080,
          allowHdr: true,
          allowDolbyVision: true,
          qualityOrder: resolutionGroups(2160, 1080),
          customFormatScores: {
            "ether:viewer-language": 20000,
            "ether:remux": 6000,
            "ether:bluray": 4000,
            "ether:webdl": 2000,
            "ether:dolby-vision": 500,
            "ether:hdr": 500,
            "ether:codec-hevc": 12,
            "ether:codec-av1": 6,
            "ether:audio-truehd": 8,
            "ether:audio-eac3": 4,
            "ether:atmos": 3,
            "ether:audio-direct": 10,
            "ether:audio-transcode": -15,
            "ether:tone-mapping": -15,
          },
          minSizeMbPerMin: 15,
          maxSizeMbPerMin: 1000,
          preferredLanguages: null,
          allowCam: false,
        },
        {
          name: "1080p",
          mediaType: "movie",
          maxHeight: 1080,
          minHeight: 720,
          allowHdr: false,
          allowDolbyVision: false,
          qualityOrder: resolutionGroups(1080, 720),
          customFormatScores: {
            "ether:viewer-language": 10000,
            "ether:webdl": 2000,
            "ether:bluray": 1000,
            "ether:codec-h264": 12,
            "ether:codec-hevc": 6,
            "ether:audio-eac3": 12,
            "ether:audio-ac3": 8,
            "ether:audio-aac": 4,
            "ether:audio-direct": 10,
            "ether:audio-transcode": -15,
          },
          minSizeMbPerMin: 5,
          maxSizeMbPerMin: 150,
          preferredLanguages: null,
          allowCam: false,
        },
        {
          name: "1080p",
          mediaType: "tv",
          maxHeight: 1080,
          minHeight: 720,
          allowHdr: false,
          allowDolbyVision: false,
          qualityOrder: resolutionGroups(1080, 720),
          customFormatScores: {
            "ether:viewer-language": 10000,
            "ether:series-year-differs": -5000,
            "ether:webdl": 2000,
            "ether:bluray": 1000,
            "ether:codec-h264": 12,
            "ether:codec-hevc": 6,
            "ether:audio-eac3": 12,
            "ether:audio-ac3": 8,
            "ether:audio-aac": 4,
            "ether:audio-direct": 10,
            "ether:audio-transcode": -15,
          },
          minSizeMbPerMin: 5,
          maxSizeMbPerMin: 150,
          preferredLanguages: null,
          allowCam: false,
        },
      ],
      rules: [{ product: "Plex for Apple TV", mediaType: "movie", profile: "4K" }],
      default: "1080p",
      remote: "1080p",
      sync: { enabled: false, interval: "24h0m0s" },
    },
    transcode: {
      ffmpegPath: "",
      ffprobePath: "",
      hwAccel: "auto",
      vaapiDevice: "",
      tempDir: "/tmp/ether-transcode",
      maxTranscodes: 2,
      remoteMaxBitrateKbps: 8000,
      rtxVideo: {
        mode: "off",
        executablePath: "",
        sourceMaxHeight: 720,
        targetMaxHeight: 1080,
        quality: 2,
        maxConcurrent: 1,
      },
    },
    trakt: { enabled: false, clientId: "", clientSecret: "" },
    plexServers: { enabled: true },
    openSubtitles: { enabled: false, apiKey: "", username: "", password: "" },
    llm: { enabled: true, provider: "ollama", baseUrl: "", apiKey: "", model: "llama3" },
    updates: {
      enabled: true,
      channel: "stable",
      autoUpdate: true,
      feedUrl: "",
      maintenanceStart: "03:00",
      maintenanceEnd: "06:00",
    },
    spike: { enabled: false, localFiles: null },
    downloads: {
      enabled: true,
      allowFriends: false,
      maxConcurrent: 2,
      maxConcurrentPerUser: 1,
      diskBudgetBytes: 20 * 1024 ** 3,
      expiry: "24h0m0s",
      maxHeight: 1080,
    },
    personalize: {
      postPlay: true,
      postPlayPrebuffer: true,
      microReasons: true,
      sessionIntent: true,
      clockFormat: "",
    },
    extras: { enabled: false, resolverTimeout: "20s", prewarmBytes: 48 * 1024 ** 2 },
    markers: { theIntroDb: true, apiKey: "" },
    setupComplete: true,
  };
}

const VIEWERS: Viewer[] = [
  {
    id: 1,
    plexId: 1001,
    username: "owner",
    title: "Alex Rivera",
    isOwner: true,
    lastActive: "",
    plays30d: 42,
    hoursWatched30d: 61.5,
    lifecycle: {
      source: "owner",
      paused: false,
      plexShared: false,
      canRemove: false,
      canMerge: false,
      canPause: false,
      managedBy: "",
    },
  },
  {
    id: 2,
    plexId: 1002,
    username: "sam",
    title: "Sam Rivera",
    isOwner: false,
    lastActive: "",
    plays30d: 27,
    hoursWatched30d: 33.2,
    lifecycle: {
      source: "local",
      paused: false,
      plexShared: false,
      canRemove: true,
      canMerge: true,
      canPause: true,
      managedBy: "",
    },
  },
  {
    id: 3,
    plexId: 1003,
    username: "kids",
    title: "The Kids",
    isOwner: false,
    lastActive: "",
    plays30d: 58,
    hoursWatched30d: 24.8,
    lifecycle: {
      source: "plexHome",
      paused: false,
      plexShared: true,
      canRemove: false,
      canMerge: false,
      canPause: false,
      managedBy: "plex",
    },
  },
  {
    id: 4,
    plexId: 1004,
    username: "jordan",
    title: "Jordan Lee",
    isOwner: false,
    lastActive: "",
    plays30d: 6,
    hoursWatched30d: 9.1,
    lifecycle: {
      source: "plex",
      paused: false,
      plexShared: true,
      canRemove: false,
      canMerge: false,
      canPause: false,
      managedBy: "plex",
    },
  },
];

/** Lifecycle as the real API reports it: Plex accounts are managed on plex.tv. */
function mockLifecycle(
  v: Pick<Viewer, "isOwner" | "plexId" | "title" | "username" | "accessStatus" | "lifecycle">,
) {
  if (v.lifecycle) return v.lifecycle;
  if (v.isOwner) {
    return {
      source: "owner" as const,
      paused: false,
      managedBy: "" as const,
      canPause: false,
      canRemove: false,
      canMerge: false,
    };
  }
  if (v.title === "Living-room guest" && v.plexId === 0 && !v.username) {
    return {
      source: "dlnaGuest" as const,
      paused: false,
      managedBy: "" as const,
      canPause: true,
      canRemove: true,
      canMerge: true,
    };
  }
  if (v.plexId > 0) {
    return {
      source: "plex" as const,
      paused: false,
      plexShared: v.accessStatus !== "inactive",
      managedBy: "plex" as const,
      canPause: false,
      canRemove: false,
      canMerge: false,
    };
  }
  return {
    source: "local" as const,
    paused: v.accessStatus === "inactive",
    managedBy: "" as const,
    canPause: true,
    canRemove: true,
    canMerge: true,
  };
}

const TASTE_SEEDS: Record<number, string[]> = {
  1: ["Science Fiction", "Thriller", "Mystery", "Crime"],
  2: ["Romance", "Comedy", "Drama", "Music"],
  3: ["Animation", "Family", "Adventure", "Fantasy"],
  4: ["Documentary", "History", "War"],
};

function decadeOf(year?: number): string {
  return year ? String(Math.floor(year / 10) * 10) : "";
}

function durationMs(w: string): number {
  const m = /^(\d+)([dwmy])$/.exec(w);
  if (!m) return 0;
  const n = Number(m[1]);
  return n * { d: 1, w: 7, m: 30, y: 365 }[m[2] as "d" | "w" | "m" | "y"] * 86400000;
}

export function createFakeServer(opts: FakeOptions = {}) {
  const xtreamGrants = new Map<string, { hash: string; surface: string; lastUsed: number }[]>();
  const catalog = buildCatalog();
  const byKey = new Map(catalog.map((t) => [t.key, t]));
  const ownerTags = new Map<string, string[]>();
  const watchlistChanges = new Map<string, boolean>();
  const savedForViewer = (viewerId: number, key: string) =>
    watchlistChanges.get(`${viewerId}:${key}`) ?? Number(key.slice(-2)) % 4 === 0;
  const releaseConsole = createReleaseConsoleMock(catalog);
  const trash = createTrashMock();
  const maintenance = createMaintenance();
  const experience = createExperienceMock();
  const sites = createSitesMock();
  const cacheMock = createCacheMock(opts.cacheRestart);
  const liveTV = createLiveTVMock(catalog, opts.liveTVEmpty);
  const onAir = createOnAirMock(liveTV, opts.onAirLayout);
  const calendar = createCalendarMock();
  const qualityMock = createQualityMock();
  const safetyPolicies = new Map<number, Record<string, unknown>>();
  const simklAccounts = new Map<number, { connected: boolean; lastSync?: string }>();
  const extendMock = createExtendMock();
  const prerolls = createPrerollsMock({
    seed: opts.prerolls,
    title: (key) => byKey.get(key),
    viewerName: (id) => VIEWERS.find((v) => v.id === id)?.title ?? `Viewer ${id}`,
  });
  const state = {
    boot: {
      ok: true,
      version: "0.9.0-mock",
      setupComplete: true,
      authenticated: true,
      authRequired: false,
      serverName: "Living Room Ether",
      signInMethods: ["local", "plex"],
      ...opts.bootstrap,
    } as Bootstrap,
    config: defaultConfig(),
    // Null until a caller opts in (the browser mock does). Viewer tests still
    // overlay these routes on a 404 from the shared fake.
    streamDefaults: (opts.householdAudio
      ? {
          audioLanguages: ["en"],
          subtitleLanguages: ["en"],
          preferOriginal: true,
          subtitleMode: "foreign-only",
          subtitleStyle: "normal",
          loudness: false,
          nightMode: false,
          dialogueBoost: false,
        }
      : null) as null | {
      audioLanguages: string[];
      subtitleLanguages: string[];
      preferOriginal: boolean;
      subtitleMode: string;
      subtitleStyle: string;
      loudness: boolean;
      nightMode: boolean;
      dialogueBoost: boolean;
    },
    pausedViewers: new Set<number>(),
    removedViewers: new Set<number>(),
    users: VIEWERS.map<EtherUser>((v) => ({
      id: v.id,
      plexId: v.plexId,
      username: v.username,
      title: v.title,
      thumb: avatarFor(v.title),
      isOwner: v.isOwner,
      lifecycle: v.lifecycle,
      streamLimit: 0,
      maxContentRating: v.id === 3 ? "PG" : "",
      allowedSections: [],
    })),
    favoritesMirror: new Map<number, boolean>(),
    dlnaGuests: [] as Viewer[],
    addedViewers: [] as Viewer[],
    collections: [] as CollectionSpec[],
    overrides: new Map<number, TasteOverride[]>(),
    layout: {
      global: [
        {
          section: "home",
          rows: [
            { row: "continue-watching", status: "always", slot: 1 },
            { row: "short-on-time", status: "eligible", when: { days: [1, 2, 3, 4, 5] } },
            { row: "something-different", status: "eligible", maxDaysPerWeek: 3 },
          ],
        },
      ],
      viewers: [
        {
          userId: 3,
          pages: [
            {
              section: "home",
              maxRows: 8,
              rows: [{ row: "short-on-time", status: "always", maxSlot: 3 }],
            },
          ],
        },
      ],
      updatedAt: new Date(Date.now() - 86400000).toISOString(),
    } as HomeLayout,
    logs: [] as LogEntry[],
    calls: [] as { method: string; path: string; body: unknown }[],
    plexDefaults: {} as Record<number, Record<string, unknown>>,
    clustered: opts.cluster ?? false,
    joinTokens: [] as JoinTokenInfo[],
    backupSettings: {
      intervalHours: 24,
      retain: 7,
      lastRun: 0,
      sharedDir: "",
      skipOnAirMedia: false,
    },
    backups: [] as {
      name: string;
      size: number;
      created: string;
      node?: string;
      nodeId?: string;
      shared?: boolean;
      remote?: boolean;
    }[],
  };
  {
    const ago = (h: number) => new Date(Date.now() - h * 3600_000).toISOString();
    const stamp = (h: number) => `ether-${ago(h).slice(0, 19).replace(/[-:]/g, "")}.tar.gz`;
    state.backups = state.clustered
      ? [
          { name: stamp(2), size: 6_200_000, created: ago(2), node: "den", nodeId: "n-den" },
          {
            name: stamp(26),
            size: 6_100_000,
            created: ago(26),
            node: "attic",
            nodeId: "n-attic",
            remote: true,
          },
        ]
      : [{ name: stamp(2), size: 4_200_000, created: ago(2) }];
    state.joinTokens = state.clustered
      ? [
          {
            id: "t7k2",
            createdAt: ago(30),
            expiresAt: ago(29),
            usedAt: ago(29.5),
            usedBy: "attic",
          },
          {
            id: "expired-example",
            createdAt: ago(2),
            expiresAt: ago(1),
          },
          {
            id: "q9wd",
            createdAt: ago(0.2),
            expiresAt: new Date(Date.now() + 40 * 60_000).toISOString(),
          },
        ]
      : [];
  }
  let seq = 1;

  // Seed collections from templates so the gallery isn't empty.
  for (const id of ["mood-edge-of-your-seat", "recently-added", "family-night", "short-under-90"]) {
    const t = TEMPLATES.find((x) => x.id === id)!;
    const spec = normalizeSpec({
      ...t.spec,
      // One seeded collection is a scheduled Home row, not a Collections-tab
      // entry, so the Home designer has a collection schedule to edit. It is
      // renamed so it does not share a name with the built-in Short on Time row.
      ...(id === "short-under-90"
        ? {
            name: "Weeknight wind-down",
            targets: {
              ...t.spec.targets,
              collectionsTab: false,
              homeRow: true,
              schedule: { days: [1, 2, 3, 4], fromHour: 18 },
            },
          }
        : {}),
      id: `c${seq++}`,
      enabled: id !== "family-night",
      updatedAt: NOW().toISOString(),
      lastBuiltAt: NOW().toISOString(),
    });
    spec.itemCount = evaluate(spec).total;
    Object.assign(spec, { ratingKey: String(20000 + seq) });
    state.collections.push(spec);
  }
  state.collections.push({
    ...normalizeSpec({
      name: "Recently added",
      description: "Plex's built-in recently added row.",
      sources: [{ kind: "catalog" }],
      sort: "added",
    }),
    id: "builtin-recent",
    builtIn: true,
    enabled: true,
    itemCount: 120,
  });

  const levels = ["debug", "info", "info", "info", "warn", "error"];
  for (let i = 0; i < 80; i++) {
    const lvl = levels[i % levels.length] as string;
    state.logs.push({
      time: new Date(Date.now() - (80 - i) * 45000).toISOString(),
      level: lvl.toUpperCase(),
      message:
        lvl === "error"
          ? "segment fetch failed"
          : lvl === "warn"
            ? "indexer budget low"
            : "served hubs",
      attrs: { component: i % 2 ? "plex" : "resolver", user: "owner" },
    });
  }

  // ---- Collection evaluation ---------------------------------------------------

  function candidates(spec: CollectionSpec): FakeTitle[] {
    const media = (t: FakeTitle) => spec.mediaType === "mixed" || t.type === spec.mediaType;
    const out = new Map<string, FakeTitle>();
    for (const s of spec.sources) {
      for (const t of sourceTitles(s)) if (media(t) || s.kind === "titles") out.set(t.key, t);
    }
    return [...out.values()];
  }

  function sourceTitles(s: Source): FakeTitle[] {
    switch (s.kind) {
      case "catalog":
        return catalog;
      case "titles":
        return s.keys.map((k) => byKey.get(k)).filter((t): t is FakeTitle => Boolean(t));
      case "tmdbPerson":
        return catalog.filter((t) => t.people.includes(s.id));
      case "tmdbKeyword":
        return catalog.filter((t) => t.keywords.includes(s.id));
      case "tmdbCompany":
        return catalog.filter((t) => t.studio === s.id);
      case "tmdbNetwork":
        return catalog.filter((t) => t.network === s.id);
      case "tmdbCollection":
        return catalog.filter((t) => t.collectionId === s.id);
      case "watchProvider":
        return catalog.filter((t) => t.providers.includes(s.id));
      case "tmdbList":
        return [...catalog]
          .sort((a, b) => (hash(s.list + a.key) % 1000) - (hash(s.list + b.key) % 1000))
          .slice(0, 40);
      default:
        return catalog.filter((t) => hash(JSON.stringify(s) + t.key) % 5 === 0);
    }
  }

  function fieldValues(t: FakeTitle, field: string): (string | number | boolean)[] {
    switch (field) {
      case "genre":
        return t.genres ?? [];
      case "keyword":
        return t.keywords;
      case "person":
        return t.people;
      case "studio":
        return [t.studio];
      case "network":
        return t.network ? [t.network] : [];
      case "country":
        return [t.country];
      case "language":
        return [t.language];
      case "year":
        return t.year ? [t.year] : [];
      case "decade":
        return [decadeOf(t.year)];
      case "rating":
        return [t.rating ?? 0];
      case "votes":
        return [t.votes];
      case "popularity":
        return [t.popularity];
      case "runtime":
        return [t.runtimeMinutes ?? 0];
      case "contentRating":
        return [t.contentRating ? canonicalRating(t.contentRating) : NOT_RATED];
      case "released":
        return [t.released];
      case "addedAt":
        return [t.addedAt.slice(0, 10)];
      case "watchProvider":
        return t.providers;
      case "collection":
        return t.collectionId ? [t.collectionId] : [];
      case "watched":
        return [hash("w" + t.key) % 4 === 0];
      case "inProgress":
        return [hash("p" + t.key) % 9 === 0];
      default:
        return [];
    }
  }

  type VisibilityGroup = { action?: string; mediaType: string; match: string; rules: Rule[] };
  function hiddenByRules(title: FakeTitle, groups: VisibilityGroup[], fallback: string) {
    const first = groups.find(
      (g) =>
        (g.mediaType === "mixed" || g.mediaType === title.type) &&
        (g.match === "any"
          ? g.rules.some((r) => matches(title, r))
          : g.rules.every((r) => matches(title, r))),
    );
    return first ? first.action !== "show" : fallback === "hide";
  }

  function matches(t: FakeTitle, r: Rule): boolean {
    if (r.field === "contentRating") return ratingMatches(t, r);
    const vals = fieldValues(t, r.field);
    const eq = (a: unknown, b: unknown) => String(a).toLowerCase() === String(b).toLowerCase();
    switch (r.op) {
      case "in":
        return Array.isArray(r.value) && r.value.some((v) => vals.some((x) => eq(x, v)));
      case "notIn":
        return !(Array.isArray(r.value) && r.value.some((v) => vals.some((x) => eq(x, v))));
      case "is":
        return vals.some((x) => eq(x, r.value));
      case "isNot":
        return !vals.some((x) => eq(x, r.value));
      case "gte":
        return vals.some((x) =>
          typeof x === "number" ? x >= Number(r.value) : String(x) >= String(r.value),
        );
      case "lte":
        return vals.some((x) =>
          typeof x === "number" ? x <= Number(r.value) : String(x) <= String(r.value),
        );
      case "between": {
        const [lo, hi] = Array.isArray(r.value) ? r.value : [];
        return vals.some((x) =>
          typeof x === "number"
            ? x >= Number(lo) && x <= Number(hi)
            : String(x) >= String(lo) && String(x) <= String(hi),
        );
      }
      case "within": {
        const since = new Date(Date.now() - durationMs(String(r.value))).toISOString().slice(0, 10);
        return vals.some((x) => String(x) >= since);
      }
      case "unknown":
        return vals.length === 0 || vals.every((x) => x === "" || x == null);
      case "known":
        return vals.some((x) => x !== "" && x != null);
    }
  }

  /** Content ratings compare by level, as on the server. */
  function ratingMatches(t: FakeTitle, r: Rule): boolean {
    const have = fieldValues(t, "contentRating")[0] as string;
    const want = (Array.isArray(r.value) ? r.value : [r.value]).map((v) =>
      canonicalRating(String(v)),
    );
    const bound = want[0] ?? "";
    const order = RATING_LEVELS.map((l) => l.value);
    switch (r.op) {
      case "in":
      case "is":
        return want.includes(have);
      case "notIn":
      case "isNot":
        return !want.includes(have);
      case "gte":
        return have !== NOT_RATED && order.indexOf(have) >= order.indexOf(bound);
      case "lte":
        return have !== NOT_RATED && order.indexOf(have) <= order.indexOf(bound);
    }
    return false;
  }

  function evaluate(spec: CollectionSpec) {
    const started = performance.now();
    const warnings: string[] = [];
    let items = candidates(spec);
    if (spec.rules.length)
      items = items.filter((t) =>
        spec.match === "any"
          ? spec.rules.some((r) => matches(t, r))
          : spec.rules.every((r) => matches(t, r)),
      );
    if (spec.exclude.length) items = items.filter((t) => !spec.exclude.some((r) => matches(t, r)));
    const dir = spec.sortDesc ? -1 : 1;
    const sorters: Record<string, (a: FakeTitle, b: FakeTitle) => number> = {
      popularity: (a, b) => (a.popularity - b.popularity) * dir,
      rating: (a, b) => ((a.rating ?? 0) - (b.rating ?? 0)) * dir,
      released: (a, b) => a.released.localeCompare(b.released) * dir,
      added: (a, b) => a.addedAt.localeCompare(b.addedAt) * dir,
      title: (a, b) => a.title.localeCompare(b.title) * dir,
      random: (a, b) =>
        (hash(a.key + new Date().toDateString()) % 997) -
        (hash(b.key + new Date().toDateString()) % 997),
      personal: (a, b) => (hash("me" + b.key) % 100) - (hash("me" + a.key) % 100),
    };
    const sorter = sorters[spec.sort];
    if (sorter) items.sort(sorter);
    if (
      spec.sources.some((s) =>
        [
          "traktList",
          "letterboxdList",
          "mdblist",
          "anilistList",
          "tmdbUserList",
          "stevenlu",
        ].includes(s.kind),
      )
    )
      warnings.push(
        "External lists are refreshed on the schedule; the preview shows the last fetched copy.",
      );
    if (items.length === 0 && spec.rules.length)
      warnings.push("No titles match every rule. Try “any” instead of “all”, or loosen a rule.");
    const total = spec.limit > 0 ? Math.min(spec.limit, items.length) : items.length;
    const limited = spec.limit > 0 ? items.slice(0, spec.limit) : items;
    const shown =
      spec.rotate && spec.rotate.size > 0 ? limited.slice(0, spec.rotate.size) : limited;
    return {
      total,
      items: shown.slice(0, 60).map(card),
      warnings,
      tookMs: Math.max(1, Math.round(performance.now() - started)) + 9,
    };
  }

  // ---- Personalization ---------------------------------------------------------------

  function taste(userId: number): TasteProfile {
    const seeds = TASTE_SEEDS[userId] ?? ["Drama"];
    const overrides = state.overrides.get(userId) ?? [];
    const genres = GENRES.map((g) => {
      const i = seeds.indexOf(g);
      let w = i >= 0 ? 0.9 - i * 0.18 : ((hash(g + userId) % 100) / 100) * 0.6 - 0.35;
      const o = overrides.find((x) => x.kind === "genre" && x.value === g);
      if (o) w = o.weight;
      return { name: g, weight: Math.round(w * 100) / 100 };
    }).sort((a, b) => b.weight - a.weight);
    return {
      userId,
      updatedAt: new Date(Date.now() - 3600_000).toISOString(),
      confidence: userId === 4 ? 0.22 : 0.55 + (userId % 3) * 0.12,
      genres,
      people: PEOPLE.slice(userId, userId + 5).map((p, i) => ({
        id: p.id,
        name: p.name,
        role: p.role,
        weight: Math.round((0.8 - i * 0.15) * 100) / 100,
        image: avatarFor(p.name),
      })),
      keywords: KEYWORDS.slice(0, 6).map((k, i) => ({
        name: k.name,
        weight: Math.round((0.7 - i * 0.22) * 100) / 100,
      })),
      decades: ["1970", "1980", "1990", "2000", "2010", "2020"].map((d, i) => ({
        decade: Number(d),
        weight: Math.round(Math.max(0, Math.sin((i + userId) / 2)) * 100) / 100,
      })),
      languages: [
        { code: "en", weight: 0.92 },
        { code: userId === 1 ? "ko" : "fr", weight: 0.31 },
        { code: "ja", weight: userId === 3 ? 0.4 : -0.1 },
      ],
      habits: {
        weeknightMinutes: userId === 3 ? 45 : 95,
        weekendMinutes: userId === 3 ? 120 : 170,
        preferredDevice: userId === 2 ? "mobile" : userId === 3 ? "tv" : "tv",
        bingeScore: userId === 1 ? 0.72 : 0.38,
        rewatchScore: userId === 3 ? 0.81 : 0.24,
        peakHours: userId === 3 ? [16, 17, 18] : [20, 21, 22],
      },
      overrides,
    };
  }

  function pickTitles(seed: string, n: number, filter?: (t: FakeTitle) => boolean): FakeTitle[] {
    const pool = filter ? catalog.filter(filter) : catalog;
    return [...pool]
      .sort((a, b) => (hash(seed + a.key) % 10007) - (hash(seed + b.key) % 10007))
      .slice(0, n);
  }

  // ---- Homes (layered Home Designer) ---------------------------------------------------

  const DEFAULT_ROWS: Record<LayoutSection, number> = { home: 10, movies: 8, shows: 8 };

  /** Candidate rows the "engine" proposes for a viewer (null = the typical viewer). */
  function candidatesFor(
    userId: number | null,
    device: string,
    at: Date,
    section: LayoutSection,
    layout: HomeLayout,
  ): HomeRow[] {
    const day = at.getDay();
    const hour = at.getHours();
    const weekend = day === 0 || day === 6;
    const t = userId === null ? null : taste(userId);
    const top = t?.genres[0]?.name ?? "Drama";
    const media =
      section === "movies"
        ? (x: FakeTitle) => x.type === "movie"
        : section === "shows"
          ? (x: FakeTitle) => x.type === "show"
          : () => true;
    const short = device === "mobile" || (!weekend && hour >= 21);
    const seed = `${userId ?? "typical"}-${section}`;
    const prefix = section === "movies" ? "movie" : section === "shows" ? "tv" : "home";
    const mk = (
      stem: string,
      title: string,
      kind: string,
      reason: string,
      score: number,
      items: FakeTitle[],
      source: HomeRow["source"] = "engine",
      why?: (t: FakeTitle) => string,
    ): HomeRow => ({
      hubId: `${prefix}.${stem}`,
      title,
      kind,
      reason,
      score: Math.round(score * 100) / 100,
      source,
      items: items.map(card),
      itemReasons: why ? Object.fromEntries(items.map((x) => [x.key, why(x)])) : undefined,
    });
    const rows: HomeRow[] = [];
    if (userId === null) {
      rows.push(
        mk(
          "top10.today",
          "Top 10 on Living Room Ether Today",
          "top-10-today",
          "Trending now, catalogue popularity and what viewers here watch.",
          0.9,
          pickTitles(seed + "t10", 10, media),
        ),
        mk(
          "showcase.thriller",
          "Acclaimed Thrillers",
          "genre-showcase",
          "Acclaimed, popular thrillers: what a new viewer plays is the first taste evidence.",
          0.74,
          pickTitles(seed + "sc1", 12, (x) => media(x) && (x.genres ?? []).includes("Thriller")),
        ),
        mk(
          "showcase.comedy",
          "Acclaimed Comedies",
          "genre-showcase",
          "Acclaimed, popular comedies.",
          0.7,
          pickTitles(seed + "sc2", 12, (x) => media(x) && (x.genres ?? []).includes("Comedy")),
        ),
        mk(
          "newreleases",
          "New Releases",
          "new-releases",
          "The newest notable releases.",
          0.62,
          pickTitles(seed + "nr", 12, media),
        ),
        mk(
          "popular.server",
          "Popular on Living Room Ether",
          "popular-on-server",
          "What viewers here watched most this month.",
          0.58,
          pickTitles(seed + "ps", 12, media),
        ),
        mk(
          "toprated",
          "Top Rated",
          "top-rated",
          "Acclaimed titles with trustworthy ratings.",
          0.5,
          pickTitles(seed + "tr", 12, (x) => media(x) && (x.rating ?? 0) > 7.4),
        ),
        mk(
          "trending",
          "Trending This Week",
          "trending",
          "What is trending on TMDB this week.",
          0.46,
          pickTitles(seed + "trn", 12, media),
        ),
        mk(
          "shortontime",
          "Short on Time",
          "short-on-time",
          "Shorter films and episodes.",
          short ? 0.72 : 0.4,
          pickTitles(seed + "sh", 12, (x) => media(x) && (x.runtimeMinutes ?? 999) <= 95),
        ),
      );
    } else {
      const anchor = pickTitles(seed + "anchor", 1, media)[0];
      const person = PEOPLE[userId];
      rows.push(
        mk(
          "top10",
          "Top 10 for You",
          "top-10",
          `The ten titles the taste model rates highest; leans on ${top}.`,
          0.91,
          pickTitles(seed + "top", 10, (x) => media(x) && (x.genres ?? []).includes(top))
            .concat(pickTitles(seed + "top2", 10, media))
            .slice(0, 10),
          "engine",
          (x) => `${(x.genres ?? []).join(", ")} · rated ${x.rating}`,
        ),
        mk(
          `because.${anchor?.key ?? 0}`,
          `Because You Watched ${anchor?.title ?? "something"}`,
          "because-you-watched",
          `Similar keywords and people to ${anchor?.title}.`,
          0.84,
          pickTitles(seed + "byw", 12, media),
          "engine",
          (x) => `Shares ${(x.genres ?? [])[0] ?? "themes"} and keywords with ${anchor?.title}`,
        ),
        mk(
          `person.${person?.id ?? 0}`,
          `More from ${person?.name}`,
          "more-from-person",
          `You've finished ${2 + userId} titles with ${person?.name}.`,
          0.77,
          pickTitles(seed + "mf", 9, media),
        ),
        mk(
          "trending.taste",
          "Trending in Your Taste",
          "trending-in-your-taste",
          `Popular this week in ${top} and neighbours.`,
          0.72,
          pickTitles(seed + "tr", 14, media),
        ),
        mk(
          "hiddengems",
          "Hidden Gems for You",
          "hidden-gems",
          "High ratings, few votes, strong taste match.",
          0.66,
          pickTitles(seed + "hg", 12, (x) => media(x) && (x.rating ?? 0) > 7.5 && x.votes < 6000),
        ),
        mk(
          "shortontime",
          "Short on Time",
          "short-on-time",
          short
            ? "Shorter titles: it's a weeknight or a phone."
            : "Shorter titles for a quick watch.",
          short ? 0.8 : 0.41,
          pickTitles(seed + "sh", 12, (x) => media(x) && (x.runtimeMinutes ?? 999) <= 95),
        ),
        mk(
          "newthisweek",
          "New This Week",
          "new-this-week",
          `Fresh releases in ${top} and other genres you like.`,
          0.58,
          pickTitles(seed + "new", 10, media),
        ),
        mk(
          "comfort",
          "Comfort Rewatches",
          "comfort-rewatches",
          "Favourites you've watched more than once.",
          weekend ? 0.61 : 0.47,
          pickTitles(seed + "cf", 8, media),
        ),
        mk(
          "different",
          "Something Different",
          "something-different",
          "Outside your usual genres to keep things fresh.",
          0.39,
          pickTitles(seed + "sd", 10, (x) => media(x) && !(x.genres ?? []).includes(top)),
        ),
        mk(
          "top10.today",
          "Top 10 on Living Room Ether Today",
          "top-10-today",
          "Trending now, catalogue popularity and what viewers here watch.",
          0.36,
          pickTitles(seed + "t10", 10, media),
        ),
      );
    }
    if (section === "home") {
      const cw = userId === null ? [] : pickTitles(seed + "cw", 6, media);
      rows.push({
        ...mk(
          "continue",
          "Continue Watching",
          "continue-watching",
          "Titles you're part-way through, most recent first.",
          cw.length ? 0.95 : 0,
          cw,
          "plex",
          () => "Started recently",
        ),
        hubId: "home.continue",
      });
    }
    // Owner collections: targeted ones, plus those the layout places on this page.
    const page = (l: "global" | number) => pageOf(layout, l, section)?.rows ?? [];
    const placed = new Set(
      [...page("global"), ...(userId === null ? [] : page(userId))]
        .filter((r) => r.status !== "off" && r.row.startsWith("collection:"))
        .map((r) => r.row.slice("collection:".length)),
    );
    for (const c of state.collections) {
      if (!c.enabled || c.builtIn) continue;
      const targeted =
        c.targets.homeRow &&
        (c.targets.users === "all" || (userId !== null && c.targets.users.includes(userId)));
      if (!targeted && !placed.has(String(c.id))) continue;
      // The collection's schedule (a draft's edit in a preview), unless this
      // viewer's own override sets a schedule; the layout pass applies that.
      const id = String(c.id);
      const own =
        userId === null ? undefined : page(userId).find((r) => collectionIdOf(r.row) === id);
      const s =
        layout.collectionSchedules && id in layout.collectionSchedules
          ? layout.collectionSchedules[id]
          : c.targets.schedule;
      if (!own?.when && !whenMatches(tidySchedule(s), { day, hour, device, date: at })) continue;
      const items = evaluate(c).items.slice(0, 12);
      if (!items.length) continue;
      rows.push({
        hubId: `collection.${c.id}`,
        title: c.name,
        kind: `collection:${c.id}`,
        score: 0.6,
        source: "owner",
        items,
        reason: targeted
          ? `Owner collection “${c.name}”, targeted at this viewer.`
          : `Owner collection “${c.name}”, placed on this page in the Home Designer.`,
      });
    }
    return rows;
  }

  function homeFor(
    userId: number | null,
    device: string,
    at: Date,
    section: string,
    layout: HomeLayout = state.layout,
  ) {
    const sec = (["home", "movies", "shows"].includes(section) ? section : "home") as LayoutSection;
    const day = at.getDay();
    const hour = at.getHours();
    const weekend = day === 0 || day === 6 || (day === 5 && hour >= 17);
    const daypart = daypartOf(hour);
    const viewer =
      userId === null
        ? null
        : { id: userId, title: VIEWERS.find((v) => v.id === userId)?.title ?? `Viewer ${userId}` };
    let rows = candidatesFor(userId, device, at, sec, layout);
    if (sec !== "home") {
      const prefix = sec === "movies" ? "movie" : "tv";
      const media = (x: FakeTitle) => (sec === "movies" ? x.type === "movie" : x.type === "show");
      rows = [
        {
          hubId: `${prefix}.inprogress.1`,
          title: "Continue Watching",
          kind: "continue-watching",
          reason: "Titles you're part-way through in this library (Plex's own row).",
          score: 0,
          source: "plex",
          items: (userId === null ? [] : pickTitles(`${userId}cw${sec}`, 5, media)).map(card),
          placement: "Plex puts its own rows at the top of a library.",
        },
        {
          hubId: `${prefix}.recentlyadded.1`,
          title: "Recently Added",
          kind: "recently-added",
          reason: "The newest additions to this library (Plex's own row).",
          score: 0,
          source: "plex",
          items: pickTitles(`ra${sec}`, 12, media).map(card),
          placement: "Plex puts its own rows at the top of a library.",
        },
        ...rows,
      ];
    }
    rows = assemble(rows, layout, {
      viewer,
      section: sec,
      device: device as Device,
      at,
      defaultRows: DEFAULT_ROWS[sec],
    });
    return {
      rows,
      generatedAt: new Date().toISOString(),
      context: { device, daypart, weekend },
      viewer,
    };
  }

  const ROW_META: Record<string, { group: string; sections?: string[] }> = {
    "continue-watching": { group: "picks", sections: ["home", "movies", "shows"] },
    "up-next": { group: "shows", sections: ["home", "shows"] },
    "top-10": { group: "picks", sections: ["home"] },
    "hidden-gems": { group: "explore", sections: ["home", "movies", "shows"] },
    "more-from-person": { group: "explore", sections: ["home", "movies", "shows"] },
    "top-10-today": { group: "household", sections: ["home"] },
    "short-on-time": { group: "picks", sections: ["home", "movies"] },
    "comfort-rewatches": { group: "picks", sections: ["home"] },
    "popular-on-server": { group: "household" },
    trending: { group: "household" },
  };
  const SECTION_ROWS = [
    {
      hubId: "fans-also-loved",
      title: "Fans of What You Watch Also Loved",
      kind: "fans-also-loved",
      description:
        "What people who love the viewer's favourites loved most, strongest when several favourites agree.",
      group: "picks",
      sections: ["home", "movies", "shows"],
    },
    {
      hubId: "trending-with-fans",
      title: "Trending With Fans of What You Watch",
      kind: "trending-with-fans",
      description: "What's trending right now among fans of the viewer's favourites.",
      group: "picks",
      sections: ["home", "movies", "shows"],
    },
    {
      hubId: "taste-cluster",
      title: "<Taste cluster>",
      kind: "taste-cluster",
      description:
        "Named rows for the viewer's strongest taste clusters, e.g. “Time-Travel Sci-Fi Films”.",
      group: "explore",
      sections: ["home", "movies", "shows"],
    },
    {
      hubId: "era-favourites",
      title: "Best of the '<decade>s for You",
      kind: "era-favourites",
      description: "Unseen films from the decades the viewer loves most.",
      group: "explore",
      sections: ["home", "movies"],
    },
    {
      hubId: "world-cinema",
      title: "<Language> Cinema for You",
      kind: "world-cinema",
      description: "Films in the languages the viewer watches beyond their main one.",
      group: "explore",
      sections: ["home", "movies"],
    },
    {
      hubId: "occasion",
      title: "Date Night · Family Night · Under 100 Minutes",
      kind: "occasion",
      description: "Occasion rows, each shown only when the viewer's history supports it.",
      group: "explore",
      sections: ["home", "movies"],
    },
    {
      hubId: "acclaimed-unseen",
      title: "Acclaimed & Unseen",
      kind: "acclaimed-unseen",
      description: "Highly rated films the viewer hasn't watched, ranked by fit.",
      group: "explore",
      sections: ["home", "movies"],
    },
    {
      hubId: "top-rated-in-genre",
      title: "Top-Rated <Genre>",
      kind: "top-rated-in-genre",
      description: "The best of the viewer's top genres.",
      group: "explore",
      sections: ["home", "movies", "shows"],
    },
    {
      hubId: "new-seasons",
      title: "New Seasons of Your Shows",
      kind: "new-seasons",
      description: "Fresh seasons of shows the viewer is caught up on.",
      group: "shows",
      sections: ["home", "shows"],
    },
    {
      hubId: "finish-what-you-started",
      title: "Finish What You Started",
      kind: "finish-what-you-started",
      description: "Shows started and stalled, ordered by how close they are to done.",
      group: "shows",
      sections: ["home", "shows"],
    },
    {
      hubId: "start-something-new",
      title: "Start Something New",
      kind: "start-something-new",
      description: "Unstarted shows that match the viewer's taste, from the pilot.",
      group: "shows",
      sections: ["home", "shows"],
    },
    {
      hubId: "complete-series",
      title: "Complete Series to Binge",
      kind: "complete-series",
      description: "Ended series with two or more seasons.",
      group: "shows",
      sections: ["home", "shows"],
    },
    {
      hubId: "limited-series",
      title: "Limited Series for You",
      kind: "limited-series",
      description: "One-season stories and miniseries.",
      group: "shows",
      sections: ["home", "shows"],
    },
    {
      hubId: "more-from-network",
      title: "More from <Network>",
      kind: "more-from-network",
      description: "Series from networks the viewer keeps coming back to.",
      group: "shows",
      sections: ["home", "shows"],
    },
  ];

  function availableRows() {
    return baseRows()
      .map((r) => ({ ...r, ...(ROW_META[r.kind] ?? { group: "picks" }) }))
      .concat(SECTION_ROWS);
  }

  function baseRows() {
    return [
      {
        hubId: "continue-watching",
        title: "Continue Watching",
        kind: "continue-watching",
        description:
          "Titles the viewer is part-way through (built by Plex; the engine only places it).",
      },
      {
        hubId: "top-10",
        title: "Top 10 for You",
        kind: "top-10",
        description: "The ten titles the taste model rates highest right now.",
      },
      {
        hubId: "top-10-today",
        title: "Top 10 Today",
        kind: "top-10-today",
        description: "The server's Top 10 today. Leads new viewers' Homes.",
      },
      {
        hubId: "genre-showcase",
        title: "Acclaimed <Genre>",
        kind: "genre-showcase",
        description: "Acclaimed, popular titles of one broad genre (new viewers).",
      },
      {
        hubId: "because-you-watched",
        title: "Because You Watched…",
        kind: "because-you-watched",
        description: "Titles similar to something the viewer recently finished.",
      },
      {
        hubId: "more-from-person",
        title: "More from…",
        kind: "more-from-person",
        description: "More from a director, writer or actor the viewer loves.",
      },
      {
        hubId: "trending-in-your-taste",
        title: "Trending in Your Taste",
        kind: "trending-in-your-taste",
        description: "This week's trending titles, ranked by taste.",
      },
      {
        hubId: "hidden-gems",
        title: "Hidden Gems for You",
        kind: "hidden-gems",
        description: "Highly rated, little-known titles that fit the viewer's taste.",
      },
      {
        hubId: "short-on-time",
        title: "Short on Time",
        kind: "short-on-time",
        description: "Shorter films and episodes, fitted to the time the viewer has.",
      },
      {
        hubId: "new-this-week",
        title: "New This Week",
        kind: "new-this-week",
        description: "Fresh releases in the viewer's favourite genres.",
      },
      {
        hubId: "comfort-rewatches",
        title: "Comfort Rewatches",
        kind: "comfort-rewatches",
        description: "Favourites the viewer has not seen in a while.",
      },
      {
        hubId: "something-different",
        title: "Something Different",
        kind: "something-different",
        description: "Exploration: acclaimed titles outside the usual genres.",
      },
      {
        hubId: "new-releases",
        title: "New & Noteworthy",
        kind: "new-releases",
        description: "The newest released titles in the catalogue.",
      },
      {
        hubId: "popular-on-server",
        title: "Popular on This Server",
        kind: "popular-on-server",
        description: "What other viewers here watched most.",
      },
      {
        hubId: "top-rated",
        title: "Top Rated",
        kind: "top-rated",
        description: "TMDB's top rated titles.",
      },
      {
        hubId: "trending",
        title: "Trending This Week",
        kind: "trending",
        description: "What is trending on TMDB this week.",
      },
      {
        hubId: "recently-added",
        title: "Recently Added",
        kind: "recently-added",
        description: "Built by Plex at the top of each library section.",
      },
      {
        hubId: "playlists",
        title: "Recent Playlists",
        kind: "playlists",
        description: "Built by Plex at the end of Home.",
      },
      ...state.collections
        .filter((c) => !c.builtIn)
        .map((c) => ({
          hubId: `collection:${c.id}`,
          title: c.name,
          kind: `collection:${c.id}`,
          description: "Owner collection (Collection Studio).",
        })),
    ];
  }

  function layoutDoc() {
    const collectionSchedules = Object.fromEntries(
      state.collections.flatMap((c) =>
        c.targets.schedule ? [[String(c.id), c.targets.schedule] as const] : [],
      ),
    );
    return {
      layout: Object.keys(collectionSchedules).length
        ? { ...state.layout, collectionSchedules }
        : state.layout,
      availableRows: availableRows(),
      defaults: { maxRows: DEFAULT_ROWS },
    };
  }

  /** The server's main validation rules, so the UI's error paths are exercised. */
  function validateLayout(doc: HomeLayout): { field: string; message: string }[] {
    const out: { field: string; message: string }[] = [];
    const pages = (field: string, ps: LayoutPage[]) =>
      ps.forEach((p, i) => {
        const slots = new Map<number, string>();
        p.rows.forEach((r, j) => {
          const f = `${field}[${i}].rows[${j}]`;
          if (!r.row) out.push({ field: `${f}.row`, message: "is required" });
          if (r.slot && slots.has(r.slot))
            out.push({
              field: `${f}.slot`,
              message: `slot ${r.slot} is already anchored to ${slots.get(r.slot)}`,
            });
          if (r.slot) slots.set(r.slot, r.row);
          if (r.minSlot && r.maxSlot && r.minSlot > r.maxSlot)
            out.push({
              field: `${f}.minSlot`,
              message: `never above slot ${r.minSlot} and never below slot ${r.maxSlot} cannot both hold`,
            });
          if (r.row === "continue-watching" && r.status === "off")
            out.push({
              field: `${f}.status`,
              message:
                "Continue Watching is Plex's own row: it can be anchored or bounded, not turned off",
            });
        });
      });
    pages("global", doc.global ?? []);
    (doc.global ?? []).forEach((p, i) =>
      p.rows.forEach((r, j) => {
        if (r.when && collectionIdOf(r.row))
          out.push({
            field: `layout.global[${i}].rows[${j}].when`,
            message: "a collection row uses its collection's schedule",
          });
      }),
    );
    (doc.viewers ?? []).forEach((v, i) => pages(`viewers[${i}].pages`, v.pages));
    return out;
  }

  function impact(draft: HomeLayout, device: string, at: Date, section: string) {
    const viewers = VIEWERS.map((v) => {
      const visible = (l: HomeLayout) =>
        homeFor(v.id, device, at, section, l).rows.filter(
          (r) =>
            (!r.hidden && r.source !== "plex") || (r.kind === "continue-watching" && !r.hidden),
        );
      const before = visible(state.layout);
      const after = visible(draft);
      const bi = new Map(before.map((r, i) => [r.hubId, i]));
      const ai = new Map(after.map((r, i) => [r.hubId, i]));
      const added = after.filter((r) => !bi.has(r.hubId)).map((r) => r.title);
      const removed = before.filter((r) => !ai.has(r.hubId)).map((r) => r.title);
      const moved = after.filter(
        (r) => bi.has(r.hubId) && bi.get(r.hubId) !== ai.get(r.hubId),
      ).length;
      const sec = (section || "home") as LayoutSection;
      return {
        viewerId: v.id,
        title: v.title,
        overrides: overrideCount(draft, v.id, sec),
        changed: added.length + removed.length + moved > 0,
        added,
        removed,
        moved,
      };
    });
    return {
      viewers,
      summary: {
        viewers: viewers.length,
        withOverrides: viewers.filter((v) => v.overrides > 0).length,
        changed: viewers.filter((v) => v.changed).length,
      },
    };
  }

  function activity(): ActivityDay[] {
    const out: ActivityDay[] = [];
    for (let i = 29; i >= 0; i--) {
      const d = new Date(Date.now() - i * 86400000);
      const dow = d.getDay();
      const base = dow === 0 || dow === 6 ? 9 : 5;
      const plays = base + (hash(d.toDateString()) % 6);
      out.push({
        date: d.toISOString().slice(0, 10),
        plays,
        hours: Math.round(plays * 1.4 * 10) / 10,
        uniqueViewers: 1 + (hash("u" + d.toDateString()) % 4),
      });
    }
    return out;
  }

  function debridStatus() {
    const debrid = state.config.debrid;
    const providers = debrid?.providers ?? [];
    return {
      enabled: providers.some((p) => p.enabled && p.apiKey),
      cachedOnly: debrid?.cachedOnly ?? true,
      engine: {
        openFiles: 1,
        blockBytes: 524288,
        memoryHits: 10,
        diskHits: 40,
        blocksFetched: 120,
        bytesFetched: 62914560,
        hedges: 2,
        hedgeWins: 1,
        linkRefreshes: 3,
        errors: 0,
      },
      providers: providers.map((p, i) => {
        const base = {
          id: p.id,
          name: p.name,
          kind: p.kind,
          enabled: p.enabled,
          priority: p.priority,
          accountSlots: 6,
          slots: state.clustered ? 2 : 6,
        };
        const idle = {
          resolveMs: 0,
          linkMs: 0,
          connectMs: 0,
          ttfbMs: 0,
          throughputBps: 0,
          samples: 0,
        };
        if (!p.enabled || !p.apiKey)
          return {
            ...base,
            state: "disabled",
            requests: 0,
            failures: 0,
            cached: 0,
            notCached: 0,
            reused: 0,
            latency: idle,
          };
        if (i === 0)
          return {
            ...base,
            state: "ok",
            lastError: "",
            requests: 48,
            failures: 1,
            cached: 17,
            notCached: 6,
            linksRefreshed: 3,
            reused: 5,
            latency: {
              resolveMs: 812.5,
              linkMs: 140.2,
              connectMs: 35.1,
              ttfbMs: 92.4,
              throughputBps: 48_000_000,
              samples: 12,
            },
            lastSuccess: new Date(Date.now() - 4 * 60_000).toISOString(),
          };
        return {
          ...base,
          state: "backoff",
          lastError: "rate limited by the service; retrying shortly",
          backoffUntil: new Date(Date.now() + 5 * 60_000).toISOString(),
          requests: 9,
          failures: 3,
          cached: 2,
          notCached: 4,
          linksRefreshed: 0,
          reused: 0,
          latency: idle,
          lastSuccess: new Date(Date.now() - 3 * 3600_000).toISOString(),
        };
      }),
    };
  }

  function dashboard() {
    const session = catalog[3]!;
    return {
      names: [
        "cache",
        "catalog",
        "database",
        "debrid",
        "hwaccel",
        "indexers",
        "providers",
        "remote",
        "sessions",
      ],
      setupComplete: state.config.setupComplete,
      version: state.boot.version,
      sections: {
        cache: { used: 87 * 1024 ** 3, budget: state.config.cache.budgetBytes },
        database: state.clustered
          ? { clustered: true, state: "ok", reachable: true, held: 0 }
          : {
              clustered: false,
              state: "ok",
              reachable: true,
              integrity: opts.damagedDatabase
                ? {
                    ok: false,
                    findings: [
                      "*** in database main ***",
                      "Page 4127: btreeInitPage() returns error code 11",
                    ],
                    checked: new Date(Date.now() - 20 * 60_000).toISOString(),
                    tookSeconds: 1.2,
                  }
                : {
                    ok: true,
                    checked: new Date(Date.now() - 3 * 3600_000).toISOString(),
                    tookSeconds: 0.9,
                  },
            },
        catalog: {
          movies: 24929,
          shows: 24298,
          browsableMovies: 24928,
          homeReleaseChecked: 24680,
          homeReleasePending: 248,
          browsableShows: 24298,
          lastSync: new Date(Date.now() - 5 * 3600_000).toISOString(),
          sync: { running: false, titlesSeen: 63500, titlesAdded: 284 },
          trending: ["movie", "show"].flatMap((mediaType) =>
            ["day", "week"].map((window) => ({
              mediaType,
              window,
              source: "TMDB",
              scope: "Global",
              titles: 200,
              state: "fresh",
              fetchedAt: new Date(Date.now() - 1800000).toISOString(),
            })),
          ),
        },
        debrid: debridStatus(),
        sources: {
          startup: [
            { protocol: "usenet", average: 2_140_000_000, last: 1_980_000_000, samples: 37 },
            { protocol: "torrent", average: 410_000_000, last: 380_000_000, samples: 22 },
          ],
        },
        hwaccel: {
          ffmpeg: "8.0.1",
          filtersProbed: true,
          toneMap: ["videotoolbox"],
          colour: ["zscale", "colorspace", "scale"],
          hwaccel: { name: "videotoolbox", encoders: ["h264_videotoolbox", "hevc_videotoolbox"] },
          enhancement: {
            backend: "nvenc-ngx",
            available: false,
            reason: "An NVIDIA RTX GPU and NVEnc are required on this server.",
            active: 0,
          },
        },
        indexers: (state.config.indexers ?? []).map((ix, i) => ({
          indexer: ix.name,
          day: new Date().toISOString().slice(0, 10),
          hitsUsed: 6 + i * 15,
          hitsLimit: ix.dailyLimit,
          hitsRemaining: Math.max(0, ix.dailyLimit - 6 - i * 15),
          grabsUsed: 3,
          grabsLimit: ix.dailyGrabs,
          grabsRemaining: ix.dailyGrabs - 3,
          enabled: ix.enabled,
          torrent: ix.protocol === "torrent" || ix.kind === "stremio",
        })),
        providers: (state.config.usenet.providers ?? []).map((p, i) => ({
          id: p.id,
          name: p.name,
          host: p.host,
          backfill: p.backfill,
          state: "ok",
          configuredConnections: p.connections,
          activeConnections: i === 0 ? 6 : 0,
          ...(state.clustered ? { nodeShare: Math.ceil(p.connections / 3) } : {}),
        })),
        remote: {
          claimed: true,
          ownerUsername: "Braedon",
          publicAddress: "203.0.113.7",
          remoteAccess: state.config.plex.remoteAccess,
          listed: true,
          presence: { connected: true },
          upnp: { active: true },
          cert: {
            present: true,
            verified: true,
            subject: "*.abc.plex.direct",
            notBefore: new Date(Date.now() - 30 * 86400000).toISOString(),
            notAfter: new Date(Date.now() + 80 * 86400000).toISOString(),
          },
        },
        sessions: [
          {
            ID: "s1",
            UserID: 2,
            Key: session.key,
            TitleName: `${session.title} (${session.year})`,
            Username: "Sam Rivera",
            Client: { product: "Plex for Apple TV", deviceName: "Living Room", remote: false },
            Decision: "directplay",
            decisionVideoCopy: true,
            decisionAudioCopy: true,
            BytesServed: 2.1 * 1024 ** 3,
            BufferedBytes: 183 * 1024 ** 2,
            BufferedUntil: 3000e9,
            SpeedBps: 3.2e6,
            StartedAt: new Date(Date.now() - 40 * 60000).toISOString(),
            Info: { size: 5.4 * 1024 ** 3, duration: 118 * 60 * 1e9 },
            viewOffset: 46 * 60 * 1000,
            origin: "Real-Debrid",
            Resolution: {
              release: {
                name: `${session.title.replace(/ /g, ".")}.${session.year}.1080p.WEB-DL.H264`,
              },
            },
          },
        ],
      },
    };
  }

  let updateRequested = false;
  let updateForced = false;
  function updateStatus() {
    return {
      enabled: true,
      current: "0.2.0",
      available: {
        version: "0.3.0",
        kind: "binary",
        notesUrl: "https://example.invalid/notes",
        staged: true,
      },
      capability: "self-apply",
      state: updateForced
        ? "applying"
        : updateRequested
          ? "waiting-for-idle"
          : "waiting-for-window",
      autoUpdate: state.config.updates.autoUpdate,
      requested: updateRequested,
      force: updateForced,
      lastCheck: new Date(Date.now() - 3600_000).toISOString(),
      window: {
        start: state.config.updates.maintenanceStart ?? "03:00",
        end: state.config.updates.maintenanceEnd ?? "06:00",
        open: false,
      },
      activeSessions: 1,
      quietPeriodSeconds: 600,
      confirming: false,
    };
  }

  // ---- Cluster -----------------------------------------------------------------------

  let clusterSetupState = "idle";
  let nodePaused = false;
  const nodeControls = new Map<
    string,
    { control: ClusterControl; patch?: ClusterControlRequest["patch"]; due?: number }
  >();
  function clusterNodes(): ClusterNode[] {
    const ago = (s: number) => new Date(Date.now() - s * 1000).toISOString();
    const node = (n: Partial<ClusterNode> & { id: string; name: string }): ClusterNode => ({
      incarnation: `boot-${n.id}`,
      version: state.boot.version,
      addrs: [`http://${n.id}.test:32400`],
      caps: { transcode: true, hwAccel: "videotoolbox", egressIp: "203.0.113.7" },
      load: 0,
      startedAt: ago(86400),
      lastSeen: ago(3),
      leader: false,
      live: true,
      ...n,
    });
    if (!state.clustered)
      return [node({ id: "n-solo", name: state.boot.serverName, leader: true, load: 1 })];
    return [
      node({
        id: "n-den",
        name: "den",
        leader: true,
        load: 1,
        draining: nodePaused,
        caps: {
          transcode: true,
          hwAccel: "videotoolbox",
          weight: state.config.server.clusterWeight || 1,
          egressIp: "203.0.113.7",
        },
      }),
      node({
        id: "n-attic",
        name: "attic",
        draining: true,
        load: 2,
        caps: {
          roles: ["transcode", "delivery"],
          transcode: true,
          hwAccel: "nvenc",
          weight: 2,
          egressIp: "198.51.100.20",
        },
      }),
      node({
        id: "n-source",
        name: "source-02",
        load: 3,
        caps: { roles: ["source"], transcode: false, weight: 2 },
      }),
      node({
        id: "n-edge",
        name: "edge-01",
        load: 4,
        caps: { roles: ["delivery"], transcode: false, weight: 1 },
      }),
      node({
        id: "n-garage",
        name: "garage",
        version: "0.8.4",
        live: false,
        lastSeen: ago(3 * 3600),
        caps: { transcode: true, egressIp: "198.51.100.20" },
      }),
    ];
  }

  function clusterStatus(): ClusterStatus {
    const nodes = clusterNodes();
    const self = nodes[0]!;
    // Mirrors internal/app egressWarning: live nodes reaching out from several IPs.
    const byIP = new Map<string, string[]>();
    for (const n of nodes)
      if (n.live && n.caps.egressIp)
        byIP.set(n.caps.egressIp, [...(byIP.get(n.caps.egressIp) ?? []), n.name]);
    const warnings =
      byIP.size > 1
        ? [
            `nodes reach providers from different public addresses (${[...byIP]
              .sort()
              .map(([ip, names]) => `${ip}: ${names.sort().join(", ")}`)
              .join(
                "; ",
              )}); a debrid account can be limited when it is used from more than one address`,
          ]
        : undefined;
    return {
      mockConsole: {
        // Explicit synthetic diagnostic evidence, independent of heartbeat liveness.
        health: Object.fromEntries(
          nodes.map((n) => [
            n.id,
            {
              nodeId: n.id,
              boot: n.incarnation!,
              sampledAt: new Date().toISOString(),
              level: n.id === "n-garage" ? ("error" as const) : ("ok" as const),
            },
          ]),
        ),
        metrics: Object.fromEntries(
          nodes.map((n, i) => [
            n.id,
            { transcodes: i === 1 ? 2 : 0, capacity: i === 1 ? 8 : 4, load: i === 1 ? 63 : 18 },
          ]),
        ),
        events: [
          { node: "attic", kind: "drain", at: new Date(Date.now() - 60000).toISOString() },
          { node: "den", kind: "failover", at: new Date(Date.now() - 180000).toISOString() },
          { node: "source-02", kind: "join", at: new Date(Date.now() - 600000).toISOString() },
        ],
        transition: { node: "n-attic", role: "transcode", phase: "drain" },
      },
      selfRuntime: {
        nodeId: self.id,
        boot: self.incarnation!,
        runtimeSampledAt: new Date().toISOString(),
        protocol: 1,
        started: true,
        readyExisting: true,
        acceptingNew: !nodePaused,
        draining: nodePaused,
      },
      warnings,
      mode: state.clustered ? "postgres" : "solo",
      self,
      leader: self.id,
      epoch: state.clustered ? 4 : 1,
      nodes,
      ...(state.clustered
        ? {
            health: {
              channel: "ether_cluster",
              busConnected: true,
              lastHeartbeat: new Date(Date.now() - 2000).toISOString(),
              clockSkewNs: 12_000_000,
            },
          }
        : {}),
    };
  }

  const metadata = createMetadataMock();
  const plexWatch = new Map<
    number,
    {
      connected: boolean;
      direction: number;
      importInterval: number;
      lastImport: number;
      lastFullImport: number;
      lastSend: number;
      nextImport: number;
      problem: string;
      needsReconnect: boolean;
      pending: number;
      failed: number;
      restricted: boolean;
      pinCode?: string;
      pinUrl?: string;
      accountMatch: boolean;
      username?: string;
      plexId?: number;
    }
  >();

  const ops = createOpsMock({
    startup: opts.startup,
    pace: opts.opsPace,
    viewers: opts.viewers,
    restore: opts.restore,
    clustered: () => state.clustered,
    backups: () => state.backups,
  });

  const viewerStreamRules = new Map<string, unknown>();
  const restoredHidden = new Set<string>();

  // ---- Router ------------------------------------------------------------------------

  const viewerCatalogRules = new Map<number, unknown>();
  async function handle(method: string, url: URL, body: unknown): Promise<Response> {
    const p = url.pathname.replace(/^\/ether\/api/, "");
    if (method === "GET" && p.startsWith("/viewer-plex-defaults/")) {
      const id = Number(p.split("/").pop());
      return json({
        hasProfile: true,
        hasSettings: false,
        audioLanguage: "en",
        autoSelectAudio: true,
        subtitleLanguage: "en",
        subtitleMode: "off",
        filterMovies: "",
        filterTelevision: "",
        allowSync: null,
        ...(state.plexDefaults[id] ?? {}),
      });
    }
    if (
      state.streamDefaults &&
      p === "/stream-preferences/0" &&
      (method === "GET" || method === "PUT")
    ) {
      if (method === "PUT" && body && typeof body === "object") {
        state.streamDefaults = {
          ...state.streamDefaults,
          ...(body as NonNullable<typeof state.streamDefaults>),
        };
      }
      return json(state.streamDefaults);
    }
    const q = url.searchParams;
    const seg = p.split("/").filter(Boolean);
    const route = `${method} /${seg.join("/")}`;
    if (seg[0] === "cluster" && seg[1] === "nodes" && seg[3] === "role-preflight") {
      const node = clusterNodes().find((n) => n.id === seg[2]);
      if (!node || q.get("boot") !== node.incarnation)
        return err(409, "stale_boot", "Refresh the node");
      return json({
        nodeId: node.id,
        boot: node.incarnation,
        observedAt: new Date().toISOString(),
        membershipSampledAt: node.lastSeen,
        eligible: false,
        executor: "none",
        reasons: [
          "executor_unavailable",
          "runtime_guard_not_verified",
          "ownership_retirement_unknown",
          "active_sessions_unknown",
          ...(node.leader ? ["last_full_server", "leader_handoff_unverified"] : []),
        ],
        safety: {
          membership: "observed",
          ownershipRetirement: "unknown",
          activeSessions: "unknown",
          restart: "unsupported",
        },
        guidance: {
          scope: "deployment_pool",
          deploymentOwnership: "unknown",
          verifiedAgainst: ["packaging/helm/ether/values.yaml"],
          exampleValues: "pools:\n  full:\n    replicas: 2",
          impact: "",
          execution: "",
        },
      });
    }
    if (seg[0] === "cluster" && seg[1] === "nodes" && seg[3] === "control") {
      const node = clusterNodes().find((n) => n.id === seg[2]);
      if (!node) return err(404, "unavailable", "Unknown node");
      let saved = nodeControls.get(node.id);
      if (!saved) {
        saved = {
          control: {
            nodeId: node.id,
            boot: node.incarnation!,
            requestedVersion: 0,
            appliedVersion: 0,
            state: "idle",
            updatedAt: new Date().toISOString(),
            observed: { weight: node.caps.weight || 1, draining: !!node.draining },
            capabilities: [
              { key: "weight", scope: "node", supported: true, reason: "" },
              ...["draining", "pause", "roles"].map((key) => ({
                key,
                scope: "node" as const,
                supported: false,
                reason: "ownership_interlock_required",
              })),
              ...["maxStreams", "defaultStreamLimit"].map((key) => ({
                key,
                scope: "fleet" as const,
                supported: false,
                reason: "fleet_setting",
              })),
              ...["maxTranscodes", "hwAccel"].map((key) => ({
                key,
                scope: "node" as const,
                supported: false,
                reason: "restart_required",
              })),
            ],
          },
        };
        nodeControls.set(node.id, saved);
      }
      if (saved.due && Date.now() >= saved.due && node.live) {
        Object.assign(saved.control.observed, saved.patch);
        saved.control.appliedVersion = saved.control.requestedVersion;
        saved.control.state = "applied";
        saved.due = undefined;
      }
      if (node.live) saved.control.updatedAt = new Date().toISOString();
      else saved.control.updatedAt = node.lastSeen;
      if (method === "GET") {
        if (q.get("boot") !== node.incarnation) return err(409, "stale_boot", "Stale boot");
        return json(saved.control);
      }
      if (method === "POST") {
        const request = body as ClusterControlRequest;
        if (
          request.boot !== node.incarnation ||
          request.expectedVersion !== saved.control.requestedVersion ||
          saved.control.state === "pending"
        )
          return err(409, "conflict", "Refresh the node");
        if (request.patch.draining !== undefined) return err(422, "unsupported", "No safe setter");
        saved.control.requestedVersion++;
        saved.control.state = "pending";
        saved.control.requestId = request.requestId;
        saved.patch = request.patch;
        saved.due = Date.now() + 5000;
        return json(saved.control, 202);
      }
    }
    if (seg[0] === "stream-preferences") {
      if (seg[2] === "shows") return json([]);
      const id = seg[1] ?? "0";
      if (method === "PUT") viewerStreamRules.set(id, body);
      if (method === "DELETE") viewerStreamRules.delete(id);
      return json(
        viewerStreamRules.get(id) ??
          state.streamDefaults ?? {
            audioLanguages: ["en"],
            subtitleLanguages: ["en"],
            preferOriginal: true,
            subtitleMode: "foreign-only",
            subtitleStyle: "normal",
            loudness: false,
            nightMode: false,
            dialogueBoost: false,
          },
      );
    }
    if (seg[0] === "plex-servers") {
      // Demo: one owner-run Plex Media Server with the owner mapped.
      const servers = (state as { plexServers?: unknown[] }).plexServers ?? [
        {
          id: 1,
          name: "Living room PMS",
          url: "http://192.168.1.10:32400",
          machineId: "demo-pms",
          version: "1.43.4.10903",
          owner: "owner",
          checkedAt: Math.floor(Date.now() / 1000),
          problem: "",
          viewers: [
            {
              accountId: 1,
              accountName: "owner",
              owner: true,
              userId: 1,
              direction: 3,
              interval: 10,
              canSend: true,
              lastImport: Math.floor(Date.now() / 1000) - 240,
              lastSend: Math.floor(Date.now() / 1000) - 240,
              nextSync: Math.floor(Date.now() / 1000) + 360,
              problem: "",
              unmatched: 1,
            },
          ],
        },
      ];
      (state as { plexServers?: unknown[] }).plexServers = servers;
      if (seg[2] === "accounts" && method === "GET")
        return json([
          { id: 1, name: "owner", owner: true, userId: 1 },
          { id: 2, name: "family", owner: false, userId: 0 },
        ]);
      if (seg[2] === "unmatched")
        return json([
          { direction: "in", ref: "9001", title: "Home Video", reason: "no_match", seenAt: 0 },
        ]);
      if (seg[1] && method === "DELETE") {
        (state as { plexServers?: unknown[] }).plexServers = [];
        return new Response(null, { status: 204 });
      }
      if (seg[1]) return json(servers[0]);
      return json(servers);
    }
    if (p === "/discovery/settings") return json({ enabled: true });
    if (seg[0] === "discovery" && seg[1] === "users") {
      const viewer = Number(seg[2]);
      if (seg[3] === "membership" && method === "POST") {
        const keys = (body as { keys?: string[] }).keys || [];
        return json(Object.fromEntries(keys.map((key) => [key, savedForViewer(viewer, key)])));
      }
      if (seg[3] === "catalog" && seg[4] && (method === "PUT" || method === "DELETE")) {
        watchlistChanges.set(`${viewer}:${decodeURIComponent(seg[4])}`, method === "PUT");
        return json({ traktFailed: false });
      }
      return json([]);
    }
    if (seg[0] === "viewers" && seg[2] === "favorite-episode-alert") return json({ enabled: true });

    if (p === "/users/local" && method === "POST") {
      const b = body as { username?: string; password?: string };
      if (!b.username || (b.password ?? "").length < 12)
        return err(
          400,
          "invalid",
          "a username and a password of at least 12 characters are required",
        );
      const id = 200 + state.addedViewers.length;
      const viewer: Viewer = {
        id,
        plexId: 0,
        username: b.username,
        title: b.username,
        isOwner: false,
        accessStatus: "active",
        plays30d: 0,
        hoursWatched30d: 0,
        lifecycle: {
          source: "local",
          paused: false,
          managedBy: "",
          canPause: true,
          canRemove: true,
          canMerge: true,
        },
      };
      state.addedViewers.push(viewer);
      state.users.push({
        id,
        plexId: 0,
        username: viewer.username,
        title: viewer.title,
        thumb: avatarFor(viewer.title),
        isOwner: false,
        lifecycle: viewer.lifecycle,
        streamLimit: 0,
        maxContentRating: "",
        allowedSections: [],
      });
      return json({ id }, 201);
    }

    if (p === "/users/invite" && method === "POST") {
      const b = body as { display?: string };
      const id = 200 + state.addedViewers.length;
      const viewer: Viewer = {
        id,
        plexId: 0,
        username: "",
        title: b.display ?? "",
        isOwner: false,
        accessStatus: "active",
        plays30d: 0,
        hoursWatched30d: 0,
        lifecycle: {
          source: "invite",
          paused: false,
          managedBy: "",
          canPause: true,
          canRemove: true,
          canMerge: true,
        },
      };
      state.addedViewers.push(viewer);
      state.users.push({
        id,
        plexId: 0,
        username: viewer.username,
        title: viewer.title,
        thumb: avatarFor(viewer.title),
        isOwner: false,
        lifecycle: viewer.lifecycle,
        streamLimit: 0,
        maxContentRating: "",
        allowedSections: [],
      });
      return json({ id, url: "/ether/me#invite=mock-secret" }, 201);
    }

    if (p === "/frontends/dlna/guest" && method === "POST") {
      const viewer: Viewer = {
        id: 100 + state.dlnaGuests.length,
        plexId: 0,
        username: "",
        title: "Living-room guest",
        isOwner: false,
        accessStatus: "active",
        plays30d: 0,
        hoursWatched30d: 0,
        lifecycle: {
          source: "dlnaGuest",
          paused: false,
          managedBy: "",
          canPause: true,
          canRemove: true,
          canMerge: true,
        },
      };
      state.dlnaGuests.push(viewer);
      return json(viewer, 201);
    }

    if (p === "/me/favorites-mirror") {
      if (method === "POST") state.favoritesMirror.set(1, (body as { enabled: boolean }).enabled);
      return json({ enabled: state.favoritesMirror.get(1) ?? true });
    }

    const appsOffset = seg[0] === "me" ? 1 : seg[0] === "viewers" && seg[1] ? 2 : -1;
    if (appsOffset >= 0 && seg[appsOffset] === "apps") {
      const enabled = [
        "plex",
        "jellyfin",
        "emby",
        "silo",
        "stremio",
        "webdav",
        "m3u",
        "xtream",
      ].filter((id) => state.config.frontends?.[id]?.enabled);
      if (method === "GET" && seg.length === appsOffset + 1) return json({ apps: enabled });
      const surface = seg[appsOffset + 1];
      if (method === "POST" && surface && enabled.includes(surface)) {
        const qr =
          "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=";
        const id = seg[0] === "viewers" ? seg[1] : "1";
        if (surface === "xtream") {
          const grants = xtreamGrants.get(id ?? "1") ?? [];
          grants.push({
            hash: `mock-xtream-${grants.length + 1}`,
            surface: "xtream",
            lastUsed: Date.now(),
          });
          xtreamGrants.set(id ?? "1", grants);
          return json({
            surface,
            url: "http://ether.local/xtream",
            username: "xt_demo",
            password: "eg1_private-mock-login",
            qr: "",
          });
        }
        if (surface === "webdav")
          return json({
            surface,
            url: "http://ether.local/webdav/",
            username: id,
            password: "mock-app-password",
            qr,
          });
        if (surface === "jellyfin" || surface === "emby" || surface === "silo") {
          const viewer = state.users.find((u) => String(u.id) === id) ?? state.users[0];
          return json({
            surface,
            url: `http://ether.local/${surface}`,
            username: viewer?.username || id,
            password: "mock-app-password",
            qr,
          });
        }
        const path =
          surface === "m3u" ? "m3u/mock-grant/playlist.m3u" : "stremio/mock-grant/manifest.json";
        return json({ surface, url: `http://ether.local/${path}`, qr });
      }
      return err(404, "not_found", "app unavailable");
    }

    const cached = cacheMock(method, seg, body, state.config, state.clustered);
    if (cached) return json(cached.body, cached.status);

    const site = sites.handle(method, seg, body, state.config);
    if (site) return json(site.body, site.status);

    const ext = await extendMock(method, seg, q, body);
    if (ext) return ext;

    const cal = calendar.handle(method, seg, q);
    if (cal) return json(cal.body, cal.status);

    const oar = onAir.handle(method, seg, q, body);
    if (oar) return json(oar.body, oar.status);
    const tvr = liveTV.handle(method, seg, q, body);
    if (tvr) return json(tvr.body, tvr.status);

    const tr = trash.handle(method, seg, body, state.config);
    if (tr) return json(tr.body, tr.status);

    const qm = qualityMock.handle(method, seg, body, state.config, (id) => safetyPolicies.get(id));
    if (qm) return json(qm.body, qm.status);

    const rc = releaseConsole.handle(method, seg, q, body);
    if (rc)
      return rc.status === 204 ? new Response(null, { status: 204 }) : json(rc.body, rc.status);

    const md = metadata(method, p, body, q);
    if (md) return md;

    const mr = maintenance.handle(method, seg, body);
    if (mr) return json(mr.body, mr.status);

    const ex = experience.handle(method, seg, q);
    if (ex) return json(ex.body, ex.status);

    const mockConnectUrls = (id: string, port?: number) => {
      if (port)
        return [
          `http://192.168.1.20:${port}`,
          id === "plex"
            ? `https://203-0-113-7.abc.plex.direct:${port}`
            : `https://${id}.example.com`,
        ];
      if (id === "stremio" || id === "webdav" || id === "dlna" || id === "m3u" || id === "xtream") {
        return [`http://192.168.1.20:${state.config.server.port}/${id}`];
      }
      return [];
    };
    // Settings schemas mirror internal/frontend (WS1d): keep beside the
    // descriptors so the Apps page renders from the same shape.
    const rowKindOptions = [
      "because-you-watched",
      "trending",
      "top-10",
      "fans-also-loved",
      "new-releases",
      "hidden-gems",
      "more-from-person",
      "popular-on-server",
    ];
    const mediaBrowserSchema = (quickConnect: boolean) => ({
      signInHelp: true,
      sections: [
        {
          key: "home",
          fields: [
            { key: "homeRows", kind: "bool" },
            { key: "homeRowCount", kind: "number", more: true, min: 0, max: 12 },
            { key: "homeRowKinds", kind: "rowKinds", more: true, options: rowKindOptions },
          ],
        },
        {
          key: "find",
          fields: [
            { key: "serverName", kind: "serverName" },
            { key: "port", kind: "port", min: 1, max: 65535 },
            { key: "discovery", kind: "bool", more: true },
            ...(quickConnect ? [{ key: "quickConnect", kind: "bool", more: true }] : []),
            { key: "publicViewerPicker", kind: "bool", more: true },
          ],
        },
      ],
    });
    const accountSchema = (note: string) => ({
      accountConnect: true,
      sections: [{ key: "connect", fields: [{ key: note, kind: "note" }] }],
    });
    const protocolDefs = [
      {
        id: "plex",
        name: "Plex",
        defaultPort: 32400,
        settingsSchema: {
          sections: [{ key: "find", fields: [{ key: "port", kind: "port", min: 1, max: 65535 }] }],
        },
      },
      {
        id: "jellyfin",
        name: "Jellyfin",
        defaultPort: 8096,
        settingsSchema: mediaBrowserSchema(true),
      },
      { id: "emby", name: "Emby", defaultPort: 8097, settingsSchema: mediaBrowserSchema(false) },
      { id: "stremio", name: "Stremio", settingsSchema: accountSchema("addon") },
      {
        id: "silo",
        name: "Silo",
        defaultPort: 8090,
        settingsSchema: {
          signInHelp: true,
          sections: [
            {
              key: "find",
              fields: [
                { key: "serverName", kind: "serverName" },
                { key: "port", kind: "port", min: 1, max: 65535 },
                { key: "discovery", kind: "bool", more: true },
              ],
            },
          ],
        },
      },
      {
        id: "dlna",
        name: "DLNA",
        settingsSchema: {
          sections: [
            {
              key: "tv",
              fields: [
                { key: "serverName", kind: "serverName" },
                { key: "viewerId", kind: "viewer" },
                { key: "discovery", kind: "bool", more: true },
              ],
            },
          ],
        },
      },
      { id: "webdav", name: "Files (WebDAV)", settingsSchema: accountSchema("files") },
      { id: "m3u", name: "M3U & XMLTV", settingsSchema: accountSchema("playlist") },
      { id: "xtream", name: "IPTV apps (Xtream)", settingsSchema: accountSchema("xtream") },
    ];
    if (seg[0] === "frontends" && seg.length === 1 && method === "GET") {
      return json(
        protocolDefs.map((descriptor) => {
          const entry = state.config.frontends?.[descriptor.id];
          const enabled =
            entry?.enabled ?? (state.config.frontends == null && descriptor.id === "plex");
          const port = state.config.frontendPorts?.[descriptor.id] ?? descriptor.defaultPort;
          return {
            descriptor,
            enabled,
            status: {
              addresses: enabled && port ? [`127.0.0.1:${port}`] : [],
              connectUrls: enabled ? mockConnectUrls(descriptor.id, port) : [],
              lan: "unknown",
              wan: "unknown",
              clients24h: 0,
              clients24hKnown: true,
              liveStreams: 0,
              liveStreamsKnown: true,
              targetVersion: "",
              conformance: "unknown",
            },
          };
        }),
      );
    }
    if (seg[0] === "frontends" && seg.length === 3) {
      const [, id, action] = seg;
      const descriptor = protocolDefs.find((item) => item.id === id);
      if (!descriptor) return err(404, "not_found", "Front end not found");
      if (action === "status" && method === "GET") {
        const enabled =
          state.config.frontends?.[id!]?.enabled ??
          (state.config.frontends == null && id === "plex");
        const port = state.config.frontendPorts?.[id!] ?? descriptor.defaultPort;
        return json({
          addresses: enabled && port ? [`127.0.0.1:${port}`] : [],
          connectUrls: enabled ? mockConnectUrls(id ?? "", port) : [],
          lan: "unknown",
          wan: "unknown",
          clients24h: 0,
          clients24hKnown: true,
          liveStreams: 0,
          liveStreamsKnown: true,
          targetVersion: "",
          conformance: "unknown",
        });
      }
      if (action === "settings" && method === "GET")
        return json({
          ...state.config.frontends?.[id!]?.settings,
          ...((state.config.frontendPorts?.[id!] ?? descriptor.defaultPort)
            ? { port: state.config.frontendPorts?.[id!] ?? descriptor.defaultPort }
            : {}),
        });
      const entries =
        state.config.frontends ?? (state.config.frontends = { plex: { enabled: true } });
      const entry = entries[id!] ?? (entries[id!] = { enabled: false });
      if (action === "settings" && method === "PUT") {
        const { port, ...settings } = body as Record<string, unknown>;
        entry.settings = settings;
        if (typeof port === "number") (state.config.frontendPorts ??= {})[id!] = port;
        return json({ ok: true });
      }
      if (
        action === "enabled" &&
        method === "PUT" &&
        typeof (body as { enabled?: unknown }).enabled === "boolean"
      ) {
        entry.enabled = (body as { enabled: boolean }).enabled;
        return json({ ok: true });
      }
    }

    // Static routes
    switch (route) {
      case "GET /availability/titles":
        return json([
          {
            key: "1",
            title: "Ready Movie",
            year: 2026,
            theatrical: 1_760_000_000,
            digital: 1_762_000_000,
            code: "playable",
            source: "Indexer One",
          },
          {
            key: "2",
            title: "Waiting Movie",
            year: 2026,
            theatrical: 1_761_000_000,
            code: "searching",
            nextCheck: Math.floor(Date.now() / 1000) + 7200,
          },
          {
            key: "3",
            title: "Theatre Movie",
            year: 2026,
            theatrical: 1_762_500_000,
            code: "theatrical",
          },
          {
            key: "4",
            title: "Quality Movie",
            year: 2026,
            theatrical: 1_760_500_000,
            code: "quality",
            reason: "cam",
          },
        ]);
      case "GET /availability/title/1":
        return json({ code: "playable" });
      case "GET /availability/summary":
        return json({
          ready: 12,
          waiting: 34,
          lastChecked: Math.floor(Date.now() / 1000) - 300,
        });
      case "GET /availability":
        return json({
          mode: state.config.availability?.verify
            ? "checking"
            : state.config.availability?.monitor
              ? "monitoring"
              : "off",
          message: "Search results remain independent of source evidence.",
          feeds: (state.config.indexers ?? [])
            .filter((x) => x.enabled)
            .map((x) => ({
              id: x.id,
              name: x.name,
              protocol: x.protocol,
              supported: true,
              selected: true,
              lastSync: 0,
              gap: false,
              message: "Ready to monitor",
              matched: 0,
            })),
          usage: { feedRequests: 12 },
          counts: {},
          recent: [],
          day: new Date().toISOString().slice(0, 10),
          effort: state.config.availability?.effort || "normal",
          effectiveFeedRequests: state.config.availability?.feedRequests ?? 100,
          effectivePollMinutes: state.config.availability?.pollMinutes ?? 30,
          effectiveMaxPages: state.config.availability?.maxPages ?? 3,
        });
      case "POST /availability/sync":
        return state.config.availability?.monitor
          ? json(null, 202)
          : err(409, "disabled", "Enable release monitoring first");
      case "GET /bootstrap":
      case "GET /health":
        return json(state.boot);
      case "GET /dashboard":
        return json(dashboard());
      case "GET /indexer-usage":
        return json(indexerUsage());
      case "GET /stats/sources":
        return json(sourceHistory(Math.min(90, Math.max(1, Number(q.get("days")) || 30))));
      case "GET /stats/infrastructure":
        return json(infrastructure());
      case "PUT /stats/costs":
        saveCost(body as { kind: string; id: string; monthly: number; currency: string });
        return json({ ok: true });
      case "GET /config":
        return json({ config: state.config });
      case "PUT /config":
        state.config = body as EtherConfig;
        return json({ config: state.config });
      case "GET /setup/status":
        return json({
          setupComplete: state.config.setupComplete,
          requiredComplete: true,
          steps: [],
        });
      case "POST /setup/complete":
        state.config.setupComplete = true;
        state.boot.setupComplete = true;
        return json({ setupComplete: true, requiredComplete: true, steps: [] });
      case "POST /setup/reopen":
        return json({
          setupComplete: state.config.setupComplete,
          requiredComplete: true,
          steps: [],
        });
      case "POST /plex/pin":
        return json({ id: "pin1", code: "WXYZ", authUrl: "https://app.plex.tv/auth#?code=WXYZ" });
      case "POST /plex/claim":
        state.config.plex.claimed = true;
        if (!state.config.plex.token) state.config.plex.token = "••••";
        if (!state.config.plex.ownerUsername) state.config.plex.ownerUsername = "Braedon";
        return json({ ok: true });
      case "POST /plex/friends/sync":
        return json({ ok: true });
      case "POST /auth/pin":
        return json({ handle: "h1", code: "ABCD", authUrl: "https://app.plex.tv/auth#?code=ABCD" });
      case "POST /auth/session":
        return json({ done: true, token: "ether-admin-mock", expiresAt: 0 });
      case "DELETE /auth/session":
        return json({ ok: true });
      case "GET /llm/providers":
        return json({ providers: MOCK_LLM_PROVIDERS });
      case "POST /llm/models": {
        const req = body as { provider?: string; apiKey?: string };
        const p = MOCK_LLM_PROVIDERS.find((x) => x.id === (req.provider || "custom"));
        if (p?.needsKey && req.apiKey === "bad")
          return json({ models: [], error: "401: invalid key" });
        return json({
          models: MOCK_LLM_MODELS[p?.id ?? ""] ?? [{ id: "llama3" }, { id: "qwen3:8b" }],
        });
      }
      case "GET /system/load":
        return json(mockSystemLoad(Date.now()));
      case "POST /outbound/check": {
        const posted = body as { enabled?: boolean; url?: string };
        if (posted?.enabled && posted.url === "http://down") {
          return json({
            sourceError: "proxy did not answer",
            publishedIp: "203.0.113.7",
            remoteAccess: true,
            proxyEnabled: true,
          });
        }
        if (posted?.enabled) {
          return json({
            sourceIp: "198.51.100.20",
            publishedIp: "203.0.113.7",
            remoteAccess: true,
            proxyEnabled: true,
          });
        }
        return json({
          sourceIp: "203.0.113.7",
          publishedIp: "203.0.113.7",
          remoteAccess: true,
          proxyEnabled: false,
        });
      }
      case "POST /test/usenet":
      case "POST /test/indexer":
      case "POST /test/debrid":
      case "POST /test/tmdb":
      case "POST /test/introdb":
      case "POST /test/llm":
      case "POST /test/trakt":
      case "POST /test/opensubtitles":
        return json({ ok: true, durationMs: 40 + (hash(route) % 200) });
      case "GET /playback/policy":
        return json({
          overrides: opts.capabilityOverrides ?? [],
          apps: [
            {
              protocol: "plex",
              name: "Plex for Android phones",
              product: "Plex for Android (Mobile)",
              devices: ["Android"],
              videoCodecs: [],
              subtitles: [],
              directPlay: "reported",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex for Android TV",
              product: "Plex for Android (TV)",
              devices: ["Android TV"],
              videoCodecs: [],
              subtitles: [],
              directPlay: "reported",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex Web",
              devices: ["Browser"],
              videoCodecs: [],
              subtitles: ["srt"],
              directPlay: "browser",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "jellyfin",
              name: "Jellyfin TV",
              devices: ["Android TV"],
              videoCodecs: [],
              subtitles: ["srt"],
              directPlay: "reported",
              note: "",
              certification: "pending",
            },
            {
              protocol: "jellyfin",
              name: "Swiftfin",
              devices: ["Apple TV"],
              videoCodecs: [],
              subtitles: [],
              directPlay: "reported",
              note: "",
              certification: "pending",
            },
            {
              protocol: "stremio",
              name: "Stremio TV",
              devices: ["Android TV"],
              videoCodecs: [],
              subtitles: ["srt"],
              directPlay: "stream",
              note: "",
              certification: "builtin",
            },
          ],
          devices: [
            {
              name: "Apple TV",
              id: "apple-tv",
              dolbyVisionProfiles: [5, 8],
              subtitleCodecs: ["srt"],
              videoCodecs: ["h264", "hevc"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: true,
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: true,
              notes:
                "Dolby Vision profile 7 uses a compatible HDR base layer; TrueHD requires an audio transcode unless declared supported.",
            },
            {
              name: "Android TV",
              dolbyVisionProfiles: [],
              subtitleCodecs: ["srt"],
              notes: "Text is served separately; image subtitles require explicit client support.",
            },
            {
              name: "Browser",
              dolbyVisionProfiles: [],
              subtitleCodecs: ["srt"],
              notes: "Browser codec declarations govern playback; text subtitles remain sidecars.",
            },
          ],
          observed: [
            { protocol: "jellyfin", name: "Jellyfin TV", lastSeen: "2026-10-31T12:00:00Z" },
          ],
        });
      case "GET /playback/prowlarr":
        return json({ config: {}, status: "" });
      case "POST /playback/prowlarr/tags":
        return json({
          tags: [
            { id: 1, label: "ether" },
            { id: 2, label: "anime" },
          ],
        });
      case "POST /playback/prowlarr": {
        const req = body as {
          config?: { dailyLimit?: number; dailyGrabs?: number };
          import?: boolean;
        };
        const found = [
          { id: "prowlarr-1", name: "Prowlarr Usenet", protocol: "usenet" as const },
          { id: "prowlarr-2", name: "Prowlarr Torrents", protocol: "torrent" as const },
        ].map((ix, i) => ({
          ...ix,
          url: `http://localhost:9696/${i + 1}/`,
          apiKey: "••••",
          dailyLimit: req.config?.dailyLimit ?? 0,
          dailyGrabs: req.config?.dailyGrabs ?? 0,
          priority: (state.config.indexers ?? []).length + i,
          enabled: true,
          movieCategories: null,
          tvCategories: null,
        }));
        if (req.import) {
          const have = new Set((state.config.indexers ?? []).map((ix) => ix.id));
          state.config.indexers = [
            ...(state.config.indexers ?? []),
            ...found.filter((ix) => !have.has(ix.id)),
          ];
        }
        return json({ indexers: found, imported: Boolean(req.import) });
      }
      case "GET /subtitle-sync":
        return json([
          {
            id: "abc123",
            language: "pt-br",
            release: "Some.Show.S01E02.720p.WEB-DL-GROUP",
            fetchedAt: new Date(Date.now() - 3 * 3600_000).toISOString(),
            title: "The Episode",
            showTitle: "Some Show",
            season: 1,
            episode: 2,
          },
          {
            id: "orphan99",
            language: "en",
            release: "Other.Movie.2023.720p",
            fetchedAt: new Date(Date.now() - 26 * 3600_000).toISOString(),
            title: "",
          },
        ]);
      case "GET /disk":
        return json({
          path: q.get("path") ?? "",
          probedPath: "/",
          totalBytes: 2e12,
          freeBytes: 1.2e12,
          usedBytes: 0.8e12,
          cacheBudgetBytes: state.config.cache.budgetBytes,
        });
      case "GET /updates":
        return json(updateStatus());
      case "POST /updates/check":
        return json(updateStatus(), 202);
      case "POST /updates/apply":
        updateRequested = true;
        updateForced = Boolean((body as { force?: boolean } | null)?.force);
        return json(updateStatus(), 202);
      case "GET /users":
        return json({
          users: state.users
            .filter((u) => !state.removedViewers.has(u.id))
            .map((u) => {
              const lifecycle = u.lifecycle ?? mockLifecycle(u);
              return { ...u, lifecycle: { ...lifecycle, paused: state.pausedViewers.has(u.id) } };
            }),
        });
      case "GET /cluster":
        return json(clusterStatus());
      case "GET /cluster/setup":
        return json({ available: true, state: clusterSetupState });
      case "POST /cluster/setup/check": {
        const req = body as { mode: string; token: string; replace: boolean };
        if (req.mode === "join" && state.boot.setupComplete && !req.replace)
          return err(409, "replace_required", "Confirm replacement of this server’s identity.");
        if (req.mode === "join" && !req.token.startsWith("ether-join-v1:"))
          return err(400, "invalid_token", "The join token is not valid.");
        return json({ ok: true });
      }
      case "POST /cluster/setup/apply":
        state.clustered = true;
        clusterSetupState = "complete";
        return json({ state: "restarting" }, 202);
      case "PATCH /cluster/node": {
        const req = body as { weight?: number; draining?: boolean };
        if (req.weight !== undefined) state.config.server.clusterWeight = req.weight;
        if (req.draining !== undefined) nodePaused = req.draining;
        return json({ weight: state.config.server.clusterWeight || 1, draining: nodePaused });
      }
      case "GET /cluster/settings":
        return json(
          state.clustered
            ? {
                clustered: true,
                keyFingerprint: "3f9c2a71d04be6a8",
                sharedVersion: 42,
                // Match the active-only owner token list; retain historical fixtures
                // in state so UI filtering can also be exercised independently.
                joinTokens: state.joinTokens.filter(
                  (token) => !token.usedAt && Date.parse(token.expiresAt) > Date.now(),
                ),
              }
            : { clustered: false, keyFingerprint: "3f9c2a71d04be6a8" },
        );
      case "POST /cluster/join-tokens": {
        if (!state.clustered)
          return err(409, "not_clustered", "clustering is not enabled: this server uses SQLite.");
        const ttl = Number((body as { ttlMinutes?: number } | null)?.ttlMinutes ?? 0);
        const minutes = ttl > 0 && ttl < 60 ? ttl : 60;
        const id = `m${seq++}x`;
        const info: JoinTokenInfo = {
          id,
          createdAt: new Date().toISOString(),
          expiresAt: new Date(Date.now() + minutes * 60_000).toISOString(),
        };
        state.joinTokens.unshift(info);
        return json(
          {
            token: `ether-join-v1:mock-${id}-c2VjcmV0LWRvLW5vdC1zaGFyZQ`,
            id,
            expiresAt: info.expiresAt,
            command: "ether cluster join <token>",
            warning:
              "This token is a secret: it contains the cluster database password and the credential key that decrypts every stored credential. Use it once, within the hour, and do not paste it into chats or tickets.",
          },
          201,
        );
      }
      case "GET /adoption/backups":
        return json({
          snapshots: state.backups,
          settings: state.backupSettings,
          warning: "",
        });
      case "POST /adoption/backups/settings": {
        const next = body as {
          intervalHours: number;
          retain: number;
          sharedDir?: string;
          skipOnAirMedia?: boolean;
        };
        if (next.sharedDir && !state.clustered)
          return err(
            400,
            "invalid_request",
            "a shared backup directory applies only to clustered servers",
          );
        state.backupSettings = {
          ...state.backupSettings,
          intervalHours: next.intervalHours,
          retain: next.retain,
          skipOnAirMedia: !!next.skipOnAirMedia,
          ...(next.sharedDir !== undefined ? { sharedDir: next.sharedDir } : {}),
        };
        return json({ ok: true });
      }
      case "POST /adoption/backups": {
        const created = new Date().toISOString();
        const snap = {
          name: `ether-${created.slice(0, 19).replace(/[-:]/g, "")}.tar.gz`,
          size: 4_300_000,
          created,
          ...(state.clustered
            ? state.backupSettings.sharedDir
              ? { node: "den", nodeId: "n-den", shared: true }
              : { node: "den", nodeId: "n-den" }
            : {}),
        };
        state.backups.unshift(snap);
        return json(snap);
      }
      case "GET /logs": {
        const lvl = q.get("level") ?? "";
        const order = ["debug", "info", "warn", "error"];
        const needle = (q.get("q") ?? "").toLowerCase();
        const entries = state.logs.filter(
          (e) =>
            (!lvl || order.indexOf(e.level.toLowerCase()) >= order.indexOf(lvl)) &&
            (!needle || e.message.toLowerCase().includes(needle)),
        );
        return json({
          entries: entries.slice(-Number(q.get("limit") ?? 500)),
          total: state.logs.length,
        });
      }
      case "DELETE /logs":
        state.logs = [];
        return json({ ok: true });
      case "GET /logs/download":
        return new Response(
          "# Ether mock logs\n" +
            state.logs.map((e) => `${e.time} ${e.level} ${e.message}`).join("\n"),
          { status: 200, headers: { "Content-Type": "text/plain; charset=utf-8" } },
        );
      case "GET /collections":
        return json({ collections: state.collections });
      case "POST /collections": {
        const spec = normalizeSpec(body as CollectionSpec);
        const saved = {
          ...spec,
          id: `c${seq++}`,
          updatedAt: new Date().toISOString(),
          lastBuiltAt: new Date().toISOString(),
          itemCount:
            spec.limit > 0 ? Math.min(evaluate(spec).total, spec.limit) : evaluate(spec).total,
        };
        state.collections.push(saved);
        return json(saved);
      }
      case "POST /collections/preview":
        return json(evaluate(normalizeSpec(body as CollectionSpec)));
      case "GET /collections/templates":
        return json({ templates: TEMPLATES });
      case "GET /collections/template-bundles":
        return json({ bundles: TEMPLATE_BUNDLES });
      case "POST /catalog/visibility/preview": {
        const groups =
          (body as { exclusions: { mediaType: string; match: string; rules: Rule[] }[] })
            .exclusions ?? [];
        const hits = (g: (typeof groups)[number]) =>
          catalog.filter(
            (t) =>
              (g.mediaType === "mixed" || g.mediaType === t.type) &&
              (g.match === "any"
                ? g.rules.some((r) => fieldValues(t, r.field).length > 0 && matches(t, r))
                : g.rules.every((r) => fieldValues(t, r.field).length > 0 && matches(t, r))),
          );
        const items = catalog.filter((t) =>
          hiddenByRules(
            t,
            groups,
            String((body as { visibilityDefault?: string }).visibilityDefault ?? "show"),
          ),
        );
        const shown = catalog.filter((t) => !items.includes(t));
        return json({
          total: catalog.length,
          hidden: items.length,
          visible: shown.length,
          movies: items.filter((t) => t.type === "movie").length,
          shows: items.filter((t) => t.type === "show").length,
          rules: groups.map((g) => {
            const hit = hits(g);
            return {
              movies: hit.filter((t) => t.type === "movie").length,
              shows: hit.filter((t) => t.type === "show").length,
            };
          }),
          unknownCountry: catalog.filter((t) => !fieldValues(t, "country").length).length,
          items: items.slice(0, 48).map(card),
          shown: shown.slice(0, 48).map(card),
        });
      }
      case "GET /catalog/interpret":
        return json({ enabled: Boolean(state.config.llm.enabled && state.config.llm.model) });
      case "POST /catalog/interpret":
        return state.config.llm.enabled
          ? json({ chips: interpretDemo((body as { text: string }).text || "") })
          : err(501, "not_configured", "AI filters are unavailable");
      case "GET /catalog/browse": {
        const needle = (q.get("q") ?? "").toLowerCase();
        const advanced = JSON.parse(q.get("filter") || "{}");
        const type = q.get("type");
        const genre = q.get("genre");
        const decade = q.get("decade");
        const waiting = q.get("waiting") === "1";
        const sort = q.get("sort");
        const ruleVisibility = q.get("ruleVisibility");
        const ruleGroups = JSON.parse(q.get("rules") || "[]") as VisibilityGroup[];
        const items = catalog.filter(
          (t) =>
            (!ruleVisibility ||
              hiddenByRules(t, ruleGroups, q.get("ruleFallback") ?? "show") ===
                (ruleVisibility === "hidden")) &&
            matchesCatalogFilter(t, advanced, ownerTags, savedForViewer) &&
            t.title.toLowerCase().includes(needle) &&
            (!type || t.type === type) &&
            (!waiting || t.type === "movie") &&
            (!genre || t.genres?.includes(genre)) &&
            (!decade || decadeOf(t.year) === decade),
        );
        const compare = (a: FakeTitle, b: FakeTitle, field: string) =>
          field === "title"
            ? a.title.localeCompare(b.title)
            : field === "year"
              ? (a.year || 0) - (b.year || 0)
              : field === "duration"
                ? (a.runtimeMinutes || 0) - (b.runtimeMinutes || 0)
                : field === "rating"
                  ? (a.rating || 0) - (b.rating || 0)
                  : field === "released"
                    ? a.released.localeCompare(b.released)
                    : field === "added"
                      ? a.addedAt.localeCompare(b.addedAt)
                      : a.popularity - b.popularity;
        const desc = q.has("desc") ? q.get("desc") === "1" : sort !== "title";
        const secondary = q.get("secondarySort");
        items.sort(
          (a, b) =>
            compare(a, b, sort || "popularity") * (desc ? -1 : 1) ||
            (secondary
              ? compare(a, b, secondary) * (q.get("secondaryDesc") === "1" ? -1 : 1)
              : 0) ||
            a.key.localeCompare(b.key),
        );
        const after = q.get("after");
        const offset = after
          ? Math.max(0, items.findIndex((item) => item.key === after) + 1)
          : Number(q.get("offset") ?? 0);
        const limit = Number(q.get("limit") ?? 48);
        const page = items.slice(offset, offset + limit);
        const hasMore = offset + limit < items.length;
        return json({
          items: page.map(card),
          total: items.length,
          totalExact: true,
          hasMore,
          nextCursor: hasMore ? page.at(-1)?.key : "",
        });
      }
      case "GET /catalog/search": {
        const needle = (q.get("q") ?? "").toLowerCase();
        const type = q.get("type");
        return json({
          items: catalog
            .filter(
              (t) =>
                t.title.toLowerCase().includes(needle) &&
                (!type || type === "mixed" || t.type === type),
            )
            .slice(0, Number(q.get("limit") ?? 24))
            .map(card),
        });
      }
      case "GET /catalog/facets": {
        const facet = q.get("facet");
        const type = q.get("type");
        const pool = catalog.filter((t) => !type || type === "mixed" || t.type === type);
        if (facet === "contentRating") {
          const level = (t: FakeTitle) => fieldValues(t, "contentRating")[0];
          return json({
            values: [
              ...RATING_LEVELS.map((l) => ({
                key: l.value,
                title: l.labels,
                count: pool.filter((t) => level(t) === l.value).length,
                labels: [...l.labels.split(" · "), ...(MOCK_EQUIVALENTS[l.value] ?? [])],
              })),
              {
                key: NOT_RATED,
                title: NOT_RATED,
                count: pool.filter((t) => level(t) === NOT_RATED).length,
                labels: [NOT_RATED],
              },
            ],
          });
        }
        const counts = new Map<string, number>();
        for (const t of pool) {
          const vals =
            facet === "tag"
              ? ownerTags.get(t.key) || []
              : facet === "genre"
                ? (t.genres ?? [])
                : facet === "decade"
                  ? [decadeOf(t.year)]
                  : facet === "contentRating"
                    ? [t.contentRating ?? ""]
                    : facet === "country"
                      ? [t.country]
                      : facet === "language"
                        ? [t.language]
                        : facet === "studio"
                          ? [String(t.studio)]
                          : [];
          for (const v of vals) if (v) counts.set(v, (counts.get(v) ?? 0) + 1);
        }
        const title = (k: string) =>
          facet === "decade"
            ? `${k}s`
            : facet === "studio"
              ? (STUDIOS.find((s) => String(s.id) === k)?.name ?? k)
              : facet === "language"
                ? (new Intl.DisplayNames(["en"], { type: "language" }).of(k) ?? k)
                : facet === "country"
                  ? (new Intl.DisplayNames(["en"], { type: "region" }).of(k) ?? k)
                  : k;
        return json({
          values: [...counts.entries()]
            .sort((a, b) => (facet === "decade" ? a[0].localeCompare(b[0]) : b[1] - a[1]))
            .map(([key, count]) => ({ key, title: title(key), count })),
        });
      }
      case "GET /catalog/lookup": {
        const kind = q.get("kind");
        const needle = (q.get("q") ?? "").toLowerCase();
        const pool =
          kind === "person"
            ? PEOPLE.map((x) => ({
                id: x.id,
                name: x.name,
                subtitle: x.role,
                image: avatarFor(x.name),
              }))
            : kind === "keyword"
              ? KEYWORDS
              : kind === "company"
                ? STUDIOS
                : kind === "network"
                  ? NETWORKS
                  : kind === "collection"
                    ? COLLECTIONS
                    : kind === "watchProvider"
                      ? PROVIDERS.map((x) => ({ ...x, subtitle: q.get("region") ?? "US" }))
                      : [];
        return json({
          results: pool.filter((x) => x.name.toLowerCase().includes(needle)).slice(0, 10),
        });
      }
      case "GET /viewers":
        return json({
          viewers: [...VIEWERS, ...state.dlnaGuests, ...state.addedViewers]
            .filter((v) => !state.removedViewers.has(v.id))
            .map((v, i) => {
              const lifecycle = v.lifecycle ?? mockLifecycle(v);
              return {
                ...v,
                thumb: avatarFor(v.title),
                lastActive: new Date(Date.now() - (i * 7 + 1) * 3600_000).toISOString(),
                lifecycle: { ...lifecycle, paused: state.pausedViewers.has(v.id) },
              };
            }),
        });
      case "GET /layout":
        return json(layoutDoc());
      case "PUT /layout": {
        const doc = (body as { layout?: HomeLayout }).layout;
        if (!doc)
          return json(
            {
              error: {
                code: "validation",
                message: "layout: is required",
                fields: [{ field: "layout", message: "is required" }],
              },
            },
            400,
          );
        const problems = validateLayout(doc);
        if (problems.length)
          return json(
            {
              error: {
                code: "validation",
                message: `${problems[0]!.field}: ${problems[0]!.message}`,
                fields: problems,
              },
            },
            400,
          );
        state.layout = {
          global: doc.global ?? [],
          viewers: doc.viewers ?? [],
          updatedAt: new Date().toISOString(),
        };
        // Edited collection schedules are saved on their collections.
        for (const [id, schedule] of Object.entries(doc.collectionSchedules ?? {})) {
          const c = state.collections.find((x) => String(x.id) === id);
          if (c) c.targets = { ...c.targets, schedule: tidySchedule(schedule) ?? null };
        }
        return json(layoutDoc());
      }
      case "POST /layout/preview": {
        const b = body as {
          viewerId?: number | null;
          layout?: HomeLayout;
          device?: string;
          at?: string;
          section?: string;
        };
        if (b.layout) {
          const problems = validateLayout(b.layout);
          if (problems.length)
            return json(
              {
                error: {
                  code: "validation",
                  message: `${problems[0]!.field}: ${problems[0]!.message}`,
                  fields: problems,
                },
              },
              400,
            );
        }
        if (b.viewerId != null && !VIEWERS.some((v) => v.id === b.viewerId))
          return err(404, "not_found", "no such viewer");
        return json(
          homeFor(
            b.viewerId ?? null,
            b.device ?? "tv",
            b.at ? new Date(b.at) : new Date(),
            b.section ?? "home",
            b.layout ?? state.layout,
          ),
        );
      }
      case "POST /layout/impact": {
        const b = body as { layout: HomeLayout; device?: string; at?: string; section?: string };
        return json(
          impact(
            b.layout,
            b.device ?? "tv",
            b.at ? new Date(b.at) : new Date(),
            b.section ?? "home",
          ),
        );
      }
      case "GET /insights/ranker":
        return json({
          trainedAt: new Date(Date.now() - 3 * 3600e3).toISOString(),
          examples: 18420,
          positives: 612,
          auc: 0.78,
          baselineAuc: 0.69,
          active: true,
          features: [
            { name: "fit", label: "Taste fit", weight: 1.42 },
            { name: "session", label: "Matches what they're in the mood for", weight: 0.88 },
            { name: "recency", label: "Newly released", weight: 0.51 },
            { name: "fatigue", label: "Shown before without a play", weight: -0.94 },
            { name: "kind.top-10", label: "In Top 10 for You", weight: 0.47 },
            { name: "runtime", label: "Longer than they usually watch", weight: -0.33 },
          ],
        });
      case "GET /insights/rows":
        return json({
          rows: availableRows()
            .slice(0, 9)
            .map((r) => {
              const impressions7d = 80 + (hash(r.hubId) % 400);
              const plays7d = Math.round(impressions7d * ((hash(r.hubId + "p") % 30) / 100));
              return {
                hubId: r.hubId,
                title: r.title,
                impressions7d,
                plays7d,
                playRate: plays7d / impressions7d,
              };
            }),
        });
      case "GET /insights/activity":
        return json({ days: activity() });
    }

    if (
      seg[0] === "collections" &&
      seg[1] === "stremio" &&
      seg[2] === "manifest" &&
      method === "POST"
    ) {
      return json({
        id: "example.catalog",
        name: "Example catalog",
        catalogs: [{ id: "top", type: "movie", name: "Top movies" }],
      });
    }

    if (
      seg[0] === "collections" &&
      seg[1] === "template-bundles" &&
      seg[2] &&
      (seg[3] === "preview" || seg[3] === "apply") &&
      method === "POST"
    ) {
      const bundleId = decodeURIComponent(seg[2]);
      const bundle = TEMPLATE_BUNDLES.find((b) => b.id === bundleId);
      if (!bundle) return err(404, "not_found", "no such template bundle");
      const items = bundle.templateIds
        .map((tid) => {
          const tpl = TEMPLATES.find((t) => t.id === tid);
          if (!tpl) return null;
          const existing = state.collections.find(
            (c) => c.templateId === tid || c.name.toLowerCase() === tpl.name.toLowerCase(),
          );
          return {
            templateId: tid,
            name: tpl.name,
            category: tpl.category,
            action: existing ? ("skip_existing" as const) : ("create" as const),
            ...(existing?.id ? { existingId: existing.id } : {}),
          };
        })
        .filter((x): x is NonNullable<typeof x> => Boolean(x));
      if (seg[3] === "preview") {
        return json({
          bundle,
          items,
          willCreate: items.filter((i) => i.action === "create").length,
          willSkip: items.filter((i) => i.action === "skip_existing").length,
        });
      }
      const created: CollectionSpec[] = [];
      const skipped = items.filter((i) => i.action === "skip_existing");
      for (const item of items) {
        if (item.action !== "create") continue;
        const tpl = TEMPLATES.find((t) => t.id === item.templateId);
        if (!tpl) continue;
        const spec = normalizeSpec({
          ...tpl.spec,
          name: tpl.name,
          description: tpl.description,
          templateId: tpl.id,
          enabled: true,
        });
        const saved: CollectionSpec = {
          ...spec,
          id: `c${seq++}`,
          updatedAt: new Date().toISOString(),
          lastBuiltAt: new Date().toISOString(),
          itemCount: evaluate(spec).total,
        };
        state.collections.push(saved);
        created.push(saved);
      }
      return json({
        bundleId,
        bundle,
        items,
        created,
        skipped,
        createdCount: created.length,
        skippedCount: skipped.length,
      });
    }

    // Parameterised routes
    if (seg[0] === "collections" && seg[1]) {
      const id = decodeURIComponent(seg[1]);
      const idx = state.collections.findIndex((c) => c.id === id);
      if (seg[2] === "artwork" && method === "GET") {
        const c = state.collections[idx];
        const svg = decodeURIComponent(posterFor(id, c?.name ?? "Collection").split(",")[1] ?? "");
        return new Response(svg, { status: 200, headers: { "Content-Type": "image/svg+xml" } });
      }
      if (idx < 0) return err(404, "not_found", "no such collection");
      const current = state.collections[idx]!;
      if (seg[2] === "rebuild" && method === "POST") {
        const r = evaluate(current);
        current.itemCount = r.total;
        current.lastBuiltAt = new Date().toISOString();
        return json({ itemCount: r.total, lastBuiltAt: current.lastBuiltAt });
      }
      if (method === "GET") return json(current);
      if (method === "PUT") {
        const input = normalizeSpec(body as CollectionSpec);
        const spec = current.builtIn
          ? {
              ...current,
              artwork: input.artwork,
              targets: input.targets,
              enabled: input.enabled,
              rotate: input.rotate,
              limit: input.limit > 0 && input.limit <= 100 ? input.limit : current.limit,
            }
          : input;
        const saved = {
          ...spec,
          id,
          updatedAt: new Date().toISOString(),
          itemCount: evaluate(spec).total,
          lastBuiltAt: new Date().toISOString(),
        };
        state.collections[idx] = saved;
        return json(saved);
      }
      if (method === "DELETE") {
        state.collections.splice(idx, 1);
        return new Response(null, { status: 204 });
      }
    }
    if (seg[0] === "catalog" && seg[1] === "title" && seg[2]) {
      const t = byKey.get(decodeURIComponent(seg[2]));
      const followed = t ? null : calendar.title(decodeURIComponent(seg[2]));
      if (followed) return json(followed);
      if (!t) return err(404, "not_found", "no such title");
      if (seg[3] === "tags") {
        if (method === "PUT")
          ownerTags.set(t.key, [
            ...new Set(
              ((body as { tags: string[] }).tags || [])
                .map((tag) => tag.trim().toLowerCase())
                .filter(Boolean),
            ),
          ]);
        return json({ tags: ownerTags.get(t.key) || [] });
      }
      return json({
        ...card(t),
        cast: t.people.map((id) => PEOPLE.find((p) => p.id === id)).filter(Boolean),
        directors: [],
        keywords: t.keywords.map((id) => KEYWORDS.find((k) => k.id === id)).filter(Boolean),
        collection: null,
      });
    }
    if (seg[0] === "viewers" && seg[1]) {
      const id = Number(seg[1]);
      const v = [...VIEWERS, ...state.dlnaGuests, ...state.addedViewers].find((x) => x.id === id);
      if (!v || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
      if (seg[2] === "catalog-rules") {
        if (method === "PUT") viewerCatalogRules.set(id, body);
        const globalRules = [
          {
            name: "French films",
            action: "hide",
            mediaType: "movie",
            match: "all",
            scope: "discovery",
            rules: [{ field: "language", op: "is", value: "fr" }],
          },
        ];
        return json({
          policy: viewerCatalogRules.get(id) ?? {
            mode: "inherit",
            exclusions: [],
            visibilityDefault: "show",
            exemptions: [],
          },
          globalRules,
          globalDefault: "show",
          globalIDs: globalRules.map((_, i) => `mock-rule-${i}`),
        });
      }
      if (seg[2] === "quickconnect" && method === "POST") {
        const code = String((body as { code?: string } | null)?.code ?? "");
        if (code === "EXPIRED") return err(400, "expired", "code expired");
        if (code === "USEDUP") return err(400, "used", "code already used");
        if (!/^[A-HJ-NP-Z2-9]{6}$/.test(code)) return err(400, "invalid", "invalid code");
        return json({ ok: true, device: "Living Room Apple TV", app: "Jellyfin" });
      }
      if (seg[2] === "favorites-mirror") {
        if (method === "POST")
          state.favoritesMirror.set(id, (body as { enabled: boolean }).enabled);
        return json({ enabled: state.favoritesMirror.get(id) ?? true });
      }
      if (!seg[2] && method === "GET") {
        const recent = pickTitles(`recent${id}`, 8).map((t, i) => ({
          title: card(t),
          at: new Date(Date.now() - (i * 19 + 2) * 3600_000).toISOString(),
          completed: i % 3 !== 1,
          progress: i % 3 === 1 ? 0.35 + (i % 4) * 0.1 : 1,
        }));
        return json({
          ...v,
          thumb: avatarFor(v.title),
          lastActive: new Date(Date.now() - 3600_000).toISOString(),
          lifecycle: { ...(v.lifecycle ?? mockLifecycle(v)), paused: state.pausedViewers.has(id) },
          recent,
        });
      }
      if (seg[2] === "taste" && !seg[3] && method === "GET") return json(taste(id));
      if (seg[2] === "taste" && seg[3] === "overrides" && method === "PUT") {
        state.overrides.set(id, (body as { overrides: TasteOverride[] }).overrides ?? []);
        return json(taste(id));
      }
      if (seg[2] === "home" && method === "GET") {
        const at = q.get("at") ? new Date(q.get("at")!) : new Date();
        return json(homeFor(id, q.get("device") ?? "tv", at, q.get("section") ?? "home"));
      }
      if (seg[2] === "refresh" && method === "POST") return new Response(null, { status: 204 });
      if (seg[2] === "plex-watch") {
        const cur = plexWatch.get(id) ?? {
          connected: false,
          direction: 0,
          importInterval: 10,
          lastImport: 0,
          lastFullImport: 0,
          lastSend: 0,
          nextImport: 0,
          problem: "",
          needsReconnect: false,
          pending: 0,
          failed: 0,
          restricted: false,
          accountMatch: true,
        };
        const action = seg[3];
        const nowSec = Math.floor(Date.now() / 1000);
        if (!action && method === "GET") return json(cur);
        if (action === "pin" && !seg[4] && method === "POST") {
          const next = {
            ...cur,
            pinCode: "MOCK",
            pinUrl: "https://app.plex.tv/auth#?code=MOCK",
            accountMatch: true,
          };
          plexWatch.set(id, next);
          return json(next);
        }
        if (action === "pin" && seg[4] === "finish" && method === "POST") {
          if (v.plexId <= 0) {
            return err(
              409,
              "account_mismatch",
              "the Plex account that approved the code is not this viewer",
            );
          }
          const next = {
            ...cur,
            connected: true,
            username: v.username,
            plexId: v.plexId,
            pinCode: undefined,
            pinUrl: undefined,
            needsReconnect: false,
            problem: "",
            accountMatch: true,
          };
          plexWatch.set(id, next);
          return json(next);
        }
        if (!action && method === "PUT") {
          const b = body as { direction?: number; interval?: number };
          const next = {
            ...cur,
            direction: b?.direction ?? cur.direction,
            importInterval: b?.interval ?? cur.importInterval,
          };
          plexWatch.set(id, next);
          return json(next);
        }
        if (action === "disconnect" && method === "POST") {
          const next = {
            ...cur,
            connected: false,
            direction: 0,
            needsReconnect: false,
            problem: "",
            username: undefined,
          };
          plexWatch.set(id, next);
          return json(next);
        }
        if (action === "import" && method === "POST") {
          const next = { ...cur, lastImport: nowSec, lastFullImport: nowSec };
          plexWatch.set(id, next);
          return json(next);
        }
        if ((action === "retry" || action === "export") && method === "POST") {
          const next = { ...cur, pending: 0, failed: 0, lastSend: nowSec };
          plexWatch.set(id, next);
          return json(next);
        }
      }
    }
    // Per-viewer safety policy (Viewer › Limits). The preset's rating limit
    // is for the user record; the rest is the policy.
    if (seg[0] === "safety" && seg[1] === "policies") {
      if (seg[2] === "presets" && seg[3] === "kids" && method === "GET") {
        const age = url.searchParams.get("age");
        const rating = age === "preschool" ? "TV-Y" : age === "teens" ? "PG-13" : "PG";
        return json({
          maxContentRating: rating,
          blockedGenres: null,
          blockedKeywords: ["pornography", "erotic movie"],
          remoteDisabled: false,
          maxBitrateKbps: 0,
          allowedHours: null,
          timezone: "",
        });
      }
      const id = Number(seg[2]);
      if (Number.isFinite(id) && method === "GET") return json(safetyPolicies.get(id) ?? {});
      if (Number.isFinite(id) && method === "PUT") {
        safetyPolicies.set(id, (body ?? {}) as Record<string, unknown>);
        return json(safetyPolicies.get(id));
      }
    }
    if (seg[0] === "users" && seg[1] && /^\d+$/.test(seg[1])) {
      const id = Number(seg[1]);
      const target = VIEWERS.find((x) => x.id === id);
      const managed = target?.lifecycle?.managedBy === "plex";
      const plexManaged = () => err(409, "managed_by_plex", "This viewer is managed on plex.tv.");
      if (seg[2] === "pause" && method === "POST") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        if (target.isOwner) return err(403, "forbidden", "The owner cannot be paused.");
        if (managed) return plexManaged();
        state.pausedViewers.add(id);
        return json({ paused: true });
      }
      if (seg[2] === "resume" && method === "POST") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        if (target.isOwner) return err(403, "forbidden", "The owner cannot be paused.");
        if (managed) return plexManaged();
        state.pausedViewers.delete(id);
        return json({ paused: false });
      }
      if (seg[2] === "signout" && method === "POST") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        return json({ revoked: 2 });
      }
      if (seg[2] === "merge" && method === "POST") {
        const into = Number((body as { into?: number } | null)?.into ?? 0);
        const dest = VIEWERS.find((x) => x.id === into);
        if (!target || !dest || state.removedViewers.has(id) || into === id)
          return err(400, "invalid", "invalid merge");
        if (target.isOwner || dest.isOwner)
          return err(403, "forbidden", "The owner cannot be merged.");
        if (managed && dest.lifecycle?.managedBy === "plex")
          return err(409, "conflict", "Two Plex viewers cannot be merged.");
        if (managed) return plexManaged();
        state.pausedViewers.delete(id);
        state.removedViewers.add(id);
        return json({ merged: true, into });
      }
      if (seg[2] === "invite" && method === "POST") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        if (target.isOwner) return err(403, "forbidden", "The owner needs no invite.");
        return json({ url: "/ether/me#invite=mock-secret" });
      }
      if (!seg[2] && method === "GET") {
        const user = state.users.find((u) => u.id === id);
        if (!user || state.removedViewers.has(id)) return err(404, "not_found", "no such user");
        return json({
          user: {
            ...user,
            lifecycle: user.lifecycle
              ? { ...user.lifecycle, paused: state.pausedViewers.has(id) }
              : user.lifecycle,
          },
        });
      }
      if (!seg[2] && method === "DELETE") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        if (target.isOwner) return err(403, "forbidden", "The owner cannot be removed.");
        if (managed) return plexManaged();
        state.pausedViewers.delete(id);
        state.removedViewers.add(id);
        return json({ removed: true });
      }
    }
    // One viewer's Simkl account (Viewer › Limits).
    if (seg[0] === "simkl" && seg[1] && /^\d+$/.test(seg[1])) {
      const id = Number(seg[1]);
      const cur = simklAccounts.get(id) ?? { connected: false };
      if (seg[2] === "status" && method === "GET") return json(cur);
      if (seg[2] === "start" && method === "POST")
        return json({
          userCode: "ABCD-EFGH",
          verificationUri: "https://simkl.com/pin",
          verificationUriComplete: "https://simkl.com/pin?user_code=ABCD-EFGH",
          expiresAt: new Date(Date.now() + 15 * 60_000).toISOString(),
          interval: 5,
        });
      if (seg[2] === "poll" && method === "POST") {
        simklAccounts.set(id, { connected: true });
        return json({ status: "authorized" });
      }
      if (seg[2] === "sync" && method === "POST") {
        simklAccounts.set(id, { ...cur, lastSync: new Date().toISOString() });
        return json({ imported: 3, skipped: 0 });
      }
      if (seg[2] === "disconnect" && method === "POST") {
        simklAccounts.delete(id);
        return json({ ok: true });
      }
    }
    if (seg[0] === "users" && seg[1] && (method === "PATCH" || method === "PUT")) {
      const id = Number(seg[1]);
      const i = state.users.findIndex((u) => u.id === id);
      if (i < 0) return err(404, "not_found", "no such user");
      state.users[i] = { ...state.users[i]!, ...(body as Partial<EtherUser>) };
      return json({ user: state.users[i] });
    }
    if (seg[0] === "config" && seg[1] && method === "GET") {
      const key =
        seg[1] === "opensubtitles"
          ? "openSubtitles"
          : seg[1] === "plexservers"
            ? "plexServers"
            : seg[1];
      return json({
        section: key,
        value: (state.config as unknown as Record<string, unknown>)[key],
        ...(key === "clientIdentity" ? { presets: IDENTITY_PRESETS } : {}),
      });
    }
    if (seg[0] === "config" && seg[1] && method === "PUT") {
      const key =
        seg[1] === "opensubtitles"
          ? "openSubtitles"
          : seg[1] === "plexservers"
            ? "plexServers"
            : seg[1];
      // Like the server: the body is merged onto the stored section.
      const prev = (state.config as unknown as Record<string, unknown>)[key];
      let value =
        prev &&
        typeof prev === "object" &&
        !Array.isArray(prev) &&
        body &&
        typeof body === "object" &&
        !Array.isArray(body)
          ? { ...(prev as Record<string, unknown>), ...(body as Record<string, unknown>) }
          : body;
      if (
        key === "plex" &&
        body !== null &&
        typeof body === "object" &&
        !Array.isArray(body) &&
        (body as { claimed?: boolean }).claimed === false
      ) {
        value = { ...(value as Record<string, unknown>), token: "" };
      }
      (state.config as unknown as Record<string, unknown>)[key] = value;
      return json({ section: key, value });
    }
    if (seg[0] === "cluster" && seg[1] === "join-tokens" && seg[2] && method === "DELETE") {
      if (!state.clustered) return err(409, "not_clustered", "clustering is not enabled");
      const id = decodeURIComponent(seg[2]);
      const i = state.joinTokens.findIndex((t) => t.id === id && !t.usedAt);
      if (i < 0) return err(404, "not_found", "no unused token with that id");
      state.joinTokens.splice(i, 1);
      return new Response(null, { status: 204 });
    }
    if (seg[0] === "auth" && seg[1] === "pin" && seg[2])
      return json({ done: true, token: "mock-owner-token" });
    if (seg[0] === "plex" && seg[1] === "pin" && seg[2]) return json({ done: true });
    if (seg.join("/") === "status/database/compact") {
      return json({
        value: {
          supported: true,
          incremental: false,
          databaseBytes: 1_103_679_488,
          reclaimableBytes: 0,
          needBytes: 1_372_114_944,
          diskFreeBytes: 58_090_143_744,
          tempNeedBytes: 1_372_114_944,
          tempFreeBytes: 58_090_143_744,
          estimateSeconds: 48,
          ready: true,
          running: method === "POST",
          ...(method === "POST" ? { started: new Date().toISOString() } : {}),
        },
      });
    }
    if (method === "POST" && seg.join("/") === "status/database/check") {
      const value = dashboard().sections.database;
      if (value.integrity) value.integrity.checked = new Date().toISOString();
      return json({ value });
    }
    if (seg[0] === "status" && seg[1])
      return json({
        name: seg[1],
        value: (dashboard().sections as Record<string, unknown>)[seg[1]] ?? null,
      });
    if (seg[0] === "me") {
      const sub = seg.slice(1).join("/");
      if (method === "POST" && sub === "login") return json({ token: "viewer-grant" });
      if (method === "POST" && sub === "pin")
        return json({ handle: "pin1", code: "ABCD", authUrl: "https://plex.example/auth" });
      if (method === "GET" && sub === "pin/pin1")
        return json({ done: true, token: "viewer-grant" });
      if (method === "GET" && sub === "") return json({ id: 1, username: "alice", title: "Alice" });
      if (method === "POST" && sub === "quickconnect") {
        const code = String((body as { code?: string } | null)?.code ?? "");
        if (code === "EXPIRED") return err(400, "expired", "code expired");
        if (code === "USEDUP") return err(400, "used", "code already used");
        if (!/^[A-HJ-NP-Z2-9]{6}$/.test(code)) return err(400, "invalid", "invalid code");
        return json({ ok: true, device: "Living Room Apple TV", app: "Jellyfin" });
      }
      if (method === "GET" && sub === "identities")
        return json({ identities: [{ id: 1, provider: "plex", display: "alice" }] });
      if (method === "GET" && sub === "devices")
        return json({
          devices: [
            {
              hash: "abc",
              surface: "jellyfin",
              client: "Apple TV",
              product: "Jellyfin",
              version: "1",
              lastUsed: Date.now(),
            },
          ],
        });
      if (sub === "favorite-episode-alert") return json({ enabled: false });
      if (method === "POST" && sub === "invite/redeem") return json({ token: "viewer-grant" });
      if (method === "POST" && sub === "password") return json({ ok: true });
      if (method === "POST" && sub === "devices/revoke") return json({ ok: true });
    }
    if (seg[0] === "users" && seg[1] && seg[2] === "identities" && method === "GET") {
      const who = VIEWERS.find((x) => x.id === Number(seg[1]));
      const provider = who?.lifecycle?.managedBy === "plex" ? "plex" : "local";
      return json({
        identities: [{ id: 1, provider, display: who?.username ?? "owner" }],
      });
    }
    if (
      seg[0] === "users" &&
      seg[1] &&
      seg[2] === "devices" &&
      seg.length === 3 &&
      method === "GET"
    )
      return json({ devices: xtreamGrants.get(seg[1]!) ?? [] });
    if (seg[0] === "users" && seg[1] && seg[2] === "password" && method === "POST")
      return json({ ok: true });
    if (seg[0] === "users" && seg[1] && seg[2] === "devices" && seg[3] === "revoke") {
      const hash = (body as { hash?: string })?.hash;
      xtreamGrants.set(
        seg[1],
        (xtreamGrants.get(seg[1]) ?? []).filter((g) => g.hash !== hash),
      );
      return json({ ok: true });
    }
    const viewerHidden = /^\/viewers\/(\d+)\/not-interested(?:\/restore)?$/.exec(p);
    if (viewerHidden && (method === "GET" || method === "POST")) {
      if (method === "GET")
        return json({
          items: catalog
            .slice(3, 9)
            .filter((title) => !restoredHidden.has(`${viewerHidden[1]}:${title.key}`))
            .map((title) => ({
              handle: String(title.key),
              title: title.title,
              year: title.year,
              posterPath: undefined,
              poster: title.poster,
            })),
        });
      restoredHidden.add(`${viewerHidden[1]}:${(body as { handle: string }).handle}`);
      return json({ restored: true });
    }
    return err(404, "not_found", "no such API endpoint");
  }

  async function fetchImpl(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    const req = input instanceof Request ? input : null;
    const url = new URL(req ? req.url : String(input), "http://localhost");
    const method = (init?.method ?? req?.method ?? "GET").toUpperCase();
    let body: unknown = undefined;
    const raw = init?.body ?? null;
    if (raw instanceof FormData) body = raw;
    if (typeof raw === "string" && raw) {
      try {
        body = JSON.parse(raw);
      } catch {
        body = raw;
      }
    }
    state.calls.push({
      method,
      path: url.pathname.replace(/^\/ether\/api/, "") + url.search,
      body,
    });
    if (opts.latencyMs)
      await new Promise((r) => setTimeout(r, opts.latencyMs! * (0.5 + Math.random())));
    if (init?.signal?.aborted) throw new DOMException("Aborted", "AbortError");
    const seg = url.pathname
      .replace(/^\/ether\/api/, "")
      .split("/")
      .filter(Boolean);
    const gate = ops.handle(method, seg, body, new Headers(init?.headers ?? req?.headers));
    if (gate === UNREACHABLE) throw new TypeError("Failed to fetch");
    if (gate) return json(gate.body, gate.status);
    if (seg[0] === "admin" && seg[1] === "prerolls") return prerolls(url, init);
    return handle(method, url, body);
  }

  return { fetch: fetchImpl, state, catalog, evaluate, homeFor, releaseConsole, sites };
}

export type FakeServer = ReturnType<typeof createFakeServer>;
export type { TitleCard };

const MOCK_LLM_PROVIDERS: LLMProvider[] = [
  {
    id: "anthropic",
    label: "Anthropic — Claude",
    api: "anthropic",
    baseUrl: "https://api.anthropic.com/v1",
    needsKey: true,
    local: false,
    requiresBaseUrl: false,
    keyHint: "sk-ant-…",
  },
  {
    id: "openai",
    label: "OpenAI — GPT",
    api: "openai",
    baseUrl: "https://api.openai.com/v1",
    needsKey: true,
    local: false,
    requiresBaseUrl: false,
    keyHint: "sk-…",
  },
  {
    id: "openrouter",
    label: "OpenRouter — any model, one key",
    api: "openai",
    baseUrl: "https://openrouter.ai/api/v1",
    needsKey: true,
    local: false,
    requiresBaseUrl: false,
    keyHint: "sk-or-…",
    modelHint: "Models are vendor/model slugs, e.g. openai/gpt-4o.",
  },
  {
    id: "ollama",
    label: "Ollama — on your network",
    api: "openai",
    baseUrl: "http://localhost:11434/v1",
    needsKey: false,
    local: true,
    requiresBaseUrl: false,
    keyHint: "Not needed",
  },
  {
    id: "custom",
    label: "Custom (OpenAI-compatible)",
    api: "openai",
    baseUrl: "",
    needsKey: false,
    local: true,
    requiresBaseUrl: true,
    keyHint: "Your provider API key",
  },
];

const MOCK_LLM_MODELS: Record<string, { id: string; label?: string }[]> = {
  anthropic: [
    { id: "claude-haiku-5-5", label: "Claude Haiku 5.5" },
    { id: "claude-opus-5-5", label: "Claude Opus 5.5" },
    { id: "claude-sonnet-5-5", label: "Claude Sonnet 5.5" },
  ],
  openai: [{ id: "gpt-4o" }, { id: "gpt-4o-mini" }, { id: "o4-mini" }],
  openrouter: [
    { id: "anthropic/claude-sonnet-5-5", label: "Anthropic: Claude Sonnet 5.5" },
    { id: "openai/gpt-4o", label: "OpenAI: GPT-4o" },
  ],
};
/** Plausible, gently moving machine load for the Overview. */
function mockSystemLoad(now: number): SystemLoad {
  const step = 2_000;
  const wave = (t: number, period: number, lo: number, hi: number) =>
    lo + ((Math.sin((t / period) * Math.PI * 2) + 1) / 2) * (hi - lo);
  const history: LoadSample[] = Array.from({ length: 150 }, (_, i) => {
    const t = now - (149 - i) * step;
    return {
      at: new Date(t).toISOString(),
      cpu: wave(t, 90_000, 8, 42),
      memUsed: wave(t, 240_000, 9.1, 10.4) * 2 ** 30,
      netRxBps: wave(t, 60_000, 1.5e6, 9e6),
      netTxBps: wave(t, 75_000, 0.8e6, 4.5e6),
      procCpu: wave(t, 90_000, 2, 14),
      procRss: 412 * 2 ** 20,
      gpu: wave(t, 50_000, 5, 38),
    };
  });
  return {
    cores: 12,
    memTotal: 24 * 2 ** 30,
    swapUsed: 0,
    swapTotal: 0,
    load: [1.84, 1.62, 1.4],
    intervalSeconds: 2,
    current: history.at(-1),
    history,
    gpus: [
      {
        name: "NVIDIA GeForce RTX 4080",
        util: history.at(-1)!.gpu,
        encoder: 18,
        decoder: 11,
        memUsed: 2.1 * 2 ** 30,
        memTotal: 16 * 2 ** 30,
      },
    ],
  };
}
