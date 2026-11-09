import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { calls, renderApp } from "./harness";
import { RELEASE_SEARCH } from "../pages/settings/schema";
import { destinations } from "../lib/sitemap";
import { settingsIndex } from "../lib/settings-index";

it("gives the checker its own searchable destination and inline controls", async () => {
  renderApp("/quality/check");
  const result = await screen.findByTestId("quality-headline", {}, { timeout: 10000 });
  expect(result).toHaveTextContent("4K");
  expect(screen.getAllByTestId("quality-headline")).toHaveLength(1);
  // The step bar shows here too, with Check quality as the current step.
  const steps = screen.getByRole("navigation", { name: "How quality works" });
  expect(steps.querySelector("[aria-current=step]")).toHaveTextContent("Check quality");
  const viewer = screen.getByRole("button", { name: "Viewer" });
  const row = viewer.closest(".grid")!;
  expect(row).toHaveClass("sm:grid-cols-3");
  expect(within(row as HTMLElement).getByRole("button", { name: "App" })).toBeVisible();
  expect(within(row as HTMLElement).getByRole("radio", { name: "Away" })).toBeVisible();
  expect(destinations().some((d) => d.href === "/quality/check")).toBe(true);
  expect(settingsIndex().find((d) => d.title === "Check quality")?.href).toBe("/quality/check");
});

it("keeps the four search settings visible", () => {
  expect(RELEASE_SEARCH[0]!.more).toBeUndefined();
  expect(RELEASE_SEARCH[0]!.fields.map((f) => f.key)).toEqual([
    "maxCandidates",
    "healthSamples",
    "searchCacheTtl",
    "unavailableCooloff",
  ]);
});

it("orders profile sections before away, source order, and versions", async () => {
  renderApp("/quality/profiles");
  await screen.findByRole("button", { name: "Add profile" }, { timeout: 10000 });
  await screen.findByText("Same-quality versions to offer", {}, { timeout: 10000 });
  const flow = screen.getByRole("navigation", { name: "How quality works" });
  expect(flow.querySelector("[aria-current=step]")).toHaveTextContent("Profiles");
  expect(within(flow).queryByRole("link", { name: /Profiles/ })).not.toBeInTheDocument();
  const headings = screen.getAllByRole("heading").map((h) => h.textContent);
  const order = ["Profiles", "Which source goes first", "Version menu"].map((s) =>
    headings.indexOf(s),
  );
  expect(order.every((i) => i >= 0)).toBe(true);
  expect(order).toEqual([...order].sort((a, b) => a - b));
  expect(screen.queryByTestId("quality-headline")).not.toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: "Import a preset quality profile" }),
  ).not.toBeInTheDocument();
});

it("switches profiles between movies and shows and badges each card", async () => {
  renderApp("/quality/profiles", {}, (fake) => {
    const profiles = fake.state.config.quality.profiles!;
    profiles.push({ ...structuredClone(profiles[2]!), name: "Sitcoms" });
  });
  const movies = await screen.findByRole("radio", { name: "Movies" }, { timeout: 10000 });
  const shows = screen.getByRole("radio", { name: "Shows" });
  expect(movies).toHaveAttribute("aria-checked", "true");
  const row = (name: string) => screen.getByRole("button", { name: `Edit ${name}` }).closest("li")!;
  await screen.findByRole("button", { name: "Edit 4K" }, { timeout: 10000 });
  expect(within(row("4K")).getByText("Movies")).toBeInTheDocument();
  expect(within(row("1080p")).getByText("Movies")).toBeInTheDocument();
  // Every profile is for one media: there is no "Movies & shows" profile.
  expect(screen.queryByText("Movies & shows")).not.toBeInTheDocument();

  fireEvent.click(shows);
  expect(
    await screen.findByRole("button", { name: "Edit Sitcoms" }, { timeout: 10000 }),
  ).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Edit 4K" })).not.toBeInTheDocument();
  // The show profile named 1080p is its own profile.
  expect(within(row("1080p")).getByText("Shows")).toBeInTheDocument();
});

