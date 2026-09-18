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
// merges rather than overwrites: the file also holds the employee's own
// settings, and replacing it would wipe them on every token refresh.
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

// matchTomlHeader reports whether trimmed is a `[table]` or
// `[[array-of-tables]]` header, with an optional trailing comment and
// nothing else on the line, and returns the name inside the brackets. It is
// only ever applied to a line the scanner already knows is not inside a
// multi-line string or an open array -- see scanTomlLines -- so it does not
// need to (and must not try to) rule out those cases itself: `[not a
// header]` in the middle of a multi-line array reads as a syntactically
// perfect header if judged on its text alone.
//
// This is a small hand-rolled scanner rather than a regexp because a TOML
// table name segment can be a quoted string (`[servers."a]b"]`), and a
// quoted string is free to contain a literal `]` -- a naive
// `\[\[?([^\]]+)\]\]?` stops at that first `]`, well short of the real one,
// and the header goes unrecognized (with the same consequences as any other
// missed header: its whole table reads as leftover content of whatever
// came before it).
func matchTomlHeader(trimmed string) (name string, ok bool) {
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return "", false
	}
	i := 1
	double := false
	if i < len(trimmed) && trimmed[i] == '[' {
		double = true
		i++
	}

	nameStart := i
	closeIdx := -1
	for i < len(trimmed) {
		switch trimmed[i] {
		case '"':
			i = skipSingleLineString(trimmed, i+1, '"', true)
			continue
		case '\'':
			i = skipSingleLineString(trimmed, i+1, '\'', false)
			continue
		case ']':
			closeIdx = i
		}
		if closeIdx != -1 {
			break
		}
		i++
	}
	if closeIdx == -1 {
		return "", false
	}

	name = strings.TrimSpace(trimmed[nameStart:closeIdx])
	if name == "" {
		return "", false
	}

	after := closeIdx + 1
	if double {
		if after >= len(trimmed) || trimmed[after] != ']' {
			return "", false
		}
		after++
	}

	tail := strings.TrimSpace(trimmed[after:])
	if tail != "" && !strings.HasPrefix(tail, "#") {
		return "", false
	}
	return name, true
}

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
//
// A genuine multi-line array or multi-line string always closes eventually
// (that is what makes it valid TOML), so after one pass over the whole file
// the scanner should be back in its fresh state: bracket depth zero and no
// triple-quoted string open. If it is not, something opened and never
// closed -- a stray `[`, or a `"""` or triple-apostrophe delimiter that
// runs to end of file -- almost always a typo rather than content that
// genuinely extends that far.
// Once that is known, the per-line state cannot be trusted for anything
// after the point it got stuck: a second pass re-classifies with recovery
// enabled, promoting any header-shaped line encountered while the scanner is
// still stuck -- inside the unclosed array *or* the unclosed string -- into
// a real header after all and resyncing the state to fresh there, rather
// than continuing to treat everything after it as the body of a value that
// is already known never to end.
//
// Left unrecovered, that stuck state either swallows the rest of the file (a
// managed root key's value that never closes drops everything after it,
// since nothing downstream is ever recognized as the header that would stop
// the drop) or, on a later merge, duplicates the delivered table: the real
// `[model_providers.gateway]` header is hidden inside the value that never
// closes, so it is never stripped before the new one is appended. That
// duplication is unbounded -- one more gateway table, and one more stale
// `experimental_bearer_token`, per delivery -- and the file never converges,
// so deploy.go rewrites config.toml forever. This is best-effort recovery
// for already-malformed input, not a fix for it -- content that genuinely
// belonged to the unterminated value is not recoverable by a line-oriented
// scanner and may still end up misplaced.
func scanTomlLines(src []byte) ([]tomlLine, error) {
	rawLines, err := splitTomlPhysicalLines(src)
	if err != nil {
		return nil, err
	}

	lines, end := classifyTomlLines(rawLines, false)
	if !end.fresh() {
		lines, _ = classifyTomlLines(rawLines, true)
	}
	return lines, nil
}

