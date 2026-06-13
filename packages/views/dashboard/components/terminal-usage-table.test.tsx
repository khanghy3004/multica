import { describe, it, expect, beforeEach, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";

// Drives useQuery by query key so we can feed the terminal-by-user query real
// rows while every other dashboard query returns an empty, settled result.
const terminalRows = vi.hoisted(() => ({
  current: [] as unknown[],
}));

vi.mock("@tanstack/react-query", async () => {
  const actual = await vi.importActual<typeof import("@tanstack/react-query")>(
    "@tanstack/react-query",
  );
  return {
    ...actual,
    useQuery: (opts: { queryKey: unknown[] }) => {
      const key = opts.queryKey;
      if (key.includes("terminal-by-user")) {
        return { data: terminalRows.current, isLoading: false };
      }
      return { data: [], isLoading: false };
    },
  };
});

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

vi.mock("@multica/core/auth", () => {
  const state = () => ({ user: { timezone: "UTC" } });
  const useAuthStore = Object.assign(
    (sel?: (s: ReturnType<typeof state>) => unknown) => (sel ? sel(state()) : state()),
    { getState: state },
  );
  return { useAuthStore };
});

vi.mock("@multica/core/runtimes/custom-pricing-store", () => {
  const state = () => ({ pricings: {} });
  const useCustomPricingStore = Object.assign(
    (sel?: (s: ReturnType<typeof state>) => unknown) => (sel ? sel(state()) : state()),
    { getState: state },
  );
  return { useCustomPricingStore };
});

// Keep the render light + deterministic — the table is what's under test.
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="avatar" />,
}));

import { DashboardPage } from "./dashboard-page";

describe("DashboardPage — terminal usage table", () => {
  beforeEach(() => {
    cleanup();
    terminalRows.current = [];
  });

  it("renders a per-user row with the user's name and total tokens", () => {
    terminalRows.current = [
      {
        user_id: "user-1",
        name: "Alice",
        email: "alice@example.com",
        input_tokens: 1000,
        output_tokens: 500,
        cache_read_tokens: 0,
        cache_write_tokens: 0,
        total_tokens: 1500,
        session_count: 3,
      },
    ];

    renderWithI18n(<DashboardPage />);

    // Table heading + the user row render.
    expect(screen.getByText("Terminal usage")).toBeInTheDocument();
    expect(screen.getByText("Alice")).toBeInTheDocument();
    // session_count rendered verbatim.
    expect(screen.getByText("3")).toBeInTheDocument();
  });
});