it("offers only Movies and Shows, and a moved profile takes its rules along", async () => {
  const { fake } = renderApp("/quality/profiles", {}, (f) => {
    f.state.config.quality.profiles!.push({
      ...structuredClone(f.state.config.quality.profiles![0]!),
      name: "Kids",
    });
    f.state.config.quality.rules!.push({ userId: 7, mediaType: "movie", profile: "Kids" });
  });
  fireEvent.click(await screen.findByRole("button", { name: "Edit Kids" }, { timeout: 10000 }));
  const drawer = await screen.findByRole("dialog", { name: "Kids" });
  const applies = within(drawer).getByRole("radiogroup", { name: "Applies to" });
  expect(
    within(applies)
      .getAllByRole("radio")
      .map((r) => r.textContent),
  ).toEqual(["Movies", "Shows"]);
  fireEvent.click(within(applies).getByRole("radio", { name: "Shows" }));
  fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
  await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
  const body = calls(fake, "PUT", "/config/quality")[0]!.body as {
    profiles: { name: string; mediaType?: string }[];
    rules: { userId?: number; mediaType?: string; profile: string }[];
  };
  expect(body.profiles.find((p) => p.name === "Kids")?.mediaType).toBe("tv");
  expect(body.rules.at(-1)).toEqual({ userId: 7, mediaType: "tv", profile: "Kids" });
});

it("collapses guide sync to one line until a profile is imported", async () => {
  renderApp("/quality/profiles");
  expect(
    await screen.findByText(
      "Applies once you import a profile from TRaSH Guides or Profilarr.",
      {},
      { timeout: 10000 },
    ),
  ).toBeInTheDocument();
  expect(
    screen.queryByRole("switch", { name: "Keep imported profiles in sync" }),
  ).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Sync now" })).not.toBeInTheDocument();
});

