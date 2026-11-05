package overlay

import (
	"io"
	"os"
	"path/filepath"
)

// copyTree copies the file, symlink or directory hierarchy at src to the new
// path dst, preserving permissions and modification times. Special files are
// skipped.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch mode := info.Mode(); {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		return linkTime(dst, info.ModTime())
	case mode.IsDir():
		if err := os.Mkdir(dst, mode.Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		if err := os.Chmod(dst, mode.Perm()); err != nil {
			return err
		}
	case mode.IsRegular():
		if err := copyFile(src, dst, mode.Perm()); err != nil {
			return err
		}
	default:
		return nil
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// copyFile copies regular-file bytes and retains permissions despite the umask.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(perm); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
