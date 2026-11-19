// Package store implements a POSIX-style file system on top of a SQL database.
//
// Every file system operation runs as exactly one database transaction, so the
// file system is always in a consistent state: there is no journal to replay
// and no fsck needed after a crash beyond dropping files that were unlinked
// while still open.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

const (
	// RootIno is the inode number of the root directory.
	RootIno = 1
	// DefaultBlockSize is the size of a data block row unless mkfs says otherwise.
	DefaultBlockSize = 32 << 10
	// MaxNameLen is the longest file name, in bytes.
	MaxNameLen = 255
	// MaxFileSize is the largest file the store accepts.
	MaxFileSize = 1 << 50

	// Schema versions. Version 2 keeps file contents in backing files next
	// to the database (see FormatOptions.DataFiles); a build that only knows
	// version 1 must not mount it, since it would see every file as empty.
	schemaVersion      = "1"
	schemaVersionFiles = "2"
	// pgLockKey identifies the single-mount advisory lock ("tuplefs").
	pgLockKey = 0x7475706c656673
	// pgTreeLockKey serializes directory moves between parents.
	pgTreeLockKey = pgLockKey + 1
)

// SyncMode says when committed operations are forced to stable storage.
type SyncMode int

const (
	// SyncOnFsync makes every operation atomic and crash-consistent, but only
	// forces it to disk when the application calls fsync. This is what local
	// file systems do.
	SyncOnFsync SyncMode = iota
	// SyncAlways forces every operation to disk before it returns.
	SyncAlways
)

type dialect int

const (
	sqliteDialect dialect = iota
	postgresDialect
)

// Store is a file system stored in a database.
type Store struct {
	dialect   dialect
	path      string  // SQLite file, empty on PostgreSQL
	wdb       *sql.DB // write transactions
	rdb       *sql.DB // read transactions (same pool as wdb on Postgres)
	wmu       sync.Mutex
	sync      SyncMode
	blockSize int64
	dataFiles bool   // file contents live in backing files, not in blocks
	uuid      string // identifies the file system to its backing directory

	// The two reads behind almost every request, prepared once.
	getAttrStmt, lookupStmt *sql.Stmt

	onDelete func(ino uint64)
	deleted  sync.Map // *sql.Tx -> []uint64, inodes deleted by a transaction in flight
	rebound  sync.Map // query -> query with $n placeholders

	lockFile *os.File
	lockConn *sql.Conn
}

