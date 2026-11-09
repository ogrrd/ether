import { t, tn } from "../i18n";
import {
  formatBytes,
  formatDate,
  formatNumber,
  isPlainObject,
  parseTime,
  relativeTime,
} from "./format";

/** "unknown" means unreported or unverified, not necessarily still loading. */
export type HealthLevel = "ok" | "warn" | "error" | "off" | "unknown";

export interface HealthItem {
  id:
    | "debrid"
    | "iptv"
    | "usenet"
    | "indexers"
    | "cache"
    | "catalog"
    | "database"
    | "remote"
    | "plexdirect"
    | "storage"
    | "transcode";
  label: string;
  level: HealthLevel;
  summary: string;
  details: string[];
  /** Explain a known limitation without presenting it as a running check. */
  statusLabel?: string;
  note?: string;
  /** Where the owner fixes this, for "Needs attention" and health links. */
  href: string;
  /** 0..1 gauge when the item has a natural fill level. */
  fill?: number;
  /**
   * Unverified diagnostics set this false. Failures still count, and an
   * unreported "unknown" still keeps the server Incomplete. This never means
   * the row was checked and found healthy.
   */
  countsOverall?: boolean;
  /** Expected on a new server: something to set up, not a failure. */
  setupGap?: boolean;
}

const RANK: Record<HealthLevel, number> = { error: 4, warn: 3, unknown: 2, ok: 1, off: 0 };

export function worstLevel(levels: HealthLevel[]): HealthLevel {
  let worst: HealthLevel = "ok";
  for (const l of levels) if (RANK[l] > RANK[worst]) worst = l;
  return worst;
}

/** Warn and error always count. Only an explicit unverified unknown may opt out. */
export function countsOverall(item: HealthItem): boolean {
  if (item.level !== "unknown") return true;
  return item.countsOverall !== false;
}

/** Overall rank after unverified diagnostics have opted out. */
export function overallLevel(items: HealthItem[]): HealthLevel {
  return worstLevel(items.filter(countsOverall).map((item) => item.level));
}

/** An installed certificate opted out because trust was not checked. */
export function hasExemptDiagnostic(items: HealthItem[]): boolean {
  return items.some((item) => item.countsOverall === false);
}

const num = (v: unknown): number =>
  typeof v === "number" && Number.isFinite(v) ? v : Number(v) || 0;
const str = (v: unknown): string => (typeof v === "string" ? v : "");

function errorOf(v: unknown): string {
  return isPlainObject(v) && typeof v.error === "string" ? v.error : "";
}

/** A missing first-run choice. It stays visible and does not raise an alarm. */
function toSetUp(item: HealthItem): HealthItem {
  return {
    ...item,
    level: "unknown",
    countsOverall: false,
    setupGap: true,
    statusLabel: t("To set up"),
  };
}

function torrentioReady(indexers: unknown): boolean {
  if (!Array.isArray(indexers)) return false;
  return indexers.some((row) => {
    if (!isPlainObject(row) || row.enabled === false) return false;
    const name = `${str(row.indexer)} ${str(row.name)}`.toLowerCase();
    const url = str(row.url).toLowerCase();
    return name.includes("torrentio") || url.includes("torrentio.strem.fun");
  });
}

/** Plain-language reason a Usenet provider is failing, or null when it is fine. */
export function usenetProblem(state: unknown, lastError?: unknown): string | null {
  const s = str(state);
  const err = str(lastError);
  if (!s || s === "ok") return null;
  const lower = err.toLowerCase();
  if (
    s === "auth_failed" ||
    lower.includes("authentication failed") ||
    lower.includes("481") ||
    lower.includes("482") ||
    lower.includes("rejected")
  ) {
    return t("Login rejected. Check the username and password.");
  }
  if (
    lower.includes("no such host") ||
    lower.includes("lookup ") ||
    lower.includes("name resolution") ||
    lower.includes("nodename nor servname") ||
    lower.includes("getaddrinfo") ||
    lower.includes("nxdomain") ||
    lower.includes("server misbehaving")
  ) {
    return t("Host not found (DNS lookup failed). Check the hostname.");
  }
  if (lower.includes("connection refused")) {
    return t("Connection refused. Check the host and port.");
  }
  if (
    lower.includes("tls") ||
    lower.includes("certificate") ||
    lower.includes("x509") ||
    lower.includes("handshake")
  ) {
    return t("TLS connection failed. Check the port and TLS setting.");
  }
  if (
    lower.includes("timeout") ||
    lower.includes("deadline exceeded") ||
    lower.includes("timed out")
  ) {
    return t("Connection timed out. Check the host and port.");
  }
  return t("Could not connect to this provider. Check the host and port.");
}

