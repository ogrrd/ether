import { Button, SettingsSection, toast } from "@braedonsaunders/appkit-ui";
import { RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";
import { useAppOptional } from "../../app-context";
import { CopyButton } from "../../components/CopyButton";
import { ErrorPanel, LoadingInline, LoadingRows } from "../../components/feedback";
import { LevelBadge } from "../../components/status";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn } from "../../i18n";
import { api, ApiError, errorMessage } from "../../lib/api";
import { formatDateTime, isPlainObject, parseTime, relativeTime } from "../../lib/format";
import { plexDirectHealth, remoteHealth } from "../../lib/health";
import { useAsync } from "../../lib/hooks";
import { phaseText } from "../../lib/startup-text";
import type { StartupPhase } from "../../lib/ops";
import type { UpdateStatus } from "../../lib/types";
import { SecureAddresses } from "./SecureAddresses";

/**
 * Settings helpers shared with the Settings page. Usenet providers and indexers
 * now live in pages/sources, quality in pages/releases, cache and transcoding
 * status in pages/playback.
 */

// ---- Status cards -----------------------------------------------------------------------

/** Remote-access and plex.direct health for the Plex app page. Account sign-in lives on that page, not here. */
export function PlexStatus({ remote }: { remote: unknown }) {
  const app = useAppOptional();
  const [syncingFriends, setSyncingFriends] = useState(false);
  const r = remoteHealth(remote);
  const c = plexDirectHealth(remote);
  const claimed = isPlainObject(remote) && remote.claimed === true;
  const friendSyncError =
    isPlainObject(remote) && typeof remote.friendSyncError === "string"
      ? remote.friendSyncError
      : "";
  const friends = isPlainObject(remote) && typeof remote.friends === "number" ? remote.friends : 0;
  const lastFriendSync = isPlainObject(remote) ? parseTime(remote.lastFriendSync) : null;
  const conns =
    isPlainObject(remote) && Array.isArray(remote.publishedUris)
      ? remote.publishedUris.filter((uri): uri is string => typeof uri === "string")
      : [];
  const syncFriends = async () => {
    setSyncingFriends(true);
    try {
      await api.syncPlexFriends();
      void app?.refreshDashboard();
      toast.success(t("Shared users synced"));
    } catch (e) {
      void app?.refreshDashboard();
      toast.error(errorMessage(e));
    } finally {
      setSyncingFriends(false);
    }
  };
  return (
    <SettingsSection title={t("Status")}>
      <SettingsRow
        title={t("Remote access")}
        description={r.details.join(" · ") || r.summary}
        control={<LevelBadge level={r.level} label={r.summary} />}
      />
      {claimed ? (
        <SettingsRow
          title={t("Shared users")}
          description={
            friendSyncError
              ? friendSyncError
              : lastFriendSync
                ? `${tn(friends, "{count} shared user", "{count} shared users")} · ${t("Last sync {when}", { when: relativeTime(lastFriendSync) })}`
                : t("Not synced yet")
          }
          control={
            <Button
              variant="outline"
              size="sm"
              disabled={syncingFriends}
              onClick={() => void syncFriends()}
            >
              <RefreshCw size={14} /> {syncingFriends ? t("Syncing…") : t("Sync shared users")}
            </Button>
          }
        />
      ) : null}
      <SettingsRow
        title={t("plex.direct certificate")}
        description={c.details.join(" · ") || c.summary}
        control={<LevelBadge level={c.level} label={c.statusLabel ?? c.summary} />}
      />
      <SecureAddresses remote={remote} />
      {conns.length ? (
        <SettingsRow
          stacked
          title={t("Published addresses")}
          description={t(
            "These are registration addresses. Plex may provide different HTTPS addresses to apps.",
          )}
        >
          <ul className="space-y-1 text-xs">
            {conns.map((uri, i) => (
              <li key={i} className="flex items-center gap-2">
                <code className="truncate text-fg-muted">{uri}</code>
              </li>
            ))}
          </ul>
        </SettingsRow>
      ) : null}
    </SettingsSection>
  );
}

// ---- Updates -----------------------------------------------------------------------------

const UPDATE_STATE: Record<UpdateStatus["state"], string> = localized(() => ({
  idle: t("Idle"),
  downloading: t("Downloading"),
  staged: t("Downloaded"),
  "waiting-for-window": t("Waiting for maintenance window"),
  "waiting-for-idle": t("Waiting until nobody is watching"),
  "waiting-for-rollout": t("Rolling out gradually"),
  "waiting-for-turn": t("Waiting for another server to finish updating"),
  draining: t("Waiting for playback on this server to finish"),
  applying: t("Installing — Ether will restart"),
  "rolled-back": t("Rolled back"),
}));