// Open connects to the database named by dsn. A dsn starting with postgres://
// or postgresql:// selects PostgreSQL; anything else is the path of a SQLite
// file, optionally prefixed with sqlite://.
func Open(dsn string, mode SyncMode) (*Store, error) {
	s := &Store{sync: mode}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		s.dialect = postgresDialect
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(16)
		s.wdb, s.rdb = db, db
	} else {
		path := strings.TrimPrefix(strings.TrimPrefix(dsn, "sqlite://"), "sqlite:")
		if path == "" {
			return nil, errors.New("empty database path")
		}
		s.path = path
		synchronous := "NORMAL"
		if mode == SyncAlways {
			synchronous = "FULL"
		}
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(30000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous("+synchronous+")")
		// SQLite URI file names: only these characters need escaping.
		base := "file:" + strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
		// One writer connection that takes the write lock up front, and a
		// pool of readers that never block behind it (WAL).
		wdb, err := sql.Open("sqlite", base+"?"+q.Encode()+"&_txlock=immediate")
		if err != nil {
			return nil, err
		}
		wdb.SetMaxOpenConns(1)
		rdb, err := sql.Open("sqlite", base+"?"+q.Encode())
		if err != nil {
			wdb.Close()
			return nil, err
		}
		rdb.SetMaxOpenConns(8)
		s.wdb, s.rdb = wdb, rdb
	}
	if err := s.wdb.Ping(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the mount lock and the database connections.
func (s *Store) Close() error {
	if s.lockConn != nil {
		s.lockConn.Close()
	}
	if s.lockFile != nil {
		s.lockFile.Close()
	}
	err := s.wdb.Close()
	if s.rdb != s.wdb {
		s.rdb.Close()
	}
	return err
}

// Path returns the SQLite file, or "" on PostgreSQL.
func (s *Store) Path() string { return s.path }

// BlockSize returns the data block size chosen at mkfs time.
func (s *Store) BlockSize() int64 { return s.blockSize }

// Backend names the database engine, for logs.
func (s *Store) Backend() string {
	if s.dialect == postgresDialect {
		return "postgres"
	}
	return "sqlite"
}

func (s *Store) schema() []string {
	blob, serial, withoutRowid, text := "BLOB", "INTEGER PRIMARY KEY AUTOINCREMENT", " WITHOUT ROWID", "TEXT"
	if s.dialect == postgresDialect {
		blob, serial, withoutRowid, text = "BYTEA", "BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY", "", `TEXT COLLATE "C"`
	}
	return []string{
		`CREATE TABLE meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE inodes (
			ino      ` + serial + `,
			mode     BIGINT NOT NULL,
			uid      BIGINT NOT NULL,
			gid      BIGINT NOT NULL,
			nlink    BIGINT NOT NULL,
			size     BIGINT NOT NULL DEFAULT 0,
			rdev     BIGINT NOT NULL DEFAULT 0,
			atime_ns BIGINT NOT NULL,
			mtime_ns BIGINT NOT NULL,
			ctime_ns BIGINT NOT NULL,
			target   ` + text + `
		)`,
		`CREATE TABLE dirents (
			parent BIGINT NOT NULL,
			name   ` + text + ` NOT NULL,
			ino    BIGINT NOT NULL,
			PRIMARY KEY (parent, name)
		)` + withoutRowid,
		`CREATE INDEX dirents_ino ON dirents (ino)`,
		`CREATE TABLE blocks (
			ino  BIGINT NOT NULL,
			idx  BIGINT NOT NULL,
			data ` + blob + ` NOT NULL,
			PRIMARY KEY (ino, idx)
		)`,
		`CREATE TABLE xattrs (
			ino   BIGINT NOT NULL,
			name  ` + text + ` NOT NULL,
			value ` + blob + ` NOT NULL,
			PRIMARY KEY (ino, name)
		)` + withoutRowid,
		`CREATE INDEX inodes_orphans ON inodes (nlink) WHERE nlink = 0`,
	}
}

// Formatted reports whether the database already holds a file system.
func (s *Store) Formatted(ctx context.Context) (bool, error) {
	q := `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'inodes'`
	if s.dialect == postgresDialect {
		q = `SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'inodes'`
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// Format creates an empty file system whose root directory has the given
// owner and mode. It fails if the database already holds one.
func (s *Store) Format(ctx context.Context, blockSize int64, uid, gid, mode uint32) error {
	return s.FormatWith(ctx, FormatOptions{BlockSize: blockSize, Uid: uid, Gid: gid, Mode: mode})
}

// FormatOptions describes a new file system.
type FormatOptions struct {
	BlockSize int64
	Uid, Gid  uint32 // owner of the root directory
	Mode      uint32 // permission bits of the root directory
	// DataFiles keeps the contents of regular files in backing files outside
	// the database. Metadata stays transactional; data is read and written at
	// the speed of the underlying file system and with its guarantees.
	DataFiles bool
	// UUID identifies the file system to its backing directory. The caller
	// picks it so that the directory can be prepared before the database.
	UUID string
}

// NewUUID returns a random identifier for FormatOptions.UUID.
func NewUUID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// FormatWith creates an empty file system. It fails if the database already
// holds one.
func (s *Store) FormatWith(ctx context.Context, o FormatOptions) error {
	blockSize, uid, gid, mode := o.BlockSize, o.Uid, o.Gid, o.Mode
	if blockSize == 0 {
		blockSize = DefaultBlockSize
	}
	if blockSize < 512 || blockSize > 4<<20 || blockSize&(blockSize-1) != 0 {
		return fmt.Errorf("block size %d must be a power of two between 512 and 4MiB", blockSize)
	}
	if ok, err := s.Formatted(ctx); err != nil {
		return err
	} else if ok {
		return errors.New("database already contains a file system")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, stmt := range s.schema() {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		now := time.Now().UnixNano()
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO inodes (ino, mode, uid, gid, nlink, atime_ns, mtime_ns, ctime_ns) VALUES (?, ?, ?, ?, 2, ?, ?, ?)`),
			RootIno, int64(syscall.S_IFDIR|mode&0o7777), int64(uid), int64(gid), now, now, now); err != nil {
			return err
		}
		if s.dialect == postgresDialect {
			// The explicit ino above does not advance the identity sequence.
			if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('inodes', 'ino'), 1)`); err != nil {
				return err
			}
		}
		meta := map[string]string{
			"version":    schemaVersion,
			"block_size": strconv.FormatInt(blockSize, 10),
			"created":    time.Now().UTC().Format(time.RFC3339),
			"sync_epoch": "0",
		}
		if o.DataFiles {
			uuid := o.UUID
			if uuid == "" {
				var err error
				if uuid, err = NewUUID(); err != nil {
					return err
				}
			}
			meta["version"], meta["data"], meta["uuid"], meta["clean"] = schemaVersionFiles, "files", uuid, "1"
		}
		for k, v := range meta {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO meta (key, value) VALUES (?, ?)`), k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Load reads the file system parameters. It must be called before any
// operation other than Format and Formatted.
func (s *Store) Load(ctx context.Context) error {
	var version, bs string
	err := s.rdb.QueryRowContext(ctx, s.q(`SELECT value FROM meta WHERE key = ?`), "version").Scan(&version)
	if err != nil {
		return fmt.Errorf("not a tuplefs database: %w", err)
	}
	switch version {
	case schemaVersion:
	case schemaVersionFiles:
		s.dataFiles = true
		if err := s.rdb.QueryRowContext(ctx, s.q(`SELECT value FROM meta WHERE key = ?`), "uuid").Scan(&s.uuid); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported schema version %s (this build reads versions %s and %s)", version, schemaVersion, schemaVersionFiles)
	}
	if err := s.rdb.QueryRowContext(ctx, s.q(`SELECT value FROM meta WHERE key = ?`), "block_size").Scan(&bs); err != nil {
		return err
	}
	if s.blockSize, err = strconv.ParseInt(bs, 10, 64); err != nil {
		return err
	}
	if s.getAttrStmt, err = s.rdb.PrepareContext(ctx, s.q(`SELECT `+attrCols+` FROM inodes WHERE ino = ?`)); err != nil {
		return err
	}
	s.lookupStmt, err = s.rdb.PrepareContext(ctx, s.q(`SELECT `+attrCols+` FROM inodes WHERE ino = (SELECT ino FROM dirents WHERE parent = ? AND name = ?)`))
	return err
}

// LockMount takes the lock that keeps a database from being mounted twice.
// The kernel caches what a mount tells it and each mount tracks its own open
// files, so two live mounts of one database would disagree.
func (s *Store) LockMount(ctx context.Context) error {
	if s.dialect == postgresDialect {
		conn, err := s.wdb.Conn(ctx)
		if err != nil {
			return err
		}
		var ok bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, int64(pgLockKey)).Scan(&ok); err != nil {
			conn.Close()
			return err
		}
		if !ok {
			conn.Close()
			return errors.New("file system is already mounted (advisory lock held by another session)")
		}
		s.lockConn = conn
		return nil
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return errors.New("file system is already mounted (" + s.path + ".lock is held)")
	}
	s.lockFile = f
	return nil
}

// q rewrites ? placeholders to $1, $2, ... for PostgreSQL.
func (s *Store) q(query string) string {
	if s.dialect != postgresDialect {
		return query
	}
	if v, ok := s.rebound.Load(query); ok {
		return v.(string)
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	out := b.String()
	s.rebound.Store(query, out)
	return out
}

// retryable reports whether a PostgreSQL error means the transaction lost a
// conflict and should simply run again.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40P01", "40001": // deadlock victim, serialization failure
			return true
		}
	}
	return false
}

// write runs fn in a read-write transaction. fn may run more than once, so it
// must not have effects outside the transaction.
//
// SQLite has a single writer, so write transactions simply run one at a time.
// On PostgreSQL they run concurrently at READ COMMITTED, and each operation
// first locks the inode rows it depends on (see lock), the way a kernel file
// system locks inodes. Lock order is not global, so PostgreSQL may pick a
// deadlock victim; that transaction runs again.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.dialect == sqliteDialect {
		// Queue here instead of spinning on SQLITE_BUSY.
		s.wmu.Lock()
		defer s.wmu.Unlock()
		return s.runTx(ctx, s.wdb, nil, fn)
	}
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		err = s.runTx(ctx, s.wdb, nil, func(tx *sql.Tx) error {
			if s.sync == SyncOnFsync {
				if _, err := tx.ExecContext(ctx, `SET LOCAL synchronous_commit = off`); err != nil {
					return err
				}
			}
			return fn(tx)
		})
		if !retryable(err) {
			return err
		}
	}
	return err
}

// lock takes the row locks of the given inodes for the rest of the
// transaction. Holding a directory's lock freezes its entries and link count;
// holding a file's lock freezes its size, data blocks and link count. Every
// operation locks before it reads what it is about to change. It is a no-op on
// SQLite, where the whole transaction is already exclusive.
func (s *Store) lock(ctx context.Context, tx *sql.Tx, inos ...uint64) error {
	if s.dialect != postgresDialect {
		return nil
	}
	if len(inos) == 2 && inos[0] > inos[1] {
		inos[0], inos[1] = inos[1], inos[0]
	}
	for i, ino := range inos {
		if i > 0 && ino == inos[i-1] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM inodes WHERE ino = $1 FOR UPDATE`, int64(ino)); err != nil {
			return err
		}
	}
	return nil
}

// lockTree serializes operations that change the shape of the directory tree,
// so that the check against moving a directory into its own subtree cannot
// race with another move. It plays the role of the kernel's rename mutex.
func (s *Store) lockTree(ctx context.Context, tx *sql.Tx) error {
	if s.dialect != postgresDialect {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(pgTreeLockKey))
	return err
}

// read runs fn in a read-only transaction that sees one snapshot.
func (s *Store) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.dialect == sqliteDialect {
		return s.runTx(ctx, s.rdb, &sql.TxOptions{ReadOnly: true}, fn)
	}
	return s.runTx(ctx, s.rdb, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, fn)
}

func (s *Store) runTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		s.deleted.Delete(tx)
		tx.Rollback()
		return err
	}
	inos, _ := s.deleted.LoadAndDelete(tx)
	if err := tx.Commit(); err != nil {
		return err
	}
	// Only now is it certain the inodes are gone, so only now may anything
	// outside the database (their backing files) be removed.
	if inos != nil && s.onDelete != nil {
		for _, ino := range inos.([]uint64) {
			s.onDelete(ino)
		}
	}
	return nil
}

