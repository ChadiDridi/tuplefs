package store

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

var ctx = context.Background()

// newStore returns an empty file system. Set TUPLEFS_TEST_POSTGRES to a
// connection URL to run the same tests against PostgreSQL; the public schema of
// that database is wiped.
func newStore(t *testing.T, blockSize int64) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "fs.db")
	if pg := os.Getenv("TUPLEFS_TEST_POSTGRES"); pg != "" {
		dsn = pg
	}
	s, err := Open(dsn, SyncOnFsync)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if s.dialect == postgresDialect {
		if err := s.Drop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Format(ctx, blockSize, 1000, 1000, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func mk(t *testing.T, s *Store, parent uint64, name string, mode uint32) Attr {
	t.Helper()
	a, err := s.Mknod(ctx, parent, name, mode, 0, 1000, 1000, "")
	if err != nil {
		t.Fatalf("mknod %s: %v", name, err)
	}
	return a
}

func wantErrno(t *testing.T, err error, want syscall.Errno) {
	t.Helper()
	if err != want {
		t.Fatalf("got error %v, want %v", err, want)
	}
}

// checkClean fails the test if fsck finds anything.
func checkClean(t *testing.T, s *Store) {
	t.Helper()
	problems, _, err := s.Fsck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("fsck: %s: %s", p.Check, p.Detail)
	}
}

func TestCreateLookupReaddir(t *testing.T) {
	s := newStore(t, 4096)
	dir := mk(t, s, RootIno, "dir", syscall.S_IFDIR|0o755)
	file := mk(t, s, dir.Ino, "file", syscall.S_IFREG|0o644)
	link, err := s.Mknod(ctx, dir.Ino, "link", syscall.S_IFLNK|0o777, 0, 1000, 1000, "file")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Lookup(ctx, dir.Ino, "file")
	if err != nil || got.Ino != file.Ino || got.Nlink != 1 {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	_, err = s.Lookup(ctx, dir.Ino, "missing")
	wantErrno(t, err, syscall.ENOENT)
	_, err = s.Mknod(ctx, dir.Ino, "file", syscall.S_IFREG|0o644, 0, 0, 0, "")
	wantErrno(t, err, syscall.EEXIST)
	_, err = s.Mknod(ctx, file.Ino, "x", syscall.S_IFREG|0o644, 0, 0, 0, "")
	wantErrno(t, err, syscall.ENOTDIR)
	_, err = s.Mknod(ctx, dir.Ino, string(make([]byte, 256)), syscall.S_IFREG|0o644, 0, 0, 0, "")
	wantErrno(t, err, syscall.ENAMETOOLONG)
	_, err = s.Mknod(ctx, dir.Ino, "bad\xff", syscall.S_IFREG|0o644, 0, 0, 0, "")
	wantErrno(t, err, syscall.EILSEQ)

	if target, err := s.Readlink(ctx, link.Ino); err != nil || target != "file" {
		t.Fatalf("readlink = %q, %v", target, err)
	}
	entries, err := s.ReadDir(ctx, dir.Ino)
	if err != nil || len(entries) != 2 || entries[0].Name != "file" || entries[1].Name != "link" {
		t.Fatalf("readdir = %+v, %v", entries, err)
	}
	root, _ := s.GetAttr(ctx, RootIno)
	if root.Nlink != 3 {
		t.Fatalf("root nlink = %d, want 3", root.Nlink)
	}
	checkClean(t, s)
}

func TestSetgidDirectoryInheritance(t *testing.T) {
	s := newStore(t, 4096)
	dir, err := s.Mknod(ctx, RootIno, "shared", syscall.S_IFDIR|syscall.S_ISGID|0o775, 0, 1000, 42, "")
	if err != nil {
		t.Fatal(err)
	}
	f := mk(t, s, dir.Ino, "f", syscall.S_IFREG|0o644)
	sub := mk(t, s, dir.Ino, "sub", syscall.S_IFDIR|0o755)
	if f.Gid != 42 || sub.Gid != 42 || sub.Mode&syscall.S_ISGID == 0 || f.Mode&syscall.S_ISGID != 0 {
		t.Fatalf("file gid %d mode %o, subdir gid %d mode %o", f.Gid, f.Mode, sub.Gid, sub.Mode)
	}
}

func TestLinkUnlinkReap(t *testing.T) {
	s := newStore(t, 4096)
	f := mk(t, s, RootIno, "a", syscall.S_IFREG|0o644)
	if _, err := s.Write(ctx, f.Ino, 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Link(ctx, f.Ino, RootIno, "b"); err != nil || a.Nlink != 2 {
		t.Fatalf("link = %+v, %v", a, err)
	}
	_, err := s.Link(ctx, f.Ino, RootIno, "b")
	wantErrno(t, err, syscall.EEXIST)
	_, err = s.Link(ctx, RootIno, RootIno, "rootlink")
	wantErrno(t, err, syscall.EPERM)

	if _, n, err := s.Unlink(ctx, RootIno, "a"); err != nil || n != 1 {
		t.Fatalf("unlink a: nlink %d, %v", n, err)
	}
	ino, n, err := s.Unlink(ctx, RootIno, "b")
	if err != nil || n != 0 || ino != f.Ino {
		t.Fatalf("unlink b: ino %d nlink %d, %v", ino, n, err)
	}
	// Still open somewhere: the data must survive until Reap.
	if buf, err := s.Read(ctx, f.Ino, 0, 10); err != nil || string(buf) != "data" {
		t.Fatalf("read after unlink = %q, %v", buf, err)
	}
	_, err = s.Link(ctx, f.Ino, RootIno, "c")
	wantErrno(t, err, syscall.ENOENT)
	if _, orphans, _ := s.Fsck(ctx); orphans != 1 {
		t.Fatalf("orphans = %d, want 1", orphans)
	}
	if err := s.Reap(ctx, f.Ino); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetAttr(ctx, f.Ino)
	wantErrno(t, err, syscall.ENOENT)
	if u, _ := s.StatFS(ctx); u.DataBlocks != 0 || u.Inodes != 1 {
		t.Fatalf("usage after reap = %+v", u)
	}
	checkClean(t, s)
}

func TestReapOrphansAfterCrash(t *testing.T) {
	s := newStore(t, 4096)
	for _, name := range []string{"a", "b"} {
		f := mk(t, s, RootIno, name, syscall.S_IFREG|0o644)
		s.Write(ctx, f.Ino, 0, make([]byte, 10000))
		s.SetXattr(ctx, f.Ino, "user.k", []byte("v"), 0)
	}
	s.Unlink(ctx, RootIno, "a") // "crash" before Reap
	n, err := s.ReapOrphans(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ReapOrphans = %d, %v", n, err)
	}
	if u, _ := s.StatFS(ctx); u.Inodes != 2 || u.DataBlocks != 3 {
		t.Fatalf("usage = %+v", u)
	}
	checkClean(t, s)
}

func TestRmdir(t *testing.T) {
	s := newStore(t, 4096)
	d := mk(t, s, RootIno, "d", syscall.S_IFDIR|0o755)
	mk(t, s, d.Ino, "f", syscall.S_IFREG|0o644)
	wantErrno(t, s.Rmdir(ctx, RootIno, "d"), syscall.ENOTEMPTY)
	wantErrno(t, s.Rmdir(ctx, d.Ino, "f"), syscall.ENOTDIR)
	_, _, err := s.Unlink(ctx, RootIno, "d")
	wantErrno(t, err, syscall.EISDIR)
	s.Unlink(ctx, d.Ino, "f")
	if err := s.Rmdir(ctx, RootIno, "d"); err != nil {
		t.Fatal(err)
	}
	// A removed directory cannot get new entries.
	_, err = s.Mknod(ctx, d.Ino, "late", syscall.S_IFREG|0o644, 0, 0, 0, "")
	wantErrno(t, err, syscall.ENOENT)
	if root, _ := s.GetAttr(ctx, RootIno); root.Nlink != 2 {
		t.Fatalf("root nlink = %d", root.Nlink)
	}
	checkClean(t, s)
}

func TestRename(t *testing.T) {
	s := newStore(t, 4096)
	a := mk(t, s, RootIno, "a", syscall.S_IFDIR|0o755)
	b := mk(t, s, RootIno, "b", syscall.S_IFDIR|0o755)
	f := mk(t, s, a.Ino, "f", syscall.S_IFREG|0o644)
	g := mk(t, s, b.Ino, "g", syscall.S_IFREG|0o644)
	sub := mk(t, s, a.Ino, "sub", syscall.S_IFDIR|0o755)

	// Plain move across directories.
	if _, _, err := s.Rename(ctx, a.Ino, "f", b.Ino, "f2", 0); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Lookup(ctx, b.Ino, "f2"); err != nil || got.Ino != f.Ino {
		t.Fatalf("after move: %+v, %v", got, err)
	}
	// Replace an existing file: the old inode loses its last link.
	replaced, nlink, err := s.Rename(ctx, b.Ino, "f2", b.Ino, "g", 0)
	if err != nil || replaced != g.Ino || nlink != 0 {
		t.Fatalf("replace: replaced %d nlink %d, %v", replaced, nlink, err)
	}
	s.Reap(ctx, replaced)
	// NOREPLACE.
	mk(t, s, b.Ino, "h", syscall.S_IFREG|0o644)
	_, _, err = s.Rename(ctx, b.Ino, "g", b.Ino, "h", RenameNoReplace)
	wantErrno(t, err, syscall.EEXIST)
	// Type mismatches.
	_, _, err = s.Rename(ctx, b.Ino, "g", a.Ino, "sub", 0)
	wantErrno(t, err, syscall.EISDIR)
	_, _, err = s.Rename(ctx, a.Ino, "sub", b.Ino, "g", 0)
	wantErrno(t, err, syscall.ENOTDIR)
	// A directory cannot move into its own subtree.
	deep := mk(t, s, sub.Ino, "deep", syscall.S_IFDIR|0o755)
	_, _, err = s.Rename(ctx, RootIno, "a", deep.Ino, "a", 0)
	wantErrno(t, err, syscall.EINVAL)
	// Moving a directory updates both parents' link counts.
	if _, _, err := s.Rename(ctx, a.Ino, "sub", b.Ino, "sub", 0); err != nil {
		t.Fatal(err)
	}
	aAttr, _ := s.GetAttr(ctx, a.Ino)
	bAttr, _ := s.GetAttr(ctx, b.Ino)
	if aAttr.Nlink != 2 || bAttr.Nlink != 3 {
		t.Fatalf("nlink a %d b %d, want 2 and 3", aAttr.Nlink, bAttr.Nlink)
	}
	// Directory over a non-empty directory fails, over an empty one works.
	mk(t, s, RootIno, "c", syscall.S_IFDIR|0o755)
	_, _, err = s.Rename(ctx, RootIno, "c", RootIno, "b", 0)
	wantErrno(t, err, syscall.ENOTEMPTY)
	if _, _, err := s.Rename(ctx, RootIno, "c", RootIno, "a", 0); err != nil {
		t.Fatal(err)
	}
	// Exchange a file and a directory in different parents.
	if _, _, err := s.Rename(ctx, b.Ino, "g", RootIno, "a", RenameExchange); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Lookup(ctx, RootIno, "a"); got.Ino != f.Ino {
		t.Fatalf("exchange: root/a is ino %d, want file %d", got.Ino, f.Ino)
	}
	if got, _ := s.Lookup(ctx, b.Ino, "g"); !got.IsDir() {
		t.Fatal("exchange: b/g should now be a directory")
	}
	_, _, err = s.Rename(ctx, RootIno, "a", RootIno, "nope", RenameExchange)
	wantErrno(t, err, syscall.ENOENT)
	// Renaming one hard link over another of the same file does nothing.
	s.Link(ctx, f.Ino, RootIno, "alias")
	if _, _, err := s.Rename(ctx, RootIno, "a", RootIno, "alias", 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetAttr(ctx, f.Ino); got.Nlink != 2 {
		t.Fatalf("nlink after same-file rename = %d, want 2", got.Nlink)
	}
	checkClean(t, s)
}

// TestReadWriteModel checks random writes, truncates and hole punches against
// an in-memory copy of the file, with a block size small enough that every
// boundary case comes up.
func TestReadWriteModel(t *testing.T) {
	const bs = 512
	s := newStore(t, bs)
	f := mk(t, s, RootIno, "f", syscall.S_IFREG|0o644)
	rng := rand.New(rand.NewSource(1))
	var model []byte
	grow := func(n int) {
		if n > len(model) {
			model = append(model, make([]byte, n-len(model))...)
		}
	}
	for i := 0; i < 400; i++ {
		switch op := rng.Intn(10); {
		case op < 6: // write
			off, n := rng.Intn(6*bs), rng.Intn(3*bs)+1
			data := make([]byte, n)
			rng.Read(data)
			if _, err := s.Write(ctx, f.Ino, int64(off), data); err != nil {
				t.Fatal(err)
			}
			grow(off + n)
			copy(model[off:], data)
		case op < 8: // truncate, up or down
			size := uint64(rng.Intn(8 * bs))
			if _, err := s.SetAttr(ctx, f.Ino, SetAttrReq{Size: &size}); err != nil {
				t.Fatal(err)
			}
			grow(int(size))
			model = model[:size]
		default: // punch a hole
			off, n := rng.Intn(6*bs), rng.Intn(3*bs)+1
			if err := s.Fallocate(ctx, f.Ino, FallocPunchHole|FallocKeepSize, int64(off), int64(n)); err != nil {
				t.Fatal(err)
			}
			for j := off; j < off+n && j < len(model); j++ {
				model[j] = 0
			}
		}
		a, err := s.GetAttr(ctx, f.Ino)
		if err != nil || a.Size != uint64(len(model)) {
			t.Fatalf("step %d: size %d, want %d (%v)", i, a.Size, len(model), err)
		}
		got, err := s.Read(ctx, f.Ino, 0, len(model)+100)
		if err != nil || !bytes.Equal(got, model) {
			t.Fatalf("step %d: content differs from model (%v)", i, err)
		}
		off, n := rng.Intn(len(model)+1), rng.Intn(2*bs)
		got, _ = s.Read(ctx, f.Ino, int64(off), n)
		if want := model[off:min(off+n, len(model))]; !bytes.Equal(got, want) {
			t.Fatalf("step %d: partial read at %d+%d differs", i, off, n)
		}
	}
	checkClean(t, s)
}

func TestSparseFileStoresNoBlocksForHoles(t *testing.T) {
	s := newStore(t, 4096)
	f := mk(t, s, RootIno, "sparse", syscall.S_IFREG|0o644)
	if _, err := s.Write(ctx, f.Ino, 1<<30, []byte("end")); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.StatFS(ctx); u.DataBlocks != 1 {
		t.Fatalf("a 1GiB sparse file uses %d blocks, want 1", u.DataBlocks)
	}
	buf, err := s.Read(ctx, f.Ino, 1<<30-2, 10)
	if err != nil || !bytes.Equal(buf, []byte{0, 0, 'e', 'n', 'd'}) {
		t.Fatalf("read = %v, %v", buf, err)
	}
	if err := s.Fallocate(ctx, f.Ino, 0, 0, 2<<30); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetAttr(ctx, f.Ino); a.Size != 2<<30 {
		t.Fatalf("size after fallocate = %d", a.Size)
	}
	checkClean(t, s)
}

func TestSetAttr(t *testing.T) {
	s := newStore(t, 4096)
	f := mk(t, s, RootIno, "f", syscall.S_IFREG|0o644)
	mode, uid, mtime := uint32(0o4755), uint32(7), int64(123456789)
	a, err := s.SetAttr(ctx, f.Ino, SetAttrReq{Mode: &mode, Uid: &uid, MtimeNs: &mtime})
	if err != nil || a.Mode != syscall.S_IFREG|0o4755 || a.Uid != 7 || a.Gid != 1000 || a.MtimeNs != mtime {
		t.Fatalf("setattr = %+v, %v", a, err)
	}
	size := uint64(10)
	_, err = s.SetAttr(ctx, RootIno, SetAttrReq{Size: &size})
	wantErrno(t, err, syscall.EISDIR)
}

func TestXattr(t *testing.T) {
	s := newStore(t, 4096)
	f := mk(t, s, RootIno, "f", syscall.S_IFREG|0o644)
	_, err := s.GetXattr(ctx, f.Ino, "user.a")
	wantErrno(t, err, syscall.ENODATA)
	wantErrno(t, s.SetXattr(ctx, f.Ino, "user.a", []byte("1"), XattrReplace), syscall.ENODATA)
	if err := s.SetXattr(ctx, f.Ino, "user.a", []byte("1"), XattrCreate); err != nil {
		t.Fatal(err)
	}
	wantErrno(t, s.SetXattr(ctx, f.Ino, "user.a", []byte("2"), XattrCreate), syscall.EEXIST)
	s.SetXattr(ctx, f.Ino, "user.b", nil, 0)
	s.SetXattr(ctx, f.Ino, "user.a", []byte("3"), 0)
	if v, _ := s.GetXattr(ctx, f.Ino, "user.a"); string(v) != "3" {
		t.Fatalf("value = %q", v)
	}
	if names, _ := s.ListXattr(ctx, f.Ino); len(names) != 2 || names[0] != "user.a" || names[1] != "user.b" {
		t.Fatalf("list = %v", names)
	}
	if err := s.RemoveXattr(ctx, f.Ino, "user.a"); err != nil {
		t.Fatal(err)
	}
	wantErrno(t, s.RemoveXattr(ctx, f.Ino, "user.a"), syscall.ENODATA)
	checkClean(t, s)
}

func TestFormatRejectsSecondFormatAndSurvivesReopen(t *testing.T) {
	if os.Getenv("TUPLEFS_TEST_POSTGRES") != "" {
		t.Skip("reopen test uses a SQLite file")
	}
	path := filepath.Join(t.TempDir(), "fs.db")
	s, err := Open(path, SyncAlways)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Format(ctx, 8192, 0, 0, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Format(ctx, 8192, 0, 0, 0o755); err == nil {
		t.Fatal("second Format should fail")
	}
	s.Load(ctx)
	f := mk(t, s, RootIno, "kept", syscall.S_IFREG|0o600)
	s.Write(ctx, f.Ino, 0, []byte("persisted"))
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.LockMount(ctx); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(path, SyncOnFsync)
	if err := s2.LockMount(ctx); err == nil {
		t.Fatal("second mount lock should fail")
	}
	s2.Close()
	s.Close()

	s, err = Open(path, SyncOnFsync)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Load(ctx); err != nil || s.BlockSize() != 8192 {
		t.Fatalf("load: block size %d, %v", s.BlockSize(), err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := s.Lookup(ctx, RootIno, "kept")
	if err != nil {
		t.Fatal(err)
	}
	if buf, _ := s.Read(ctx, a.Ino, 0, 100); string(buf) != "persisted" {
		t.Fatalf("content after reopen = %q", buf)
	}
}

func TestFsckDetectsCorruption(t *testing.T) {
	s := newStore(t, 4096)
	d := mk(t, s, RootIno, "d", syscall.S_IFDIR|0o755)
	f := mk(t, s, d.Ino, "f", syscall.S_IFREG|0o644)
	s.Write(ctx, f.Ino, 0, []byte("x"))
	// Break things behind the store's back.
	for _, q := range []string{
		`UPDATE inodes SET nlink = 5 WHERE ino = ` + itoa(f.Ino),
		`INSERT INTO blocks (ino, idx, data) VALUES (9999, 0, 'zz')`,
		`UPDATE inodes SET size = 0 WHERE ino = ` + itoa(f.Ino),
		`DELETE FROM dirents WHERE ino = ` + itoa(d.Ino),
	} {
		if _, err := s.wdb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	problems, _, err := s.Fsck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, p := range problems {
		found[p.Check] = true
	}
	for _, want := range []string{
		"file link count does not match its directory entries",
		"data block belongs to a missing inode",
		"data stored past the end of the file",
		"directory is not reachable from the root",
		"directory link count is not 2 + subdirectories",
	} {
		if !found[want] {
			t.Errorf("fsck did not report: %s (got %v)", want, problems)
		}
	}
}

func itoa(n uint64) string {
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
