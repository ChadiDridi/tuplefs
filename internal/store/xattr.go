package store

import (
	"context"
	"database/sql"
	"errors"
	"syscall"
	"time"
	"unicode/utf8"
)

// Setxattr flags, as in setxattr(2).
const (
	XattrCreate  = 1
	XattrReplace = 2
)

// GetXattr returns the value of an extended attribute.
func (s *Store) GetXattr(ctx context.Context, ino uint64, name string) (value []byte, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, s.q(`SELECT value FROM xattrs WHERE ino = ? AND name = ?`), int64(ino), name).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return syscall.ENODATA
		}
		return err
	})
	return value, err
}

// SetXattr sets an extended attribute.
func (s *Store) SetXattr(ctx context.Context, ino uint64, name string, value []byte, flags uint32) error {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) {
		return syscall.EINVAL
	}
	if len(value) > 64<<10 {
		return syscall.E2BIG
	}
	if value == nil {
		value = []byte{}
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.lock(ctx, tx, ino); err != nil {
			return err
		}
		if _, err := s.getAttr(ctx, tx, ino); err != nil {
			return err
		}
		var one int
		err := tx.QueryRowContext(ctx, s.q(`SELECT 1 FROM xattrs WHERE ino = ? AND name = ?`), int64(ino), name).Scan(&one)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		switch {
		case exists && flags&XattrCreate != 0:
			return syscall.EEXIST
		case !exists && flags&XattrReplace != 0:
			return syscall.ENODATA
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO xattrs (ino, name, value) VALUES (?, ?, ?)
			ON CONFLICT (ino, name) DO UPDATE SET value = excluded.value`), int64(ino), name, value); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q(`UPDATE inodes SET ctime_ns = ? WHERE ino = ?`), time.Now().UnixNano(), int64(ino))
		return err
	})
}

// ListXattr returns the names of all extended attributes of an inode.
func (s *Store) ListXattr(ctx context.Context, ino uint64) (names []string, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		names = names[:0]
		rows, err := tx.QueryContext(ctx, s.q(`SELECT name FROM xattrs WHERE ino = ? ORDER BY name`), int64(ino))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return rows.Err()
	})
	return names, err
}

// RemoveXattr deletes an extended attribute.
func (s *Store) RemoveXattr(ctx context.Context, ino uint64, name string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.lock(ctx, tx, ino); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM xattrs WHERE ino = ? AND name = ?`), int64(ino), name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return syscall.ENODATA
		}
		_, err = tx.ExecContext(ctx, s.q(`UPDATE inodes SET ctime_ns = ? WHERE ino = ?`), time.Now().UnixNano(), int64(ino))
		return err
	})
}
