// Package report is the read-only diagnostics behind `modavd report-config` and `modavd collect-info`: ports of the
// ModAVD package scripts report-config.ps1 and collect-info.ps1 (package/ in the ModAVD repository), with the same
// text, the same zip layout and the same rules as their headers state. Neither command starts an emulator, an adb
// server or adb.exe, and neither touches port 5037: installed apps and the live state of a running instance are
// asked over the adb host protocol, and only of a server that already listens.
//
// Everything the scripts ask Windows goes through Host, so the tests compare the produced text line by line with
// what the scripts print for the same answers (testdata/). Where the scripts format with .NET (-f '{0:N1}',
// Sort-Object, Get-ChildItem order), the helpers below reproduce Windows PowerShell 5.1.
// Apple Silicon macOS has its own native Host and a bounded, privacy-filtered
// report layout, with existing-server and instance ownership checks before ADB.
package report

import (
	"bytes"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// PrivacyNote is the head of summary.txt and the last thing printed: collect-info.ps1's $PrivacyNote word for word,
// but for the first line. The script's said "脚本不会上传"; from ModAVD 0.38.2 the package has no such script any
// more (ModAVD/ModAVD#45), and what collects and what could upload is the manager or the CLI, so it says those.
const PrivacyNote = "隐私提示：问题包只保存在本机，管理器和命令行都不会上传它。\r\n" +
	"- 里面有：summary.txt（电脑和模拟器的情况）、模拟器日志、实例配置、AUTO-MAS 日志（mas 文件夹）；有实例在运行时还有现场截图和安卓状态（live 文件夹）；模拟器近 7 天崩溃过的话还有崩溃转储（crash 文件夹）。不收磁盘镜像，不收 AUTO-MAS 的配置和账号数据，也不会有意收集账号密码。\r\n" +
	"- 这些文件里都有本机路径，路径里可能有 Windows 用户名；截图和 AUTO-MAS 日志里可能有游戏账号名。\r\n" +
	"- 可以公开：在 GitHub Issue（https://github.com/ModAVD/ModAVD/issues）里贴 summary.txt 和日志，或者上传删掉 crash 文件夹后的问题包。上传前看一眼，不想公开的内容先删掉。\r\n" +
	"- 不要公开：crash 文件夹（崩溃转储，含模拟器进程的线程栈和少量内存片段）。带 crash 文件夹的完整问题包请发邮件到 qiyinxi@foxmail.com，邮件里写上 Issue 编号。"

// ---- .NET number formatting --------------------------------------------------------------------------------------

// formatN is '{0:N<decimals>}' -f v of Windows PowerShell 5.1 (.NET Framework, en-US / zh-CN): the double cut to 15
// significant digits, then rounded half away from zero to the decimals, thousands grouped with ','.
func formatN(v float64, decimals int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	neg := v < 0
	s := strconv.FormatFloat(math.Abs(v), 'e', 14, 64) // d.dddddddddddddde±XX
	mant, expText, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expText)
	digits := []byte(strings.Replace(mant, ".", "", 1)) // 15 digits; the value is 0.d1d2... × 10^(exp+1)
	point := exp + 1                                    // digits before the decimal point
	keep := point + decimals
	switch {
	case keep < 0:
		digits, point = nil, 0
	case keep < len(digits):
		up := digits[keep] >= '5'
		digits = digits[:keep]
		for i := len(digits) - 1; up && i >= 0; i-- {
			if digits[i] == '9' {
				digits[i] = '0'
				continue
			}
			digits[i]++
			up = false
		}
		if up {
			digits = append([]byte{'1'}, digits...)
			point++
		}
	}
	var intPart, frac string
	if point <= 0 {
		intPart = "0"
		frac = strings.Repeat("0", -point) + string(digits)
	} else {
		for len(digits) < point {
			digits = append(digits, '0')
		}
		intPart = strings.TrimLeft(string(digits[:point]), "0")
		if intPart == "" {
			intPart = "0"
		}
		frac = string(digits[point:])
	}
	if len(frac) < decimals {
		frac += strings.Repeat("0", decimals-len(frac))
	}
	frac = frac[:decimals]
	var b strings.Builder
	if neg && strings.Trim(intPart+frac, "0") != "" {
		b.WriteByte('-')
	}
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if decimals > 0 {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}

const (
	kb = 1 << 10
	mb = 1 << 20
	gb = 1 << 30
)

// formatSize is collect-info.ps1's Format-Size.
func formatSize(n float64) string {
	switch {
	case n >= gb:
		return formatN(n/gb, 2) + " GB"
	case n >= mb:
		return formatN(n/mb, 1) + " MB"
	case n >= kb:
		return formatN(n/kb, 1) + " KB"
	}
	return strconv.FormatInt(int64(math.RoundToEven(n)), 10) + " B" // [long] rounds to even
}

// optInt is '{0}' -f of a nullable integer property: "" for null.
func optInt(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

// optBool is '{0}' -f of a nullable boolean: True / False, "" for null.
func optBool(v *bool) string {
	switch {
	case v == nil:
		return ""
	case *v:
		return "True"
	}
	return "False"
}

// ---- .NET strings ------------------------------------------------------------------------------------------------

// netTrim is String.Trim() (Char.IsWhiteSpace at both ends).
func netTrim(s string) string { return strings.TrimFunc(s, unicode.IsSpace) }

// netTrimEnd is String.TrimEnd().
func netTrimEnd(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) }

var netSpaces = regexp.MustCompile(`[\s\p{Z}\x{85}\v]+`)

// collapseSpace is -replace '\s+', ' ' (.NET \s is Unicode white space).
func collapseSpace(s string) string { return netSpaces.ReplaceAllString(s, " ") }

// splitNetLines splits like [IO.File]::ReadAllLines and -split "`r?`n" after both: on \r\n, \n and \r, with no empty
// line after a final line break.
func splitNetLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// splitCRLF is -split "`r?`n": a final line break leaves an empty last line.
func splitCRLF(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

// decodeText is how .NET reads a text file or a UTF-8 pipe: BOM dropped, invalid bytes replaced.
func decodeText(b []byte) string {
	return root.DecodeUTF8Replace(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")))
}

// utf16Len is String.Length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// padRight is '{0,-n}' -f s: padded with spaces to n UTF-16 units, never cut.
func padRight(s string, n int) string {
	if k := n - utf16Len(s); k > 0 {
		return s + strings.Repeat(" ", k)
	}
	return s
}

// padLeft is '{0,n}' -f s.
func padLeft(s string, n int) string {
	if k := n - utf16Len(s); k > 0 {
		return strings.Repeat(" ", k) + s
	}
	return s
}

// ---- ordering --------------------------------------------------------------------------------------------------

// cultureSymbols is the order Windows sorts ASCII punctuation in (measured with Sort-Object on PowerShell 5.1):
// mostly code point order, with + < = > after ~. '-' and the apostrophe are missing: they are ignored at first (see
// cultureCompare).
const cultureSymbols = " !\"#$%&()*,./:;?@[\\]^_`{|}~+<=>"

// cultureWeight is the first-level weight of one character: control characters, then punctuation, digits, letters
// (case ignored), then everything else by code point.
func cultureWeight(r rune) int {
	switch {
	case r < 0x20 || r == 0x7f:
		return int(r)
	case r >= '0' && r <= '9':
		return 0x100 + int(r-'0')
	case r < 0x80 && unicode.IsLetter(r):
		return 0x200 + int(unicode.ToLower(r))
	case r < 0x80:
		if i := strings.IndexRune(cultureSymbols, r); i >= 0 {
			return 0x80 + i
		}
		return 0xff
	}
	return 0x400 + int(unicode.ToLower(r))
}

func cultureIgnored(r rune) bool { return r == '-' || r == '\'' }

// cultureCompare orders strings like Sort-Object on Windows PowerShell 5.1 (the culture-aware comparison of .NET
// Framework): '-' and the apostrophe do not count at first, punctuation sorts before digits and digits before letters, case
// does not count; then lower case goes first at the first letter that differs only in case, then the string with
// fewer ignored characters, then code point order. That is NLS for the ASCII file, package and zip entry names
// sorted here (mas-avd.json after mas/…, live/mas_10-… before live/mas_1-…), not a full implementation of it.
func cultureCompare(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	var pa, pb []rune
	for _, r := range ra {
		if !cultureIgnored(r) {
			pa = append(pa, r)
		}
	}
	for _, r := range rb {
		if !cultureIgnored(r) {
			pb = append(pb, r)
		}
	}
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if wa, wb := cultureWeight(pa[i]), cultureWeight(pb[i]); wa != wb {
			return cmpInt(wa, wb)
		}
	}
	if len(pa) != len(pb) {
		return cmpInt(len(pa), len(pb))
	}
	for i := range pa {
		if ua, ub := unicode.IsUpper(pa[i]), unicode.IsUpper(pb[i]); ua != ub {
			if ua {
				return 1
			}
			return -1
		}
	}
	if len(ra) != len(rb) {
		return cmpInt(len(ra), len(rb))
	}
	return strings.Compare(a, b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// sortCulture is Sort-Object on strings.
func sortCulture(s []string) {
	sort.SliceStable(s, func(i, j int) bool { return cultureCompare(s[i], s[j]) < 0 })
}

// ntfsLess is the order NTFS lists a directory in (names compared upper-cased), which Get-ChildItem keeps.
func ntfsLess(a, b string) bool {
	ua, ub := strings.ToUpper(a), strings.ToUpper(b)
	if ua != ub {
		return ua < ub
	}
	return a < b
}

// listDir is Get-ChildItem -LiteralPath dir: directories first, then files, each in NTFS order; hidden entries only
// with force (-Force; a system file that is not hidden is listed anyway, measured on PowerShell 5.1).
func listDir(dir string, force bool) ([]fs.FileInfo, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dirs, files []fs.FileInfo
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue // gone meanwhile
		}
		if !force && isHidden(info) {
			continue
		}
		if info.IsDir() {
			dirs = append(dirs, info)
		} else {
			files = append(files, info)
		}
	}
	byName := func(s []fs.FileInfo) {
		sort.SliceStable(s, func(i, j int) bool { return ntfsLess(s[i].Name(), s[j].Name()) })
	}
	byName(dirs)
	byName(files)
	return append(dirs, files...), nil
}

// listFiles is Get-ChildItem -LiteralPath dir -File.
func listFiles(dir string) ([]fs.FileInfo, error) {
	all, err := listDir(dir, false)
	if err != nil {
		return nil, err
	}
	var out []fs.FileInfo
	for _, fi := range all {
		if !fi.IsDir() {
			out = append(out, fi)
		}
	}
	return out, nil
}

// ---- files -------------------------------------------------------------------------------------------------------

// readProps is collect-info.ps1's Read-Properties: key=value lines (not starting with #), keys and values trimmed,
// keys case-insensitive (a PowerShell hashtable), the last one wins. A missing file is empty.
func readProps(path string) (map[string]string, error) {
	h := map[string]string{}
	b, err := root.ReadFileShared(path)
	if err != nil {
		if os.IsNotExist(err) {
			return h, nil
		}
		return h, err
	}
	for _, line := range splitNetLines(decodeText(b)) {
		i := strings.IndexByte(line, '=')
		if i > 0 && !strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "#") {
			h[strings.ToLower(netTrim(line[:i]))] = netTrim(line[i+1:])
		}
	}
	return h, nil
}

// prop reads a key of readProps' map.
func prop(h map[string]string, key string) string { return h[strings.ToLower(key)] }

// isFile is Test-Path -PathType Leaf.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// exists is Test-Path -LiteralPath.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// trueLength is collect-info.ps1's Get-TrueLength: the length of an open handle (a directory listing lags behind a
// file that is being written), else the listed length.
func trueLength(path string) (int64, error) {
	if f, err := root.OpenShared(path); err == nil {
		defer f.Close()
		if fi, err := f.Stat(); err == nil {
			return fi.Size(), nil
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// copyShared is collect-info.ps1's Copy-Shared: the source opened with read / write / delete sharing (a log the
// emulator or AUTO-MAS is writing), only its last tail bytes when tail > 0; returns the length of the copy.
func copyShared(src, dst string, tail int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	in, err := root.OpenShared(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if tail > 0 {
		if fi, err := in.Stat(); err == nil && fi.Size() > tail {
			if _, err := in.Seek(-tail, io.SeekEnd); err != nil {
				return 0, err
			}
		}
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// writeLinesBOM is [IO.File]::WriteAllLines(path, lines, UTF8 with BOM): CRLF after every line.
func writeLinesBOM(path string, lines []string) error {
	var b bytes.Buffer
	b.WriteString("\xef\xbb\xbf")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// ---- mas-avd.json as ConvertFrom-Json sees it --------------------------------------------------------------------

// readConfig reads mas-avd.json like `[IO.File]::ReadAllText(...) | ConvertFrom-Json`. obj is nil when the file is
// missing, empty, null or not an object; msg is what collect-info.ps1 writes about a file it cannot use ("" when
// there is nothing to say).
func readConfig(path string) (obj *jsonord.Obj, msg string) {
	b, err := root.ReadFileShared(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		return nil, "mas-avd.json 解析失败，按空配置处理：" + err.Error()
	}
	text := bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if strings.TrimSpace(string(text)) == "" {
		return nil, "" // ConvertFrom-Json of nothing is $null, no error
	}
	v, err := jsonord.Parse(text)
	if err != nil {
		return nil, "mas-avd.json 解析失败，按空配置处理：" + err.Error()
	}
	switch v.Kind() {
	case jsonord.Null:
		return nil, ""
	case jsonord.Object:
		return v.Obj(), ""
	}
	return nil, "mas-avd.json 不是 JSON 对象，按空配置处理"
}

// getCI is $obj.Name on a ConvertFrom-Json object: property names are case-insensitive.
func getCI(o *jsonord.Obj, key string) *jsonord.Value {
	if o.Has(key) {
		return o.Get(key)
	}
	for _, k := range o.Keys() {
		if strings.EqualFold(k, key) {
			return o.Get(k)
		}
	}
	return nil
}

// hasCI is $obj.PSObject.Properties.Name -contains key.
func hasCI(o *jsonord.Obj, key string) bool {
	for _, k := range o.Keys() {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// sdkRootOf is mas-avd.json's sdkRoot when it is a string that is not blank, trimmed; "" otherwise.
func sdkRootOf(o *jsonord.Obj) string {
	s, ok := getCI(o, "sdkRoot").Str()
	if !ok {
		return ""
	}
	return netTrim(s)
}

// truthy is PowerShell's truth of a JSON value: null, false, 0, "" and empty arrays are false.
func truthy(v *jsonord.Value) bool {
	switch v.Kind() {
	case jsonord.Null:
		return false
	case jsonord.Bool:
		b, _ := v.BoolVal()
		return b
	case jsonord.Number:
		f, _ := v.Float()
		return f != 0
	case jsonord.String:
		s, _ := v.Str()
		return s != ""
	case jsonord.Array:
		return len(v.Items()) > 0
	}
	return true
}

// psDouble is [double]$v for a JSON number, boolean or numeric string; ok=false where the cast would throw.
func psDouble(v *jsonord.Value) (float64, bool) {
	switch v.Kind() {
	case jsonord.Number:
		return v.Float()
	case jsonord.Bool:
		if b, _ := v.BoolVal(); b {
			return 1, true
		}
		return 0, true
	case jsonord.String:
		s, _ := v.Str()
		f, err := strconv.ParseFloat(netTrim(s), 64)
		return f, err == nil
	}
	return 0, false
}

var scriptPortText = regexp.MustCompile(`^[+-]?[0-9]+$`)

// scriptPort is collect-info.ps1's Resolve-MetadataPort: an integer (fractions truncated, a numeric string, a
// boolean as 0 / 1) of 1024-65535 that is not 5037 and not a console / adb / gRPC port of instances 0-9 in plan (5 is
// reserved; the script knows only the default plan).
func scriptPort(plan root.PortPlan, v *jsonord.Value) (int, bool) {
	var port int64
	switch v.Kind() {
	case jsonord.Bool:
		if b, _ := v.BoolVal(); b {
			port = 1
		}
	case jsonord.Number:
		if i, ok := v.Int(); ok {
			port = i
		} else {
			f, ok := v.Float()
			if !ok || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) >= 1<<63 {
				return 0, false
			}
			port = int64(f)
		}
	case jsonord.String:
		s, _ := v.Str()
		s = netTrim(s)
		if !scriptPortText.MatchString(s) {
			return 0, false
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		port = i
	default:
		return 0, false
	}
	if port < 1024 || port > 65535 || port == 5037 {
		return 0, false
	}
	for i := 0; i <= root.MaxIndex; i++ {
		if i == root.ReservedIndex {
			continue
		}
		if c := int64(plan.Console(i)); port >= c && port <= c+2 {
			return 0, false
		}
	}
	return int(port), true
}

// psInt is [int]$v (report-config.ps1's Try-Get { [int]$cfg.adbServerPort }): numbers rounded half to even, numeric
// strings parsed; ok=false where the cast throws (out of Int32, not a number) or for null.
func psInt(v *jsonord.Value) (int, bool) {
	var f float64
	switch v.Kind() {
	case jsonord.Bool:
		if b, _ := v.BoolVal(); b {
			return 1, true
		}
		return 0, true
	case jsonord.Number:
		if i, ok := v.Int(); ok {
			if i < math.MinInt32 || i > math.MaxInt32 {
				return 0, false
			}
			return int(i), true
		}
		var ok bool
		if f, ok = v.Float(); !ok {
			return 0, false
		}
	case jsonord.String:
		s, _ := v.Str()
		s = netTrim(s)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			f = float64(i)
		} else if h, ok := strings.CutPrefix(strings.ToLower(s), "0x"); ok {
			u, err := strconv.ParseUint(h, 16, 32)
			if err != nil {
				return 0, false
			}
			f = float64(int32(uint32(u)))
		} else if f, err = strconv.ParseFloat(s, 64); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	f = math.RoundToEven(f)
	if math.IsNaN(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

// psJSON is `$v | ConvertTo-Json -Compress` on Windows PowerShell 5.1 for a value ConvertFrom-Json produced: null
// pipes nothing (""), a one-item array unrolls to the item, decimals keep their digits, doubles (with an exponent)
// print shortest, and strings escape < > & ' like JavaScriptSerializer.
func psJSON(v *jsonord.Value) string {
	if v.Kind() == jsonord.Null {
		return ""
	}
	if items := v.Items(); v.Kind() == jsonord.Array && len(items) == 1 {
		return psJSON(items[0])
	}
	var b strings.Builder
	writePSJSON(&b, v)
	return b.String()
}

func writePSJSON(b *strings.Builder, v *jsonord.Value) {
	switch v.Kind() {
	case jsonord.Null:
		b.WriteString("null")
	case jsonord.Bool:
		x, _ := v.BoolVal()
		b.WriteString(strconv.FormatBool(x))
	case jsonord.Number:
		lit, _ := v.NumberLiteral()
		if strings.ContainsAny(lit, "eE") {
			f, _ := v.Float()
			lit = strconv.FormatFloat(f, 'f', -1, 64)
		}
		b.WriteString(lit)
	case jsonord.String:
		s, _ := v.Str()
		b.WriteByte('"')
		for _, r := range s {
			switch {
			case r == '"':
				b.WriteString(`\"`)
			case r == '\\':
				b.WriteString(`\\`)
			case r == '\n':
				b.WriteString(`\n`)
			case r == '\r':
				b.WriteString(`\r`)
			case r == '\t':
				b.WriteString(`\t`)
			case r < 0x20 || r == '<' || r == '>' || r == '&' || r == '\'':
				b.WriteString(`\u00`)
				b.WriteString(strconv.FormatInt(int64(r)>>4, 16))
				b.WriteString(strconv.FormatInt(int64(r)&15, 16))
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
	case jsonord.Array:
		b.WriteByte('[')
		for i, it := range v.Items() {
			if i > 0 {
				b.WriteByte(',')
			}
			writePSJSON(b, it)
		}
		b.WriteByte(']')
	case jsonord.Object:
		b.WriteByte('{')
		o := v.Obj()
		for i, k := range o.Keys() {
			if i > 0 {
				b.WriteByte(',')
			}
			writePSJSON(b, jsonord.NewString(k))
			b.WriteByte(':')
			writePSJSON(b, o.Get(k))
		}
		b.WriteByte('}')
	}
}

// ---- regular expressions as .NET runs them -----------------------------------------------------------------------

// isWordRune is \w in .NET: letters, digits, connector punctuation and marks.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Pc, r) || unicode.Is(unicode.Mn, r)
}

// containsWordFold is -match '\bword\b' (case-insensitive, Unicode word boundaries).
func containsWordFold(s, word string) bool {
	ls, lw := strings.ToLower(s), strings.ToLower(word)
	for from := 0; ; {
		i := strings.Index(ls[from:], lw)
		if i < 0 {
			return false
		}
		i += from
		before, _ := utf8.DecodeLastRuneInString(ls[:i])
		after, _ := utf8.DecodeRuneInString(ls[i+len(lw):])
		if (i == 0 || !isWordRune(before)) && (i+len(lw) == len(ls) || !isWordRune(after)) {
			return true
		}
		from = i + 1
	}
}
