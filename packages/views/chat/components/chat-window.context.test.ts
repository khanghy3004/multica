import { describe, expect, it, vi } from "vitest";

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => null,
}));

import { buildOutgoingChatContent } from "./chat-window";

describe("buildOutgoingChatContent", () => {
  it("prepends the stored selected context to the sent message", () => {
    expect(buildOutgoingChatContent("Ship it", {
      type: "issue",
      id: "issue-1",
      label: "MUL-2959",
      subtitle: "Chat context behavior",
    })).toBe('Context: [MUL-2959](mention://issue/issue-1) — "Chat context behavior"\n\nShip it');
  });

  it("sends plain content after the selected context is cleared", () => {
    expect(buildOutgoingChatContent("Ship it", null)).toBe("Ship it");
  });
});
