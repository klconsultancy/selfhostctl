// Package envsubst replaces ${NAME} with environment variables in manifests and Helm
// values, and nothing else: no defaults, no $NAME, no other templating.
package envsubst

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var ref = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Refs lists the variables text refers to, sorted and once each.
func Refs(text string) []string {
	var names []string
	for _, m := range ref.FindAllStringSubmatch(text, -1) {
		names = append(names, m[1])
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Expand replaces every ${NAME} with its value. A variable that is unset or empty is
// an error naming it; the config check catches that earlier, so this is a backstop.
// The returned values are what was substituted, for Redact.
func Expand(text string, lookup func(string) (string, bool)) (string, []string, error) {
	var missing, values []string
	out := ref.ReplaceAllStringFunc(text, func(m string) string {
		name := ref.FindStringSubmatch(m)[1]
		v, ok := lookup(name)
		if !ok || v == "" {
			missing = append(missing, name)
			return m
		}
		values = append(values, v)
		return v
	})
	if len(missing) > 0 {
		slices.Sort(missing)
		return "", nil, fmt.Errorf("unset or empty environment variables: %s", strings.Join(slices.Compact(missing), ", "))
	}
	return out, values, nil
}

// Redact replaces every substituted value in s, so an error from the API server that
// echoes an object never shows one.
func Redact(s string, values []string) string {
	// Longest first, so a value that contains another is replaced whole.
	sorted := slices.Clone(values)
	slices.SortFunc(sorted, func(a, b string) int { return len(b) - len(a) })
	for _, v := range sorted {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "<redacted>")
		}
	}
	return s
}
