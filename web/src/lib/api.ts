import type {
  ActivityDay,
  CollectionPreview,
  CollectionSpec,
  CollectionTemplate,
  CollectionTemplateBundle,
  CollectionTemplateBundleApplyResult,
  CollectionTemplateBundlePreview,
  FacetName,
  FacetValue,
  HomeLayout,
  LayoutDoc,
  LayoutImpactResult,
  PreviewContext,
  LookupKind,
  LookupResult,
  RankerInsight,
  RowInsight,
  TasteOverride,
  TasteProfile,
  TitleCard,
  TitleDetail,
  Viewer,
  ViewerDetail,
  ViewerHome,
} from "./contract";
import type {
  Bootstrap,
  ClusterSetupRequest,
  ClusterSetupStatus,
  ClusterSettings,
  ClusterStatus,
  ClusterControl,
  ClusterRolePreflight,
  ClusterControlRequest,
  CreatedJoinToken,
  DashboardData,
  DebridProvider,
  DebridStatus,
  DiskInfo,
  EtherConfig,
  EtherUser,
  FieldError,
  CatalogEntry,
  IdentityPreset,
  Indexer,
  LLMModel,
  LLMProvider,
  LogEntry,
  QualityAnswer,
  QualityClient,
  QualityQuestion,
  SiteDefinition,
  SiteTestResult,
  SiteTestStatus,
  SiteUpdateReport,
  SetupStatus,
  SourceStartup,
  SystemLoad,
  TestResult,
  UpdateStatus,
  UsenetProvider,
} from "./types";
import { t } from "../i18n";

export interface SupportDevice {
  id: string;
  name?: string;
  model?: string;
  product?: string;
  version?: string;
  lastSeen: string;
}

export interface SupportCapture {
  id: string;
  state: string;
  deviceConfirmed: boolean;
  startedAt: string;
  deadline: string;
  finishedAt?: string;
  category: string;
  note?: string;
  device?: SupportDevice;
  discovery?: SupportDevice[];
  observations: { at: string; kind: string; count?: number; status?: number; route?: string }[];
  hypotheses: { code: string; text: string }[];
  coverage: { collector: string; status: string; detail: string }[];
  preview?: {
    files?: string[];
    identity: {
      usernamesIncluded: boolean;
      addressesIncluded: boolean;
      usernames?: string[];
      addresses?: string[];
      titlesIncluded: boolean;
      explanation: string;
    };
    note?: string;
    redaction: string;
  };
}

export interface SupportStart {
  deviceId?: string;
  category?: string;
  note?: string;
}

export interface SupportFinish {
  includeTitles: boolean;
  includeUsernames: boolean;
  includeAddresses: boolean;
}

// The console keeps an Ether-issued session that only opens this API and can
// be revoked, never the owner's plex.tv account token.
const TOKEN_KEY = "ether-admin-session";
/** The header the console authenticates with. */
export const SESSION_HEADER = "X-Ether-Admin-Token";

// Earlier consoles stored the plex.tv account token under this key; drop it so
// a browser never keeps that credential at rest.
try {
  localStorage.removeItem("ether-admin-token");
} catch {
  // Storage is unavailable; there is nothing to remove.
}

// A browser may allow reads while rejecting writes (quota/privacy settings).
// Keep the current tab's explicit sign-in/sign-out authoritative in that case.
let sessionToken = "";
let sessionOnly = false;

export function getToken(): string {
  if (sessionOnly) return sessionToken;
  try {
    sessionToken = localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    // The current tab can still authenticate without persistent storage.
  }
  return sessionToken;
}
export function setToken(token: string): void {
  sessionToken = token;
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
    sessionOnly = false;
  } catch {
    sessionOnly = true;
  }
}

export class ApiError extends Error {
  status: number;
  code: string;
  fields?: FieldError[];
  phase?: string;
  constructor(status: number, code: string, message: string, fields?: FieldError[]) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.fields = fields;
  }
}

export function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}

export const API_BASE = "/ether/api";

