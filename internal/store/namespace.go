package store

import (
	"context"
	"database/sql"
	"errors"
	"syscall"
	"time"
	"unicode/utf8"
)

// Attr is the metadata of one inode.
type Attr struct {
	Ino     uint64
	Mode    uint32 // file type and permission bits
	Uid     uint32
	Gid     uint32
	Nlink   uint32
	Size    uint64
	Rdev    uint32
	AtimeNs int64
	MtimeNs int64
	CtimeNs int64
}

// IsDir reports whether the inode is a directory.
func (a Attr) IsDir() bool { return a.Mode&syscall.S_IFMT == syscall.S_IFDIR }

// DirEntry is one name in a directory.
type DirEntry struct {
	Name string
	Ino  uint64
	Mode uint32
}

// Rename flags, as in renameat2(2).
const (
	RenameNoReplace = 1
	RenameExchange  = 2
)

const attrCols = `ino, mode, uid, gid, nlink, size, rdev, atime_ns, mtime_ns, ctime_ns`

type rowScanner interface{ Scan(dest ...any) error }

func scanAttr(row rowScanner) (Attr, error) {
	var a Attr
	var ino, mode, uid, gid, nlink, size, rdev int64
	err := row.Scan(&ino, &mode, &uid, &gid, &nlink, &size, &rdev, &a.AtimeNs, &a.MtimeNs, &a.CtimeNs)
	if errors.Is(err, sql.ErrNoRows) {
		return a, syscall.ENOENT
	}
	a.Ino, a.Mode, a.Uid, a.Gid = uint64(ino), uint32(mode), uint32(uid), uint32(gid)
	a.Nlink, a.Size, a.Rdev = uint32(nlink), uint64(size), uint32(rdev)
	return a, err
}

func (s *Store) getAttr(ctx context.Context, tx *sql.Tx, ino uint64) (Attr, error) {
	return scanAttr(tx.QueryRowContext(ctx, s.q(`SELECT `+attrCols+` FROM inodes WHERE ino = ?`), int64(ino)))
}

// getDir loads a directory that new entries may be added to.
func (s *Store) getDir(ctx context.Context, tx *sql.Tx, ino uint64) (Attr, error) {
	a, err := s.getAttr(ctx, tx, ino)
	if err != nil {
		return a, err
	}
	if !a.IsDir() {
		return a, syscall.ENOTDIR
	}
	return a, nil
}

// lookup returns the inode a name points to, or ENOENT.
func (s *Store) lookup(ctx context.Context, tx *sql.Tx, parent uint64, name string) (uint64, error) {
	var ino int64
	err := tx.QueryRowContext(ctx, s.q(`SELECT ino FROM dirents WHERE parent = ? AND name = ?`), int64(parent), name).Scan(&ino)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, syscall.ENOENT
	}
	return uint64(ino), err
}

func checkName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return syscall.EINVAL
	case len(name) > MaxNameLen:
		return syscall.ENAMETOOLONG
	case !utf8.ValidString(name):
		// Names are stored as SQL text so they can be queried.
		return syscall.EILSEQ
	}
	return nil
}

// touch sets mtime and ctime of a directory whose entries changed.
func (s *Store) touch(ctx context.Context, tx *sql.Tx, ino uint64, now int64) error {
	_, err := tx.ExecContext(ctx, s.q(`UPDATE inodes SET mtime_ns = ?, ctime_ns = ? WHERE ino = ?`), now, now, int64(ino))
	return err
}

func (s *Store) addNlink(ctx context.Context, tx *sql.Tx, ino uint64, delta int, now int64) error {
	_, err := tx.ExecContext(ctx, s.q(`UPDATE inodes SET nlink = nlink + ?, ctime_ns = ? WHERE ino = ?`), delta, now, int64(ino))
	return err
}

// GetAttr returns the metadata of an inode.
func (s *Store) GetAttr(ctx context.Context, ino uint64) (a Attr, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		a, err = s.getAttr(ctx, tx, ino)
		return err
	})
	return a, err
}

// Lookup resolves a name in a directory.
func (s *Store) Lookup(ctx context.Context, parent uint64, name string) (a Attr, err error) {
	if len(name) > MaxNameLen {
		return a, syscall.ENAMETOOLONG
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		a, err = scanAttr(tx.QueryRowContext(ctx, s.q(`SELECT `+attrCols+` FROM inodes WHERE ino = (SELECT ino FROM dirents WHERE parent = ? AND name = ?)`), int64(parent), name))
		return err
	})
	return a, err
}

