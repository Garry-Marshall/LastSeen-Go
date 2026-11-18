// Package textfmt fills in the bot's texts: the {name} and {name:spec}
// placeholders of the translation catalogs, and the number formats used in
// the code. The catalogs were written for the Python bot, so placeholders
// follow Python's str.format rules and render exactly as they did there,
// limited to the formats the bot uses:
//
//	{n}       the value as is; a float in its shortest exact form, always
//	          with a decimal point ("12.0"); nil as "None"
//	{n:,}     thousands separators ("1,234"); {n:+,} also "+" when positive
//	{n:.1f}   fixed decimals ("3.1"); {n:.0%} as a percentage ("42%")
//	{n:02d}   zero-padded to a width ("07")
//	{s:<8}    padded to a width, left-aligned; {n:>5,} right-aligned
//
// A spec is [<|>][+][0][width][,][.precision][d|f|%]. Anything else is an
// error, so a typo in a catalog shows up in the tests.
package textfmt

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Format fills template's {name} and {name:spec} placeholders from args;
// "{{" and "}}" are literal braces. It fails on an unknown name, an
// unsupported spec or a stray brace.
func Format(template string, args map[string]any) (string, error) {
	var b strings.Builder
	b.Grow(len(template) + 16)
	for i := 0; i < len(template); {
		c := template[i]
		switch {
		case c == '{' && strings.HasPrefix(template[i:], "{{"),
			c == '}' && strings.HasPrefix(template[i:], "}}"):
			b.WriteByte(c)
			i += 2
		case c == '{':
			end := strings.IndexByte(template[i+1:], '}')
			if end < 0 {
				return "", errors.New("unmatched '{'")
			}
			out, err := field(template[i+1:i+1+end], args)
			if err != nil {
				return "", err
			}
			b.WriteString(out)
			i += end + 2
		case c == '}':
			return "", errors.New("unmatched '}'")
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

func field(f string, args map[string]any) (string, error) {
	name, spec, _ := strings.Cut(f, ":")
	if name == "" || (name[0] >= '0' && name[0] <= '9') || strings.ContainsAny(name, "{.[!") {
		return "", fmt.Errorf("unsupported placeholder {%s}", f)
	}
	v, ok := args[name]
	if !ok {
		return "", fmt.Errorf("no value for {%s}", name)
	}
	return format(v, spec)
}

// Value formats v with spec (as in a placeholder); for a spec that doesn't
// suit v it falls back to the plain value.
func Value(v any, spec string) string {
	s, err := format(v, spec)
	if err != nil {
		return plain(v)
	}
	return s
}

// Round rounds to a whole number, ties to even (2.5 -> 2), as the texts
// always have.
func Round(x float64) int64 { return int64(math.RoundToEven(x)) }

// RoundTo rounds x to n decimals: correctly rounded, ties to even.
func RoundTo(x float64, n int) float64 {
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return x
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	if err != nil {
		return x
	}
	return r
}

type spec struct {
	align     byte // 0 (by type), '<' or '>'
	plus      bool
	zero      bool
	width     int
	comma     bool
	precision int  // -1 = unset
	typ       byte // 0, 'd', 'f' or '%'
}

func parseSpec(s string) (spec, error) {
	sp := spec{precision: -1}
	r := s
	if r != "" && (r[0] == '<' || r[0] == '>') {
		sp.align, r = r[0], r[1:]
	}
	if r != "" && r[0] == '+' {
		sp.plus, r = true, r[1:]
	}
	if r != "" && r[0] == '0' {
		if sp.align == 0 {
			sp.zero = true
		} else if len(r) > 1 && r[1] >= '0' && r[1] <= '9' {
			// "<05" would fill with zeros on the aligned side: not supported.
			return sp, fmt.Errorf("unsupported format spec %q", s)
		} // else "<0": a computed width of 0
		r = r[1:]
	}
	n := leadingDigits(r)
	sp.width, _ = strconv.Atoi(r[:n])
	r = r[n:]
	if r != "" && r[0] == ',' {
		sp.comma, r = true, r[1:]
	}
	if r != "" && r[0] == '.' {
		n := leadingDigits(r[1:])
		if n == 0 {
			return sp, fmt.Errorf("format spec %q: missing precision", s)
		}
		sp.precision, _ = strconv.Atoi(r[1 : 1+n])
		r = r[1+n:]
	}
	if r != "" && strings.IndexByte("df%", r[0]) >= 0 {
		sp.typ, r = r[0], r[1:]
	}
	if r != "" {
		return sp, fmt.Errorf("unsupported format spec %q", s)
	}
	return sp, nil
}

func leadingDigits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

func format(v any, s string) (string, error) {
	sp, err := parseSpec(s)
	if err != nil {
		return "", err
	}
	switch x := v.(type) {
	case string:
		if sp.plus || sp.zero || sp.comma || sp.precision >= 0 || sp.typ != 0 {
			return "", fmt.Errorf("format spec %q is not for text", s)
		}
		return pad(x, "", sp, '<'), nil
	case float64:
		return formatFloat(x, sp)
	case float32:
		return formatFloat(float64(x), sp)
	case bool:
		if s != "" { // a bool with a spec counts as 0 or 1
			if x {
				return formatInt(1, sp)
			}
			return formatInt(0, sp)
		}
	}
	if i, ok := toInt(v); ok {
		return formatInt(i, sp)
	}
	if s != "" {
		return "", fmt.Errorf("format spec %q is not for %T", s, v)
	}
	return plain(v), nil
}

// plain is the value as is, like Python's str().
func plain(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return shortestFloat(x)
	case float32:
		return shortestFloat(float64(x))
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	}
	if i, ok := toInt(v); ok {
		return strconv.FormatInt(i, 10)
	}
	if u, ok := v.(uint64); ok {
		return strconv.FormatUint(u, 10)
	}
	return fmt.Sprint(v)
}

func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint:
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x), true
		}
	}
	return 0, false
}

