package cmd

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
	"go.kenn.io/kwt/internal/maintenance"
)

type doctorRenderOptions struct {
	Width   int
	Color   bool
	HomeDir string
	TempDir string
}

type doctorFindingPresentation struct {
	Rank   int
	Title  string
	Action string
	Known  bool
}

type doctorFindingGroup struct {
	Code         maintenance.FindingCode
	Fixable      bool
	Presentation doctorFindingPresentation
	Repositories []doctorRepositoryFindings
}

type doctorRepositoryFindings struct {
	Label    string
	Findings []maintenance.Finding
}

type doctorStyles struct {
	title, summary, ready, review, group, repository, path, muted, action lipgloss.Style
}

func renderDoctorHuman(report maintenance.Report, options doctorRenderOptions) string {
	if options.Width <= 0 {
		options.Width = 100
	}
	if (report.Summary.Healthy || report.Summary.Findings == 0) && report.Summary.FixedFindings == 0 {
		return doctorStyled(options.Color, lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true),
			"✓ Worktree maintenance is healthy.") + "\n"
	}

	styles := newDoctorStyles()
	groups := doctorFindingGroups(report)
	var output strings.Builder
	writeDoctorLine(&output, doctorStyled(options.Color, styles.title, "kwt doctor"))
	reviewVerb := "need"
	if report.Summary.ManualFindings == 1 {
		reviewVerb = "needs"
	}
	summary := fmt.Sprintf("%d %s · %d ready to fix · %d %s review",
		report.Summary.Findings, plural(report.Summary.Findings, "issue", "issues"),
		report.Summary.FixableFindings, report.Summary.ManualFindings, reviewVerb)
	if report.Summary.FixedFindings > 0 {
		remainVerb := "remain"
		if report.Summary.Findings == 1 {
			remainVerb = "remains"
		}
		summary = fmt.Sprintf("%d fixed · %d %s %s · %d ready to fix · %d %s review",
			report.Summary.FixedFindings,
			report.Summary.Findings, plural(report.Summary.Findings, "issue", "issues"),
			remainVerb,
			report.Summary.FixableFindings, report.Summary.ManualFindings, reviewVerb)
	}
	writeDoctorWrapped(&output, "", doctorStyled(options.Color, styles.summary, summary), options.Width)

	if report.Summary.FixedFindings > 0 {
		fixedGroups := doctorFindingGroups(maintenance.Report{Repositories: report.Fixed})
		renderDoctorSection(&output, "Fixed", report.Summary.FixedFindings, true, false, fixedGroups, options, styles)
	}
	renderDoctorSection(&output, "Ready to fix", report.Summary.FixableFindings, true, true, groups, options, styles)
	renderDoctorSection(&output, "Needs review", report.Summary.ManualFindings, false, true, groups, options, styles)
	if report.Summary.Findings == 0 {
		writeDoctorLine(&output, "")
		writeDoctorLine(&output, doctorStyled(options.Color, styles.ready, "No issues remain."))
	}
	if report.Summary.FixableFindings > 0 {
		writeDoctorLine(&output, "")
		footer := fmt.Sprintf("Run kwt doctor --fix to repair confirmed issues. (%d available)", report.Summary.FixableFindings)
		writeDoctorWrapped(&output, "", doctorStyled(options.Color, styles.action, footer), options.Width)
	}
	return output.String()
}

