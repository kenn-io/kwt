// Package template provides directory name template processing functionality.
package template

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"text/template/parse"

	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
)

// TemplateData contains the data available for template processing.
type TemplateData struct {
	Host       string // e.g., "github.com"
	Owner      string // e.g., "user1"
	Repository string // e.g., "myapp"
	FullPath   string // e.g., "github.com/user1/myapp"
	Branch     string // e.g., "feature/new-ui"
	Hash       string // Short hash of the repository URL + branch
	Path       string // Absolute worktree path (empty while rendering naming.template)
}

// Processor handles template processing for worktree path generation.
type Processor struct {
	template      *template.Template
	sanitizeChars map[string]string
}

// EnvironmentOptions selects which naming components inherit environment
// expansion from global configuration.
type EnvironmentOptions struct {
	ExpandTemplate      bool
	ExpandSanitizeChars bool
}

// New creates a new template processor.
func New(templateStr string, sanitizeChars map[string]string) (*Processor, error) {
	tmpl, err := template.New("worktree").Parse(templateStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse template: %w", err)
	}

	return &Processor{
		template:      tmpl,
		sanitizeChars: sanitizeChars,
	}, nil
}

// NewWithEnvironment creates a processor for trusted global configuration.
// Environment references expand only in literal template text and replacement
// values, never inside template actions or branch-derived data.
func NewWithEnvironment(
	templateStr string,
	sanitizeChars map[string]string,
) (*Processor, error) {
	return NewWithEnvironmentOptions(
		templateStr,
		sanitizeChars,
		EnvironmentOptions{
			ExpandTemplate:      true,
			ExpandSanitizeChars: true,
		},
	)
}

// NewWithEnvironmentOptions creates a processor with per-component
// environment-expansion provenance.
func NewWithEnvironmentOptions(
	templateStr string,
	sanitizeChars map[string]string,
	options EnvironmentOptions,
) (*Processor, error) {
	processor, err := New(templateStr, sanitizeChars)
	if err != nil {
		return nil, err
	}
	if options.ExpandTemplate {
		for _, tmpl := range processor.template.Templates() {
			if tmpl.Tree != nil {
				expandLiteralText(tmpl.Root)
			}
		}
	}
	if options.ExpandSanitizeChars {
		expandedSanitizeChars := make(map[string]string, len(sanitizeChars))
		for old, replacement := range sanitizeChars {
			expandedSanitizeChars[old] = os.ExpandEnv(replacement)
		}
		processor.sanitizeChars = expandedSanitizeChars
	}
	return processor, nil
}

// LiteralTextHasEnvironmentReference reports whether environment expansion
// would affect literal template output. Dollar-prefixed Go template variables
// inside actions are syntax, not environment references.
func LiteralTextHasEnvironmentReference(templateStr string) (bool, error) {
	processor, err := New(templateStr, nil)
	if err != nil {
		return false, err
	}
	for _, tmpl := range processor.template.Templates() {
		if tmpl.Tree != nil && literalTextHasEnvironmentReference(tmpl.Root) {
			return true, nil
		}
	}
	return false, nil
}

func literalTextHasEnvironmentReference(node parse.Node) bool {
	switch current := node.(type) {
	case *parse.ListNode:
		if current == nil {
			return false
		}
		for _, child := range current.Nodes {
			if literalTextHasEnvironmentReference(child) {
				return true
			}
		}
	case *parse.TextNode:
		found := false
		_ = os.Expand(string(current.Text), func(string) string {
			found = true
			return ""
		})
		return found
	case *parse.IfNode:
		return literalTextHasEnvironmentReference(current.List) ||
			literalTextHasEnvironmentReference(current.ElseList)
	case *parse.RangeNode:
		return literalTextHasEnvironmentReference(current.List) ||
			literalTextHasEnvironmentReference(current.ElseList)
	case *parse.WithNode:
		return literalTextHasEnvironmentReference(current.List) ||
			literalTextHasEnvironmentReference(current.ElseList)
	}
	return false
}

func expandLiteralText(node parse.Node) {
	switch current := node.(type) {
	case *parse.ListNode:
		if current == nil {
			return
		}
		for _, child := range current.Nodes {
			expandLiteralText(child)
		}
	case *parse.TextNode:
		current.Text = []byte(os.ExpandEnv(string(current.Text)))
	case *parse.IfNode:
		expandLiteralText(current.List)
		expandLiteralText(current.ElseList)
	case *parse.RangeNode:
		expandLiteralText(current.List)
		expandLiteralText(current.ElseList)
	case *parse.WithNode:
		expandLiteralText(current.List)
		expandLiteralText(current.ElseList)
	}
}

// GeneratePath generates a worktree path using the configured template.
func (p *Processor) GeneratePath(baseDir string, repoInfo *url.RepositoryInfo, branch string) (string, error) {
	// Sanitize branch name only
	sanitizedBranch := p.sanitizeBranch(branch)
	filesystemInfo := url.RepositoryInfoForFilesystem(repoInfo)

	// Create template data
	data := &TemplateData{
		Host:       filesystemInfo.Host,
		Owner:      filesystemInfo.Owner,
		Repository: filesystemInfo.Repository,
		FullPath:   filesystemInfo.FullPath,
		Branch:     sanitizedBranch,
		Hash:       generateShortHash(repoInfo.FullPath + "/" + branch),
	}

	// Execute template
	var buf strings.Builder
	if err := p.template.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	relativePath := buf.String()

	// Join with base directory directly (no additional sanitization)
	// The template output should be used as-is, with only branch having been sanitized
	fullPath := filepath.Join(baseDir, relativePath)

	return fullPath, nil
}

// sanitizeBranch applies character sanitization rules to branch name only.
func (p *Processor) sanitizeBranch(branch string) string {
	sanitized := branch

	// Apply custom sanitize characters to branch name first
	for old, new := range p.sanitizeChars {
		sanitized = strings.ReplaceAll(sanitized, old, new)
	}

	// Then apply default filesystem sanitization to handle remaining problematic characters
	sanitized = utils.SanitizeForFilesystem(sanitized)

	return sanitized
}

// generateShortHash creates a short hash for the given input.
func generateShortHash(input string) string {
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", hash[:4]) // 8 character hex string
}

// ShortHash returns the 8-character short hash used to disambiguate worktrees.
// Callers outside this package (e.g. setup_commands template data) share this
// helper so the hash formula stays in one place.
func ShortHash(input string) string {
	return generateShortHash(input)
}
