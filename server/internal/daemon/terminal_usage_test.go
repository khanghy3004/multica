package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseTranscriptUsage verifies per-model cumulative summing across turns,
// tolerance of non-usage / malformed lines, and that lines without a model or
// without usage are ignored.
func TestParseTranscriptUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	lines := []string{
		// Two assistant turns, same model — should sum.
		`{"type":"assistant","message":{"model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":5,"cache_read_input_tokens":100}}}`,
		`{"type":"assistant","message":{"model":"claude-opus-4-8","usage":{"input_tokens":3,"output_tokens":7,"cache_creation_input_tokens":1,"cache_read_input_tokens":40}}}`,
		// Different model — separate bucket.
		`{"type":"assistant","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		// A user line — no usage, must be ignored.
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		// Usage present but no model — ignored.
		`{"message":{"usage":{"input_tokens":999,"output_tokens":999}}}`,
		// Malformed JSON — skipped, not fatal.
		`{not json`,
		// Trailing line without newline handled by ReadBytes loop.
		`{"type":"assistant","message":{"model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	totals, err := parseTranscriptUsage(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	opus, ok := totals["claude-opus-4-8"]
	if !ok {
		t.Fatal("expected opus bucket")
	}
	// 10+3+1 input, 20+7+1 output, 5+1+0 cacheWrite, 100+40+0 cacheRead.
	if opus.input != 14 || opus.output != 28 || opus.cacheWrite != 6 || opus.cacheRead != 140 {
		t.Fatalf("opus totals wrong: %+v", opus)
	}

	haiku, ok := totals["claude-haiku-4-5"]
	if !ok {
		t.Fatal("expected haiku bucket")
	}
	if haiku.input != 1 || haiku.output != 2 {
		t.Fatalf("haiku totals wrong: %+v", haiku)
	}

	if len(totals) != 2 {
		t.Fatalf("expected exactly 2 model buckets, got %d: %+v", len(totals), totals)
	}
}

func TestLocateTranscriptMissing(t *testing.T) {
	if got := locateTranscript(""); got != "" {
		t.Fatalf("empty session id should yield no path, got %q", got)
	}
	if got := locateTranscript("00000000-0000-0000-0000-000000000000"); got != "" {
		t.Fatalf("nonexistent session should yield no path, got %q", got)
	}
}