/** When a self-applying install will put the available update in, in plain words. */
function installPlanText(s: UpdateStatus): string | null {
  const a = s.available;
  if (!a || s.capability !== "self-apply" || s.state === "applying") return null;
  if (s.requested) return null; // the toast and state already say it
  if (!s.autoUpdate) return t("Installs when you choose Update now.");
  if (a.critical) return t("Security fix: installs the next time nobody is watching.");
  if (a.installAfter)
    return t(
      "New releases reach servers a few at a time. This one installs on its own after {time}, at night once nobody is watching.",
      { time: formatDateTime(a.installAfter) },
    );
  if (a.catchUp)
    return t(
      "Someone has watched every night lately, so it installs the next time nobody watches for half an hour.",
    );
  return t("Installs tonight between {start} and {end}, once nobody is watching.", {
    start: s.window.start,
    end: s.window.end,
  });
}

function CommandLine({ command }: { command: string }) {
  return (
    <span className="mt-1 flex items-center gap-2">
      <code className="truncate rounded bg-bg-subtle px-2 py-1 text-xs">{command}</code>
      <CopyButton value={command} label={t("Copy command")} />
    </span>
  );
}

/** The exact way to update an install that cannot update itself. */
function ManualUpdate({ s }: { s: UpdateStatus }) {
  const a = s.available;
  if (!a) return null;
  const download =
    a.downloadUrl && a.downloadName ? (
      <a className="underline" href={a.downloadUrl} target="_blank" rel="noreferrer">
        {t("Download {file}", { file: a.downloadName })}
      </a>
    ) : null;
  const tag = s.channel === "beta" ? a.version : "latest";
  switch (s.notifyKind) {
    case "container":
      return (
        <span className="block">
          {t(
            "Ether runs in a container, so it updates when you pull the new image. With Docker Compose:",
          )}
          <CommandLine command="docker compose pull && docker compose up -d" />
          <span className="mt-1 block">
            {t("With plain Docker, pull the image, then recreate the container:")}
          </span>
          <CommandLine command={`docker pull ghcr.io/braedonsaunders/ether:${tag}`} />
          <span className="mt-1 block">
            {t("Watchtower can pull new images for you automatically.")}
          </span>
        </span>
      );
    case "macos":
      return (
        <span className="block">
          {t("Open the new version's disk image and drag Ether to Applications to update.")}{" "}
          {download}
        </span>
      );
    case "deb":
    case "rpm":
      return (
        <span className="block">
          {t(
            "Ether was installed with your package manager. Download the new package, then install it:",
          )}{" "}
          {download}
          {a.downloadName ? (
            <CommandLine
              command={
                s.notifyKind === "deb"
                  ? `sudo apt install ./${a.downloadName}`
                  : `sudo dnf install ./${a.downloadName}`
              }
            />
          ) : null}
        </span>
      );
    case "package":
      return (
        <span className="block">
          {t("Update Ether with the package manager that installed it.")}
        </span>
      );
    case "unsupervised":
      return (
        <span className="block">
          {t(
            "Ether isn't running as a service, so nothing would restart it after an update. Install the new version by hand, or run Ether as a service to update automatically.",
          )}{" "}
          {download}
        </span>
      );
    case "unwritable":
      return (
        <span className="block">
          {t(
            "The Ether service can't write to its install folder, so install the new version by hand.",
          )}{" "}
          {download}
        </span>
      );
    case "installer":
      return (
        <span className="block">
          {t("Run the new version's installer to update.")} {download}
        </span>
      );
    case "dev":
      return (
        <span className="block">{t("Development build: build the new version from source.")}</span>
      );
    default:
      return s.capabilityReason ? (
        <span className="block">
          {t("Install manually: {reason}.", { reason: s.capabilityReason })}
        </span>
      ) : null;
  }
}