// Mknod creates a file, directory, symlink, device, FIFO or socket, depending
// on the type bits in mode. target is the symlink target.
func (s *Store) Mknod(ctx context.Context, parent uint64, name string, mode, rdev, uid, gid uint32, target string) (a Attr, err error) {
	if err := checkName(name); err != nil {
		return a, err
	}
	isDir := mode&syscall.S_IFMT == syscall.S_IFDIR
	isLink := mode&syscall.S_IFMT == syscall.S_IFLNK
	if isLink && !utf8.ValidString(target) {
		return a, syscall.EILSEQ
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.lock(ctx, tx, parent); err != nil {
			return err
		}
		dir, err := s.getDir(ctx, tx, parent)
		if err != nil {
			return err
		}
		if _, err := s.lookup(ctx, tx, parent, name); err == nil {
			return syscall.EEXIST
		} else if err != syscall.ENOENT {
			return err
		}
		mode, gid := mode, gid
		if dir.Mode&syscall.S_ISGID != 0 {
			// BSD group semantics for setgid directories.
			gid = dir.Gid
			if isDir {
				mode |= syscall.S_ISGID
			}
		}
		now := time.Now().UnixNano()
		nlink, size := 1, 0
		var tgt any
		if isDir {
			nlink = 2
		}
		if isLink {
			size, tgt = len(target), target
		}
		var ino int64
		err = tx.QueryRowContext(ctx, s.q(`INSERT INTO inodes (mode, uid, gid, nlink, size, rdev, atime_ns, mtime_ns, ctime_ns, target)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING ino`),
			int64(mode), int64(uid), int64(gid), nlink, size, int64(rdev), now, now, now, tgt).Scan(&ino)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO dirents (parent, name, ino) VALUES (?, ?, ?)`), int64(parent), name, ino); err != nil {
			return err
		}
		if isDir {
			if err := s.addNlink(ctx, tx, parent, 1, now); err != nil {
				return err
			}
		}
		if err := s.touch(ctx, tx, parent, now); err != nil {
			return err
		}
		a = Attr{Ino: uint64(ino), Mode: mode, Uid: uid, Gid: gid, Nlink: uint32(nlink), Size: uint64(size), Rdev: rdev, AtimeNs: now, MtimeNs: now, CtimeNs: now}
		return nil
	})
	return a, err
}

// Link adds a second name for an existing non-directory.
func (s *Store) Link(ctx context.Context, ino, newParent uint64, newName string) (a Attr, err error) {
	if err := checkName(newName); err != nil {
		return a, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.lock(ctx, tx, newParent); err != nil {
			return err
		}
		if err := s.lock(ctx, tx, ino); err != nil {
			return err
		}
		if a, err = s.getAttr(ctx, tx, ino); err != nil {
			return err
		}
		if a.IsDir() {
			return syscall.EPERM
		}
		if a.Nlink == 0 {
			return syscall.ENOENT // already unlinked, only open handles keep it alive
		}
		if _, err := s.getDir(ctx, tx, newParent); err != nil {
			return err
		}
		if _, err := s.lookup(ctx, tx, newParent, newName); err == nil {
			return syscall.EEXIST
		} else if err != syscall.ENOENT {
			return err
		}
		now := time.Now().UnixNano()
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO dirents (parent, name, ino) VALUES (?, ?, ?)`), int64(newParent), newName, int64(ino)); err != nil {
			return err
		}
		if err := s.addNlink(ctx, tx, ino, 1, now); err != nil {
			return err
		}
		a.Nlink++
		a.CtimeNs = now
		return s.touch(ctx, tx, newParent, now)
	})
	return a, err
}

// StillOpen is asked, inside the transaction that removes the last name of a
// file, whether the file is still open. If it is, the inode and its data stay
// until the caller calls Reap after the last close. A nil StillOpen means no.
type StillOpen func(ino uint64) bool

// dropIfUnused deletes an inode that just lost its last name, unless it is
// still open.
func (s *Store) dropIfUnused(ctx context.Context, tx *sql.Tx, ino uint64, open StillOpen) error {
	if open != nil && open(ino) {
		return nil
	}
	return s.deleteInode(ctx, tx, ino)
}

func (s *Store) deleteInode(ctx context.Context, tx *sql.Tx, ino uint64) error {
	for _, q := range []string{`DELETE FROM blocks WHERE ino = ?`, `DELETE FROM xattrs WHERE ino = ?`, `DELETE FROM inodes WHERE ino = ?`} {
		if _, err := tx.ExecContext(ctx, s.q(q), int64(ino)); err != nil {
			return err
		}
	}
	return nil
}

