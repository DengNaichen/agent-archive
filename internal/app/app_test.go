package app

import (
	"bytes"
	"context"
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
