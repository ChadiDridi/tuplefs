package fusefs

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/ChadiDridi/tuplefs/internal/store"
)

// mountFS mounts a fresh file system and returns its mount point and store.
// Tests that need a real mount are skipped where FUSE is not available.
func mountFS(t *testing.T) (string, *store.Store) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		skip(t, "no /dev/fuse")
	}
	if _, err := exec.LookPath("fusermount"); err != nil {
		if _, err := exec.LookPath("fusermount3"); err != nil {
			skip(t, "no fusermount")
		}
	}
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "fs.db")
	if pg := os.Getenv("TUPLEFS_TEST_POSTGRES"); pg != "" {
		dsn = pg
	}
	st, err := store.Open(dsn, store.SyncOnFsync)
	if err != nil {
		t.Fatal(err)
	}
	if st.Backend() == "postgres" {
		if err := st.Drop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Format(ctx, 4096, uint32(os.Getuid()), uint32(os.Getgid()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := st.Load(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var server *fuse.Server
	server, err = Mount(st, dir, Options{})
	if err != nil {
		st.Close()
		skip(t, "cannot mount: "+err.Error())
	}
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Errorf("unmount: %v", err)
		}
		problems, orphans, err := st.Fsck(ctx)
		if err != nil {
			t.Errorf("fsck: %v", err)
		}
		for _, p := range problems {
			t.Errorf("fsck: %s: %s", p.Check, p.Detail)
		}
		if orphans != 0 {
			t.Errorf("%d orphan inodes left after unmount", orphans)
		}
		st.Close()
	})
	return dir, st
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMountBasics(t *testing.T) {
	mnt, _ := mountFS(t)
	write(t, mnt+"/a", "hello")
	if got := read(t, mnt+"/a"); got != "hello" {
		t.Fatalf("read = %q", got)
	}
	if err := os.MkdirAll(mnt+"/d/e", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../a", mnt+"/d/sym"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, mnt+"/d/sym"); got != "hello" {
		t.Fatalf("read through symlink = %q", got)
	}
	if err := os.Link(mnt+"/a", mnt+"/d/hard"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(mnt + "/a")
	if err != nil || st.Sys().(*syscall.Stat_t).Nlink != 2 {
		t.Fatalf("nlink after link: %+v, %v", st, err)
	}
	if err := os.Chmod(mnt+"/a", 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(mnt + "/d/hard"); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode through hard link = %v", st.Mode())
	}
	entries, err := os.ReadDir(mnt + "/d")
	if err != nil || len(entries) != 3 {
		t.Fatalf("readdir = %v, %v", entries, err)
	}
	if err := os.Remove(mnt + "/d"); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("rmdir non-empty = %v", err)
	}
	if _, err := os.Stat(mnt + "/nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat missing = %v", err)
	}
	if err := os.RemoveAll(mnt + "/d"); err != nil {
		t.Fatal(err)
	}
}

func TestMountRenameReplacesAtomically(t *testing.T) {
	mnt, _ := mountFS(t)
	write(t, mnt+"/config", "old")
	write(t, mnt+"/config.tmp", "new")
	if err := os.Rename(mnt+"/config.tmp", mnt+"/config"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, mnt+"/config"); got != "new" {
		t.Fatalf("after rename = %q", got)
	}
	if _, err := os.Stat(mnt + "/config.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old name still there: %v", err)
	}
}

func TestMountUnlinkWhileOpen(t *testing.T) {
	mnt, st := mountFS(t)
	ctx := context.Background()
	write(t, mnt+"/f", "still here")
	f, err := os.Open(mnt + "/f")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(mnt + "/f"); err != nil {
		t.Fatal(err)
	}
	if _, orphans, _ := st.Fsck(ctx); orphans != 1 {
		t.Fatalf("orphans while open = %d, want 1", orphans)
	}
	buf := make([]byte, 100)
	n, _ := f.Read(buf)
	if string(buf[:n]) != "still here" {
		t.Fatalf("read after unlink = %q", buf[:n])
	}
	f.Close()
	// The kernel sends RELEASE asynchronously; the cleanup check that no
	// orphans remain runs after unmount, which waits for it.
}

