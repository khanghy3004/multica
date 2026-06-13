"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Maximize2, Minimize2 } from "lucide-react";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
import { terminalMachineListOptions } from "@multica/core/runtimes/queries";
import { useWS } from "@multica/core/realtime";
import type { RuntimeDevice } from "@multica/core/types";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Button } from "@multica/ui/components/ui/button";
import { cn } from "@multica/ui/lib/utils";
import { PageHeader } from "../../layout/page-header";
import { useT } from "../../i18n";

// One connectable machine, derived by collapsing the workspace's runtimes on
// daemon_id (a machine may expose several provider runtimes — claude, codex —
// but the terminal targets the machine, not a single runtime). We carry one
// representative runtime_id per machine because the server routes the terminal
// by runtime_id (the daemon WS authenticates with a user token and exposes no
// reliable daemon_id).
interface DaemonOption {
  daemonId: string;
  runtimeId: string;
  name: string;
  online: boolean;
}

type ConnState = "idle" | "connecting" | "connected" | "closed";

function deriveDaemons(runtimes: RuntimeDevice[]): DaemonOption[] {
  const byDaemon = new Map<string, DaemonOption>();
  for (const rt of runtimes) {
    if (!rt.daemon_id) continue;
    const existing = byDaemon.get(rt.daemon_id);
    const online = rt.status === "online";
    if (existing) {
      // Prefer an online runtime as the representative so the connection
      // targets a live link.
      if (online && !existing.online) existing.runtimeId = rt.id;
      existing.online = existing.online || online;
    } else {
      byDaemon.set(rt.daemon_id, {
        daemonId: rt.daemon_id,
        runtimeId: rt.id,
        name: rt.name || rt.daemon_id.slice(0, 8),
        online,
      });
    }
  }
  // Online machines first, then by name.
  return [...byDaemon.values()].sort((a, b) =>
    a.online === b.online ? a.name.localeCompare(b.name) : a.online ? -1 : 1,
  );
}

// Build the terminal WebSocket endpoint from the configured realtime ws URL so
// it inherits the same origin + proxy (same-origin keeps the auth cookie on
// the upgrade). The realtime URL ends in "/ws"; the terminal lives under
// "/api/workspaces/{id}/terminal/ws".
function buildTerminalUrl(
  wsUrl: string,
  workspaceId: string,
  runtimeId: string,
): string {
  const u = new URL(wsUrl);
  u.pathname = `/api/workspaces/${workspaceId}/terminal/ws`;
  u.search = `?runtime_id=${encodeURIComponent(runtimeId)}`;
  return u.toString();
}

function encodeBase64(s: string): string {
  const bytes = new TextEncoder().encode(s);
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary);
}

function decodeBase64(s: string): Uint8Array {
  const binary = atob(s);
  const arr = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) arr[i] = binary.charCodeAt(i);
  return arr;
}

