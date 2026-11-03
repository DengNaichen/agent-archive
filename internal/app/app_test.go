package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalCLIAndExplicitSelection(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if e := os.MkdirAll(filepath.Join(home, "projects/p"), 0700); e != nil {
		t.Fatal(e)
	}
	secret := "PRIVATE_BODY_FIXTURE"
	if e := os.WriteFile(filepath.Join(home, "projects/p/s.jsonl"), []byte(`{"type":"user","uuid":"u","sessionId":"s","timestamp":"2026-11-01T00:00:00Z","message":{"content":"`+secret+`"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(dir, "snapshot")
	var stdout, stderr bytes.Buffer
	ctx := context.Background()
	if e := Run(ctx, []string{"collect", "--source", "claude-code", "--home", home, "--output", out}, &stdout, &stderr); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("collector leaked content")
	}
	for _, command := range [][]string{{"status"}, {"plan", "--all"}, {"verify-local"}} {
		stdout.Reset()
		stderr.Reset()
		args := append([]string{"--local-db", filepath.Join(out, "cleaned.sqlite")}, command...)
		if e := Run(ctx, args, &stdout, &stderr); e != nil {
			t.Fatal(command, e)
		}
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("local command leaked content")
		}
	}
	if e := Run(ctx, []string{"--local-db", filepath.Join(out, "cleaned.sqlite"), "plan"}, &stdout, &stderr); e == nil {
		t.Fatal("implicit selection accepted")
	}
	if e := Run(ctx, []string{"sync", "--all"}, &stdout, &stderr); e == nil {
		t.Fatal("missing db accepted")
	}
}
func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"collect", "--help"}, {"sync", "--help"}} {
		var out, err bytes.Buffer
		if e := Run(context.Background(), args, &out, &err); e != nil {
			t.Fatal(e)
		}
	}
}

func TestHelpIsCompleteAndUsesStdout(t *testing.T) {
	for _, command := range []string{"", "collect", "status", "plan", "verify-local", "ping", "init", "sync", "verify", "version"} {
		forms := [][]string{{"help"}}
		if command != "" {
			forms = [][]string{{"help", command}, {command, "--help"}, {command, "-h"}}
		} else {
			forms = append(forms, []string{"--help"}, []string{"-h"}, []string{})
		}
		var baseline string
		for _, args := range forms {
			var out, err bytes.Buffer
			if e := Run(context.Background(), args, &out, &err); e != nil {
				t.Fatalf("%v: %v", args, e)
			}
			if err.Len() != 0 || !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("help must succeed on stdout: %v stdout=%q stderr=%q", args, out.String(), err.String())
			}
			if baseline != "" && baseline != out.String() {
				t.Errorf("help aliases differ for %s", command)
			}
			baseline = out.String()
			if command != "" && !strings.Contains(out.String(), "Examples:") {
				t.Errorf("missing examples: %s", command)
			}
			if needsSnapshot(command) && !strings.Contains(out.String(), "--snapshot PATH") {
				t.Errorf("missing snapshot requirement: %s", command)
			}
			if needsCloud(command) && !strings.Contains(out.String(), "--config FILE") {
				t.Errorf("missing connection requirement: %s", command)
			}
		}
	}
}
func TestHumanAndJSONOutputAndSharedFlagPlacement(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if e := os.MkdirAll(filepath.Join(home, "projects/p"), 0700); e != nil {
		t.Fatal(e)
	}
	raw := `{"type":"user","uuid":"u","sessionId":"s","timestamp":"2026-11-01T00:00:00Z","message":{"content":"PRIVATE_BODY_FIXTURE"}}`
	if e := os.WriteFile(filepath.Join(home, "projects/p/s.jsonl"), []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	snapshot := filepath.Join(dir, "snapshot")
	var out, progress bytes.Buffer
	if e := Run(context.Background(), []string{"collect", "--source", "claude-code", "--home", home, "--output", snapshot}, &out, &progress); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "Snapshot collected and verified.") || !strings.Contains(progress.String(), "Checking source files:") {
		t.Fatal(out.String(), progress.String())
	}
	for _, args := range [][]string{{"--snapshot", snapshot, "status"}, {"status", "--snapshot", snapshot}, {"status", "--local-db", filepath.Join(snapshot, "cleaned.sqlite")}, {"--db", filepath.Join(snapshot, "cleaned.sqlite"), "status"}} {
		out.Reset()
		progress.Reset()
		if e := Run(context.Background(), args, &out, &progress); e != nil {
			t.Fatal(args, e)
		}
		if !strings.Contains(out.String(), "Claude Code") || !strings.Contains(out.String(), "Sessions:") || strings.HasPrefix(out.String(), "{") || progress.Len() != 0 {
			t.Fatalf("unexpected human output: %q / %q", out.String(), progress.String())
		}
	}
	for _, args := range [][]string{{"--json", "--snapshot", snapshot, "status"}, {"status", "--snapshot", snapshot, "--json"}, {"verify-local", "--snapshot", snapshot, "--json"}} {
		out.Reset()
		progress.Reset()
		if e := Run(context.Background(), args, &out, &progress); e != nil {
			t.Fatal(args, e)
		}
		decoder := json.NewDecoder(&out)
		n := 0
		for {
			var row map[string]any
			e := decoder.Decode(&row)
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				t.Fatal("non-JSON mixed into machine output", e)
			}
			n++
		}
		if n == 0 || progress.Len() != 0 {
			t.Fatal("machine progress must be JSON on stdout")
		}
	}
}
func TestUsageErrorsHaveCorrectExitCodeAndRecoveryHint(t *testing.T) {
	tests := []struct {
		args    []string
		message string
	}{
		{[]string{"syncc"}, "did you mean 'sync'"},
		{[]string{"sync", "--nonsense"}, "unknown option --nonsense"},
		{[]string{"sync", "--batch-rows", "PRIVATE_VALUE"}, "invalid value for --batch-rows"},
		{[]string{"collect", "--source"}, "missing value for --source"},
		{[]string{"collect", "--source", "codex"}, "--output is required"},
		{[]string{"collect", "--source", "invalid", "--output", "unused"}, "--source must be"},
		{[]string{"status"}, "--snapshot is required"},
		{[]string{"plan", "--snapshot", "DO_NOT_OPEN"}, "choose either --all"},
		{[]string{"plan", "--snapshot", "DO_NOT_OPEN", "--all", "--session", "s"}, "choose either --all"},
		{[]string{"sync", "--snapshot", "DO_NOT_OPEN", "--config", "DO_NOT_OPEN", "--all", "--delay", "NaN"}, "--delay must be"},
		{[]string{"version", "extra"}, "unexpected positional argument"},
	}
	for _, test := range tests {
		var out, stderr bytes.Buffer
		e := Run(context.Background(), test.args, &out, &stderr)
		if e == nil {
			t.Fatalf("accepted invalid invocation: %v", test.args)
		}
		if code := WriteError(&stderr, e); code != 2 {
			t.Fatalf("usage exit code %d: %v", code, test.args)
		}
		if !strings.Contains(stderr.String(), test.message) || !strings.Contains(stderr.String(), "agent-archive help") || strings.Contains(stderr.String(), "PRIVATE_VALUE") || out.Len() != 0 {
			t.Fatalf("unhelpful/leaky error for %v: %q", test.args, stderr.String())
		}
	}
	var out, stderr bytes.Buffer
	e := Run(context.Background(), []string{"--json", "collect", "--source", "codex"}, &out, &stderr)
	if WriteError(&stderr, e) != 2 {
		t.Fatal("wrong machine exit code")
	}
	var row map[string]any
	if e = json.Unmarshal(stderr.Bytes(), &row); e != nil {
		t.Fatal(e)
	}
	if row["exit_code"] != float64(2) || row["hint"] == nil {
		t.Fatal(row)
	}
}
func TestOperationalErrorsAndCancellation(t *testing.T) {
	var out, stderr bytes.Buffer
	e := Run(context.Background(), []string{"status", "--snapshot", filepath.Join(t.TempDir(), "missing")}, &out, &stderr)
	if WriteError(&stderr, e) != 1 || !strings.Contains(stderr.String(), "snapshot path cannot be read") {
		t.Fatal(stderr.String())
	}
	stderr.Reset()
	if WriteError(&stderr, &commandError{err: context.Canceled, command: "collect"}) != 130 || !strings.Contains(stderr.String(), "new directory") {
		t.Fatal(stderr.String())
	}
}
