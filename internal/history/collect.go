package history

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/klauspost/compress/zstd"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Progress func(any) error
type CollectOptions struct {
	Source, Home, Output string
	Progress             Progress
}
type CollectReport struct {
	Source      string `json:"source"`
	Files       int    `json:"files"`
	Occurrences int    `json:"occurrences"`
	Issues      int    `json:"issues"`
	Changed     int    `json:"changed_files"`
	Verified    bool   `json:"verified"`
}

const maxLine = 64 * 1024 * 1024

func eventTime(s string) (string, bool) {
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return "1970-01-01 00:00:00.000", false
	}
	return t.UTC().Format("2006-01-02 15:04:05.000"), true
}
func within(parent, path string) bool {
	rel, e := filepath.Rel(parent, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func resolvedPath(path string) (string, error) {
	p, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	var rest []string
	for {
		real, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		}
		if !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", e
		}
		rest = append(rest, filepath.Base(p))
		p = parent
	}
}
func Collect(ctx context.Context, o CollectOptions) (report CollectReport, err error) {
	if o.Source == "claude-code" {
		o.Source = "claude_code"
	}
	if o.Source != "codex" && o.Source != "claude_code" {
		return report, fault.New("unsupported source")
	}
	home, e := resolvedPath(o.Home)
	if e != nil {
		return report, fmt.Errorf("resolve source home: %w", e)
	}
	out, e := resolvedPath(o.Output)
	if e != nil {
		return report, fmt.Errorf("resolve output: %w", e)
	}
	if within(home, out) {
		return report, fault.New("output must be outside source home")
	}
	if _, e = os.Lstat(out); !errors.Is(e, os.ErrNotExist) {
		return report, fault.New("output must be a new directory")
	}
	roots := []string{"sessions", "archived_sessions"}
	if o.Source == "claude_code" {
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
			if !d.IsDir() && (strings.HasSuffix(path, ".jsonl") || (o.Source == "codex" && strings.HasSuffix(path, ".jsonl.zst"))) {
				files = append(files, path)
			}
			return nil
		})
		if e != nil {
			return report, fmt.Errorf("inventory source files: %w", e)
		}
	}
	if len(files) == 0 {
		return report, fault.New("no transcript files found")
	}
	if e = os.MkdirAll(filepath.Dir(out), 0700); e != nil {
		return report, fmt.Errorf("create output parent: %w", e)
	}
	if e = os.Mkdir(out, 0700); e != nil {
		return report, fmt.Errorf("create output: %w", e)
	}
	dbpath := filepath.Join(out, "cleaned.sqlite")
	f, e := os.OpenFile(dbpath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return report, fmt.Errorf("create snapshot: %w", e)
	}
	if e = f.Close(); e != nil {
		return report, e
	}
	db, e := sql.Open("sqlite", dbpath)
	if e != nil {
		return report, fmt.Errorf("open new snapshot: %w", e)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	if _, e = db.ExecContext(ctx, schema); e != nil {
		return report, fmt.Errorf("create snapshot schema: %w", e)
	}
	if _, e = db.ExecContext(ctx, "INSERT INTO metadata VALUES('source',?),('schema_version','1'),('collection_complete','false')", o.Source); e != nil {
		return report, fmt.Errorf("write metadata: %w", e)
	}
	report.Source = o.Source
	for i, path := range files {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		rel, e := filepath.Rel(home, path)
		if e != nil {
			return report, e
		}
		rel = filepath.ToSlash(rel)
		a, e := snapshot(ctx, path, out, rel, o.Source == "claude_code")
		if e != nil {
			return report, e
		}
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			return report, fmt.Errorf("start file import: %w", e)
		}
		e = saveArchive(ctx, tx, a)
		if e == nil {
			if o.Source == "codex" {
				e = ingestCodex(ctx, tx, a, &report)
			} else {
				e = ingestClaude(ctx, tx, a, &report)
			}
		}
		if e != nil {
			return report, errors.Join(e, tx.Rollback())
		}
		if e = tx.Commit(); e != nil {
			return report, fmt.Errorf("commit file import: %w", e)
		}
		report.Files++
		if a.Changed {
			report.Changed++
		}
		if o.Progress != nil && (i%25 == 24 || i+1 == len(files)) {
			if e = o.Progress(map[string]any{"event": "collection_progress", "files": i + 1, "total_files": len(files), "occurrences": report.Occurrences}); e != nil {
				return report, e
			}
		}
	}
	if o.Source == "claude_code" {
		history := filepath.Join(home, "history.jsonl")
		if st, e := os.Lstat(history); e == nil && st.Mode().IsRegular() {
			a, e := snapshot(ctx, history, out, "history.jsonl", true)
			if e != nil {
				return report, e
			}
			tx, e := db.BeginTx(ctx, nil)
			if e != nil {
				return report, e
			}
			if e = saveArchive(ctx, tx, a); e != nil {
				return report, errors.Join(e, tx.Rollback())
			}
			if e = tx.Commit(); e != nil {
				return report, e
			}
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return report, fmt.Errorf("inspect history index: %w", e)
		}
	}
	// A separate read-only connection verifies durable bytes, not the in-memory parser state.
	store, e := Open(dbpath)
	if e != nil {
		return report, e
	}
	verification, e := store.verify(ctx, o.Progress, true)
	e = errors.Join(e, store.Close())
	if e != nil {
		return report, e
	}
	report.Verified = true
	if e = writeJSON(filepath.Join(out, "verification.json"), verification); e != nil {
		return report, e
	}
	if e = writeJSON(filepath.Join(out, "summary.json"), report); e != nil {
		return report, e
	}
	if _, e = db.ExecContext(ctx, "UPDATE metadata SET value='true' WHERE key='collection_complete'"); e != nil {
		return report, e
	}
	return report, nil
}
func writeJSON(path string, v any) (err error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return fmt.Errorf("create report: %w", e)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if e = json.NewEncoder(f).Encode(v); e != nil {
		return fmt.Errorf("write report: %w", e)
	}
	return nil
}

