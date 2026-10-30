// Package jsonord reads and writes JSON keeping key order and number literals, so modavd can rewrite mas-avd.json
// (owned jointly with AUTO-MAS) without reordering keys or dropping fields it does not know.
//
// Output matches Python's json.dumps(data, ensure_ascii=False, indent=2) byte for byte, which is how AUTO-MAS writes
// the file: two-space indent, ", " never used (item separator "," + newline), key separator ": ", empty containers as
// {} and [], non-ASCII written as is, no trailing newline. Strings escape only '"', '\\', \b \f \n \r \t and other
// control characters below 0x20 (as \u00XX, lowercase hex).
//
// Numbers come out the way Python prints what json.loads made of them: integer literals unchanged ("-0" becomes
// "0"), anything with a fraction or exponent as float repr ("1e3" -> "1000.0", "1.50" -> "1.5", "1e400" ->
// "Infinity"). The literal itself is kept, so Int / Float see what the file said. NaN, Infinity and -Infinity are
// accepted like Python's json does.
//
// Parsing follows Python's json.loads on a file read with encoding="utf-8-sig": one leading BOM is skipped, the text
// must be valid UTF-8, whitespace is space / tab / CR / LF only, control characters inside strings are errors.
// Escaped lone surrogates (which Python keeps but then cannot write back as UTF-8) become U+FFFD.
package jsonord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"modavd/internal/errs"
)

type Kind int

const (
	Null Kind = iota
	Bool
	Number
	String
	Array
	Object
)

// Value is one JSON value. The zero Value is null.
type Value struct {
	kind Kind
	b    bool
	s    string   // string contents, or the number literal exactly as read / formatted
	arr  []*Value // Array
	obj  *Obj     // Object
}

// Obj is an ordered JSON object.
type Obj struct {
	keys []string
	m    map[string]*Value
}

// Parse decodes one JSON document. A leading UTF-8 BOM is skipped (Windows PowerShell 5.1 writes one); anything but
// whitespace after the value is an error. Duplicate keys: the last one wins, at the position of the first.
func Parse(data []byte) (*Value, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		off := 0
		for off < len(data) {
			r, n := utf8.DecodeRune(data[off:])
			if r == utf8.RuneError && n <= 1 {
				break
			}
			off += n
		}
		return nil, errs.Newf(errs.InvalidArgument, "不是有效的 UTF-8 文本（第 %d 个字节）", off)
	}
	p := &parser{data: data}
	p.skipWS()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos != len(p.data) {
		return nil, p.fail("值后面还有多余的内容", p.pos)
	}
	return v, nil
}

// maxDepth bounds nesting so a hostile file cannot exhaust the stack. Python 3.12's json.loads takes up to 2997
// levels, but json.dumps(indent=2), which AUTO-MAS writes with, already fails beyond about 990 (the interpreter's
// recursion limit), so nothing deeper than this can round-trip through AUTO-MAS anyway.
const maxDepth = 1000

// maxIntDigits is Python 3.12's int_max_str_digits: json.loads refuses longer integer literals.
const maxIntDigits = 4300

type parser struct {
	data []byte
	pos  int
}

// fail builds the error with Python's position convention (line, column and character offset count characters, not
// bytes).
func (p *parser) fail(msg string, at int) error {
	if at > len(p.data) {
		at = len(p.data)
	}
	before := p.data[:at]
	line := bytes.Count(before, []byte{'\n'}) + 1
	lineStart := bytes.LastIndexByte(before, '\n') + 1
	col := utf8.RuneCount(before[lineStart:]) + 1
	return errs.Newf(errs.InvalidArgument, "JSON 格式不对：%s（第 %d 行第 %d 列，第 %d 个字符）",
		msg, line, col, utf8.RuneCount(before))
}

func (p *parser) skipWS() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) hasPrefix(s string) bool {
	return bytes.HasPrefix(p.data[p.pos:], []byte(s))
}

