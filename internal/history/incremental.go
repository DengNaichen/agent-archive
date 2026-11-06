package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/DengNaichen/agent-archive/internal/runstate"
)

// Incremental preserves prior evidence, including files removed or rewritten at
// the source. Changed files are replayed whole so parser context is never lost.
func collectIncremental(ctx context.Context, o CollectOptions) (report CollectReport, err error) {
	home, e := resolvedPath(o.Home)
	if e != nil {
		return report, e
	}
	out, e := resolvedPath(o.Output)
	if e != nil {
		return report, e
	}
	if within(home, out) {
		return report, fault.New("output must be outside source home")
	}
	lock, e := runstate.Lock(ctx, out+".lock.sqlite")
	if e != nil {
		return report, e
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if _, e = os.Stat(out); errors.Is(e, os.ErrNotExist) {
		o.Incremental = false
		report, e = Collect(ctx, o)
		return report, e
	}
	store, e := Open(filepath.Join(out, "cleaned.sqlite"))
	if e != nil {
		return report, e
	}
	var native string
	nativeErr := store.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='schema_version'").Scan(&native)
	if store.legacyCodex || nativeErr != nil || native != "1" {
		return report, errors.Join(fault.New("incremental collection requires a Go snapshot"), store.Close())
	}
	source := o.Source
	if source == "claude-code" {
		source = "claude_code"
	}
	if source != store.Source() {
		return report, errors.Join(fault.New("snapshot source differs from requested source"), store.Close())
	}
	if e = store.Close(); e != nil {
		return report, e
	}
	db, e := sql.Open("sqlite", filepath.Join(out, "cleaned.sqlite"))
	if e != nil {
		return report, e
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	var bound string
	e = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='incremental_home'").Scan(&bound)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return report, e
	}
	if bound != "" && bound != home {
		return report, fault.New("incremental snapshot belongs to a different source home")
	}
	if _, e = db.ExecContext(ctx, `INSERT OR IGNORE INTO metadata VALUES('incremental_home',?),('incremental_version','1');
 CREATE TABLE IF NOT EXISTS archive_versions(path TEXT,sha256 TEXT,bytes INTEGER,snapshot TEXT,changed_during_copy INTEGER,compression TEXT,PRIMARY KEY(path,sha256));
 INSERT OR IGNORE INTO archive_versions SELECT * FROM source_files;
`, home); e != nil {
		return report, e
	}
	roots := []string{"sessions", "archived_sessions"}
	if source == "claude_code" {
		roots = []string{"projects"}
	}
	var files []string
	for _, root := range roots {
		base := filepath.Join(home, root)
		if _, e = os.Stat(base); errors.Is(e, os.ErrNotExist) {
			continue
		}
		e = filepath.WalkDir(base, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if e = ctx.Err(); e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.Type().IsRegular() && (strings.HasSuffix(path, ".jsonl") || (source == "codex" && strings.HasSuffix(path, ".jsonl.zst"))) {
				files = append(files, path)
			}
			return nil
		})
		if e != nil {
			return report, e
		}
	}
	if len(files) == 0 {
		return report, fault.New("no transcript files found")
	}
	if source == "claude_code" {
		p := filepath.Join(home, "history.jsonl")
		if st, e := os.Lstat(p); e == nil && st.Mode().IsRegular() {
			files = append(files, p)
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return report, e
		}
	}
	if _, e = db.ExecContext(ctx, "UPDATE metadata SET value='false' WHERE key='collection_complete'"); e != nil {
		return report, e
	}
	report.Source = source
	for i, path := range files {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		rel, e := filepath.Rel(home, path)
		if e != nil {
			return report, e
		}
		rel = filepath.ToSlash(rel)
		hash, e := fileHash(ctx, path)
		if e != nil {
			return report, e
		}
		var previous string
		e = db.QueryRowContext(ctx, "SELECT sha256 FROM source_files WHERE path=?", rel).Scan(&previous)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return report, e
		}
		if hash == previous {
			report.Skipped++
			continue
		}
		// A unique capture directory is retained on failure; no old evidence is replaced.
		version, e := os.MkdirTemp(out, "capture-")
		if e != nil {
			return report, e
		}
		a, e := snapshot(ctx, path, version, rel, source == "claude_code")
		if e != nil {
			return report, e
		}
		a.Snapshot = filepath.ToSlash(filepath.Join(filepath.Base(version), a.Snapshot))
		var priorSnapshot string
		lookupErr := db.QueryRowContext(ctx, "SELECT snapshot FROM archive_versions WHERE path=? AND sha256=?", rel, a.SHA).Scan(&priorSnapshot)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return report, lookupErr
		}
		knownVersion := lookupErr == nil
		if knownVersion {
			a.Snapshot = priorSnapshot
			a.DiskPath = filepath.Join(out, priorSnapshot)
		}
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			return report, e
		}
		_, e = tx.ExecContext(ctx, "DELETE FROM source_files WHERE path=?", rel)
		if e == nil {
			e = saveArchive(ctx, tx, a)
		}
		if e == nil {
			_, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO archive_versions SELECT * FROM source_files WHERE path=?", rel)
		}
		// Reverting to a captured version reuses its original archive and occurrences.
		if e == nil && !knownVersion && rel != "history.jsonl" {
			if source == "codex" {
				e = ingestCodex(ctx, tx, a, &report)
			} else {
				e = ingestClaude(ctx, tx, a, &report)
			}
		}
		if e != nil {
			return report, errors.Join(e, tx.Rollback())
		}
		if e = tx.Commit(); e != nil {
			return report, e
		}
		report.Files++
		if a.Changed {
			report.Changed++
		}
		if o.Progress != nil {
			if e = o.Progress(map[string]any{"event": "collection_progress", "files": i + 1, "total_files": len(files), "occurrences": report.Occurrences}); e != nil {
				return report, e
			}
		}
	}
	store, e = Open(filepath.Join(out, "cleaned.sqlite"))
	if e != nil {
		return report, e
	}
	v, e := store.verify(ctx, o.Progress, true)
	e = errors.Join(e, store.Close())
	if e != nil {
		return report, e
	}
	report.Verified = true
	if e = runstate.Write(filepath.Join(out, "verification.json"), v); e != nil {
		return report, e
	}
	if e = runstate.Write(filepath.Join(out, "summary.json"), report); e != nil {
		return report, e
	}
	_, e = db.ExecContext(ctx, "UPDATE metadata SET value='true' WHERE key='collection_complete'")
	return report, e
}

func fileHash(ctx context.Context, path string) (value string, err error) {
	f, e := os.Open(path)
	if e != nil {
		return "", fmt.Errorf("read source fingerprint: %w", e)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := sha256.New()
	if _, e = io.Copy(h, &contextReader{ctx: ctx, r: f}); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
