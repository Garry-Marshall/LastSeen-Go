package db

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf16"
)

// The Python bot stores lists as json.dumps(...) text: ", " separators and
// every non-ASCII character escaped as \uXXXX. Writing the identical bytes
// keeps the database byte-for-byte interchangeable between the two bots.

// DumpStrings is json.dumps(list_of_str).
func DumpStrings(items []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		writePyString(&b, s)
	}
	b.WriteByte(']')
	return b.String()
}

// DumpInts is json.dumps(list_of_int).
func DumpInts(items []int64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.FormatInt(v, 10))
	}
	b.WriteByte(']')
	return b.String()
}

func writePyString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	u4 := func(r rune) {
		b.WriteString(`\u`)
		b.WriteByte(hex[(r>>12)&0xf])
		b.WriteByte(hex[(r>>8)&0xf])
		b.WriteByte(hex[(r>>4)&0xf])
		b.WriteByte(hex[r&0xf])
	}
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
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			u4(r)
		case r < 0x7f || r == 0x7f:
			b.WriteRune(r)
		case r > 0xffff:
			r1, r2 := utf16.EncodeRune(r)
			u4(r1)
			u4(r2)
		default:
			u4(r)
		}
	}
	b.WriteByte('"')
}

// LoadStrings parses a JSON string list; ok is false when s is not one.
func LoadStrings(s string) ([]string, bool) {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, false
	}
	return out, true
}

// LoadInts parses a JSON integer list; ok is false when s is not one.
func LoadInts(s string) ([]int64, bool) {
	var out []int64
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, false
	}
	return out, true
}
