"use client";

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link2, Search, X } from "lucide-react";
import type { ContextAnchor } from "@multica/core/chat";
import { selectRecentContexts, useChatStore, useRecentContextStore } from "@multica/core/chat";
import { useWorkspaceId } from "@multica/core/hooks";
import { api } from "@multica/core/api";
import { issueDetailOptions } from "@multica/core/issues/queries";
import { projectDetailOptions } from "@multica/core/projects/queries";
import { inboxListOptions } from "@multica/core/inbox/queries";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { Tooltip, TooltipTrigger, TooltipContent } from "@multica/ui/components/ui/tooltip";
import { IssueChip } from "../../issues/components/issue-chip";
import { ProjectChip } from "../../projects/components/project-chip";
import { AppLink, useNavigation } from "../../navigation";
import { useWorkspacePaths } from "@multica/core/paths";
import { useT } from "../../i18n";

export function buildAnchorMarkdown(anchor: ContextAnchor): string {
  if (anchor.type === "issue") {
    const base = `Context: [${anchor.label}](mention://issue/${anchor.id})`;
    return anchor.subtitle ? `${base} — "${anchor.subtitle}"` : base;
  }
  return `Context: Project "${anchor.label}"`;
}

export function useRouteAnchorCandidate(wsId: string): { candidate: ContextAnchor | null; isResolving: boolean } {
  const { pathname, searchParams } = useNavigation();
  const issueMatch = pathname.match(/^\/[^/]+\/issues\/([^/]+)$/);
  const projectMatch = pathname.match(/^\/[^/]+\/projects\/([^/]+)$/);
  const isInbox = /^\/[^/]+\/inbox$/.test(pathname);
  const routeIssueId = issueMatch ? decodeURIComponent(issueMatch[1]!) : null;
  const routeProjectId = projectMatch ? decodeURIComponent(projectMatch[1]!) : null;

  const { data: inboxItems = [] } = useQuery({ ...inboxListOptions(wsId), enabled: isInbox });
  const inboxKey = isInbox ? searchParams.get("issue") : null;
  const inboxSelectedIssueId = isInbox && inboxKey
    ? inboxItems.find((i) => (i.issue_id ?? i.id) === inboxKey)?.issue_id ?? null
    : null;

  const issueIdToFetch = routeIssueId ?? inboxSelectedIssueId;
  const { data: issue, isLoading: issueLoading } = useQuery({
    ...issueDetailOptions(wsId, issueIdToFetch ?? ""),
    enabled: !!issueIdToFetch,
  });
  const { data: project, isLoading: projectLoading } = useQuery({
    ...projectDetailOptions(wsId, routeProjectId ?? ""),
    enabled: !!routeProjectId,
  });

  if (issueIdToFetch) {
    if (!issue) return { candidate: null, isResolving: issueLoading };
    return { candidate: { type: "issue", id: issue.id, label: issue.identifier, subtitle: issue.title }, isResolving: false };
  }
  if (routeProjectId) {
    if (!project) return { candidate: null, isResolving: projectLoading };
    return { candidate: { type: "project", id: project.id, label: project.title }, isResolving: false };
  }
  return { candidate: null, isResolving: false };
}

function anchorKey(anchor: ContextAnchor): string {
  return `${anchor.type}:${anchor.id}`;
}

function ContextRow({ anchor, onSelect }: { anchor: ContextAnchor; onSelect: (anchor: ContextAnchor) => void }) {
  return (
    <button type="button" className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-xs hover:bg-accent" onClick={() => onSelect(anchor)}>
      {anchor.type === "issue" ? <IssueChip issueId={anchor.id} fallbackLabel={anchor.label} /> : <ProjectChip projectId={anchor.id} fallbackLabel={anchor.label} />}
      {anchor.subtitle && <span className="min-w-0 flex-1 truncate text-muted-foreground">{anchor.subtitle}</span>}
    </button>
  );
}