export function usenetHealth(v: unknown, debrid?: unknown, indexers?: unknown): HealthItem {
  const base = {
    id: "usenet" as const,
    label: "Usenet",
    href: "/sources/usenet",
  };
  const err = errorOf(v);
  if (err) return { ...base, level: "error", summary: err, details: [] };
  if (!Array.isArray(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const providers = v.filter(isPlainObject);
  if (providers.length === 0)
    return debridOn(debrid)
      ? { ...base, level: "off", summary: t("Not used · streaming through debrid"), details: [] }
      : toSetUp({
          ...base,
          label: t("Sources"),
          href: "/sources",
          level: "error",
          summary: torrentioReady(indexers)
            ? t("Torrentio finds releases; add a debrid or Usenet account to play.")
            : t("No debrid account or Usenet provider yet"),
          details: [],
        });
  const primary = providers.filter((p) => !p.backfill);
  const bad = providers.filter((p) => str(p.state) && str(p.state) !== "ok");
  const primaryBad = primary.filter((p) => str(p.state) && str(p.state) !== "ok");
  const active = providers.reduce((n, p) => n + num(p.activeConnections), 0);
  const configured = providers.reduce(
    (n, p) => n + num(p.configuredConnections ?? p.maxConnections),
    0,
  );
  const details = providers.map((p) => {
    const problem = usenetProblem(p.state, p.lastError);
    const values = {
      name: str(p.name) || str(p.host) || str(p.id),
      state: problem ?? (str(p.state) || t("unknown")),
      active: num(p.activeConnections),
      max: num(p.configuredConnections ?? p.maxConnections),
    };
    return p.backfill
      ? t("{name}: {state} (backfill) · {active}/{max} connections", values)
      : t("{name}: {state} · {active}/{max} connections", values);
  });
  let level: HealthLevel = "ok";
  if (primary.length > 0 && primaryBad.length === primary.length) level = "error";
  else if (bad.length > 0) level = "warn";
  const firstProblem = bad.length > 0 ? usenetProblem(bad[0]!.state, bad[0]!.lastError) : null;
  const degradedCount = tn(
    providers.length,
    "{bad} of {count} provider degraded",
    "{bad} of {count} providers degraded",
    { bad: bad.length },
  );
  const summary =
    level === "ok"
      ? tn(
          providers.length,
          "{count} provider healthy · {active}/{configured} connections busy",
          "{count} providers healthy · {active}/{configured} connections busy",
          { active, configured },
        )
      : providers.length === 1 && firstProblem
        ? firstProblem
        : firstProblem
          ? `${degradedCount} · ${firstProblem}`
          : degradedCount;
  return {
    ...base,
    level,
    summary,
    details,
    statusLabel: level === "ok" ? undefined : t("Needs attention"),
    fill: configured > 0 ? active / configured : undefined,
  };
}

/**
 * Debrid accounts from the "debrid" status section. Null when no account is
 * configured, so pages show only the sources the owner uses.
 */
export function debridHealth(v: unknown): HealthItem | null {
  const base = { id: "debrid" as const, label: t("Debrid"), href: "/sources/debrid" };
  const err = errorOf(v);
  if (err) return { ...base, level: "error", summary: err, details: [] };
  if (!isPlainObject(v) || !Array.isArray(v.providers)) return null;
  const accounts = v.providers.filter(isPlainObject).filter((p) => str(p.state) !== "disabled");
  if (accounts.length === 0) return null;
  const bad = accounts.filter((p) => str(p.state) !== "ok");
  const details = accounts.map((p) =>
    t("{name}: {state}", {
      name: str(p.name) || str(p.id),
      state: str(p.state) === "ok" ? t("healthy") : str(p.lastError) || str(p.state),
    }),
  );
  if (bad.length === 0)
    return {
      ...base,
      level: "ok",
      summary: tn(accounts.length, "{count} account healthy", "{count} accounts healthy"),
      details,
    };
  return {
    ...base,
    level: bad.length === accounts.length ? "error" : "warn",
    summary:
      accounts.length === 1
        ? str(bad[0]!.lastError) || t("Account needs attention")
        : tn(
            accounts.length,
            "{bad} of {count} account needs attention",
            "{bad} of {count} accounts need attention",
            {
              bad: bad.length,
            },
          ),
    details,
    statusLabel: t("Needs attention"),
  };
}

/**
 * IPTV channel sources (Live TV sources list). Null when none is configured.
 */
export function iptvHealth(
  sources: { name?: string; lastError?: string }[] | null,
): HealthItem | null {
  if (!sources || sources.length === 0) return null;
  const base = { id: "iptv" as const, label: t("IPTV"), href: "/sources/iptv" };
  const bad = sources.filter((s) => !!s.lastError);
  const details = sources.map((s) =>
    t("{name}: {state}", { name: s.name ?? "", state: s.lastError || t("healthy") }),
  );
  if (bad.length === 0)
    return {
      ...base,
      level: "ok",
      summary: tn(sources.length, "{count} source healthy", "{count} sources healthy"),
      details,
    };
  return {
    ...base,
    level: bad.length === sources.length ? "error" : "warn",
    summary: tn(
      sources.length,
      "{bad} of {count} source failing to refresh",
      "{bad} of {count} sources failing to refresh",
      {
        bad: bad.length,
      },
    ),
    details,
    statusLabel: t("Needs attention"),
  };
}

/** A debrid account is enabled (from the "debrid" status section). */
function debridOn(v: unknown): boolean {
  return isPlainObject(v) && v.enabled === true;
}

/**
 * Indexer budgets from the "indexers" status section. With the "debrid"
 * section it also warns when debrid is on but no enabled indexer returns
 * torrents: every play would then fail with nothing to stream.
 */
export function indexersHealth(v: unknown, debrid?: unknown): HealthItem {
  const base = {
    id: "indexers" as const,
    label: t("Indexers"),
    href: "/sources/indexers",
  };
  const err = errorOf(v);
  if (err) return { ...base, level: "error", summary: err, details: [] };
  if (!Array.isArray(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const rows = v.filter(isPlainObject);
  if (rows.length === 0)
    return toSetUp({ ...base, level: "error", summary: t("No indexers configured"), details: [] });
  const enabled = rows.filter((r) => r.enabled !== false);
  if (enabled.length === 0)
    return {
      ...base,
      level: "warn",
      summary: t("No indexers enabled"),
      details: [t("Ether has nothing to search until an indexer is enabled.")],
    };
  const noTorrent = debridOn(debrid) && !enabled.some((r) => r.torrent === true);
  let exhausted = 0;
  let remaining = 0;
  let limited = 0;
  let used = 0;
  let limit = 0;
  const details = rows.map((r) => {
    const hl = num(r.hitsLimit);
    const hr = num(r.hitsRemaining);
    if (hl > 0) {
      limited++;
      remaining += hr;
      used += num(r.hitsUsed);
      limit += hl;
      if (hr <= 0) exhausted++;
    }
    const parts = [
      hl > 0 ? t("{left}/{limit} searches left", { left: hr, limit: hl }) : t("unlimited searches"),
    ];
    if (num(r.grabsLimit) > 0)
      parts.push(
        t("{left}/{limit} grabs left", { left: num(r.grabsRemaining), limit: num(r.grabsLimit) }),
      );
    return `${str(r.indexer) || str(r.name)}: ${parts.join(" · ")}`;
  });
  let level: HealthLevel = "ok";
  if (exhausted > 0 && exhausted === rows.length) level = "error";
  else if (exhausted > 0) level = "warn";
  let summary =
    limited === 0
      ? tn(rows.length, "{count} indexer · no daily limits", "{count} indexers · no daily limits")
      : exhausted > 0
        ? tn(
            rows.length,
            "{exhausted} of {count} indexer out of searches today",
            "{exhausted} of {count} indexers out of searches today",
            { exhausted },
          )
        : tn(
            rows.length,
            "{count} indexer · {remaining} searches left today",
            "{count} indexers · {remaining} searches left today",
            { remaining },
          );
  if (noTorrent) {
    details.unshift(
      t(
        "Debrid streams torrents, but no enabled indexer returns them. Add a Stremio addon or Torznab indexer.",
      ),
    );
    if (level !== "error") {
      level = "warn";
      summary = t("No torrent indexer for debrid");
    }
  }
  return { ...base, level, summary, details, fill: limit > 0 ? used / limit : undefined };
}

export function cacheHealth(v: unknown): HealthItem {
  const base = { id: "cache" as const, label: t("Cache"), href: "/cache" };
  if (!isPlainObject(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const used = num(v.used);
  const budget = num(v.budget);
  if (v.mode === "off")
    return { ...base, level: "off", summary: t("Persistent media bypassed"), details: [] };
  if (budget <= 0)
    return {
      ...base,
      level: "ok",
      summary: t("{used} · unlimited budget", { used: formatBytes(used) }),
      details: [],
    };
  const fill = Math.min(1, used / budget);
  return {
    ...base,
    level: "ok",
    fill,
    summary: t("{used} of {budget}", { used: formatBytes(used), budget: formatBytes(budget) }),
    details: [t("Older segments are evicted automatically when the budget is reached.")],
  };
}

function yearCorrectionDetail(v: unknown): string {
  if (!isPlainObject(v)) return "";
  const pending = num(v.pending);
  const done = num(v.stored) + num(v.refreshed);
  if (v.running === true || pending > 0) {
    return t("Correcting movie years · {done} done · {pending} left", {
      done: formatNumber(done),
      pending: formatNumber(pending),
    });
  }
  return "";
}

/** The library-tier migration's progress (catalog status tierMigration). */
export type TierMigration = {
  state?: string;
  paused?: boolean;
  remove?: number;
  removed?: number;
  finishedAt?: string;
  lastError?: string;
};

/**
 * One line for the tier migration, or "" when there is nothing to show
 * (never armed, or finished more than a week ago).
 */
export function tierMigrationLine(v: unknown, now = new Date()): string {
  if (!isPlainObject(v)) return "";
  const m = v as TierMigration;
  const state = typeof m.state === "string" ? m.state : "";
  if (!state) return "";
  const done = num(m.removed);
  if (state === "done") {
    const at = parseTime(m.finishedAt);
    if (!at || now.getTime() - at.getTime() > 7 * 86400000) return "";
    return t("Library tidied: {count} long-tail titles moved to search-only", {
      count: formatNumber(done),
    });
  }
  if (state === "waiting_backup") {
    return t("Waiting for a backup before tidying the library: {error}", {
      error: typeof m.lastError === "string" ? m.lastError : "",
    });
  }
  if (state === "error" && typeof m.lastError === "string" && m.lastError) {
    return t("Tidying the library stopped: {error}", { error: m.lastError });
  }
  const line = t("Tidying the library: {done} of {total} long-tail titles moved to search-only", {
    done: formatNumber(done),
    total: formatNumber(Math.max(done, num(m.remove))),
  });
  return m.paused ? `${line} · ${t("Paused")}` : line;
}

export function catalogHealth(v: unknown, now = new Date()): HealthItem {
  const base = {
    id: "catalog" as const,
    label: t("Catalog"),
    href: "/catalog",
  };
  const err = errorOf(v);
  if (err) return { ...base, level: "error", summary: err, details: [] };
  if (!isPlainObject(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const movies = num(v.browsableMovies ?? v.movies);
  const shows = num(v.browsableShows ?? v.shows);
  const sync = isPlainObject(v.sync) ? v.sync : {};
  const last = parseTime(v.lastSync);
  const details = [
    t("{movies} movies · {shows} shows browsable", {
      movies: formatNumber(movies),
      shows: formatNumber(shows),
    }),
    t("Last sync {when}", { when: relativeTime(last, now) }),
  ];
  const yearLine = yearCorrectionDetail(v.yearCorrection);
  if (yearLine) details.push(yearLine);
  const tierLine = tierMigrationLine(v.tierMigration, now);
  if (tierLine) details.push(tierLine);
  if (typeof sync.lastError === "string" && sync.lastError) {
    const message = t("Last sync error: {error}", { error: sync.lastError });
    details.push(message);
    return { ...base, level: "warn", summary: message, details };
  }
  if (sync.running === true) {
    const total = movies + shows;
    const checked = formatNumber(num(sync.titlesSeen));
    return {
      ...base,
      level: "ok",
      summary:
        total > 0
          ? t("{count} titles · syncing ({checked} checked)", {
              count: formatNumber(total),
              checked,
            })
          : t("Syncing… {checked} checked", { checked }),
      details,
    };
  }
  if (!last) return toSetUp({ ...base, level: "warn", summary: t("Not synced yet"), details });
  const ageDays = (now.getTime() - last.getTime()) / 86400000;
  const level: HealthLevel = ageDays > 3 ? "warn" : "ok";
  return {
    ...base,
    level,
    summary: t("{count} titles · synced {when}", {
      count: formatNumber(movies + shows),
      when: relativeTime(last, now),
    }),
    details,
  };
}

export function remoteHealth(v: unknown): HealthItem {
  const base = {
    id: "remote" as const,
    label: t("Remote access"),
    href: "/apps/plex/remote",
  };
  if (!isPlainObject(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  if (v.claimed === false)
    return {
      ...base,
      href: "/apps/plex/account",
      level: "error",
      summary: t("Server is not claimed"),
      details: [t("Sign in and claim this server on the Plex page.")],
    };
  const presence = isPlainObject(v.presence) ? v.presence : {};
  const details: string[] = [];
  if (str(v.ownerUsername)) details.push(t("Claimed by {name}", { name: str(v.ownerUsername) }));
  if (str(v.publicAddress))
    details.push(t("Public address {address}", { address: str(v.publicAddress) }));
  details.push(presence.connected ? t("Connected to plex.tv") : t("Not connected to plex.tv"));
  if (v.listed === false) details.push(t("Not yet published to plex.tv"));
  if (v.tokenRejected === true)
    return {
      ...base,
      level: "error",
      summary: t("plex.tv rejects this server's sign-in"),
      details: [
        ...details,
        t(
          "If this server was claimed by another Plex account before, remove it from that account under Authorized Devices on plex.tv, then sign out and claim it again here.",
        ),
      ],
    };
  if (str(v.lastPublishError))
    return {
      ...base,
      level: "warn",
      summary: t("Publishing to plex.tv is failing"),
      details: [...details, str(v.lastPublishError)],
    };
  if (str(v.friendSyncError))
    return {
      ...base,
      level: "warn",
      summary: t("Shared users are not syncing"),
      details: [...details, str(v.friendSyncError)],
    };
  if (presence.connected === false)
    return { ...base, level: "warn", summary: t("plex.tv connection lost"), details };
  // remoteAccess is the saved setting (registrar refreshConfigStatus), not a
  // reachability result. plexReachable is only plex.tv's last notifyConnectivity
  // verdict and is not tied to a particular published route.
  const customPublished = publishedCustomURIs(v);
  for (const address of customPublished) {
    details.push(t("Published custom connection {address}", { address }));
  }
  const publicationNote = t(
    "A published address is not proof that clients outside your network can connect.",
  );
  if (customPublished.length > 0) details.push(publicationNote);
  const lastCheck =
    v.plexReachable === true
      ? t("plex.tv's last connectivity check reported the server reachable.")
      : v.plexReachable === false
        ? t("plex.tv's last connectivity check reported the server unreachable.")
        : "";
  if (v.remoteClientReachable === true) {
    if (lastCheck) details.push(lastCheck);
    return {
      ...base,
      level: "ok",
      summary: t("Reachable away from home."),
      details,
      statusLabel: t("Reachable"),
    };
  }
  // plex.tv's verdict is not tied to the published proxy URL. Keep it as
  // diagnostic detail rather than declaring every custom route broken.
  if (customPublished.length > 0) {
    if (lastCheck) details.push(lastCheck);
    return {
      ...base,
      level: "off",
      summary: t("Custom connection published"),
      details,
      statusLabel: t("Published"),
      note: publicationNote,
    };
  }
  if (v.remoteAccess === true) {
    const upnp = isPlainObject(v.upnp) ? v.upnp : {};
    if (upnp.active === true) details.push(t("Port mapped with UPnP"));
    const clientSeen = parseTime(v.remoteClientLastSeen);
    const plexChecked = parseTime(v.plexReachableCheckedAt);
    const superseded = clientSeen && (!plexChecked || clientSeen > plexChecked);
    if (v.plexReachable === false && !superseded) {
      return {
        ...base,
        level: "warn",
        summary: lastCheck,
        details: [t("Direct remote access enabled"), ...details],
      };
    }
    if (lastCheck) details.push(lastCheck);
    return {
      ...base,
      level: "ok",
      summary: t("Direct remote access enabled"),
      details,
      statusLabel: t("Configured"),
    };
  }
  if (lastCheck) details.push(lastCheck);
  return { ...base, level: "off", summary: t("Local network only"), details };
}

/** Custom routes plex.tv was actually given: Connection.Custom whose URI was published. */
function publishedCustomURIs(v: Record<string, unknown>): string[] {
  if (!Array.isArray(v.publishedUris) || !Array.isArray(v.connections)) return [];
  const published = new Set(
    v.publishedUris.filter((uri): uri is string => typeof uri === "string" && uri !== ""),
  );
  const out: string[] = [];
  for (const connection of v.connections) {
    if (!isPlainObject(connection) || connection.custom !== true) continue;
    const uri = typeof connection.uri === "string" ? connection.uri : "";
    if (uri && published.has(uri) && !out.includes(uri)) out.push(uri);
  }
  return out;
}

export function plexDirectHealth(v: unknown, now = new Date()): HealthItem {
  const base = {
    id: "plexdirect" as const,
    label: "plex.direct",
    href: "/apps/plex/remote",
  };
  if (!isPlainObject(v))
    return { ...base, level: "unknown", summary: t("Not reported"), details: [] };
  const cert = isPlainObject(v.cert) ? v.cert : null;
  if (!cert || cert.present !== true) {
    return {
      ...base,
      level: "warn",
      summary: t("No certificate yet"),
      details: [
        t("Secure connections fall back to plain HTTP until a plex.direct certificate is issued."),
      ],
    };
  }
  const starts = parseTime(cert.notBefore);
  const expires = parseTime(cert.notAfter);
  const details = [str(cert.subject)].filter(Boolean);
  if (cert.custom === true) details.push(t("Custom certificate"));
  if (str(v.certError)) {
    details.push(str(v.certError));
    return { ...base, level: "warn", summary: t("Certificate check failed"), details };
  }
  if (cert.reissueRequired === true) {
    return { ...base, level: "warn", summary: t("Certificate reissue pending"), details };
  }
  if (cert.selfSigned === true) {
    details.push(t("Plex apps do not normally trust a self-signed certificate."));
    return { ...base, level: "warn", summary: t("Self-signed certificate"), details };
  }
  if (expires) details.push(t("Expires {date}", { date: formatDate(expires) }));
  if (expires && (expires.getTime() - now.getTime()) / 86400000 < 0)
    return { ...base, level: "error", summary: t("Certificate expired"), details };
  if (starts && starts.getTime() > now.getTime())
    return { ...base, level: "warn", summary: t("Certificate is not valid yet"), details };
  const trusted = t("Trusted by Plex apps");
  if (expires && (expires.getTime() - now.getTime()) / 86400000 < 14) {
    if (cert.verified === true) details.push(trusted);
    return {
      ...base,
      level: "warn",
      summary: t("Certificate renews {when}", { when: relativeTime(expires, now) }),
      details,
    };
  }
  if (!expires || !starts) {
    details.push(t("Both a start and an expiry have to be reported."));
    return { ...base, level: "unknown", summary: t("Certificate dates are incomplete"), details };
  }
  if (cert.verified === true) {
    details.push(trusted);
    return {
      ...base,
      level: "ok",
      summary: t("Expires {date}", { date: formatDate(expires) }),
      details,
      statusLabel: t("Valid"),
      note: trusted,
    };
  }
  const reason = str(cert.verifyReason);
  if (reason === "chain" || reason === "identity" || reason === "handshake") {
    return certificateTrustProblem(base, reason, details);
  }
  return {
    ...base,
    level: "unknown",
    summary: t("Expires {date}", { date: formatDate(expires) }),
    details,
    statusLabel: t("Checking certificate"),
    countsOverall: false,
    note: t("Ether checks this certificate on a schedule, not on every refresh."),
  };
}

function certificateTrustProblem(
  base: { id: "plexdirect"; label: string; href: string },
  reason: string,
  details: string[],
): HealthItem {
  const summary =
    reason === "identity"
      ? t("Doesn't match this server")
      : reason === "handshake"
        ? t("This server did not accept a secure connection")
        : t("Chain not trusted");
  const hint =
    reason === "identity"
      ? t(
          "The certificate names a different server. Let Ether request a new plex.direct certificate.",
        )
      : reason === "handshake"
        ? t("Check that Ether is running, then look again after the next certificate check.")
        : t("Let Ether request a new plex.direct certificate. Plex apps trust that one.");
  return {
    ...base,
    level: "warn",
    summary,
    details: [...details, hint],
    statusLabel: summary,
    note: hint,
  };
}

/** Shown only when this ffmpeg cannot convert or tone-map, or a playback just failed for that reason. */
export function transcodeHealth(v: unknown): HealthItem | null {
  if (!isPlainObject(v)) return null;
  const playback = str(v.playbackError);
  const notice = str(v.notice);
  const text = playback || notice;
  if (!text) return null;
  const details = playback && notice && playback !== notice ? [notice] : [];
  return {
    id: "transcode",
    label: t("Transcoding"),
    href: "/playback/transcoding",
    level: "warn",
    summary: text,
    details,
  };
}

/**
 * The database from the "database" status section: a SQLite file's
 * integrity (checked after start and daily) or, on a cluster, the shared
 * database's reachability. Damage links to Backups to restore. Null until
 * there is something to report.
 */
export function databaseHealth(v: unknown, now = new Date()): HealthItem | null {
  const base = { id: "database" as const, label: t("Database"), href: "/maintenance/backups" };
  if (!isPlainObject(v)) return null;
  if (v.clustered === true) {
    const held = num(v.held);
    if (v.state === "degraded")
      return {
        ...base,
        href: "/cluster",
        level: "error",
        summary: t("Shared database unreachable"),
        details: [str(v.message), str(v.lastError)].filter(Boolean),
      };
    if (v.state === "recovering")
      return {
        ...base,
        href: "/cluster",
        level: "warn",
        summary: tn(held, "Writing {count} held update", "Writing {count} held updates"),
        details: [],
      };
    return {
      ...base,
      href: "/cluster",
      level: v.state === "ok" && v.reachable === true ? "ok" : "unknown",
      summary:
        v.state === "ok" && v.reachable === true
          ? t("Shared PostgreSQL database reachable")
          : t("Not reported"),
      details: [],
    };
  }
  // The first check runs a couple of minutes after start; until then there
  // is nothing to report (and nothing to hold the overall status back).
  const integrity = isPlainObject(v.integrity) ? v.integrity : null;
  if (!integrity) return null;
  const checked = parseTime(integrity.checked);
  if (integrity.skipped === true || !checked || typeof integrity.ok !== "boolean")
    return { ...base, level: "unknown", summary: t("Not checked"), details: [] };
  if (integrity.ok === true)
    return {
      ...base,
      level: "ok",
      summary: t("No damage found · checked {when}", { when: relativeTime(checked, now) }),
      details: [],
    };
  const findings = Array.isArray(integrity.findings)
    ? integrity.findings.filter((f): f is string => typeof f === "string").slice(0, 3)
    : [];
  return {
    ...base,
    level: "error",
    summary: t("The last database check found an issue"),
    details: findings,
  };
}

/**
 * The data folder's filesystem from the "storage" status section (#305).
 * Only safe mode is a row: a filesystem Ether trusts has nothing to say.
 */
export function storageHealth(v: unknown): HealthItem | null {
  if (!isPlainObject(v) || v.trust !== "safe-mode") return null;
  const type = str(v.type);
  const path = str(v.path);
  const why =
    v.reasonKind === "network"
      ? t("It is a network share, so Ether can't tell when a crashed run has let go of its files.")
      : t(
          "It failed Ether's startup check, so Ether can't tell when a crashed run has let go of its files.",
        );
  const fixes: Record<string, string> = {
    unraid: t(
      "Map /config to the pool or disk path, for example /mnt/cache/appdata/ether, not /mnt/user.",
    ),
    "docker-desktop": t(
      "Use a Docker named volume for /config instead of a folder from your computer.",
    ),
    network: t("Move the data folder to a disk in this machine."),
    fuse: t("Move the data folder to a disk in this machine."),
  };
  return {
    id: "storage",
    label: t("Data folder"),
    href: "/cache",
    level: "warn",
    summary: t("Cache cleanup is in safe mode: {path} is on {type}", { path, type }),
    note:
      fixes[str(v.fixKind)] ??
      t("Move the data folder to a local disk (ext4, XFS, Btrfs, ZFS, f2fs, bcachefs or NTFS)."),
    details: [
      why,
      t(
        "Files left by a crash are kept for 10 minutes before they are cleaned up. Everything else works normally.",
      ),
      str(v.reason),
    ].filter(Boolean),
  };
}

/** One Plex row until the server is claimed. The certificate check is the same missing sign-in. */
function plexConnectionHealth(remote: unknown, now: Date): HealthItem[] {
  if (isPlainObject(remote) && remote.claimed === false) {
    return [
      toSetUp({
        id: "remote",
        label: t("Plex"),
        href: "/apps/plex/account",
        level: "error",
        summary: t("Sign in to finish connecting"),
        details: [t("Plex is on, and this server is not claimed yet.")],
      }),
    ];
  }
  return [remoteHealth(remote), plexDirectHealth(remote, now)];
}

export function computeHealth(
  sections: Record<string, unknown> | undefined,
  now = new Date(),
  opts?: { plexEnabled?: boolean },
): HealthItem[] {
  const s = sections ?? {};
  const plexOn = opts?.plexEnabled !== false;
  const items: HealthItem[] = [
    usenetHealth(s.providers, s.debrid, s.indexers),
    indexersHealth(s.indexers, s.debrid),
    cacheHealth(s.cache),
    catalogHealth(s.catalog, now),
    databaseHealth(s.database, now),
    storageHealth(s.storage),
    ...(plexOn ? plexConnectionHealth(s.remote, now) : []),
  ].filter((item): item is HealthItem => item !== null);
  const transcode = transcodeHealth(s.hwaccel);
  if (transcode) items.push(transcode);
  return items;
}

// ---- Now playing --------------------------------------------------------------

export interface NowPlaying {
  node?: string;
  processor?: string;
  cluster?: {
    source: string;
    encoder: string;
    delivery: string;
    recovery?: string;
    recovering: boolean;
  };
  protocol: string;
  id: string;
  key: string;
  title: string;
  isLive: boolean;
  channelId: string;
  channelName: string;
  channelNumber: string;
  user: string;
  device: string;
  product: string;
  decision: string;
  decisionReason: string;
  qualityFallback?: string;
  /** Whether the current decision copies each stream; a remux with audioCopy false is transcoding only the audio. */
  videoCopy: boolean;
  audioCopy: boolean;
  remote: boolean;
  bytesServed: number;
  size: number;
  /** 0..1 of the file downloaded ahead of the reader. */
  buffered: number;
  speedBps: number;
  startedAt: Date | null;
  durationMs: number;
  /** 0..1 when the server reports a playback position, else null. */
  progress: number | null;
  release: string;
  /** Where the release streams from: "Usenet" or a debrid service such as "Real-Debrid". */
  origin: string;
  failovers: number;
  /** The client's address, when the server reports it. */
  ip: string;
  /** Delivered bitrate now (or the encode's target), kbps; 0 when unknown. */
  bitrateKbps: number;
  /** Short human descriptions, e.g. "HEVC 2160p HDR10" → "H.264 1080p SDR". */
  sourceVideo: string;
  outputVideo: string;
  sourceAudio: string;
  outputAudio: string;
  container: string;
  /** Catalogue key for artwork, when known. */
  ratingKey: number;
  /** Selected subtitle, e.g. "English SRT" or "English PGS (burned)". */
  subtitle: string;
}

function pick(o: Record<string, unknown>, ...keys: string[]): unknown {
  for (const k of keys) if (o[k] !== undefined && o[k] !== null) return o[k];
  return undefined;
}

/** Sessions come from playback.Sessions() (Go field names, no JSON tags); accept camelCase too. */
export function normalizeSessions(v: unknown): NowPlaying[] {
  if (!Array.isArray(v)) return [];
  return v.filter(isPlainObject).map((s) => {
    const info = (pick(s, "Info", "info") ?? {}) as Record<string, unknown>;
    const client = (pick(s, "Client", "client") ?? {}) as Record<string, unknown>;
    const channel = pick(s, "liveChannel", "LiveChannel");
    const liveChannel = isPlainObject(channel) ? channel : null;
    const resolution = (pick(s, "Resolution", "resolution") ?? {}) as Record<string, unknown>;
    const release = isPlainObject(resolution.release) ? resolution.release : {};
    const durationNs = num(pick(info, "duration", "Duration"));
    const offsetMs = pick(s, "viewOffset", "ViewOffset", "positionMs", "PositionMs");
    const durationMs = durationNs / 1e6;
    const explicit = pick(s, "progress", "Progress");
    let progress: number | null = null;
    if (typeof explicit === "number" && Number.isFinite(explicit))
      progress = Math.max(0, Math.min(1, explicit));
    else if (typeof offsetMs === "number" && Number.isFinite(offsetMs) && durationMs > 0)
      progress = Math.max(0, Math.min(1, offsetMs / durationMs));
    const size = num(pick(info, "size", "Size"));
    const bufferedBytes = num(pick(s, "BufferedBytes", "bufferedBytes"));
    const bufferedUntilNs = num(pick(s, "BufferedUntil", "bufferedUntil"));
    let buffered = 0;
    if (bufferedUntilNs > 0 && durationMs > 0)
      buffered = Math.min(1, bufferedUntilNs / 1e6 / durationMs);
    else if (size > 0 && bufferedBytes > 0)
      buffered = Math.min(1, (progress ?? 0) + bufferedBytes / size);
    return {
      node: str(s.node),
      processor: isPlainObject(s.encoder) ? str(s.encoder.processor) : undefined,
      protocol: str(pick(client, "protocol", "Protocol")) || "plex",
      id: str(pick(s, "ID", "id")),
      key: String(pick(s, "Key", "key") ?? ""),
      title: str(pick(s, "TitleName", "titleName", "title")) || t("Untitled"),
      isLive: liveChannel !== null,
      channelId: liveChannel ? str(pick(liveChannel, "id", "ID")).trim() : "",
      channelName: liveChannel ? str(pick(liveChannel, "name", "Name")).trim() : "",
      channelNumber: liveChannel ? str(pick(liveChannel, "number", "Number")).trim() : "",
      user: str(pick(s, "Username", "username")) || t("Someone"),
      device:
        str(pick(client, "deviceName", "device")) ||
        str(pick(client, "product")) ||
        t("Unknown device"),
      product: str(pick(client, "product")),
      decision: str(pick(s, "Decision", "decision")),
      decisionReason: str(pick(s, "decisionReason", "DecisionReason")),
      qualityFallback: str(pick(s, "qualityFallback", "QualityFallback")),
      videoCopy: pick(s, "decisionVideoCopy", "DecisionVideoCopy") === true,
      audioCopy: pick(s, "decisionAudioCopy", "DecisionAudioCopy") === true,
      remote: pick(client, "remote") === true,
      bytesServed: num(pick(s, "BytesServed", "bytesServed")),
      size,
      buffered,
      speedBps: num(pick(s, "SpeedBps", "speedBps")),
      startedAt: parseTime(pick(s, "StartedAt", "startedAt")),
      durationMs,
      progress,
      release: str(release.name),
      origin: str(pick(s, "origin", "Origin")),
      failovers: num(pick(s, "Failovers", "failovers")),
      ip: str(pick(s, "ip", "IP", "Ip")) || str(pick(client, "ip", "address")),
      bitrateKbps: num(pick(s, "bitrateKbps", "BitrateKbps")),
      sourceVideo: str(pick(s, "sourceVideo", "SourceVideo")),
      outputVideo: str(pick(s, "outputVideo", "OutputVideo")),
      sourceAudio: str(pick(s, "sourceAudio", "SourceAudio")),
      outputAudio: str(pick(s, "outputAudio", "OutputAudio")),
      container: str(pick(s, "container", "Container")),
      ratingKey: num(pick(s, "ratingKey", "RatingKey")),
      subtitle: str(pick(s, "subtitle", "Subtitle")),
    };
  });
}

/** Selected channel, independent of the programme title. */
export function channelLabel(s: Pick<NowPlaying, "channelName" | "channelNumber">): string {
  return [s.channelNumber, s.channelName].filter(Boolean).join(" · ");
}

/**
 * Chip label for a now-playing decision. The server reports the delivery
 * that is actually running: "directplay" (original file), "copy" (remux /
 * direct stream) or "transcode". A remux that copies the video but
 * re-encodes the audio is transcoding only the audio. Anything else,
 * including no decision yet, is still starting.
 */
export function decisionLabel(s: Pick<NowPlaying, "decision" | "videoCopy" | "audioCopy">): string {
  const audioOnly = s.videoCopy && !s.audioCopy;
  switch (s.decision) {
    case "directplay":
      return t("Direct play");
    case "copy":
      return audioOnly ? t("Transcoding (audio)") : t("Direct stream");
    case "transcode":
      return audioOnly ? t("Transcoding (audio)") : t("Transcoding");
    default:
      return t("Starting");
  }
}
