import {
  Badge,
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@braedonsaunders/appkit-ui";
import { ChevronLeft, ChevronRight, Film, Pin, Search } from "lucide-react";
import { Fragment, useState } from "react";
import { getLocale, localized, t, tn } from "../../i18n";
import { formatBytes, formatPercent } from "../../lib/format";
import type { CacheSnapshot } from "./Cache";
import { causeDescription, causeLabel, pinLabel, retentionLabel } from "./reasons";

const FILTERS = localized(
  () =>
    [
      ["all", t("All releases")],
      ["watching", t("Watching now")],
      ["paused", t("Paused")],
      ["unfinished", t("Unfinished")],
      ["shelf", t("Shelf")],
      ["prefetched", t("Prefetched")],
      ["checked", t("Checked")],
      ["owner", t("Kept by you")],
      ["unexplained", t("Unexplained")],
      ["reclaimable", t("Reclaimable")],
    ] as const,
);

const PREFETCHED = [
  "next-episode",
  "post-play",
  "resume-warm",
  "detail-warm",
  "trailer-warm",
  "browse-warm",
];

function matchesFilter(r: CacheSnapshot["releases"][number], filter: string): boolean {
  const codes = new Set((r.reason?.causes ?? []).map((c) => c.code));
  const pins = new Set(r.Pins ?? []);
  switch (filter) {
    case "watching":
      return codes.has("playing") || pins.has("playback:live");
    case "paused":
      return codes.has("paused");
    case "unfinished":
      return codes.has("unfinished") || pins.has("playback:inprogress");
    case "shelf":
      return codes.has("shelf") || pins.has("preloaded");
    case "prefetched":
      return PREFETCHED.some((code) => codes.has(code));
    case "checked":
      return codes.has("quality-check");
    case "owner":
      return pins.has("owner");
    case "unexplained":
      return codes.size === 0 || codes.has("unrecorded");
    case "reclaimable":
      return (
        pins.size === 0 &&
        !codes.has("playing") &&
        !codes.has("paused") &&
        r.reason?.retention.kind !== "pinned"
      );
    default:
      return true;
  }
}

function dateLabel(value: string): string {
  return value && !value.startsWith("0001")
    ? new Date(value).toLocaleDateString(getLocale(), { month: "short", day: "numeric" })
    : "—";
}

// A release id that reached the title field is a hash, not something a person names.
function isOpaque(value: string): boolean {
  return /^[0-9a-f]{24,}$/i.test(value.replace(/\.[a-z0-9]{2,5}$/i, ""));
}

function friendlyName(r: CacheSnapshot["releases"][number]): string {
  const title = r.Title?.trim();
  if (title && !isOpaque(title)) return title;
  const file = r.reason?.file?.trim();
  if (file && !isOpaque(file)) return file;
  const source = r.reason?.source?.trim();
  if (source && !isOpaque(source)) return source;
  const codes = new Set((r.reason?.causes ?? []).map((c) => c.code));
  if (codes.has("quality-check") || codes.has("repair")) return t("Cached check data");
  if (codes.has("detail-warm")) return t("Detail-page cache");
  return t("Cached release data");
}

export function CacheExplorer({
  snapshot,
  busy,
  onPin,
  onClear,
}: {
  snapshot?: CacheSnapshot;
  busy: boolean;
  onPin: (id: string, pinned: boolean) => void;
  onClear: (id: string, title: string, block: boolean) => void;
}) {
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState("all");
  const [page, setPage] = useState(0);
  const [expanded, setExpanded] = useState<string | null>(null);
  const rows = (snapshot?.releases ?? []).filter(
    (r) =>
      matchesFilter(r, filter) &&
      `${r.Title} ${r.ReleaseID} ${r.reason?.file ?? ""} ${r.reason?.source ?? ""}`
        .toLowerCase()
        .includes(search.toLowerCase()),
  );
  const pages = Math.max(1, Math.ceil(rows.length / 20)),
    current = Math.min(page, pages - 1);
  return (
    <section
      aria-label={t("Cache explorer")}
      className="flex min-h-0 flex-1 flex-col overflow-hidden rounded-xl border border-border bg-surface"
    >
      <div className="flex shrink-0 flex-wrap items-center gap-3 border-b border-border p-4">
        <div className="relative min-w-48 flex-1">
          <Search size={14} className="pointer-events-none absolute left-3 top-3 text-fg-subtle" />
          <Input
            aria-label={t("Search cached releases")}
            placeholder={t("Search titles, files, or release IDs…")}
            className="pl-9"
            value={search}
            onChange={(e) => {
              setSearch(e.target.value);
              setPage(0);
            }}
          />
        </div>
        <select
          aria-label={t("Filter cached releases")}
          value={filter}
          onChange={(e) => {
            setFilter(e.target.value);
            setPage(0);
          }}
          className="h-10 rounded-md border border-border bg-surface px-3 text-sm"
        >
          {FILTERS.map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </select>
        <span className="text-xs text-fg-muted">
          {tn(rows.length, "{count} release", "{count} releases")}
        </span>
      </div>
      <div className="app-scroll min-h-0 flex-1 overflow-auto">
        <Table className="w-full table-fixed">
          <TableHeader>
            <TableRow>
              <TableHead>{t("Title / release")}</TableHead>
              <TableHead>{t("Retained")}</TableHead>
              <TableHead className="hidden md:table-cell">{t("Reason")}</TableHead>
              <TableHead className="hidden lg:table-cell">{t("Last used")}</TableHead>
              <TableHead className="w-56">
                <span className="sr-only">{t("Owner pin")}</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.slice(current * 20, (current + 1) * 20).map((r) => {
              const open = expanded === r.ReleaseID;
              const causes = r.reason?.causes ?? [];
              const primaryCode = causes[0]?.code ?? "unrecorded";
              const extra = Math.max(0, causes.length - 1);
              return (
                <Fragment key={r.ReleaseID}>
                  <TableRow
                    tabIndex={0}
                    aria-expanded={open}
                    className="cursor-pointer transition-colors hover:bg-bg-subtle/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                    onClick={() => setExpanded(open ? null : r.ReleaseID)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        setExpanded(open ? null : r.ReleaseID);
                      }
                    }}
                  >
                    <TableCell className="max-w-0">
                      <div className="flex items-center gap-3">
                        <div className="hidden shrink-0 rounded-lg bg-primary/10 p-2 text-primary sm:block">
                          <Film size={16} />
                        </div>
                        <div className="min-w-0">
                          <p
                            className="truncate text-sm font-medium"
                            title={r.Title || r.reason?.file || r.ReleaseID}
                          >
                            {friendlyName(r)}
                          </p>
                          <p
                            className="mt-0.5 truncate text-[10px] text-fg-subtle"
                            title={r.reason?.file || r.reason?.source || r.ReleaseID}
                          >
                            {r.reason?.file || r.reason?.source || r.ReleaseID}
                          </p>
                          {!!r.Pins?.length && (
                            <div className="mt-1 flex flex-wrap gap-1">
                              {r.Pins.map((p) => (
                                <Badge key={p} variant="secondary">
                                  {pinLabel(p)}
                                </Badge>
                              ))}
                            </div>
                          )}
                        </div>
                      </div>
                    </TableCell>
                    <TableCell className="w-28 sm:w-40">
                      <p className="text-xs font-medium tabular-nums">{formatBytes(r.Bytes)}</p>
                      {r.Size > 0 && (
                        <>
                          <div className="mt-1.5 h-1 overflow-hidden rounded-full bg-primary/10">
                            <div
                              className="h-full bg-primary/60"
                              style={{ width: `${Math.min(100, (r.Bytes / r.Size) * 100)}%` }}
                            />
                          </div>
                          <p className="mt-1 text-[10px] text-fg-subtle">
                            {t("{percent} retained", {
                              percent: formatPercent(
                                Math.min(100, Math.round((r.Bytes / r.Size) * 100)) / 100,
                              ),
                            })}
                          </p>
                        </>
                      )}
                    </TableCell>
                    <TableCell className="hidden text-xs md:table-cell">
                      <p
                        className="truncate font-medium text-fg"
                        title={causes
                          .map((c) => `${causeLabel(c.code)}: ${causeDescription(c.code)}`)
                          .join("\n")}
                      >
                        {causeLabel(primaryCode)}
                        {extra > 0 ? ` +${extra}` : ""}
                      </p>
                      <p className="mt-0.5 line-clamp-2 text-[10px] text-fg-subtle">
                        {causeDescription(primaryCode)}
                      </p>
                    </TableCell>
                    <TableCell className="hidden text-xs text-fg-muted lg:table-cell">
                      {dateLabel(r.LastUsed)}
                    </TableCell>
                    <TableCell className="w-56">
                      <div
                        className="flex items-center justify-end gap-1 whitespace-nowrap"
                        onClick={(e) => e.stopPropagation()}
                      >
                        <Button
                          variant="ghost"
                          size="sm"
                          aria-label={
                            r.Pins?.includes("owner")
                              ? t("Unpin {title}", { title: friendlyName(r) })
                              : t("Pin {title}", { title: friendlyName(r) })
                          }
                          disabled={busy}
                          onClick={() => onPin(r.ReleaseID, !r.Pins?.includes("owner"))}
                        >
                          <Pin
                            size={14}
                            className={r.Pins?.includes("owner") ? "fill-primary text-primary" : ""}
                          />
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={busy}
                          onClick={() => onClear(r.ReleaseID, friendlyName(r), false)}
                        >
                          {t("Clear")}
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={busy}
                          onClick={() => onClear(r.ReleaseID, friendlyName(r), true)}
                        >
                          {t("Clear & block")}
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                  {open && (
                    <TableRow key={`${r.ReleaseID}-detail`}>
                      <TableCell colSpan={5} className="bg-bg-subtle/40">
                        <div className="grid gap-3 py-1 text-xs md:grid-cols-3">
                          <div>
                            <p className="text-[10px] uppercase tracking-wide text-fg-subtle">
                              {t("Fetched because")}
                            </p>
                            <ul className="mt-0.5 space-y-1 text-fg-muted">
                              {causes.length ? (
                                causes.map((c) => (
                                  <li key={c.code}>
                                    <span className="text-fg">{causeLabel(c.code)}</span> —{" "}
                                    {causeDescription(c.code)}
                                  </li>
                                ))
                              ) : (
                                <li>
                                  <span className="text-fg">{t("Unexplained")}</span> —{" "}
                                  {causeDescription("unrecorded")}
                                </li>
                              )}
                            </ul>
                          </div>
                          <div>
                            <p className="text-[10px] uppercase tracking-wide text-fg-subtle">
                              {t("Kept because")}
                            </p>
                            <p className="mt-0.5 text-fg-muted">
                              {retentionLabel(r.reason?.retention)}
                            </p>
                          </div>
                          <div>
                            <p className="text-[10px] uppercase tracking-wide text-fg-subtle">
                              {t("Bytes")}
                            </p>
                            <p className="mt-0.5 text-fg-muted">
                              {formatBytes(r.Bytes)}
                              {r.Size > 0 ? ` / ${formatBytes(r.Size)}` : ""}
                            </p>
                          </div>
                          <div>
                            <p className="text-[10px] uppercase tracking-wide text-fg-subtle">
                              {t("Play history")}
                            </p>
                            <p className="mt-0.5 text-fg-muted">
                              {tn(r.Plays, "{count} play", "{count} plays")}
                              {r.LastPlayed && !r.LastPlayed.startsWith("0001")
                                ? ` · ${dateLabel(r.LastPlayed)}`
                                : ""}
                            </p>
                          </div>
                          <div>
                            <p className="text-[10px] uppercase tracking-wide text-fg-subtle">
                              {t("Source")}
                            </p>
                            <p className="mt-0.5 truncate text-fg-muted" title={r.reason?.source}>
                              {r.reason?.source || "—"}
                            </p>
                          </div>
                          <div>
                            <p className="text-[10px] uppercase tracking-wide text-fg-subtle">
                              {t("File")}
                            </p>
                            <p className="mt-0.5 truncate text-fg-muted" title={r.reason?.file}>
                              {r.reason?.file || "—"}
                            </p>
                          </div>
                        </div>
                      </TableCell>
                    </TableRow>
                  )}
                </Fragment>
              );
            })}
          </TableBody>
        </Table>
        {!rows.length && (
          <div className="px-5 py-16 text-center">
            <Film size={24} className="mx-auto mb-3 text-fg-subtle" />
            <p className="text-sm font-medium">
              {!snapshot
                ? t("Loading cache…")
                : search || filter !== "all"
                  ? t("No matching releases")
                  : t("No retained media yet")}
            </p>
            <p className="mt-2 text-xs text-fg-muted">
              {search || filter !== "all"
                ? t("Try a different search or filter.")
                : t("Cached movies and episodes appear here as viewers watch.")}
            </p>
          </div>
        )}
      </div>
      <div className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-t border-border px-4 py-2.5">
        <p className="text-[11px] text-fg-muted">
          {rows.length
            ? t("{start}–{end} of {total}", {
                start: current * 20 + 1,
                end: Math.min((current + 1) * 20, rows.length),
                total: rows.length,
              })
            : tn(0, "{count} release", "{count} releases")}{" "}
          · {t("Pins protect data from eviction")}
        </p>
        <div className="flex items-center gap-1">
          <Button
            variant="ghost"
            size="sm"
            aria-label={t("Previous cache page")}
            disabled={current === 0}
            onClick={() => setPage(current - 1)}
          >
            <ChevronLeft size={15} />
          </Button>
          <span className="px-1 text-xs">
            {current + 1} / {pages}
          </span>
          <Button
            variant="ghost"
            size="sm"
            aria-label={t("Next cache page")}
            disabled={current + 1 >= pages}
            onClick={() => setPage(current + 1)}
          >
            <ChevronRight size={15} />
          </Button>
        </div>
      </div>
    </section>
  );
}