// Unlink removes a name that is not a directory. It returns the inode and its
// remaining link count. A file whose count reaches zero is deleted in the same
// transaction unless open reports that it is still in use.
func (s *Store) Unlink(ctx context.Context, parent uint64, name string, open StillOpen) (ino uint64, nlink uint32, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.lock(ctx, tx, parent); err != nil {
			return err
		}
		if ino, err = s.lookup(ctx, tx, parent, name); err != nil {
			return err
		}
		if err := s.lock(ctx, tx, ino); err != nil {
			return err
		}
		a, err := s.getAttr(ctx, tx, ino)
		if err != nil {
			return err
		}
		if a.IsDir() {
			return syscall.EISDIR
		}
		now := time.Now().UnixNano()
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM dirents WHERE parent = ? AND name = ?`), int64(parent), name); err != nil {
			return err
		}
		if err := s.addNlink(ctx, tx, ino, -1, now); err != nil {
			return err
		}
		if nlink = a.Nlink - 1; nlink == 0 {
			if err := s.dropIfUnused(ctx, tx, ino, open); err != nil {
				return err
			}
		}
		return s.touch(ctx, tx, parent, now)
	})
	return ino, nlink, err
}

func (s *Store) dirEmpty(ctx context.Context, tx *sql.Tx, ino uint64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, s.q(`SELECT count(*) FROM (SELECT 1 FROM dirents WHERE parent = ? LIMIT 1) AS t`), int64(ino)).Scan(&n)
	return n == 0, err
}

// removeDir deletes an empty directory inode and fixes its parent's link count.
func (s *Store) removeDir(ctx context.Context, tx *sql.Tx, parent, ino uint64, now int64) error {
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM xattrs WHERE ino = ?`), int64(ino)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM inodes WHERE ino = ?`), int64(ino)); err != nil {
		return err
	}
	return s.addNlink(ctx, tx, parent, -1, now)
}

// Rmdir removes an empty directory.
func (s *Store) Rmdir(ctx context.Context, parent uint64, name string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.lock(ctx, tx, parent); err != nil {
			return err
		}
		ino, err := s.lookup(ctx, tx, parent, name)
		if err != nil {
			return err
		}
		if err := s.lock(ctx, tx, ino); err != nil {
			return err
		}
		a, err := s.getAttr(ctx, tx, ino)
		if err != nil {
			return err
		}
		if !a.IsDir() {
			return syscall.ENOTDIR
		}
		if empty, err := s.dirEmpty(ctx, tx, ino); err != nil {
			return err
		} else if !empty {
			return syscall.ENOTEMPTY
		}
		now := time.Now().UnixNano()
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM dirents WHERE parent = ? AND name = ?`), int64(parent), name); err != nil {
			return err
		}
		if err := s.removeDir(ctx, tx, parent, ino, now); err != nil {
			return err
		}
		return s.touch(ctx, tx, parent, now)
	})
}

// isAncestor reports whether dir anc is ino itself or one of its ancestors.
func (s *Store) isAncestor(ctx context.Context, tx *sql.Tx, anc, ino uint64) (bool, error) {
	for cur := ino; ; {
		if cur == anc {
			return true, nil
		}
		if cur == RootIno {
			return false, nil
		}
		var parent int64
		err := tx.QueryRowContext(ctx, s.q(`SELECT parent FROM dirents WHERE ino = ?`), int64(cur)).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // detached directory
		}
		if err != nil {
			return false, err
		}
		cur = uint64(parent)
	}
}