export function ContextAnchorButton() {
  const { t } = useT("chat");
  const wsId = useWorkspaceId();
  const { candidate } = useRouteAnchorCandidate(wsId);
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const selectedContext = useChatStore((s) => s.selectedContext);
  const setSelectedContext = useChatStore((s) => s.setSelectedContext);
  const recentContextRefs = useRecentContextStore(selectRecentContexts(wsId));
  const recordVisit = useRecentContextStore((s) => s.recordVisit);
  const trimmedQuery = query.trim();

  const { data: recentContexts = [] } = useQuery({
    queryKey: ["chat", "recent-contexts", wsId, recentContextRefs],
    enabled: open && recentContextRefs.length > 0,
    queryFn: async () => {
      const resolved: Array<ContextAnchor | null> = await Promise.all(recentContextRefs.map(async (entry) => {
        try {
          if (entry.type === "issue") {
            const issue = await api.getIssue(entry.id);
            return { type: "issue" as const, id: issue.id, label: issue.identifier, subtitle: issue.title };
          }
          const project = await api.getProject(entry.id);
          return { type: "project" as const, id: project.id, label: project.title };
        } catch {
          return null;
        }
      }));
      return resolved.flatMap((item) => (item ? [item] : []));
    },
  });

  const { data: searchResults = [], isLoading: searching } = useQuery({
    queryKey: ["chat", "context-search", wsId, trimmedQuery],
    enabled: open && trimmedQuery.length >= 2,
    queryFn: async ({ signal }) => {
      const [issues, projects] = await Promise.all([
        api.searchIssues({ q: trimmedQuery, limit: 8, include_closed: true, signal }),
        api.searchProjects({ q: trimmedQuery, limit: 8, include_closed: true, signal }),
      ]);
      const results: ContextAnchor[] = [
        ...issues.issues.map((issue) => ({ type: "issue" as const, id: issue.id, label: issue.identifier, subtitle: issue.title })),
        ...projects.projects.map((project) => ({ type: "project" as const, id: project.id, label: project.title })),
      ];
      return results;
    },
  });

  const visibleRecent = useMemo(() => {
    const hidden = new Set<string>();
    if (candidate) hidden.add(anchorKey(candidate));
    return recentContexts.filter((item) => !hidden.has(anchorKey(item))).slice(0, 6);
  }, [candidate, recentContexts]);

  const visibleSearch = useMemo(() => {
    const hidden = new Set<string>();
    if (candidate) hidden.add(anchorKey(candidate));
    for (const item of visibleRecent) hidden.add(anchorKey(item));
    return searchResults.filter((item) => !hidden.has(anchorKey(item))).slice(0, 8);
  }, [candidate, visibleRecent, searchResults]);

  const select = (anchor: ContextAnchor) => {
    setSelectedContext(anchor);
    recordVisit(wsId, { type: anchor.type, id: anchor.id });
    setOpen(false);
    setQuery("");
  };

  const tooltipText = selectedContext
    ? selectedContext.type === "issue"
      ? t(($) => $.context_anchor.tooltip_selected_issue, { label: selectedContext.label })
      : t(($) => $.context_anchor.tooltip_selected_project, { label: selectedContext.label })
    : t(($) => $.context_anchor.tooltip_picker);

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <Tooltip>
        <TooltipTrigger render={<PopoverTrigger render={<Button variant={selectedContext ? "secondary" : "ghost"} size="icon-sm" className={selectedContext ? undefined : "text-muted-foreground"} aria-label={t(($) => $.context_anchor.aria_pick)} aria-pressed={!!selectedContext}><Link2 /></Button>} />} />
        <TooltipContent side="top">{tooltipText}</TooltipContent>
      </Tooltip>
      <PopoverContent side="top" align="end" className="w-80 p-2">
        <div className="relative mb-2">
          <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder={t(($) => $.context_anchor.search_placeholder)} className="h-8 pl-7 text-xs" />
        </div>
        {candidate && <div className="mb-2"><div className="px-2 pb-1 text-[11px] font-medium text-muted-foreground">{t(($) => $.context_anchor.section_current)}</div><ContextRow anchor={candidate} onSelect={select} /></div>}
        {visibleRecent.length > 0 && <div className="mb-2"><div className="px-2 pb-1 text-[11px] font-medium text-muted-foreground">{t(($) => $.context_anchor.section_recent)}</div>{visibleRecent.map((item) => <ContextRow key={anchorKey(item)} anchor={item} onSelect={select} />)}</div>}
        {(trimmedQuery.length >= 2 || searching) && <div><div className="px-2 pb-1 text-[11px] font-medium text-muted-foreground">{t(($) => $.context_anchor.section_search)}</div>{visibleSearch.map((item) => <ContextRow key={anchorKey(item)} anchor={item} onSelect={select} />)}{!searching && visibleSearch.length === 0 && <div className="px-2 py-2 text-xs text-muted-foreground">{t(($) => $.context_anchor.search_empty)}</div>}</div>}
      </PopoverContent>
    </Popover>
  );
}

export function ContextAnchorCard() {
  const { t } = useT("chat");
  const paths = useWorkspacePaths();
  const selectedContext = useChatStore((s) => s.selectedContext);
  const setSelectedContext = useChatStore((s) => s.setSelectedContext);
  if (!selectedContext) return null;

  const href = selectedContext.type === "issue" ? paths.issueDetail(selectedContext.id) : paths.projectDetail(selectedContext.id);
  const tooltipText = selectedContext.type === "issue"
    ? selectedContext.subtitle
      ? t(($) => $.context_anchor.card_tooltip_issue_with_subtitle, { label: selectedContext.label, subtitle: selectedContext.subtitle })
      : t(($) => $.context_anchor.card_tooltip_issue, { label: selectedContext.label })
    : t(($) => $.context_anchor.card_tooltip_project, { label: selectedContext.label });

  return (
    <div className="mx-2 mt-2 flex items-center gap-1">
      <Tooltip>
        <TooltipTrigger render={<AppLink href={href} className="inline-flex">{selectedContext.type === "issue" ? <IssueChip issueId={selectedContext.id} fallbackLabel={selectedContext.label} className="cursor-pointer hover:bg-accent transition-colors" /> : <ProjectChip projectId={selectedContext.id} fallbackLabel={selectedContext.label} className="cursor-pointer hover:bg-accent transition-colors" />}</AppLink>} />
        <TooltipContent side="top">{tooltipText}</TooltipContent>
      </Tooltip>
      <Button variant="ghost" size="icon-xs" className="text-muted-foreground" onClick={() => setSelectedContext(null)} aria-label={t(($) => $.context_anchor.aria_clear)}><X /></Button>
    </div>
  );
}
