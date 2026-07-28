package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/mattn/go-runewidth"
)

type helpItem struct {
	key  string
	desc string
}

func defaultHelpRows(row Row) [][]helpItem {
	action := helpItem{key: "c", desc: "shell"}
	if rowCanSync(row) {
		action = helpItem{key: "s", desc: "sync"}
	}
	return [][]helpItem{{
		{key: "↑↓", desc: "move"},
		{key: "↵", desc: "attach"},
		action,
		{key: "P", desc: "project"},
		{key: "n", desc: "new"},
		{key: "b", desc: "branch"},
		{key: "L", desc: "layout"},
		{key: "d", desc: "delete"},
		{key: "K", desc: "kill"},
		{key: "p", desc: "filter"},
		{key: "/", desc: "search"},
		{key: "r", desc: "refresh"},
		{key: "?", desc: "help"},
		{key: "q", desc: "quit"},
	}}
}

func confirmHelpRows() [][]helpItem {
	return [][]helpItem{{
		{key: "y", desc: "confirm"},
		{key: "n/esc", desc: "cancel"},
	}}
}

func inputHelpRows(mode inputMode) [][]helpItem {
	switch mode {
	case inputNewBranch:
		return [][]helpItem{{
			{key: "enter", desc: "create"},
			{key: "esc", desc: "cancel"},
		}}
	case inputExistingBranch:
		return [][]helpItem{{
			{key: "↑↓", desc: "select"},
			{key: "type", desc: "narrow"},
			{key: "enter", desc: "create"},
			{key: "esc", desc: "cancel"},
		}}
	case inputFilter, inputProjectFilter:
		return [][]helpItem{{
			{key: "enter", desc: "apply"},
			{key: "esc", desc: "clear"},
		}}
	case inputProjectSwitch:
		return [][]helpItem{{
			{key: "↑↓", desc: "select"},
			{key: "type", desc: "narrow"},
			{key: "enter", desc: "choose"},
			{key: "esc", desc: "cancel"},
		}}
	default:
		return nil
	}
}

func (m Model) helpRows() [][]helpItem {
	if m.confirm.kind != confirmNone {
		return confirmHelpRows()
	}
	if m.inputMode != inputNone {
		return inputHelpRows(m.inputMode)
	}
	return defaultHelpRows(m.selectedRow())
}

func rowCanSync(row Row) bool {
	return row.Entry == nil && row.Fleet != nil && !row.Fleet.Local && row.Fleet.CanMaterialize
}

func (m Model) renderFooter() string {
	return renderHelpTable(m.helpRows(), m.width)
}

func (m Model) footerHelpLines() int {
	return helpLines(m.helpRows(), m.width)
}

func helpLines(rows [][]helpItem, width int) int {
	lines := len(reflowHelpRows(rows, width))
	if lines < 1 {
		return 1
	}
	return lines
}

func reflowHelpRows(rows [][]helpItem, width int) [][]helpItem {
	if width <= 0 {
		return rows
	}

	cellWidth := func(item helpItem) int {
		w := runewidth.StringWidth(item.key)
		if item.desc != "" {
			w += 1 + runewidth.StringWidth(item.desc)
		}
		return w
	}

	maxItemsPerRow := 0
	for _, row := range rows {
		if len(row) > maxItemsPerRow {
			maxItemsPerRow = len(row)
		}
	}

	for ncols := maxItemsPerRow; ncols >= 1; ncols-- {
		var candidate [][]helpItem
		for _, row := range rows {
			for i := 0; i < len(row); i += ncols {
				end := min(i+ncols, len(row))
				candidate = append(candidate, row[i:end])
			}
		}

		colWidths := make([]int, ncols)
		for _, row := range candidate {
			for col, item := range row {
				if w := cellWidth(item); w > colWidths[col] {
					colWidths[col] = w
				}
			}
		}

		total := 0
		for col, colWidth := range colWidths {
			total += colWidth
			if col > 0 {
				total += 2
			}
		}
		if total <= width {
			return candidate
		}
	}

	var result [][]helpItem
	for _, row := range rows {
		for _, item := range row {
			result = append(result, []helpItem{item})
		}
	}
	return result
}

func renderHelpTable(rows [][]helpItem, width int) string {
	rows = reflowHelpRows(rows, width)
	if len(rows) == 0 {
		return ""
	}

	borderColor := lipgloss.Color("240")
	keyStyle := lipgloss.NewStyle().Faint(true)
	descStyle := lipgloss.NewStyle().Faint(true)
	cellStyle := lipgloss.NewStyle()
	cellWithBorder := lipgloss.NewStyle().
		PaddingLeft(1).
		Border(lipgloss.Border{Left: "▕"}, false, false, false, true).
		BorderForeground(borderColor)

	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}

	colMinW := make([]int, maxCols)
	for _, row := range rows {
		for col, item := range row {
			w := runewidth.StringWidth(item.key)
			if item.desc != "" {
				w += 1 + runewidth.StringWidth(item.desc)
			}
			if w > colMinW[col] {
				colMinW[col] = w
			}
		}
	}

	empty := make([][]bool, len(rows))
	t := table.New().
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderRow(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			minW := 0
			if col < len(colMinW) {
				minW = colMinW[col]
			}
			if col == 0 || (row < len(empty) && col < len(empty[row]) && empty[row][col]) {
				return cellStyle.Width(minW)
			}
			return cellWithBorder.Width(minW + 2)
		}).
		Wrap(false)

	for rowIndex, row := range rows {
		styled := make([]string, maxCols)
		empty[rowIndex] = make([]bool, maxCols)
		for i, item := range row {
			if item.desc != "" {
				styled[i] = keyStyle.Render(item.key) + " " + descStyle.Render(item.desc)
			} else {
				styled[i] = keyStyle.Render(item.key)
			}
		}
		for i := len(row); i < maxCols; i++ {
			empty[rowIndex][i] = true
		}
		t = t.Row(styled...)
	}

	return strings.TrimRight(t.Render(), "\n")
}