type archive struct {
	Path, Snapshot, DiskPath, SHA, Compression string
	Size                                       int64
	Changed                                    bool
}

func snapshot(ctx context.Context, src, out, rel string, compress bool) (a archive, err error) {
	before, e := os.Stat(src)
	if e != nil {
		return a, fmt.Errorf("stat source: %w", e)
	}
	dest := filepath.Join("source-snapshots", filepath.FromSlash(rel))
	if compress {
		dest += ".gz"
	}
	a = archive{Path: rel, Snapshot: filepath.ToSlash(dest), DiskPath: filepath.Join(out, dest)}
	if compress {
		a.Compression = "gzip"
	} else if strings.HasSuffix(rel, ".zst") {
		a.Compression = "zstd"
	}
	if e = os.MkdirAll(filepath.Dir(a.DiskPath), 0700); e != nil {
		return a, e
	}
	r, e := os.Open(src)
	if e != nil {
		return a, e
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	f, e := os.OpenFile(a.DiskPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return a, e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	var w io.Writer = f
	var z *gzip.Writer
	if compress {
		z = gzip.NewWriter(f)
		w = z
	}
	h := sha256.New()
	a.Size, e = io.Copy(io.MultiWriter(w, h), &contextReader{ctx: ctx, r: io.LimitReader(r, before.Size())})
	if z != nil {
		e = errors.Join(e, z.Close())
	}
	if e != nil {
		return a, fmt.Errorf("copy source snapshot: %w", e)
	}
	a.SHA = hex.EncodeToString(h.Sum(nil))
	after, e := os.Stat(src)
	if e != nil {
		return a, fmt.Errorf("source disappeared during copy: %w", e)
	}
	a.Changed = a.Size != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime())
	return a, nil
}
func saveArchive(ctx context.Context, tx *sql.Tx, a archive) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO source_files VALUES(?,?,?,?,?,?)", a.Path, a.SHA, a.Size, a.Snapshot, a.Changed, a.Compression)
	if e != nil {
		return fmt.Errorf("register source snapshot: %w", e)
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}
func scanArchive(ctx context.Context, a archive, fn func(int, []byte) error) (err error) {
	f, e := os.Open(a.DiskPath)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	var r io.Reader = f
	if a.Compression == "gzip" {
		z, e := gzip.NewReader(f)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, z.Close()) }()
		r = z
	} else if a.Compression == "zstd" {
		z, e := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(256<<20))
		if e != nil {
			return e
		}
		defer z.Close()
		r = z
	}
	scanner := bufio.NewScanner(&contextReader{ctx, r})
	scanner.Buffer(make([]byte, 64<<10), maxLine)
	i := 0
	for scanner.Scan() {
		i++
		if e = fn(i, scanner.Bytes()); e != nil {
			return e
		}
	}
	if e = scanner.Err(); e != nil {
		return fmt.Errorf("read transcript (maximum record 64 MiB): %w", e)
	}
	return nil
}
func issue(ctx context.Context, tx *sql.Tx, file string, line int, kind string, r *CollectReport) error {
	r.Issues++
	_, e := tx.ExecContext(ctx, "INSERT INTO issues VALUES(?,?,?)", file, line, kind)
	return e
}
func saveEvent(ctx context.Context, tx *sql.Tx, e Event, loc map[string]any) error {
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO events VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", e.Source, e.SessionID, e.EventID, e.EventTime, e.EventType, e.Subtype, e.TurnID, e.Activity, e.Model, e.Role, e.Phase, e.RecordSHA, e.RecordJSON, "")
	if err != nil {
		return fmt.Errorf("store event: %w", err)
	}
	b, err := json.Marshal(loc)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO provenance VALUES(?,?)", e.EventID, string(b)); err != nil {
		return fmt.Errorf("store event provenance: %w", err)
	}
	return nil
}
func saveSession(ctx context.Context, tx *sql.Tx, id, parent, kind string) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO sessions VALUES(?,?,?) ON CONFLICT(session_id) DO UPDATE SET parent_session_id=excluded.parent_session_id,thread_kind=excluded.thread_kind`, id, parent, kind)
	return e
}
