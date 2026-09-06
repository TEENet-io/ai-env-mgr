package creds

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
)

// codexManagedKeys are the root-level config.toml keys this tool owns.
//
// Everything outside this list belongs to the employee: approval policy,
// sandbox settings, MCP servers they wired up themselves. Delivering a
// gateway configuration must not cost them any of that, which is why this
// merges rather than overwrites -- the same reason mergeClaudeConfig exists
// for .claude.json.
var codexManagedKeys = map[string]bool{
	"model":                  true,
	"model_provider":         true,
	"model_catalog_json":     true,
	"web_search":             true,
	"stream_idle_timeout_ms": true,
}

// codexManagedTable is the one table this tool owns outright. It is replaced
// wholesale because a half-updated provider block (new base_url, stale token)
// is worse than either version alone.
const codexManagedTable = "model_providers.gateway"

// mergeCodexConfig folds the delivered gateway settings into the employee's
// existing config.toml, preserving every line this tool does not own.
//
// It deliberately does not parse TOML in full. The repository keeps its
// dependency set minimal, and a complete parser would also *rewrite* the
// file -- discarding comments, reordering keys, normalizing quoting -- which
// turns "we added a provider" into "your config looks nothing like it did".
// A line-oriented edit touches only what it must and leaves the rest byte
// for byte.
//
// The tradeoff is that it understands only what it needs to: root-level
// `key = value` lines and `[table]` headers. That is sufficient because the
// managed surface is fixed and small. Anything it cannot make sense of is
// carried through untouched.
func mergeCodexConfig(target string, incoming []byte) ([]byte, error) {
	existing, err := os.ReadFile(target)
	if err != nil {
		// No file yet, or unreadable: the delivered config is the whole truth.
		return incoming, nil
	}
	if len(bytes.TrimSpace(existing)) == 0 {
		return incoming, nil
	}

	kept, err := stripCodexManaged(existing)
	if err != nil {
		// Something about the file defeats the line scanner. Writing the
		// delivered config verbatim at least leaves Codex able to start,
		// which beats preserving a file we cannot reason about and having
		// the employee end up with no working provider at all.
		return incoming, nil
	}

	var out bytes.Buffer
	out.Write(bytes.TrimRight(incoming, "\n"))
	out.WriteString("\n")
	if trimmed := bytes.TrimSpace(kept); len(trimmed) > 0 {
		out.WriteString("\n")
		out.Write(bytes.TrimRight(kept, "\n"))
		out.WriteString("\n")
	}
	return out.Bytes(), nil
}

// stripCodexManaged returns src with the managed root keys and the managed
// provider table removed, so the delivered version can be prepended without
// producing duplicate definitions.
//
// Duplicates matter: TOML rejects a repeated key outright, so leaving the old
// `model_provider` in place would make Codex fail to load the file rather
// than quietly prefer one of them.
func stripCodexManaged(src []byte) ([]byte, error) {
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	inRoot := true          // before the first [table] header
	inManagedTable := false // inside [model_providers.gateway]

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if header, ok := tomlTableHeader(trimmed); ok {
			inRoot = false
			inManagedTable = header == codexManagedTable
			if inManagedTable {
				continue
			}
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}

		// Drop the body of the managed table along with its header.
		if inManagedTable {
			continue
		}

		if inRoot {
			if key, ok := tomlRootKey(trimmed); ok && codexManagedKeys[key] {
				continue
			}
		}

		out.WriteString(line)
		out.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan config.toml: %w", err)
	}
	return out.Bytes(), nil
}

// tomlTableHeader reports whether trimmed is a `[table]` or `[[array]]`
// header and returns the name inside the brackets.
func tomlTableHeader(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]") // [[array]]
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	return name, true
}

// tomlRootKey returns the key of a `key = value` line, or false for comments,
// blanks and anything that is not a simple assignment.
//
// Quoted keys are not recognized, and that is intentional: none of the
// managed keys are ever written quoted, so a quoted `"model"` in the
// employee's file is theirs, not ours, and must survive.
func tomlRootKey(trimmed string) (string, bool) {
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	eq := strings.Index(trimmed, "=")
	if eq <= 0 {
		return "", false
	}
	key := strings.TrimSpace(trimmed[:eq])
	if key == "" || strings.ContainsAny(key, " \t\"'[]") {
		return "", false
	}
	return key, true
}