func TestMountLargeFileAndTruncate(t *testing.T) {
	mnt, _ := mountFS(t)
	data := make([]byte, 5<<20+123)
	rand.New(rand.NewSource(2)).Read(data)
	if err := os.WriteFile(mnt+"/big", data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(mnt + "/big")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("5MiB round trip differs (%v)", err)
	}
	if err := os.Truncate(mnt+"/big", 10000); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(mnt+"/big", 20000); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(mnt + "/big")
	want := append(append([]byte{}, data[:10000]...), make([]byte, 10000)...)
	if !bytes.Equal(got, want) {
		t.Fatal("after shrinking and growing, the tail must read as zeros")
	}
	// Overwrite in the middle through a second descriptor, then append.
	f, err := os.OpenFile(mnt+"/big", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("XYZ"), 4094) // straddles a block boundary
	if err := f.Sync(); err != nil {
		t.Fatalf("fsync: %v", err)
	}
	f.Close()
	f, _ = os.OpenFile(mnt+"/big", os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("tail")
	f.Close()
	copy(want[4094:], "XYZ")
	want = append(want, "tail"...)
	if got, _ = os.ReadFile(mnt + "/big"); !bytes.Equal(got, want) {
		t.Fatal("content after overwrite and append differs")
	}
}

func TestMountConcurrentWriters(t *testing.T) {
	mnt, _ := mountFS(t)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			dir := filepath.Join(mnt, "w", string(rune('a'+w)))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Error(err)
				return
			}
			for i := 0; i < 50; i++ {
				name := filepath.Join(dir, "f")
				if err := os.WriteFile(name+".tmp", bytes.Repeat([]byte{byte(i)}, 3000), 0o644); err != nil {
					t.Error(err)
					return
				}
				if err := os.Rename(name+".tmp", name); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	for w := 0; w < 8; w++ {
		got, err := os.ReadFile(filepath.Join(mnt, "w", string(rune('a'+w)), "f"))
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{49}, 3000)) {
			t.Fatalf("writer %d: final content wrong (%v)", w, err)
		}
	}
}

func TestMountXattr(t *testing.T) {
	mnt, _ := mountFS(t)
	write(t, mnt+"/f", "x")
	if err := syscall.Setxattr(mnt+"/f", "user.color", []byte("blue"), 0); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := syscall.Getxattr(mnt+"/f", "user.color", buf)
	if err != nil || string(buf[:n]) != "blue" {
		t.Fatalf("getxattr = %q, %v", buf[:n], err)
	}
	n, err = syscall.Listxattr(mnt+"/f", buf)
	if err != nil || string(buf[:n]) != "user.color\x00" {
		t.Fatalf("listxattr = %q, %v", buf[:n], err)
	}
	if err := syscall.Removexattr(mnt+"/f", "user.color"); err != nil {
		t.Fatal(err)
	}
	if _, err := syscall.Getxattr(mnt+"/f", "user.color", buf); err != syscall.ENODATA {
		t.Fatalf("getxattr after remove = %v", err)
	}
}

func TestMountPermissionsAreEnforced(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	mnt, _ := mountFS(t)
	write(t, mnt+"/secret", "x")
	if err := os.Chmod(mnt+"/secret", 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(mnt + "/secret"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("reading a mode 000 file = %v, want permission denied", err)
	}
	if err := os.Mkdir(mnt+"/ro", 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mnt+"/ro/f", nil, 0o644); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("creating in a read-only directory = %v, want permission denied", err)
	}
}

// skip skips a test that needs FUSE, unless TUPLEFS_REQUIRE_FUSE is set, in
// which case missing FUSE support is a failure. CI sets it so that the mount
// tests cannot silently stop running.
func skip(t *testing.T, why string) {
	t.Helper()
	if os.Getenv("TUPLEFS_REQUIRE_FUSE") != "" {
		t.Fatal(why)
	}
	t.Skip(why)
}
