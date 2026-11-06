package runstate

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExclusiveLockReleaseAndPrivateAtomicState(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lock.sqlite")
	a, e := Lock(context.Background(), p)
	if e != nil {
		t.Fatal(e)
	}
	if b, e := Lock(context.Background(), p); e == nil {
		if e = b.Close(); e != nil {
			t.Error(e)
		}
		t.Fatal("two owners")
	}
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	b, e := Lock(context.Background(), p)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Close(); e != nil {
		t.Fatal(e)
	}
	state := filepath.Join(dir, "task.json")
	for _, v := range []string{"first", "second"} {
		if e = Write(state, v); e != nil {
			t.Fatal(e)
		}
	}
	raw, e := os.ReadFile(state)
	if e != nil || string(raw) != "\"second\"\n" {
		t.Fatal(string(raw), e)
	}
	st, e := os.Stat(state)
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, e)
	}
}

func TestLockReleasedAfterProcessDeath(t *testing.T) {
	path := os.Getenv("AGENT_ARCHIVE_TEST_LOCK")
	if path != "" {
		lock, e := Lock(context.Background(), path)
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			if e := lock.Close(); e != nil {
				t.Error(e)
			}
		}()
		if _, e = fmt.Fprintln(os.Stdout, "locked"); e != nil {
			t.Fatal(e)
		}
		time.Sleep(time.Minute)
		return
	}
	path = filepath.Join(t.TempDir(), "process.sqlite")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockReleasedAfterProcessDeath$")
	cmd.Env = append(os.Environ(), "AGENT_ARCHIVE_TEST_LOCK="+path)
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	line, e := bufio.NewReader(pipe).ReadString('\n')
	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait()
	if e != nil || line != "locked\n" || killErr != nil || waitErr == nil {
		t.Fatal(line, e, killErr, waitErr)
	}
	lock, e := Lock(context.Background(), path)
	if e != nil {
		t.Fatal("lock stayed held after process exit", e)
	}
	if e = lock.Close(); e != nil {
		t.Fatal(e)
	}
}