func (p *parser) value(depth int) (*Value, error) {
	if p.pos >= len(p.data) {
		return nil, p.fail("缺少值", p.pos)
	}
	switch c := p.data[p.pos]; {
	case c == '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return &Value{kind: String, s: s}, nil
	case c == '{':
		if depth >= maxDepth {
			return nil, p.fail("嵌套太深", p.pos)
		}
		return p.object(depth + 1)
	case c == '[':
		if depth >= maxDepth {
			return nil, p.fail("嵌套太深", p.pos)
		}
		return p.array(depth + 1)
	case p.hasPrefix("null"):
		p.pos += 4
		return &Value{}, nil
	case p.hasPrefix("true"):
		p.pos += 4
		return &Value{kind: Bool, b: true}, nil
	case p.hasPrefix("false"):
		p.pos += 5
		return &Value{kind: Bool}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		if lit, ok := p.number(); ok {
			return lit, nil
		}
		if p.hasPrefix("-Infinity") {
			p.pos += 9
			return &Value{kind: Number, s: "-Infinity"}, nil
		}
		return nil, p.fail("缺少值", p.pos)
	case p.hasPrefix("NaN"):
		p.pos += 3
		return &Value{kind: Number, s: "NaN"}, nil
	case p.hasPrefix("Infinity"):
		p.pos += 8
		return &Value{kind: Number, s: "Infinity"}, nil
	}
	return nil, p.fail("缺少值", p.pos)
}

// number matches -?(0|[1-9]\d*)(\.\d+)?([eE][-+]?\d+)? like Python's scanner; ok=false (position unchanged) when
// nothing matches. A fraction or exponent that does not complete is left out of the match, as in Python, so the
// caller then fails on the leftover characters.
func (p *parser) number() (*Value, bool) {
	d := p.data
	i := p.pos
	if i < len(d) && d[i] == '-' {
		i++
	}
	if i >= len(d) || d[i] < '0' || d[i] > '9' {
		return nil, false
	}
	if d[i] == '0' {
		i++
	} else {
		for i < len(d) && d[i] >= '0' && d[i] <= '9' {
			i++
		}
	}
	intEnd := i
	isFloat := false
	if i+1 < len(d) && d[i] == '.' && d[i+1] >= '0' && d[i+1] <= '9' {
		i += 2
		for i < len(d) && d[i] >= '0' && d[i] <= '9' {
			i++
		}
		isFloat = true
	}
	if i < len(d) && (d[i] == 'e' || d[i] == 'E') {
		j := i + 1
		if j < len(d) && (d[j] == '+' || d[j] == '-') {
			j++
		}
		if j < len(d) && d[j] >= '0' && d[j] <= '9' {
			for j < len(d) && d[j] >= '0' && d[j] <= '9' {
				j++
			}
			i = j
			isFloat = true
		}
	}
	if !isFloat {
		digits := intEnd - p.pos
		if d[p.pos] == '-' {
			digits--
		}
		if digits > maxIntDigits {
			return nil, false
		}
	}
	lit := string(d[p.pos:i])
	p.pos = i
	return &Value{kind: Number, s: lit}, true
}

func (p *parser) str() (string, error) {
	start := p.pos
	p.pos++ // opening quote
	var sb []byte
	chunk := p.pos
	for {
		if p.pos >= len(p.data) {
			return "", p.fail("字符串没有结束", start)
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			if sb == nil {
				s := string(p.data[chunk:p.pos])
				p.pos++
				return s, nil
			}
			sb = append(sb, p.data[chunk:p.pos]...)
			p.pos++
			return string(sb), nil
		case c < 0x20:
			return "", p.fail("字符串里有控制字符", p.pos)
		case c == '\\':
			sb = append(sb, p.data[chunk:p.pos]...)
			if p.pos+1 >= len(p.data) {
				return "", p.fail("字符串没有结束", start)
			}
			esc := p.data[p.pos+1]
			switch esc {
			case '"', '\\', '/':
				sb = append(sb, esc)
				p.pos += 2
			case 'b':
				sb = append(sb, '\b')
				p.pos += 2
			case 'f':
				sb = append(sb, '\f')
				p.pos += 2
			case 'n':
				sb = append(sb, '\n')
				p.pos += 2
			case 'r':
				sb = append(sb, '\r')
				p.pos += 2
			case 't':
				sb = append(sb, '\t')
				p.pos += 2
			case 'u':
				r, ok := p.hex4(p.pos + 2)
				if !ok {
					return "", p.fail("\\u 转义写法不对", p.pos+1)
				}
				p.pos += 6
				if r >= 0xd800 && r <= 0xdbff && p.pos+1 < len(p.data) && p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
					if lo, ok := p.hex4(p.pos + 2); ok {
						if lo >= 0xdc00 && lo <= 0xdfff {
							r = 0x10000 + (r-0xd800)<<10 + (lo - 0xdc00)
							p.pos += 6
						}
					} else {
						return "", p.fail("\\u 转义写法不对", p.pos+1)
					}
				}
				if r >= 0xd800 && r <= 0xdfff {
					r = utf8.RuneError
				}
				sb = utf8.AppendRune(sb, r)
			default:
				return "", p.fail("转义写法不对", p.pos)
			}
			chunk = p.pos
		default:
			p.pos++
		}
	}
}

