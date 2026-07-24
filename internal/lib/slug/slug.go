// Package slug turns human titles into clean, URL-safe slugs used in public
// links (e.g. "Sunset Yoga on the Beach" → "sunset-yoga-on-the-beach").
//
// Slugs are generated once when a record is created and are immutable
// afterwards, so existing links never break when a title is edited. Callers
// are responsible for guaranteeing uniqueness (see Disambiguate).
package slug

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)
	dashRuns    = regexp.MustCompile(`-{2,}`)
)

// Make converts an arbitrary string into a lowercase, hyphen-separated slug.
// It strips accents-free non-alphanumerics, collapses separators, and trims
// leading/trailing hyphens. An empty or all-symbol input yields fallback.
func Make(s, fallback string) string {
	out := strings.ToLower(strings.TrimSpace(s))
	out = nonAlphaNum.ReplaceAllString(out, "-")
	out = dashRuns.ReplaceAllString(out, "-")
	out = strings.Trim(out, "-")
	if out == "" {
		return fallback
	}
	return out
}

// Disambiguate appends a numeric suffix to base to make it unique. It calls
// exists for each candidate ("base", "base-2", "base-3", …) and returns the
// first one that does not already exist. Used at record-creation time.
func Disambiguate(base string, exists func(candidate string) (bool, error)) (string, error) {
	candidate := base
	for i := 2; ; i++ {
		taken, err := exists(candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}