func renderDoctorSection(
	output *strings.Builder,
	title string,
	count int,
	fixable bool,
	showAction bool,
	groups []doctorFindingGroup,
	options doctorRenderOptions,
	styles doctorStyles,
) {
	if count == 0 {
		return
	}
	writeDoctorLine(output, "")
	headingStyle := styles.review
	if fixable {
		headingStyle = styles.ready
	}
	writeDoctorLine(output, doctorStyled(options.Color, headingStyle,
		fmt.Sprintf("%s  %d", title, count)))
	for _, group := range groups {
		if group.Fixable != fixable {
			continue
		}
		groupCount := 0
		for _, repository := range group.Repositories {
			groupCount += len(repository.Findings)
		}
		writeDoctorLine(output, "")
		groupHeading := middleElide(fmt.Sprintf("%s (%d)", group.Presentation.Title, groupCount), max(8, options.Width-2))
		writeDoctorLine(output, "  "+doctorStyled(options.Color, styles.group, groupHeading))
		for _, repository := range group.Repositories {
			label := doctorDisplayPath(repository.Label, max(8, options.Width-4), options)
			writeDoctorLine(output, "    "+doctorStyled(options.Color, styles.repository, label))
			for _, finding := range repository.Findings {
				renderDoctorFinding(output, finding, group.Presentation, options, styles)
			}
		}
		action := group.Presentation.Action
		if action == "" {
			for _, repository := range group.Repositories {
				for _, finding := range repository.Findings {
					if finding.Remediation != "" {
						action = finding.Remediation
						break
					}
				}
				if action != "" {
					break
				}
			}
		}
		if showAction && action != "" {
			writeDoctorWrapped(output, "    Next: ", doctorStyled(options.Color, styles.action, action), options.Width)
		}
	}
}

func renderDoctorFinding(
	output *strings.Builder,
	finding maintenance.Finding,
	presentation doctorFindingPresentation,
	options doctorRenderOptions,
	styles doctorStyles,
) {
	const rowPrefix = "      • "
	available := max(8, options.Width-runewidth.StringWidth(rowPrefix))
	line := ""
	switch finding.Code {
	case maintenance.ProjectPathMoved:
		oldPath := finding.Evidence["old_path"]
		newPath := finding.Evidence["new_path"]
		if oldPath == "" {
			oldPath = finding.Path
		}
		line = doctorPathTransition(oldPath, newPath, available, options)
	case maintenance.BrokenWorktreeBacklink:
		current := finding.Evidence["current_target"]
		expected := finding.Evidence["expected_target"]
		if current != "" && expected != "" {
			line = doctorPathTransition(current, expected, available, options)
		}
	case maintenance.AmbiguousProjectRelocation:
		line = doctorDisplayPath(finding.Path, available, options)
	default:
		line = doctorDisplayPath(finding.Path, available, options)
	}
	if line == "" {
		line = doctorDisplayPath(finding.Path, available, options)
	}
	if line == "" {
		line = finding.Message
	}
	writeDoctorLine(output, rowPrefix+doctorStyled(options.Color, styles.path, line))

	if finding.Code == maintenance.AmbiguousProjectRelocation {
		candidates := splitDoctorEvidencePaths(finding.Evidence["candidate_paths"])
		for _, candidate := range candidates {
			writeDoctorLine(output, "        "+doctorStyled(options.Color, styles.muted,
				"candidate: "+doctorDisplayPath(candidate, max(8, options.Width-19), options)))
		}
	}
	if finding.Code == maintenance.AmbiguousWorktreeBacklink {
		for _, claimant := range splitDoctorClaimants(finding.Evidence["claimants"]) {
			writeDoctorLine(output, "        "+doctorStyled(options.Color, styles.muted,
				"claimed by: "+doctorDisplayPath(claimant, max(8, options.Width-20), options)))
		}
	}
	if finding.Code == maintenance.DuplicateRegistryEntry {
		for _, alias := range splitDoctorEvidencePaths(finding.Evidence["paths"]) {
			writeDoctorLine(output, "        "+doctorStyled(options.Color, styles.muted,
				"alias: "+doctorDisplayPath(alias, max(8, options.Width-15), options)))
		}
	}
	if (!presentation.Known || !finding.Fixable) && finding.Message != "" && finding.Message != line {
		writeDoctorWrapped(output, "        ", doctorStyled(options.Color, styles.muted, finding.Message), options.Width)
	}
	for _, evidence := range doctorHumanEvidence(finding) {
		writeDoctorWrapped(output, "        ", doctorStyled(options.Color, styles.muted, evidence), options.Width)
	}
}

