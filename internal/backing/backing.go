// Package backing stores the contents of regular files as ordinary files in a
// directory, one per inode, for the hybrid layout.
//
// A backing file is named after its inode number and nothing else, so renames
// and hard links never touch it, and it lives exactly as long as the inode
// row in the database. A missing backing file is an empty file: the row is
// created first and the file on first use.
package backing

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const idFile = "tuplefs-id"

// Dir is a directory of backing files.
type Dir struct {
	root string
}

// Create makes the backing directory of a new file system.
func Create(root, uuid string) (*Dir, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(root); err != nil {
		return nil, err
	} else if len(entries) > 0 {
		return nil, fmt.Errorf("data directory %s is not empty", root)
	}
	if err := os.WriteFile(filepath.Join(root, idFile), []byte(uuid+"\n"), 0o600); err != nil {
		return nil, err
	}
	return &Dir{root: root}, nil
}

// Open opens the backing directory of an existing file system and checks that
// it belongs to the database with this uuid.
func Open(root, uuid string) (*Dir, error) {
	id, err := os.ReadFile(filepath.Join(root, idFile))
	if err != nil {
		return nil, fmt.Errorf("%s is not the data directory of this file system: %w", root, err)
	}
	if strings.TrimSpace(string(id)) != uuid {
		return nil, fmt.Errorf("data directory %s belongs to a different file system", root)
	}
	return &Dir{root: root}, nil
}

// Root returns the directory path.
func (d *Dir) Root() string { return d.root }

// Path returns where an inode's backing file lives. Files are spread over 256
// subdirectories to keep each one small.
func (d *Dir) Path(ino uint64) string {
	return filepath.Join(d.root, fmt.Sprintf("%02x", ino&0xff), strconv.FormatUint(ino, 10))
}

// OpenFile opens the backing file of an inode for reading and writing,
// creating it if this is its first use.
func (d *Dir) OpenFile(ino uint64) (int, error) {
	path := d.Path(ino)
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC, 0o600)
	if err == syscall.ENOENT {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return -1, err
		}
		fd, err = syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC, 0o600)
	}
	return fd, err
}

// State is what the backing file says about a regular file.
type State struct {
	Size    int64
	Blocks  int64 // 512-byte units actually allocated
	MtimeNs int64
	CtimeNs int64
}

// Stat returns the state of an inode's backing file. A file that was never
// written has no backing file yet; ok is false and the file is empty.
func (d *Dir) Stat(ino uint64) (st State, ok bool, err error) {
	var s syscall.Stat_t
	if err := syscall.Stat(d.Path(ino), &s); err != nil {
		if err == syscall.ENOENT {
			return st, false, nil
		}
		return st, false, err
	}
	return State{Size: s.Size, Blocks: s.Blocks, MtimeNs: s.Mtim.Nano(), CtimeNs: s.Ctim.Nano()}, true, nil
}

// Truncate sets the size of an inode's backing file.
func (d *Dir) Truncate(ino uint64, size int64) error {
	fd, err := d.OpenFile(ino)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return syscall.Ftruncate(fd, size)
}

// SetMtime sets the modification time of an inode's backing file.
func (d *Dir) SetMtime(ino uint64, mtimeNs int64) error {
	fd, err := d.OpenFile(ino) // make sure it exists
	if err != nil {
		return err
	}
	syscall.Close(fd)
	ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, unix.NsecToTimespec(mtimeNs)}
	return unix.UtimesNanoAt(unix.AT_FDCWD, d.Path(ino), ts, 0)
}

// Remove deletes the backing file of an inode that no longer exists.
func (d *Dir) Remove(ino uint64) error {
	err := syscall.Unlink(d.Path(ino))
	if err == syscall.ENOENT {
		return nil
	}
	return err
}

// Inodes calls fn for every backing file in the directory, with the inode
// number its name claims and the path it was found at.
func (d *Dir) Inodes(fn func(ino uint64, path string) error) error {
	subdirs, err := os.ReadDir(d.root)
	if err != nil {
		return err
	}
	for _, sub := range subdirs {
		if !sub.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(d.root, sub.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			ino, err := strconv.ParseUint(f.Name(), 10, 64)
			if err != nil {
				return errors.New("unexpected file in data directory: " + filepath.Join(sub.Name(), f.Name()))
			}
			if err := fn(ino, filepath.Join(d.root, sub.Name(), f.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// Statfs reports the capacity of the file system holding the directory.
func (d *Dir) Statfs() (syscall.Statfs_t, error) {
	var st syscall.Statfs_t
	err := syscall.Statfs(d.root, &st)
	return st, err
}