// Rename moves a name, atomically replacing the destination if it exists. If a
// file was replaced it returns that inode and its remaining link count; the
// replaced file is deleted under the same rule as in Unlink.
func (s *Store) Rename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string, flags uint32, open StillOpen) (replaced uint64, nlink uint32, err error) {
	if flags&^(RenameNoReplace|RenameExchange) != 0 || flags == RenameNoReplace|RenameExchange {
		return 0, 0, syscall.EINVAL
	}
	if err := checkName(newName); err != nil {
		return 0, 0, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		replaced, nlink = 0, 0
		// Both directories first, which freezes the two names, then the
		// inodes those names point to.
		if err := s.lock(ctx, tx, oldParent, newParent); err != nil {
			return err
		}
		srcIno, err := s.lookup(ctx, tx, oldParent, oldName)
		if err != nil {
			return err
		}
		if _, err := s.getDir(ctx, tx, newParent); err != nil {
			return err
		}
		dstIno, err := s.lookup(ctx, tx, newParent, newName)
		hasDst := err == nil
		if err != nil && err != syscall.ENOENT {
			return err
		}
		if hasDst {
			err = s.lock(ctx, tx, srcIno, dstIno)
		} else {
			err = s.lock(ctx, tx, srcIno)
		}
		if err != nil {
			return err
		}
		src, err := s.getAttr(ctx, tx, srcIno)
		if err != nil {
			return err
		}
		var dst Attr
		if hasDst {
			if dst, err = s.getAttr(ctx, tx, dstIno); err != nil {
				return err
			}
		}
		if oldParent != newParent && (src.IsDir() || dst.IsDir()) {
			if err := s.lockTree(ctx, tx); err != nil {
				return err
			}
		}
		now := time.Now().UnixNano()
		setDirent := func(parent uint64, name string, ino uint64) error {
			_, err := tx.ExecContext(ctx, s.q(`UPDATE dirents SET ino = ? WHERE parent = ? AND name = ?`), int64(ino), int64(parent), name)
			return err
		}
		// A directory must not move into itself or its own subtree.
		checkLoop := func(dir Attr, into uint64) error {
			if !dir.IsDir() || into == oldParent && into == newParent {
				return nil
			}
			if loop, err := s.isAncestor(ctx, tx, dir.Ino, into); err != nil {
				return err
			} else if loop {
				return syscall.EINVAL
			}
			return nil
		}

		if flags&RenameExchange != 0 {
			if !hasDst {
				return syscall.ENOENT
			}
			if srcIno == dstIno {
				return nil
			}
			if err := checkLoop(src, newParent); err != nil {
				return err
			}
			if err := checkLoop(dst, oldParent); err != nil {
				return err
			}
			if err := setDirent(oldParent, oldName, dstIno); err != nil {
				return err
			}
			if err := setDirent(newParent, newName, srcIno); err != nil {
				return err
			}
			if oldParent != newParent && src.IsDir() != dst.IsDir() {
				// Only one side is a directory: its ".." link changes parent.
				from, to := oldParent, newParent
				if dst.IsDir() {
					from, to = newParent, oldParent
				}
				if err := s.addNlink(ctx, tx, from, -1, now); err != nil {
					return err
				}
				if err := s.addNlink(ctx, tx, to, 1, now); err != nil {
					return err
				}
			}
			for _, ino := range []uint64{srcIno, dstIno} {
				if err := s.addNlink(ctx, tx, ino, 0, now); err != nil { // ctime only
					return err
				}
			}
			if err := s.touch(ctx, tx, oldParent, now); err != nil {
				return err
			}
			return s.touch(ctx, tx, newParent, now)
		}

		if hasDst {
			if srcIno == dstIno {
				return nil // two names of the same file: POSIX says do nothing
			}
			if flags&RenameNoReplace != 0 {
				return syscall.EEXIST
			}
			switch {
			case src.IsDir() && !dst.IsDir():
				return syscall.ENOTDIR
			case !src.IsDir() && dst.IsDir():
				return syscall.EISDIR
			}
		}
		if err := checkLoop(src, newParent); err != nil {
			return err
		}
		if hasDst {
			if dst.IsDir() {
				if empty, err := s.dirEmpty(ctx, tx, dstIno); err != nil {
					return err
				} else if !empty {
					return syscall.ENOTEMPTY
				}
				if err := s.removeDir(ctx, tx, newParent, dstIno, now); err != nil {
					return err
				}
			} else {
				if err := s.addNlink(ctx, tx, dstIno, -1, now); err != nil {
					return err
				}
				if replaced, nlink = dstIno, dst.Nlink-1; nlink == 0 {
					if err := s.dropIfUnused(ctx, tx, dstIno, open); err != nil {
						return err
					}
				}
			}
			if err := setDirent(newParent, newName, srcIno); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO dirents (parent, name, ino) VALUES (?, ?, ?)`), int64(newParent), newName, int64(srcIno)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM dirents WHERE parent = ? AND name = ?`), int64(oldParent), oldName); err != nil {
			return err
		}
		if src.IsDir() && oldParent != newParent {
			if err := s.addNlink(ctx, tx, oldParent, -1, now); err != nil {
				return err
			}
			if err := s.addNlink(ctx, tx, newParent, 1, now); err != nil {
				return err
			}
		}
		if err := s.addNlink(ctx, tx, srcIno, 0, now); err != nil { // ctime only
			return err
		}
		if err := s.touch(ctx, tx, oldParent, now); err != nil {
			return err
		}
		return s.touch(ctx, tx, newParent, now)
	})
	return replaced, nlink, err
}

