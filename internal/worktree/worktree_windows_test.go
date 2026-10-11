//go:build windows

package worktree

import (
	"path/filepath"
	"testing"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/pkg/models"
)

func TestGenerateWorktreePathEncodesWindowsInvalidAuthorityCharacters(t *testing.T) {
	tests := []struct {
		name      string
		remote    string
		authority string
	}{
		{
			name:      "port",
			remote:    "ssh://git@github.com:2222/org/repo.git",
			authority: "github.com%3A2222",
		},
		{
			name:      "IPv6 with port",
			remote:    "ssh://git@[2001:db8::1]:2222/org/repo.git",
			authority: "%5B2001%3Adb8%3A%3A1%5D%3A2222",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseDir := t.TempDir()
			manager := newMockManager(&mockGit{repoURL: tt.remote}, &models.Config{
				Worktree: models.WorktreeConfig{BaseDir: baseDir},
				Naming:   models.NamingConfig{Template: config.DefaultNamingTemplate},
			})

			got, err := manager.generateWorktreePath("main")
			if err != nil {
				t.Fatalf("generateWorktreePath() unexpected error: %v", err)
			}
			want := filepath.Join(baseDir, tt.authority, "org", "repo", "main")
			if got != want {
				t.Fatalf("generateWorktreePath() = %q, want %q", got, want)
			}
		})
	}
}