func formatInt(i int64, sp spec) (string, error) {
	if sp.typ == 'f' || sp.typ == '%' {
		return formatFloat(float64(i), sp)
	}
	if sp.precision >= 0 {
		return "", errors.New("an integer takes no precision")
	}
	digits := strconv.FormatUint(absInt(i), 10)
	if sp.comma {
		digits = group(digits)
	}
	return pad(digits, sign(i < 0, sp.plus), sp, '>'), nil
}

func absInt(i int64) uint64 {
	if i < 0 {
		return uint64(-(i + 1)) + 1 // also right for math.MinInt64
	}
	return uint64(i)
}

func formatFloat(f float64, sp spec) (string, error) {
	neg := math.Signbit(f) && !math.IsNaN(f)
	a := math.Abs(f)
	p := sp.precision
	if p < 0 {
		p = 6
	}
	var body string
	switch sp.typ {
	case 0:
		if sp.precision >= 0 {
			return "", errors.New("a float with a precision needs a type, e.g. .1f")
		}
		body = shortestFloat(a)
	case 'f':
		body = fixed(a, p)
	case '%':
		body = fixed(a*100, p) + "%"
	default:
		return "", errors.New("a float can't be formatted as an integer (d)")
	}
	if sp.comma && !math.IsInf(a, 0) && !math.IsNaN(a) {
		k := strings.IndexAny(body, ".e%")
		if k < 0 {
			k = len(body)
		}
		body = group(body[:k]) + body[k:]
	}
	return pad(body, sign(neg, sp.plus), sp, '>'), nil
}

func fixed(a float64, p int) string {
	switch {
	case math.IsInf(a, 0):
		return "inf"
	case math.IsNaN(a):
		return "nan"
	}
	return strconv.FormatFloat(a, 'f', p, 64)
}

// shortestFloat is the shortest text that reads back as f, always with a
// decimal point ("12.0"), switching to an exponent outside
// 1e-4 <= |f| < 1e16 ("1e+16") - Python's float repr.
func shortestFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	s := ""
	if math.Signbit(f) {
		s, f = "-", -f
	}
	if f == 0 {
		return s + "0.0"
	}
	mant, expStr, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	switch {
	case exp >= 16 || exp < -4:
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", s, m, es, exp)
	case exp < 0:
		return s + "0." + strings.Repeat("0", -exp-1) + digits
	case len(digits) <= exp+1:
		return s + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}
	return s + digits[:exp+1] + "." + digits[exp+1:]
}

func sign(neg, plus bool) string {
	switch {
	case neg:
		return "-"
	case plus:
		return "+"
	}
	return ""
}

// group puts a comma between every three digits, from the right.
func group(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	b.WriteString(digits[:lead])
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// pad widens sign+body to the spec's width (counted in characters): zeros
// go between the sign and the digits, spaces on the side the alignment
// leaves free.
func pad(body, sign string, sp spec, defaultAlign byte) string {
	n := utf8.RuneCountInString(body) + len(sign)
	if sp.width <= n {
		return sign + body
	}
	if sp.zero {
		return sign + strings.Repeat("0", sp.width-n) + body
	}
	fill := strings.Repeat(" ", sp.width-n)
	if sp.align == '<' || (sp.align == 0 && defaultAlign == '<') {
		return sign + body + fill
	}
	return fill + sign + body
}
