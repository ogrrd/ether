import { SubtabNav } from "../../components/layout";
import { Badge, Button, Input, Spinner, cn, toast } from "@braedonsaunders/appkit-ui";
import { ArrowLeft, ArrowUpRight, HardDrive, Network, Settings2, Sparkles } from "lucide-react";
import { CACHE_PRESETS, CachePresetCards, cachePresetKey } from "./presets";
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { localized, t, tn } from "../../i18n";
import { useRouteTab } from "../../components/layout";
import { CacheOverview } from "./CacheOverview";
import { CacheExplorer } from "./CacheExplorer";
import { selectionReason, shelfStateLabel } from "./reasons";
import { WarmNewArrivals } from "../viewers/sections/WatchlistDefaults";
const CACHE_TABS = localized(
  () =>
    [
      { key: "overview", label: t("Overview") },
      { key: "explorer", label: t("Explorer") },
      { key: "preloading", label: t("Preloading") },
    ] as const,
);
import { Drawer } from "../../components/Drawer";
import { RestartButton } from "../../components/RestartDrawer";
import { ErrorPanel } from "../../components/feedback";
import { Field, NumberInput } from "../../components/fields";
import { useDraft, useUnsavedGuard } from "../../components/SaveBar";
import { Workspace } from "../../components/workspace";
import { api, apiFetch } from "../../lib/api";
import { useApp } from "../../app-context";
import { formatBytes, formatNumber, formatPercent } from "../../lib/format";
import { useAsync, useInterval } from "../../lib/hooks";
import type { EtherConfig } from "../../lib/types";

type Policy = EtherConfig["cache"];
export type CacheCause = {
  code: string;
  firstSeen?: string;
  shelf?: { reasonCodes?: string[]; score?: number; state?: string };
};
export type CacheRetention = {
  kind: "pinned" | "expiry" | "replay" | "lru";
  pins?: string[];
  days?: number;
  hours?: number;
  until?: string;
};
export type ReleaseReason = {
  causes: CacheCause[];
  retention: CacheRetention;
  file: string;
  source: string;
};
type Release = {
  ReleaseID: string;
  Title: string;
  Bytes: number;
  Size: number;
  Segments: number;
  Plays: number;
  LastUsed: string;
  LastPlayed: string;
  Pins: string[] | null;
  reason?: ReleaseReason;
};
type PreloadStatus = {
  state: string;
  message: string;
  title: string;
  fetched: number;
  size: number;
  allocated: number;
  ready: number;
  failed: number;
  paused: boolean;
  viewers?: number;
  selection?: {
    key: number;
    title: string;
    type: string;
    score: number;
    catalogue: number;
    household: number;
    reason: string;
    reasonCodes?: string[];
    reasonArgs?: Record<string, number>;
    state?: string;
  }[];
  items: {
    key: number;
    title: string;
    releaseId: string;
    size: number;
    complete: boolean;
    state?: string;
  }[];
};
export type CacheSnapshot = {
  policy?: Policy;
  shared?: boolean;
  clustered?: boolean;
  restartRequired?: boolean;
  storageError?: string;
  preload: PreloadStatus;
  used: number;
  budget: number;
  directory: string;
  protectedBytes: number;
  releases: Release[];
  metrics: { hits: number; misses: number; servedBytes: number; evictedBytes: number };
  prefetch: {
    queued: number;
    running: number;
    completed: number;
    failed: number;
    dropped: number;
    current?: { title: string; cause: string };
  };
};
const GiB = 1024 ** 3,
  MiB = 1024 ** 2;
function Toggle({
  title,
  description,
  checked,
  disabled,
  onChange,
}: {
  title: string;
  description: string;
  checked: boolean;
  disabled?: boolean;
  onChange: (v: boolean) => void;
}) {
  return (
    <label
      className={cn(
        "flex cursor-pointer items-start justify-between gap-5 py-3",
        disabled && "opacity-45",
      )}
    >
      <span>
        <span className="block text-sm font-medium text-fg">{title}</span>
        <span className="mt-1 block text-xs leading-relaxed text-fg-muted">{description}</span>
      </span>
      <input
        type="checkbox"
        role="switch"
        aria-label={title}
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
        className="mt-1 h-4 w-4 shrink-0 accent-primary"
      />
    </label>
  );
}

