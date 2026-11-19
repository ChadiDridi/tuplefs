package fusefs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ChadiDridi/tuplefs/internal/backing"
	"github.com/ChadiDridi/tuplefs/internal/store"
)

// TestReconcile sets up what an unclean shutdown can leave behind in the
// hybrid layout and checks that Reconcile repairs all of it: a size in the
// database that trails the backing file, a backing file whose inode is gone,
// and a stray file filed under the wrong subdirectory.
func TestReconcile(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "fs.db"), store.SyncOnFsync)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.FormatWith(ctx, store.FormatOptions{DataFiles: true, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := st.Load(ctx); err != nil {
		t.Fatal(err)
	}
	dir, err := backing.Create(filepath.Join(t.TempDir(), "data"), st.UUID())
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Mknod(ctx, store.RootIno, "written", syscall.S_IFREG|0o644, 0, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Mknod(ctx, store.RootIno, "never-written", syscall.S_IFREG|0o644, 0, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	put := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(dir.Path(a.Ino), "twelve bytes")                  // the database still says size 0
	put(dir.Path(4242), "inode 4242 does not exist")      // leftover of a deleted file
	put(filepath.Join(dir.Root(), "zz", "7"), "misfiled") // not where inode 7 would live

	if problems, _ := CheckFiles(ctx, st, dir); len(problems) != 3 {
		t.Fatalf("before: fsck found %d problems, want 3: %v", len(problems), problems)
	}
	sizes, removed, err := Reconcile(ctx, st, dir)
	if err != nil || sizes != 1 || removed != 2 {
		t.Fatalf("Reconcile = %d sizes, %d removed, %v; want 1, 2", sizes, removed, err)
	}
	if got, _ := st.GetAttr(ctx, a.Ino); got.Size != 12 {
		t.Errorf("recorded size = %d, want 12", got.Size)
	}
	if problems, _ := CheckFiles(ctx, st, dir); len(problems) != 0 {
		t.Errorf("after: fsck still finds %v", problems)
	}
	if _, err := os.Stat(dir.Path(a.Ino)); err != nil {
		t.Errorf("the live backing file was removed: %v", err)
	}
}