export function UpdatesStatus() {
  const status = useAsync(() => api.updateStatus(), []);
  const [busy, setBusy] = useState(false);
  const s = status.data;
  const starting =
    status.error instanceof ApiError &&
    status.error.code === "starting" &&
    status.error.phase !== "failed";
  const reconnecting =
    status.error instanceof ApiError &&
    status.error.status === 0 &&
    (s?.state === "applying" || s?.requested === true);
  useEffect(() => {
    if (!starting && !reconnecting) return;
    const timer = setInterval(() => void status.reload(), 5000);
    return () => clearInterval(timer);
  }, [starting, reconnecting, status.reload]);
  useEffect(() => {
    if (!s?.enabled) return;
    void api
      .checkUpdates()
      .then(status.setData)
      .catch(() => {});
    // Check once on entry; subsequent checks are manual.
  }, [s?.enabled]);
  useEffect(() => {
    if (!s || !(s.checking || s.requested || s.state === "downloading" || s.state === "applying"))
      return;
    const timer = setInterval(() => void status.reload(), 5000);
    return () => clearInterval(timer);
  }, [s?.checking, s?.requested, s?.state]);
  if (starting) {
    const phase = (status.error as ApiError).phase as StartupPhase;
    const text = phaseText({
      phase,
      ready: false,
      message: "",
      started: "",
      since: "",
      elapsedSeconds: 0,
    });
    return (
      <div>
        <LoadingInline label={text.title} />
        <p>{text.detail}</p>
      </div>
    );
  }
  if (reconnecting) return <LoadingInline label={t("Starting Ether")} />;
  if (status.error) return <ErrorPanel error={status.error} onRetry={() => void status.reload()} />;
  if (!s) return <LoadingRows rows={2} />;

  const updateNow = async (force: boolean) => {
    setBusy(true);
    try {
      await api.updateNow(force);
      toast.success(
        force ? t("Installing update now") : t("Update will install once nobody is watching"),
      );
      await status.reload();
    } catch (e) {
      toast.error(t("Couldn't start the update"), { description: (e as Error).message });
    } finally {
      setBusy(false);
    }
  };
  const canApply = s.capability === "self-apply" && s.state !== "applying";
  let availableText = t("Ether is up to date");
  if (s.available)
    availableText = t("Version {version} — {state}", {
      version: s.available.version,
      state: UPDATE_STATE[s.state],
    });
  else if (s.state === "rolled-back" && s.failedVersion)
    availableText = t("Version {version} was rolled back and will be skipped", {
      version: s.failedVersion,
    });
  else if (!s.enabled) availableText = s.disabledReason || t("Updates are disabled in this build");
  else if (s.lastError) availableText = s.lastError;
  else if (!s.lastCheck) availableText = t("Not checked yet");

  const dismissInstalled = async () => {
    try {
      status.setData(await api.dismissInstalledUpdate());
    } catch (e) {
      toast.error((e as Error).message);
    }
  };
  const plan = installPlanText(s);

  return (
    <SettingsSection title={t("Status")}>
      {s.installed ? (
        <SettingsRow
          title={t("Updated to {version}", { version: s.installed.to })}
          description={t("Ether installed it by itself on {time}, while nobody was watching.", {
            time: formatDateTime(s.installed.at),
          })}
          control={
            <Button size="sm" variant="outline" onClick={() => void dismissInstalled()}>
              {t("Dismiss")}
            </Button>
          }
        />
      ) : null}
      <SettingsRow
        title={t("Installed")}
        description={
          s.confirming
            ? t("Version {version} — confirming the update", { version: s.current })
            : t("Version {version}", { version: s.current })
        }
        control={
          s.state === "rolled-back" ? (
            <LevelBadge level="warn" label={t("Rolled back")} />
          ) : (
            <LevelBadge
              level={s.lastError ? "warn" : "ok"}
              label={s.lastError ? t("Issue") : t("OK")}
            />
          )
        }
      />
      <SettingsRow
        title={t("Available")}
        description={
          <>
            {availableText}
            {s.available?.notesUrl ? (
              <>
                {" · "}
                <a
                  className="underline"
                  href={s.available.notesUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  {t("Release notes")}
                </a>
              </>
            ) : null}
            {plan ? <span className="block">{plan}</span> : null}
            {s.capability === "notify-only" ? <ManualUpdate s={s} /> : null}
          </>
        }
        control={
          s.available && canApply ? (
            s.requested && s.state === "waiting-for-idle" ? (
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                onClick={() => void updateNow(true)}
              >
                {t("Install now anyway")}
              </Button>
            ) : (
              <Button
                size="sm"
                disabled={busy || s.requested}
                onClick={() => void updateNow(false)}
              >
                {t("Update now")}
              </Button>
            )
          ) : null
        }
      />
      {s.capability === "self-apply" ? (
        <SettingsRow
          title={t("Automatic updates")}
          description={
            s.autoUpdate
              ? t("Ether installs updates by itself at night, once nobody is watching.")
              : t(
                  "Off. Turn on “Install updates automatically” below to get fixes overnight, while nobody is watching.",
                )
          }
          control={
            <LevelBadge
              level={s.autoUpdate ? "ok" : "off"}
              label={s.autoUpdate ? t("On") : t("Off")}
            />
          }
        />
      ) : null}
      <SettingsRow
        title={t("Maintenance window")}
        description={`${
          s.window.open
            ? t("{start}–{end} local time (open now)", { start: s.window.start, end: s.window.end })
            : t("{start}–{end} local time", { start: s.window.start, end: s.window.end })
        } · ${tn(s.activeSessions, "{count} active stream", "{count} active streams")}`}
      />
      <SettingsRow
        title={t("Last check")}
        control={
          <Button
            size="sm"
            variant="outline"
            disabled={!s.enabled || busy || s.checking}
            onClick={() => {
              setBusy(true);
              void api
                .checkUpdates()
                .then(status.setData)
                .catch((e) => toast.error((e as Error).message))
                .finally(() => setBusy(false));
            }}
          >
            {s.checking ? t("Checking…") : t("Check for updates")}
          </Button>
        }
        description={
          <>
            {s.lastError
              ? s.lastError
              : s.lastCheck
                ? relativeTime(s.lastCheck)
                : s.enabled
                  ? t("Not checked yet")
                  : t("Never")}
            {s.enabled && s.nextCheck && !s.checking ? (
              <span className="block">
                {t("Next check: {time}", { time: formatDateTime(s.nextCheck) })}
              </span>
            ) : null}
          </>
        }
      />
    </SettingsSection>
  );
}
