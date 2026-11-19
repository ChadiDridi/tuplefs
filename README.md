# tuplefs

[![CI](https://github.com/ChadiDridi/tuplefs/actions/workflows/ci.yml/badge.svg)](https://github.com/ChadiDridi/tuplefs/actions/workflows/ci.yml)

**A POSIX file system kept in a SQL database.** Files and directories are rows, and by default so are the contents of files. Every metadata operation is exactly one database transaction; in the default layout every write is too. Mount it with FUSE and use it like any other directory; open the same database with `sqlite3` or `psql` and query it like any other tables.

```console
$ tuplefs mount fs.db /mnt/data &
tuplefs: database was empty, created a new file system
tuplefs: fs.db mounted on /mnt/data (sqlite backend, sync=fsync)

$ cp -r ~/src/project /mnt/data/                    # it is a normal directory
$ sqlite3 fs.db "SELECT count(*) FROM inodes"       # and a normal database
```

It runs on SQLite (one file, no server) or PostgreSQL, in one of two layouts:

- **Everything in the database** (default). File contents are rows too, so data and metadata change in the same transaction.
- **Hybrid** (`mkfs -data files`). Metadata stays in the database; the contents of regular files live in ordinary backing files next to it. Large-file reads and writes then run at the speed of the disk's own file system. See [Two layouts](#two-layouts).

## Why

Mike Stonebraker has argued for years that operating system services should be built on a database. In the [DBOS paper](https://www.vldb.org/pvldb/vol15/p21-skiadopoulos.pdf) (Skiadopoulos et al., PVLDB 15(1)) and again on [The Peterman Pod](https://www.developing.dev/i/194713116/1555-one-size-fits-none), he makes the case for a file system written on top of a DBMS: transactions, recovery and high availability are solved once, in the database, and everything above gets them for free.

tuplefs is a small, working version of the file system half of that idea, so the claim can be tried instead of debated. It is an independent project, not affiliated with DBOS.

What the database gives you here (the last column says what changes in the [hybrid layout](#two-layouts), where file contents live outside the database):

| | Default layout | Hybrid layout |
|---|---|---|
| **Atomic operations** | `rename`, `write`, `truncate`, `unlink` either happen completely or not at all. There is no window where a rename has removed the old name but not added the new one. | Same for the namespace: `rename`, `link`, `unlink`, `mkdir`, `chmod`. A `write` or `truncate` is not atomic with its metadata. |
| **No journal, no repair** | After a crash the database rolls back the transaction that was in flight. The file system is consistent by construction; `tuplefs fsck` exists to prove that, not to fix anything. | Same for metadata. The next mount also brings recorded sizes in line with the backing files and removes leftovers. |
| **SQL over your files** | Find the biggest files, total a directory tree, or look for duplicate blocks with a query. No `find`, no walking the tree. | Same, except for queries over file contents. |
| **Consistent live backups** | `sqlite3 fs.db ".backup snap.db"` or `pg_dump` takes a snapshot of a mounted file system at one point in time. | The snapshot holds metadata only. Back up the data directory with it, unmounted or from a storage snapshot of both. |
| **Replication and failover** | On PostgreSQL the file system is as available as the database. Promote a replica, mount it there. (This comes from PostgreSQL; this repository does not test failover.) | Replication covers metadata only. File contents need their own. |

What it does **not** give you is speed on metadata. Creating, listing and deleting files is slower than on a kernel file system in both layouts. Large-file throughput is slow in the default layout and matches ext4 in the hybrid one. See [Performance](#performance).

## Install

Needs Linux with FUSE (`fusermount` or `fusermount3` on the PATH) and Go 1.26 or newer. No C compiler is needed.

```sh
go install github.com/ChadiDridi/tuplefs/cmd/tuplefs@latest
```

## Use

```sh
tuplefs mount fs.db /mnt/data                    # SQLite; creates the file system if fs.db is empty
tuplefs mount postgres://user:pass@host/db /mnt/data
fusermount -u /mnt/data                          # or Ctrl-C in the mount process

tuplefs mkfs -block-size 65536 fs.db             # optional: choose the block size up front
tuplefs mkfs -data files fs.db                   # hybrid layout: file contents in fs.db.data/
tuplefs fsck fs.db                               # verify every invariant, read-only, works while mounted
tuplefs info fs.db
```

Mount flags:

- `-sync fsync` (default): every operation is atomic and survives a crash of tuplefs, and is forced to disk when a program calls `fsync`. This is what local file systems do.
- `-sync always`: every operation is forced to disk before it returns.
- `-allow-other`, `-ro`, `-debug`.
- `-data-dir DIR`: where the backing files of a hybrid file system are, if not `<database>.data`. Required with PostgreSQL. `mkfs` and `fsck` take it too.

A database can be mounted by one tuplefs process at a time. A second mount is refused.

## Two layouts

The layout is chosen at `mkfs` and recorded in the database.

| | Everything in the database | Hybrid (`-data files`) |
|---|---|---|
| Metadata operations (create, rename, unlink, chmod, ...) | One transaction each | One transaction each |
| File contents | Rows in `blocks` | One backing file per inode in the data directory |
| A write and its metadata | Atomic together | Not atomic. A write is as crash-safe as the disk's file system makes it, like any ordinary file |
| Database backup or replication covers | Everything | Metadata only. The data directory needs its own |
| SQL over file contents | Yes | No. Sizes and names, yes |
| Large-file throughput | 4 to 13 times slower than ext4 | Same as ext4 with kernel passthrough, up to about 2 times slower without |

In the hybrid layout a backing file is named after its inode number, so `rename` and `link` never touch it. The backing file is the authority on a file's size and modification time. The copies in the `inodes` table, which queries read, are refreshed about once a second, on `fsync` and at unmount. If tuplefs dies, the next mount corrects them from the files on disk and removes backing files whose inode is gone.

**Kernel passthrough.** When the hybrid layout is mounted by root on a kernel with FUSE passthrough, the kernel reads and writes backing files directly and tuplefs is not involved in file I/O at all. CI checks this on Linux 7.0: after a write and a read, the count of I/O requests that reached tuplefs is zero. Mounted by an ordinary user, or on a kernel without passthrough, data still goes through tuplefs: one system call per request and no SQL.

## Query your file system

These ran against a mount holding two git checkouts. The schema is four tables: `inodes`, `dirents`, `blocks`, `xattrs`.

The five largest files, with full paths:

```sql
WITH RECURSIVE tree(ino, path) AS (
  SELECT 1, ''
  UNION ALL
  SELECT d.ino, tree.path || '/' || d.name FROM dirents d JOIN tree ON d.parent = tree.ino
)
SELECT path, size FROM tree JOIN inodes USING (ino)
WHERE mode / 4096 = 8 ORDER BY size DESC LIMIT 5;
```

```text
path                                       size
-----------------------------------------  -----
/waveform/testdata/cases/otlp/traces.json  40745
/onprem-lb/src/onprem_lb/config.py         19867
/onprem-lb/src/onprem_lb/http/proxy.py     17614
/waveform/internal/execute/execute.go      14034
/waveform/go.sum                           11559
```

Disk usage per top-level directory (`du`, as one query):

```sql
WITH RECURSIVE tree(top, ino) AS (
  SELECT name, ino FROM dirents WHERE parent = 1
  UNION ALL
  SELECT tree.top, d.ino FROM dirents d JOIN tree ON d.parent = tree.ino
)
SELECT top, count(*) AS files, sum(size) AS bytes
FROM tree JOIN inodes USING (ino) WHERE mode / 4096 = 8 GROUP BY top ORDER BY bytes DESC;
```

```text
top           files  bytes
------------  -----  ------
waveform      292    466997
onprem-lb     143    342418
LICENSE.copy  1      11358
```

Duplicate data blocks, across all files (default layout, where file contents are rows):

```sql
SELECT count(*) AS copies, sum(length(data)) AS wasted_bytes
FROM blocks b WHERE EXISTS (
  SELECT 1 FROM blocks o WHERE o.data = b.data AND (o.ino, o.idx) < (b.ino, b.idx));
```

```text
copies  wasted_bytes
------  ------------
27      52442
```

Read-only queries are always safe, also while mounted. Do not write to the tables of a mounted file system: the kernel caches what tuplefs told it.

## How it works

```text
 application ── syscall ──> kernel VFS ── FUSE ──> tuplefs ── one transaction ──> SQLite / PostgreSQL
```

| Table | Row | Key |
|---|---|---|
| `inodes` | one file, directory, symlink or device: mode, owner, size, link count, timestamps | `ino` |
| `dirents` | one name in a directory | `(parent, name)` |
| `blocks` | one fixed-size piece of a file's data (32 KiB by default) | `(ino, idx)` |
| `xattrs` | one extended attribute | `(ino, name)` |

- **One metadata operation, one transaction.** `rename` over an existing file removes the old directory entry, repoints the new one, drops the replaced file's link count, deletes its data if that was the last link, and updates both directories' timestamps, all in a single commit.
- **Sparse files are free.** A block with no row reads as zeros. A 1 GiB file with 4 KiB written to it stores one row. In the hybrid layout, holes are whatever the backing file system makes of them.
- **Deleting an open file** keeps the inode with a link count of zero until the last handle closes. If tuplefs dies first, the next mount deletes those inodes. That is the only cleanup a mount ever does.
- **fsync** maps to a synchronous commit, which flushes the database's write-ahead log and with it every earlier commit. In the hybrid layout it first fsyncs the backing file.
- **Concurrency.** SQLite has one writer, so write transactions queue; readers never wait. On PostgreSQL, transactions run concurrently and each operation first locks the inode rows it depends on (`SELECT ... FOR UPDATE`), the way a kernel file system locks inodes. Directory moves between parents take one extra lock so that two moves cannot build a loop.
- **Permissions** are enforced by the kernel from the stored mode bits (`default_permissions`).

The file system itself is about 2,700 lines of Go. Journaling, recovery, locking and caching of pages are the database's job.

## What is supported

| Supported | Not supported |
|---|---|
| Regular files, directories, symlinks, hard links, FIFOs, sockets | POSIX ACLs (permission bits only) |
| `rename`, including `RENAME_NOREPLACE` and `RENAME_EXCHANGE` | `O_TMPFILE` |
| Sparse files, `truncate`, `fallocate` (extend, punch hole) | File names that are not valid UTF-8 (rejected with `EILSEQ`, since names are SQL text) |
| Extended attributes | Updating atime on read (behaves like `noatime`) |
| Unlink while open, `fsync`, `flock`/`fcntl` locks (handled by the kernel) | Mounting one database from two machines at once |
| `mmap`, including shared writable mappings | In the default layout, `st_blocks` reflects file size, not allocated blocks |

## Performance

Measured with [bench/bench.sh](bench/bench.sh), default settings, `-sync fsync`, SQLite. Each table compares against ext4 on the same machine and disk.

**GitHub-hosted runner, Linux 7.0, mounted by root, so the hybrid layout uses kernel passthrough.** This is the [benchmark job](.github/workflows/ci.yml) in CI.

| Workload | ext4 | In the database | Hybrid, passthrough |
|---|---:|---:|---:|
| Write 256 MiB sequentially, then fsync | 181 MiB/s | 42 MiB/s | 205 MiB/s |
| Read 256 MiB sequentially | 9,797 MiB/s | 6,345 MiB/s | 9,027 MiB/s |
| Overwrite 2,000 × 4 KiB, then fsync | 48,294 /s | 4,503 /s | 5,478 /s |
| Create 5,000 files of 4 KiB | 1,044 /s | 687 /s | 737 /s |
| `ls -l` of 5,000 files | 132,119 /s | 9,073 /s | 8,561 /s |
| Read 5,000 files | 74,679 /s | 6,093 /s | 12,679 /s |
| Delete 5,000 files | 4,725 /s | 1,135 /s | 1,331 /s |

**Laptop (Intel i7-1355U, NVMe), Linux 6.8, mounted by an ordinary user, which rules out passthrough.**

| Workload | ext4 | In the database | Hybrid, no passthrough |
|---|---:|---:|---:|
| Write 256 MiB sequentially, then fsync | 495 MiB/s | 84 MiB/s | 430 MiB/s |
| Read 256 MiB sequentially | 4,404 MiB/s | 335 MiB/s | 2,318 MiB/s |
| Overwrite 2,000 × 4 KiB, then fsync | 140,772 /s | 2,640 /s | 11,283 /s |
| Create 5,000 files of 4 KiB | 18,220 /s | 738 /s | 1,318 /s |
| `ls -l` of 5,000 files | 106,380 /s | 9,760 /s | 8,961 /s |
| Read 5,000 files | 48,770 /s | 5,065 /s | 10,682 /s |
| Delete 5,000 files | 55,738 /s | 1,540 /s | 1,981 /s |

Sequential reads on ext4, and on the runner, are served from the page cache. Single runs; expect noise of tens of percent. PostgreSQL as the backend is slower than SQLite on one machine, because every statement also crosses a socket: an earlier run of this benchmark in the default layout reached 50 MiB/s for the sequential write and 198 file creates per second.

What the numbers say:

- **Large files.** With passthrough, the hybrid layout reads and writes large files as fast as ext4, because it is ext4 doing the I/O. Without passthrough it is within about a factor of two. The default layout is 4 to 13 times slower, since every write is a SQL transaction.
- **Metadata.** Creating, listing and deleting files is slower than ext4 in both layouts: from 1.4 times (creates on the runner, where ext4 itself is slow) to 36 times (deletes on the laptop). Each request crosses from the kernel to tuplefs and back, which on the laptop takes about 25 microseconds before any SQL runs, where ext4 needs a few microseconds in total. No database tuning removes that crossing.
- **Small overwrites followed by fsync** stay slow in both layouts: fsync also commits the database synchronously.

This project does not reproduce the claim that a DBMS-backed file system beats the Linux file system. The DBOS paper measured a prototype on VoltDB, a multi-node main-memory DBMS, with in-memory operations only, and calls its own ext4 comparison "apples-to-oranges": with 40 threads its writes matched or beat ext4, while ext4 reads were considerably faster. tuplefs is a different design (FUSE, a disk-based database, one machine), and the numbers above are what that design costs.

Use the default layout where transactional, queryable, replicated contents matter more than throughput. Use the hybrid layout when you want transactional metadata and SQL over the tree, with ordinary disk speed for the data.

## Testing

- **Store tests** run every operation against SQLite and, with `TUPLEFS_TEST_POSTGRES` set, PostgreSQL. They include a randomized read/write/truncate/punch-hole test checked against an in-memory model, a stress test of colliding creates, links, renames and directory moves from eight goroutines, and a test that fires two opposing directory moves at the same instant. After each one, `fsck` must find nothing.
- **Mount tests** use a real FUSE mount: permissions, hard links, atomic rename, unlink while open, block-boundary writes, concurrent writers.
- **Crash test**: `SIGKILL` the tuplefs process while eight writers are busy, then check the database. Every invariant must hold, the fsynced file must be intact, and every file must be either empty or complete, never torn. This covers a crash of tuplefs; surviving power loss relies on the database's own durability.
- **Real workloads**, run by hand on a mount: `git clone` followed by `git fsck --full`, a tar round trip compared with `diff -r`, and `go build` of a project inside the mount.
- **POSIX conformance**: CI runs [pjdfstest](https://github.com/pjd/pjdfstest) against a mount as root on every push, once per layout. All 8,798 tests in its 238 files pass in both, and `fsck` is clean afterwards.
- **Both layouts**: the mount tests and the crash test run in each layout, on SQLite and PostgreSQL. One CI job runs them as root with passthrough required and fails if any file I/O reaches tuplefs.

```sh
go test ./...                                                   # SQLite; mount tests skip without /dev/fuse
TUPLEFS_TEST_POSTGRES=postgres://... go test -p 1 ./internal/...  # wipes the tuplefs tables in that database
```

## Limits

- One mount per database. Sharing a database between machines needs cache invalidation between mounts, which is not implemented.
- Default layout: whole blocks are rewritten, so changing one byte rewrites its 32 KiB block row, and `statfs` reports a fixed 1 TiB capacity (the real limit is the database's).
- Hybrid layout: while mounted, sizes in the `inodes` table can trail the files by about a second. Back up the database and the data directory together, with the file system unmounted or from a snapshot of both.
- Passthrough needs a mount by root and a kernel that has it. It was verified on Linux 7.0. The laptop numbers come from an unprivileged mount, where it is not available.
- Linux only.

## License

[Apache-2.0](LICENSE)
