package history

import (
	"context"
	"database/sql"
	"fmt"
)

func ingestCodex(ctx context.Context, tx *sql.Tx, a archive, r *CollectReport) error {
	sid, tid, kind := "", "", "unknown"
	fid, err := Fingerprint(a.Path)
	if err != nil {
		return err
	}
	return scanArchive(ctx, a, func(line int, b []byte) error {
		if len(b) == 0 {
			return nil
		}
		o, e := Decode(b)
		if e != nil {
			return issue(ctx, tx, a.Path, line, "invalid_json", r)
		}
		p, ok := o["payload"].(map[string]any)
		typ := Str(o, "type")
		if !ok || typ == "" {
			return issue(ctx, tx, a.Path, line, "unsupported_envelope", r)
		}
		sub := Str(p, "type")
		if typ == "session_meta" {
			id := Str(p, "id")
			if id == "" {
				id = Str(p, "session_id")
			}
			if id != "" {
				sid = id
				tid = ""
				kind = Str(p, "thread_source")
				if kind == "" {
					kind = "unknown"
					if _, ok := Obj(p["source"])["subagent"]; ok {
						kind = "subagent"
					}
				}
				if e = saveSession(ctx, tx, sid, Str(p, "parent_thread_id"), kind); e != nil {
					return e
				}
			}
		}
		eventSid := Str(p, "thread_id")
		if eventSid == "" {
			eventSid = sid
		}
		if eventSid == "" {
			eventSid = Str(p, "session_id")
		}
		if eventSid == "" {
			eventSid = "unattributed:" + fid
		}
		if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sessions VALUES(?,?,?)", eventSid, "", kind); e != nil {
			return e
		}
		eventTurn := Str(p, "turn_id")
		if eventTurn == "" {
			eventTurn = tid
		}
		if typ == "turn_context" || (typ == "event_msg" && sub == "task_started") {
			if id := Str(p, "turn_id"); id != "" {
				eventTurn = id
				tid = id
			}
		}
		activity := "unknown"
		switch kind {
		case "user":
			activity = "main_session"
		case "subagent":
			activity = "subagent"
		case "guardian_review":
			activity = "auto_review"
		}
		attribution := Obj(p["turn_attribution"])
		if Str(p, "model") == "codex-auto-review" || Str(attribution, "turn_trigger") == "guardian_review" {
			activity = "auto_review"
		}
		model := ""
		if typ == "turn_context" {
			model = Str(p, "model")
		}
		if eventTurn != "" {
			_, e = tx.ExecContext(ctx, `INSERT INTO turns VALUES(?,?,?,?) ON CONFLICT(session_id,turn_id) DO UPDATE SET model=coalesce(nullif(excluded.model,''),model),activity_class=CASE WHEN excluded.activity_class='auto_review' THEN excluded.activity_class ELSE activity_class END`, eventSid, eventTurn, model, activity)
			if e != nil {
				return fmt.Errorf("store codex turn: %w", e)
			}
		}
		ph, e := Fingerprint(p)
		if e != nil {
			return e
		}
		eid, e := Fingerprint([]any{eventSid, typ, o["timestamp"], ph})
		if e != nil {
			return e
		}
		raw, e := CompactOriginal(b)
		if e != nil {
			return e
		}
		stamp, valid := eventTime(Str(o, "timestamp"))
		basis := "record"
		if !valid {
			basis = "unknown_sentinel"
			if e = issue(ctx, tx, a.Path, line, "missing_or_invalid_timestamp", r); e != nil {
				return e
			}
		}
		record := Event{Source: "codex", SessionID: eventSid, EventID: eid, EventTime: stamp, EventType: typ, Subtype: sub, TurnID: eventTurn, Activity: activity, Model: model, Role: Str(p, "role"), Phase: Str(p, "phase"), RecordJSON: raw, RecordSHA: SHA([]byte(raw))}
		loc := map[string]any{"file": a.Path, "line": line, "ordinal": o["ordinal"], "source_file_sha256": a.SHA}
		if !valid {
			loc["timestamp_basis"] = basis
		}
		r.Occurrences++
		return saveEvent(ctx, tx, record, loc)
	})
}
