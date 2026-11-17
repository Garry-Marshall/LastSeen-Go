// Package locale loads the translation catalogs (locales/<lang>.json) and
// renders user-facing text, exactly like bot/locale.py in the Python bot.
//
// English ("en") is the canonical catalog and the fallback for any key missing
// from another language. Templates use Python str.format named placeholders
// (rendered by pyfmt), e.g. "Removed {count:,} record(s)".
package locale

import (
	"encoding/json"
	"fmt"
	"lastseen/internal/logx"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lastseen/internal/pyfmt"
)

// Default is the canonical language and the fallback for missing keys.
const Default = "en"

var (
	log      = logx.Get("bot.locale")
	catalogs = map[string]map[string]string{}
)

// Load reads every *.json file in dir. Call once at startup, before anything
// user-facing runs (the catalogs are read-only afterwards).
func Load(dir string) {
	catalogs = map[string]map[string]string{}

	if _, err := os.Stat(dir); err != nil {
		log.Error("Locale directory '%s' not found; no translations loaded", dir)
		return
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(paths)
	for _, p := range paths {
		lang := strings.TrimSuffix(filepath.Base(p), ".json")
		data, err := os.ReadFile(p)
		if err != nil {
			log.Error("Failed to load locale '%s' from %s: %v", lang, p, err)
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			log.Error("Failed to load locale '%s' from %s: %v", lang, p, err)
			continue
		}
		catalogs[lang] = m
	}
	if _, ok := catalogs[Default]; !ok {
		log.Error("Default locale '%s' missing; user-facing text will fall back to raw keys", Default)
	}
	names := Available()
	list := strings.Join(names, ", ")
	if list == "" {
		list = "none"
	}
	log.Info("Loaded %d locale(s): %s", len(names), list)
}

// Available returns the sorted loaded language codes.
func Available() []string {
	out := make([]string, 0, len(catalogs))
	for k := range catalogs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Has reports whether lang is loaded.
func Has(lang string) bool {
	_, ok := catalogs[lang]
	return ok
}

// LanguageName is the human-readable name of a language (its
// meta.language_name key), or the bare code.
func LanguageName(code string) string {
	if v, ok := catalogs[code]["meta.language_name"]; ok {
		return v
	}
	return code
}

var weekdays = [7]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}

// Weekday is the localized name of a Monday=0 weekday index.
func Weekday(day int, lang string) string {
	if day < 0 || day >= 7 {
		return fmt.Sprint(day)
	}
	return T("weekday."+weekdays[day], lang)
}

// WeekdayName is the localized name of an English weekday name ("Monday");
// unknown values are returned unchanged.
func WeekdayName(name, lang string) string {
	c := strings.ToLower(strings.TrimSpace(name))
	for _, w := range weekdays {
		if w == c {
			return T("weekday."+c, lang)
		}
	}
	return name
}

// GuildLanguage resolves a guild's configured language, falling back to the
// default when it is unset or not loaded.
func GuildLanguage(lang string) string {
	if lang == "" {
		return Default
	}
	if _, ok := catalogs[lang]; ok {
		return lang
	}
	return Default
}

// A holds named placeholder values for T.
type A = map[string]any

// T translates key into lang, filling named placeholders from args (pairs of
// name, value). Missing keys fall back to English, then to the raw key. A
// template that fails to format falls back to the English template, so the
// user never sees raw {placeholder} braces.
func T(key, lang string, args ...any) string {
	return TA(key, lang, pairs(args))
}

// TA is T with the placeholders given as a map.
func TA(key, lang string, args A) string {
	template, ok := catalogs[lang][key]
	if !ok {
		template, ok = catalogs[Default][key]
	}
	if !ok {
		log.Warn("Missing translation key: '%s'", key)
		return key
	}
	out, err := pyfmt.Format(template, args)
	if err == nil {
		return out
	}
	log.Error("Failed to format translation key '%s' (%s): %v", key, lang, err)
	if en, ok := catalogs[Default][key]; ok && en != template {
		if out, err := pyfmt.Format(en, args); err == nil {
			return out
		}
		return en
	}
	return template
}

// Catalog returns the raw catalog for lang (nil if not loaded). For tooling/tests.
func Catalog(lang string) map[string]string { return catalogs[lang] }

func pairs(args []any) A {
	if len(args) == 0 {
		return nil
	}
	m := make(A, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		k, _ := args[i].(string)
		m[k] = args[i+1]
	}
	return m
}
