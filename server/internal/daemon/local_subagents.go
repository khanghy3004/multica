package daemon

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxLocalSubagentFileSize int64 = 256 << 10
	subagentDelimiter              = "---"
)

// LocalSubagent is one parsed `~/.claude/agents/<slug>.md` file.
type LocalSubagent struct {
	Path        string
	Slug        string
	Name        string
	Description string
	Model       string
	Tools       []string
	Body        string
	Extra       map[string]any
	Mtime       time.Time
}

// Frontmatter is the YAML block at the top of a subagent file.
type Frontmatter struct {
	Name        string
	Description string
	Model       string
	Tools       []string
	Extra       map[string]any
}

// subagentRootForProvider returns the directory the provider keeps subagent
// files in. The bool reports whether the provider has any subagent surface
// at all — false for every provider that does not expose one today.
func subagentRootForProvider(provider string) (string, bool, error) {
	switch provider {
	case "claude":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", true, err
		}
		return filepath.Join(home, ".claude", "agents"), true, nil
	default:
		return "", false, nil
	}
}

// listLocalSubagents enumerates parseable .md subagents under the provider's
// root, sorted by slug. Malformed files are skipped; supported=false means
// the provider has no subagent surface at all.
func listLocalSubagents(provider string) (out []LocalSubagent, supported bool, err error) {
	root, supported, err := subagentRootForProvider(provider)
	if err != nil || !supported {
		return nil, supported, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, true, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		la, perr := parseLocalSubagent(filepath.Join(root, e.Name()))
		if perr != nil {
			continue
		}
		out = append(out, la)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, true, nil
}

// parseLocalSubagent reads a subagent .md and returns its frontmatter + body.
// An error is returned when no `---` frontmatter block is present so a body-
// only file is never silently treated as a valid subagent.
func parseLocalSubagent(path string) (LocalSubagent, error) {
	info, err := os.Stat(path)
	if err != nil {
		return LocalSubagent{}, err
	}
	if info.Size() > maxLocalSubagentFileSize {
		return LocalSubagent{}, fmt.Errorf("subagent file too large: %d bytes", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return LocalSubagent{}, err
	}

	fmBytes, body, ok := splitFrontmatter(data)
	if !ok {
		return LocalSubagent{}, errors.New("missing yaml frontmatter")
	}

	raw := map[string]any{}
	if err := yaml.Unmarshal(fmBytes, &raw); err != nil {
		// Real-world Claude Code subagent files routinely have unquoted
		// `description:` values that contain colons (e.g. "Triggers on:
		// 'run X'") which violate YAML's mapping-value-after-key rule
		// and trip yaml.v3. Fall back to a line-based key/value parser
		// that splits on the first ":" of each top-level line. The
		// fallback only recovers scalar fields — list-valued fields like
		// `tools` are recovered when they sit on a single line in JSON
		// flow form (`tools: ["Read", "Edit"]`) but ignored when split
		// across multiple lines. This matches what we see in practice;
		// the alternative (refusing to import) is worse for the user.
		raw = parseFrontmatterLineFallback(fmBytes)
		if len(raw) == 0 {
			return LocalSubagent{}, fmt.Errorf("yaml: %w", err)
		}
	}

	slug := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	la := LocalSubagent{
		Path:  path,
		Slug:  slug,
		Body:  string(body),
		Mtime: info.ModTime(),
		Extra: map[string]any{},
	}
	for k, v := range raw {
		switch k {
		case "name":
			la.Name, _ = v.(string)
		case "description":
			la.Description, _ = v.(string)
		case "model":
			la.Model, _ = v.(string)
		case "tools":
			la.Tools = anyToStringSlice(v)
		default:
			la.Extra[k] = v
		}
	}
	if la.Name == "" {
		la.Name = slug
	}
	return la, nil
}

func splitFrontmatter(data []byte) (fm, body []byte, ok bool) {
	r := bufio.NewReader(bytes.NewReader(data))
	first, err := r.ReadString('\n')
	if err != nil || strings.TrimRight(first, "\r\n") != subagentDelimiter {
		return nil, nil, false
	}
	var fmBuf bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, nil, false
		}
		if strings.TrimRight(line, "\r\n") == subagentDelimiter {
			rest, _ := readAllFromReader(r)
			return fmBuf.Bytes(), rest, true
		}
		fmBuf.WriteString(line)
	}
}

func readAllFromReader(r *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	if _, err := out.ReadFrom(r); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func anyToStringSlice(v any) []string {
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if s, ok := v.(string); ok {
		// Fallback path stores `tools` as a raw string (e.g.
		// `["Read", "Edit"]` or `[Read, Edit]`). Best-effort JSON
		// decode; if that fails, split on commas after stripping
		// brackets.
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		out := []string{}
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			part = strings.Trim(part, "\"'")
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	return nil
}

// parseFrontmatterLineFallback recovers key/value pairs from a YAML block
// that yaml.Unmarshal rejected. Splits on the first ":" of each line,
// trims whitespace, and stores everything as raw strings. Lines that
// don't contain ":" are appended to the previous key's value (best-effort
// continuation handling). The caller's switch statement coerces values
// per key.
func parseFrontmatterLineFallback(data []byte) map[string]any {
	out := map[string]any{}
	var lastKey string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.Index(line, ":")
		// Treat as a continuation only when this line is INDENTED
		// (i.e. starts with whitespace) — top-level lines whose value
		// happens to contain a colon (e.g. `Triggers on:` in the
		// middle of a description) still have idx > 0 but should
		// remain key lines.
		isContinuation := idx < 0 || (len(line) > 0 && (line[0] == ' ' || line[0] == '\t'))
		if isContinuation {
			if lastKey == "" {
				continue
			}
			if cur, ok := out[lastKey].(string); ok {
				out[lastKey] = cur + " " + trimmed
			}
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		value = strings.Trim(value, "\"")
		out[key] = value
		lastKey = key
	}
	return out
}

// writeLocalSubagent atomically writes the file (temp + rename) and forces
// its mtime to <mtime>. Forcing the mtime stops the next heartbeat from
// looking "newer than DB" and bouncing the push back as a pull.
func writeLocalSubagent(path, body string, fm Frontmatter, mtime time.Time) error {
	doc := map[string]any{}
	for k, v := range fm.Extra {
		doc[k] = v
	}
	if fm.Name != "" {
		doc["name"] = fm.Name
	}
	if fm.Description != "" {
		doc["description"] = fm.Description
	}
	if fm.Model != "" {
		doc["model"] = fm.Model
	}
	if len(fm.Tools) > 0 {
		doc["tools"] = fm.Tools
	}

	var buf bytes.Buffer
	buf.WriteString(subagentDelimiter + "\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	enc.Close()
	buf.WriteString(subagentDelimiter + "\n")
	buf.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		buf.WriteString("\n")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".subagent-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Chtimes(path, mtime, mtime)
}

// deleteLocalSubagent removes the file. Missing-file is not an error: the
// reconciler may have already archived its row and a retry should be
// idempotent.
func deleteLocalSubagent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
