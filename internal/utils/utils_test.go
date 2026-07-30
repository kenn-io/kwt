package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTildePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("Failed to get user home directory: %v", err)
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Home directory",
			input:    home,
			expected: "~",
		},
		{
			name:     "Home subdirectory",
			input:    filepath.Join(home, "Documents"),
			expected: "~/Documents",
		},
		{
			name:     "Deep subdirectory",
			input:    filepath.Join(home, "ghq", "github.com", "d-kuro", "gwq"),
			expected: "~/ghq/github.com/d-kuro/gwq",
		},
		{
			name:     "Non-home path",
			input:    "/usr/local/bin",
			expected: "/usr/local/bin",
		},
		{
			name:     "Root path",
			input:    "/",
			expected: "/",
		},
		{
			name:     "Empty path",
			input:    "",
			expected: "",
		},
		{
			name:     "Path similar to home but not home",
			input:    home + "extra",
			expected: home + "extra",
		},
		{
			name:     "Relative path",
			input:    "./relative/path",
			expected: "./relative/path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TildePath(tt.input)
			if result != tt.expected {
				t.Errorf("TildePath(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestTildePathWithDifferentSeparators(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("Failed to get user home directory: %v", err)
	}

	// Test with unclean paths that might have extra separators
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Home with trailing slash",
			input:    home + string(filepath.Separator),
			expected: "~",
		},
		{
			name:     "Home subdirectory with double separators",
			input:    home + string(filepath.Separator) + string(filepath.Separator) + "Documents",
			expected: "~/Documents",
		},
		{
			name:     "Path with ./ elements",
			input:    filepath.Join(home, ".", "Documents", ".", "Projects"),
			expected: "~/Documents/Projects",
		},
		{
			name:     "Path with ../ elements",
			input:    filepath.Join(home, "Documents", "..", "Downloads"),
			expected: "~/Downloads",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TildePath(tt.input)
			if result != tt.expected {
				t.Errorf("TildePath(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSanitizeForFilesystem(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"feature/test", "feature-test"},
		{"bugfix:issue-123", "bugfix-issue-123"},
		{"feature\\windows", "feature-windows"}, // backslashes are replaced
		{"feat*ure", "feat-ure"},                // asterisks are replaced
		{"normal-branch", "normal-branch"},
		{"multiple//slashes", "multiple--slashes"},
		{"complex?path\"with<bad>chars|", "complex-path-with-bad-chars-"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := SanitizeForFilesystem(tt.input)
			if result != tt.expected {
				t.Errorf("SanitizeForFilesystem(%s) = %s, want %s", tt.input, result, tt.expected)
			}
		})
	}
}

func TestMatchPath(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{
			name:    "exact match",
			pattern: "/Users/test/src/myproject",
			path:    "/Users/test/src/myproject",
			want:    true,
		},
		{
			name:    "exact match - no match",
			pattern: "/Users/test/src/myproject",
			path:    "/Users/test/src/other",
			want:    false,
		},
		{
			name:    "wildcard single segment",
			pattern: "/Users/test/src/*",
			path:    "/Users/test/src/myproject",
			want:    true,
		},
		{
			name:    "wildcard single segment - no match nested",
			pattern: "/Users/test/src/*",
			path:    "/Users/test/src/github.com/user/repo",
			want:    false,
		},
		{
			name:    "wildcard middle segment",
			pattern: "/Users/*/src/myproject",
			path:    "/Users/test/src/myproject",
			want:    true,
		},
		{
			name:    "wildcard middle segment - different user",
			pattern: "/Users/*/src/myproject",
			path:    "/Users/other/src/myproject",
			want:    true,
		},
		{
			name:    "question mark single char",
			pattern: "/Users/test/src/project?",
			path:    "/Users/test/src/project1",
			want:    true,
		},
		{
			name:    "question mark - no match multiple chars",
			pattern: "/Users/test/src/project?",
			path:    "/Users/test/src/project12",
			want:    false,
		},
		{
			name:    "character class",
			pattern: "/Users/test/src/project[123]",
			path:    "/Users/test/src/project2",
			want:    true,
		},
		{
			name:    "character class - no match",
			pattern: "/Users/test/src/project[123]",
			path:    "/Users/test/src/project4",
			want:    false,
		},
		{
			name:    "empty pattern and path",
			pattern: "",
			path:    "",
			want:    true,
		},
		{
			name:    "empty pattern - no match",
			pattern: "",
			path:    "/some/path",
			want:    false,
		},
		// ** recursive matching tests
		{
			name:    "double star - match nested path",
			pattern: "/Users/test/src/**",
			path:    "/Users/test/src/github.com/user/repo",
			want:    true,
		},
		{
			name:    "double star - match direct child",
			pattern: "/Users/test/src/**",
			path:    "/Users/test/src/myproject",
			want:    true,
		},
		{
			name:    "double star - match deeply nested",
			pattern: "/Users/test/src/**",
			path:    "/Users/test/src/a/b/c/d/e/f",
			want:    true,
		},
		{
			name:    "double star - no match different prefix",
			pattern: "/Users/test/src/**",
			path:    "/Users/other/src/myproject",
			want:    false,
		},
		{
			name:    "double star in middle",
			pattern: "/Users/**/repo",
			path:    "/Users/test/src/github.com/user/repo",
			want:    true,
		},
		{
			name:    "double star in middle - direct",
			pattern: "/Users/**/repo",
			path:    "/Users/repo",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchPath(tt.pattern, tt.path)
			if got != tt.want {
				t.Errorf("MatchPath(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestEscapeForShell(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple string",
			input:    "hello world",
			expected: "hello world",
		},
		{
			name:     "string with double quotes",
			input:    `echo "hello"`,
			expected: `echo \"hello\"`,
		},
		{
			name:     "string with dollar signs",
			input:    "echo $HOME",
			expected: `echo \$HOME`,
		},
		{
			name:     "string with backticks",
			input:    "echo `date`",
			expected: "echo \\`date\\`",
		},
		{
			name:     "string with backslashes",
			input:    `path\to\file`,
			expected: `path\\to\\file`,
		},
		{
			name:     "complex command",
			input:    `git commit -m "Fix bug with $variable and \path"`,
			expected: `git commit -m \"Fix bug with \$variable and \\path\"`,
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "multiple special chars",
			input:    `"$test"` + "`" + `\`,
			expected: `\"\$test\"` + "\\`" + `\\`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EscapeForShell(tt.input)
			if result != tt.expected {
				t.Errorf("EscapeForShell(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsSameOrChildPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		parent   string
		expected bool
	}{
		{name: "Same directory", path: "/w/kwt/main", parent: "/w/kwt/main", expected: true},
		{name: "Direct child", path: "/w/kwt/main/internal", parent: "/w/kwt/main", expected: true},
		{name: "Nested descendant", path: "/w/kwt/main/a/b/c", parent: "/w/kwt/main", expected: true},
		{name: "Unclean input", path: "/w/kwt/main/./a/../a", parent: "/w/kwt/main/", expected: true},
		{name: "Sibling sharing a name prefix", path: "/w/kwt/feat-two", parent: "/w/kwt/feat"},
		{name: "Parent of the directory", path: "/w/kwt", parent: "/w/kwt/main"},
		{name: "Unrelated directory", path: "/w/kata/main", parent: "/w/kwt/main"},
		{name: "Empty path", path: "", parent: "/w/kwt/main"},
		{name: "Empty parent", path: "/w/kwt/main", parent: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSameOrChildPath(tt.path, tt.parent); got != tt.expected {
				t.Errorf("IsSameOrChildPath(%q, %q) = %v, want %v",
					tt.path, tt.parent, got, tt.expected)
			}
		})
	}
}

func TestIsSameOrChildPathResolvesSymlinkedAncestor(t *testing.T) {
	realDir := t.TempDir()
	child := filepath.Join(realDir, "worktree", "internal")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("failed to create child directory: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}

	if !IsSameOrChildPath(child, filepath.Join(link, "worktree")) {
		t.Errorf("a parent reached through a symlink must still contain %q", child)
	}
}

func TestPlatformPathKeyFoldsWindowsCaseAndSeparators(t *testing.T) {
	got := platformPathKey(`C:\Worktrees\Feature`, "windows")

	if got != "c:/worktrees/feature" {
		t.Fatalf("platformPathKey() = %q, want c:/worktrees/feature", got)
	}
}

func TestPlatformPathKeyPreservesUnixBackslashes(t *testing.T) {
	got := platformPathKey(`/worktrees/feature\name`, "darwin")

	if got != `/worktrees/feature\name` {
		t.Fatalf(
			"platformPathKey() = %q, want /worktrees/feature\\name",
			got,
		)
	}
}