// splitTomlPhysicalLines splits src into its physical lines (scanTomlLines'
// only pass over the raw bytes; classifyTomlLines runs against the result,
// possibly more than once).
func splitTomlPhysicalLines(src []byte) ([]string, error) {
	var rawLines []string
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		rawLines = append(rawLines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan config.toml: %w", err)
	}
	return rawLines, nil
}

// classifyTomlLines is scanTomlLines' classification pass, run once with
// recoverUnterminated false and, only if that leaves the scanner mid-value
// at EOF, run again with it true. With it false this is exactly the state
// machine described on scanTomlLines. With it true, a header-shaped line
// encountered while the scanner is not fresh -- inside an unclosed array
// *or* inside an unclosed multi-line string -- is promoted to a real
// header and the scanner is resynced to fresh, instead of being treated as
// more content of a value that, this second time around, is already known
// never to close. The unclosed-string case matters as much as the unclosed
// bracket: without it a `notes = """` with no closing delimiter hides every
// later header, the delivered gateway table is appended once per merge, and
// the file grows without bound.
//
// It also returns the state the scan ends on, which is how scanTomlLines
// decides whether a second, recovering pass is warranted at all: for a
// well-formed file it always comes back fresh, so recovery is never invoked
// and behavior is unchanged from a single, non-recovering pass.
func classifyTomlLines(rawLines []string, recoverUnterminated bool) ([]tomlLine, tomlScanState) {
	lines := make([]tomlLine, 0, len(rawLines))
	var st tomlScanState

	for _, line := range rawLines {
		entering := st
		trimmed := strings.TrimSpace(line)

		isHeader := false
		header := ""
		switch {
		case entering.fresh():
			if name, ok := matchTomlHeader(trimmed); ok {
				isHeader = true
				header = name
			}
		case recoverUnterminated:
			// entering is not fresh: an array bracket, a triple-quoted
			// string, or both are still open from a line above. This pass
			// only runs when the file, scanned straight through, never
			// closed them, so treat the header as real and resync.
			if name, ok := matchTomlHeader(trimmed); ok {
				isHeader = true
				header = name
				st = tomlScanState{}
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
	return lines, st
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
// This is applied to every header except the managed gateway table, not
// just the first, which is what makes stripCodexManaged and
// splitTomlRootAndTables agree on where a table "starts" -- without it, a
// comment that this merge itself moved next to a later table (round one)
// would, on being merged again (round two), land back inside whatever the
// immediately preceding table happens to be, which is how the merge lost
// its idempotency: a comment documenting `[mcp_servers.foo]` got dropped as
// if it were still part of the managed gateway table above it, because only
// a header line reset "still inside the managed table" -- a trailing
// comment never did.
//
// The managed gateway table follows a narrower rule (see the loop below): it
// takes only a comment run written *directly* against its header, and a
// blank line ends the run rather than being absorbed into it.
//
// Blank-separated runs must stay where they are. stripCodexManaged uses this
// same map to decide what gets dropped wholesale with the managed table, and
// in an existing config.toml a blank-separated comment above that header is
// always the employee's: either their own note about their existing gateway
// config, or preserved root content that a previous merge's own assembly
// placed right before the delivered header -- appendTomlSection separates
// every section it writes with exactly one blank line, so anything this tool
// itself put there is blank-separated by construction. Deleting it because
// it merely sits next to a table about to be replaced is a second,
// independent way this merge would otherwise leak content on every re-merge.
//
// A directly adjacent run, by that same construction, can only have come
// from the delivered fragment, where it documents the managed table itself.
// Keeping it with that table is what stops it from accumulating: it is
// re-emitted from the fragment on every merge, so it must also be stripped
// with the table on every merge, exactly like the table's own body. Were it
// instead left in the preserved root, each delivery would add one more copy.
// Comments preceding every other header are unaffected: the whole
// comment/blank run still moves with that header.
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
		managed := ln.header == codexManagedTable
		for j := h - 1; j >= 0; j-- {
			trimmed := strings.TrimSpace(lines[j].text)
			if trimmed == "" {
				if managed {
					break // a blank line ends the managed table's run
				}
				owner[j] = h
				continue
			}
			if !strings.HasPrefix(trimmed, "#") {
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
