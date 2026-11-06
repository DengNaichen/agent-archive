package cloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DengNaichen/agent-archive/internal/history"
)

type unreliable struct {
	*fakeTransport
	fail   bool
	reads  int
	always bool
}

func (f *unreliable) Request(ctx context.Context, sql string, b []byte, z bool) ([]byte, error) {
	r, e := f.fakeTransport.Request(ctx, sql, b, z)
	if e != nil {
		return r, e
	}
	if f.fail || f.always {
		f.fail = false
		return nil, transient("simulated commit response lost")
	}
	return r, nil
}
func (f *unreliable) Rows(ctx context.Context, sql string, out any) error {
	f.reads++
	return f.fakeTransport.Rows(ctx, sql, out)
}
func TestRetryCommittedBatchThenPersistentResumeAndFreshVerify(t *testing.T) {
	s := fixture(t)
	f := &unreliable{fakeTransport: &fakeTransport{events: map[string]history.Event{}}, fail: true}
	o := SyncOptions{Upload: true, BatchRows: 2, BatchBytes: 10000, Retries: 2, StateDir: t.TempDir(), Destination: "target"}
	r, e := Synchronize(context.Background(), s, f, []string{"s"}, o)
	if e != nil || r.Verified != 5 || f.writes != 3 {
		t.Fatalf("%+v %v writes=%d", r, e, f.writes)
	}
	reads := f.reads
	r, e = Synchronize(context.Background(), s, f, []string{"s"}, o)
	if e != nil || r.Inserted != 0 || r.Verified != 5 || f.reads != reads {
		t.Fatalf("resume %+v %v", r, e)
	}
	f.corrupt = true
	// A fresh verification pass must not inherit upload checkpoints.
	o.Upload = false
	o.StateDir = t.TempDir()
	if _, e = Synchronize(context.Background(), s, f, []string{"s"}, o); !errors.Is(e, ErrConflict) {
		t.Fatal("fresh check accepted corrupt server", e)
	}
	f.corrupt = false
	o.Upload = true
	o.Destination = "different-target"
	f.events = map[string]history.Event{}
	if r, e = Synchronize(context.Background(), s, f, []string{"s"}, o); e != nil || r.Inserted != 5 {
		t.Fatal("destination binding", r, e)
	}
}
func TestRetryLimitCancellationAndCheckpointFailure(t *testing.T) {
	s := fixture(t)
	f := &unreliable{fakeTransport: &fakeTransport{events: map[string]history.Event{}}, always: true}
	o := SyncOptions{Upload: true, BatchRows: 1, BatchBytes: 10000, Retries: 1}
	if _, e := Synchronize(context.Background(), s, f, []string{"s"}, o); !Retryable(e) || f.writes != 2 {
		t.Fatal("retry budget", f.writes, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.Retries = 5
	o.RetryDelay = time.Hour
	o.Progress = func(v any) error {
		if m, ok := v.(map[string]any); ok && m["event"] == "retry_wait" {
			cancel()
		}
		return nil
	}
	if _, e := Synchronize(ctx, s, f, []string{"s"}, o); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	o.Progress = nil
	o.RetryDelay = 0
	o.StateDir = t.TempDir()
	o.Destination = "target"
	if e := os.WriteFile(filepath.Join(o.StateDir, "task.json"), []byte("bad state"), 0600); e != nil {
		t.Fatal(e)
	}
	writes := f.writes
	if _, e := Synchronize(context.Background(), s, f, []string{"s"}, o); e == nil || f.writes != writes {
		t.Fatal("invalid state mutated server", e)
	}
}
