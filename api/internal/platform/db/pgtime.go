package db

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Timestamptz wraps t as a non-null Postgres timestamp.
func Timestamptz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
