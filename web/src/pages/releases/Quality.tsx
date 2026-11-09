import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  SearchSelect,
  SettingsSection,
  Switch,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Check, Download, Film, Plus, RefreshCw, SlidersHorizontal, Tv } from "lucide-react";
import { useState } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { ChipGroup, NumberInput, Segmented } from "../../components/fields";
import { Section } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { ApiError, api } from "../../lib/api";
import { relativeTime } from "../../lib/format";
import { itemName, resolutionGroups, withMediaFormats } from "../../lib/qualityRanking";
import {
  MEDIA_MOVIE,
  MEDIA_TV,
  PROFILE_MEDIA,
  appForMedia,
  mediaApplies,
  mediaLabel,
  mediaTypeForApp,
  profileMediaType,
  releaseConsole,
  type ProfileMedia,
  type QualitySyncReport,
} from "../../lib/releases";
import type { CustomFormatDefinition, QualityProfile } from "../../lib/types";
import { parseDuration } from "../settings/duration";
import { SourceOrderSection } from "../sources/EqualQuality";
import { RemoteBitrateSection } from "./RemoteBitrateSection";
import { ConfigSection } from "../../components/ConfigSection";
import { PLAY_VERSIONS } from "../settings/schema";
import {
  BoolRow,
  RecordDrawer,
  RecordItem,
  TextRow,
  recordErrors,
  type Errors,
} from "../sources/form";
import { FilterRulesEditor, releaseRuleConflict, useRankingRegistry } from "./ReleasePreferences";
import { CustomFormatsSection, QualityOrderSection } from "./QualityRanking";
import {
  formatsJSON,
  mergeCustomFormats,
  parseFormats,
  sameFormats,
  type ResolverPolicy,
} from "./resolverDraft";
import { TrashImport } from "./TrashImport";
import { settleQualityRules, useQuality, type Quality } from "./useQuality";
/**
 * A new profile starts as a copy of the media's first profile (its quality
 * order and format scores), so it ranks like the owner's own until changed.
 */
function blankProfile(profiles: QualityProfile[], media: ProfileMedia): QualityProfile {
  const taken = (name: string) => profiles.some((p) => p.name === name && p.mediaType === media);
  let n = profiles.filter((p) => p.mediaType === media).length + 1;
  while (taken(t("Profile {n}", { n }))) n++;
  const base = profiles.find((p) => p.mediaType === media && !p.import);
  return {
    name: t("Profile {n}", { n }),
    mediaType: media,
    maxHeight: 1080,
    minHeight: 720,
    allowHdr: false,
    allowDolbyVision: false,
    qualityOrder: resolutionGroups(1080, 720),
    customFormatScores: Object.fromEntries(
      Object.entries(base?.customFormatScores ?? {}).filter(([id]) => !id.startsWith("rule:")),
    ),
    minSizeMbPerMin: 5,
    maxSizeMbPerMin: 150,
    episodeMinSizeMbPerMin: 0,
    episodeMaxSizeMbPerMin: 0,
    preferredLanguages: null,
    allowCam: false,
  };
}

const RESOLUTIONS = localized(() => [
  { value: "0", label: t("Any") },
  { value: "480", label: "480p" },
  { value: "720", label: "720p" },
  { value: "1080", label: "1080p" },
  { value: "2160", label: t("4K (2160p)") },
]);

const resolutionLabel = (h: number) =>
  RESOLUTIONS.find((x) => x.value === String(h))?.label ?? `${h}p`;

function profileSummary(p: QualityProfile) {
  const range =
    p.minHeight === p.maxHeight
      ? resolutionLabel(p.maxHeight)
      : t("{min} to {max}", {
          min: resolutionLabel(p.minHeight),
          max: resolutionLabel(p.maxHeight),
        });
  const first = p.qualityOrder?.[0];
  const formats = Object.values(p.customFormatScores ?? {}).filter((v) => v !== 0).length;
  return [
    range,
    p.allowDolbyVision ? t("HDR and Dolby Vision") : p.allowHdr ? t("HDR allowed") : t("SDR only"),
    first ? t("{quality} first", { quality: itemName(first) }) : "",
    formats ? tn(formats, "{count} custom format", "{count} custom formats") : "",
  ]
    .filter(Boolean)
    .join(" · ");
}

const MEDIA_OPTIONS = localized(() =>
  PROFILE_MEDIA.map((value) => ({ value, label: mediaLabel(value) })),
);
/** Movies / Shows badge for a profile (Movies & shows for a rule for every media). */
export function MediaBadge({ media }: { media?: string }) {
  return <Badge variant={media ? "info" : "secondary"}>{mediaLabel(media)}</Badge>;
}

