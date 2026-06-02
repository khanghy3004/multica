#!/usr/bin/env python3
"""Restore ~/.claude/agents/*.md frontmatter from session-context metadata.

Reads the current file's body (post-frontmatter) and `model:` field, then
rewrites the frontmatter with the original description + tools list.
"""

import os
import re
import sys
from pathlib import Path

import yaml

AGENTS = {
    "ba-report-creator": {
        "tools": ["Read", "Write", "Edit", "Bash", "Grep", "Glob", "AskUserQuestion", "Skill", "TodoWrite"],
        "description": 'Use PROACTIVELY when the user asks to create, update, or modify a BA (Business Analyst) report handler in the Biotricity BA reports service. Triggers on phrases like "tạo report", "viết report", "viết script report", "new report", "create report", "issue NNN", "add handler", "update issue_NNN", "export data to xlsx/json/csv", "enrich existing xlsx", or any request to add a new query for the BA team. The agent operates exclusively inside `/Users/hynguyen/project/btcy-bioflux-backend-ba_reports`, invokes the `ba-report-generator-2` skill for all codebase conventions, and asks clarifying questions whenever the requirement is ambiguous. It never touches git (no commits, no branches, no pushes, no resets).',
    },
    "chihiro": {
        "tools": ["Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch", "ToolSearch", "Bash", "Skill", "AskUserQuestion", "mcp__figma__get_design_context", "mcp__figma__get_screenshot", "mcp__figma__get_metadata", "mcp__figma__get_variable_defs", "mcp__figma__generate_figma_design"],
        "description": 'Use PROACTIVELY when a PM, BA, designer, or tech lead wants to write requirements, draft an SRS, create UI/UX specs, define component structures, produce design tokens, or conduct technical analysis. Triggers on phrases like "write requirements for...", "draft the SRS for...", "define components for...", "create user flow for...", "design specs for...", "analyze the technical approach for...", or when a user shares a PRD, wireframe, or Figma link and wants structured documentation from it. Chihiro is the Design Mode agent — she bridges raw ideas into baselined SRS and design artifacts that Build Mode can act on.',
    },
    "compliance-specialist": {
        "tools": ["Read", "Write", "Edit", "Bash", "Grep", "Glob"],
        "description": "Security compliance and regulatory framework specialist. Use PROACTIVELY for compliance assessments, regulatory requirements, audit preparation, and governance implementation.",
    },
    "doc-updater": {
        "tools": ["Read", "Write", "Edit", "Bash", "Grep", "Glob"],
        "description": "Documentation and codemap specialist. Use PROACTIVELY for updating codemaps and documentation. Runs /update-codemaps and /update-docs, generates docs/CODEMAPS/*, updates READMEs and guides.",
    },
    "docs-manager": {
        # "(Tools: All tools)" in session prompt → no restriction. Omit field.
        "tools": None,
        "description": "Use this agent when you need to manage technical documentation, establish implementation standards, analyze and update existing documentation based on code changes, write or update Product Development Requirements (PDRs), organize documentation for developer productivity, or produce documentation summary reports. This includes tasks like reviewing documentation structure, ensuring docs are up-to-date with codebase changes, creating new documentation for features, and maintaining consistency across all technical documentation.",
    },
    "e2e-runner": {
        "tools": ["Read", "Write", "Edit", "Bash", "Grep", "Glob"],
        "description": "End-to-end testing specialist using Vercel Agent Browser (preferred) with Playwright fallback. Use PROACTIVELY for generating, maintaining, and running E2E tests. Manages test journeys, quarantines flaky tests, uploads artifacts (screenshots, videos, traces), and ensures critical user flows work.",
    },
    "haku": {
        "tools": ["Read", "Write", "Bash", "Glob", "Grep"],
        "description": 'Use PROACTIVELY when a release is being prepared, a change request needs impact analysis, or CCB documentation is required. Triggers on: "prepare release notes for...", "assess the impact of...", "generate CCB form for...", "create deployment plan for release...", "summarize changes for milestone...", or when Yubaba signals a change control event. Haku gathers all information from the GitHub issue hierarchy for a release, then produces release notes, impact assessment reports, CCB forms, and deployment plans.',
    },
    "kamaji": {
        # Note: "Agent, (susu-be, susu-fe)" rendered weirdly in session
        # prompt; canonical form is "Agent (susu-be, susu-fe)".
        "tools": ["Read", "Write", "Edit", "Glob", "Grep", "Bash", "ToolSearch", "Agent (susu-be, susu-fe)", "Skill", "AskUserQuestion", "TodoWrite"],
        "description": 'Use PROACTIVELY when a Build Mode dev pipeline needs to be orchestrated — reading an issues.json file, managing task dependencies, spawning Susu workers, and tracking progress until all PRs are opened. Triggers when Jenkins starts a build session, when an issues.json path is provided, or when the user says "run the build for...", "start the dev pipeline for...", or "dispatch tasks for feature ...". Kamaji spawns Susu as subagents (default) or agent team members. He does not guide Susu through their steps — Susu knows its own workflow. Kamaji handles orchestration, escalations, bot PR creation, and pipeline-level GitHub status only.',
    },
    "kaonashi": {
        "tools": ["Read", "Write", "Edit", "Bash", "Glob", "Grep"],
        "description": 'Use PROACTIVELY when triggered by Yubaba after a test environment is deployed and ready for QC. Takes a parent requirement issue and the completed PRs for that requirement, designs test cases from requirement coverage, generates Playwright E2E test scripts, commits them to the test suite, and reports coverage gaps. Triggers on: "run QC for requirement #...", "generate E2E tests for...", "test the deployed feature for...", or when Yubaba signals that a staging environment is ready for a given requirement.',
    },
    "lin": {
        "tools": ["Read", "Write", "Edit", "Glob", "Grep", "Bash", "ToolSearch", "Skill", "TodoWrite"],
        "description": "Subagent invoked by Susu to review code diffs before a PR is opened. Triggered internally within Susu's pipeline — not invoked directly by users. Reviews test quality first (TDD compliance), then code style, security, compliance markers, and architecture drift. Produces a structured findings report with a review checklist, then MUST post it as a GitHub comment before returning to Susu. Lin only reports — Susu decides what to fix, skip, or escalate to Kamaji. Lin does not update project status.",
    },
    "planner": {
        "tools": ["Read", "Grep", "Glob"],
        "description": "Expert planning specialist for complex features and refactoring. Use PROACTIVELY when users request feature implementation, architectural changes, or complex refactoring. Automatically activated for planning tasks.",
    },
    "pro-code-writer": {
        "tools": None,  # "All tools"
        "description": "Use this agent when you need professional, production-ready code written from scratch or when enhancing existing code. This agent is ideal for implementing features, creating utilities, building APIs, writing algorithms, or any task requiring high-quality, maintainable code.",
    },
    "scout": {
        "tools": ["Glob", "Grep", "Read", "WebFetch", "TodoWrite", "WebSearch", "Bash", "BashOutput", "KillShell", "ListMcpResourcesTool", "ReadMcpResourceTool"],
        "description": "Use this agent when you need to quickly locate relevant files across a large codebase to complete a specific task. This agent is particularly useful when you need to find files across multiple directories, understand file relationships, or explore unfamiliar codebases.",
    },
    "service-scanner": {
        "tools": ["Read", "Write", "Edit", "Bash", "Grep", "Glob", "Skill"],
        "description": "Multi-repository service discovery agent. Scans local repos, ensures CLAUDE.md and codemaps exist, extracts service metadata, builds services.json registry. Use PROACTIVELY when setting up feature planning or auditing service inventory.",
    },
    "susu-be": {
        "tools": ["Read", "Write", "Edit", "Glob", "Grep", "Bash", "ToolSearch", "Agent (lin, susu-step)", "Skill", "TodoWrite"],
        "description": "Use PROACTIVELY when spawned by Kamaji to implement a single backend task. Reads the GitHub issue body, sets up the worktree, invokes sw:backend-delivery (which runs plan-and-execute via sw:susu-plan + sw:susu-step), runs Lin at the end, handles findings, and signals Kamaji when Lin passes. One task per instance, ephemeral.",
    },
    "susu-fe": {
        "tools": ["Read", "Write", "Edit", "Glob", "Grep", "Bash", "ToolSearch", "Agent (sw:lin, sw:susu-step)", "Skill", "TodoWrite"],
        "description": "Use PROACTIVELY when spawned by Kamaji to implement a single frontend task. Reads the GitHub issue body, sets up the worktree, invokes sw:frontend-delivery (which runs plan-and-execute via sw:susu-plan + sw:susu-step), runs Lin at the end, handles findings, and signals Kamaji when Lin passes. One task per instance, ephemeral.",
    },
    "susu-step": {
        "tools": ["Read", "Write", "Edit", "Glob", "Grep", "Bash", "ToolSearch", "Skill", "TodoWrite"],
        "description": "Use PROACTIVELY when spawned by Big Susu (sw:susu-be or sw:susu-fe) to implement one plan step. Receives a step file path, reads exactly that file, implements the step to its exit criterion, commits locally (no push), returns a 3-line summary. Ephemeral — one step per instance, fresh context.",
    },
    "unit-test-runner": {
        "tools": None,  # "All tools"
        "description": "Use this agent when a meaningful chunk of code has been written or modified and needs unit tests created and executed. This includes new functions, classes, modules, bug fixes, or refactored code that requires test coverage validation.",
    },
}