func (p *parser) hex4(at int) (rune, bool) {
	if at+4 > len(p.data) {
		return 0, false
	}
	var r rune
	for _, c := range p.data[at : at+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}

func (p *parser) object(depth int) (*Value, error) {
	v := NewObject()
	o := v.obj
	p.pos++ // {
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return v, nil
	}
	for {
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, p.fail("缺少用双引号括起来的键名", p.pos)
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, p.fail("缺少 ':'", p.pos)
		}
		p.pos++
		p.skipWS()
		item, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		o.Set(key, item)
		p.skipWS()
		if p.pos < len(p.data) && p.data[p.pos] == '}' {
			p.pos++
			return v, nil
		}
		if p.pos >= len(p.data) || p.data[p.pos] != ',' {
			return nil, p.fail("缺少 ','", p.pos)
		}
		p.pos++
		p.skipWS()
	}
}

func (p *parser) array(depth int) (*Value, error) {
	v := &Value{kind: Array, arr: []*Value{}}
	p.pos++ // [
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return v, nil
	}
	for {
		item, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		v.arr = append(v.arr, item)
		p.skipWS()
		if p.pos < len(p.data) && p.data[p.pos] == ']' {
			p.pos++
			return v, nil
		}
		if p.pos >= len(p.data) || p.data[p.pos] != ',' {
			return nil, p.fail("缺少 ','", p.pos)
		}
		p.pos++
		p.skipWS()
	}
}

// ---- output ------------------------------------------------------------------------------------------------------

// MarshalPython encodes v like Python's json.dumps(v, ensure_ascii=False, indent=2) (see the package comment).
func (v *Value) MarshalPython() []byte {
	var b bytes.Buffer
	writePython(&b, v, 0)
	return b.Bytes()
}

func writeIndent(b *bytes.Buffer, level int) {
	b.WriteByte('\n')
	for i := 0; i < level; i++ {
		b.WriteString("  ")
	}
}

func writePython(b *bytes.Buffer, v *Value, level int) {
	switch v.Kind() {
	case Null:
		b.WriteString("null")
	case Bool:
		if v.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case Number:
		b.WriteString(pythonNumber(v.s))
	case String:
		writePythonString(b, v.s)
	case Array:
		if len(v.arr) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, item := range v.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			writeIndent(b, level+1)
			writePython(b, item, level+1)
		}
		writeIndent(b, level)
		b.WriteByte(']')
	case Object:
		if v.obj.Len() == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range v.obj.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeIndent(b, level+1)
			writePythonString(b, k)
			b.WriteString(": ")
			writePython(b, v.obj.m[k], level+1)
		}
		writeIndent(b, level)
		b.WriteByte('}')
	}
}

// writePythonString is py_encode_basestring: only '"', '\\' and characters below 0x20 are escaped. Bytes that are
// not UTF-8 (possible only for strings built in Go) come out as U+FFFD, since a Python str cannot hold them.
func writePythonString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// isIntLiteral: a number literal without fraction or exponent (Python makes an int of it).
func isIntLiteral(lit string) bool {
	return lit != "NaN" && lit != "Infinity" && lit != "-Infinity" && !strings.ContainsAny(lit, ".eE")
}

