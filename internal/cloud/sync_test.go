package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DengNaichen/agent-archive/internal/history"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeTransport struct {
	events       map[string]history.Event
	writes       int
	loseResponse bool
	corrupt      bool
}

func (f *fakeTransport) Rows(ctx context.Context, sql string, out any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	var v any
	if strings.HasPrefix(sql, "DESCRIBE") {
		a := []map[string]string{}
		for _, name := range history.Columns {
			a = append(a, map[string]string{"name": name})
		}
		v = a
	} else {
		a := []remoteHash{}
		for _, ev := range f.events {
			sha := history.SHA([]byte(ev.RecordJSON))
			if f.corrupt {
				sha = "bad"
			}
			a = append(a, remoteHash{ev.EventID, ev.RecordSHA, sha})
		}
		v = a
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, out)
}
func (f *fakeTransport) Request(ctx context.Context, sql string, b []byte, gzip bool) ([]byte, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if !strings.HasPrefix(sql, "INSERT") || !gzip {
		return nil, fmt.Errorf("unexpected request")
	}
	f.writes++
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev history.Event
		if e := json.Unmarshal([]byte(line), &ev); e != nil {
			return nil, e
		}
		f.events[ev.EventID] = ev
	}
	if f.loseResponse {
		f.loseResponse = false
		return nil, fmt.Errorf("simulated lost response after server commit")
	}
	return nil, nil
}
func fixture(t *testing.T) *history.Store {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if e := os.MkdirAll(filepath.Join(home, "projects/p"), 0700); e != nil {
		t.Fatal(e)
	}
	var b strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "{\"type\":\"user\",\"uuid\":\"u%d\",\"sessionId\":\"s\",\"timestamp\":\"2026-11-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"fixture%d\"}}\n", i, i)
	}
	if e := os.WriteFile(filepath.Join(home, "projects/p/s.jsonl"), []byte(b.String()), 0600); e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(dir, "out")
	if _, e := history.Collect(context.Background(), history.CollectOptions{Source: "claude-code", Home: home, Output: out}); e != nil {
		t.Fatal(e)
	}
	s, e := history.Open(filepath.Join(out, "cleaned.sqlite"))
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
func TestSyncResumeAfterAmbiguousCommitAndNoDuplicateWrites(t *testing.T) {
	s := fixture(t)
	f := &fakeTransport{events: map[string]history.Event{}, loseResponse: true}
	o := SyncOptions{Upload: true, BatchRows: 2, BatchBytes: 10000}
	if _, e := Synchronize(context.Background(), s, f, []string{"s"}, o); e == nil {
		t.Fatal("failure not surfaced")
	}
	if len(f.events) != 2 {
		t.Fatal("expected committed batch")
	}
	r, e := Synchronize(context.Background(), s, f, []string{"s"}, o)
	if e != nil || r.Inserted != 3 || r.AlreadyPresent != 2 || r.Verified != 5 {
		t.Fatalf("%+v %v", r, e)
	}
	writes := f.writes
	r, e = Synchronize(context.Background(), s, f, []string{"s"}, o)
	if e != nil || r.Inserted != 0 || f.writes != writes {
		t.Fatalf("duplicate writes %+v %v", r, e)
	}
}
func TestSyncPreflightAndReadOnlyVerify(t *testing.T) {
	s := fixture(t)
	f := &fakeTransport{events: map[string]history.Event{}}
	o := SyncOptions{Upload: true, BatchRows: 2, BatchBytes: 1}
	if _, e := Synchronize(context.Background(), s, f, []string{"s"}, o); e == nil || f.writes != 0 {
		t.Fatal("oversized event uploaded")
	}
	o.BatchBytes = 10000
	o.Upload = false
	if _, e := Synchronize(context.Background(), s, f, []string{"s"}, o); e == nil || f.writes != 0 {
		t.Fatal("verify mutated or accepted incomplete data")
	}
	o.Upload = true
	if _, e := Synchronize(context.Background(), s, f, []string{"s"}, o); e != nil {
		t.Fatal(e)
	}
	f.corrupt = true
	writes := f.writes
	if _, e := Synchronize(context.Background(), s, f, []string{"s"}, o); !errors.Is(e, ErrConflict) || writes != f.writes {
		t.Fatal("corrupt remote accepted", e)
	}
}
