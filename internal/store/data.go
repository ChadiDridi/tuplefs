package store

import (
	"context"
	"database/sql"
	"errors"
	"syscall"
	"time"
)

// Fallocate modes, as in fallocate(2).
const (
	FallocKeepSize  = 0x1
	FallocPunchHole = 0x2
)

// A file is a set of rows in blocks, keyed by (ino, idx). Block idx holds the
// bytes [idx*blockSize, (idx+1)*blockSize). A missing row is a hole and reads
// as zeros. A row may be shorter than the block size, in which case the rest of
// the block is zeros too. No row ever holds bytes past the file size.

func (s *Store) getBlock(ctx context.Context, tx *sql.Tx, ino uint64, idx int64) ([]byte, error) {
	var data []byte
	err := tx.QueryRowContext(ctx, s.q(`SELECT data FROM blocks WHERE ino = ? AND idx = ?`), int64(ino), idx).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return data, err
}

func (s *Store) putBlock(ctx context.Context, tx *sql.Tx, ino uint64, idx int64, data []byte) error {
	_, err := tx.ExecContext(ctx, s.q(`INSERT INTO blocks (ino, idx, data) VALUES (?, ?, ?)
		ON CONFLICT (ino, idx) DO UPDATE SET data = excluded.data`), int64(ino), idx, data)
	return err
}

// Read returns up to size bytes starting at off. It returns fewer bytes only
// at end of file.
func (s *Store) Read(ctx context.Context, ino uint64, off int64, size int) (buf []byte, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		var fsize int64
		err := tx.QueryRowContext(ctx, s.q(`SELECT size FROM inodes WHERE ino = ?`), int64(ino)).Scan(&fsize)
		if errors.Is(err, sql.ErrNoRows) {
			return syscall.ENOENT
		}
		if err != nil {
			return err
		}
		if off >= fsize || size <= 0 {
			buf = nil
			return nil
		}
		if int64(size) > fsize-off {
			size = int(fsize - off)
		}
		buf = make([]byte, size) // holes stay zero
		bs := s.blockSize
		rows, err := tx.QueryContext(ctx, s.q(`SELECT idx, data FROM blocks WHERE ino = ? AND idx >= ? AND idx <= ? ORDER BY idx`),
			int64(ino), off/bs, (off+int64(size)-1)/bs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var idx int64
			var data []byte
			if err := rows.Scan(&idx, &data); err != nil {
				return err
			}
			// Copy the part of this block that overlaps [off, off+size).
			blockOff := idx * bs
			from := max(off-blockOff, 0)
			if from < int64(len(data)) {
				copy(buf[blockOff+from-off:], data[from:])
			}
		}
		return rows.Err()
	})
	return buf, err
}

// Write stores data at off, growing the file if needed.
func (s *Store) Write(ctx context.Context, ino uint64, off int64, data []byte) (n int, err error) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if off+int64(len(data)) > MaxFileSize {
		return 0, syscall.EFBIG
	}
	if len(data) == 0 {
		return 0, nil
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var fsize int64
		err := tx.QueryRowContext(ctx, s.q(`SELECT size FROM inodes WHERE ino = ?`), int64(ino)).Scan(&fsize)
		if errors.Is(err, sql.ErrNoRows) {
			return syscall.ENOENT
		}
		if err != nil {
			return err
		}
		bs := s.blockSize
		for pos := off; pos < off+int64(len(data)); {
			idx, inBlock := pos/bs, pos%bs
			chunk := data[pos-off:]
			if int64(len(chunk)) > bs-inBlock {
				chunk = chunk[:bs-inBlock]
			}
			block := chunk
			if int64(len(chunk)) != bs {
				// Partial block: merge with what is already stored.
				old, err := s.getBlock(ctx, tx, ino, idx)
				if err != nil {
					return err
				}
				block = make([]byte, max(int64(len(old)), inBlock+int64(len(chunk))))
				copy(block, old)
				copy(block[inBlock:], chunk)
			}
			if err := s.putBlock(ctx, tx, ino, idx, block); err != nil {
				return err
			}
			pos += int64(len(chunk))
		}
		now := time.Now().UnixNano()
		_, err = tx.ExecContext(ctx, s.q(`UPDATE inodes SET size = ?, mtime_ns = ?, ctime_ns = ? WHERE ino = ?`),
			max(fsize, off+int64(len(data))), now, now, int64(ino))
		return err
	})
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// truncate drops the data past newSize. Growing a file needs no rows: the new
// range is a hole.
func (s *Store) truncate(ctx context.Context, tx *sql.Tx, ino uint64, oldSize, newSize uint64) error {
	if newSize >= oldSize {
		return nil
	}
	return s.zeroRange(ctx, tx, ino, int64(newSize), int64(oldSize))
}