// pythonNumber is how Python prints the number json.loads made of lit.
func pythonNumber(lit string) string {
	switch lit {
	case "NaN", "Infinity", "-Infinity":
		return lit
	}
	if isIntLiteral(lit) {
		if strings.Trim(lit, "-0") == "" {
			return "0" // int("-0") is 0
		}
		return lit
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil && !math.IsInf(f, 0) {
		return lit // not produced by Parse / FromGo
	}
	return PythonFloat(f)
}

// PythonFloat is Python's repr(float) as json.dumps writes it: the shortest digits that read back to the same
// double, fixed notation while the decimal exponent is in [-4, 16), otherwise d.ddde±XX; integral values keep ".0";
// NaN / Infinity / -Infinity for the special values.
func PythonFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // "-1.2345e+17": same digits and exponent style as Python
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	decpt := exp + 1 // value = 0.DIGITS * 10^decpt
	if decpt <= -4 || decpt > 16 {
		return e
	}
	sign := ""
	if strings.HasPrefix(mant, "-") {
		sign, mant = "-", mant[1:]
	}
	digits := strings.Replace(mant, ".", "", 1)
	switch {
	case decpt <= 0:
		return sign + "0." + strings.Repeat("0", -decpt) + digits
	case decpt < len(digits):
		return sign + digits[:decpt] + "." + digits[decpt:]
	default:
		return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	}
}

// ---- construction ------------------------------------------------------------------------------------------------

func NewNull() *Value                 { return &Value{} }
func NewBool(b bool) *Value           { return &Value{kind: Bool, b: b} }
func NewString(s string) *Value       { return &Value{kind: String, s: s} }
func NewInt(i int64) *Value           { return &Value{kind: Number, s: strconv.FormatInt(i, 10)} }
func NewArray(items ...*Value) *Value { return &Value{kind: Array, arr: items} }
func NewObject() *Value               { return &Value{kind: Object, obj: &Obj{m: map[string]*Value{}}} }

// NewFloat is a Python float (written as PythonFloat).
func NewFloat(f float64) *Value { return &Value{kind: Number, s: PythonFloat(f)} }