/** True for URLs on this console's own API, the only place the session goes. */
export function isConsoleApiUrl(url: string): boolean {
  try {
    const u = new URL(url, window.location.href);
    return u.origin === window.location.origin && u.pathname.startsWith(API_BASE + "/");
  } catch {
    return false;
  }
}

/** hls.js xhrSetup: sends the session to this console's API and nowhere else. */
export function sessionXhrSetup(xhr: XMLHttpRequest, url: string): void {
  const token = getToken();
  if (token && isConsoleApiUrl(url)) xhr.setRequestHeader(SESSION_HEADER, token);
}

function authHeaders(init: RequestInit, withSession = true): Headers {
  const headers = new Headers(init.headers);
  const token = getToken();
  if (token && withSession) headers.set(SESSION_HEADER, token);
  const method = (init.method ?? "GET").toUpperCase();
  if (method !== "GET" && method !== "HEAD") {
    headers.set("X-Ether-Request", "1");
    if (init.body != null && !(init.body instanceof FormData) && !headers.has("Content-Type"))
      headers.set("Content-Type", "application/json");
  }
  return headers;
}

export async function apiFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
  let res: Response;
  try {
    res = await fetch(API_BASE + path, { ...init, headers: authHeaders(init) });
  } catch (e) {
    if (isAbort(e)) throw e;
    throw new ApiError(
      0,
      "network",
      t("Could not reach the Ether server: {error}", { error: (e as Error).message }),
    );
  }
  const text = await res.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const err = (
      data as {
        error?: { code?: string; message?: string; fields?: FieldError[]; phase?: string };
      } | null
    )?.error;
    const error = new ApiError(
      res.status,
      err?.code ?? "http",
      err?.message ?? t("Request failed ({status})", { status: res.status }),
      err?.fields,
    );
    error.phase = err?.phase;
    throw error;
  }
  return data as T;
}

/**
 * Fetch a streaming console API response (Ask Ether turns) with the same
 * session and CSRF headers as apiFetch. The caller owns the Response body.
 */
export async function apiStream(path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(API_BASE + path, { ...init, headers: authHeaders(init) });
}

let routeImagesThroughFetch = false;

/** Load /ether/api images through fetch() even without a token (mock mode). */
export function routeApiImagesThroughFetch(on: boolean): void {
  routeImagesThroughFetch = on;
}

/** True when an image URL must be fetched with headers rather than put in an <img>. */
export function imageNeedsFetch(src: string | null | undefined): boolean {
  return Boolean(src && src.startsWith(API_BASE) && (routeImagesThroughFetch || getToken()));
}

/** Fetch an authenticated console image, keeping the response headers. */
export async function apiImage(url: string, signal?: AbortSignal): Promise<Response> {
  const res = await fetch(url, { headers: authHeaders({}, isConsoleApiUrl(url)), signal });
  if (!res.ok)
    throw new ApiError(
      res.status,
      "http",
      t("Image request failed ({status})", { status: res.status }),
    );
  return res;
}

/** Fetch an authenticated binary resource (artwork) as a Blob. */
export async function apiBlob(url: string, signal?: AbortSignal): Promise<Blob> {
  return (await apiImage(url, signal)).blob();
}

function send<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  return apiFetch<T>(path, {
    method,
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });
}
const post = <T>(path: string, body?: unknown, signal?: AbortSignal) =>
  send<T>("POST", path, body ?? {}, signal);
const put = <T>(path: string, body: unknown) => send<T>("PUT", path, body);

function qs(params: Record<string, string | number | undefined | null>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params))
    if (v !== undefined && v !== null && v !== "") q.set(k, String(v));
  const s = q.toString();
  return s ? `?${s}` : "";
}

export type TestFn = () => Promise<TestResult>;

