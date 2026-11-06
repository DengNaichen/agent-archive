package job

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DengNaichen/agent-archive/internal/cloud"
	"github.com/DengNaichen/agent-archive/internal/history"
)

type server struct {
	events     map[string]history.Event
	writes     int
	loseClaude bool
}

func (f *server) Rows(ctx context.Context, sql string, out any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	var rows any
	if strings.HasPrefix(sql, "DESCRIBE") {
		cols := []map[string]string{}
		for _, n := range history.Columns {
			cols = append(cols, map[string]string{"name": n})
		}
		rows = cols
	} else {
		hashes := []map[string]string{}
		for _, ev := range f.events {
			if strings.Contains(sql, "source='"+ev.Source+"'") && strings.Contains(sql, "session_id='"+ev.SessionID+"'") {
				hashes = append(hashes, map[string]string{"event_id": ev.EventID, "record_sha256": ev.RecordSHA, "actual_sha256": history.SHA([]byte(ev.RecordJSON))})
			}
		}
		rows = hashes
	}
	b, e := json.Marshal(rows)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, out)
}
func (f *server) Request(ctx context.Context, sql string, b []byte, z bool) ([]byte, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	f.writes++
	source := ""
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev history.Event
		if e := json.Unmarshal([]byte(line), &ev); e != nil {
			return nil, e
		}
		f.events[ev.Source+ev.EventID] = ev
		source = ev.Source
	}
	if source == "claude_code" && f.loseClaude {
		f.loseClaude = false
		return nil, errors.New("lost response after commit")
	}
	return nil, nil
}
func put(t *testing.T, path, body string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestJobResumeThenNextIncrementalCycle(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	claude := filepath.Join(dir, "claude")
	cpath := filepath.Join(codex, "sessions", "s.jsonl")
	raw := `{"type":"session_meta","timestamp":"2026-11-01T00:00:00Z","payload":{"id":"s"}}` + "\n"
	put(t, cpath, raw)
	put(t, filepath.Join(claude, "projects", "p", "s.jsonl"), `{"type":"user","uuid":"u","sessionId":"s","message":{"content":"fixture"}}`+"\n")
	client, e := cloud.New(cloud.Config{Host: "example.test", Secure: true})
	if e != nil {
		t.Fatal(e)
	}
	o := Options{StateDir: filepath.Join(dir, "state"), CodexHome: codex, ClaudeHome: claude, Client: client, Sync: cloud.SyncOptions{BatchRows: 2, BatchBytes: 10000}}
	f := &server{events: map[string]history.Event{}, loseClaude: true}
	s, e := run(context.Background(), o, f)
	if e == nil || s.Status != "failed" || s.Stage != 3 {
		t.Fatalf("%+v %v", s, e)
	}
	saved, e := Read(o.StateDir)
	if e != nil || saved.Stage != 3 || saved.Status != "failed" {
		t.Fatal(saved, e)
	}
	put(t, cpath, raw+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}`+"\n")
	writes := f.writes
	s, e = run(context.Background(), o, f)
	if e != nil || s.Status != "complete" || f.writes != writes || s.Results["verify-codex"].Verified != 1 {
		t.Fatalf("resume %+v %v writes=%d", s, e, f.writes)
	}
	// Finishing a pending cycle must not silently collect a newer source halfway through.
	s, e = run(context.Background(), o, f)
	if e != nil || s.Status != "complete" || s.Results["verify-codex"].Verified != 2 || f.writes != writes+1 {
		t.Fatalf("next cycle %+v %v writes=%d", s, e, f.writes)
	}
	oldCycle := s.Cycle
	s, e = run(context.Background(), o, f)
	if e != nil || s.Cycle == oldCycle || f.writes != writes+1 {
		t.Fatal("no-op cycle", s, e)
	}
	o.ClaudeHome = codex
	if _, e = run(context.Background(), o, f); e == nil {
		t.Fatal("source-home binding ignored")
	}
}
