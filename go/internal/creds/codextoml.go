package creds

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"regexp"
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
// `key = value` lines and `[table]` headers, tracked carefully enough not to
// be fooled by a `[` that is actually inside a string or a multi-line array
// (see scanTomlLines). That is sufficient because the managed surface is
// fixed and small. Anything it cannot make sense of is carried through
// untouched.
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

	// Both the delivered fragment and what survived stripping split into a
	// root-level part (before the first real [table] header) and a tables
	// part (that header onward, with any comment/blank lines immediately
	// preceding it -- see splitTomlRootAndTables). Interleaving them as
	// deliveredRoot, keptRoot, deliveredTables, keptTables keeps every
	// preserved root key ahead of the delivered [model_providers.gateway]
	// header. Concatenating the delivered fragment then the preserved
	// remainder verbatim -- an earlier approach -- put the employee's root
	// keys after that header, where TOML silently treats a `key = value`
	// line as belonging to the preceding table: their settings quietly
	// became fields of the gateway provider instead of staying root-level,
	// and Codex simply ignored them.
	deliveredRoot, deliveredTables, err := splitTomlRootAndTables(incoming)
	if err != nil {
		return incoming, nil
	}
	keptRoot, keptTables, err := splitTomlRootAndTables(kept)
	if err != nil {
		return incoming, nil
	}

	var out bytes.Buffer
	appendTomlSection(&out, deliveredRoot)
	appendTomlSection(&out, keptRoot)
	appendTomlSection(&out, deliveredTables)
	appendTomlSection(&out, keptTables)
	return out.Bytes(), nil
}

// tomlHeaderRe matches a `[table]` or `[[array-of-tables]]` header, with an
// optional trailing comment, and nothing else on the line. It is only ever
// applied to a line the scanner already knows is not inside a multi-line
// string or an open array -- see scanTomlLines -- so it does not need to (and
// must not try to) rule out those cases itself: `[not a header]` in the
// middle of a multi-line array reads as a syntactically perfect header if
// judged on its text alone.
var tomlHeaderRe = regexp.MustCompile(`^\[\[?([^\]]+)\]\]?\s*(#.*)?$`)

// tomlLine is one physical line of a config.toml file, classified by
// scanTomlLines using real scanner state rather than the line's own text in
// isolation.
type tomlLine struct {
	text string

	// isHeader is true only for a line that is genuinely a table header:
	// not inside a multi-line string, not inside an unclosed array, and
	// matching tomlHeaderRe.
	isHeader bool
	// header is the table name inside the brackets, valid when isHeader.
	header string

	// continuation is true when this line is not a fresh statement of its
	// own but the tail of the previous line's value -- the body of a
	// multi-line array or a multi-line string that has not yet closed.
	// Such a line cannot be a header and, taken alone, rarely even looks
	// like a `key = value` assignment; callers that care about either
	// should instead let a continuation line simply share the fate of the
	// line that opened the value it belongs to.
	continuation bool
}

// tomlScanState is the scanner state carried from one line to the next: are
// we inside a `"""`-quoted or triple-apostrophe-quoted multi-line string,
// and how many `[`/`]` array brackets outside of any string are currently
// unclosed.
type tomlScanState struct {
	inTripleDouble bool
	inTripleSingle bool
	depth          int
}

func (s tomlScanState) fresh() bool {
	return !s.inTripleDouble && !s.inTripleSingle && s.depth == 0
}

