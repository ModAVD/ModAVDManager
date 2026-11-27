package i18n

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// go test ./internal/i18n -run TestCatalog -update adds the Chinese strings of the source that the tables lack, as ""
// (to translate) in locales/<lang>/<package>.json; -prune also drops keys the source no longer has.
var (
	update = flag.Bool("update", false, "add missing keys to the tables")
	prune  = flag.Bool("prune", false, "with -update: drop keys the source no longer has")
)

// repoRoot is the module root (two levels up from this package).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// sourceKey is one string the code shows, and the table (file name without .json) it belongs to.
type sourceKey struct {
	key, group, pos string
}

// goKeys extracts the Chinese strings of every non-test Go file: string literals, a concatenation chain as one key
// with %v for each operand that is not a literal.
func goKeys(t *testing.T, root string) []sourceKey {
	t.Helper()
	var out []sourceKey
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "web" || name == "i18n") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		group := groupOf(filepath.ToSlash(rel))
		fs := token.NewFileSet()
		f, err := parser.ParseFile(fs, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		inChain := map[ast.Node]bool{}
		var tags = map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Field:
				if n.Tag != nil {
					tags[n.Tag] = true
				}
			case *ast.BinaryExpr:
				if n.Op != token.ADD || inChain[n] {
					return true
				}
				ops := flattenAdd(n, inChain)
				if !anyStringLit(ops) {
					return true
				}
				var b strings.Builder
				for _, op := range ops {
					if bl, ok := op.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						v, _ := strconv.Unquote(bl.Value)
						b.WriteString(v)
						inChain[bl] = true
					} else {
						b.WriteString("%v")
					}
				}
				if k := b.String(); HasHan(k) {
					out = append(out, sourceKey{k, group, fs.Position(n.Pos()).String()})
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING || inChain[n] || tags[n] {
					return true
				}
				v, err := strconv.Unquote(n.Value)
				if err == nil && HasHan(v) {
					out = append(out, sourceKey{v, group, fs.Position(n.Pos()).String()})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// flattenAdd lists the operands of a chain of + (parentheses included), marking the inner nodes.
func flattenAdd(e ast.Expr, seen map[ast.Node]bool) []ast.Expr {
	switch x := e.(type) {
	case *ast.ParenExpr:
		if b, ok := x.X.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			return flattenAdd(b, seen)
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			seen[x] = true
			return append(flattenAdd(x.X, seen), flattenAdd(x.Y, seen)...)
		}
	}
	return []ast.Expr{e}
}

func anyStringLit(ops []ast.Expr) bool {
	for _, op := range ops {
		if bl, ok := op.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			return true
		}
	}
	return false
}

// groupOf: internal/<pkg>/… is <pkg>, cmd/<name>/… is <name>, the root package is cli.
func groupOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) >= 3 && (parts[0] == "internal" || parts[0] == "cmd") {
		return parts[1]
	}
	return "cli"
}

// webDir holds the page.
func webDir(root string) string { return filepath.Join(root, "internal", "ui", "web") }

