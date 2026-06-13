import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { DashboardLayout } from "./dashboard-layout";

const { state } = vi.hoisted(() => ({
  state: {
    role: "member" as string | null,
    pathname: "/acme/issues",
    replace: vi.fn(),
  },
}));

// Guard is exercised elsewhere — here it just passes children through.
vi.mock("./dashboard-guard", () => ({
  DashboardGuard: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/permissions", () => ({
  useCurrentMember: () => ({ role: state.role }),
}));
vi.mock("@multica/core/paths", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/paths")>(
    "@multica/core/paths",
  );
  return { ...actual, useCurrentWorkspace: () => ({ slug: "acme" }) };
});
vi.mock("../navigation", () => ({
  useNavigation: () => ({ pathname: state.pathname, replace: state.replace }),
}));
vi.mock("@multica/ui/components/ui/sidebar", () => ({
  SidebarProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  SidebarInset: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("./app-sidebar", () => ({ AppSidebar: () => <div data-testid="sidebar" /> }));
vi.mock("./navigation-progress", () => ({ NavigationProgress: () => null }));
vi.mock("./workspace-presence-prefetch", () => ({ WorkspacePresencePrefetch: () => null }));
vi.mock("../modals/registry", () => ({ ModalRegistry: () => null }));
vi.mock("../onboarding", () => ({ SourceBackfillModal: () => null }));

function renderLayout() {
  return render(
    <DashboardLayout>
      <div data-testid="page">page content</div>
    </DashboardLayout>,
  );
}

describe("DashboardLayout terminal-only gate", () => {
  beforeEach(() => {
    state.role = "member";
    state.pathname = "/acme/issues";
    state.replace = vi.fn();
  });

  it("renders the sidebar shell for a normal member", () => {
    renderLayout();
    expect(screen.getByTestId("sidebar")).toBeInTheDocument();
    expect(screen.getByTestId("page")).toBeInTheDocument();
  });

  it("redirects a terminal-only user off a non-terminal path and renders nothing", () => {
    state.role = "terminal";
    state.pathname = "/acme/issues";
    renderLayout();
    expect(state.replace).toHaveBeenCalledWith("/acme/terminal");
    expect(screen.queryByTestId("sidebar")).not.toBeInTheDocument();
    expect(screen.queryByTestId("page")).not.toBeInTheDocument();
  });

  it("renders the terminal full-bleed (no sidebar) when already on the terminal path", () => {
    state.role = "terminal";
    state.pathname = "/acme/terminal";
    renderLayout();
    expect(screen.getByTestId("page")).toBeInTheDocument();
    expect(screen.queryByTestId("sidebar")).not.toBeInTheDocument();
    expect(state.replace).not.toHaveBeenCalled();
  });
});
