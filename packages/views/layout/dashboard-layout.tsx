"use client";

import { type ReactNode, useEffect } from "react";
import { SidebarProvider, SidebarInset } from "@multica/ui/components/ui/sidebar";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
import { useCurrentWorkspace, paths } from "@multica/core/paths";
import { ModalRegistry } from "../modals/registry";
import { useNavigation } from "../navigation";
import { SourceBackfillModal } from "../onboarding";
import { AppSidebar } from "./app-sidebar";
import { DashboardGuard } from "./dashboard-guard";
import { NavigationProgress } from "./navigation-progress";
import { WorkspacePresencePrefetch } from "./workspace-presence-prefetch";

interface DashboardLayoutProps {
  children: ReactNode;
  /** Rendered inside SidebarInset (e.g. ChatWindow, ChatFab — absolute-positioned overlays) */
  extra?: ReactNode;
  /** Rendered inside sidebar header as a search trigger */
  searchSlot?: ReactNode;
  /** Loading indicator */
  loadingIndicator?: ReactNode;
}

export function DashboardLayout({
  children,
  extra,
  searchSlot,
  loadingIndicator,
}: DashboardLayoutProps) {
  return (
    <DashboardGuard
      loadingFallback={
        <div className="flex h-svh items-center justify-center">
          {loadingIndicator}
        </div>
      }
    >
      <DashboardShell extra={extra} searchSlot={searchSlot}>
        {children}
      </DashboardShell>
    </DashboardGuard>
  );
}

function DashboardShell({
  children,
  extra,
  searchSlot,
}: {
  children: ReactNode;
  extra?: ReactNode;
  searchSlot?: ReactNode;
}) {
  const wsId = useWorkspaceId();
  const { role } = useCurrentMember(wsId);

  // The terminal-only role gets the terminal full-bleed and nothing else — no
  // sidebar, no chrome, no other page mounts (so their queries never fire and
  // hit the deny-by-default 403). Web-only: desktop uses its own layout.
  if (role === "terminal") {
    return <TerminalOnlyShell>{children}</TerminalOnlyShell>;
  }

  return (
    <SidebarProvider className="h-svh">
      <WorkspacePresencePrefetch />
      <AppSidebar searchSlot={searchSlot} />
      <SidebarInset className="relative overflow-hidden">
        <NavigationProgress />
        {children}
        <ModalRegistry />
        <SourceBackfillModal />
        {extra}
      </SidebarInset>
    </SidebarProvider>
  );
}

// Locks the terminal-only role onto the terminal route. Any other path is
// replaced with the terminal path, and nothing is rendered until we're there,
// so a non-terminal page never mounts its (forbidden) data queries.
function TerminalOnlyShell({ children }: { children: ReactNode }) {
  const navigation = useNavigation();
  const workspace = useCurrentWorkspace();
  const terminalPath = workspace
    ? paths.workspace(workspace.slug).terminal()
    : null;
  const onTerminal =
    terminalPath !== null && navigation.pathname === terminalPath;

  useEffect(() => {
    if (terminalPath && !onTerminal) navigation.replace(terminalPath);
  }, [terminalPath, onTerminal, navigation]);

  if (!onTerminal) return null;

  return <div className="h-svh w-full overflow-hidden bg-black">{children}</div>;
}
