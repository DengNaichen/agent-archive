package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/DengNaichen/agent-archive/internal/fault"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const schema = `
CREATE TABLE metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE sessions(session_id TEXT PRIMARY KEY,parent_session_id TEXT NOT NULL,thread_kind TEXT NOT NULL);
CREATE TABLE turns(session_id TEXT,turn_id TEXT,model TEXT,activity_class TEXT,PRIMARY KEY(session_id,turn_id));
CREATE TABLE events(source TEXT,session_id TEXT,event_id TEXT PRIMARY KEY,event_time TEXT,event_type TEXT,subtype TEXT,turn_id TEXT,activity_class TEXT,model TEXT,role TEXT,phase TEXT,record_sha256 TEXT,record_json TEXT,source_locations_json TEXT);
CREATE INDEX events_session ON events(session_id);
CREATE TABLE provenance(event_id TEXT,location_json TEXT NOT NULL);
CREATE INDEX provenance_event ON provenance(event_id);
CREATE TABLE source_files(path TEXT PRIMARY KEY,sha256 TEXT,bytes INTEGER,snapshot TEXT,changed_during_copy INTEGER,compression TEXT);
CREATE TABLE issues(file TEXT,line INTEGER,kind TEXT);
CREATE VIEW event_documents AS SELECT event_id,record_json FROM events;
CREATE VIEW cloud_events AS SELECT e.source,e.session_id,e.event_id,e.event_time,e.event_type,e.subtype,e.turn_id,
CASE WHEN e.source='codex' THEN coalesce(nullif(t.activity_class,''),e.activity_class) ELSE e.activity_class END activity_class,CASE WHEN e.source='codex' THEN coalesce(nullif(t.model,''),e.model) ELSE e.model END model,e.role,e.phase,e.record_sha256,e.record_json,
(SELECT json_group_array(json(p.location_json)) FROM provenance p WHERE p.event_id=e.event_id) source_locations_json
FROM events e LEFT JOIN turns t ON e.session_id=t.session_id AND e.turn_id=t.turn_id;
`

type Store struct {
	db           *sql.DB
	path, source string
	legacyCodex  bool
}

func (s *Store) Source() string { return s.source }

func Open(path string) (_ *Store, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot: %w", err)
	}
	if _, err = os.Stat(path); err != nil {
		return nil, fmt.Errorf("snapshot not found: %w", err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, source: "codex"}
	var exists int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='metadata'").Scan(&exists); err != nil {
		return nil, fmt.Errorf("read snapshot schema: %w", err)
	}
	if exists > 0 {
		if err = db.QueryRow("SELECT value FROM metadata WHERE key='source'").Scan(&s.source); err != nil {
			return nil, fmt.Errorf("read source: %w", err)
		}
	}
	if s.source != "codex" && s.source != "claude_code" {
		return nil, fault.New("unsupported source")
	}
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='cloud_events'").Scan(&exists); err != nil {
		return nil, fmt.Errorf("detect snapshot format: %w", err)
	}
	var hasVersion int
	if e := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='metadata'").Scan(&hasVersion); e != nil {
		return nil, e
	}
	if hasVersion > 0 {
		var version string
		e := db.QueryRow("SELECT value FROM metadata WHERE key='schema_version'").Scan(&version)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if e == nil && version != "1" {
			return nil, fault.New("unsupported snapshot schema version")
		}
	}
	s.legacyCodex = exists == 0
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Sessions(ctx context.Context, ids []string, all bool) (_ []string, err error) {
	if all == (len(ids) > 0) {
		return nil, fault.New("choose --all or one or more --session flags")
	}
	if all {
		rows, e := s.db.QueryContext(ctx, "SELECT session_id FROM sessions ORDER BY session_id")
		if e != nil {
			return nil, fmt.Errorf("list sessions: %w", e)
		}
		defer func() { err = errors.Join(err, rows.Close()) }()
		ids = nil
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				return nil, fmt.Errorf("read session: %w", e)
			}
			ids = append(ids, id)
		}
		if e = rows.Err(); e != nil {
			return nil, fmt.Errorf("list sessions: %w", e)
		}
		return ids, nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		var n int
		if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE session_id=?", id).Scan(&n); e != nil {
			return nil, fmt.Errorf("find session: %w", e)
		}
		if n == 0 {
			return nil, fault.New("selected session is absent")
		}
		out = append(out, id)
		seen[id] = true
	}
	return out, nil
}
func (s *Store) Walk(ctx context.Context, sid string, fn func(Event) error) (err error) {
	if s.legacyCodex {
		return s.walkLegacy(ctx, sid, fn)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+strings.Join(Columns, ",")+" FROM cloud_events WHERE session_id=? ORDER BY event_time,event_id", sid)
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Source, &e.SessionID, &e.EventID, &e.EventTime, &e.EventType, &e.Subtype, &e.TurnID, &e.Activity, &e.Model, &e.Role, &e.Phase, &e.RecordSHA, &e.RecordJSON, &e.Locations); err != nil {
			return fmt.Errorf("decode stored event: %w", err)
		}
		if err = fn(e); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	return nil
}

// Legacy SQLite snapshots remain usable without an export/re-import step.
func (s *Store) walkLegacy(ctx context.Context, sid string, fn func(Event) error) (err error) {
	// Read all locations for one session in a single scan. Legacy provenance
	// has no event_id index; per-event queries would scan the full database.
	locations := map[string]string{}
	locationRows, err := s.db.QueryContext(ctx, `SELECT p.event_id,json_group_array(json_object('file',f.relative_path,'line',p.line_no,'ordinal',p.ordinal,'source_file_sha256',a.sha256)) FROM provenance p JOIN events e USING(event_id) JOIN source_files f USING(file_id) JOIN source_archives a USING(file_id) WHERE e.session_id=? GROUP BY p.event_id`, sid)
	if err != nil {
		return fmt.Errorf("read legacy provenance: %w", err)
	}
	for locationRows.Next() {
		var id, location string
		if err = locationRows.Scan(&id, &location); err != nil {
			break
		}
		locations[id] = location
	}
	err = errors.Join(err, locationRows.Err(), locationRows.Close())
	if err != nil {
		return fmt.Errorf("read legacy provenance: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.session_id,e.event_id,coalesce(e.timestamp,''),e.event_type,coalesce(e.subtype,''),coalesce(e.turn_id,''),e.activity_class,coalesce(e.model,''),d.record_json FROM classified_events e JOIN event_documents d USING(event_id) WHERE e.session_id=? ORDER BY e.timestamp,e.event_id`, sid)
	if err != nil {
		return fmt.Errorf("read legacy events: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var e Event
		e.Source = "codex"
		if err = rows.Scan(&e.SessionID, &e.EventID, &e.EventTime, &e.EventType, &e.Subtype, &e.TurnID, &e.Activity, &e.Model, &e.RecordJSON); err != nil {
			return fmt.Errorf("decode legacy event: %w", err)
		}
		e.EventTime, _ = eventTime(e.EventTime)
		o, er := Decode([]byte(e.RecordJSON))
		if er != nil {
			return er
		}
		p := Obj(o["payload"])
		e.Role = Str(p, "role")
		e.Phase = Str(p, "phase")
		e.RecordSHA = SHA([]byte(e.RecordJSON))
		e.Locations = locations[e.EventID]
		if er = fn(e); er != nil {
			return er
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("read legacy events: %w", err)
	}
	return nil
}

type Stats struct {
	Source        string `json:"source"`
	Sessions      int    `json:"sessions"`
	Events        int    `json:"events"`
	Turns         int    `json:"turns"`
	SnapshotBytes int64  `json:"snapshot_bytes"`
}

func (s *Store) Status(ctx context.Context) (Stats, error) {
	v := Stats{Source: s.source}
	for table, dest := range map[string]*int{"sessions": &v.Sessions, "events": &v.Events, "turns": &v.Turns} {
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(dest); err != nil {
			return v, fmt.Errorf("count %s: %w", table, err)
		}
	}
	st, err := os.Stat(s.path)
	if err != nil {
		return v, fmt.Errorf("stat snapshot: %w", err)
	}
	v.SnapshotBytes = st.Size()
	return v, nil
}

// EnsureComplete rejects interrupted Go collections before any cloud access.
// Prototype snapshots predate this marker and remain supported read-only.
func (s *Store) EnsureComplete(ctx context.Context) error {
	var present int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='metadata'").Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	var complete string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='collection_complete'").Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read completion marker: %w", err)
	}
	if complete != "true" {
		return fault.New("snapshot collection is incomplete; collect into a new directory")
	}
	return nil
}

type Plan struct {
	Source   string `json:"source"`
	Sessions int    `json:"sessions"`
	Events   int    `json:"events"`
	Bytes    int64  `json:"raw_event_json_bytes"`
	Note     string `json:"note"`
}

func (s *Store) Plan(ctx context.Context, ids []string) (Plan, error) {
	result := Plan{Source: s.source, Sessions: len(ids), Note: "excludes upload wrapper, provenance, compression and cloud storage overhead"}
	for _, sid := range ids {
		var count int
		var size int64
		if err := s.db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(length(cast(d.record_json AS blob))),0) FROM event_documents d JOIN events e USING(event_id) WHERE e.session_id=?`, sid).Scan(&count, &size); err != nil {
			return result, fmt.Errorf("calculate upload plan: %w", err)
		}
		result.Events += count
		result.Bytes += size
	}
	return result, nil
}
