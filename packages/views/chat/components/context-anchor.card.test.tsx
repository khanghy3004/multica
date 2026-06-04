import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import enChat from "../../locales/en/chat.json";

const store = vi.hoisted(() => ({
  selectedContext: {
    type: "issue" as const,
    id: "issue-1",
    label: "MUL-2959",
    subtitle: "Chat context behavior",
  },
  setSelectedContext: vi.fn(),
}));

vi.mock("@multica/core/chat", () => ({
  useChatStore: (selector: (state: typeof store) => unknown) => selector(store),
  useRecentContextStore: vi.fn(),
  selectRecentContexts: vi.fn(),
}));

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    issueDetail: (id: string) => `/test/issues/${id}`,
    projectDetail: (id: string) => `/test/projects/${id}`,
  }),
}));

vi.mock("../../navigation", () => ({
  AppLink: ({ href, children, className }: { href: string; children: React.ReactNode; className?: string }) => (
    <a href={href} className={className}>{children}</a>
  ),
  useNavigation: () => ({ pathname: "/test/issues/issue-1", searchParams: new URLSearchParams() }),
}));

vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ fallbackLabel }: { fallbackLabel: string }) => <span>{fallbackLabel}</span>,
}));

vi.mock("../../projects/components/project-chip", () => ({
  ProjectChip: ({ fallbackLabel }: { fallbackLabel: string }) => <span>{fallbackLabel}</span>,
}));

import { ContextAnchorCard } from "./context-anchor";

const TEST_RESOURCES = { en: { chat: enChat } };

describe("ContextAnchorCard", () => {
  it("clears the selected context from the visible card", () => {
    store.setSelectedContext.mockClear();

    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ContextAnchorCard />
      </I18nProvider>,
    );

    expect(screen.getByText("MUL-2959")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Clear chat context" }));

    expect(store.setSelectedContext).toHaveBeenCalledWith(null);
  });
});
