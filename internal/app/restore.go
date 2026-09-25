package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Restore replaces a stopped instance's database. The daemon uses the same lock.
// Copy and validate before touching the old database; never copy live WAL files.
func Restore(path, source string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	path = abs
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("stop the server before restoring its database")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	dest, err := os.CreateTemp(filepath.Dir(path), ".restore-*.db")
	if err != nil {
		return err
	}
	tmp := dest.Name()
	defer os.Remove(tmp)
	defer os.Remove(tmp + "-wal")
	defer os.Remove(tmp + "-shm")
	if _, err = io.Copy(dest, src); err != nil {
		dest.Close()
		return err
	}
	if err = dest.Sync(); err != nil {
		dest.Close()
		return err
	}
	if err = dest.Close(); err != nil {
		return err
	}
	test, err := Open(tmp, false)
	if err != nil {
		return err
	}
	var check string
	err = test.DB.QueryRow(`PRAGMA integrity_check`).Scan(&check)
	if err == nil && check != "ok" {
		err = errors.New("backup integrity check failed")
	}
	if err == nil {
		err = test.ResetAccess()
	}
	if err == nil {
		_, err = test.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	closeErr := test.DB.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