AGENTS_DIR = Path.home() / ".claude" / "agents"


def split_frontmatter(text: str) -> tuple[dict, str]:
    """Return (frontmatter_dict, body) from a markdown file. Frontmatter
    is the YAML block between the first and second `---` lines."""
    m = re.match(r"^---\n(.*?\n)---\n?", text, re.DOTALL)
    if not m:
        return {}, text
    fm = yaml.safe_load(m.group(1)) or {}
    body = text[m.end():]
    return fm, body


def restore(slug: str, meta: dict) -> None:
    path = AGENTS_DIR / f"{slug}.md"
    if not path.exists():
        print(f"SKIP {slug}: not on disk")
        return
    text = path.read_text(encoding="utf-8")
    fm, body = split_frontmatter(text)
    model = fm.get("model", "sonnet")

    new_fm: dict = {
        "name": slug,
        "description": meta["description"],
    }
    if meta["tools"] is not None:
        new_fm["tools"] = meta["tools"]
    new_fm["model"] = model

    fm_yaml = yaml.safe_dump(
        new_fm,
        default_flow_style=False,
        allow_unicode=True,
        sort_keys=False,
        width=10_000,
    )
    new_text = f"---\n{fm_yaml}---\n{body.lstrip(chr(10))}"
    path.write_text(new_text, encoding="utf-8")
    print(f"OK   {slug}  desc={len(meta['description'])}  tools={'-' if meta['tools'] is None else len(meta['tools'])}")


def main() -> None:
    if not AGENTS_DIR.is_dir():
        print(f"no dir: {AGENTS_DIR}", file=sys.stderr)
        sys.exit(1)
    for slug, meta in AGENTS.items():
        restore(slug, meta)


if __name__ == "__main__":
    main()