// scanTomlLines walks src and classifies every line using a small
// TOML-aware state machine: it tracks whether the scanner is inside a
// multi-line basic (`"""`-quoted) or literal (triple-apostrophe-quoted)
// string, and the nesting depth of `[`/`]` array brackets outside of any
// string or comment. A line can only be a table header when the scanner is
// "fresh" at the start of that line (depth zero, not mid-string) --
// otherwise it is a continuation of whatever value is still open, however
// much it might look like a header on its own.
//
// This is what earlier, purely line-textual header detection got wrong: a
// line inside a multi-line array (`multi = [\n  "a",\n  [1, 2]\n]`) or a
// multi-line string (`notes = """\n[not a table]\n"""`) can read exactly
// like `[table]` in isolation. Treating it as a real header would sever the
// array or string apart, or -- inside the managed gateway table -- let the
// header-mimicking line prematurely end the "still inside the managed
// table" state and leak the rest of the table (including a stale bearer
// token) through as ordinary content.
func scanTomlLines(src []byte) ([]tomlLine, error) {
	var lines []tomlLine
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var st tomlScanState
	for scanner.Scan() {
		line := scanner.Text()
		entering := st

		isHeader := false
		header := ""
		if entering.fresh() {
			if m := tomlHeaderRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				isHeader = true
				header = strings.TrimSpace(m[1])
			}
		}

		if !isHeader {
			st = advanceTomlScanState(st, line)
		}

		lines = append(lines, tomlLine{
			text:         line,
			isHeader:     isHeader,
			header:       header,
			continuation: !isHeader && !entering.fresh(),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan config.toml: %w", err)
	}
	return lines, nil
}

// advanceTomlScanState scans one line's characters, given the state on
// entry, and returns the state that carries into the next line. It looks
// only for the handful of tokens that matter to header detection: the
// start/end of triple-quoted strings, single-line quoted strings (whose
// contents are skipped over so a `[` or `#` inside a string value is never
// mistaken for array or comment syntax), `#` comments, and unquoted `[`/`]`
// array brackets.
func advanceTomlScanState(st tomlScanState, line string) tomlScanState {
	i, n := 0, len(line)
	for i < n {
		if st.inTripleDouble {
			if strings.HasPrefix(line[i:], `"""`) {
				st.inTripleDouble = false
				i += 3
				continue
			}
			i++
			continue
		}
		if st.inTripleSingle {
			if strings.HasPrefix(line[i:], `'''`) {
				st.inTripleSingle = false
				i += 3
				continue
			}
			i++
			continue
		}

		switch {
		case strings.HasPrefix(line[i:], `"""`):
			st.inTripleDouble = true
			i += 3
		case strings.HasPrefix(line[i:], `'''`):
			st.inTripleSingle = true
			i += 3
		case line[i] == '"':
			i = skipSingleLineString(line, i+1, '"', true) // basic string: backslash escapes
		case line[i] == '\'':
			i = skipSingleLineString(line, i+1, '\'', false) // literal string: no escapes
		case line[i] == '#':
			i = n // rest of the line is a comment
		case line[i] == '[':
			st.depth++
			i++
		case line[i] == ']':
			if st.depth > 0 {
				st.depth--
			}
			i++
		default:
			i++
		}
	}
	return st
}

// skipSingleLineString returns the index just past the closing quote of a
// single-line string that opened at start-1, so the caller's scan does not
// look inside it for `[`, `]` or `#`. It is a best-effort skip, not a
// validator: an unterminated string (malformed TOML in the first place)
// simply runs to end of line, which matches this file's stance of never
// crashing on input it cannot fully make sense of.
func skipSingleLineString(line string, start int, quote byte, escapes bool) int {
	i, n := start, len(line)
	for i < n {
		if escapes && line[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if line[i] == quote {
			return i + 1
		}
		i++
	}
	return n
}

// tomlSectionOwners returns, for each line in lines, the index of the
// header that line belongs to, or -1 if the line sits at the root (before
// any header at all).
//
// A run of comment and/or blank lines immediately preceding a header
// belongs to that header's section, not to whatever table (or the root)
// came before it: such lines read as documenting the table that follows.
// This is applied uniformly to every header, not just the first, which is
// what makes stripCodexManaged and splitTomlRootAndTables agree on where a
// table "starts" -- without it, a comment that this merge itself moved
// next to a later table (round one) would, on being merged again (round
// two), land back inside whatever the immediately preceding table happens
// to be, which is how the merge lost its idempotency: a comment
// documenting `[mcp_servers.foo]` got dropped as if it were still part of
// the managed gateway table above it, because only a header line reset
// "still inside the managed table" -- a trailing comment never did.
func tomlSectionOwners(lines []tomlLine) []int {
	owner := make([]int, len(lines))
	current := -1
	for i, ln := range lines {
		if ln.isHeader {
			current = i
		}
		owner[i] = current
	}

	for h, ln := range lines {
		if !ln.isHeader {
			continue
		}
		for j := h - 1; j >= 0; j-- {
			trimmed := strings.TrimSpace(lines[j].text)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				break
			}
			owner[j] = h
		}
	}
	return owner
}

// splitTomlRootAndTables splits src at its first real [table] header (as
// classified by scanTomlLines, with comment/blank lines immediately before
// it reassigned by tomlSectionOwners): everything before that point is
// root-level content, and the header onward -- including all further
// tables -- is returned as tables. A src with no header at all comes back
// entirely as root, with tables empty.
func splitTomlRootAndTables(src []byte) (root, tables []byte, err error) {
	lines, err := scanTomlLines(src)
	if err != nil {
		return nil, nil, err
	}
	owner := tomlSectionOwners(lines)

	splitAt := len(lines)
	for i, o := range owner {
		if o != -1 {
			splitAt = i
			break
		}
	}

	return joinTomlLines(lines[:splitAt]), joinTomlLines(lines[splitAt:]), nil
}

func joinTomlLines(lines []tomlLine) []byte {
	var buf bytes.Buffer
	for _, ln := range lines {
		buf.WriteString(ln.text)
		buf.WriteString("\n")
	}
	return buf.Bytes()
}

// appendTomlSection appends section to out, with leading and trailing blank
// lines trimmed and exactly one blank line inserted to separate it from
// whatever out already holds. A section that is empty, or blank lines only,
// is skipped entirely -- contributing neither content nor a spurious
// separating blank line.
//
// Trimming (rather than only trimming trailing newlines, as an earlier
// version did) matters for idempotency: stripCodexManaged leaves behind the
// blank lines that used to separate the delivered content it removed from
// the preserved content around it, and if those accumulated on every merge
// the output would grow a blank line per delivery cycle, so
// bytes.Equal(previous, payload) in deploy.go would never hold and every
// delivery would rewrite config.toml forever.
func appendTomlSection(out *bytes.Buffer, section []byte) {
	trimmed := trimBlankTomlLines(section)
	if len(trimmed) == 0 {
		return
	}
	if out.Len() > 0 {
		out.WriteString("\n")
	}
	out.Write(trimmed)
	out.WriteString("\n")
}

// trimBlankTomlLines drops whole leading and trailing blank (whitespace-only)
// lines from section, leaving every interior line -- blank or not -- exactly
// as it was. It returns the content with no trailing newline; the caller
// adds one back. Operating line-by-line, rather than trimming the raw bytes,
// is what keeps this a boundary trim: raw byte trimming would also eat
// meaningful leading whitespace on the first surviving line.
func trimBlankTomlLines(section []byte) []byte {
	lines := strings.Split(string(section), "\n")
	// strings.Split on a section that (as every one here does) ends in "\n"
	// produces a trailing "" element; drop it so it is not treated as an
	// extra blank line at the tail.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if start >= end {
		return nil
	}
	return []byte(strings.Join(lines[start:end], "\n"))
}