/**
 * The page-level scope choice. Two large, filled tabs make the selected
 * media obvious before the dependent profile/default/Away sections.
 */
function MediaScopeToggle({
  media,
  onChange,
}: {
  media: ProfileMedia;
  onChange: (media: ProfileMedia) => void;
}) {
  const options = [
    ...PROFILE_MEDIA.map((value) => ({
      value,
      label: mediaLabel(value),
      Icon: value === MEDIA_TV ? Tv : Film,
    })),
  ];
  return (
    <div
      role="radiogroup"
      aria-label={t("Media")}
      className="grid w-full max-w-sm grid-cols-2 gap-1 rounded-xl border border-border bg-bg-subtle p-1"
    >
      {options.map(({ value, label, Icon }, i) => {
        const on = value === media;
        return (
          <button
            key={value}
            type="button"
            role="radio"
            aria-checked={on}
            aria-label={label}
            tabIndex={on ? 0 : -1}
            onClick={() => onChange(value)}
            onKeyDown={(e) => {
              if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
              e.preventDefault();
              const next =
                options[(i + (e.key === "ArrowRight" ? 1 : options.length - 1)) % options.length];
              if (!next) return;
              onChange(next.value);
              (
                e.currentTarget.parentElement?.children[options.indexOf(next)] as
                  HTMLElement | undefined
              )?.focus();
            }}
            className={[
              "flex h-12 items-center justify-center gap-2 rounded-lg px-4 text-base font-semibold transition-colors",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
              on
                ? "bg-primary text-primary-fg shadow-sm shadow-primary/30"
                : "text-fg-muted hover:bg-surface hover:text-fg",
            ].join(" ")}
          >
            <Icon size={18} aria-hidden />
            {label}
            {on ? <Check size={16} aria-hidden className="ml-0.5" /> : null}
          </button>
        );
      })}
    </div>
  );
}

const HOME_FIELD = { movie: "defaultMovie", tv: "defaultTv" } as const;
const AWAY_FIELD = { movie: "remoteMovie", tv: "remoteTv" } as const;

/** Whether a profile named name serves media. Names are unique per media. */
const hasProfile = (profiles: QualityProfile[], name: string, media: string) =>
  profiles.some((p) => p.name === name && p.mediaType === media);

/**
 * The stored default for a media: its own field, else the media-neutral one
 * when a profile of that name serves the media.
 */
function mediaDefault(quality: Quality, media: ProfileMedia, away: boolean): string {
  const scoped = quality[away ? AWAY_FIELD[media] : HOME_FIELD[media]];
  if (scoped) return scoped;
  const neutral = (away ? quality.remote : quality.default) ?? "";
  return neutral && hasProfile(quality.profiles ?? [], neutral, media) ? neutral : "";
}

/** First profile (optionally other than `exclude`) for media. */
function mediaFallback(profiles: QualityProfile[], media: string, exclude?: string): string {
  return profiles.find((p) => p.name !== exclude && p.mediaType === media)?.name ?? "";
}

/**
 * Rename media's profile `from` to `to` in every reference. A movie and a
 * show profile may share a name, so references that served both keep the
 * other media's profile: a media-neutral default becomes explicit per
 * media, and a rule for every media splits into one rule per media.
 * `profiles` is the list after the rename.
 */
function withRenamedProfile(
  quality: Quality,
  profiles: QualityProfile[],
  media: ProfileMedia,
  from: string,
  to: string,
): Quality {
  const next: Quality = { ...quality };
  if (next[HOME_FIELD[media]] === from) next[HOME_FIELD[media]] = to;
  if (next[AWAY_FIELD[media]] === from) next[AWAY_FIELD[media]] = to;
  for (const [neutral, fields] of [
    ["default", HOME_FIELD],
    ["remote", AWAY_FIELD],
  ] as const) {
    if (next[neutral] !== from) continue;
    for (const m of PROFILE_MEDIA) {
      if (!next[fields[m]]) {
        next[fields[m]] = m === media ? to : hasProfile(profiles, from, m) ? from : "";
      }
    }
    next[neutral] = to;
  }
  next.rules = (quality.rules ?? []).flatMap((r) => {
    if (r.profile !== from || (r.mediaType && r.mediaType !== media)) return [r];
    if (r.mediaType === media) return [{ ...r, profile: to }];
    return PROFILE_MEDIA.flatMap((m) =>
      m === media
        ? [{ ...r, mediaType: m, profile: to }]
        : hasProfile(profiles, from, m)
          ? [{ ...r, mediaType: m }]
          : [],
    );
  });
  return next;
}

