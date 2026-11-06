package history

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DengNaichen/agent-archive/internal/runstate"
	"github.com/klauspost/compress/zstd"
)

func TestIncrementalKeepsEvidenceAndResumes(t *testing.T) {
	for _, source := range []string{"codex", "claude-code"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			out := filepath.Join(dir, "out")
			path := filepath.Join(home, "sessions", "s.jsonl")
			original := "{\"type\":\"session_meta\",\"timestamp\":\"2026-11-01T00:00:00Z\",\"payload\":{\"id\":\"s\"}}\n"
			extra := "{\"type\":\"response_item\",\"timestamp\":\"2026-11-01T00:00:01Z\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":\"new\"}}\n"
			if source == "claude-code" {
				path = filepath.Join(home, "projects", "p", "s.jsonl")
				original = "{\"type\":\"user\",\"uuid\":\"u\",\"sessionId\":\"s\",\"message\":{\"content\":\"old\"}}\n"
				extra = "{\"type\":\"assistant\",\"uuid\":\"a\",\"parentUuid\":\"u\",\"sessionId\":\"s\",\"message\":{\"content\":\"new\"}}\n"
			}
			put(t, path, original)
			o := CollectOptions{Source: source, Home: home, Output: out, Incremental: true}
			if _, e := Collect(context.Background(), o); e != nil {
				t.Fatal(e)
			}
			report, e := Collect(context.Background(), o)
			if e != nil || report.Files != 0 || report.Skipped != 1 {
				t.Fatalf("no-op %+v %v", report, e)
			}
			put(t, path, original+extra)
			// Commit the changed file, then interrupt before the completion marker.
			cancelled := errors.New("simulated process interruption")
			o.Progress = func(any) error { return cancelled }
			if _, e = Collect(context.Background(), o); !errors.Is(e, cancelled) {
				t.Fatal(e)
			}
			s := openTest(t, filepath.Join(out, "cleaned.sqlite"))
			if e = s.EnsureComplete(context.Background()); e == nil {
				t.Fatal("interrupted snapshot accepted")
			}
			o.Progress = nil
			if _, e = Collect(context.Background(), o); e != nil {
				t.Fatal("resume", e)
			}
			if _, e = s.Verify(context.Background(), nil); e != nil {
				t.Fatal(e)
			}
			var n int
			if e = s.db.QueryRow("SELECT count(*) FROM events").Scan(&n); e != nil || n != 2 {
				t.Fatal(n, e)
			}
			// Rewriting/truncating a file keeps the earlier event and its exact source version.
			put(t, path, original)
			if _, e = Collect(context.Background(), o); e != nil {
				t.Fatal(e)
			}
			if e = s.db.QueryRow("SELECT count(*) FROM events").Scan(&n); e != nil || n != 2 {
				t.Fatal(n, e)
			}
			var old string
			if e = s.db.QueryRow("SELECT snapshot FROM archive_versions WHERE sha256=?", SHA([]byte(original+extra))).Scan(&old); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(out, old), []byte("corrupt retained version"), 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = s.Verify(context.Background(), nil); e == nil {
				t.Fatal("old-version corruption accepted")
			}
		})
	}
}
func TestIncrementalCompressedAndRemovalAndLock(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	out := filepath.Join(dir, "out")
	raw := `{"type":"session_meta","timestamp":"2026-11-01T00:00:00Z","payload":{"id":"s"}}` + "\n"
	z, e := zstd.NewWriter(nil)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := z.Close(); e != nil {
			t.Error(e)
		}
	}()
	path := filepath.Join(home, "sessions", "s.jsonl.zst")
	put(t, path, string(z.EncodeAll([]byte(raw), nil)))
	o := CollectOptions{Source: "codex", Home: home, Output: out, Incremental: true}
	if _, e = Collect(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	put(t, path, string(z.EncodeAll([]byte(raw+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}`+"\n"), nil)))
	if _, e = Collect(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	put(t, filepath.Join(home, "sessions", "other.jsonl"), raw)
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if _, e = Collect(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	s := openTest(t, filepath.Join(out, "cleaned.sqlite"))
	v, e := s.Verify(context.Background(), nil)
	if e != nil || v.Events != 2 {
		t.Fatal(v, e)
	}
	lock, e := runstate.Lock(context.Background(), out+".lock.sqlite")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Collect(context.Background(), o); e == nil {
		t.Fatal("concurrent collector admitted")
	}
	if e = lock.Close(); e != nil {
		t.Fatal(e)
	}
	other := filepath.Join(dir, "another")
	put(t, filepath.Join(other, "sessions", "x.jsonl"), raw)
	o.Home = other
	if _, e = Collect(context.Background(), o); e == nil {
		t.Fatal("home binding ignored")
	}
}

func TestIncrementalDoesNotTrustSizeOrMtime(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	out := filepath.Join(dir, "out")
	p := filepath.Join(home, "projects", "p", "s.jsonl")
	raw := `{"type":"user","sessionId":"s","message":{"content":"old"}}` + "\n"
	put(t, p, raw)
	o := CollectOptions{Source: "claude-code", Home: home, Output: out, Incremental: true}
	if _, e := Collect(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(p)
	if e != nil {
		t.Fatal(e)
	}
	put(t, p, strings.Replace(raw, "old", "new", 1))
	if e = os.Chtimes(p, st.ModTime(), st.ModTime()); e != nil {
		t.Fatal(e)
	}
	r, e := Collect(context.Background(), o)
	if e != nil || r.Files != 1 || r.Skipped != 0 {
		t.Fatal(r, e)
	}
	s := openTest(t, filepath.Join(out, "cleaned.sqlite"))
	v, e := s.Verify(context.Background(), nil)
	if e != nil || v.Events != 2 {
		t.Fatal(v, e)
	}
}