// stripCodexManaged returns src with the managed root keys and the managed
// provider table removed, so the delivered version can be prepended without
// producing duplicate definitions.
//
// Duplicates matter: TOML rejects a repeated key outright, so leaving the old
// `model_provider` in place would make Codex fail to load the file rather
// than quietly prefer one of them.
func stripCodexManaged(src []byte) ([]byte, error) {
	lines, err := scanTomlLines(src)
	if err != nil {
		return nil, err
	}
	owner := tomlSectionOwners(lines)

	var out bytes.Buffer
	// dropRootContinuation tracks whether the line(s) continuing a managed
	// root key's multi-line value (an array spanning several lines, or a
	// multi-line string) must be dropped along with the key itself. Judged
	// on its own text, a continuation line essentially never looks like a
	// managed key -- or any key at all -- so it needs its fate handed down
	// from the line that opened the value.
	dropRootContinuation := false

	for i, ln := range lines {
		// managedTableOwner is true for the managed table's header, its
		// body, and any comment/blank run tomlSectionOwners reassigned to
		// it (a comment immediately above the header, or a trailing
		// comment/blank run below the previous line that turned out to
		// belong to this header instead). Using ownership rather than "have
		// we merely not yet seen a header" is what makes this idempotent:
		// a comment this merge itself moved to sit above some *other*,
		// later table must not be swept up here just because it physically
		// follows the managed table's last field and precedes the next
		// header -- only a genuine header line used to reset that state,
		// so a comment in that gap was silently dropped on every re-merge.
		managedTableOwner := owner[i] != -1 && lines[owner[i]].header == codexManagedTable

		if ln.continuation {
			if managedTableOwner || dropRootContinuation {
				continue
			}
			out.WriteString(ln.text)
			out.WriteString("\n")
			continue
		}

		if ln.isHeader {
			dropRootContinuation = false
			if ln.header == codexManagedTable {
				continue
			}
			out.WriteString(ln.text)
			out.WriteString("\n")
			continue
		}

		// Drop the body of the managed table along with its header. Because
		// isHeader now only fires on a genuine header (see scanTomlLines),
		// a nested array inside the managed table's own fields can no
		// longer be mistaken for the next header and end this early --
		// which used to let the rest of the table, stale bearer token
		// included, leak through as if it were ordinary preserved content.
		if managedTableOwner {
			continue
		}

		if owner[i] == -1 { // at the root, before any header
			trimmed := strings.TrimSpace(ln.text)
			if key, ok := tomlRootKey(trimmed); ok && codexManagedKeys[key] {
				dropRootContinuation = true
				continue
			}
		}

		dropRootContinuation = false
		out.WriteString(ln.text)
		out.WriteString("\n")
	}
	return out.Bytes(), nil
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
