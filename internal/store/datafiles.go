package store

import (
	"context"
	"database/sql"
)

// This file is the store's side of the hybrid layout, where the contents of
// regular files live in backing files outside the database. The store keeps
// owning all metadata. For those files the size and modification time in the
// inodes table are a copy, refreshed from the backing file, so that queries
// keep working; the backing file is the authority.

// DataFiles reports whether file contents live in backing files.
func (s *Store) DataFiles() bool { return s.dataFiles }

// UUID identifies the file system to its backing directory.
func (s *Store) UUID() string { return s.uuid }

// OnDelete registers a function that is called, after the transaction has
// committed, for every inode the store deletes. It is how backing files get
// removed without ever removing one whose inode survived a rollback.
func (s *Store) OnDelete(fn func(ino uint64)) { s.onDelete = fn }

// FileState is the part of a regular file's metadata that its backing file
// determines.
type FileState struct {
	Ino     uint64
	Size    int64
	MtimeNs int64
}

// UpdateFileStates copies sizes and modification times from backing files
// into the inodes table, in one transaction.
func (s *Store) UpdateFileStates(ctx context.Context, states []FileState) error {
	if len(states) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, st := range states {
			if _, err := tx.ExecContext(ctx, s.q(`UPDATE inodes SET size = ?, mtime_ns = ? WHERE ino = ?`), st.Size, st.MtimeNs, int64(st.Ino)); err != nil {
				return err
			}
		}
		return nil
	})
}

// RegularFiles returns the recorded state of every regular file.
func (s *Store) RegularFiles(ctx context.Context) (states []FileState, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		states = states[:0]
		rows, err := tx.QueryContext(ctx, `SELECT ino, size, mtime_ns FROM inodes WHERE mode / 4096 = 8 ORDER BY ino`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st FileState
			var ino int64
			if err := rows.Scan(&ino, &st.Size, &st.MtimeNs); err != nil {
				return err
			}
			st.Ino = uint64(ino)
			states = append(states, st)
		}
		return rows.Err()
	})
	return states, err
}

// Clean reports whether the last mount shut down cleanly, which means the
// copies of size and mtime in the inodes table match the backing files and no
// backing file is left without an inode.
func (s *Store) Clean(ctx context.Context) (bool, error) {
	var v string
	err := s.rdb.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'clean'`).Scan(&v)
	return v == "1", err
}

// SetClean records whether the file system is cleanly unmounted.
func (s *Store) SetClean(ctx context.Context, clean bool) error {
	v := "0"
	if clean {
		v = "1"
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.q(`UPDATE meta SET value = ? WHERE key = 'clean'`), v)
		return err
	})
}
