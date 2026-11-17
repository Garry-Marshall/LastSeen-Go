// Package pyfmt reproduces the parts of Python's string formatting that the
// bot's user-facing text depends on, so the Go port renders exactly what the
// Python bot rendered:
//
//   - Format: str.format with named fields ({name}, {name:spec}, {{ and }}),
//     used by the locale catalogs.
//   - Value: format(value, spec), for the f-strings in the code
//     (e.g. f"{n:>5,}" or f"{rate:.1f}").
//   - Str: str(value), including Python's float repr ("12.0", "1e+16").
//   - Round / RoundTo: Python's round() (banker's rounding on ties).
package pyfmt

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Format is Python's template.format(**args) for named fields.
// It returns an error where Python would raise (missing key, bad spec,
// unbalanced braces), so callers can fall back like the Python code does.
func Format(template string, args map[string]any) (string, error) {
	var b strings.Builder
	b.Grow(len(template) + 16)
	i := 0
	for i < len(template) {
		c := template[i]
		switch c {
		case '{':
			if i+1 < len(template) && template[i+1] == '{' {
				b.WriteByte('{')
				i += 2
				continue
			}
			end := strings.IndexByte(template[i+1:], '}')
			if end < 0 {
				return "", errors.New("Single '{' encountered in format string")
			}
			field := template[i+1 : i+1+end]
			i += end + 2
			out, err := formatField(field, args)
			if err != nil {
				return "", err
			}
			b.WriteString(out)
		case '}':
			if i+1 < len(template) && template[i+1] == '}' {
				b.WriteByte('}')
				i += 2
				continue
			}
			return "", errors.New("Single '}' encountered in format string")
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

func formatField(field string, args map[string]any) (string, error) {
	if strings.ContainsRune(field, '{') {
		return "", errors.New("nested format fields are not supported")
	}
	name, spec, _ := strings.Cut(field, ":")
	conv := ""
	if n, c, ok := strings.Cut(name, "!"); ok {
		name, conv = n, c
	}
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return "", fmt.Errorf("IndexError: positional field {%s} has no argument", name)
	}
	if strings.ContainsAny(name, ".[") {
		return "", fmt.Errorf("attribute/index access in field {%s} is not supported", name)
	}
	v, ok := args[name]
	if !ok {
		return "", fmt.Errorf("KeyError: '%s'", name)
	}
	switch conv {
	case "":
	case "s":
		v = Str(v)
	case "r":
		v = Repr(v)
	default:
		return "", fmt.Errorf("Unknown conversion specifier %s", conv)
	}
	return Value(v, spec)
}

// MustValue is Value for specs known to be valid for the value's type
// (f-strings in the code); on a mismatch it falls back to str(v).
func MustValue(v any, spec string) string {
	s, err := Value(v, spec)
	if err != nil {
		return Str(v)
	}
	return s
}

// Str is Python's str(v).
func Str(v any) string {
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
		return reprFloat(x)
	case float32:
		return reprFloat(float64(x))
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

// Repr is Python's repr(v) for the types the bot formats.
func Repr(v any) string {
	if s, ok := v.(string); ok {
		return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
	}
	return Str(v)
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

// spec is a parsed Python format-spec mini-language string:
// [[fill]align][sign][0][width][grouping][.precision][type]
type spec struct {
	fill      rune
	align     byte // 0, '<', '>', '^', '='
	sign      byte // 0, '+', '-', ' '
	zero      bool
	width     int
	grouping  byte // 0, ',', '_'
	precision int  // -1 = unset
	typ       byte // 0 or a type letter
}

func parseSpec(s string) (spec, error) {
	sp := spec{fill: ' ', precision: -1}
	if s == "" {
		return sp, nil
	}
	rs := []rune(s)
	i := 0
	isAlign := func(r rune) bool { return r == '<' || r == '>' || r == '^' || r == '=' }
	if len(rs) >= 2 && isAlign(rs[1]) {
		sp.fill, sp.align = rs[0], byte(rs[1])
		i = 2
	} else if len(rs) >= 1 && isAlign(rs[0]) {
		sp.align = byte(rs[0])
		i = 1
	}
	if i < len(rs) && (rs[i] == '+' || rs[i] == '-' || rs[i] == ' ') {
		sp.sign = byte(rs[i])
		i++
	}
	if i < len(rs) && rs[i] == '0' {
		sp.zero = true
		i++
	}
	start := i
	for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
		i++
	}
	if i > start {
		sp.width, _ = strconv.Atoi(string(rs[start:i]))
	}
	if i < len(rs) && (rs[i] == ',' || rs[i] == '_') {
		sp.grouping = byte(rs[i])
		i++
	}
	if i < len(rs) && rs[i] == '.' {
		i++
		start = i
		for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
			i++
		}
		if i == start {
			return sp, errors.New("Format specifier missing precision")
		}
		sp.precision, _ = strconv.Atoi(string(rs[start:i]))
	}
	if i < len(rs) {
		if i != len(rs)-1 || rs[i] > 127 {
			return sp, fmt.Errorf("Invalid format specifier '%s'", s)
		}
		sp.typ = byte(rs[i])
	}
	if sp.zero && sp.align == 0 {
		sp.fill, sp.align = '0', '='
	}
	return sp, nil
}

// Value is Python's format(v, spec).
func Value(v any, specStr string) (string, error) {
	sp, err := parseSpec(specStr)
	if err != nil {
		return "", err
	}
	switch x := v.(type) {
	case string:
		return formatString(x, sp, specStr)
	case nil:
		if specStr != "" {
			return "", errors.New("unsupported format string passed to NoneType.__format__")
		}
		return "None", nil
	case bool:
		if specStr == "" {
			return Str(x), nil
		}
		if x {
			return formatInt(1, sp, specStr)
		}
		return formatInt(0, sp, specStr)
	case float64:
		return formatFloat(x, sp, specStr)
	case float32:
		return formatFloat(float64(x), sp, specStr)
	}
	if i, ok := toInt(v); ok {
		return formatInt(i, sp, specStr)
	}
	// Anything else formats through str() like object.__format__ (empty spec only).
	if specStr != "" {
		return "", fmt.Errorf("unsupported format string passed to %T.__format__", v)
	}
	return Str(v), nil
}

func formatString(s string, sp spec, raw string) (string, error) {
	if sp.typ != 0 && sp.typ != 's' {
		return "", fmt.Errorf("Unknown format code '%c' for object of type 'str'", sp.typ)
	}
	if sp.sign != 0 {
		return "", errors.New("Sign not allowed in string format specifier")
	}
	if sp.grouping != 0 {
		return "", fmt.Errorf("Cannot specify '%c' with 's'.", sp.grouping)
	}
	if sp.align == '=' {
		return "", errors.New("'=' alignment not allowed in string format specifier")
	}
	if sp.precision >= 0 {
		rs := []rune(s)
		if len(rs) > sp.precision {
			s = string(rs[:sp.precision])
		}
	}
	return pad(s, "", sp, '<'), nil
}

func formatInt(i int64, sp spec, raw string) (string, error) {
	switch sp.typ {
	case 0, 'd', 'n':
		if sp.precision >= 0 {
			return "", errors.New("Precision not allowed in integer format specifier")
		}
	case 'f', 'F', 'e', 'E', 'g', 'G', '%':
		return formatFloat(float64(i), sp, raw)
	default:
		return "", fmt.Errorf("Unknown format code '%c' for object of type 'int'", sp.typ)
	}
	neg := i < 0
	var digits string
	if neg {
		digits = strconv.FormatUint(uint64(-(i+1))+1, 10)
	} else {
		digits = strconv.FormatInt(i, 10)
	}
	digits = group(digits, sp.grouping)
	return pad(digits, signFor(neg, sp.sign), sp, '>'), nil
}

func formatFloat(f float64, sp spec, raw string) (string, error) {
	neg := math.Signbit(f) && !math.IsNaN(f)
	a := math.Abs(f)
	var body string
	switch sp.typ {
	case 0:
		if sp.precision >= 0 {
			// Python: like 'g', but fixed notation keeps at least one decimal.
			body = strconv.FormatFloat(a, 'g', max(sp.precision, 1), 64)
			if !strings.ContainsAny(body, "e.") && !math.IsInf(a, 0) && !math.IsNaN(a) {
				body += ".0"
			}
			body = pyExponent(body)
		} else {
			body = reprFloat(a)
		}
	case 'f', 'F':
		p := sp.precision
		if p < 0 {
			p = 6
		}
		body = fixedOrSpecial(a, p)
	case '%':
		p := sp.precision
		if p < 0 {
			p = 6
		}
		body = fixedOrSpecial(a*100, p) + "%"
	case 'e', 'E':
		p := sp.precision
		if p < 0 {
			p = 6
		}
		body = pyExponent(strconv.FormatFloat(a, 'e', p, 64))
	case 'g', 'G':
		p := sp.precision
		if p < 0 {
			p = 6
		}
		body = pyExponent(strconv.FormatFloat(a, 'g', max(p, 1), 64))
	default:
		return "", fmt.Errorf("Unknown format code '%c' for object of type 'float'", sp.typ)
	}
	if sp.grouping != 0 {
		intPart, rest := body, ""
		if k := strings.IndexAny(body, ".e%"); k >= 0 {
			intPart, rest = body[:k], body[k:]
		}
		if intPart != "inf" && intPart != "nan" {
			body = group(intPart, sp.grouping) + rest
		}
	}
	return pad(body, signFor(neg, sp.sign), sp, '>'), nil
}

func fixedOrSpecial(a float64, p int) string {
	if math.IsInf(a, 0) {
		return "inf"
	}
	if math.IsNaN(a) {
		return "nan"
	}
	return strconv.FormatFloat(a, 'f', p, 64)
}

// pyExponent rewrites Go's exponent form to Python's (at least two exponent
// digits, which Go also uses, so only special values need translating).
func pyExponent(s string) string {
	switch s {
	case "+Inf", "Inf":
		return "inf"
	case "NaN":
		return "nan"
	}
	return s
}

// reprFloat is Python's repr(float) (shortest round-trip digits; scientific
// notation outside 1e-4 <= |x| < 1e16) for a non-negative value.
func reprFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	if math.IsNaN(f) {
		return "nan"
	}
	sign := ""
	if math.Signbit(f) {
		sign = "-"
		f = -f
	}
	if f == 0 {
		return sign + "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	if exp >= -4 && exp < 16 {
		if exp >= 0 {
			if len(digits) <= exp+1 {
				return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
			}
			return sign + digits[:exp+1] + "." + digits[exp+1:]
		}
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
	m := digits[:1]
	if len(digits) > 1 {
		m += "." + digits[1:]
	}
	es := "+"
	if exp < 0 {
		es = "-"
		exp = -exp
	}
	return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
}

func signFor(neg bool, sign byte) string {
	if neg {
		return "-"
	}
	switch sign {
	case '+':
		return "+"
	case ' ':
		return " "
	}
	return ""
}

func group(digits string, sep byte) string {
	if sep == 0 || len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(sep)
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// pad applies fill/align/width. Width counts code points, like Python's len().
func pad(body, sign string, sp spec, defAlign byte) string {
	n := utf8.RuneCountInString(body) + utf8.RuneCountInString(sign)
	if sp.width <= n {
		return sign + body
	}
	fill := strings.Repeat(string(sp.fill), sp.width-n)
	align := sp.align
	if align == 0 {
		align = defAlign
	}
	switch align {
	case '<':
		return sign + body + fill
	case '^':
		left := (sp.width - n) / 2
		rs := []rune(fill)
		return string(rs[:left]) + sign + body + string(rs[left:])
	case '=':
		return sign + fill + body
	default:
		return fill + sign + body
	}
}

// Round is Python's round(x) -> int: halves round to even.
func Round(x float64) int64 {
	return int64(math.RoundToEven(x))
}

// RoundTo is Python's round(x, n) -> float (correctly rounded, ties to even).
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
