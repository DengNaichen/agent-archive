package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/DengNaichen/agent-archive/internal/history"
	"github.com/DengNaichen/agent-archive/internal/runstate"
)

type TaskState struct {
	Version   int                   `json:"version"`
	Scope     string                `json:"scope"`
	Status    string                `json:"status"`
	Source    string                `json:"source"`
	Session   string                `json:"current_session,omitempty"`
	Attempt   int                   `json:"attempt"`
	Updated   time.Time             `json:"updated_at"`
	Error     string                `json:"error,omitempty"`
	Completed map[string]SyncResult `json:"completed"`
	Result    SyncResult            `json:"result"`
}

// Synchronize retries an entire session only after checking the server again.
// A checkpoint is written only after actual content verification, never on INSERT ack.
func Synchronize(ctx context.Context, store *history.Store, t Transport, ids []string, o SyncOptions) (result SyncResult, err error) {
	result.Source = store.Source()
	snapshotLock, e := store.Lock(ctx)
	if e != nil {
		return result, e
	}
	defer func() { err = errors.Join(err, snapshotLock.Close()) }()
	if o.BatchRows <= 0 || o.BatchBytes <= 0 || o.Delay < 0 {
		return result, fault.New("batch limits must be positive and delay nonnegative")
	}
	if o.Retries < 0 || o.RetryDelay < 0 {
		return result, fault.New("retry limits must be nonnegative")
	}
	state := TaskState{Version: 1, Status: "running", Source: store.Source(), Completed: map[string]SyncResult{}}
	persist := func() error { return nil }
	if o.StateDir != "" {
		if o.Destination == "" {
			return result, fault.New("resumable sync requires destination identity")
		}
		lock, e := runstate.Lock(ctx, filepath.Join(o.StateDir, "lock.sqlite"))
		if e != nil {
			return result, e
		}
		defer func() { err = errors.Join(err, lock.Close()) }()
		digest, e := store.ContentDigest(ctx)
		if e != nil {
			return result, e
		}
		binding, e := json.Marshal([]any{digest, o.Destination, ids, o.Upload})
		if e != nil {
			return result, e
		}
		sum := sha256.Sum256(binding)
		state.Scope = hex.EncodeToString(sum[:])
		path := filepath.Join(o.StateDir, "task.json")
		b, e := os.ReadFile(path)
		if e == nil {
			var previous TaskState
			if e = json.Unmarshal(b, &previous); e != nil {
				return result, fault.New("task state is invalid; choose a new --state directory")
			}
			if previous.Version != 1 {
				return result, fault.New("unsupported task state version")
			}
			if previous.Scope == state.Scope {
				state.Completed = previous.Completed
				if state.Completed == nil {
					state.Completed = map[string]SyncResult{}
				}
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return result, e
		}
		persist = func() error {
			state.Updated = time.Now().UTC()
			state.Result = result
			return runstate.Write(path, state)
		}
	}
	defer func() {
		if err != nil {
			state.Status = "failed"
			state.Error = fault.Message(err)
			if errors.Is(err, context.Canceled) {
				state.Status = "interrupted"
			}
		} else {
			state.Status = "complete"
			state.Error = ""
		}
		err = errors.Join(err, persist())
	}()
	if err = store.EnsureComplete(ctx); err != nil {
		return result, err
	}
	for _, sid := range ids {
		if e := ctx.Err(); e != nil {
			return result, e
		}
		if previous, ok := state.Completed[sid]; ok {
			result.Sessions++
			result.Verified += previous.Verified
			result.AlreadyPresent += previous.Verified
			if o.Progress != nil {
				if e := o.Progress(map[string]any{"event": "session_resumed", "session_id": sid, "completed_sessions": result.Sessions, "total_sessions": len(ids)}); e != nil {
					return result, e
				}
			}
			continue
		}
		state.Session = sid
		var session SyncResult
		for attempt := 0; ; attempt++ {
			state.Attempt = attempt + 1
			state.Status = "running"
			state.Error = ""
			if e := persist(); e != nil {
				return result, e
			}
			one := o
			one.StateDir = ""
			one.Retries = 0
			one.Progress = func(v any) error {
				if m, ok := v.(map[string]any); ok && m["event"] == "session_verified" {
					m["completed_sessions"] = result.Sessions + 1
					m["total_sessions"] = len(ids)
				}
				if o.Progress != nil {
					return o.Progress(v)
				}
				return nil
			}
			var e error
			session, e = synchronizeOnce(ctx, store, t, []string{sid}, one)
			// Includes acknowledged writes before a failed attempt, without assuming
			// unacknowledged commits. Final verification determines completeness.
			result.Inserted += session.Inserted
			if e == nil {
				break
			}
			if !Retryable(e) || attempt >= o.Retries || ctx.Err() != nil {
				return result, e
			}
			state.Status = "retry_wait"
			state.Error = fault.Message(e)
			if e = persist(); e != nil {
				return result, e
			}
			if o.Progress != nil {
				if e = o.Progress(map[string]any{"event": "retry_wait", "session_id": sid, "attempt": attempt + 1, "retry_in_seconds": o.RetryDelay.Seconds()}); e != nil {
					return result, e
				}
			}
			timer := time.NewTimer(o.RetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return result, ctx.Err()
			case <-timer.C:
			}
		}
		result.Sessions++
		result.Verified += session.Verified
		result.AlreadyPresent += session.AlreadyPresent
		state.Completed[sid] = session
		if e := persist(); e != nil {
			return result, e
		}
	}
	result.Success = true
	return result, nil
}
