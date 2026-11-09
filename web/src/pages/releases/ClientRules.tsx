import { QualityHelp } from "./QualityHelp";
import {
  Badge,
  Button,
  EmptyState,
  Input,
  SearchSelect,
  SettingsSection,
  Switch,
  confirmDialog,
} from "@braedonsaunders/appkit-ui";
import { ListFilter, Plus } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { Section } from "../../components/layout";
import { useViewers, viewerName } from "../../components/pickers";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t } from "../../i18n";
import type { Viewer } from "../../lib/contract";
import {
  MEDIA_MOVIE,
  MEDIA_TV,
  mediaApplies,
  mediaLabel,
  ruleProfileNames,
  profileMediaType,
} from "../../lib/releases";
import type { QualityRule } from "../../lib/types";
import {
  OrderButtons,
  RecordDrawer,
  RecordItem,
  recordErrors,
  swap,
  toastError,
  type Errors,
} from "../sources/form";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { settleQualityRules, useQuality } from "./useQuality";

const DEVICES = [
  "Plex for Apple TV",
  "Plex Web",
  "Plex for iOS",
  "Plex for Android",
  "Roku",
  "Fire TV",
  "LG",
  "Samsung",
  "Xbox",
  "PlayStation",
];
const CUSTOM = "__custom__";

const RESOLUTIONS = localized(() => [
  { value: "0", label: t("Any") },
  { value: "480", label: "480p" },
  { value: "720", label: "720p" },
  { value: "1080", label: "1080p" },
  { value: "2160", label: t("4K (2160p)") },
]);

const APPLIES_OPTIONS = localized(() => [
  { value: "" as const, label: t("Both") },
  { value: MEDIA_MOVIE as "movie", label: t("Movies") },
  { value: MEDIA_TV as "tv", label: t("Shows") },
]);

const FILTER_OPTIONS = localized(() => [
  { value: "" as const, label: t("All") },
  { value: MEDIA_MOVIE as "movie", label: t("Movies") },
  { value: MEDIA_TV as "tv", label: t("Shows") },
]);

const resolutionLabel = (h: number) =>
  RESOLUTIONS.find((x) => x.value === String(h))?.label ?? `${h}p`;

function ruleViewer(r: QualityRule, viewers: Viewer[] | null) {
  if (!r.userId) return "";
  const v = viewers?.find((x) => x.id === r.userId);
  return v ? viewerName(v) : t("Unavailable viewer");
}

function ruleSummary(r: QualityRule, viewers: Viewer[] | null) {
  return [
    r.protocol,
    r.product || t("Any device"),
    r.platform,
    ruleViewer(r, viewers),
    r.maxHeight ? resolutionLabel(r.maxHeight) : "",
  ]
    .filter(Boolean)
    .join(" · ");
}

