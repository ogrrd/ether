import { toast } from "@braedonsaunders/appkit-ui";
import { useCallback } from "react";
import { useApp } from "../../app-context";
import { api } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import { PROFILE_MEDIA, type ProfileMedia } from "../../lib/releases";
import type { EtherConfig, QualityRule } from "../../lib/types";

export type Quality = EtherConfig["quality"];

const HOME_FIELD = { movie: "defaultMovie", tv: "defaultTv" } as const;

/**
 * Repoints any client rules (other than `keepIndex`) whose profile no longer
 * serves their media to that media's At home profile, keeping device matchers
 * and limits intact so unrelated dangling rules never block a quality save.
 */
export function settleQualityRules(
  quality: Quality,
  keepIndex = -1,
): { rules: QualityRule[]; keptAt: number } {
  const profiles = quality.profiles ?? [];
  const rules = quality.rules ?? [];
  if (profiles.length === 0) return { rules, keptAt: keepIndex };

  const serves = (name: string, media?: string) =>
    Boolean(name) && profiles.some((p) => p.name === name && (!media || p.mediaType === media));

  const homeFor = (media: ProfileMedia): string => {
    const scoped = quality[HOME_FIELD[media]];
    if (scoped && serves(scoped, media)) return scoped;
    if (quality.default && serves(quality.default, media)) return quality.default;
    return profiles.find((p) => p.mediaType === media)?.name ?? "";
  };

  let keptAt = keepIndex;
  const out: QualityRule[] = [];
  for (let i = 0; i < rules.length; i++) {
    const r = rules[i]!;
    if (i === keepIndex) {
      keptAt = out.length;
      out.push(r);
      continue;
    }
    const media = r.mediaType ?? "";
    if ((media && media !== "movie" && media !== "tv") || !r.profile || serves(r.profile, media)) {
      out.push(r);
      continue;
    }
    if (media === "movie" || media === "tv") {
      const to = homeFor(media);
      if (to) out.push({ ...r, profile: to });
      continue;
    }
    const targets = PROFILE_MEDIA.flatMap((m) => {
      const to = homeFor(m);
      return to ? [{ media: m, to }] : [];
    });
    const shared =
      targets.length === PROFILE_MEDIA.length &&
      targets.every((t) => t.to === targets[0]!.to && serves(t.to, t.media))
        ? targets[0]!.to
        : "";
    if (shared) {
      out.push({ ...r, profile: shared });
    } else {
      for (const t of targets) out.push({ ...r, mediaType: t.media, profile: t.to });
    }
  }
  return { rules: out, keptAt };
}

/** The quality section (profiles, client rules, defaults) with a save that reloads it. */
export function useQuality() {
  const { refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const quality = config.data?.quality ?? null;
  const commit = useCallback(
    async (next: Quality, message: string) => {
      await api.putSection("quality", next);
      toast.success(message);
      await config.reload();
      void refreshDashboard();
    },
    [config, refreshDashboard],
  );
  /** Reload after a change saved elsewhere, such as a profile import. */
  const refresh = useCallback(async () => {
    await config.reload();
    void refreshDashboard();
  }, [config, refreshDashboard]);
  return { config, quality, commit, refresh };
}