/**
 * The server names a refused profile by its position in the list sent. Name
 * the profile instead, so a change blocked by another profile (removing one
 * while two others clash) says which one to fix.
 */
function withProfileNames(e: unknown, sent: QualityProfile[]): unknown {
  if (!(e instanceof ApiError) || !e.fields?.length) return e;
  const fields = e.fields.map((f) => {
    const at = /^quality\.profiles\[(\d+)\]/.exec(f.field);
    const p = at ? sent[Number(at[1])] : undefined;
    if (!p) return f;
    return { ...f, field: t("{name} ({media})", { name: p.name, media: mediaLabel(p.mediaType) }) };
  });
  return new ApiError(e.status, e.code, e.message, fields);
}

/**
 * Keep every stored default pointing at a profile that serves its media. An
 * At home default that outlives its profile falls back to the media's first
 * profile; an Away default falls back to the At home one.
 */
function settleDefaults(next: Quality): Quality {
  const profiles = next.profiles ?? [];
  for (const media of PROFILE_MEDIA) {
    const home = HOME_FIELD[media];
    const away = AWAY_FIELD[media];
    if (next[home] && !hasProfile(profiles, next[home]!, media)) {
      next[home] = mediaFallback(profiles, media);
    }
    if (next[away] && !hasProfile(profiles, next[away]!, media)) next[away] = "";
    // The media-neutral default must not decide for a media it cannot serve.
    if (!next[home] && next.default && !hasProfile(profiles, next.default, media)) {
      next[home] = mediaFallback(profiles, media);
    }
  }
  if (profiles.length > 0) {
    if (next.default && !profiles.some((p) => p.name === next.default)) next.default = "";
    if (next.remote && !profiles.some((p) => p.name === next.remote)) next.remote = "";
    if (next.rules) next.rules = settleQualityRules(next).rules;
  }
  return next;
}

const SYNC_INTERVALS = localized(() => [
  { value: "6", label: t("Every 6 hours") },
  { value: "12", label: t("Every 12 hours") },
  { value: "24", label: t("Every 24 hours") },
  { value: "72", label: t("Every 3 days") },
  { value: "168", label: t("Every 7 days") },
]);

/** Hours in a Go duration string, defaulting to 24. */
function syncHours(raw: string | undefined): string {
  if (!raw) return "24";
  const { amount, unit } = parseDuration(raw);
  const n = Number(amount);
  if (!Number.isFinite(n)) return "24";
  const hours = unit === "d" ? n * 24 : unit === "h" ? n : n / 60;
  return hours > 0 ? String(hours) : "24";
}

/**
 * Automatic update of imported profiles and their format definitions. "Sync
 * now" stays available while automatic sync is off: a manual pull is how an
 * owner takes guide changes only when they choose to.
 */
