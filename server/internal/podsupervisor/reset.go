package podsupervisor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
// leaving alone the processes keep names (the daemon and the agents the
// supervisor started, each reaped by its own Wait).
func reapZombies(keep func(pid int) bool) {
	self := os.Getpid()
	for _, p := range listProcs() {
		if p.ppid == self && p.zombie && !keep(p.pid) {
			reapProc(p.pid)
		}
	}
}

// emptyDir removes everything inside dir but keeps dir itself, since it is a
// mount point. Hidden files, symlinks (never followed) and read-only nested
// directories are all removed. Every step goes through an os.Root, so a link
// planted while the tree is being emptied cannot lead the removal outside dir.
func emptyDir(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return emptyRoot(root)
}

func emptyRoot(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := removeIn(root, e.Name()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeIn removes name, a direct child of root, with everything below it.
func removeIn(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.IsDir() {
		// A read-only directory can neither be listed nor emptied.
		if err := root.Chmod(name, 0o700); err != nil {
			return err
		}
		sub, err := root.OpenRoot(name)
		if err != nil {
			return err
		}
		err = emptyRoot(sub)
		_ = sub.Close()
		if err != nil {
			return err
		}
	}
	return root.Remove(name)
}

// owner is the non-root user that must own the restored files.
type owner struct{ uid, gid int }

func (o *owner) chown(root *os.Root, name string) error {
	if o == nil {
		return nil
	}
	return root.Chown(name, o.uid, o.gid)
}

// chownPath hands a directory the supervisor itself just created to the user.
func (o *owner) chownPath(path string) error {
	if o == nil {
		return nil
	}
	return os.Chown(path, o.uid, o.gid)
}

func (o *owner) lchown(root *os.Root, name string) error {
	if o == nil {
		return nil
	}
	return root.Lchown(name, o.uid, o.gid)
}

// restoreTemplate copies the read-only template tree into dst, preserving
// file modes and symlinks. owner, when set, is applied to every copied entry.
// The template is read and dst is written through os.Root handles, so neither
// a link in the template nor one planted in dst can redirect a copy.
func restoreTemplate(template, dst string, own *owner) error {
	if template == "" {
		return nil
	}
	src, err := os.OpenRoot(template)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirs []dirMode
	err = fs.WalkDir(src.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := src.Readlink(rel)
			if err != nil {
				return err
			}
			if err := out.Symlink(link, rel); err != nil {
				return err
			}
			return own.lchown(out, rel)
		case d.IsDir():
			// Created writable so children can be added; the final mode is
			// applied once the directory is fully populated.
			if err := out.Mkdir(rel, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMode{rel, info.Mode().Perm()})
			return own.chown(out, rel)
		case info.Mode().IsRegular():
			return copyFile(src, out, rel, info.Mode().Perm(), own)
		default:
			return fmt.Errorf("template entry %s has unsupported type %v", rel, d.Type())
		}
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := out.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst *os.Root, rel string, mode fs.FileMode, own *owner) error {
	in, err := src.Open(rel)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dst.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := own.chown(dst, rel); err != nil {
		return err
	}
	return dst.Chmod(rel, mode)
}
