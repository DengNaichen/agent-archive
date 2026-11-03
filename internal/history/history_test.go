package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/klauspost/compress/zstd"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func openTest(t *testing.T, path string) *Store {
	t.Helper()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	return s
}
func collectTest(t *testing.T, source, home, out string) CollectReport {
	t.Helper()
	v, e := Collect(context.Background(), CollectOptions{Source: source, Home: home, Output: out})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestCodexLosslessCompressedAndDedup(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	out := filepath.Join(dir, "snapshot")
	raw := `{"timestamp":"2026-11-01T00:00:00Z","type":"session_meta","payload":{"id":"s","thread_source":"user"}}
{"timestamp":"2026-11-01T00:00:01Z","type":"turn_context","payload":{"turn_id":"t","model":"test"}}
{"timestamp":"2026-11-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"中文\n` + strings.Repeat("x", 70000) + `"}]}}
`
	path := filepath.Join(home, "sessions", "rollout.jsonl")
	put(t, path, raw)
	put(t, filepath.Join(home, "auth.json"), "private fixture should not be collected")
	encoder, e := zstd.NewWriter(nil)
	if e != nil {
		t.Fatal(e)
	}
	packed := encoder.EncodeAll([]byte(raw), nil)
	if e = encoder.Close(); e != nil {
		t.Fatal(e)
	}
	compressed := filepath.Join(home, "archived_sessions", "copy.jsonl.zst")
	put(t, compressed, string(packed))
	v := collectTest(t, "codex", home, out)
	if !v.Verified || v.Occurrences != 6 {
		t.Fatalf("report %+v", v)
	}
	s := openTest(t, filepath.Join(out, "cleaned.sqlite"))
	stats, e := s.Status(context.Background())
	if e != nil || stats.Events != 3 || stats.Turns != 1 {
		t.Fatalf("stats %+v %v", stats, e)
	}
	n := 0
	e = s.Walk(context.Background(), "s", func(ev Event) error {
		n++
		var loc []any
		if e := json.Unmarshal([]byte(ev.Locations), &loc); e != nil {
			return e
		}
		if len(loc) != 2 {
			t.Errorf("source locations %d", len(loc))
		}
		if ev.TurnID == "t" && ev.Model != "test" {
			t.Errorf("turn model %q", ev.Model)
		}
		return nil
	})
	if e != nil || n != 3 {
		t.Fatal(n, e)
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != raw {
		t.Fatal("source changed", e)
	}
	if _, e = os.Stat(filepath.Join(out, "source-snapshots", "auth.json")); !os.IsNotExist(e) {
		t.Fatal("auth copied")
	}
	if _, e = os.Stat(filepath.Join(out, "verification.json")); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(out, "source-snapshots", "sessions", "rollout.jsonl"), []byte("bad"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Verify(context.Background(), nil); e == nil {
		t.Fatal("corrupt source accepted")
	}
}
func TestClaudeBlocksBranchesAndUnknownMetadata(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	out := filepath.Join(dir, "snapshot")
	raw := `{"type":"user","uuid":"u","sessionId":"s","timestamp":"2026-11-01T00:00:00Z","message":{"role":"user","content":"保留原文"}}
{"type":"assistant","uuid":"a","parentUuid":"u","sessionId":"s","timestamp":"2026-11-01T00:00:01Z","message":{"role":"assistant","model":"test","content":[{"type":"thinking","thinking":"fixture"},{"type":"tool_use","id":"call","name":"Edit","input":{"old":"a","new":"b"}},{"type":"image","source":{"data":"fixture"}}],"usage":{"input_tokens":99}},"future_unknown":{"keep":true}}
{"type":"user","uuid":"r","parentUuid":"a","sessionId":"s","timestamp":"2026-11-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"完整输出"}]}}
{"type":"ai-title","sessionId":"s","aiTitle":"fixture"}
{invalid
`
	put(t, filepath.Join(home, "projects", "p", "s.jsonl"), raw)
	put(t, filepath.Join(home, "projects", "p", "s", "subagents", "agent-x.jsonl"), `{"type":"user","uuid":"child-u","sessionId":"s","timestamp":"2026-11-01T00:00:00Z","message":{"role":"user","content":"child"}}`)
	put(t, filepath.Join(home, "history.jsonl"), "index\n")
	v := collectTest(t, "claude-code", home, out)
	if v.Issues != 1 || v.Occurrences != 5 {
		t.Fatalf("report %+v", v)
	}
	s := openTest(t, filepath.Join(out, "cleaned.sqlite"))
	stats, e := s.Status(context.Background())
	if e != nil || stats.Sessions != 2 || stats.Events != 5 || stats.Turns != 2 {
		t.Fatalf("stats %+v %v", stats, e)
	}
	e = s.Walk(context.Background(), "s", func(ev Event) error {
		o, e := Decode([]byte(ev.RecordJSON))
		if e != nil {
			return e
		}
		if Str(o, "uuid") != "" && ev.TurnID != "u" {
			t.Errorf("wrong inferred turn %q", ev.TurnID)
		}
		if ev.EventType == "assistant" && !strings.Contains(ev.RecordJSON, "future_unknown") {
			t.Error("unknown field lost")
		}
		if ev.EventType == "ai-title" && !strings.Contains(ev.Locations, "file_first_timestamp") {
			t.Error("inferred timestamp unmarked")
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	child := 0
	if e = s.Walk(context.Background(), "s:agent-x", func(ev Event) error {
		child++
		if !strings.Contains(ev.Locations, `"parent_session_id":"s"`) {
			t.Error("parent relation lost")
		}
		return nil
	}); e != nil || child != 1 {
		t.Fatal(child, e)
	}
	if _, e = s.Verify(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
}
func TestCollectionRefusesUnsafeOutputsAndIncompleteInput(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	put(t, filepath.Join(home, "projects/p/s.jsonl"), `{"type":"ai-title","sessionId":"s"}`)
	existing := filepath.Join(dir, "exists")
	if e := os.Mkdir(existing, 0700); e != nil {
		t.Fatal(e)
	}
	for _, out := range []string{existing, filepath.Join(home, "bad")} {
		if _, e := Collect(context.Background(), CollectOptions{Source: "claude-code", Home: home, Output: out}); e == nil {
			t.Fatal("unsafe output accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := Collect(ctx, CollectOptions{Source: "claude-code", Home: home, Output: filepath.Join(dir, "cancelled")}); e == nil {
		t.Fatal("cancelled collection accepted")
	}
}
func TestCanonicalMatchesPython(t *testing.T) {
	cases := map[string]string{`{"b":1e3,"a":"<>&\u2028中文","zero":-0,"small":0.00001,"large":1000000000000000.0}`: "{\"a\":\"<>&\u2028中文\",\"b\":1000.0,\"large\":1000000000000000.0,\"small\":1e-05,\"zero\":0}", `{"n":12345678901234567890123456789,"x":1e16}`: `{"n":12345678901234567890123456789,"x":1e+16}`}
	for input, want := range cases {
		o, e := Decode([]byte(input))
		if e != nil {
			t.Fatal(e)
		}
		got, e := Canonical(o)
		if e != nil || got != want {
			t.Errorf("got %s want %s: %v", got, want, e)
		}
	}
	for _, s := range []string{`{"x":1} garbage`, `null`, `[]`} {
		if _, e := Decode([]byte(s)); e == nil {
			t.Error("invalid envelope accepted")
		}
	}
}

func TestLegacyCodexSnapshotReadAndVerify(t *testing.T) {
	dir := t.TempDir()
	raw := `{"timestamp":"2026-11-01T00:00:00Z","type":"session_meta","payload":{"id":"legacy","thread_source":"user"}}`
	obj, e := Decode([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	ph, e := Fingerprint(obj["payload"])
	if e != nil {
		t.Fatal(e)
	}
	id, e := Fingerprint([]any{"legacy", "session_meta", obj["timestamp"], ph})
	if e != nil {
		t.Fatal(e)
	}
	source := raw + "\n"
	put(t, filepath.Join(dir, "source-snapshots/sessions/a.jsonl"), source)
	db, e := sql.Open("sqlite", filepath.Join(dir, "cleaned.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`CREATE TABLE sessions(session_id TEXT);INSERT INTO sessions VALUES('legacy');CREATE TABLE turns(turn_id TEXT);CREATE TABLE events(event_id TEXT,session_id TEXT,timestamp TEXT,event_type TEXT,subtype TEXT,turn_id TEXT,activity_class TEXT,model TEXT);CREATE VIEW classified_events AS SELECT * FROM events;CREATE TABLE event_documents(event_id TEXT,record_json TEXT);CREATE TABLE provenance(file_id TEXT,event_id TEXT,line_no INTEGER,ordinal INTEGER);CREATE TABLE source_files(file_id TEXT,relative_path TEXT,decompression_exit INTEGER,changed INTEGER);CREATE TABLE source_archives(file_id TEXT,archive_path TEXT,sha256 TEXT,bytes INTEGER);CREATE TABLE issues(kind TEXT);`)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO events VALUES(?,?,?,?,?,?,?,?)", id, "legacy", "2026-11-01T00:00:00Z", "session_meta", "", nil, "main_session", nil); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO event_documents VALUES(?,?)", id, raw); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO provenance VALUES('f',?,1,NULL)", id); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO source_files VALUES('f','sessions/a.jsonl',0,0)"); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO source_archives VALUES('f','source-snapshots/sessions/a.jsonl',?,?)", SHA([]byte(source)), len(source)); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	s := openTest(t, filepath.Join(dir, "cleaned.sqlite"))
	n := 0
	if e = s.Walk(context.Background(), "legacy", func(ev Event) error {
		n++
		if ev.EventID != id || ev.RecordJSON != raw || !strings.Contains(ev.Locations, "sessions/a.jsonl") {
			t.Error("legacy fields differ")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if n != 1 {
		t.Fatal(n)
	}
	if _, e = s.Verify(context.Background(), nil); e != nil {
		t.Fatal(e)
	}
}

func TestInterruptedCollectionAndFutureSchemaAreRejected(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	out := filepath.Join(dir, "out")
	put(t, filepath.Join(home, "sessions/broken.jsonl.zst"), "not a zstd stream")
	if _, e := Collect(context.Background(), CollectOptions{Source: "codex", Home: home, Output: out}); e == nil {
		t.Fatal("broken compression accepted")
	}
	s := openTest(t, filepath.Join(out, "cleaned.sqlite"))
	if e := s.EnsureComplete(context.Background()); e == nil {
		t.Fatal("incomplete marker accepted")
	}
	if _, e := s.Verify(context.Background(), nil); e == nil {
		t.Fatal("partial snapshot verified")
	}
	db, e := sql.Open("sqlite", filepath.Join(out, "cleaned.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("UPDATE metadata SET value='999' WHERE key='schema_version'"); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	if future, e := Open(filepath.Join(out, "cleaned.sqlite")); e == nil {
		if e = future.Close(); e != nil {
			t.Fatal(e)
		}
		t.Fatal("future schema silently accepted")
	}
}