// zeroRange makes [from, to) read as zeros by deleting whole blocks and
// clearing the partial blocks at both ends.
func (s *Store) zeroRange(ctx context.Context, tx *sql.Tx, ino uint64, from, to int64) error {
	if from >= to {
		return nil
	}
	bs := s.blockSize
	first, last := from/bs, (to-1)/bs
	firstFull, lastFull := first, last
	if from%bs != 0 {
		firstFull++
	}
	if to%bs != 0 {
		lastFull--
	}
	if firstFull <= lastFull {
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM blocks WHERE ino = ? AND idx >= ? AND idx <= ?`), int64(ino), firstFull, lastFull); err != nil {
			return err
		}
	}
	zero := func(idx, lo, hi int64) error { // zero bytes [lo, hi) of one block
		data, err := s.getBlock(ctx, tx, ino, idx)
		if err != nil || int64(len(data)) <= lo {
			return err
		}
		if hi >= int64(len(data)) {
			data = data[:lo] // nothing stored after the range: shorten the row
		} else {
			for i := lo; i < hi; i++ {
				data[i] = 0
			}
		}
		if len(data) == 0 {
			_, err = tx.ExecContext(ctx, s.q(`DELETE FROM blocks WHERE ino = ? AND idx = ?`), int64(ino), idx)
			return err
		}
		return s.putBlock(ctx, tx, ino, idx, data)
	}
	if first == last && (from%bs != 0 || to%bs != 0) {
		return zero(first, from%bs, (to-1)%bs+1)
	}
	if from%bs != 0 {
		if err := zero(first, from%bs, bs); err != nil {
			return err
		}
	}
	if to%bs != 0 {
		return zero(last, 0, to%bs)
	}
	return nil
}

// Fallocate implements the fallocate(2) modes that make sense for a store
// without preallocation: extending a file, and punching a hole.
func (s *Store) Fallocate(ctx context.Context, ino uint64, mode uint32, off, length int64) error {
	if off < 0 || length <= 0 {
		return syscall.EINVAL
	}
	if off+length > MaxFileSize {
		return syscall.EFBIG
	}
	switch mode {
	case 0, FallocKeepSize, FallocPunchHole | FallocKeepSize:
	default:
		return syscall.EOPNOTSUPP
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		a, err := s.getAttr(ctx, tx, ino)
		if err != nil {
			return err
		}
		if a.Mode&syscall.S_IFMT != syscall.S_IFREG {
			return syscall.ENODEV
		}
		now := time.Now().UnixNano()
		switch {
		case mode&FallocPunchHole != 0:
			if err := s.zeroRange(ctx, tx, ino, off, min(off+length, int64(a.Size))); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, s.q(`UPDATE inodes SET mtime_ns = ?, ctime_ns = ? WHERE ino = ?`), now, now, int64(ino))
		case mode == 0 && uint64(off+length) > a.Size:
			_, err = tx.ExecContext(ctx, s.q(`UPDATE inodes SET size = ?, mtime_ns = ?, ctime_ns = ? WHERE ino = ?`), off+length, now, now, int64(ino))
		}
		return err
	})
}
