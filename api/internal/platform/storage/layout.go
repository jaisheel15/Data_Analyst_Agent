// Package storage defines where per-session files live on disk.
//
//	<root>/sessions/<id>/data/session.duckdb   uploaded tables (sandbox: /data, read-only)
//	<root>/sessions/<id>/out/                   charts written by a tool call (sandbox: /out)
//	<root>/sessions/<id>/scratch/               intermediate results (sandbox: /scratch)
//	<root>/sessions/<id>/charts/<chart>.png     charts kept after a tool call
package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

type Layout struct{ Root string }

func (l Layout) SessionDir(id string) string { return filepath.Join(l.Root, "sessions", id) }
func (l Layout) DataDir(id string) string    { return filepath.Join(l.SessionDir(id), "data") }
func (l Layout) DBPath(id string) string     { return filepath.Join(l.DataDir(id), "session.duckdb") }
func (l Layout) OutDir(id string) string     { return filepath.Join(l.SessionDir(id), "out") }
func (l Layout) ScratchDir(id string) string { return filepath.Join(l.SessionDir(id), "scratch") }
func (l Layout) ChartsDir(id string) string  { return filepath.Join(l.SessionDir(id), "charts") }

// EnsureSession creates the session directories. The sandbox runs as a
// different uid, so data is world-readable and out/scratch world-writable.
func (l Layout) EnsureSession(id string) error {
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{l.SessionDir(id), 0o755},
		{l.DataDir(id), 0o755},
		{l.OutDir(id), 0o777},
		{l.ScratchDir(id), 0o777},
		{l.ChartsDir(id), 0o755},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return fmt.Errorf("create %s: %w", d.path, err)
		}
		// MkdirAll is subject to umask; set the mode explicitly.
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", d.path, err)
		}
	}
	return nil
}

func (l Layout) RemoveSession(id string) error { return os.RemoveAll(l.SessionDir(id)) }

// ClearOut empties the session's out dir, where the sandbox writes charts.
func (l Layout) ClearOut(id string) {
	dir := l.OutDir(id)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
