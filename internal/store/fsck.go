package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// Problem is one inconsistency found by Fsck.
type Problem struct {
	Check  string
	Detail string
}

// The file system invariants, each written as a query that returns the rows
// violating it. Because every operation is one transaction, none of these
// should ever return a row; fsck exists to prove that, not to repair anything.
var fsckChecks = []struct{ name, query string }{
	{"root directory is missing", `
		SELECT 'ino 1' WHERE NOT EXISTS (SELECT 1 FROM inodes WHERE ino = 1 AND mode / 4096 = 4)`},
	{"directory entry points to a missing inode", `
		SELECT 'parent ' || d.parent || ' name ' || d.name || ' -> ino ' || d.ino
		FROM dirents d LEFT JOIN inodes i ON i.ino = d.ino WHERE i.ino IS NULL`},
	{"directory entry lives in a missing or non-directory parent", `
		SELECT 'parent ' || d.parent || ' name ' || d.name
		FROM dirents d LEFT JOIN inodes p ON p.ino = d.parent WHERE p.ino IS NULL OR p.mode / 4096 <> 4`},
	{"file link count does not match its directory entries", `
		SELECT 'ino ' || i.ino || ' nlink ' || i.nlink || ' entries ' || count(d.ino)
		FROM inodes i LEFT JOIN dirents d ON d.ino = i.ino
		WHERE i.mode / 4096 <> 4 GROUP BY i.ino, i.nlink HAVING i.nlink <> count(d.ino)`},
	{"directory link count is not 2 + subdirectories", `
		SELECT 'ino ' || i.ino || ' nlink ' || i.nlink || ' subdirs ' || count(c.ino)
		FROM inodes i LEFT JOIN dirents d ON d.parent = i.ino LEFT JOIN inodes c ON c.ino = d.ino AND c.mode / 4096 = 4
		WHERE i.mode / 4096 = 4 GROUP BY i.ino, i.nlink HAVING i.nlink <> 2 + count(c.ino)`},
	{"directory has more than one name", `
		SELECT 'ino ' || d.ino || ' names ' || count(*)
		FROM dirents d JOIN inodes i ON i.ino = d.ino WHERE i.mode / 4096 = 4 GROUP BY d.ino HAVING count(*) > 1`},
	{"directory is not reachable from the root", `
		WITH RECURSIVE reach(ino) AS (
			SELECT CAST(1 AS BIGINT)
			UNION ALL
			SELECT d.ino FROM dirents d JOIN reach r ON d.parent = r.ino JOIN inodes i ON i.ino = d.ino WHERE i.mode / 4096 = 4
		)
		SELECT 'ino ' || i.ino FROM inodes i WHERE i.mode / 4096 = 4 AND i.ino NOT IN (SELECT ino FROM reach)`},
	{"data block belongs to a missing inode", `
		SELECT 'ino ' || b.ino || ' block ' || b.idx
		FROM blocks b LEFT JOIN inodes i ON i.ino = b.ino WHERE i.ino IS NULL`},
	{"data stored past the end of the file", `
		SELECT 'ino ' || b.ino || ' block ' || b.idx || ' size ' || i.size
		FROM blocks b JOIN inodes i ON i.ino = b.ino
		WHERE b.idx * %[1]d + length(b.data) > i.size`},
	{"data block is larger than the block size", `
		SELECT 'ino ' || ino || ' block ' || idx || ' bytes ' || length(data) FROM blocks WHERE length(data) > %[1]d OR idx < 0`},
	{"extended attribute belongs to a missing inode", `
		SELECT 'ino ' || x.ino || ' name ' || x.name
		FROM xattrs x LEFT JOIN inodes i ON i.ino = x.ino WHERE i.ino IS NULL`},
}

// Fsck checks every structural invariant of the file system in one snapshot
// and returns what it found, plus the number of orphans: files that were
// unlinked while open and are waiting to be reclaimed. Orphans are normal
// while the file system is mounted, and after a crash until the next mount.
func (s *Store) Fsck(ctx context.Context) (problems []Problem, orphans int64, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		problems = problems[:0]
		for _, c := range fsckChecks {
			query := c.query
			if containsVerb(query) {
				query = fmt.Sprintf(query, s.blockSize)
			}
			rows, err := tx.QueryContext(ctx, query+" LIMIT 20")
			if err != nil {
				return fmt.Errorf("%s: %w", c.name, err)
			}
			for rows.Next() {
				var detail string
				if err := rows.Scan(&detail); err != nil {
					rows.Close()
					return err
				}
				problems = append(problems, Problem{c.name, detail})
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM inodes WHERE nlink = 0`).Scan(&orphans)
	})
	return problems, orphans, err
}

func containsVerb(q string) bool {
	for i := 0; i+1 < len(q); i++ {
		if q[i] == '%' && q[i+1] == '[' {
			return true
		}
	}
	return false
}

// Info describes a file system for the info command.
type Info struct {
	Backend   string
	BlockSize int64
	Created   string
	Inodes    int64
	Dirs      int64
	Files     int64
	Bytes     int64 // sum of file sizes
	Stored    int64 // bytes held in data blocks
}

// Info summarizes the file system.
func (s *Store) Info(ctx context.Context) (info Info, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		info = Info{Backend: s.Backend(), BlockSize: s.blockSize}
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'created'`).Scan(&info.Created); err != nil && err != sql.ErrNoRows {
			return err
		}
		var bytes, stored sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT count(*),
				sum(CASE WHEN mode / 4096 = 4 THEN 1 ELSE 0 END),
				sum(CASE WHEN mode / 4096 = 8 THEN 1 ELSE 0 END),
				CAST(sum(CASE WHEN mode / 4096 = 8 THEN size ELSE 0 END) AS TEXT)
			FROM inodes`).Scan(&info.Inodes, &info.Dirs, &info.Files, &bytes)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT CAST(coalesce(sum(length(data)), 0) AS TEXT) FROM blocks`).Scan(&stored); err != nil {
			return err
		}
		info.Bytes, _ = strconv.ParseInt(bytes.String, 10, 64)
		info.Stored, _ = strconv.ParseInt(stored.String, 10, 64)
		return nil
	})
	return info, err
}