/** Rules: the first rule matching a viewer, app or device picks its quality profile. */
export function ClientRulesTab() {
  const { config, quality, commit } = useQuality();
  const { viewers } = useViewers();
  const [editing, setEditing] = useState<{ index: number; draft: QualityRule } | null>(null);
  const [filter, setFilter] = useState<"" | "movie" | "tv">("");
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!quality) return <LoadingRows rows={3} />;

  const rules = quality.rules ?? [];
  const profiles = quality.profiles ?? [];
  const shown = rules
    .map((rule, index) => ({ rule, index }))
    .filter(({ rule }) => mediaApplies(rule.mediaType, filter || undefined));

  const open = (index: number, draft: QualityRule) => {
    const compatible = ruleProfileNames(profiles, draft.mediaType);
    const copy = { ...draft };
    if (!compatible.includes(copy.profile) && compatible.length) copy.profile = compatible[0]!;
    setErrors({});
    setError(null);
    setEditing({ index, draft: copy });
  };

  /** Change a rule's media and point its profile at a compatible one when needed. */
  function setMedia(media: "" | "movie" | "tv") {
    setEditing((e) => {
      if (!e) return e;
      const draft = { ...e.draft, mediaType: media || undefined };
      const compatible = ruleProfileNames(profiles, media);
      if (!compatible.includes(draft.profile)) draft.profile = compatible[0] ?? "";
      return { ...e, draft };
    });
  }

  async function save() {
    if (!editing || !quality) return;
    const { index, draft } = editing;
    const list =
      index < rules.length ? rules.map((r, j) => (j === index ? draft : r)) : [...rules, draft];
    const { rules: settled, keptAt } = settleQualityRules({ ...quality, rules: list }, index);
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await commit(
        { ...quality, rules: settled },
        index < rules.length ? t("Rule saved") : t("Rule added"),
      );
      setEditing(null);
    } catch (e) {
      setError(e);
      setErrors(recordErrors(e, `quality.rules[${keptAt}].`));
    } finally {
      setSaving(false);
    }
  }

  async function remove() {
    if (!editing || !quality) return;
    const { index } = editing;
    const ok = await confirmDialog({
      title: t("Remove rule?"),
      message: t("Apps it matched will use the next matching rule, or the defaults."),
      confirmLabel: t("Remove"),
      tone: "danger",
    });
    if (!ok) return;
    setSaving(true);
    setError(null);
    try {
      const remaining = rules.filter((_, j) => j !== index);
      const { rules: settled } = settleQualityRules({ ...quality, rules: remaining });
      await commit({ ...quality, rules: settled }, t("Rule removed"));
      setEditing(null);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  async function move(i: number, d: -1 | 1) {
    if (!quality) return;
    try {
      const swapped = swap(rules, i, i + d);
      const { rules: settled } = settleQualityRules({ ...quality, rules: swapped });
      await commit({ ...quality, rules: settled }, t("Rule order saved"));
    } catch (e) {
      toastError(t("Couldn't reorder rules"), e);
    }
  }

  const draft = editing?.draft;
  const isNew = editing ? editing.index >= rules.length : false;
  const set = (patch: Partial<QualityRule>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...patch } } : e));
  const custom =
    draft?.product !== undefined && !DEVICES.includes(draft.product) && draft.product !== "";

  return (
    <>
      <Section
        title={t("Rules")}
        description={
          <QualityHelp
            summary={t("The first matching rule picks the profile.")}
            detail={t(
              "When no rule matches, the At home or Away default applies. Device capabilities still apply before playback.",
            )}
          />
        }
        actions={
          <>
            {rules.length ? (
              <Segmented
                ariaLabel={t("Show rules for")}
                size="sm"
                value={filter}
                options={FILTER_OPTIONS}
                onChange={setFilter}
              />
            ) : null}
            <Button
              size="sm"
              onClick={() =>
                open(rules.length, {
                  profile: quality.default || profiles[0]?.name || "",
                  mediaType: filter || undefined,
                })
              }
              disabled={!profiles.length}
            >
              <Plus size={14} /> {t("Add rule")}
            </Button>
          </>
        }
        bodyClassName=""
      >
        {shown.length === 0 ? (
          <EmptyState
            icon={<ListFilter />}
            title={rules.length ? t("No matching rules") : t("No rules")}
            description={
              rules.length
                ? t("No rule applies to {media}. Choose All to see every rule.", {
                    media: mediaLabel(filter || undefined),
                  })
                : t("Every app uses the default quality profiles.")
            }
          />
        ) : (
          <ul className="divide-y divide-border">
            {shown.map(({ rule: r, index: i }) => (
              <RecordItem
                key={i}
                title={`${i + 1}. ${ruleSummary(r, viewers)}`}
                description={t("Uses {profile}", { profile: r.profile || t("no profile") })}
                meta={
                  <Badge variant={r.mediaType ? "info" : "secondary"}>
                    {mediaLabel(r.mediaType)}
                  </Badge>
                }
                order={
                  filter ? undefined : (
                    <OrderButtons
                      label={t("rule {n}", { n: i + 1 })}
                      i={i}
                      n={rules.length}
                      onMove={(d) => void move(i, d)}
                    />
                  )
                }
                editLabel={t("Edit rule {n}", { n: i + 1 })}
                onEdit={() => open(i, r)}
              />
            ))}
          </ul>
        )}
      </Section>
      <p className="text-sm text-fg-muted">
        <Link className="text-primary underline" to="/playback/devices">
          {t("View device capabilities")}
        </Link>
      </p>

      <RecordDrawer
        open={editing !== null}
        onClose={() => !saving && setEditing(null)}
        title={isNew ? t("New rule") : t("Rule {n}", { n: (editing?.index ?? 0) + 1 })}
        description={t("Match an app by device, platform or viewer.")}
        onSave={() => void save()}
        saving={saving}
        saveLabel={t("Save rule")}
        onRemove={isNew ? undefined : () => void remove()}
        removeLabel={t("Remove rule")}
        error={error}
      >
        {draft ? (
          <SettingsSection title={t("Match")}>
            <SettingsRow
              title={t("Applies to")}
              description={t("Which titles this rule can choose a profile for.")}
              control={
                <Segmented
                  ariaLabel={t("Applies to")}
                  size="sm"
                  value={profileMediaType(draft) as "" | "movie" | "tv"}
                  options={APPLIES_OPTIONS}
                  onChange={setMedia}
                />
              }
            />
            <SettingsRow
              title={t("Protocol")}
              description={t("Choose which kind of app this rule matches.")}
              control={
                <div className="w-56">
                  <SearchSelect
                    value={draft.protocol ?? ""}
                    onChange={(v) => set({ protocol: v || undefined })}
                    options={[
                      { value: "", label: t("Any protocol") },
                      { value: "plex", label: "Plex" },
                      { value: "jellyfin", label: "Jellyfin" },
                      { value: "emby", label: "Emby" },
                      { value: "silo", label: "Silo" },
                      { value: "stremio", label: "Stremio" },
                      { value: "webdav", label: t("Files") },
                      { value: "dlna", label: "DLNA" },
                    ]}
                    ariaLabel={t("Protocol")}
                    searchable={false}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Device")}
              description={t("The app’s product name.")}
              control={
                <div className="w-56">
                  <SearchSelect
                    {...searchSelectLabels()}
                    value={custom ? CUSTOM : (draft.product ?? "")}
                    onChange={(v) =>
                      set({ product: v === CUSTOM ? "Custom device" : v || undefined })
                    }
                    options={[
                      { value: "", label: t("Any device") },
                      ...DEVICES.map((v) => ({ value: v, label: v })),
                      { value: CUSTOM, label: t("Custom product name…") },
                    ]}
                    ariaLabel={t("Device")}
                  />
                </div>
              }
            />
            {custom ? (
              <SettingsRow
                title={t("Product name contains")}
                control={
                  <Input
                    value={draft.product ?? ""}
                    onChange={(e) => set({ product: e.currentTarget.value })}
                    aria-label={t("Custom product")}
                    className="sm:w-56"
                  />
                }
              />
            ) : null}
            <SettingsRow
              title={t("Platform")}
              description={t("e.g. tvOS, Android. Empty matches any.")}
              control={
                <Input
                  value={draft.platform ?? ""}
                  placeholder={t("Any")}
                  onChange={(e) => set({ platform: e.currentTarget.value || undefined })}
                  aria-label={t("Platform")}
                  className="sm:w-56"
                />
              }
            />
            <SettingsRow
              title={t("Viewer")}
              control={
                <div className="w-56">
                  <SearchSelect
                    {...searchSelectLabels()}
                    value={String(draft.userId ?? 0)}
                    onChange={(v) => set({ userId: Number(v) || undefined })}
                    options={[
                      { value: "0", label: t("Everyone") },
                      ...(viewers ?? []).map((v) => ({
                        value: String(v.id),
                        label: viewerName(v),
                      })),
                    ]}
                    ariaLabel={t("Viewer")}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Quality profile")}
              description={
                errors.profile ? (
                  <span className="text-danger">{errors.profile}</span>
                ) : draft.mediaType ? undefined : (
                  t("Movies use the movie profile with this name, and shows the show one.")
                )
              }
              control={
                <div className="w-56">
                  <SearchSelect
                    {...searchSelectLabels()}
                    value={draft.profile}
                    onChange={(v) => set({ profile: v })}
                    options={ruleProfileNames(profiles, draft.mediaType).map((name) => ({
                      value: name,
                      label: name,
                    }))}
                    ariaLabel={t("Profile")}
                    searchable={false}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Quality limit")}
              description={
                errors.maxHeight ? (
                  <span className="text-danger">{errors.maxHeight}</span>
                ) : (
                  t("Caps this device below its quality profile. Any uses the profile's ceiling.")
                )
              }
              control={
                <div className="w-56">
                  <SearchSelect
                    {...searchSelectLabels()}
                    value={String(draft.maxHeight ?? 0)}
                    onChange={(v) => set({ maxHeight: Number(v) || undefined })}
                    options={RESOLUTIONS}
                    ariaLabel={t("Quality limit")}
                    searchable={false}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Convert Dolby and DTS audio to AAC")}
              description={t("For free Infuse and other players that can't play surround formats.")}
              control={
                <Switch
                  checked={Boolean(draft.convertSurroundAudio)}
                  onChange={(e) =>
                    set({ convertSurroundAudio: e.currentTarget.checked || undefined })
                  }
                  aria-label={t("Convert Dolby and DTS audio to AAC")}
                />
              }
            />
          </SettingsSection>
        ) : null}
      </RecordDrawer>
    </>
  );
}