it("saves the automatic sync cadence as a patch and runs it", async () => {
  const { fake } = renderApp("/quality/profiles", {}, (f) => {
    f.state.config.quality.profiles![0]!.import = {
      source: "trash",
      app: "radarr",
      profile: "UHD Bluray + WEB",
    };
  });
  await screen.findByRole("switch", { name: "Keep imported profiles in sync" }, { timeout: 10000 });
  // The title appears once, in the card header with its switch.
  expect(screen.getAllByText("Keep imported profiles in sync")).toHaveLength(1);
  expect(screen.getByText("Never synced")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Check for changes" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("switch", { name: "Keep imported profiles in sync" }));
  fireEvent.click(screen.getByRole("button", { name: "Check for changes" }));
  fireEvent.click(await screen.findByRole("option", { name: "Every 3 days" }, { timeout: 4000 }));
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
  expect(calls(fake, "PUT", "/config/quality")[0]!.body).toEqual({
    sync: { enabled: true, interval: "72h" },
  });
  await waitFor(() => expect(fake.state.config.quality.sync?.interval).toBe("72h"));

  fireEvent.click(screen.getByRole("button", { name: "Sync now" }));
  await waitFor(() => expect(calls(fake, "POST", "/releases/quality/sync")).toHaveLength(1));
  expect(await screen.findByText("2 formats added")).toBeInTheDocument();
});

it("shows a sync failure without a settings save bar and clears it on retry", async () => {
  let fail = true;
  renderApp("/quality/profiles", {}, (f) => {
    f.state.config.quality.profiles![0]!.import = {
      source: "trash",
      app: "radarr",
      profile: "UHD Bluray + WEB",
    };
    const original = f.fetch;
    f.fetch = async (input, init) => {
      if (String(input).endsWith("/releases/quality/sync") && init?.method === "POST" && fail) {
        fail = false;
        return new Response(
          JSON.stringify({ error: { code: "validation", message: "Sync failed" } }),
          {
            status: 400,
          },
        );
      }
      return original(input, init);
    };
  });
  const sync = await screen.findByRole("button", { name: "Sync now" });
  fireEvent.click(sync);
  expect(await screen.findByText("Sync failed")).toBeVisible();
  expect(screen.queryByRole("region", { name: "Unsaved changes" })).not.toBeInTheDocument();
  fireEvent.click(sync);
  expect(await screen.findByText("2 formats added")).toBeVisible();
  expect(screen.queryByText("Sync failed")).not.toBeInTheDocument();
});

it("keeps an empty media tab selected so its first profile can be added", async () => {
  renderApp("/quality/profiles", {}, (fake) => {
    fake.state.config.quality.profiles = fake.state.config.quality.profiles!.map((p) => ({
      ...p,
      mediaType: "movie",
    }));
  });
  await screen.findByRole("button", { name: "Add profile" });
  fireEvent.click(screen.getByRole("radio", { name: "Shows" }));
  await waitFor(() =>
    expect(screen.getByRole("radio", { name: "Shows" })).toHaveAttribute("aria-checked", "true"),
  );
  fireEvent.click(screen.getByRole("button", { name: "Add profile" }));
  const drawer = await screen.findByRole("dialog");
  expect(within(drawer).getByText("Shows")).toBeInTheDocument();
  expect(within(drawer).queryByRole("radio", { name: "Movies" })).not.toBeInTheDocument();
  expect(within(drawer).queryByRole("radio", { name: "Both" })).not.toBeInTheDocument();
});

it("sets a shared profile as the away default only for the selected media", async () => {
  const { fake } = renderApp("/quality/profiles", {}, (server) => {
    server.state.config.quality.remote = "";
    server.state.config.quality.remoteMovie = "";
    server.state.config.quality.remoteTv = "";
  });
  fireEvent.click(await screen.findByRole("radio", { name: "Shows" }, { timeout: 10000 }));
  fireEvent.click(screen.getByRole("button", { name: "Edit 1080p" }));
  const drawer = await screen.findByRole("dialog", { name: "1080p" });
  fireEvent.click(within(drawer).getByRole("button", { name: "Away" }));
  fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
  await waitFor(() => expect(fake.state.config.quality.remoteTv).toBe("1080p"));
  expect(fake.state.config.quality.remoteMovie).toBe("");
});

it("clears a legacy away default for one media while preserving the other", async () => {
  const { fake } = renderApp("/quality/profiles", {}, (server) => {
    server.state.config.quality.remote = "1080p";
    server.state.config.quality.remoteMovie = "";
    server.state.config.quality.remoteTv = "";
  });
  fireEvent.click(await screen.findByRole("button", { name: "Edit 1080p" }, { timeout: 10000 }));
  const drawer = await screen.findByRole("dialog", { name: "1080p" });
  fireEvent.click(within(drawer).getByRole("button", { name: "Away" }));
  fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
  await waitFor(() => expect(fake.state.config.quality.remote).toBe(""));
  expect(fake.state.config.quality.remoteMovie).toBe("");
  expect(fake.state.config.quality.remoteTv).toBe("1080p");
});

it("settles dangling client rules when saving a profile", async () => {
  const { fake } = renderApp("/quality/profiles", {}, (server) => {
    server.state.config.quality.default = "Gone";
    server.state.config.quality.rules = [
      { product: "Plex for Apple TV", profile: "4K" },
      { product: "Roku", profile: "TRaSH UHD", maxHeight: 1080 },
      { platform: "iOS", mediaType: "movie", profile: "TRaSH Remux" },
      { userId: 7, mediaType: "tv", profile: "TRaSH WEB" },
    ];
  });
  fireEvent.click(await screen.findByRole("button", { name: "Edit 4K" }, { timeout: 10000 }));
  const drawer = await screen.findByRole("dialog", { name: "4K" });
  fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
  await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
  expect(fake.state.config.quality.default).toBe("");
  // Mock fixture has movie [4K, 1080p] and tv [1080p], so a media-neutral
  // dangling rule splits across each media's fallback profile.
  expect(fake.state.config.quality.rules).toEqual([
    { product: "Plex for Apple TV", profile: "4K" },
    { product: "Roku", mediaType: "movie", profile: "4K", maxHeight: 1080 },
    { product: "Roku", mediaType: "tv", profile: "1080p", maxHeight: 1080 },
    { platform: "iOS", mediaType: "movie", profile: "4K" },
    { userId: 7, mediaType: "tv", profile: "1080p" },
  ]);
});
