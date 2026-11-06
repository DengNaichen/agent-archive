package history

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"io"
	"os"
	"path/filepath"
)

type Verification struct {
	Success bool           `json:"success"`
	Source  string         `json:"source"`
	Files   int            `json:"verified_source_files"`
	Events  int            `json:"verified_events"`
	Changed int            `json:"changed_files"`
	Issues  map[string]int `json:"issues"`
}

func (s *Store) Verify(ctx context.Context, progress Progress) (Verification, error) {
	return s.verify(ctx, progress, false)
}
func (s *Store) verify(ctx context.Context, progress Progress, collecting bool) (v Verification, err error) {
	if !collecting {
		if e := s.EnsureComplete(ctx); e != nil {
			return v, e
		}
	}
	v.Source = s.source
	v.Issues = map[string]int{}
	var integrity string
	if e := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); e != nil {
		return v, fmt.Errorf("check sqlite: %w", e)
	}
	if integrity != "ok" {
		return v, fault.New("sqlite integrity failed")
	}
	var violations int
	if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); e != nil {
		return v, e
	}
	if violations != 0 {
		return v, fault.New("foreign key integrity failed")
	}
	query := "SELECT snapshot,sha256,bytes FROM source_files"
	var versions int
	if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='archive_versions'").Scan(&versions); e != nil {
		return v, e
	}
	if versions > 0 {
		query = "SELECT snapshot,sha256,bytes FROM archive_versions"
	}
	if s.legacyCodex {
		query = "SELECT archive_path,sha256,bytes FROM source_archives"
		var n int
		if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM source_files WHERE decompression_exit!=0").Scan(&n); e != nil {
			return v, e
		}
		if n != 0 {
			return v, fault.New("source decompression failed")
		}
	}
	rows, e := s.db.QueryContext(ctx, query)
	if e != nil {
		return v, fmt.Errorf("read source manifest: %w", e)
	}
	type item struct {
		path, sha string
		size      int64
	}
	var archives []item
	for rows.Next() {
		var a item
		if e = rows.Scan(&a.path, &a.sha, &a.size); e != nil {
			break
		}
		archives = append(archives, a)
	}
	e = errors.Join(e, rows.Err(), rows.Close())
	if e != nil {
		return v, e
	}
	var count int
	if e = s.db.QueryRowContext(ctx, func() string {
		if versions > 0 {
			return "SELECT count(*) FROM archive_versions"
		}
		return "SELECT count(*) FROM source_files"
	}()).Scan(&count); e != nil {
		return v, e
	}
	if len(archives) != count {
		return v, fault.New("source archive coverage mismatch")
	}
	var hasCompression int
	if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('source_files') WHERE name='compression'").Scan(&hasCompression); e != nil {
		return v, e
	}
	for i, a := range archives {
		file, e := resolvedPath(filepath.Join(filepath.Dir(s.path), a.path))
		if e != nil {
			return v, e
		}
		root, e := resolvedPath(filepath.Dir(s.path))
		if e != nil {
			return v, e
		}
		if !within(root, file) {
			return v, fault.New("archive escapes snapshot directory")
		}
		compressed := s.source == "claude_code"
		if hasCompression > 0 {
			var kind string
			if e = s.db.QueryRowContext(ctx, func() string {
				if versions > 0 {
					return "SELECT compression FROM archive_versions WHERE snapshot=?"
				}
				return "SELECT compression FROM source_files WHERE snapshot=?"
			}(), a.path).Scan(&kind); e != nil {
				return v, e
			}
			compressed = kind == "gzip"
		}
		if e = verifyArchive(ctx, file, a.sha, a.size, compressed); e != nil {
			return v, e
		}
		v.Files++
		if progress != nil && (i%250 == 249 || i+1 == len(archives)) {
			if e = progress(map[string]any{"event": "verify_sources", "files": v.Files, "total_files": len(archives)}); e != nil {
				return v, e
			}
		}
	}
	q := "SELECT event_id,session_id,record_json,record_sha256 FROM events"
	if s.legacyCodex {
		q = "SELECT e.event_id,e.session_id,d.record_json,'' FROM events e JOIN event_documents d USING(event_id)"
	}
	records, e := s.db.QueryContext(ctx, q)
	if e != nil {
		return v, e
	}
	for records.Next() {
		var id, sid, raw, sha string
		if e = records.Scan(&id, &sid, &raw, &sha); e != nil {
			break
		}
		obj, er := Decode([]byte(raw))
		if er != nil {
			e = er
			break
		}
		if !s.legacyCodex && SHA([]byte(raw)) != sha {
			e = fault.New("event content checksum mismatch")
			break
		}
		var expected string
		if s.source == "claude_code" {
			expected = SHA([]byte("claude_code\x00" + sid + "\x00" + SHA([]byte(raw))))
		} else {
			ph, er := Fingerprint(obj["payload"])
			if er != nil {
				e = er
				break
			}
			expected, er = Fingerprint([]any{sid, Str(obj, "type"), obj["timestamp"], ph})
			if er != nil {
				e = er
				break
			}
		}
		if id != expected {
			e = fault.New("event identity does not match content")
			break
		}
		v.Events++
		if progress != nil && v.Events%50000 == 0 {
			if e = progress(map[string]any{"event": "verify_content", "events": v.Events}); e != nil {
				break
			}
		}
	}
	e = errors.Join(e, records.Err(), records.Close())
	if e != nil {
		return v, fmt.Errorf("verify event content: %w", e)
	}
	if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count); e != nil {
		return v, e
	}
	if count != v.Events {
		return v, fault.New("event document coverage mismatch")
	}
	if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM events e WHERE NOT EXISTS(SELECT 1 FROM provenance p WHERE p.event_id=e.event_id)").Scan(&count); e != nil {
		return v, e
	}
	if count > 0 {
		return v, fault.New("events lack provenance")
	}
	if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM provenance p WHERE NOT EXISTS(SELECT 1 FROM events e WHERE e.event_id=p.event_id)").Scan(&count); e != nil {
		return v, e
	}
	if count > 0 {
		return v, fault.New("orphan provenance records")
	}
	issues, e := s.db.QueryContext(ctx, "SELECT kind,count(*) FROM issues GROUP BY kind")
	if e != nil {
		return v, e
	}
	for issues.Next() {
		var kind string
		var n int
		if e = issues.Scan(&kind, &n); e != nil {
			break
		}
		v.Issues[kind] = n
	}
	e = errors.Join(e, issues.Err(), issues.Close())
	if e != nil {
		return v, e
	}
	changed := "changed_during_copy"
	if s.legacyCodex {
		changed = "changed"
	}
	if e = s.db.QueryRowContext(ctx, "SELECT coalesce(sum("+changed+"),0) FROM source_files").Scan(&v.Changed); e != nil {
		return v, e
	}
	v.Success = true
	return v, nil
}
func verifyArchive(ctx context.Context, path, sha string, size int64, compressed bool) (err error) {
	f, e := os.Open(path)
	if e != nil {
		return fmt.Errorf("open source snapshot: %w", e)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	var r io.Reader = f
	if compressed {
		z, e := gzip.NewReader(f)
		if e != nil {
			return fmt.Errorf("open gzip snapshot: %w", e)
		}
		defer func() { err = errors.Join(err, z.Close()) }()
		r = z
	}
	h := sha256.New()
	n, e := io.Copy(h, &contextReader{ctx, r})
	if e != nil {
		return fmt.Errorf("read source snapshot: %w", e)
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sha {
		return fault.New("source snapshot checksum/size mismatch")
	}
	return nil
}