export default function Cache() {
  const { health } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const live = useAsync((signal) => apiFetch<CacheSnapshot>("/cache/", { signal }), []);
  const tab = useRouteTab(CACHE_TABS);
  // Keep the explorer live, and back off after failures.
  const [pollFailures, setPollFailures] = useState(0);
  useEffect(() => {
    if (live.error) setPollFailures((n) => Math.min(n + 1, 4));
    else if (live.data) setPollFailures(0);
  }, [live.error, live.data]);
  useInterval(
    () => {
      if (!live.loading) void live.reload();
    },
    Math.min(5000 * 2 ** pollFailures, 60_000),
  );
  const { draft, setDraft, dirty, reset } = useDraft<Policy>(config.data?.cache);
  useUnsavedGuard(dirty);
  const [saving, setSaving] = useState(false),
    [error, setError] = useState<unknown>(null);
  const [advanced, setAdvanced] = useState(false);
  const [editor, setEditor] = useState<"profiles" | "policy" | null>(null);
  const [profileChoice, setProfileChoice] = useState<{
    key: string;
    name: string;
    values: Partial<Policy>;
  } | null>(null);
  const navigate = useNavigate();
  const [unit, setUnit] = useState<"GiB" | "TiB">("GiB");
  const [busy, setBusy] = useState<string | null>(null);
  const activeDirectory = live.data?.directory ?? config.data?.cache.dir ?? "";
  const disk = useAsync(() => api.disk(activeDirectory), [activeDirectory]);
  const patch = (v: Partial<Policy>) => setDraft((d) => (d ? { ...d, ...v } : d));
  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await api.putSection("cache", draft);
      await config.reload();
      await live.reload();
      setEditor(null);
      setProfileChoice(null);
      toast.success(t("Cache policy saved"));
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }
  async function action(path: string, body?: unknown) {
    setBusy(path);
    setError(null);
    try {
      await apiFetch(`/cache/${path}`, {
        method: "POST",
        ...(body ? { body: JSON.stringify(body) } : {}),
      });
      await live.reload();
      if (path === "warm") toast.success(t("Resume warming requested"));
    } catch (e) {
      setError(e);
    } finally {
      setBusy(null);
    }
  }
  async function clearRelease(releaseId: string, title: string, block: boolean) {
    const question = block
      ? t(
          "Clear cached bytes for {title} and block this release from future playback? Active or paused playback must be stopped first. Protected bytes may remain until background work ends. You can unblock it in the release controls.",
          { title },
        )
      : t(
          "Clear cached bytes for {title}? The release stays playable and is fetched again the next time it plays. Active or paused playback must be stopped first. Protected bytes may remain until background work ends.",
          { title },
        );
    if (!window.confirm(question)) return;
    const path = block ? "clear-block" : "clear";
    setBusy(path);
    setError(null);
    try {
      const result = await apiFetch<{ cleared: boolean; message: string }>(`/cache/${path}`, {
        method: "POST",
        body: JSON.stringify({ releaseId }),
      });
      await live.reload();
      if (result.cleared) toast.success(result.message);
      else setError(new Error(result.message));
    } catch (e) {
      setError(e);
    } finally {
      setBusy(null);
    }
  }
  if (!draft)
    return (
      <Workspace title={t("Cache")}>
        {config.error ? (
          <ErrorPanel error={config.error} onRetry={() => void config.reload()} />
        ) : (
          <div className="grid flex-1 place-items-center">
            <Spinner label={t("Loading cache")} />
          </div>
        )}
      </Workspace>
    );
  const snap = live.data;
  const storage = health.find((h) => h.id === "storage");
  const used = snap?.used ?? 0;
  const saved = config.data!.cache;
  const matched = cachePresetKey(saved);
  const savedPreset = CACHE_PRESETS.find((p) => p.key === matched);
  const policyName =
    saved.mode === "off" || matched === "pass-through"
      ? t("Pass-through")
      : savedPreset?.name || t("Custom policy");
  const enabled = draft.mode !== "off",
    speculate = enabled && !draft.disableSpeculation;
  return (
    <Workspace
      title={t("Cache")}
      actions={
        <>
          {dirty && <Badge variant="warning">{t("Unsaved policy")}</Badge>}
          <Badge variant="secondary">
            {snap?.shared
              ? snap.clustered
                ? t("Shared cluster pool")
                : t("NAS storage")
              : t("This node")}
          </Badge>
          <Button size="sm" onClick={() => setEditor("policy")}>
            <Settings2 size={14} /> {dirty ? t("Resume editing") : t("Edit policy")}
          </Button>
        </>
      }
    >
      <div className="@container flex min-h-0 flex-1 flex-col">
        {storage && (
          <div
            role="status"
            title={storage.details.join("\n")}
            className="flex shrink-0 flex-col gap-0.5 border-b border-warning/20 bg-warning/10 px-5 py-2 text-xs text-warning"
          >
            <span className="font-medium">{storage.summary}</span>
            <span>
              {storage.details[0]} {storage.note}
            </span>
          </div>
        )}
        {snap?.restartRequired && (
          <div
            role="status"
            className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b border-warning/20 bg-warning/10 px-5 py-2 text-xs text-warning"
          >
            <span>
              {t(
                "Storage change saved. Restart this node to use the new backend or folder. The overview shows the currently active cache.",
              )}
            </span>
            <RestartButton
              context={t("Restarting puts your new cache storage to work.")}
              label={t("Restart now")}
            />
          </div>
        )}
        {snap?.storageError && (
          <div
            role="alert"
            className="shrink-0 border-b border-danger/20 bg-danger/10 px-5 py-2 text-xs text-danger"
          >
            {t("{error}. Cache writes are suspended until the mount and database are available.", {
              error: snap.storageError,
            })}
          </div>
        )}
        <div className="shrink-0 border-b border-border bg-surface px-4 sm:px-6">
          <SubtabNav
            ariaLabel={t("Cache sections")}
            active={tab}
            onSelect={(key) => navigate(`/cache/${key}`)}
            tabs={CACHE_TABS.map((tabDef) =>
              tabDef.key === "explorer" && snap?.releases.length
                ? { ...tabDef, count: snap.releases.length }
                : tabDef,
            )}
          />
        </div>
        <main
          aria-label={
            tab === "overview"
              ? t("Cache overview")
              : tab === "explorer"
                ? t("Cache explorer")
                : t("Cache preloading")
          }
          className={cn(
            "app-scroll flex min-h-0 flex-1 flex-col bg-bg",
            tab === "overview" ? "overflow-y-auto cockpit:overflow-hidden" : "overflow-hidden",
          )}
        >
          {error ? (
            <div className="shrink-0 px-4 pt-3">
              <ErrorPanel error={error} />
            </div>
          ) : null}
          {live.error ? (
            <div className="shrink-0 px-4 pt-3">
              <ErrorPanel error={live.error} onRetry={() => void live.reload()} />
            </div>
          ) : null}
          {tab === "overview" ? (
            <div className="mx-auto w-full max-w-[1600px] p-4 sm:p-5 cockpit:min-h-0 cockpit:flex-1">
              <CacheOverview
                snapshot={snap}
                policy={snap?.policy ?? saved}
                name={
                  snap?.policy
                    ? snap.policy.mode === "off"
                      ? t("Pass-through")
                      : (CACHE_PRESETS.find((p) => p.key === cachePresetKey(snap.policy!))?.name ??
                        t("Custom policy"))
                    : policyName
                }
                freeBytes={disk.data?.freeBytes}
                busy={busy !== null}
                onProfile={() => {
                  setProfileChoice(null);
                  setEditor("profiles");
                }}
                onExplore={() => navigate("/cache/explorer")}
                onPreloading={() => navigate("/cache/preloading")}
                onWarm={() => void action("warm")}
              />
            </div>
          ) : tab === "explorer" ? (
            <div className="mx-auto flex min-h-0 w-full max-w-[1600px] flex-1 flex-col p-4 sm:p-5">
              <CacheExplorer
                snapshot={snap}
                busy={busy !== null}
                onPin={(releaseId, pinned) => void action("pin", { releaseId, pinned })}
                onClear={(id, title, block) => void clearRelease(id, title, block)}
              />
            </div>
          ) : (
            <div className="app-scroll mx-auto grid min-h-0 w-full max-w-[1600px] flex-1 grid-cols-1 gap-4 overflow-y-auto p-4 sm:p-5 lg:grid-cols-12 lg:overflow-hidden">
              <div className="lg:col-span-12">
                <WarmNewArrivals />
              </div>
              <section className="lg:col-span-5 min-h-0 overflow-y-auto rounded-xl border border-primary/20 bg-surface">
                <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border p-4">
                  <div>
                    <p className="text-[10px] font-semibold uppercase tracking-[.18em] text-primary">
                      {t("Managed local library")}
                    </p>
                    <h2 className="mt-1 text-lg font-semibold">{t("Preload activity")}</h2>
                  </div>
                  <Button
                    variant="secondary"
                    size="sm"
                    disabled={busy !== null || !config.data?.cache.preloadEnabled}
                    onClick={() => void action("preload", { paused: !snap?.preload.paused })}
                  >
                    {snap?.preload.paused ? t("Resume preloading") : t("Pause preloading")}
                  </Button>
                </div>
                <div className="space-y-4 p-4">
                  <div className="grid grid-cols-3 gap-3">
                    <div>
                      <p className="text-2xl font-semibold">{snap?.preload.ready ?? 0}</p>
                      <p className="text-xs text-fg-muted">{t("Sources preloaded")}</p>
                    </div>
                    <div>
                      <p className="text-2xl font-semibold">
                        {formatBytes(snap?.preload.allocated ?? 0)}
                      </p>
                      <p className="text-xs text-fg-muted">{t("Reserved, including overhead")}</p>
                    </div>
                    <div>
                      <p className="text-2xl font-semibold">
                        {formatBytes(config.data?.cache.preloadBudgetBytes ?? 0)}
                      </p>
                      <p className="text-xs text-fg-muted">{t("Preload allocation")}</p>
                    </div>
                  </div>
                  <div className="rounded-lg bg-bg-subtle p-3">
                    <p className="text-sm font-medium">
                      {snap?.preload.paused
                        ? t("Paused")
                        : snap?.preload.state === "filling"
                          ? snap.preload.title
                          : snap?.preload.state === "resolving"
                            ? t("Finding a source for {title}", { title: snap.preload.title })
                            : !saved.preloadEnabled
                              ? t("Preloading disabled")
                              : snap?.preload.state === "selecting"
                                ? t("Selecting titles")
                                : snap?.preload.state === "ready"
                                  ? t("Selection pass complete")
                                  : t("Waiting for the next pass")}
                    </p>
                    <p className="mt-1 text-xs text-fg-muted">
                      {snap?.preload.message ||
                        t("Enable movie and TV preloading to build a local shelf.")}
                    </p>
                    {snap?.preload.state === "filling" && (
                      <>
                        <div
                          role="progressbar"
                          aria-label={t("Current preload")}
                          aria-valuenow={snap.preload.fetched}
                          aria-valuemax={snap.preload.size}
                          className="mt-3 h-1.5 overflow-hidden rounded bg-primary/10"
                        >
                          <div
                            className="h-full bg-primary"
                            style={{
                              width: `${snap.preload.size ? Math.min(100, (snap.preload.fetched / snap.preload.size) * 100) : 0}%`,
                            }}
                          />
                        </div>
                        <p className="mt-1 text-[10px] text-fg-subtle">
                          {formatBytes(snap.preload.fetched)} / {formatBytes(snap.preload.size)}
                        </p>
                      </>
                    )}
                  </div>
                  <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border pt-3">
                    <p className="text-xs text-fg-muted">
                      {config.data?.cache.preloadDuringPlayback
                        ? t("May run during playback")
                        : t("Waits during playback")}{" "}
                      ·{" "}
                      {config.data?.cache.preloadRateMiB
                        ? t("{rate} MiB/s limit", { rate: config.data.cache.preloadRateMiB })
                        : t("No bandwidth cap")}
                    </p>
                  </div>
                  {(snap?.preload.failed ?? 0) > 0 && (
                    <p className="text-xs text-fg-muted">
                      {tn(
                        snap?.preload.failed ?? 0,
                        "{count} attempt skipped or interrupted this run. Unavailable or oversized titles do not stop the shelf.",
                        "{count} attempts skipped or interrupted this run. Unavailable or oversized titles do not stop the shelf.",
                      )}
                    </p>
                  )}
                  {!!snap?.preload.items?.length && (
                    <div className="border-t border-border pt-3">
                      <h3 className="text-sm font-medium">{t("Shelf manifest")}</h3>
                      <ul className="mt-2 space-y-2">
                        {snap.preload.items.slice(0, 8).map((it) => (
                          <li
                            key={it.key}
                            className="flex items-center justify-between gap-2 text-xs"
                          >
                            <span className="truncate">{it.title}</span>
                            <Badge
                              variant={
                                it.state === "failed"
                                  ? "destructive"
                                  : it.state === "cached"
                                    ? "success"
                                    : "secondary"
                              }
                            >
                              {shelfStateLabel(it.state)}
                            </Badge>
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                </div>
              </section>
              <section className="app-scroll lg:col-span-7 min-h-0 overflow-y-auto rounded-xl border border-border bg-surface">
                <div className="flex items-start justify-between gap-3 border-b border-border p-4">
                  <div>
                    <h2 className="flex items-center gap-2 text-sm font-semibold">
                      <Sparkles size={16} className="text-primary" /> {t("Preload selection")}
                    </h2>
                    <p className="mt-1 text-xs text-fg-muted">
                      {tn(
                        snap?.preload.viewers ?? 0,
                        "Titles chosen for the local shelf from the last pass · {count} active viewer considered",
                        "Titles chosen for the local shelf from the last pass · {count} active viewers considered",
                      )}
                    </p>
                  </div>
                  <Badge>{t("Local shelf choices")}</Badge>
                </div>
                {snap?.preload.selection?.length ? (
                  <div className="divide-y divide-border">
                    {snap.preload.selection.slice(0, 6).map((item, i) => (
                      <div key={item.key} className="flex items-start gap-3 p-4">
                        <span className="mt-0.5 w-5 shrink-0 font-mono text-xs text-fg-subtle">
                          {String(i + 1).padStart(2, "0")}
                        </span>
                        <div className="min-w-0 flex-1">
                          <div className="flex items-center gap-2">
                            <span className="truncate text-sm font-medium">{item.title}</span>
                            <span className="shrink-0 text-[10px] uppercase text-fg-subtle">
                              {item.type === "episode" ? t("TV") : t("Film")}
                            </span>
                            {(() => {
                              const state =
                                snap.preload.items.find((it) => it.key === item.key)?.state ??
                                item.state;
                              return (
                                <Badge
                                  variant={
                                    state === "failed"
                                      ? "destructive"
                                      : state === "cached"
                                        ? "success"
                                        : "secondary"
                                  }
                                >
                                  {shelfStateLabel(state)}
                                </Badge>
                              );
                            })()}
                          </div>
                          <p className="mt-1 text-xs text-fg-muted">
                            {selectionReason(item.reasonCodes, item.reasonArgs)}
                          </p>
                          <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-[10px] text-fg-subtle">
                            <span>
                              {t("Catalogue {score}/100", { score: Math.round(item.catalogue) })}
                            </span>
                            <span>
                              {t("Household {score}/100", { score: Math.round(item.household) })}
                            </span>
                          </div>
                        </div>
                        <span
                          className="shrink-0 rounded-md bg-primary/10 px-2 py-1 font-mono text-xs text-primary"
                          aria-label={t("Selection score {score} out of 100", {
                            score: Math.round(item.score),
                          })}
                        >
                          {Math.round(item.score)}
                        </span>
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className="p-4 text-sm leading-relaxed text-fg-muted">
                    {t(
                      "Enable preloading and save to build your first selection. Movies blend discovery with household favourites; TV keeps a rolling window of episodes ready. Scores appear after the next selection pass.",
                    )}
                  </p>
                )}
                <p className="border-t border-border px-4 py-3 text-[10px] text-fg-subtle">
                  {t(
                    "Scores rank cache candidates, not predicted viewing probabilities. TV windows taper by episode distance. Selection does not guarantee source availability.",
                  )}
                </p>
              </section>
            </div>
          )}
        </main>
      </div>
      <Drawer
        open={editor !== null}
        onClose={() => {
          setEditor(null);
          setAdvanced(false);
        }}
        title={
          editor === "profiles"
            ? t("Choose a cache profile")
            : profileChoice
              ? t("Review your cache profile")
              : t("Edit cache policy")
        }
        size="lg"
        footer={
          editor === "profiles" ? (
            <div className="flex items-center justify-between gap-3">
              <span className="text-xs text-fg-muted">{t("1 / Choose a starting point")}</span>
              <Button
                disabled={!profileChoice}
                onClick={() => {
                  if (profileChoice) {
                    patch(profileChoice.values);
                    setEditor("policy");
                  }
                }}
              >
                {t("Review profile")} <ArrowUpRight size={14} />
              </Button>
            </div>
          ) : (
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Button
                variant="ghost"
                onClick={() => {
                  reset();
                  setEditor(null);
                  setProfileChoice(null);
                }}
                disabled={saving}
              >
                {t("Discard changes")}
              </Button>
              <Button onClick={() => void save()} disabled={!dirty || saving}>
                {saving ? t("Saving…") : t("Save policy")}
              </Button>
            </div>
          )
        }
      >
        {editor === "profiles" ? (
          <div className="space-y-4">
            <p className="text-sm leading-relaxed text-fg-muted">
              {t(
                "Pick a starting point, then review the budget and behavior. Nothing changes until you save.",
              )}
            </p>
            <p className="rounded-lg border border-primary/20 bg-primary/5 p-3 text-xs text-fg-muted">
              {t(
                "Cinema node and Edge library enable full movie and episode preloading. All sizes and limits can be customized in the next step.",
              )}
            </p>
            <CachePresetCards selected={profileChoice?.key ?? null} onSelect={setProfileChoice} />
          </div>
        ) : (
          <div className="space-y-6">
            <div className="rounded-xl border border-primary/20 bg-primary/5 p-4">
              <div className="flex items-center justify-between gap-3">
                <div>
                  <p className="text-[10px] font-semibold uppercase tracking-[.16em] text-primary">
                    {profileChoice ? t("Review your preset") : t("Your starting point")}
                  </p>
                  <p className="mt-1 text-sm font-semibold">{profileChoice?.name ?? policyName}</p>
                </div>
                <Button variant="secondary" size="sm" onClick={() => setEditor("profiles")}>
                  <ArrowLeft size={14} /> {t("Back to presets")}
                </Button>
              </div>
              <p className="mt-2 text-xs leading-relaxed text-fg-muted">
                {t(
                  "Choose a different preset at any time, then customize it here. Your current policy stays active until you save.",
                )}
              </p>
            </div>
            {error ? <ErrorPanel error={error} /> : null}
            <section aria-label={t("Cache storage")} className="space-y-3">
              <div className="flex items-center justify-between">
                <h3 className="text-sm font-semibold">{t("Where your cache lives")}</h3>
                <Badge variant="secondary">
                  {draft.storage === "shared" && snap?.clustered
                    ? t("Cluster policy")
                    : t("Server policy")}
                </Badge>
              </div>
              <div
                className="grid grid-cols-2 gap-3"
                role="radiogroup"
                aria-label={t("Storage backend")}
              >
                <button
                  type="button"
                  role="radio"
                  aria-checked={draft.storage !== "shared"}
                  onClick={() => patch({ storage: "local" })}
                  className={cn(
                    "rounded-xl border p-3 text-left transition",
                    draft.storage !== "shared"
                      ? "border-primary bg-primary/5"
                      : "border-border hover:border-primary/40",
                  )}
                >
                  <HardDrive size={18} className="mb-2 text-primary" />
                  <span className="block text-sm font-medium">{t("Local disk")}</span>
                  <span className="mt-1 block text-xs text-fg-muted">
                    {t("An independent cache on this node.")}
                  </span>
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={draft.storage === "shared"}
                  onClick={() =>
                    patch({ ...config.data?.sharedCache, storage: "shared", dir: draft.dir })
                  }
                  className={cn(
                    "rounded-xl border p-3 text-left transition disabled:opacity-50",
                    draft.storage === "shared"
                      ? "border-primary bg-primary/5"
                      : "border-border hover:border-primary/40",
                  )}
                >
                  <Network size={18} className="mb-2 text-primary" />
                  <span className="block text-sm font-medium">{t("Shared NAS")}</span>
                  <span className="mt-1 block text-xs text-fg-muted">
                    {snap?.clustered
                      ? t("One library and budget across your cluster.")
                      : t("Keep your cache on a mounted network share.")}
                  </span>
                </button>
              </div>
              {draft.storage === "shared" && (
                <p className="rounded-lg bg-primary/5 p-3 text-xs leading-relaxed text-fg-muted">
                  {snap?.clustered
                    ? t(
                        "Mount the same NAS share on every participating node. Each node may use a different mount path. Budgets, retention, pins, and preloading are shared; saving this policy updates all members of the pool.",
                      )
                    : t(
                        "Mount your NAS share on this server and choose a dedicated folder. Your media cache lives on the NAS; its database stays on this server. No cluster is required. To let multiple Ether servers use the same cache folder, connect them in a cluster first.",
                      )}
                </p>
              )}
              <Field
                label={
                  draft.storage === "shared"
                    ? t("NAS mount path on this node")
                    : t("Cache directory")
                }
                htmlFor="cache-dir"
                hint={t(
                  "Changing storage or its folder requires a node restart. Existing cache bytes stay in their original folder.",
                )}
              >
                <Input
                  id="cache-dir"
                  value={draft.dir}
                  onChange={(e) => patch({ dir: e.target.value })}
                  placeholder={draft.storage === "shared" ? "/mnt/media-cache" : undefined}
                />
              </Field>
            </section>
            <Field
              label={t("Media disk budget")}
              hint={t(
                "Zero means unlimited, not disabled. Choose No persistent media below to turn disk caching off.",
              )}
              htmlFor="cache-budget"
            >
              <div className="flex items-center gap-2">
                <NumberInput
                  id="cache-budget"
                  min={0}
                  step={0.25}
                  value={draft.budgetBytes / (unit === "TiB" ? 1024 * GiB : GiB)}
                  onChange={(n) =>
                    patch({ budgetBytes: Math.round(n * (unit === "TiB" ? 1024 * GiB : GiB)) })
                  }
                />
                <select
                  aria-label={t("Budget unit")}
                  value={unit}
                  onChange={(e) => setUnit(e.target.value as "GiB" | "TiB")}
                  className="rounded border border-border bg-surface p-2 text-xs"
                >
                  <option>GiB</option>
                  <option>TiB</option>
                </select>
              </div>
            </Field>
            {disk.data && draft.dir === activeDirectory && (
              <p className="flex items-center gap-2 text-xs text-fg-muted">
                <HardDrive size={13} />
                {t("{size} currently free", { size: formatBytes(disk.data.freeBytes) })}
              </p>
            )}
            {disk.data &&
              draft.dir === activeDirectory &&
              enabled &&
              draft.budgetBytes > 0 &&
              draft.budgetBytes - used > disk.data.freeBytes && (
                <p className="rounded-lg bg-warning/10 p-3 text-xs text-warning">
                  {t(
                    "This budget needs more space than is currently free. Choose a smaller budget.",
                  )}
                </p>
              )}
            <div className="border-t border-border pt-5">
              <p className="text-[10px] font-semibold uppercase tracking-[.2em] text-fg-subtle">
                {t("02 / What stays")}
              </p>
              <label className="mt-3 block text-sm font-medium" htmlFor="cache-mode">
                {t("Retention strategy")}
              </label>
              <select
                id="cache-mode"
                value={draft.mode || "demand"}
                onChange={(e) =>
                  patch({
                    mode: e.target.value,
                    ...(e.target.value === "off" ? { preloadEnabled: false } : {}),
                  })
                }
                className="mt-2 w-full rounded-lg border border-border bg-surface p-2.5 text-sm"
              >
                <option value="off">{t("No persistent media")}</option>
                <option value="demand">{t("Keep what is streamed")}</option>
                <option value="adaptive">{t("Favour repeat watches")}</option>
              </select>
              <p className="mt-2 text-xs leading-relaxed text-fg-muted">
                {!enabled
                  ? t(
                      "New media bytes bypass disk. Unprotected existing bytes are released; playback and pinned bytes remain protected.",
                    )
                  : draft.mode === "adaptive"
                    ? t(
                        "Repeat plays earn extra time before eviction. Popularity is learned from playback, never from background reads.",
                      )
                    : t(
                        "Keep recently used bytes. The oldest unprotected segments make room first.",
                      )}
              </p>
            </div>
            <Field
              label={t("Expire after inactivity")}
              hint={t("Days since last cache use. Zero keeps bytes until space is needed.")}
              htmlFor="cache-retention"
            >
              <NumberInput
                id="cache-retention"
                min={0}
                max={3650}
                value={draft.retentionDays ?? 0}
                onChange={(n) => patch({ retentionDays: n })}
              />
            </Field>
            <div className="border-t border-border pt-5">
              <p className="text-[10px] font-semibold uppercase tracking-[.2em] text-fg-subtle">
                {t("03 / Think ahead")}
              </p>
              <Toggle
                title={t("Allow opening and resume warming")}
                description={t(
                  "Let Ether fetch small windows before Play. Turn off for strictly on-demand media.",
                )}
                checked={speculate}
                disabled={!enabled}
                onChange={(v) => patch({ disableSpeculation: !v })}
              />
              <Toggle
                title={t("Warm detail pages")}
                description={t(
                  "Opening a movie or episode prepares its source and opening bytes. Shows warm their On Deck episode when supported.",
                )}
                checked={draft.detailWarmup !== "off"}
                disabled={!speculate}
                onChange={(v) => patch({ detailWarmup: v ? "on" : "off" })}
              />
              <Toggle
                title={t("Prepare the next episode")}
                description={t("Warm the opening near the end of the current episode.")}
                checked={draft.prefetchNextEpisode}
                disabled={!speculate}
                onChange={(v) => patch({ prefetchNextEpisode: v })}
              />
              <Toggle
                title={t("Keep resume points warm")}
                description={t("Periodically refresh a small window around Continue Watching.")}
                checked={draft.keepInProgressWarm}
                disabled={!speculate}
                onChange={(v) => patch({ keepInProgressWarm: v })}
              />
              <Field
                label={t("Read ahead while playing")}
                hint={t(
                  "Keeps the next minutes of whatever is playing downloaded to this cache, so a slow Usenet provider does not make viewers buffer. Uses spare connections; playback always comes first.",
                )}
                htmlFor="cache-playback-read-ahead"
              >
                <select
                  id="cache-playback-read-ahead"
                  className="w-full rounded border border-border bg-surface p-2 text-sm"
                  disabled={!enabled}
                  value={draft.playbackReadAheadMinutes ?? 5}
                  onChange={(e) => patch({ playbackReadAheadMinutes: Number(e.target.value) })}
                >
                  {[...new Set([0, 2, 5, 10, draft.playbackReadAheadMinutes ?? 5])]
                    .sort((a, b) => a - b)
                    .map((minutes) => (
                      <option key={minutes} value={minutes}>
                        {minutes === 0
                          ? t("Off")
                          : minutes === 5
                            ? t("5 minutes (recommended)")
                            : tn(minutes, "{count} minute", "{count} minutes")}
                      </option>
                    ))}
                </select>
              </Field>
            </div>
            <div className="border-t border-border pt-5">
              <p className="text-[10px] font-semibold uppercase tracking-[.2em] text-fg-subtle">
                {t("04 / Your local library")}
              </p>
              <Toggle
                title={t("Preload movies & TV")}
                description={t(
                  "Proactively fill a dedicated part of your cache from the local catalogue using your sources and quality rules. This downloads complete movie and episode sources.",
                )}
                checked={draft.preloadEnabled ?? false}
                disabled={!enabled}
                onChange={(v) =>
                  patch({
                    preloadEnabled: v,
                    ...(v && !draft.preloadBudgetBytes
                      ? { preloadBudgetBytes: Math.min(draft.budgetBytes || 100 * GiB, 100 * GiB) }
                      : {}),
                  })
                }
              />
              {draft.preloadEnabled && (
                <div className="space-y-4 rounded-xl border border-primary/20 bg-primary/5 p-3">
                  <Field
                    label={t("Preload allocation (GiB)")}
                    hint={t("Part of the total budget, not additional storage.")}
                    htmlFor="preload-budget"
                  >
                    <NumberInput
                      id="preload-budget"
                      min={1}
                      value={(draft.preloadBudgetBytes ?? 0) / GiB}
                      onChange={(n) => patch({ preloadBudgetBytes: Math.round(n * GiB) })}
                    />
                  </Field>
                  <Field label={t("Include in your shelf")} htmlFor="preload-content">
                    <select
                      id="preload-content"
                      className="w-full rounded border border-border bg-surface p-2 text-sm"
                      value={draft.preloadContent || "mixed"}
                      onChange={(e) => patch({ preloadContent: e.target.value })}
                    >
                      <option value="mixed">{t("Movies & TV episodes")}</option>
                      <option value="movies">{t("Movies only")}</option>
                      <option value="tv">{t("TV episodes only")}</option>
                    </select>
                  </Field>
                  <Field label={t("Selection strategy")} htmlFor="preload-source">
                    <select
                      id="preload-source"
                      className="w-full rounded border border-border bg-surface p-2 text-sm"
                      value={draft.preloadSource || "smart"}
                      onChange={(e) => patch({ preloadSource: e.target.value })}
                    >
                      <option value="smart">{t("Smart household blend")}</option>
                      <option value="popular">{t("Catalogue popularity")}</option>
                      <option value="recent">{t("Newest releases")}</option>
                    </select>
                  </Field>
                  {(!draft.preloadSource || draft.preloadSource === "smart") && (
                    <div className="space-y-3 rounded-lg border border-primary/20 bg-surface p-3">
                      <div className="flex items-center justify-between gap-2 text-xs">
                        <label htmlFor="household-weight" className="font-medium">
                          {t("Household influence")}
                        </label>
                        <span className="font-mono text-primary">
                          {formatPercent((draft.preloadHouseholdWeight ?? 65) / 100)}
                        </span>
                      </div>
                      <input
                        id="household-weight"
                        type="range"
                        min={0}
                        max={100}
                        step={5}
                        className="w-full accent-primary"
                        value={draft.preloadHouseholdWeight ?? 65}
                        onChange={(e) => patch({ preloadHouseholdWeight: Number(e.target.value) })}
                      />
                      <div className="flex justify-between text-[10px] text-fg-subtle">
                        <span>{t("Catalogue discovery")}</span>
                        <span>{t("Your household")}</span>
                      </div>
                      <p className="text-[11px] leading-relaxed text-fg-muted">
                        {t(
                          "Recent repeat viewing, shared favourites and genre interests guide the shelf. Older viewing fades; no history falls back to catalogue popularity.",
                        )}
                      </p>
                    </div>
                  )}
                  <Field
                    label={t("Learn from")}
                    htmlFor="household-scope"
                    hint={t("Also determines whose TV progress guides episode selection.")}
                  >
                    <select
                      id="household-scope"
                      className="w-full rounded border border-border bg-surface p-2 text-sm"
                      value={draft.preloadHouseholdScope || "home"}
                      onChange={(e) => patch({ preloadHouseholdScope: e.target.value })}
                    >
                      <option value="home">{t("Owner & Plex Home members")}</option>
                      <option value="all">{t("All active shared viewers")}</option>
                    </select>
                  </Field>
                  {draft.preloadContent !== "movies" && (
                    <Field
                      label={t("Episode window per show")}
                      htmlFor="episode-window"
                      hint={t(
                        "A rolling shelf around current viewing positions. New shows start at episode one; different viewers share this limit.",
                      )}
                    >
                      <NumberInput
                        id="episode-window"
                        min={1}
                        max={24}
                        value={draft.preloadEpisodesAhead || 3}
                        onChange={(n) => patch({ preloadEpisodesAhead: n })}
                      />
                    </Field>
                  )}
                  <p className="text-[11px] text-fg-muted">
                    {tn(
                      Math.floor((draft.preloadBudgetBytes ?? 0) / (8 * GiB)),
                      "Room for roughly {count} movie-sized source at 8 GiB each. Episode sizes vary; both share this allocation.",
                      "Room for roughly {count} movie-sized sources at 8 GiB each. Episode sizes vary; both share this allocation.",
                    )}
                  </p>
                </div>
              )}
            </div>
            <Button variant="secondary" className="w-full" onClick={() => setAdvanced(true)}>
              <Settings2 size={15} /> {t("Fine tune policy")} <ArrowUpRight size={14} />
            </Button>
            <p className="text-[11px] leading-relaxed text-fg-subtle">
              {t(
                "Live streams and pinned releases can exceed the budget. Expiry never removes protected bytes. This budget covers media segments; artwork, metadata and transcode output use separate storage.",
              )}
            </p>
            <Drawer
              open={advanced}
              onClose={() => setAdvanced(false)}
              title={t("Fine tune your cache")}
              size="lg"
            >
              <div className="space-y-6 p-1">
                <p className="text-sm text-fg-muted">
                  {t("Changes join your draft. Save the policy when you’re ready.")}
                </p>
                <Field
                  label={t("Household history window (days)")}
                  htmlFor="history-window"
                  hint={t(
                    "Recent views count more; frequency is capped per viewer. Completed watches and reviewed imports contribute, never preload activity.",
                  )}
                >
                  <NumberInput
                    id="history-window"
                    min={1}
                    max={730}
                    value={draft.preloadHistoryDays || 90}
                    onChange={(n) => patch({ preloadHistoryDays: n })}
                  />
                </Field>
                <Field
                  label={t("Maximum preloaded titles")}
                  hint={t("Up to {max}. The allocation still limits how many fit.", {
                    max: formatNumber(5000),
                  })}
                  htmlFor="preload-count"
                >
                  <NumberInput
                    id="preload-count"
                    min={1}
                    max={5000}
                    value={draft.preloadMaxTitles || 200}
                    onChange={(n) => patch({ preloadMaxTitles: n })}
                  />
                </Field>
                <Field
                  label={t("Preload speed (MiB/s)")}
                  hint={t(
                    "Average payload cap, with short bursts for each 16 MiB window. Zero removes the cap.",
                  )}
                  htmlFor="preload-rate"
                >
                  <NumberInput
                    id="preload-rate"
                    min={0}
                    max={10000}
                    value={draft.preloadRateMiB ?? 0}
                    onChange={(n) => patch({ preloadRateMiB: n })}
                  />
                </Field>
                <Field
                  label={t("Keep disk space free (GiB)")}
                  hint={t("Preloading waits below this reserve. Zero uses the 5 GiB default.")}
                  htmlFor="preload-reserve"
                >
                  <NumberInput
                    id="preload-reserve"
                    min={0}
                    value={(draft.preloadMinFreeBytes ?? 5 * GiB) / GiB}
                    onChange={(n) => patch({ preloadMinFreeBytes: Math.round(n * GiB) })}
                  />
                </Field>
                <Toggle
                  title={t("Preload during playback")}
                  description={t(
                    "Otherwise, complete-title filling waits while viewers are streaming. Opening and next-episode warming use their separate controls.",
                  )}
                  checked={draft.preloadDuringPlayback ?? false}
                  onChange={(v) => patch({ preloadDuringPlayback: v })}
                />
                <Field
                  label={t("Opening window (MiB)")}
                  hint={t(
                    "2–64 MiB for detail pages. Larger windows use more bandwidth; startup still depends on the source and client.",
                  )}
                  htmlFor="cache-opening"
                >
                  <NumberInput
                    id="cache-opening"
                    min={2}
                    max={64}
                    value={(draft.detailBytes || 2 * MiB) / MiB}
                    onChange={(n) => patch({ detailBytes: Math.round(n * MiB) })}
                  />
                </Field>
                <Field label={t("Next episode / resume window (MiB)")} htmlFor="cache-prefetch">
                  <NumberInput
                    id="cache-prefetch"
                    min={1}
                    value={draft.prefetchBytes / MiB}
                    onChange={(n) => patch({ prefetchBytes: Math.round(n * MiB) })}
                  />
                </Field>
                <Field label={t("Prepare next episode at (%)")} htmlFor="cache-trigger">
                  <NumberInput
                    id="cache-trigger"
                    min={1}
                    max={99}
                    value={Math.round(draft.nextEpisodeAt * 100)}
                    onChange={(n) => patch({ nextEpisodeAt: n / 100 })}
                  />
                </Field>
                <Field
                  label={t("Replay credit (hours per repeat play)")}
                  hint={t(
                    "Adaptive mode gives up to eight repeat plays a recency credit. It never overrides expiry or pins. Credit fades out after 30 days without a play.",
                  )}
                  htmlFor="cache-boost"
                >
                  <NumberInput
                    id="cache-boost"
                    min={0}
                    max={720}
                    value={draft.replayBoostHours ?? 0}
                    onChange={(n) => patch({ replayBoostHours: n })}
                  />
                </Field>
                <Field
                  label={t("Resume protection share (%)")}
                  hint={t(
                    "Maximum share of the budget assigned to in-progress releases. The most recently watched are considered first.",
                  )}
                  htmlFor="cache-share"
                >
                  <NumberInput
                    id="cache-share"
                    min={1}
                    max={100}
                    value={draft.inProgressPinPercent || 50}
                    onChange={(n) => patch({ inProgressPinPercent: n })}
                  />
                </Field>
                <Field
                  label={t("Resume protection age")}
                  hint={t(
                    "A duration such as 720h (30 days). Abandoned titles lose their resume pin after this interval.",
                  )}
                  htmlFor="cache-age"
                >
                  <Input
                    id="cache-age"
                    value={draft.inProgressPinMaxAge || "720h"}
                    onChange={(e) => patch({ inProgressPinMaxAge: e.target.value })}
                  />
                </Field>
                <p className="text-xs leading-relaxed text-fg-muted">
                  {t(
                    "Detail warming is rate limited and pauses during active playback. Trailer and post-play warming also require background warming here and their own feature settings.",
                  )}
                </p>
                <Button onClick={() => setAdvanced(false)}>{t("Done")}</Button>
              </div>
            </Drawer>
          </div>
        )}
      </Drawer>
    </Workspace>
  );
}