// ReadDir lists a directory in name order.
func (s *Store) ReadDir(ctx context.Context, ino uint64) (entries []DirEntry, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		entries = entries[:0]
		rows, err := tx.QueryContext(ctx, s.q(`SELECT d.name, d.ino, i.mode FROM dirents d JOIN inodes i ON i.ino = d.ino WHERE d.parent = ? ORDER BY d.name`), int64(ino))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e DirEntry
			var eino, mode int64
			if err := rows.Scan(&e.Name, &eino, &mode); err != nil {
				return err
			}
			e.Ino, e.Mode = uint64(eino), uint32(mode)
			entries = append(entries, e)
		}
		return rows.Err()
	})
	return entries, err
}

// Readlink returns the target of a symbolic link.
func (s *Store) Readlink(ctx context.Context, ino uint64) (target string, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		var t sql.NullString
		err := tx.QueryRowContext(ctx, s.q(`SELECT target FROM inodes WHERE ino = ?`), int64(ino)).Scan(&t)
		if errors.Is(err, sql.ErrNoRows) {
			return syscall.ENOENT
		}
		if err == nil && !t.Valid {
			return syscall.EINVAL
		}
		target = t.String
		return err
	})
	return target, err
}

// SetAttrReq lists the attributes to change. Nil fields are left alone.
type SetAttrReq struct {
	Mode    *uint32 // permission bits only
	Uid     *uint32
	Gid     *uint32
	Size    *uint64
	AtimeNs *int64
	MtimeNs *int64
}

// SetAttr changes attributes of an inode: chmod, chown, truncate and utimens.
func (s *Store) SetAttr(ctx context.Context, ino uint64, req SetAttrReq) (a Attr, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.lock(ctx, tx, ino); err != nil {
			return err
		}
		if a, err = s.getAttr(ctx, tx, ino); err != nil {
			return err
		}
		now := time.Now().UnixNano()
		if req.Mode != nil {
			a.Mode = a.Mode&syscall.S_IFMT | *req.Mode&0o7777
		}
		if req.Uid != nil {
			a.Uid = *req.Uid
		}
		if req.Gid != nil {
			a.Gid = *req.Gid
		}
		if req.Size != nil {
			switch a.Mode & syscall.S_IFMT {
			case syscall.S_IFREG:
			case syscall.S_IFDIR:
				return syscall.EISDIR
			default:
				return syscall.EINVAL
			}
			if *req.Size > MaxFileSize {
				return syscall.EFBIG
			}
			if err := s.truncate(ctx, tx, ino, a.Size, *req.Size); err != nil {
				return err
			}
			a.Size, a.MtimeNs = *req.Size, now
		}
		if req.AtimeNs != nil {
			a.AtimeNs = *req.AtimeNs
		}
		if req.MtimeNs != nil {
			a.MtimeNs = *req.MtimeNs
		}
		a.CtimeNs = now
		_, err := tx.ExecContext(ctx, s.q(`UPDATE inodes SET mode = ?, uid = ?, gid = ?, size = ?, atime_ns = ?, mtime_ns = ?, ctime_ns = ? WHERE ino = ?`),
			int64(a.Mode), int64(a.Uid), int64(a.Gid), int64(a.Size), a.AtimeNs, a.MtimeNs, a.CtimeNs, int64(ino))
		return err
	})
	return a, err
}

// Reap deletes an inode and its data if it has no names left. Call it when the
// last open handle of a file that was unlinked while open is closed.
func (s *Store) Reap(ctx context.Context, ino uint64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.lock(ctx, tx, ino); err != nil {
			return err
		}
		a, err := s.getAttr(ctx, tx, ino)
		if err == syscall.ENOENT {
			return nil
		}
		if err != nil || a.Nlink != 0 {
			return err
		}
		return s.deleteInode(ctx, tx, ino)
	})
}

// ReapOrphans deletes every inode that has no names. A crash can leave such
// inodes behind when a file was unlinked while still open; this is the only
// cleanup a mount ever needs. It returns how many were removed.
func (s *Store) ReapOrphans(ctx context.Context) (n int64, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		const orphans = `(SELECT ino FROM inodes WHERE nlink = 0)`
		for _, q := range []string{`DELETE FROM blocks WHERE ino IN ` + orphans, `DELETE FROM xattrs WHERE ino IN ` + orphans} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM inodes WHERE nlink = 0`)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// Usage is what statfs(2) reports.
type Usage struct {
	Inodes     uint64
	DataBlocks uint64
}

// StatFS counts inodes and stored data blocks.
func (s *Store) StatFS(ctx context.Context) (u Usage, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		var inodes, blocks int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM inodes`).Scan(&inodes); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM blocks`).Scan(&blocks); err != nil {
			return err
		}
		u = Usage{Inodes: uint64(inodes), DataBlocks: uint64(blocks)}
		return nil
	})
	return u, err
}