export const api = {
  // ---- Bootstrap, sign-in, setup -----------------------------------------
  bootstrap: () => apiFetch<Bootstrap>("/bootstrap"),
  localSignIn: (username: string, password: string) =>
    post<{ token: string }>("/auth/local", { username, password }),
  localSetup: (username: string, password: string) =>
    post<{ token: string }>("/setup/local", { username, password }),
  signInStart: () => post<{ handle: string; code: string; authUrl: string }>("/auth/pin"),
  signInPoll: (handle: string) =>
    apiFetch<{ done: boolean; token?: string }>(`/auth/pin/${encodeURIComponent(handle)}`),
  /** Exchanges a pasted owner Plex token for a console session. */
  createSession: (plexToken: string) =>
    apiFetch<{ done: boolean; token: string }>("/auth/session", {
      method: "POST",
      headers: { "X-Plex-Token": plexToken },
    }),
  /** Ends this console's session on the server. */
  signOut: () => apiFetch<{ ok: boolean }>("/auth/session", { method: "DELETE" }),
  setupStatus: () => apiFetch<SetupStatus>("/setup/status"),
  setupComplete: (force = false) => post<SetupStatus>("/setup/complete", { force }),
  setupReopen: () => post<SetupStatus>("/setup/reopen"),
  pinStart: () => post<{ id: string; code: string; authUrl: string }>("/plex/pin"),
  pinPoll: (id: string) => apiFetch<{ done: boolean }>(`/plex/pin/${encodeURIComponent(id)}`),
  claim: () => post<{ ok: boolean }>("/plex/claim"),
  syncPlexFriends: () => post<{ ok: boolean }>("/plex/friends/sync"),

  // ---- Config ---------------------------------------------------------------
  getConfig: async () => (await apiFetch<{ config: EtherConfig }>("/config")).config,
  putConfig: async (config: EtherConfig) =>
    (await put<{ config: EtherConfig }>("/config", config)).config,
  getSection: <T>(section: string) =>
    apiFetch<{ section: string; value: T; presets?: IdentityPreset[] }>(`/config/${section}`),
  putSection: (section: string, value: unknown) =>
    put<{ section: string; value: unknown }>(`/config/${section}`, value),

  // ---- Quality: who gets what -------------------------------------------------
  qualityClients: async () =>
    (await apiFetch<{ clients: QualityClient[] }>("/quality/clients")).clients,
  explainQuality: (question: QualityQuestion, signal?: AbortSignal) =>
    post<QualityAnswer>("/quality/explain", question, signal),

  // ---- Integration tests ----------------------------------------------------
  testUsenet: (p: UsenetProvider) => post<TestResult>("/test/usenet", p),
  testIndexer: (ix: Indexer) => post<TestResult>("/test/indexer", ix),
  testDebrid: (p: DebridProvider) => post<TestResult>("/test/debrid", p),
  testTMDB: (key: string) => post<TestResult>("/test/tmdb", { key }),
  testIntroDB: (apiKey: string) => post<TestResult>("/test/introdb", { apiKey }),
  testOpenSubtitles: (cfg: EtherConfig["openSubtitles"]) =>
    post<TestResult>("/test/opensubtitles", cfg),
  testLLM: (cfg: EtherConfig["llm"]) => post<TestResult>("/test/llm", cfg),
  llmProviders: async () =>
    (await apiFetch<{ providers: LLMProvider[] }>("/llm/providers")).providers,
  /** Models for unsaved settings; a provider failure comes back as `error`. */
  llmModels: (cfg: EtherConfig["llm"], signal?: AbortSignal) =>
    post<{ models: LLMModel[]; error?: string }>("/llm/models", cfg, signal),
  testTrakt: (cfg: EtherConfig["trakt"]) => post<TestResult>("/test/trakt", cfg),
  checkOutbound: (cfg: EtherConfig["outbound"]) =>
    post<{
      sourceIp?: string;
      sourceError?: string;
      publishedIp?: string;
      publishedError?: string;
      remoteAccess?: boolean;
      proxyEnabled?: boolean;
    }>("/outbound/check", cfg),

  // ---- Built-in indexer sites ------------------------------------------------------
  sites: {
    definitions: async () =>
      (await apiFetch<{ definitions: SiteDefinition[] | null }>("/indexer-sites/definitions"))
        .definitions ?? [],
    upload: async (yaml: string) =>
      (await post<{ definition: SiteDefinition }>("/indexer-sites/definitions", { yaml }))
        .definition,
    remove: (id: string) =>
      apiFetch<{ removed: string }>(`/indexer-sites/definitions/${encodeURIComponent(id)}`, {
        method: "DELETE",
      }),
    catalog: async (refresh = false) =>
      (
        await apiFetch<{ entries: CatalogEntry[] | null }>(
          `/indexer-sites/catalog${refresh ? "?refresh=1" : ""}`,
        )
      ).entries ?? [],
    install: async (id: string) =>
      (await post<{ definition: SiteDefinition }>("/indexer-sites/catalog/install", { id }))
        .definition,
    update: () => post<SiteUpdateReport>("/indexer-sites/update"),
    test: (ix: Indexer) => post<SiteTestResult>("/indexer-sites/test", ix),
    status: async () =>
      (await apiFetch<{ statuses: SiteTestStatus[] | null }>("/indexer-sites/status")).statuses ??
      [],
  },

  // ---- Status ----------------------------------------------------------------
  dashboard: () => apiFetch<DashboardData>("/dashboard"),
  systemLoad: (signal?: AbortSignal) => apiFetch<SystemLoad>("/system/load", { signal }),
  debridStatus: async () =>
    (await apiFetch<{ value: DebridStatus | null }>("/status/debrid")).value,
  sourceStartup: async () =>
    (await apiFetch<{ value: { startup: SourceStartup[] } | null }>("/status/sources")).value
      ?.startup ?? [],
  disk: (path: string) => apiFetch<DiskInfo>(`/disk${qs({ path })}`),
  logs: (params: { limit?: number; level?: string; q?: string }) =>
    apiFetch<{ entries: LogEntry[]; total: number }>(`/logs${qs(params)}`),
  clearLogs: () => apiFetch<{ ok: boolean }>("/logs", { method: "DELETE" }),
  supportDevices: () => apiFetch<{ devices: SupportDevice[] }>("/support/devices"),
  supportStart: (body: SupportStart) => post<SupportCapture>("/support/captures", body),
  supportGet: (id: string) =>
    apiFetch<SupportCapture>(`/support/captures/${encodeURIComponent(id)}`),
  supportSelect: (id: string, deviceId: string) =>
    post<SupportCapture>(`/support/captures/${encodeURIComponent(id)}/device`, { deviceId }),
  supportFinish: (id: string, body: SupportFinish) =>
    post<SupportCapture>(`/support/captures/${encodeURIComponent(id)}/finish`, body),
  supportDiscard: (id: string) =>
    apiFetch<{ ok: boolean }>(`/support/captures/${encodeURIComponent(id)}`, { method: "DELETE" }),
  supportArchive: (id: string) =>
    apiBlob(`${API_BASE}/support/captures/${encodeURIComponent(id)}/archive`),

  // ---- Updates -----------------------------------------------------------------
  checkUpdates: () => post<UpdateStatus>("/updates/check", {}),
  updateStatus: () => apiFetch<UpdateStatus>("/updates/"),
  updateNow: (force = false) => post<UpdateStatus>("/updates/apply", { force }),
  dismissInstalledUpdate: () => post<UpdateStatus>("/updates/dismiss-installed", {}),

  // ---- Cluster ---------------------------------------------------------------------
  clusterSetupStatus: (signal?: AbortSignal) =>
    apiFetch<ClusterSetupStatus>("/cluster/setup", { signal }),
  checkClusterSetup: (request: ClusterSetupRequest) =>
    post<{ ok: boolean }>("/cluster/setup/check", request),
  applyClusterSetup: (request: ClusterSetupRequest) =>
    post<{ state: string }>("/cluster/setup/apply", request),
  updateClusterNode: (body: { weight?: number; draining?: boolean }) =>
    send<{ weight: number; draining: boolean }>("PATCH", "/cluster/node", body),
  clusterRolePreflight: (id: string, boot: string, signal?: AbortSignal) =>
    apiFetch<ClusterRolePreflight>(
      `/cluster/nodes/${encodeURIComponent(id)}/role-preflight?boot=${encodeURIComponent(boot)}`,
      { signal },
    ),
  clusterControl: (id: string, boot: string, signal?: AbortSignal) =>
    apiFetch<ClusterControl>(
      `/cluster/nodes/${encodeURIComponent(id)}/control?boot=${encodeURIComponent(boot)}`,
      { signal },
    ),
  updateClusterControl: (id: string, request: ClusterControlRequest) =>
    post<ClusterControl>(`/cluster/nodes/${encodeURIComponent(id)}/control`, request),
  clusterConsole: (signal?: AbortSignal) =>
    apiFetch<import("./types").ClusterConsole>("/cluster/console", { signal }),
  clusterStatus: (signal?: AbortSignal) => apiFetch<ClusterStatus>("/cluster/", { signal }),
  clusterSettings: (signal?: AbortSignal) =>
    apiFetch<ClusterSettings>("/cluster/settings", { signal }),
  createJoinToken: (ttlMinutes: number) =>
    post<CreatedJoinToken>("/cluster/join-tokens", { ttlMinutes }),
  revokeJoinToken: (id: string) =>
    apiFetch<null>(`/cluster/join-tokens/${encodeURIComponent(id)}`, { method: "DELETE" }),

  // ---- Users (limits) ----------------------------------------------------------
  users: async () => (await apiFetch<{ users: EtherUser[] | null }>("/users")).users ?? [],
  updateUser: async (id: number, patch: Partial<EtherUser>) =>
    (await send<{ user: EtherUser }>("PATCH", `/users/${id}`, patch)).user,

  // ---- Collection Studio ------------------------------------------------------
  collections: async () =>
    (await apiFetch<{ collections: CollectionSpec[] | null } | null>("/collections"))
      ?.collections ?? [],
  collection: (id: string) => apiFetch<CollectionSpec>(`/collections/${encodeURIComponent(id)}`),
  createCollection: (spec: CollectionSpec) => post<CollectionSpec>("/collections", spec),
  updateCollection: (id: string, spec: CollectionSpec) =>
    put<CollectionSpec>(`/collections/${encodeURIComponent(id)}`, spec),
  deleteCollection: (id: string) =>
    apiFetch<null>(`/collections/${encodeURIComponent(id)}`, { method: "DELETE" }),
  previewCollection: (spec: CollectionSpec, signal?: AbortSignal, n?: number) =>
    post<CollectionPreview>(`/collections/preview${qs({ n })}`, spec, signal),
  rebuildCollection: (id: string) =>
    post<{ itemCount: number; lastBuiltAt: string }>(
      `/collections/${encodeURIComponent(id)}/rebuild`,
    ),
  templates: async () =>
    (await apiFetch<{ templates: CollectionTemplate[] | null }>("/collections/templates"))
      .templates ?? [],
  templateBundles: async () =>
    (
      await apiFetch<{ bundles: CollectionTemplateBundle[] | null }>(
        "/collections/template-bundles",
      )
    ).bundles ?? [],
  previewTemplateBundle: (id: string) =>
    post<CollectionTemplateBundlePreview>(
      `/collections/template-bundles/${encodeURIComponent(id)}/preview`,
    ),
  applyTemplateBundle: (id: string) =>
    post<CollectionTemplateBundleApplyResult>(
      `/collections/template-bundles/${encodeURIComponent(id)}/apply`,
    ),
  artworkUrl: (id: string, w: number, version?: string) =>
    `${API_BASE}/collections/${encodeURIComponent(id)}/artwork${qs({ w, v: version })}`,

  // ---- Catalogue helpers ---------------------------------------------------------
  browseCatalog: (
    params: {
      noTotal?: string;
      cursor?: string;
      after?: string;
      filter?: string;
      secondarySort?: string;
      secondaryDesc?: string;
      desc?: string;
      ruleVisibility?: string;
      rules?: string;
      ruleFallback?: string;
      q?: string;
      type?: string;
      genre?: string;
      decade?: string;
      waiting?: string;
      sort?: string;
      offset?: number;
      limit?: number;
    },
    signal?: AbortSignal,
  ) =>
    apiFetch<{
      items: TitleCard[];
      total: number;
      totalExact?: boolean;
      hasMore?: boolean;
      nextCursor?: string;
    }>(`/catalog/browse${qs(params)}`, { signal }),
  searchCatalog: async (q: string, type?: string, limit = 24, signal?: AbortSignal) =>
    (
      await apiFetch<{ items: TitleCard[] | null }>(`/catalog/search${qs({ q, type, limit })}`, {
        signal,
      })
    ).items ?? [],
  facets: async (type: string, facet: FacetName) =>
    (await apiFetch<{ values: FacetValue[] | null }>(`/catalog/facets${qs({ type, facet })}`))
      .values ?? [],
  lookup: async (kind: LookupKind, q: string, region?: string, signal?: AbortSignal) =>
    (
      await apiFetch<{ results: LookupResult[] | null }>(
        `/catalog/lookup${qs({ kind, q, region })}`,
        { signal },
      )
    ).results ?? [],
  title: (key: string) => apiFetch<TitleDetail>(`/catalog/title/${encodeURIComponent(key)}`),

  // ---- Viewers -------------------------------------------------------------------
  viewers: async () => (await apiFetch<{ viewers: Viewer[] | null }>("/viewers")).viewers ?? [],
  viewer: (id: number) => apiFetch<ViewerDetail>(`/viewers/${id}`),
  taste: (id: number) => apiFetch<TasteProfile>(`/viewers/${id}/taste`),
  putOverrides: (id: number, overrides: TasteOverride[]) =>
    put<TasteProfile>(`/viewers/${id}/taste/overrides`, { overrides }),
  viewerHome: (
    id: number,
    ctx: { device: string; at: string; section?: string },
    signal?: AbortSignal,
  ) => apiFetch<ViewerHome>(`/viewers/${id}/home${qs(ctx)}`, { signal }),
  refreshViewer: (id: number) => post<null>(`/viewers/${id}/refresh`),

  // ---- Home Designer -----------------------------------------------------------
  layout: () => apiFetch<LayoutDoc>("/layout"),
  putLayout: (layout: HomeLayout) => put<LayoutDoc>("/layout", { layout }),
  /** The Plex Home a viewer (null = the typical viewer) gets with a draft layout (omitted = saved). */
  previewLayout: (
    body: PreviewContext & { viewerId: number | null; layout?: HomeLayout; count?: number },
    signal?: AbortSignal,
  ) => post<ViewerHome>("/layout/preview", body, signal),
  layoutImpact: (body: PreviewContext & { layout: HomeLayout }, signal?: AbortSignal) =>
    post<LayoutImpactResult>("/layout/impact", body, signal),

  // ---- Insights ---------------------------------------------------------------------
  rowInsights: async () =>
    (await apiFetch<{ rows: RowInsight[] | null }>("/insights/rows")).rows ?? [],
  /** The learned ranking model's state (bare or wrapped as { ranker }). */
  rankerInsight: async (): Promise<RankerInsight> => {
    const r = await apiFetch<RankerInsight | { ranker: RankerInsight }>("/insights/ranker");
    return "ranker" in r ? r.ranker : r;
  },
  activity: async () =>
    (await apiFetch<{ days: ActivityDay[] | null }>("/insights/activity")).days ?? [],
};

/**
 * A random v4 UUID. crypto.randomUUID only exists in secure contexts, and owners
 * open the console over plain HTTP on their LAN, so fall back to getRandomValues.
 */
export function uuid(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6]! & 0x0f) | 0x40;
  b[8] = (b[8]! & 0x3f) | 0x80;
  const h = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

export function newId(prefix: string): string {
  const rand = Math.random().toString(36).slice(2, 8);
  return `${prefix}-${rand}`;
}

/** Turn any thrown value into a sentence for the owner. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 401) return t("Your session has expired. Sign in again.");
    if (err.code === "setup_incomplete") return t("You can finish now and add sources later.");
    if (err.code === "plex_required")
      return t("Create an owner account, or connect Plex, before finishing.");
    return err.message;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}
