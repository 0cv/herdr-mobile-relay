package question

import (
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

type optionRow struct {
	line     int
	focus    bool
	number   int
	checkbox bool
	checked  bool
	text     string
}

type rowScan struct {
	checkboxes   bool
	restartAtOne bool
	endRunOnGap  bool
}

func matchRow(line string, checkboxes bool) (optionRow, bool) {
	if checkboxes {
		if match := checkboxPattern.FindStringSubmatch(line); match != nil {
			number, _ := strconv.Atoi(match[2])
			return optionRow{
				focus:    match[1] != "",
				number:   number,
				checkbox: true,
				checked:  strings.TrimSpace(match[3]) != "",
				text:     match[4],
			}, true
		}
	}
	match := menuPattern.FindStringSubmatch(line)
	if match == nil {
		return optionRow{}, false
	}
	number, _ := strconv.Atoi(match[2])
	return optionRow{focus: match[1] != "", number: number, text: match[3]}, true
}

func scanRows(lines []string, from, to int, scan rowScan) [][]optionRow {
	var runs [][]optionRow
	var run []optionRow
	expected := 1
	endRun := func() {
		if len(run) > 0 {
			runs = append(runs, run)
		}
		run = nil
		expected = 1
	}
	for index := from; index < to; index++ {
		row, ok := matchRow(lines[index], scan.checkboxes)
		if !ok {
			continue
		}
		row.line = index
		if scan.restartAtOne && row.number == 1 {
			endRun()
		}
		if row.number != expected {
			if scan.endRunOnGap {
				endRun()
			}
			continue
		}
		run = append(run, row)
		expected++
	}
	endRun()
	return runs
}

func lastRun(runs [][]optionRow) []optionRow {
	if len(runs) == 0 {
		return nil
	}
	return runs[len(runs)-1]
}

func compactRows(rows []optionRow, stripSelectedMark bool) {
	for index := range rows {
		row := &rows[index]
		row.text = textutil.Compact(row.text, 500)
		if stripSelectedMark && selectedPattern.MatchString(row.text) {
			row.checked = true
			row.text = strings.TrimSpace(selectedPattern.ReplaceAllString(row.text, ""))
		}
	}
}

func optionsFromRows(lines []string, rows []optionRow, end int) ([]Option, Focus) {
	options := make([]Option, 0, len(rows))
	focus := Focus{Kind: "option"}
	for index, row := range rows {
		rowEnd := end
		if index+1 < len(rows) {
			rowEnd = rows[index+1].line
		}
		options = append(options, Option{
			Index:       index,
			Label:       row.text,
			Description: description(lines, row.line, rowEnd),
			Selected:    row.checked,
		})
		if row.focus {
			focus = Focus{Kind: "option", Index: index}
		}
	}
	return options, focus
}