function SyncSection({ quality, onSaved }: { quality: Quality; onSaved: () => Promise<void> }) {
  const sync = quality.sync;
  const hasRun = Boolean(sync?.lastRun) && !sync!.lastRun!.startsWith("0001-");
  const { draft, setDraft, dirty, reset } = useDraft({
    enabled: sync?.enabled ?? false,
    hours: syncHours(sync?.interval),
  });
  const [saving, setSaving] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [syncError, setSyncError] = useState<unknown>(null);
  const [report, setReport] = useState<QualitySyncReport | null>(null);
  if (!draft) return null;
  const imported = (quality.profiles ?? []).some((p) => p.import);
  const hours = draft.hours;
  const options = SYNC_INTERVALS.some((o) => o.value === hours)
    ? SYNC_INTERVALS
    : [...SYNC_INTERVALS, { value: hours, label: t("Every {hours} hours", { hours }) }];

  async function save() {
    if (!draft) return;
    setSaving(true);
    setSaveError(null);
    try {
      // The admin endpoint patches the quality section, so omitted run
      // state stays whatever the server recorded while this form was open.
      await api.putSection("quality", {
        sync: { enabled: draft.enabled, interval: `${draft.hours}h` },
      });
      toast.success(t("Sync settings saved"));
      await onSaved();
    } catch (e) {
      setSaveError(e);
    } finally {
      setSaving(false);
    }
  }

  async function syncNow() {
    setSyncing(true);
    setSyncError(null);
    setReport(null);
    try {
      setReport(await releaseConsole.syncQuality());
      toast.success(t("Guide sync finished"));
      await onSaved();
    } catch (e) {
      setSyncError(e);
    } finally {
      setSyncing(false);
    }
  }

  const title = t("Keep imported profiles in sync");
  if (!imported) {
    return (
      <Section title={title}>
        <p className="text-sm text-fg-muted">
          {t("Applies once you import a profile from TRaSH Guides or Profilarr.")}
        </p>
      </Section>
    );
  }

  return (
    <Section
      title={title}
      description={t("Only profiles imported from TRaSH Guides or Profilarr are updated.")}
      actions={
        <Switch
          aria-label={title}
          checked={draft.enabled}
          onChange={(e) => setDraft({ ...draft, enabled: e.currentTarget.checked })}
        />
      }
    >
      {draft.enabled ? (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <span className="text-sm font-medium text-fg">{t("Check for changes")}</span>
          <div className="w-52">
            <SearchSelect
              {...searchSelectLabels()}
              ariaLabel={t("Check for changes")}
              value={hours}
              onChange={(value) => setDraft({ ...draft, hours: value })}
              options={options}
              searchable={false}
            />
          </div>
        </div>
      ) : null}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="text-sm text-fg-muted">
          {sync?.lastError ? (
            <span className="text-danger">{sync.lastError}</span>
          ) : hasRun ? (
            t("Last synced {when}", { when: relativeTime(sync!.lastRun!) })
          ) : (
            t("Never synced")
          )}
        </span>
        <Button variant="outline" size="sm" disabled={syncing} onClick={() => void syncNow()}>
          <RefreshCw size={14} className={syncing ? "animate-spin" : undefined} />
          {syncing ? t("Syncing…") : t("Sync now")}
        </Button>
      </div>
      {syncError ? <ErrorPanel error={syncError} /> : null}
      {report ? (
        <Alert variant="success" role="status">
          <AlertTitle>{t("Guide sync finished")}</AlertTitle>
          <AlertDescription>
            <ul className="list-disc space-y-1 pl-5">
              <li>
                {tn(report.profilesUpdated, "{count} profile updated", "{count} profiles updated")}
              </li>
              <li>{tn(report.formatsAdded, "{count} format added", "{count} formats added")}</li>
              <li>
                {tn(report.formatsUpdated, "{count} format updated", "{count} formats updated")}
              </li>
              <li>
                {tn(report.formatsRemoved, "{count} format removed", "{count} formats removed")}
              </li>
              {(report.warnings ?? []).map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={saveError}
        onSave={() => void save()}
        onDiscard={() => {
          reset();
          setSaveError(null);
        }}
      />
    </Section>
  );
}

export function QualityTab() {
  const { config, quality, commit, refresh } = useQuality();
  const [media, setMedia] = useState<ProfileMedia>(MEDIA_MOVIE);
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!quality) return <LoadingRows rows={4} />;
  return (
    <>
      <div className="space-y-5 rounded-xl border border-border p-5">
        <ProfileList
          quality={quality}
          resolver={config.data?.resolver ?? null}
          commit={commit}
          refresh={refresh}
          media={media}
          onMediaChange={setMedia}
        />
      </div>
      <div className="pt-1">
        <div className="flex items-center gap-3">
          <div className="h-px flex-1 bg-border" />
          <span className="text-[11px] font-semibold uppercase tracking-[0.14em] text-fg-muted">
            {t("Shared for movies and shows")}
          </span>
          <div className="h-px flex-1 bg-border" />
        </div>
      </div>
      <SyncSection quality={quality} onSaved={refresh} />
      <RemoteBitrateSection />
      <SourceOrderSection />
      <ConfigSection
        configKey="resolver"
        endpoint="resolver"
        groups={PLAY_VERSIONS}
        label={t("Version menu")}
      />
    </>
  );
}

type Commit = (next: Quality, message: string) => Promise<void>;

function ProfileList({
  quality,
  resolver,
  commit,
  refresh,
  media,
  onMediaChange,
}: {
  quality: Quality;
  resolver: ResolverPolicy | null;
  commit: Commit;
  refresh: () => Promise<void>;
  media: ProfileMedia;
  onMediaChange: (media: ProfileMedia) => void;
}) {
  const { fields } = useRankingRegistry();
  const profiles = quality.profiles ?? [];
  const visible = profiles
    .map((profile, index) => ({ profile, index }))
    .filter(({ profile }) => profile.mediaType === media);
  const homeDefault = mediaDefault(quality, media, false);
  const awayDefault = mediaDefault(quality, media, true);
  const [importing, setImporting] = useState(false);
  const rules = quality.rules ?? [];
  const [editing, setEditing] = useState<{ index: number; draft: QualityProfile } | null>(null);
  const [defaultFor, setDefaultFor] = useState<{ home: boolean; away: boolean }>({
    home: false,
    away: false,
  });
  /** The shared custom format definitions editor (JSON). */
  const [formatsText, setFormatsText] = useState("[]");
  /** The definitions editor text when the drawer opened; unchanged means no PUT. */
  const [formatBaseline, setFormatBaseline] = useState("");
  /** The shared definitions as they were when the drawer opened (for merging). */
  const [formatSnapshot, setFormatSnapshot] = useState<CustomFormatDefinition[]>([]);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  const open = (index: number, draft: QualityProfile) => {
    setErrors({});
    setError(null);
    const cloned = structuredClone(draft);
    setEditing({ index, draft: cloned });
    const isExisting = index < profiles.length;
    const scope = (profileMediaType(cloned) || media) as ProfileMedia;
    setDefaultFor({
      // The first profile for a media becomes its At home default.
      home: isExisting
        ? mediaDefault(quality, scope, false) === cloned.name
        : !profiles.some((p) => p.mediaType === scope),
      away: isExisting && mediaDefault(quality, scope, true) === cloned.name,
    });
    const text = formatsJSON(resolver);
    setFormatsText(text);
    setFormatBaseline(text);
    setFormatSnapshot(structuredClone(resolver?.customFormats ?? []));
  };

  async function save() {
    if (!editing) return;
    const { index, draft: profileDraft } = editing;
    let draft: QualityProfile = { ...profileDraft };
    let parsed: CustomFormatDefinition[] | null = null;
    if (formatsText !== formatBaseline) {
      try {
        parsed = parseFormats(formatsText);
      } catch (e) {
        setError(e);
        return;
      }
    }
    const old = profiles[index];
    const scope = (profileMediaType(draft) || media) as ProfileMedia;
    draft = withMediaFormats({ ...draft, mediaType: scope });
    const list =
      index < profiles.length
        ? profiles.map((p, j) => (j === index ? draft : p))
        : [...profiles, draft];
    const renamed = old && old.name !== draft.name ? old.name : null;

    let next: Quality = { ...quality, profiles: list };
    // Moved to the other media: the rules that used it for its old media
    // follow it. Defaults it can no longer serve are settled below.
    const moved = old?.mediaType && old.mediaType !== scope ? old : null;
    if (moved) {
      next.rules = rules.map((r) =>
        r.profile === moved.name && r.mediaType === moved.mediaType
          ? { ...r, mediaType: scope }
          : r,
      );
    }
    if (renamed) next = withRenamedProfile(next, list, scope, renamed, draft.name);

    const homeField = HOME_FIELD[scope];
    const awayField = AWAY_FIELD[scope];
    if (defaultFor.home) {
      next[homeField] = draft.name;
    } else if (old && mediaDefault(quality, scope, false) === old.name) {
      next[homeField] = mediaFallback(list, scope, draft.name);
    }
    // Away is optional: with no Away profile, people away from home get
    // the At home one.
    if (defaultFor.away) {
      next[awayField] = draft.name;
    } else if (old && mediaDefault(quality, scope, true) === old.name) {
      // Clearing an Away default that came from the media-neutral field
      // keeps the other media's Away profile.
      if (next.remote) {
        for (const m of PROFILE_MEDIA) {
          if (m !== scope && !next[AWAY_FIELD[m]]) {
            next[AWAY_FIELD[m]] = mediaDefault(quality, m, true);
          }
        }
        next.remote = "";
      }
      next[awayField] = "";
    }
    if (!next.default) next.default = draft.name;
    next = settleDefaults(next);

    const conflict = releaseRuleConflict(draft.releaseRules ?? [], fields);
    if (conflict) {
      setErrors({ releaseRules: conflict });
      return;
    }
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await commit(next, index < profiles.length ? t("Profile saved") : t("Profile added"));
      // Only touch the shared definitions when this drawer actually edited
      // them; otherwise a stale snapshot must not be sent back.
      if (parsed) {
        const latestConfig = await api.getConfig();
        const latest = latestConfig.resolver.customFormats ?? [];
        const merged = mergeCustomFormats({
          original: formatSnapshot,
          edited: parsed,
          latest,
          app: appForMedia(scope),
        });
        if (!sameFormats(merged, latest)) {
          await api.putSection("resolver", { customFormats: merged });
          await refresh();
        }
      }
      setEditing(null);
    } catch (e) {
      setError(withProfileNames(e, list));
      setErrors(recordErrors(e, `quality.profiles[${index}].`));
    } finally {
      setSaving(false);
    }
  }

  async function remove() {
    if (!editing) return;
    const { index } = editing;
    const name = profiles[index]?.name ?? editing.draft.name;
    const scope = (profileMediaType(profiles[index] ?? editing.draft) || media) as ProfileMedia;
    const rest = profiles.filter((_, j) => j !== index);
    // A rule keeps the media that still have a profile of this name; a rule
    // left with none is removed.
    let usedBy = 0;
    const nextRules = rules.flatMap((r) => {
      if (r.profile !== name) return [r];
      const left = PROFILE_MEDIA.filter(
        (m) => mediaApplies(r.mediaType, m) && hasProfile(rest, name, m),
      );
      if (!left.length) {
        usedBy++;
        return [];
      }
      return r.mediaType || left.length === PROFILE_MEDIA.length
        ? [r]
        : left.map((m) => ({ ...r, mediaType: m }));
    });
    let next: Quality = { ...quality, profiles: rest, rules: nextRules };
    if (!rest.some((p) => p.name === name)) {
      if (next.default === name) next.default = rest[0]?.name ?? "";
      if (next.remote === name) next.remote = "";
    }
    if (next[HOME_FIELD[scope]] === name) next[HOME_FIELD[scope]] = mediaFallback(rest, scope);
    if (next[AWAY_FIELD[scope]] === name) next[AWAY_FIELD[scope]] = "";
    next = settleDefaults(next);
    const fallback = mediaDefault(next, scope, false) || mediaFallback(rest, scope);
    const moves = mediaDefault(quality, scope, false) === name;
    const consequences = [
      name ? t("{name} will be removed.", { name }) : t("This profile will be removed."),
      usedBy
        ? tn(
            usedBy,
            "The device rule using it will be removed.",
            "{count} device rules using it will be removed.",
          )
        : "",
      moves && fallback
        ? t("Clients that used it as a default will use {profile}.", { profile: fallback })
        : "",
    ]
      .filter(Boolean)
      .join(" ");
    const ok = await confirmDialog({
      title: t("Remove profile?"),
      message: consequences,
      confirmLabel: t("Remove"),
      tone: "danger",
    });
    if (!ok) return;
    setSaving(true);
    setError(null);
    try {
      await commit(next, t("Profile removed"));
      setEditing(null);
    } catch (e) {
      setError(withProfileNames(e, rest));
    } finally {
      setSaving(false);
    }
  }

  const draft = editing?.draft;
  const draftScope = draft ? profileMediaType(draft) : "";
  const isNew = editing ? editing.index >= profiles.length : false;
  const patch = (p: Partial<QualityProfile>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...p } } : e));

  /** Change "Applies to" and point the default checkboxes at the new media's fields. */
  function changeApplies(nextMedia: ProfileMedia) {
    const existing = editing ? profiles[editing.index] : undefined;
    patch({ mediaType: nextMedia });
    if (existing) {
      const selected = nextMedia;
      setDefaultFor({
        home: mediaDefault(quality, selected, false) === existing.name,
        away: mediaDefault(quality, selected, true) === existing.name,
      });
    }
  }

  return (
    <>
      <div>
        <div className="flex flex-wrap items-center justify-between gap-4">
          <div className="min-w-0">
            <p className="text-[11px] font-semibold uppercase tracking-[0.14em] text-primary">
              {t("Profile scope")}
            </p>
            <div className="mt-3 flex flex-wrap items-center gap-4">
              <MediaScopeToggle media={media} onChange={onMediaChange} />
              <p className="max-w-md text-sm text-fg-muted">
                {t("Profiles and their At home and Away defaults apply to {media}.", {
                  media: mediaLabel(media),
                })}
              </p>
            </div>
          </div>
          <Button size="sm" onClick={() => open(profiles.length, blankProfile(profiles, media))}>
            <Plus size={14} /> {t("Add profile")}
          </Button>
        </div>
      </div>

      <Section
        title={t("Profiles")}
        description={t("Resolution, HDR, release type and language rules used to rank releases.")}
        bodyClassName=""
      >
        <div className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-border bg-bg-subtle/40 px-5 py-3 text-sm">
          <span className="flex items-center gap-2">
            <span className="text-xs font-medium text-fg-muted">{t("At home default")}</span>
            <Badge variant="info">{homeDefault || t("First compatible profile")}</Badge>
          </span>
          <span className="flex items-center gap-2">
            <span className="text-xs font-medium text-fg-muted">{t("Away default")}</span>
            <Badge variant="info">{awayDefault || t("Same as at home")}</Badge>
          </span>
          <span className="text-xs text-fg-muted">{t("Set these from a profile’s editor.")}</span>
        </div>
        {visible.length === 0 ? (
          <EmptyState
            icon={<SlidersHorizontal />}
            title={
              profiles.length
                ? media === MEDIA_MOVIE
                  ? t("No movie profiles")
                  : t("No show profiles")
                : t("No profiles")
            }
            description={
              profiles.length
                ? t("Switch to {media} to see its profiles.", {
                    media: mediaLabel(media === MEDIA_MOVIE ? MEDIA_TV : MEDIA_MOVIE),
                  })
                : t("Add a profile so Ether knows which releases to prefer.")
            }
          />
        ) : (
          <ul className="divide-y divide-border">
            {visible.map(({ profile: p, index: i }) => {
              const usedBy = rules.filter((r) => r.profile === p.name).length;
              return (
                <RecordItem
                  key={`${p.name}-${i}`}
                  title={p.name || t("Profile {n}", { n: i + 1 })}
                  description={profileSummary(p)}
                  meta={
                    <>
                      <MediaBadge media={profileMediaType(p)} />
                      {homeDefault === p.name ? (
                        <Badge variant="info">{t("Default at home")}</Badge>
                      ) : null}
                      {awayDefault === p.name ? (
                        <Badge variant="info">{t("Default away")}</Badge>
                      ) : null}
                      {usedBy ? (
                        <Badge variant="secondary">
                          {tn(usedBy, "{count} rule", "{count} rules")}
                        </Badge>
                      ) : null}
                    </>
                  }
                  editLabel={t("Edit {name}", { name: p.name || t("profile {n}", { n: i + 1 }) })}
                  onEdit={() => open(i, p)}
                />
              );
            })}
          </ul>
        )}
      </Section>

      <TrashImport
        open={importing}
        media={media}
        onClose={() => setImporting(false)}
        onImported={refresh}
      />

      <RecordDrawer
        open={editing !== null}
        onClose={() => !saving && setEditing(null)}
        title={isNew ? t("New profile") : draft?.name || t("Profile")}
        description={t("How releases are filtered and ranked for apps using this profile.")}
        onSave={() => void save()}
        saving={saving}
        saveLabel={t("Save profile")}
        onRemove={isNew ? undefined : () => void remove()}
        removeLabel={t("Remove profile")}
        removeDisabled={profiles.length <= 1}
        error={error}
      >
        {draft ? (
          <>
            {isNew ? (
              <Button
                variant="outline"
                onClick={() => {
                  setEditing(null);
                  setImporting(true);
                }}
              >
                <Download size={16} />
                {t("Import a preset quality profile")}
              </Button>
            ) : null}
            <SettingsSection title={t("Profile")}>
              <TextRow
                label={t("Name")}
                value={draft.name}
                error={errors.name}
                onChange={(v) => patch({ name: v })}
              />
              <SettingsRow
                title={t("Applies to")}
                description={
                  draft.import
                    ? t("Set by the {app} profile it was imported from.", {
                        app: draft.import.app === "sonarr" ? "Sonarr" : "Radarr",
                      })
                    : t("Movies or shows: a profile ranks releases for one of them.")
                }
                control={
                  draft.import || isNew ? (
                    <span className="flex flex-wrap items-center gap-1.5">
                      <Badge variant="secondary">
                        {mediaLabel(
                          draft.mediaType ||
                            mediaTypeForApp(draft.import?.app === "sonarr" ? "sonarr" : "radarr"),
                        )}
                      </Badge>
                      {draft.import ? (
                        <Badge variant="outline">
                          {draft.import.source === "dictionarry"
                            ? "Profilarr (Dictionarry)"
                            : "TRaSH Guides"}
                        </Badge>
                      ) : null}
                    </span>
                  ) : (
                    <Segmented
                      ariaLabel={t("Applies to")}
                      size="sm"
                      value={profileMediaType(draft) as ProfileMedia}
                      options={MEDIA_OPTIONS}
                      onChange={changeApplies}
                    />
                  )
                }
              />
              <SettingsRow
                title={t("Default for")}
                description={t(
                  "Used when no device rule matches for {media}. With no profile for Away, people away from home get the At home profile.",
                  { media: mediaLabel(draftScope || media) },
                )}
                control={
                  <ChipGroup
                    ariaLabel={t("Default for")}
                    size="sm"
                    options={[
                      { value: "home", label: t("At home") },
                      { value: "away", label: t("Away") },
                    ]}
                    value={[
                      ...(defaultFor.home ? ["home" as const] : []),
                      ...(defaultFor.away ? ["away" as const] : []),
                    ]}
                    onChange={(next) =>
                      setDefaultFor({
                        home: next.includes("home"),
                        away: next.includes("away"),
                      })
                    }
                  />
                }
              />
            </SettingsSection>

            <SettingsSection
              title={t("Never play")}
              description={
                draft.fallbackBelowMin
                  ? t(
                      "When no release meets this profile, allow lower resolutions, including qualities excluded by an imported profile. Other limits still apply.",
                    )
                  : t("Releases outside these limits are skipped.")
              }
            >
              <SettingsRow
                title={t("Resolution")}
                description={
                  errors.minHeight || errors.maxHeight ? (
                    <span className="text-danger">{errors.minHeight || errors.maxHeight}</span>
                  ) : draft.fallbackBelowMin ? (
                    t(
                      "When no release meets this profile, allow lower resolutions, including qualities excluded by an imported profile. Other limits still apply.",
                    )
                  ) : (
                    t("Lowest and highest acceptable.")
                  )
                }
                control={
                  <span className="flex items-center gap-2">
                    <span className="w-32">
                      <SearchSelect
                        {...searchSelectLabels()}
                        value={String(draft.minHeight)}
                        onChange={(v) => patch({ minHeight: Number(v) })}
                        options={RESOLUTIONS}
                        ariaLabel={t("Minimum resolution")}
                        searchable={false}
                      />
                    </span>
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <span className="w-32">
                      <SearchSelect
                        {...searchSelectLabels()}
                        value={String(draft.maxHeight)}
                        onChange={(v) => patch({ maxHeight: Number(v) })}
                        options={RESOLUTIONS}
                        ariaLabel={t("Maximum resolution")}
                        searchable={false}
                      />
                    </span>
                  </span>
                }
              />
              <SettingsRow
                title={t("Fall back below minimum resolution")}
                description={t(
                  "When no release meets this profile, allow lower resolutions, including qualities excluded by an imported profile. Other limits still apply.",
                )}
                control={
                  <Switch
                    aria-label={t("Fall back below minimum resolution")}
                    checked={draft.fallbackBelowMin ?? false}
                    onChange={(e) => patch({ fallbackBelowMin: e.currentTarget.checked })}
                  />
                }
              />
              <div className="grid gap-6 border-t border-border px-4 py-3 sm:grid-cols-2">
                <fieldset className="min-w-0 space-y-2">
                  <legend className="text-sm font-medium text-fg">{t("Movies")}</legend>
                  {errors.minSizeMbPerMin || errors.maxSizeMbPerMin ? (
                    <p className="text-xs text-danger">
                      {errors.minSizeMbPerMin || errors.maxSizeMbPerMin}
                    </p>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      {t("Lowest and highest MB per minute for a film. 0 means no limit.")}
                    </p>
                  )}
                  <span className="flex flex-wrap items-center gap-2">
                    <NumberInput
                      aria-label={t("Minimum MB per minute")}
                      value={draft.minSizeMbPerMin}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ minSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <NumberInput
                      aria-label={t("Maximum MB per minute")}
                      value={draft.maxSizeMbPerMin}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ maxSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">MB</span>
                  </span>
                </fieldset>
                <fieldset className="min-w-0 space-y-2">
                  <legend className="text-sm font-medium text-fg">{t("TV episodes")}</legend>
                  {errors.episodeMinSizeMbPerMin || errors.episodeMaxSizeMbPerMin ? (
                    <p className="text-xs text-danger">
                      {errors.episodeMinSizeMbPerMin || errors.episodeMaxSizeMbPerMin}
                    </p>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      {t("Optional override for TV episodes. 0 uses the movie limits.")}
                    </p>
                  )}
                  <span className="flex flex-wrap items-center gap-2">
                    <NumberInput
                      aria-label={t("Episode minimum MB per minute")}
                      value={draft.episodeMinSizeMbPerMin ?? 0}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ episodeMinSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <NumberInput
                      aria-label={t("Episode maximum MB per minute")}
                      value={draft.episodeMaxSizeMbPerMin ?? 0}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ episodeMaxSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">MB</span>
                  </span>
                </fieldset>
              </div>
              <BoolRow
                label={t("Allow HDR")}
                value={draft.allowHdr}
                help={t("Only used when the app supports it.")}
                onChange={(v) => patch({ allowHdr: v })}
              />
              <BoolRow
                label={t("Allow Dolby Vision")}
                value={draft.allowDolbyVision}
                onChange={(v) => patch({ allowDolbyVision: v })}
              />
              <BoolRow
                label={t("Allow CAM and screener releases")}
                value={draft.allowCam}
                onChange={(v) => patch({ allowCam: v })}
              />
              <FilterRulesEditor
                profile={draft}
                onChange={(next) => patch(next)}
                error={errors.releaseRules}
              />
            </SettingsSection>

            <QualityOrderSection draft={draft} patch={patch} />

            <CustomFormatsSection
              draft={draft}
              patch={patch}
              resolver={resolver}
              formatsText={formatsText}
              onFormatsText={setFormatsText}
              revealed={false}
            />
          </>
        ) : null}
      </RecordDrawer>
    </>
  );
}
