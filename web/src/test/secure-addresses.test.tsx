import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SecureAddresses, secureAddresses } from "../pages/settings/SecureAddresses";
import { PlexStatus } from "../pages/settings/sections";

const lan = "https://192-168-1-20.example.plex.direct:32450";
const wan = "https://203-0-113-20.example.plex.direct:32450";
const remote = {
  publishedUris: ["http://192.168.1.20:32450"],
  resourceConnections: [
    { uri: lan, local: true },
    { uri: wan, local: false },
  ],
};

afterEach(() => vi.unstubAllGlobals());

describe("secure discovery addresses", () => {
  it("includes secure discovery addresses in Connectivity beside HTTP registration addresses", () => {
    render(<PlexStatus remote={remote} />);
    expect(screen.getByRole("textbox", { name: "Local network" })).toHaveValue(lan);
    expect(screen.getByText("http://192.168.1.20:32450")).toBeInTheDocument();
    expect(
      screen.getByText(
        "These are registration addresses. Plex may provide different HTTPS addresses to apps.",
      ),
    ).toBeInTheDocument();
  });
  it("shows the discovered local and remote HTTPS URLs even when registration is HTTP", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<SecureAddresses remote={remote} />);
    expect(screen.getByRole("textbox", { name: "Local network" })).toHaveValue(lan);
    expect(screen.getByRole("textbox", { name: "Remote" })).toHaveValue(wan);
    fireEvent.click(screen.getAllByRole("button", { name: "Copy address" })[1]!);
    await waitFor(() => expect(screen.getByText("Address copied")).toBeInTheDocument());
    expect(writeText).toHaveBeenCalledWith(wan);
  });

  it.each([undefined, { writeText: vi.fn().mockRejectedValue(new Error("denied")) }])(
    "selects the full URL with visible instructions when clipboard access is unavailable",
    async (clipboard) => {
      vi.stubGlobal("navigator", { clipboard });
      render(<SecureAddresses remote={remote} />);
      fireEvent.click(screen.getAllByRole("button", { name: "Copy address" })[0]!);
      await screen.findByText("Copy the selected address.");
      const input = screen.getByRole("textbox", { name: "Local network" }) as HTMLInputElement;
      expect(input).toHaveFocus();
      expect(input.selectionStart).toBe(0);
      expect(input.selectionEnd).toBe(lan.length);
      expect(screen.queryByText("Address copied")).toBeNull();
    },
  );

  it("does not imply generated HTTPS candidates have been reported by Plex", () => {
    render(<SecureAddresses remote={{ connections: [{ uri: lan, local: true }] }} />);
    expect(screen.getByText("Plex has not reported any HTTPS addresses yet.")).toBeInTheDocument();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("deduplicates origins, rejects malformed or secret-bearing URLs and preserves relay labels", () => {
    const value = {
      resourceConnections: [
        null,
        { uri: "not a url" },
        { uri: "http://example.test" },
        { uri: "https://user:secret@example.test" },
        { uri: "https://example.test?token=secret" },
        { uri: "https://example.test/#secret" },
        { uri: "https://example.test/secret" },
        { uri: lan, local: true },
        { uri: lan + "/", local: true },
        { uri: "https://relay.example.test", relay: true },
      ],
    };
    expect(secureAddresses(value)).toHaveLength(2);
    render(<SecureAddresses remote={value} />);
    expect(screen.getByRole("textbox", { name: "Relay" })).toHaveValue(
      "https://relay.example.test",
    );
  });

  it("renders shared-user sync status and triggers manual sync when claimed", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    render(
      <PlexStatus
        remote={{
          ...remote,
          claimed: true,
          friends: 3,
          lastFriendSync: new Date().toISOString(),
        }}
      />,
    );
    expect(screen.getByText(/3 shared users/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Sync shared users/i }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        "/ether/api/plex/friends/sync",
        expect.objectContaining({ method: "POST" }),
      ),
    );
  });
});