// webKeys are the strings the page passes to t() and the data-t texts of index.html; untranslated lists the Chinese
// literals outside t() (each a bug: the page would show it in every language).
func webKeys(t *testing.T, root string) (keys []sourceKey, untranslated []string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(webDir(root), "js", "*.js"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		lines := strings.Split(string(b), "\n")
		for _, s := range scanJS(string(b)) {
			if !HasHan(s.text) {
				continue
			}
			// "// i18n: keep" on the line: Chinese on purpose (a language's own name, text matched as it is).
			if s.line-1 < len(lines) && strings.Contains(lines[s.line-1], "i18n: keep") {
				continue
			}
			pos := fmt.Sprintf("%s:%d", name, s.line)
			if s.inT {
				keys = append(keys, sourceKey{s.text, "web", pos})
			} else {
				untranslated = append(untranslated, fmt.Sprintf("%s %q", pos, s.text))
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(webDir(root), "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range htmlKeys(string(b)) {
		keys = append(keys, sourceKey{k, "web", "index.html"})
	}
	// CSS text (content: "…") cannot be translated: use content: attr(data-…) with the attribute set through t().
	css, err := os.ReadFile(filepath.Join(webDir(root), "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cssContentRE.FindAllString(commentCSSRE.ReplaceAllString(string(css), ""), -1) {
		if HasHan(m) {
			untranslated = append(untranslated, "app.css "+m)
		}
	}
	if rest := htmlUntranslated(string(b)); len(rest) > 0 {
		for _, r := range rest {
			untranslated = append(untranslated, "index.html "+strconv.Quote(r))
		}
	}
	return keys, untranslated
}

type jsString struct {
	text string
	line int
	inT  bool // the first argument of t( … )
}

// scanJS lists the string literals of a script (comments skipped; a template literal is one string, its ${…} parts
// scanned too). A literal is inT when it directly follows "t(" or ", t(" style calls: the identifier t, then "(".
func scanJS(src string) []jsString {
	var out []jsString
	line := 1
	lastIdent, lastTok := "", ""
	prevIdent := "" // identifier before the last "("
	i := 0
	var scan func(end byte) // scans until end (0: end of input)
	readQuoted := func(q byte) string {
		var b strings.Builder
		i++ // opening quote
		for i < len(src) && src[i] != q {
			if src[i] == '\\' && i+1 < len(src) {
				switch src[i+1] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case '\n':
					line++
				default:
					b.WriteByte(src[i+1])
				}
				i += 2
				continue
			}
			if src[i] == '\n' {
				line++
			}
			b.WriteByte(src[i])
			i++
		}
		i++ // closing quote
		return b.String()
	}
	readTemplate := func() string {
		var b strings.Builder
		i++
		for i < len(src) && src[i] != '`' {
			if src[i] == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i += 2
				continue
			}
			if src[i] == '$' && i+1 < len(src) && src[i+1] == '{' {
				b.WriteString("${…}")
				i += 2
				scan('}')
				i++
				continue
			}
			if src[i] == '\n' {
				line++
			}
			b.WriteByte(src[i])
			i++
		}
		i++
		return b.String()
	}
	regexAllowed := func() bool {
		return lastTok == "" || strings.Contains("(,=:[!&|?{};+-*%<>~^", lastTok) || lastIdent == "return" && lastTok == "ident"
	}
	scan = func(end byte) {
		depth := 0
		for i < len(src) {
			c := src[i]
			switch {
			case end != 0 && c == end && depth == 0:
				return
			case c == '{':
				depth++
				lastTok = "{"
				i++
			case c == '}':
				depth--
				lastTok = "}"
				i++
			case c == '\n':
				line++
				i++
			case c == ' ' || c == '\t' || c == '\r':
				i++
			case strings.HasPrefix(src[i:], "//"):
				for i < len(src) && src[i] != '\n' {
					i++
				}
			case strings.HasPrefix(src[i:], "/*"):
				j := strings.Index(src[i+2:], "*/")
				if j < 0 {
					i = len(src)
					break
				}
				line += strings.Count(src[i:i+2+j], "\n")
				i += j + 4
			case c == '\'' || c == '"':
				l := line
				s := readQuoted(c)
				out = append(out, jsString{s, l, lastTok == "(" && prevIdent == "t"})
				lastTok, lastIdent = "str", ""
			case c == '`':
				l := line
				s := readTemplate()
				out = append(out, jsString{s, l, false})
				lastTok, lastIdent = "str", ""
			case c == '/' && regexAllowed():
				i++
				inClass := false
				for i < len(src) && (src[i] != '/' || inClass) && src[i] != '\n' {
					if src[i] == '\\' {
						i++
					} else if src[i] == '[' {
						inClass = true
					} else if src[i] == ']' {
						inClass = false
					}
					i++
				}
				i++
				for i < len(src) && isIdentByte(src[i]) {
					i++
				}
				lastTok, lastIdent = "regex", ""
			case isIdentByte(c):
				j := i
				for j < len(src) && isIdentByte(src[j]) {
					j++
				}
				lastIdent = src[i:j]
				lastTok = "ident"
				i = j
			default:
				if c == '(' {
					if lastTok == "ident" {
						prevIdent = lastIdent
					} else {
						prevIdent = ""
					}
				}
				lastTok = string(c)
				lastIdent = ""
				i++
			}
		}
	}
	scan(0)
	return out
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

var (
	dataTRE      = regexp.MustCompile(`(?s)<([a-z0-9]+)[^>]*\sdata-t(?:\s|>|=)[^>]*>(.*?)</[a-z0-9]+>`)
	attrRE       = regexp.MustCompile(`\s(title|placeholder|aria-label|alt)="([^"]*)"`)
	tagRE        = regexp.MustCompile(`(?s)<[^>]*>`)
	commentRE    = regexp.MustCompile(`(?s)<!--.*?-->`)
	cssContentRE = regexp.MustCompile(`content:\s*("[^"]*"|'[^']*')`)
	commentCSSRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	scriptRE     = regexp.MustCompile(`(?s)<script.*?</script>`)
)

// htmlKeys: the text of each element marked data-t (no child elements), and every Chinese title / placeholder /
// aria-label / alt attribute (the page translates those wherever they are).
func htmlKeys(src string) []string {
	src = commentRE.ReplaceAllString(src, "")
	var out []string
	for _, m := range dataTRE.FindAllStringSubmatch(src, -1) {
		if s := strings.TrimSpace(m[2]); HasHan(s) {
			out = append(out, s)
		}
	}
	for _, m := range attrRE.FindAllStringSubmatch(src, -1) {
		if HasHan(m[2]) {
			out = append(out, m[2])
		}
	}
	return out
}

// htmlUntranslated is Chinese text of index.html outside data-t elements and the attributes above.
func htmlUntranslated(src string) []string {
	src = commentRE.ReplaceAllString(src, "")
	src = scriptRE.ReplaceAllString(src, "")
	src = dataTRE.ReplaceAllString(src, "")
	src = attrRE.ReplaceAllString(src, "")
	var out []string
	for _, piece := range tagRE.Split(src, -1) {
		if s := strings.TrimSpace(piece); HasHan(s) {
			out = append(out, s)
		}
	}
	// Chinese left inside a tag: an attribute the page does not translate.
	for _, tag := range tagRE.FindAllString(src, -1) {
		if HasHan(tag) {
			out = append(out, tag)
		}
	}
	return out
}

// table is one locales/<lang>/<group>.json: nil values are kept untranslated on purpose.
type table map[string]*string

func readTable(t *testing.T, path string) table {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return table{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var tb table
	if err := json.Unmarshal(b, &tb); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return tb
}

func writeTable(t *testing.T, path string, tb table) {
	t.Helper()
	keys := make([]string, 0, len(tb))
	for k := range tb {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("{\n")
	for n, k := range keys {
		kb, _ := marshalNoEscape(k)
		b.WriteString("  ")
		b.Write(kb)
		b.WriteString(": ")
		if tb[k] == nil {
			b.WriteString("null")
		} else {
			vb, _ := marshalNoEscape(*tb[k])
			b.Write(vb)
		}
		if n < len(keys)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func marshalNoEscape(s string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// TestCatalog checks every table against the source: each string the code or the page shows has an entry (null or
// a translation, not ""), no entry is stale, and a translation uses the same arguments as its key.
func TestCatalog(t *testing.T) {
	root := repoRoot(t)
	src := goKeys(t, root)
	web, untranslated := webKeys(t, root)
	src = append(src, web...)
	for _, u := range untranslated {
		t.Errorf("page text outside t(): %s", u)
	}
	want := map[string]map[string]string{} // group → key → first position
	for _, k := range src {
		if want[k.group] == nil {
			want[k.group] = map[string]string{}
		}
		if _, ok := want[k.group][k.key]; !ok {
			want[k.group][k.key] = k.pos
		}
	}
	for _, lang := range tableCodes() {
		dir := filepath.Join("locales", lang)
		groups := map[string]bool{}
		for g := range want {
			groups[g] = true
		}
		existing, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, f := range existing {
			groups[strings.TrimSuffix(filepath.Base(f), ".json")] = true
		}
		var missing []string
		for g := range groups {
			path := filepath.Join(dir, g+".json")
			tb := readTable(t, path)
			changed := false
			for k, pos := range want[g] {
				v, ok := tb[k]
				if !ok {
					if *update {
						empty := ""
						tb[k] = &empty
						changed = true
					}
					missing = append(missing, fmt.Sprintf("%s: %s %q", lang, pos, k))
					continue
				}
				if v != nil && *v == "" {
					missing = append(missing, fmt.Sprintf("%s: %s %q (empty)", lang, pos, k))
				}
			}
			for k, v := range tb {
				// extra.json: messages the code composes from parts without a key of their own (see README).
				if _, ok := want[g][k]; !ok && g != "extra" {
					if *update && *prune {
						delete(tb, k)
						changed = true
						continue
					}
					t.Errorf("%s/%s.json: stale key %q", lang, g, k)
					continue
				}
				if v != nil && *v != "" {
					if err := sameArgs(g, k, *v); err != nil {
						t.Errorf("%s/%s.json %q: %v", lang, g, k, err)
					}
				}
			}
			if changed {
				writeTable(t, path, tb)
			}
		}
		// One key, one translation: the tables are merged into one.
		seen := map[string]string{}
		for g := range groups {
			if g == "web" {
				continue // the page's own table, looked up on its own
			}
			for k, v := range readTable(t, filepath.Join(dir, g+".json")) {
				if v == nil || *v == "" {
					continue
				}
				k, val := strings.TrimSpace(k), strings.TrimSpace(*v)
				if prev, ok := seen[k]; ok && prev != val {
					t.Errorf("%s: %q has two translations: %q and %q (%s.json)", lang, k, prev, val, g)
				}
				seen[k] = val
			}
		}
		sort.Strings(missing)
		if n := len(missing); n > 0 {
			show := missing
			if len(show) > 40 {
				show = show[:40]
			}
			t.Errorf("%d strings not translated (go test ./internal/i18n -run TestCatalog -update adds them):\n%s", n, strings.Join(show, "\n"))
		}
	}
}

var braceRE = regexp.MustCompile(`\{(\w+)\}`)

// sameArgs: a Go key and its translation take the same arguments (an explicit index may reorder them); a page key
// and its translation the same {names}.
func sameArgs(group, key, val string) error {
	if group == "web" {
		a, b := braceSet(key), braceSet(val)
		if a != b {
			return fmt.Errorf("placeholders %s, translation has %s", a, b)
		}
		return nil
	}
	a, b := argSet(parseFormat(key)), argSet(parseFormat(val))
	if a != b {
		return fmt.Errorf("arguments %s, translation has %s", a, b)
	}
	return nil
}

func braceSet(s string) string {
	set := map[string]bool{}
	for _, m := range braceRE.FindAllStringSubmatch(s, -1) {
		set[m[1]] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "{" + strings.Join(keys, ",") + "}"
}

func argSet(segs []segment) string {
	set := map[int]bool{}
	for _, s := range segs {
		if s.verb != "" {
			set[s.arg] = true
		}
	}
	keys := make([]int, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return fmt.Sprint(keys)
}
