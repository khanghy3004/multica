package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseLocalSubagent_RoundTrip(t *testing.T) {
	body := "You are a careful refactorer.\n\nDo not change behaviour.\n"
	src := `---
name: refactorer
description: Use for non-behavioural cleanups.
model: claude-opus-4-7
tools:
  - Read
  - Edit
  - Grep
custom_flag: keep-this
---
` + body

	dir := t.TempDir()
	path := filepath.Join(dir, "refactorer.md")
	mustWrite(t, path, src)

	got, err := parseLocalSubagent(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Slug != "refactorer" {
		t.Errorf("slug = %q, want refactorer", got.Slug)
	}
	if got.Name != "refactorer" {
		t.Errorf("name = %q, want refactorer", got.Name)
	}
	if got.Description != "Use for non-behavioural cleanups." {
		t.Errorf("description = %q", got.Description)
	}
	if got.Model != "claude-opus-4-7" {
		t.Errorf("model = %q", got.Model)
	}
	if strings.Join(got.Tools, ",") != "Read,Edit,Grep" {
		t.Errorf("tools = %v", got.Tools)
	}
	if !strings.HasPrefix(got.Body, "You are a careful refactorer.") {
		t.Errorf("body lost: %q", got.Body)
	}
	if got.Extra["custom_flag"] != "keep-this" {
		t.Errorf("extra lost: %v", got.Extra)
	}
}

func TestParseLocalSubagent_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.md")
	mustWrite(t, path, "just a body\n")
	if _, err := parseLocalSubagent(path); err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}

func TestParseLocalSubagent_NameDefaultsToSlug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unnamed.md")
	mustWrite(t, path, "---\ndescription: no name field\n---\nbody\n")
	got, err := parseLocalSubagent(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Name != "unnamed" {
		t.Errorf("name = %q, want unnamed (slug)", got.Name)
	}
}

func TestWriteLocalSubagent_AtomicAndPreservesMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "writer.md")
	want := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	err := writeLocalSubagent(path, "Body text.", Frontmatter{
		Name:        "writer",
		Description: "desc",
		Model:       "claude-opus-4-7",
		Tools:       []string{"Read"},
	}, want)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(want) {
		t.Errorf("mtime = %v, want %v", info.ModTime(), want)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || strings.HasPrefix(e.Name(), ".subagent-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteThenParse_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roundtrip.md")
	mtime := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := writeLocalSubagent(path, "Hello body.\n", Frontmatter{
		Name:        "roundtrip",
		Description: "desc",
		Model:       "claude-opus-4-7",
		Tools:       []string{"Read", "Edit"},
		Extra:       map[string]any{"custom_flag": "preserved"},
	}, mtime); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := parseLocalSubagent(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Name != "roundtrip" || got.Description != "desc" || got.Model != "claude-opus-4-7" {
		t.Errorf("scalar fields lost: %+v", got)
	}
	if strings.Join(got.Tools, ",") != "Read,Edit" {
		t.Errorf("tools = %v", got.Tools)
	}
	if got.Extra["custom_flag"] != "preserved" {
		t.Errorf("extra lost: %v", got.Extra)
	}
}

func TestListLocalSubagents_FiltersAndSorts(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "zeta.md"), "---\nname: zeta\n---\nbody z\n")
	mustWrite(t, filepath.Join(root, "alpha.md"), "---\nname: alpha\n---\nbody a\n")
	mustWrite(t, filepath.Join(root, "midfile.md"), "---\nname: mid\n---\nbody m\n")
	mustWrite(t, filepath.Join(root, "noyaml.md"), "no frontmatter\n")
	mustWrite(t, filepath.Join(root, "ignore.txt"), "---\nname: nope\n---\n")

	t.Setenv("HOME", home)
	got, supported, err := listLocalSubagents("claude")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !supported {
		t.Fatal("expected supported=true")
	}
	if len(got) != 3 {
		t.Fatalf("len = %d (%+v), want 3", len(got), got)
	}
	if got[0].Slug != "alpha" || got[1].Slug != "midfile" || got[2].Slug != "zeta" {
		t.Errorf("not sorted by slug: %v", []string{got[0].Slug, got[1].Slug, got[2].Slug})
	}
}

func TestListLocalSubagents_MissingDirIsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, supported, err := listLocalSubagents("claude")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !supported {
		t.Fatal("expected supported=true even when dir absent")
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

func TestListLocalSubagents_UnsupportedProvider(t *testing.T) {
	_, supported, err := listLocalSubagents("opencode")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if supported {
		t.Error("expected supported=false for opencode")
	}
}

func TestDeleteLocalSubagent_IdempotentOnMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.md")
	if err := deleteLocalSubagent(path); err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
	mustWrite(t, path, "x")
	if err := deleteLocalSubagent(path); err != nil {
		t.Errorf("delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still present: %v", err)
	}
}