// Sync forces everything committed so far to stable storage. It is what
// fsync(2) on any file in the file system maps to.
func (s *Store) Sync(ctx context.Context) error {
	if s.sync == SyncAlways {
		return nil // every commit was already synced
	}
	const bump = `UPDATE meta SET value = CAST(CAST(value AS BIGINT) + 1 AS TEXT) WHERE key = 'sync_epoch'`
	if s.dialect == postgresDialect {
		// A commit with synchronous_commit on flushes the WAL up to its own
		// commit record, which covers every earlier asynchronous commit.
		return s.runTx(ctx, s.wdb, nil, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, bump)
			return err
		})
	}
	// In WAL mode a commit with synchronous=FULL fsyncs the WAL file, which
	// holds every earlier commit that has not been checkpointed.
	s.wmu.Lock()
	defer s.wmu.Unlock()
	conn, err := s.wdb.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA synchronous = FULL`); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, bump)
	if _, rerr := conn.ExecContext(ctx, `PRAGMA synchronous = NORMAL`); err == nil {
		err = rerr
	}
	return err
}

// Drop removes the file system from the database. It is meant for tests that
// reuse one PostgreSQL database.
func (s *Store) Drop(ctx context.Context) error {
	_, err := s.wdb.ExecContext(ctx, `DROP TABLE IF EXISTS meta, inodes, dirents, blocks, xattrs`)
	return err
}
