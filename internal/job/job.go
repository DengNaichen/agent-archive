// Package job owns one resumable collect/sync/verify cycle for both sources.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/DengNaichen/agent-archive/internal/cloud"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/DengNaichen/agent-archive/internal/history"
	"github.com/DengNaichen/agent-archive/internal/runstate"
)

type Options struct {
	StateDir, CodexHome, ClaudeHome string
	Client                          *cloud.Client
	Sync                            cloud.SyncOptions
	Progress                        history.Progress
}
type State struct {
	Version int                         `json:"version"`
	Status  string                      `json:"status"`
	Cycle   string                      `json:"cycle"`
	Binding string                      `json:"binding"`
	Stage   int                         `json:"next_stage"`
	Phase   string                      `json:"phase"`
	Updated time.Time                   `json:"updated_at"`
	Error   string                      `json:"error,omitempty"`
	Results map[string]cloud.SyncResult `json:"results"`
}

func Read(dir string) (s State, err error) {
	b, e := os.ReadFile(filepath.Join(dir, "job.json"))
	if e != nil {
		return s, e
	}
	if e = json.Unmarshal(b, &s); e != nil {
		return s, fault.New("job state is invalid")
	}
	if s.Stage < 0 || s.Stage > 6 {
		return s, fault.New("job stage is invalid")
	}
	if s.Status == "complete" && s.Stage != 6 {
		return s, fault.New("completed job has unfinished stages")
	}
	if s.Version != 1 {
		return s, fault.New("unsupported job state version")
	}
	return s, nil
}
func Run(ctx context.Context, o Options) (State, error) { return run(ctx, o, o.Client) }
func run(ctx context.Context, o Options, transport cloud.Transport) (state State, err error) {
	if o.Client == nil || o.StateDir == "" {
		return state, fault.New("run requires state directory and cloud connection")
	}
	lock, e := runstate.Lock(ctx, filepath.Join(o.StateDir, "job-lock.sqlite"))
	if e != nil {
		return state, e
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	binding, e := json.Marshal([]string{o.Client.Destination(), o.CodexHome, o.ClaudeHome})
	if e != nil {
		return state, e
	}
	state, e = Read(o.StateDir)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return state, e
	}
	key := history.SHA(binding)
	if e == nil && state.Binding != key {
		return state, fault.New("run state belongs to different source homes or destination; choose another --state directory")
	}
	if errors.Is(e, os.ErrNotExist) || state.Status == "complete" {
		state = State{Version: 1, Cycle: time.Now().UTC().Format("20060102T150405.000000000"), Binding: key, Results: map[string]cloud.SyncResult{}}
	}
	persist := func() error {
		state.Updated = time.Now().UTC()
		return runstate.Write(filepath.Join(o.StateDir, "job.json"), state)
	}
	defer func() {
		if err != nil {
			state.Status = "failed"
			state.Error = fault.Message(err)
			if errors.Is(err, context.Canceled) {
				state.Status = "interrupted"
			}
		}
		err = errors.Join(err, persist())
	}()
	if state.Results == nil {
		state.Results = map[string]cloud.SyncResult{}
	}
	state.Status = "running"
	state.Error = ""
	sources := []string{"codex", "claude-code"}
	homes := []string{o.CodexHome, o.ClaudeHome}
	for state.Stage < 6 {
		step := state.Stage
		source := sources[step%2]
		snapshot := filepath.Join(o.StateDir, "snapshots", source)
		phase := []string{"collect", "sync", "verify"}[step/2]
		state.Phase = phase + "-" + source
		if e = persist(); e != nil {
			return state, e
		}
		if o.Progress != nil {
			if e = o.Progress(map[string]any{"event": "job_stage", "phase": state.Phase, "stage": step + 1, "total_stages": 6}); e != nil {
				return state, e
			}
		}
		if phase == "collect" {
			_, e = history.Collect(ctx, history.CollectOptions{Source: source, Home: homes[step%2], Output: snapshot, Incremental: true, Progress: o.Progress})
		} else {
			var store *history.Store
			store, e = history.Open(filepath.Join(snapshot, "cleaned.sqlite"))
			if e == nil {
				var ids []string
				ids, e = store.Sessions(ctx, nil, true)
				if e == nil {
					opt := o.Sync
					opt.Upload = phase == "sync"
					opt.Progress = o.Progress
					opt.Destination = o.Client.Destination()
					opt.StateDir = filepath.Join(o.StateDir, "cycles", state.Cycle, state.Phase)
					var result cloud.SyncResult
					result, e = cloud.Synchronize(ctx, store, transport, ids, opt)
					state.Results[state.Phase] = result
				}
				e = errors.Join(e, store.Close())
			}
		}
		if e != nil {
			return state, e
		}
		state.Stage++
		if e = persist(); e != nil {
			return state, e
		}
	}
	state.Status = "complete"
	state.Phase = "complete"
	state.Error = ""
	return state, nil
}
