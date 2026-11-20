package httpapi_test

// Seam 5: POST /api/sessions/{id}/datasets over HTTP, real Postgres and DuckDB.

import (
	"net/http"
	"strings"
	"testing"

	"dataanalyst/api/internal/httpapi"
)

const salesCSV = "region,amount,sold on\nnorth,10,2024-01-05\nsouth,20,2024-02-10\nnorth,5,2024-03-15\n"

func TestCSVUploadCreatesATableAndReturnsItsSchema(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	table := h.mustUpload(sid, "sales.csv", []byte(salesCSV))

	if table.Name != "sales" || table.Rows != 3 {
		t.Fatalf("table = %+v", table)
	}
	var names []string
	for _, c := range table.Columns {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "region,amount,sold on" {
		t.Fatalf("columns = %v (odd names like 'sold on' must be kept as is)", names)
	}
	if table.Columns[1].Type != "BIGINT" || table.Columns[2].Type != "DATE" {
		t.Fatalf("types = %+v", table.Columns)
	}
	if len(table.SampleRows) != 3 || table.SampleRows[0][0] != "north" {
		t.Fatalf("sample rows = %v", table.SampleRows)
	}
	if table.SampleRows[0][2] != "2024-01-05" {
		t.Fatalf("dates should be ISO strings, got %v", table.SampleRows[0][2])
	}
	if !fileExists(h.layout.DBPath(sid)) {
		t.Fatal("session.duckdb not created")
	}
}

func TestUploadedDatasetsAppearOnTheSession(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	h.mustUpload(sid, "sales.csv", []byte(salesCSV))
	_, raw := h.do("GET", "/api/sessions/"+sid, nil, "")
	var got httpapi.SessionView
	mustJSON(t, raw, &got)
	if len(got.Datasets) != 1 || got.Datasets[0].Name != "sales" {
		t.Fatalf("datasets = %+v", got.Datasets)
	}
}

func TestTableNamesAreCleanedAndKeptUnique(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	first := h.mustUpload(sid, "Q3 Sales (final).csv", []byte(salesCSV))
	second := h.mustUpload(sid, "q3-sales-final.csv", []byte(salesCSV))
	digits := h.mustUpload(sid, "2024.csv", []byte(salesCSV))
	if first.Name != "q3_sales_final" || second.Name != "q3_sales_final_2" || digits.Name != "t_2024" {
		t.Fatalf("names = %q, %q, %q", first.Name, second.Name, digits.Name)
	}
}

func TestUploadErrors(t *testing.T) {
	h := newHarness(t, func(c *httpapi.Config) { c.MaxUploadBytes = 64 })
	sid := h.newSession()
	for _, tc := range []struct {
		name     string
		session  string
		filename string
		content  string
		status   int
	}{
		{"too large", sid, "big.csv", strings.Repeat("a,b\n", 100), http.StatusRequestEntityTooLarge},
		{"empty", sid, "empty.csv", "", http.StatusBadRequest},
		{"unsupported type", sid, "notes.txt", "hello", http.StatusUnsupportedMediaType},
		{"unknown session", "00000000-0000-0000-0000-000000000000", "a.csv", "a\n1\n", http.StatusNotFound},
		{"malformed session id", "not-a-uuid", "a.csv", "a\n1\n", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := h.upload(tc.session, tc.filename, []byte(tc.content))
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.status, raw)
			}
			if !strings.Contains(string(raw), `"error"`) {
				t.Fatalf("error body should be JSON with an error field: %s", raw)
			}
		})
	}
}

func TestNaNValuesInUploadsBecomeNull(t *testing.T) {
	h := newHarness(t, nil)
	sid := h.newSession()
	table := h.mustUpload(sid, "nan.csv", []byte("a,b\n1,NaN\n2,inf\n3,1.5\n"))
	if table.SampleRows[0][1] != nil || table.SampleRows[1][1] != nil || table.SampleRows[2][1] != 1.5 {
		t.Fatalf("sample rows = %v", table.SampleRows)
	}
	// The session (and its schema in every run request) must keep working.
	if resp, _ := h.ask(sid, "q"); resp.StatusCode != http.StatusOK {
		t.Fatalf("ask status = %d", resp.StatusCode)
	}
	if got := h.agent.lastRequest(t).Schema[0].SampleRows[0][1]; got != nil {
		t.Fatalf("run request sample = %v", got)
	}
}
