// Package credentials centralizes environment-variable names that must not
// cross into repository-controlled processes.
package credentials

import (
	"strings"

	"go.kenn.io/kwt/pkg/models"
)

// ProtectedNames returns kwt's built-in credential variables plus the
// configured fleet token variable.
func ProtectedNames(cfg *models.Config) []string {
	names := []string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"}
	if cfg != nil {
		names = append(names, cfg.Fleet.TokenEnv)
	}
	protected := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		folded := strings.ToLower(name)
		if name == "" || seen[folded] {
			continue
		}
		seen[folded] = true
		protected = append(protected, name)
	}
	return protected
}

// StripEnvironment removes protected variables case-insensitively. Windows
// environment names are case-insensitive, and preserving that rule here keeps
// alternate casing from bypassing checkout isolation.
func StripEnvironment(env, protectedNames []string) []string {
	protected := make(map[string]bool, len(protectedNames))
	for _, name := range protectedNames {
		name = strings.TrimSpace(name)
		if name != "" {
			protected[strings.ToLower(name)] = true
		}
	}
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && protected[strings.ToLower(name)] {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