export function TerminalPage() {
  const { t } = useT("terminal");
  const wsId = useWorkspaceId();
  const { wsUrl } = useWS();
  const { role } = useCurrentMember(wsId);
  const { data: runtimes } = useQuery(terminalMachineListOptions(wsId));

  const daemons = useMemo(() => deriveDaemons(runtimes ?? []), [runtimes]);
  const [selected, setSelected] = useState<string | null>(null);
  const [isFs, setIsFs] = useState(false);
  const termAreaRef = useRef<HTMLDivElement>(null);

  // The terminal-only role has no other surface, so the gate already gives it
  // a chrome-free full-bleed terminal; just hide the page header + machine bar
  // for it. Everyone else gets a fullscreen toggle backed by the Fullscreen
  // API — requesting it on the terminal area alone renders only the terminal
  // fullscreen (sidebar + header excluded). The role can't auto-enter the API
  // (it needs a user gesture), so it relies on the gate's full-bleed instead.
  const forcedChromeless = role === "terminal";
  const chromeless = forcedChromeless || isFs;

  // Keep isFs in sync with the actual fullscreen element (covers Esc to exit).
  useEffect(() => {
    const onChange = () => setIsFs(document.fullscreenElement === termAreaRef.current);
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, []);

  const enterFullscreen = () => {
    termAreaRef.current?.requestFullscreen?.().catch(() => {
      // Fullscreen denied (no gesture / unsupported) — leave chrome in place.
    });
  };
  const exitFullscreen = () => {
    if (document.fullscreenElement) void document.exitFullscreen();
  };

  // Default to the first online machine once the list loads.
  useEffect(() => {
    if (selected) return;
    const firstOnline = daemons.find((d) => d.online);
    if (firstOnline) setSelected(firstOnline.daemonId);
  }, [daemons, selected]);

  const selectedRuntimeId = useMemo(
    () => daemons.find((d) => d.daemonId === selected)?.runtimeId ?? null,
    [daemons, selected],
  );

  const supported = typeof window !== "undefined" && "WebSocket" in window;

  return (
    <div className="flex h-full flex-col">
      {!chromeless && (
        <>
          <PageHeader>
            <h1 className="text-sm font-semibold">{t(($) => $.title)}</h1>
            <Button
              variant="ghost"
              size="icon-sm"
              className="ml-auto"
              onClick={enterFullscreen}
              title={t(($) => $.enter_fullscreen)}
              aria-label={t(($) => $.enter_fullscreen)}
            >
              <Maximize2 className="h-4 w-4" />
            </Button>
          </PageHeader>

          <div className="flex shrink-0 items-center gap-2 border-b px-4 py-2">
            <span className="text-xs text-muted-foreground">
              {t(($) => $.pick_machine)}
            </span>
            {daemons.map((d) => (
              <button
                key={d.daemonId}
                type="button"
                disabled={!d.online}
                onClick={() => setSelected(d.daemonId)}
                className={cn(
                  "flex items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs transition-colors",
                  d.daemonId === selected
                    ? "border-primary bg-primary/10 text-foreground"
                    : "border-border text-muted-foreground hover:bg-muted",
                  !d.online && "cursor-not-allowed opacity-50",
                )}
              >
                <span
                  className={cn(
                    "size-1.5 rounded-full",
                    d.online ? "bg-green-500" : "bg-muted-foreground",
                  )}
                />
                {d.name}
              </button>
            ))}
          </div>
        </>
      )}

      <div ref={termAreaRef} className="relative min-h-0 flex-1 bg-black">
        {!supported ? (
          <EmptyHint text={t(($) => $.unsupported)} />
        ) : daemons.length === 0 ? (
          <EmptyHint text={t(($) => $.no_machines)} />
        ) : selectedRuntimeId ? (
          <TerminalSession
            key={`${selectedRuntimeId}:${wsId}`}
            wsUrl={wsUrl}
            workspaceId={wsId}
            runtimeId={selectedRuntimeId}
          />
        ) : (
          <EmptyHint text={t(($) => $.no_machines)} />
        )}
        {/* Exit-fullscreen affordance, shown only while the Fullscreen API is
            active (never for the terminal-only role, which uses the gate). */}
        {isFs && (
          <Button
            variant="ghost"
            size="icon-sm"
            className="absolute right-2 top-2 z-10 bg-background/70 text-muted-foreground hover:bg-background"
            onClick={exitFullscreen}
            title={t(($) => $.exit_fullscreen)}
            aria-label={t(($) => $.exit_fullscreen)}
          >
            <Minimize2 className="h-4 w-4" />
          </Button>
        )}
      </div>
    </div>
  );
}

function EmptyHint({ text }: { text: string }) {
  return (
    <div className="flex h-full items-center justify-center p-8 text-center text-sm text-muted-foreground">
      {text}
    </div>
  );
}

// Mounts an xterm instance, opens the relay WebSocket, and bridges bytes both
// ways. Remounted (via key) whenever the target machine changes, so each
// session is fully torn down and rebuilt.
function TerminalSession({
  wsUrl,
  workspaceId,
  runtimeId,
}: {
  wsUrl: string;
  workspaceId: string;
  runtimeId: string;
}) {
  const { t } = useT("terminal");
  const containerRef = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<ConnState>("connecting");

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const term = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily:
        'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
      theme: { background: "#000000" },
      convertEol: false,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(container);
    fit.fit();

    const url = buildTerminalUrl(wsUrl, workspaceId, runtimeId);
    const socket = new WebSocket(url);

    const sendResize = () => {
      if (socket.readyState !== WebSocket.OPEN) return;
      socket.send(
        JSON.stringify({
          type: "terminal:resize",
          payload: { cols: term.cols, rows: term.rows },
        }),
      );
    };

    socket.onopen = () => {
      setState("connected");
      sendResize();
      term.focus();
    };

    socket.onmessage = (event) => {
      let msg: { type: string; payload?: { data?: string; message?: string } };
      try {
        msg = JSON.parse(event.data as string);
      } catch {
        return;
      }
      switch (msg.type) {
        case "terminal:stdout":
          if (msg.payload?.data) term.write(decodeBase64(msg.payload.data));
          break;
        case "terminal:error":
          if (msg.payload?.message) term.writeln(`\r\n[${msg.payload.message}]`);
          break;
        case "terminal:exit":
          setState("closed");
          break;
        default:
          break;
      }
    };

    socket.onclose = () => setState("closed");
    socket.onerror = () => setState("closed");

    const dataSub = term.onData((data) => {
      if (socket.readyState !== WebSocket.OPEN) return;
      socket.send(
        JSON.stringify({
          type: "terminal:stdin",
          payload: { data: encodeBase64(data) },
        }),
      );
    });

    const resizeObserver = new ResizeObserver(() => {
      try {
        fit.fit();
      } catch {
        // Container detached mid-resize; ignore.
      }
      sendResize();
    });
    resizeObserver.observe(container);

    return () => {
      resizeObserver.disconnect();
      dataSub.dispose();
      // Closing the socket triggers the server's terminal:close to the daemon.
      socket.close();
      term.dispose();
    };
  }, [wsUrl, workspaceId, runtimeId]);

  return (
    <>
      <div ref={containerRef} className="absolute inset-0 p-2" />
      {state !== "connected" && (
        <div className="pointer-events-none absolute right-3 top-2 rounded bg-background/80 px-2 py-0.5 text-xs text-muted-foreground">
          {state === "connecting"
            ? t(($) => $.connecting)
            : state === "closed"
              ? t(($) => $.session_ended)
              : t(($) => $.disconnected)}
        </div>
      )}
    </>
  );
}
