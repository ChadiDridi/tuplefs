# tuplefs

**A POSIX file system whose entire state lives in a SQL database.** Files, directories and data blocks are rows. Every file system operation is exactly one database transaction. Mount it with FUSE and use it like any other directory; open the same database with `sqlite3` or `psql` and query it like any other tables.

```console
$ tuplefs mount fs.db /mnt/data &
tuplefs: database was empty, created a new file system
tuplefs: fs.db mounted on /mnt/data (sqlite backend, sync=fsync)

$ cp -r ~/src/project /mnt/data/                    # it is a normal directory
$ sqlite3 fs.db "SELECT count(*) FROM inodes"       # and a normal database
```

It runs on SQLite (one file, no server) or PostgreSQL.

## Why

Mike Stonebraker has argued for years that operating system services should be built on a database. In the [DBOS paper](https://www.vldb.org/pvldb/vol15/p21-skiadopoulos.pdf) (Skiadopoulos et al., PVLDB 15(1)) and again on The Peterman Pod, he makes the case for a file system written on top of a DBMS: transactions, recovery and high availability are solved once, in the database, and everything above gets them for free.

tuplefs is a small, working version of the file system half of that idea, so the claim can be tried instead of debated. It is an independent project, not affiliated with DBOS.

What the database gives you here:

| | |
|---|---|
| **Atomic operations** | `rename`, `write`, `truncate`, `unlink` either happen completely or not at all. There is no window where a rename has removed the old name but not added the new one. |
| **No journal, no repair** | After a crash the database rolls back the transaction that was in flight. The file system is consistent by construction; `tuplefs fsck` exists to prove that, not to fix anything. |
| **SQL over your files** | Find the biggest files, total a directory tree, or look for duplicate blocks with a query. No `find`, no walking the tree. |
| **Consistent live backups** | `sqlite3 fs.db ".backup snap.db"` or `pg_dump` takes a snapshot of a mounted file system at one point in time. |
| **Replication and failover** | On PostgreSQL the file system is as available as the database. Promote a replica, mount it there. (This comes from PostgreSQL; this repository does not test failover.) |

What it does **not** give you is speed. See [Performance](#performance): on one machine, a kernel file system is far faster.

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
tuplefs fsck fs.db                               # verify every invariant, read-only, works while mounted
tuplefs info fs.db
```

Mount flags:

- `-sync fsync` (default): every operation is atomic and survives a crash of tuplefs, and is forced to disk when a program calls `fsync`. This is what local file systems do.
- `-sync always`: every operation is forced to disk before it returns.
- `-allow-other`, `-ro`, `-debug`.

A database can be mounted by one tuplefs process at a time. A second mount is refused.

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

Duplicate data blocks, across all files:

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

- **One operation, one transaction.** `rename` over an existing file removes the old directory entry, repoints the new one, drops the replaced file's link count, deletes its data if that was the last link, and updates both directories' timestamps, all in a single commit.
- **Sparse files are free.** A block with no row reads as zeros. A 1 GiB file with 4 KiB written to it stores one row.
- **Deleting an open file** keeps the inode with a link count of zero until the last handle closes. If tuplefs dies first, the next mount deletes those inodes. That is the only cleanup a mount ever does.
- **fsync** maps to a synchronous commit, which flushes the database's write-ahead log and with it every earlier commit.
- **Concurrency.** SQLite has one writer, so write transactions queue; readers never wait. On PostgreSQL, transactions run concurrently and each operation first locks the inode rows it depends on (`SELECT ... FOR UPDATE`), the way a kernel file system locks inodes. Directory moves between parents take one extra lock so that two moves cannot build a loop.
- **Permissions** are enforced by the kernel from the stored mode bits (`default_permissions`).

The file system itself is about 2,100 lines of Go. Journaling, recovery, locking and caching of pages are the database's job.

## What is supported

| Supported | Not supported |
|---|---|
| Regular files, directories, symlinks, hard links, FIFOs, sockets | POSIX ACLs (permission bits only) |
| `rename`, including `RENAME_NOREPLACE` and `RENAME_EXCHANGE` | `O_TMPFILE` |
| Sparse files, `truncate`, `fallocate` (extend, punch hole) | File names that are not valid UTF-8 (rejected with `EILSEQ`, since names are SQL text) |
| Extended attributes | Updating atime on read (behaves like `noatime`) |
| Unlink while open, `fsync`, `flock`/`fcntl` locks (handled by the kernel) | Mounting one database from two machines at once |
| `mmap`, including shared writable mappings | `st_blocks` reflects file size, not allocated blocks |

## Performance

Measured with [bench/bench.sh](bench/bench.sh) on a laptop (Intel i7-1355U, NVMe, Linux 6.8), default settings, `-sync fsync`. PostgreSQL 17 ran in a container on the same machine with its default configuration.

| Workload | ext4 | tuplefs on SQLite | tuplefs on PostgreSQL |
|---|---:|---:|---:|
| Write 256 MiB sequentially, then fsync | 833 MiB/s | 77 MiB/s | 50 MiB/s |
| Read 256 MiB sequentially | 5,038 MiB/s (page cache) | 340 MiB/s | 64 MiB/s |
| Overwrite 2,000 × 4 KiB, then fsync | 144,360 /s | 2,527 /s | 342 /s |
| Create 5,000 files of 4 KiB | 13,703 /s | 767 /s | 198 /s |
| `ls -l` of 5,000 files | 132,528 /s | 5,775 /s | 1,076 /s |
| Read 5,000 files | 50,700 /s | 3,065 /s | 582 /s |
| Delete 5,000 files | 77,112 /s | 1,041 /s | 264 /s |

So on a single machine tuplefs on SQLite is roughly 10 to 75 times slower than ext4, and on PostgreSQL, where every statement also crosses a socket, slower again. Every request crosses from the kernel to a user-space process and becomes a SQL transaction, while ext4 runs in the kernel with its own page cache. The claim that a DBMS-backed file system can beat the Linux file system was made about a distributed, in-memory DBMS on a cluster; this project does not reproduce it and does not claim it.

Use tuplefs where the properties matter more than throughput: configuration and state you want transactional and queryable, a file tree you want replicated by your database, or as a readable reference for how a file system maps onto tables.

## Testing

- **Store tests** run every operation against SQLite and, with `TUPLEFS_TEST_POSTGRES` set, PostgreSQL. They include a randomized read/write/truncate/punch-hole test checked against an in-memory model, a stress test of colliding creates, links, renames and directory moves from eight goroutines, and a test that fires two opposing directory moves at the same instant. After each one, `fsck` must find nothing.
- **Mount tests** use a real FUSE mount: permissions, hard links, atomic rename, unlink while open, block-boundary writes, concurrent writers.
- **Crash test**: `SIGKILL` the tuplefs process while eight writers are busy, then check the database. Every invariant must hold, the fsynced file must be intact, and every file must be either empty or complete, never torn. This covers a crash of tuplefs; surviving power loss relies on the database's own durability.
- **Real workloads**, run by hand on a mount: `git clone` followed by `git fsck --full`, a tar round trip compared with `diff -r`, and `go build` of a project inside the mount.
- **POSIX conformance**: CI runs [pjdfstest](https://github.com/pjd/pjdfstest) against a mount as root.

```sh
go test ./...                                                   # SQLite; mount tests skip without /dev/fuse
TUPLEFS_TEST_POSTGRES=postgres://... go test -p 1 ./internal/...  # wipes the tuplefs tables in that database
```

## Limits

- One mount per database. Sharing a database between machines needs cache invalidation between mounts, which is not implemented.
- Whole blocks are rewritten: changing one byte rewrites its 32 KiB block row.
- `statfs` reports a fixed 1 TiB capacity. The real limit is the database's.
- Linux only.

## License

[Apache-2.0](LICENSE)
