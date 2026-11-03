package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/DengNaichen/agent-archive/internal/history"
	"reflect"
	"time"
)

// Transport is the real variation seam: HTTP in production, a fault-injecting
// transport in tests. Synchronization owns batching, comparison and verification.
type Transport interface {
	Request(context.Context, string, []byte, bool) ([]byte, error)
	Rows(context.Context, string, any) error
}
type SyncOptions struct {
	Upload                bool
	BatchBytes, BatchRows int
	Delay                 time.Duration
	Progress              history.Progress
}
type SyncResult struct {
	Success        bool   `json:"success"`
	Source         string `json:"source"`
	Sessions       int    `json:"sessions"`
	Verified       int    `json:"verified_events"`
	Inserted       int    `json:"inserted_this_run"`
	AlreadyPresent int    `json:"already_present"`
}
type remoteHash struct {
	ID     string `json:"event_id"`
	SHA    string `json:"record_sha256"`
	Actual string `json:"actual_sha256"`
}

func Synchronize(ctx context.Context, store *history.Store, t Transport, ids []string, o SyncOptions) (result SyncResult, err error) {
	result.Source = store.Source()
	if e := store.EnsureComplete(ctx); e != nil {
		return result, e
	}
	if o.BatchBytes <= 0 || o.BatchRows <= 0 || o.Delay < 0 {
		return result, fault.New("batch limits must be positive and delay nonnegative")
	}
	var columns []struct {
		Name string `json:"name"`
	}
	if err = t.Rows(ctx, "DESCRIBE TABLE agent_history.raw_events", &columns); err != nil {
		return result, err
	}
	names := []string{}
	for _, c := range columns {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, history.Columns) {
		return result, fault.New("cloud schema differs; insertion refused")
	}
	for _, sid := range ids {
		expected := map[string]string{}
		initial, e := hashes(ctx, t, store.Source(), sid)
		if e != nil {
			return result, e
		}
		existing := map[string]string{}
		for _, r := range initial {
			if _, ok := existing[r.ID]; ok || r.SHA != r.Actual {
				return result, ErrConflict
			}
			existing[r.ID] = r.SHA
		}
		// Preflight the whole session before any insert: conflicts, oversized records,
		// local content hashes and source references. A partial failure is resumable.
		e = store.Walk(ctx, sid, func(ev history.Event) error {
			if ev.Source != store.Source() || ev.SessionID != sid || len(ev.EventID) != 64 || len(ev.RecordSHA) != 64 {
				return fault.New("local event identity/source is inconsistent")
			}
			if history.SHA([]byte(ev.RecordJSON)) != ev.RecordSHA {
				return fault.New("local event checksum mismatch")
			}
			if ev.Locations == "" || ev.Locations == "[]" {
				return fault.New("event lacks source references")
			}
			if _, ok := expected[ev.EventID]; ok {
				return fault.New("duplicate local event id")
			}
			expected[ev.EventID] = ev.RecordSHA
			if sha, ok := existing[ev.EventID]; ok && sha != ev.RecordSHA {
				return ErrConflict
			}
			b, e := json.Marshal(ev)
			if e != nil {
				return fmt.Errorf("encode event: %w", e)
			}
			if _, already := existing[ev.EventID]; o.Upload && !already && len(b)+1 > o.BatchBytes {
				return fault.New("event exceeds batch byte limit; raise --batch-bytes to preserve it whole")
			}
			return nil
		})
		if e != nil {
			return result, e
		}
		for id := range existing {
			if _, ok := expected[id]; !ok {
				return result, ErrConflict
			}
		}
		if !o.Upload && len(existing) != len(expected) {
			return result, fault.New("remote session is incomplete")
		}
		var batch bytes.Buffer
		rows := 0
		flush := func() error {
			if rows == 0 {
				return nil
			}
			if _, e := t.Request(ctx, "INSERT INTO agent_history.raw_events FORMAT JSONEachRow", batch.Bytes(), true); e != nil {
				return e
			}
			result.Inserted += rows
			if o.Progress != nil {
				if e := o.Progress(map[string]any{"event": "batch_uploaded", "session_id": sid, "rows": rows, "uncompressed_bytes": batch.Len()}); e != nil {
					return e
				}
			}
			batch.Reset()
			rows = 0
			if o.Delay > 0 {
				timer := time.NewTimer(o.Delay)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
				}
			}
			return nil
		}
		if o.Upload {
			e = store.Walk(ctx, sid, func(ev history.Event) error {
				if _, ok := existing[ev.EventID]; ok {
					return nil
				}
				b, e := json.Marshal(ev)
				if e != nil {
					return e
				}
				if rows > 0 && (rows >= o.BatchRows || batch.Len()+len(b)+1 > o.BatchBytes) {
					if e = flush(); e != nil {
						return e
					}
				}
				if _, e = batch.Write(b); e != nil {
					return e
				}
				if e = batch.WriteByte('\n'); e != nil {
					return e
				}
				rows++
				return nil
			})
			if e != nil {
				return result, e
			}
			if e = flush(); e != nil {
				return result, e
			}
		}
		remote, e := hashes(ctx, t, store.Source(), sid)
		if e != nil {
			return result, e
		}
		if len(remote) != len(expected) {
			return result, fault.New("remote event count mismatch")
		}
		seen := map[string]bool{}
		for _, r := range remote {
			sha, ok := expected[r.ID]
			if !ok || seen[r.ID] || r.SHA != sha || r.Actual != sha {
				return result, fault.New("remote full-content verification failed")
			}
			seen[r.ID] = true
		}
		result.Verified += len(expected)
		result.AlreadyPresent += len(existing)
		result.Sessions++
		if o.Progress != nil {
			if e = o.Progress(map[string]any{"event": "session_verified", "session_id": sid, "events": len(expected), "completed_sessions": result.Sessions, "total_sessions": len(ids)}); e != nil {
				return result, e
			}
		}
	}
	result.Success = true
	return result, nil
}
func hashes(ctx context.Context, t Transport, source, sid string) ([]remoteHash, error) {
	var rows []remoteHash
	e := t.Rows(ctx, "SELECT event_id,record_sha256,lower(hex(SHA256(record_json))) AS actual_sha256 FROM agent_history.raw_events FINAL WHERE "+Scope(source, sid), &rows)
	return rows, e
}