func doctorFindingGroups(report maintenance.Report) []doctorFindingGroup {
	type groupKey struct {
		fixable bool
		code    maintenance.FindingCode
		action  string
	}
	type groupAccumulator struct {
		presentation doctorFindingPresentation
		repositories map[string][]maintenance.Finding
	}
	accumulators := make(map[groupKey]*groupAccumulator)
	for _, repository := range report.Repositories {
		if len(repository.Findings) == 0 {
			continue
		}
		label := repository.RepositoryIdentity
		if label == "" {
			label = repository.Root
		}
		if label == "" {
			label = "Registry"
		}
		for _, finding := range repository.Findings {
			presentation := doctorPresentation(finding.Code)
			action := presentation.Action
			if !finding.Fixable || action == "" {
				action = finding.Remediation
			}
			key := groupKey{fixable: finding.Fixable, code: finding.Code, action: action}
			accumulator := accumulators[key]
			if accumulator == nil {
				presentation.Action = action
				accumulator = &groupAccumulator{
					presentation: presentation,
					repositories: make(map[string][]maintenance.Finding),
				}
				accumulators[key] = accumulator
			}
			accumulator.repositories[label] = append(accumulator.repositories[label], finding)
		}
	}

	groups := make([]doctorFindingGroup, 0, len(accumulators))
	for key, accumulator := range accumulators {
		group := doctorFindingGroup{Code: key.code, Fixable: key.fixable, Presentation: accumulator.presentation}
		for label, findings := range accumulator.repositories {
			sort.Slice(findings, func(left, right int) bool {
				if findings[left].Path == findings[right].Path {
					return findings[left].Message < findings[right].Message
				}
				return findings[left].Path < findings[right].Path
			})
			group.Repositories = append(group.Repositories, doctorRepositoryFindings{Label: label, Findings: findings})
		}
		sort.Slice(group.Repositories, func(left, right int) bool {
			return group.Repositories[left].Label < group.Repositories[right].Label
		})
		groups = append(groups, group)
	}
	sort.Slice(groups, func(left, right int) bool {
		if groups[left].Fixable != groups[right].Fixable {
			return groups[left].Fixable
		}
		if groups[left].Presentation.Rank != groups[right].Presentation.Rank {
			return groups[left].Presentation.Rank < groups[right].Presentation.Rank
		}
		if groups[left].Code != groups[right].Code {
			return groups[left].Code < groups[right].Code
		}
		return groups[left].Presentation.Action < groups[right].Presentation.Action
	})
	return groups
}

func doctorHumanEvidence(finding maintenance.Finding) []string {
	labels := []struct {
		key   string
		label string
	}{
		{key: "configured_repository", label: "configured"},
		{key: "registry_repository", label: "registry"},
		{key: "live_repository", label: "live"},
	}
	result := make([]string, 0, len(labels))
	for _, item := range labels {
		if value := finding.Evidence[item.key]; value != "" {
			result = append(result, item.label+": "+value)
		}
	}
	return result
}

func doctorPresentation(code maintenance.FindingCode) doctorFindingPresentation {
	presentations := map[maintenance.FindingCode]doctorFindingPresentation{
		maintenance.ProjectPathMoved:           {Rank: 10, Title: "Project moved", Action: "Update the registration to the verified repository path.", Known: true},
		maintenance.StaleProjectRegistration:   {Rank: 20, Title: "Stale project registration", Action: "Remove the confirmed stale project registration.", Known: true},
		maintenance.BrokenWorktreeBacklink:     {Rank: 30, Title: "Outdated worktree backlink", Action: "Repair the verified worktree backlink.", Known: true},
		maintenance.MissingWorktreeDirectory:   {Rank: 40, Title: "Missing worktree directory", Action: "Prune the confirmed stale Git worktree record.", Known: true},
		maintenance.StaleRegistryEntry:         {Rank: 50, Title: "Stale registry entry", Action: "Remove the confirmed stale registry record.", Known: true},
		maintenance.RegistryGenerationMismatch: {Rank: 60, Title: "Registry generation mismatch", Action: "Reconcile only the verified legacy registry record.", Known: true},
		maintenance.DuplicateRegistryEntry:     {Rank: 70, Title: "Duplicate registry paths", Action: "Collapse equivalent aliases to one registry record.", Known: true},
		maintenance.AmbiguousProjectRelocation: {Rank: 10, Title: "Project location is ambiguous", Action: "Choose the intended repository path and update the registration manually.", Known: true},
		maintenance.ProjectUnreachable:         {Rank: 20, Title: "Repository could not be inspected", Action: "Restore access or update the registration, then rerun kwt doctor.", Known: true},
		maintenance.AmbiguousWorktreeBacklink:  {Rank: 30, Title: "Worktree backlink is ambiguous", Action: "Choose the intended worktree and repair the conflicting copy manually.", Known: true},
		maintenance.RepositoryIdentityMismatch: {Rank: 40, Title: "Repository identity mismatch", Action: "Review the configured, registry, and live repository identities.", Known: true},
		maintenance.UnverifiedRegistryEntry:    {Rank: 45, Title: "Unverified registry path", Action: "Review or remove the unverified registry policy manually.", Known: true},
		maintenance.MissingGeneration:          {Rank: 50, Title: "Missing worktree generation", Action: "Initialize the Git generation from the verified repository, then rerun doctor.", Known: true},
	}
	if presentation, ok := presentations[code]; ok {
		return presentation
	}
	title := strings.TrimSpace(strings.ReplaceAll(string(code), "_", " "))
	if title == "" {
		title = "Unknown finding"
	}
	return doctorFindingPresentation{Rank: 1000, Title: strings.ToUpper(title[:1]) + title[1:]}
}

