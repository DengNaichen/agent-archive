package history

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
)

func prompt(o map[string]any) bool {
	if Str(o, "type") != "user" || o["isMeta"] == true {
		return false
	}
	blocks, _ := Obj(o["message"])["content"].([]any)
	for _, b := range blocks {
		if Str(Obj(b), "type") == "tool_result" {
			return false
		}
	}
	return true
}
func ingestClaude(ctx context.Context, tx *sql.Tx, a archive, r *CollectReport) error {
	parents := map[string]string{}
	prompts := map[string]bool{}
	embedded := map[string]bool{}
	first := ""
	err := scanArchive(ctx, a, func(_ int, b []byte) error {
		o, e := Decode(b)
		if e != nil {
			return nil
		}
		if id := Str(o, "sessionId"); id != "" {
			embedded[id] = true
		}
		if t, ok := eventTime(Str(o, "timestamp")); ok && (first == "" || t < first) {
			first = t
		}
		if id := Str(o, "uuid"); id != "" {
			parents[id] = Str(o, "parentUuid")
			if prompt(o) {
				prompts[id] = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	sub := strings.Contains("/"+a.Path, "/subagents/")
	stem := strings.TrimSuffix(filepath.Base(a.Path), ".jsonl")
	root := stem
	if sub {
		root = filepath.Base(filepath.Dir(filepath.Dir(a.Path)))
	}
	defaultID := root
	if len(embedded) == 1 {
		for id := range embedded {
			defaultID = id
		}
	}
	stream := defaultID
	parent, kind := "", "main_session"
	if sub {
		stream += ":" + stem
		parent = defaultID
		kind = "subagent"
	}
	if err = saveSession(ctx, tx, stream, parent, kind); err != nil {
		return err
	}
	resolved := map[string]string{}
	resolve := func(node string) string {
		var trail []string
		seen := map[string]bool{}
		for node != "" && !prompts[node] && !seen[node] {
			if _, ok := resolved[node]; ok {
				break
			}
			seen[node] = true
			trail = append(trail, node)
			node = parents[node]
		}
		id := resolved[node]
		if prompts[node] {
			id = node
		}
		for _, n := range trail {
			resolved[n] = id
		}
		return id
	}
	return scanArchive(ctx, a, func(line int, b []byte) error {
		o, e := Decode(b)
		if e != nil {
			return issue(ctx, tx, a.Path, line, "invalid_json_object", r)
		}
		sid := stream
		if !sub && Str(o, "sessionId") != "" && Str(o, "sessionId") != defaultID {
			sid = Str(o, "sessionId")
			if e = saveSession(ctx, tx, sid, "", "main_session"); e != nil {
				return e
			}
		}
		raw := string(b)
		sha := SHA(b)
		eid := SHA([]byte("claude_code\x00" + sid + "\x00" + sha))
		stamp, ok := eventTime(Str(o, "timestamp"))
		basis := "record"
		if !ok {
			stamp = first
			basis = "file_first_timestamp"
			if first == "" {
				stamp = "1970-01-01 00:00:00.000"
				basis = "unknown_sentinel"
			}
		}
		tid := resolve(Str(o, "uuid"))
		if tid != "" {
			if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO turns VALUES(?,?,?,?)", sid, tid, "", kind); e != nil {
				return e
			}
		}
		m := Obj(o["message"])
		role := Str(m, "role")
		typ := Str(o, "type")
		if role == "" && (typ == "user" || typ == "assistant") {
			role = typ
		}
		activity := kind
		if o["isSidechain"] == true {
			activity = "subagent"
		}
		ev := Event{Source: "claude_code", SessionID: sid, EventID: eid, EventTime: stamp, EventType: typ, Subtype: Str(o, "subtype"), TurnID: tid, Activity: activity, Model: Str(m, "model"), Role: role, RecordJSON: raw, RecordSHA: sha}
		loc := map[string]any{"file": a.Path, "line": line, "source_file_sha256": a.SHA, "timestamp_basis": basis, "parent_session_id": parent}
		r.Occurrences++
		return saveEvent(ctx, tx, ev, loc)
	})
}
