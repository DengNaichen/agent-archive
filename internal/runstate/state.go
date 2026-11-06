// Package runstate owns private, atomic task records and process-independent locks.
package runstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/DengNaichen/agent-archive/internal/fault"
	sqlite "modernc.org/sqlite"
)

var ErrLocked = fault.New("another operation holds the task lock; wait for it to finish")

// Lock uses SQLite's OS locks: process death releases ownership without stale PID files.
func Lock(ctx context.Context, path string) (db *sql.DB, err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = f.Close(); e != nil {
		return nil, e
	}
	db, e = sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if _, e = db.ExecContext(ctx, "BEGIN EXCLUSIVE"); e != nil {
		var failure *sqlite.Error
		if errors.As(e, &failure) && (failure.Code()&255 == 5 || failure.Code()&255 == 6) {
			return nil, errors.Join(ErrLocked, db.Close())
		}
		return nil, errors.Join(fmt.Errorf("acquire task lock: %w", e), db.Close())
	}
	return db, nil
}
func Write(path string, v any) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".state-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer func() {
		if e := os.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	e = json.NewEncoder(f).Encode(v)
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return fmt.Errorf("persist task state: %w", e)
	}
	return os.Rename(name, path)
}

// Busy distinguishes an abandoned persisted running state from a current owner.
// Missing locks are not created by status inspection.
func Busy(ctx context.Context, path string) (bool, error) {
	if _, e := os.Stat(path); errors.Is(e, os.ErrNotExist) {
		return false, nil
	} else if e != nil {
		return false, e
	}
	lock, e := Lock(ctx, path)
	if errors.Is(e, ErrLocked) {
		return true, nil
	}
	if e != nil {
		return false, e
	}
	return false, lock.Close()
}
