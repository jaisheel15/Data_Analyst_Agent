package httpapi

import (
	"fmt"
	"regexp"
	"strings"

	"dataanalyst/api/internal/contract"
	"dataanalyst/api/internal/sandbox"
)

// Tool output is plain text tuned for the model: compact and unambiguous.

func formatSQL(r sandbox.Result) string {
	if r.Error != "" && len(r.Columns) == 0 {
		return ""
	}
	if len(r.Columns) == 0 {
		return "(statement ran; it returned no rows)"
	}
	var b strings.Builder
	b.WriteString(strings.Join(r.Columns, "\t"))
	for _, row := range r.Rows {
		b.WriteByte('\n')
		for i, v := range row {
			if i > 0 {
				b.WriteByte('\t')
			}
			b.WriteString(cell(v))
		}
	}
	if r.TotalRows > len(r.Rows) {
		fmt.Fprintf(&b, "\n(showing %d of %d rows)", len(r.Rows), r.TotalRows)
	} else {
		fmt.Fprintf(&b, "\n(%d rows)", r.TotalRows)
	}
	return b.String()
}

func formatPython(r sandbox.Result, chartIDs []string) string {
	var parts []string
	if r.Stdout != "" {
		parts = append(parts, strings.TrimRight(r.Stdout, "\n"))
	}
	if r.Stderr != "" {
		parts = append(parts, "stderr:\n"+strings.TrimRight(r.Stderr, "\n"))
	}
	for _, id := range chartIDs {
		parts = append(parts, "[chart saved: "+id+"; it is shown to the user]")
	}
	if len(parts) == 0 && r.Error == "" {
		return "(no output)"
	}
	return strings.Join(parts, "\n")
}

func formatTables(tables []contract.Table) string {
	if len(tables) == 0 {
		return "No datasets uploaded yet."
	}
	var b strings.Builder
	for i, t := range tables {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "table %s (%d rows)\ncolumns:", ident(t.Name), t.Rows)
		for _, c := range t.Columns {
			fmt.Fprintf(&b, "\n  %s %s", ident(c.Name), c.Type)
		}
		if len(t.SampleRows) > 0 {
			b.WriteString("\nsample rows:")
			for _, row := range t.SampleRows {
				cells := make([]string, len(row))
				for j, v := range row {
					cells[j] = cell(v)
				}
				b.WriteString("\n  " + strings.Join(cells, " | "))
			}
		}
	}
	return b.String()
}

var plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ident shows a name the way it must be written in SQL.
func ident(name string) string {
	if plainIdent.MatchString(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func cell(v any) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(v)
}