func doctorPathTransition(oldPath, newPath string, width int, options doctorRenderOptions) string {
	if newPath == "" {
		return doctorDisplayPath(oldPath, width, options)
	}
	oldDisplay := doctorDisplayPath(oldPath, 0, options)
	newDisplay := doctorDisplayPath(newPath, 0, options)
	separator := " → "
	budget := max(2, width-runewidth.StringWidth(separator))
	leftBudget := budget / 2
	rightBudget := budget - leftBudget
	return middleElide(oldDisplay, leftBudget) + separator + middleElide(newDisplay, rightBudget)
}

func doctorDisplayPath(path string, width int, options doctorRenderOptions) string {
	display := shortenDoctorPath(path, options.HomeDir, "~")
	if display == path {
		display = shortenDoctorPath(path, options.TempDir, "$TMPDIR")
	}
	if width > 0 {
		display = middleElide(display, width)
	}
	return display
}

func shortenDoctorPath(path, prefix, replacement string) string {
	if path == "" || prefix == "" {
		return path
	}
	cleanPath := filepath.Clean(path)
	cleanPrefix := filepath.Clean(prefix)
	if cleanPath == cleanPrefix {
		return replacement
	}
	relative, err := filepath.Rel(cleanPrefix, cleanPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.Join(replacement, relative)
}

func middleElide(value string, width int) string {
	if width <= 0 || runewidth.StringWidth(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	contentWidth := width - 1
	leftWidth := contentWidth / 2
	rightWidth := contentWidth - leftWidth
	left := runewidth.Truncate(value, leftWidth, "")
	right := runewidth.TruncatePrefix(value, rightWidth, "")
	return left + "…" + right
}

func splitDoctorEvidencePaths(value string) []string {
	var result []string
	for _, path := range strings.Split(value, "\n") {
		if path = strings.TrimSpace(path); path != "" {
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func splitDoctorClaimants(value string) []string {
	var result []string
	for _, claimant := range strings.Split(value, ",") {
		if claimant = strings.TrimSpace(claimant); claimant != "" {
			result = append(result, claimant)
		}
	}
	sort.Strings(result)
	return result
}

func writeDoctorWrapped(output *strings.Builder, prefix, value string, width int) {
	available := max(8, width-runewidth.StringWidth(prefix))
	wrapped := lipgloss.Wrap(value, available, " ")
	for index, line := range strings.Split(wrapped, "\n") {
		linePrefix := strings.Repeat(" ", runewidth.StringWidth(prefix))
		if index == 0 {
			linePrefix = prefix
		}
		writeDoctorLine(output, linePrefix+line)
	}
}

func writeDoctorLine(output *strings.Builder, line string) {
	output.WriteString(line)
	output.WriteByte('\n')
}

func newDoctorStyles() doctorStyles {
	return doctorStyles{
		title:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")),
		summary:    lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		ready:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")),
		review:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11")),
		group:      lipgloss.NewStyle().Bold(true),
		repository: lipgloss.NewStyle().Foreground(lipgloss.Color("12")),
		path:       lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
		muted:      lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		action:     lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
	}
}

func doctorStyled(enabled bool, style lipgloss.Style, value string) string {
	if !enabled {
		return value
	}
	return style.Render(value)
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