// FromGo converts nil, bool, string, int*, uint*, float64, json.Number, []any, []string, map[string]any (keys
// sorted), *Value and *Obj. Other types are an error.
func FromGo(x any) (*Value, error) {
	switch t := x.(type) {
	case nil:
		return NewNull(), nil
	case *Value:
		if t == nil {
			return NewNull(), nil
		}
		return t, nil
	case *Obj:
		if t == nil {
			return NewNull(), nil
		}
		if t.m == nil {
			t.m = map[string]*Value{}
		}
		return &Value{kind: Object, obj: t}, nil
	case bool:
		return NewBool(t), nil
	case string:
		return NewString(t), nil
	case int:
		return NewInt(int64(t)), nil
	case int8:
		return NewInt(int64(t)), nil
	case int16:
		return NewInt(int64(t)), nil
	case int32:
		return NewInt(int64(t)), nil
	case int64:
		return NewInt(t), nil
	case uint:
		return &Value{kind: Number, s: strconv.FormatUint(uint64(t), 10)}, nil
	case uint8:
		return NewInt(int64(t)), nil
	case uint16:
		return NewInt(int64(t)), nil
	case uint32:
		return NewInt(int64(t)), nil
	case uint64:
		return &Value{kind: Number, s: strconv.FormatUint(t, 10)}, nil
	case float64:
		return NewFloat(t), nil
	case json.Number:
		lit := string(t)
		p := &parser{data: []byte(lit)}
		if v, ok := p.number(); ok && p.pos == len(lit) {
			return v, nil
		}
		return nil, errs.Newf(errs.InvalidArgument, "不是 JSON 数字: %q", lit)
	case []any:
		arr := make([]*Value, 0, len(t))
		for _, item := range t {
			v, err := FromGo(item)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return &Value{kind: Array, arr: arr}, nil
	case []string:
		arr := make([]*Value, 0, len(t))
		for _, s := range t {
			arr = append(arr, NewString(s))
		}
		return &Value{kind: Array, arr: arr}, nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		v := NewObject()
		for _, k := range keys {
			item, err := FromGo(t[k])
			if err != nil {
				return nil, err
			}
			v.obj.Set(k, item)
		}
		return v, nil
	}
	return nil, errs.Newf(errs.InvalidArgument, "不能转成 JSON 的类型 %T", x)
}

// ToGo converts to plain Go values: nil, bool, json.Number, string, []any, map[string]any (order is lost; use it
// only for output). NaN / Infinity become nil, since encoding/json cannot write them.
func (v *Value) ToGo() any {
	switch v.Kind() {
	case Bool:
		return v.b
	case Number:
		switch v.s {
		case "NaN", "Infinity", "-Infinity":
			return nil
		}
		return json.Number(v.s)
	case String:
		return v.s
	case Array:
		out := make([]any, 0, len(v.arr))
		for _, item := range v.arr {
			out = append(out, item.ToGo())
		}
		return out
	case Object:
		out := make(map[string]any, v.obj.Len())
		for _, k := range v.obj.keys {
			out[k] = v.obj.m[k].ToGo()
		}
		return out
	}
	return nil
}

// Clone is a deep copy.
func (v *Value) Clone() *Value {
	if v == nil {
		return NewNull()
	}
	c := &Value{kind: v.kind, b: v.b, s: v.s}
	switch v.kind {
	case Array:
		c.arr = make([]*Value, len(v.arr))
		for i, item := range v.arr {
			c.arr[i] = item.Clone()
		}
	case Object:
		c.obj = v.obj.Clone()
	}
	return c
}

// ---- access ------------------------------------------------------------------------------------------------------

func (v *Value) Kind() Kind {
	if v == nil {
		return Null
	}
	return v.kind
}

// Obj returns the object, or nil when v is not an object.
func (v *Value) Obj() *Obj {
	if v.Kind() != Object {
		return nil
	}
	return v.obj
}

// Items returns the array items, or nil when v is not an array.
func (v *Value) Items() []*Value {
	if v.Kind() != Array {
		return nil
	}
	if v.arr == nil {
		return []*Value{}
	}
	return v.arr
}

// Str returns the string, ok=false when v is not a string.
func (v *Value) Str() (string, bool) {
	if v.Kind() != String {
		return "", false
	}
	return v.s, true
}

// Int returns the number as int64; ok=false when v is not a number or not an integer literal (1.0, 1e3 count as
// not integer) or out of range.
func (v *Value) Int() (int64, bool) {
	if v.Kind() != Number || !isIntLiteral(v.s) {
		return 0, false
	}
	i, err := strconv.ParseInt(v.s, 10, 64)
	return i, err == nil
}

// Float returns the number as Python's float() of it would (integers too; 1e400 is +Inf); ok=false when v is not a
// number.
func (v *Value) Float() (float64, bool) {
	if v.Kind() != Number {
		return 0, false
	}
	switch v.s {
	case "NaN":
		return math.NaN(), true
	case "Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	f, err := strconv.ParseFloat(v.s, 64)
	if err != nil && !math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// NumberLiteral is the number as written in the source (or as formatted for numbers built in Go); ok=false when v is
// not a number.
func (v *Value) NumberLiteral() (string, bool) {
	if v.Kind() != Number {
		return "", false
	}
	return v.s, true
}

// BoolVal returns the boolean, ok=false when v is not a boolean.
func (v *Value) BoolVal() (bool, bool) {
	if v.Kind() != Bool {
		return false, false
	}
	return v.b, true
}

func (o *Obj) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Keys is a copy of the keys in order.
func (o *Obj) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}

func (o *Obj) Get(key string) *Value {
	if o == nil {
		return nil
	}
	return o.m[key]
}

func (o *Obj) Has(key string) bool {
	if o == nil {
		return false
	}
	_, ok := o.m[key]
	return ok
}

// Set replaces an existing key in place or appends a new one at the end. A nil v is stored as null.
func (o *Obj) Set(key string, v *Value) {
	if v == nil {
		v = NewNull()
	}
	if o.m == nil {
		o.m = map[string]*Value{}
	}
	if _, ok := o.m[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.m[key] = v
}

func (o *Obj) Delete(key string) {
	if o == nil {
		return
	}
	if _, ok := o.m[key]; !ok {
		return
	}
	delete(o.m, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// Clone is a deep copy of the object (an empty one for nil).
func (o *Obj) Clone() *Obj {
	c := &Obj{m: map[string]*Value{}}
	if o == nil {
		return c
	}
	c.keys = append([]string(nil), o.keys...)
	for k, v := range o.m {
		c.m[k] = v.Clone()
	}
	return c
}
