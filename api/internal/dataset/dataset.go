// Package datasets loads uploaded files into a session's DuckDB database and
// describes its tables. Go opens the database only briefly per operation, so
// the sandbox can open it read-only between uploads.
package dataset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"dataanalyst/api/internal/agent"
	"dataanalyst/api/internal/platform/storage"

	"github.com/duckdb/duckdb-go/v2"
)

var (
	ErrTooLarge    = errors.New("file is larger than the upload limit")
	ErrEmpty       = errors.New("file is empty")
	ErrUnsupported = errors.New("unsupported file type: upload a .csv or .parquet file")
	ErrBadFile     = errors.New("could not read the file")
	ErrBusy        = errors.New("the dataset is in use by a running query, try again in a moment")
)

const SampleRows = 5

var readers = map[string]string{
	".csv":     "read_csv_auto",
	".parquet": "read_parquet",
}

type Loader struct {
	Layout   storage.Layout
	MaxBytes int64
}

// Load stores the upload as a new table and returns its description.
// existing holds the session's current table names, to keep names unique.
func (l Loader) Load(ctx context.Context, sessionID, filename string, r io.Reader, existing []string) (agent.Table, error) {
	reader, ok := readers[strings.ToLower(filepath.Ext(filename))]
	if !ok {
		return agent.Table{}, ErrUnsupported
	}
	if err := l.Layout.EnsureSession(sessionID); err != nil {
		return agent.Table{}, err
	}
	tmp, err := os.CreateTemp(l.Layout.SessionDir(sessionID), "upload-*"+filepath.Ext(filename))
	if err != nil {
		return agent.Table{}, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r, l.MaxBytes+1))
	tmp.Close()
	switch {
	case err != nil:
		return agent.Table{}, fmt.Errorf("save upload: %w", err)
	case n > l.MaxBytes:
		return agent.Table{}, ErrTooLarge
	case n == 0:
		return agent.Table{}, ErrEmpty
	}

	name := TableName(filename, existing)
	dbPath := l.Layout.DBPath(sessionID)
	stmt := fmt.Sprintf(`CREATE TABLE %s AS SELECT * FROM %s(%s)`,
		quoteIdent(name), reader, quoteLiteral(tmp.Name()))
	if err := withWriter(ctx, dbPath, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, stmt)
		return err
	}); err != nil {
		if errors.Is(err, ErrBusy) {
			return agent.Table{}, err
		}
		return agent.Table{}, fmt.Errorf("%w: %v", ErrBadFile, err)
	}
	// The sandbox runs as another uid and opens the file read-only.
	_ = os.Chmod(dbPath, 0o644)

	tables, err := Describe(ctx, dbPath, name)
	if err != nil {
		return agent.Table{}, err
	}
	return tables[0], nil
}

// Describe returns every table, or only the named one, with sample rows.
func Describe(ctx context.Context, dbPath string, only string) ([]agent.Table, error) {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return []agent.Table{}, nil
	}
	db, err := sql.Open("duckdb", dbPath+"?access_mode=READ_ONLY")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	names, err := tableNames(ctx, db)
	if err != nil {
		return nil, err
	}
	if only != "" {
		if !slices.Contains(names, only) {
			return nil, fmt.Errorf("no table named %q; tables: %s", only, strings.Join(names, ", "))
		}
		names = []string{only}
	}
	out := make([]agent.Table, 0, len(names))
	for _, n := range names {
		t, err := describeTable(ctx, db, n)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func tableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = 'main' ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func describeTable(ctx context.Context, db *sql.DB, name string) (agent.Table, error) {
	t := agent.Table{Name: name, Columns: []agent.Column{}, SampleRows: [][]any{}}
	cols, err := db.QueryContext(ctx, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = 'main' AND table_name = ? ORDER BY ordinal_position`, name)
	if err != nil {
		return t, err
	}
	for cols.Next() {
		var c agent.Column
		if err := cols.Scan(&c.Name, &c.Type); err != nil {
			cols.Close()
			return t, err
		}
		t.Columns = append(t.Columns, c)
	}
	cols.Close()

	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+quoteIdent(name)).Scan(&t.Rows); err != nil {
		return t, err
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s LIMIT %d", quoteIdent(name), SampleRows))
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]any, len(t.Columns))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return t, err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = JSONSafe(v, t.Columns[i].Type)
		}
		t.SampleRows = append(t.SampleRows, row)
	}
	return t, rows.Err()
}

// JSONSafe converts a DuckDB value into something encoding/json renders well.
func JSONSafe(v any, duckType string) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil // encoding/json cannot represent NaN or infinity
		}
		return x
	case float32:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil
		}
		return x
	case nil, bool, string, int8, int16, int32, int64, uint8, uint16, uint32, uint64:
		return x
	case time.Time:
		if duckType == "DATE" {
			return x.Format(time.DateOnly)
		}
		return x.Format(time.RFC3339Nano)
	case []byte:
		return string(x)
	case *big.Int:
		return x.String()
	case duckdb.Decimal:
		f, _ := new(big.Float).SetInt(x.Value).Float64()
		for range x.Scale {
			f /= 10
		}
		return f
	default:
		return fmt.Sprint(x)
	}
}

var (
	nonWord    = regexp.MustCompile(`[^a-z0-9]+`)
	edgeUnders = regexp.MustCompile(`^_+|_+$`)
)

// TableName turns a filename into a safe, unique SQL identifier.
func TableName(filename string, existing []string) string {
	base := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	name := edgeUnders.ReplaceAllString(nonWord.ReplaceAllString(strings.ToLower(base), "_"), "")
	if name == "" {
		name = "data"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "t_" + name
	}
	candidate := name
	for i := 2; slices.Contains(existing, candidate); i++ {
		candidate = fmt.Sprintf("%s_%d", name, i)
	}
	return candidate
}

// withWriter opens the database for writing, retrying briefly while a
// sandbox process holds it open read-only.
func withWriter(ctx context.Context, dbPath string, fn func(*sql.DB) error) error {
	var lastErr error
	for attempt := range 10 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
		db, err := sql.Open("duckdb", dbPath)
		if err == nil {
			err = db.PingContext(ctx)
		}
		if err != nil {
			if db != nil {
				db.Close()
			}
			if strings.Contains(err.Error(), "lock") {
				lastErr = err
				continue
			}
			return err
		}
		err = fn(db)
		db.Close()
		return err
	}
	return fmt.Errorf("%w (%v)", ErrBusy, lastErr)
}

func quoteIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }
