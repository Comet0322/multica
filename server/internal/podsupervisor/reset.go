package podsupervisor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const killDeadline = 10 * time.Second

// killDescendants SIGKILLs every descendant of this process until none is
// left, reaping them as it goes. It loops because a dying process can fork
// once more before the signal lands.
func killDescendants() error {
	self := os.Getpid()
	deadline := time.Now().Add(killDeadline)
	for {
		procs := descendants(self)
		if len(procs) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d descendant processes survived SIGKILL", len(procs))
		}
		for _, p := range procs {
			if !p.zombie {
				killProc(p.pid)
			}
		}
		for _, p := range procs {
			reapProc(p.pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reapZombies collects exited orphans that were reparented to the supervisor,
// leaving keepPid (the daemon, reaped by its own Wait) alone.
func reapZombies(keepPid int) {
	self := os.Getpid()
	for _, p := range listProcs() {
		if p.ppid == self && p.zombie && p.pid != keepPid {
			reapProc(p.pid)
		}
	}
}

// emptyDir removes everything inside dir but keeps dir itself, since it is a
// mount point. Hidden files, symlinks (never followed) and read-only nested
// directories are all removed.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := removeTree(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeTree(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.IsDir() {
		// A read-only directory can neither be listed nor emptied.
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
		if err := emptyDir(path); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// owner is the non-root user that must own the restored files.
type owner struct{ uid, gid int }

func (o *owner) chown(path string) error {
	if o == nil {
		return nil
	}
	return os.Chown(path, o.uid, o.gid)
}

func (o *owner) lchown(path string) error {
	if o == nil {
		return nil
	}
	return os.Lchown(path, o.uid, o.gid)
}

// restoreTemplate copies the read-only template tree into dst, preserving
// file modes and symlinks. owner, when set, is applied to every copied entry.
func restoreTemplate(template, dst string, own *owner) error {
	if template == "" {
		return nil
	}
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	err := filepath.WalkDir(template, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(template, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			return own.lchown(target)
		case d.IsDir():
			// Created writable so children can be added; the final mode is
			// applied once the directory is fully populated.
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{target, info.Mode().Perm()})
			return own.chown(target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm(), own)
		default:
			return fmt.Errorf("template entry %s has unsupported type %v", rel, d.Type())
		}
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode fs.FileMode, own *owner) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := own.chown(dst); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
