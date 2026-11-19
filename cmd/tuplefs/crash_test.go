package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ChadiDridi/tuplefs/internal/store"
)

func fusermount(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		skip(t, "no /dev/fuse")
	}
	for _, name := range []string{"fusermount3", "fusermount"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	skip(t, "no fusermount")
	return ""
}

// startMount runs the real binary and waits until the mount point is live.
func startMount(t *testing.T, bin, db, mnt string) *exec.Cmd {
	t.Helper()
	var before syscall.Stat_t
	if err := syscall.Stat(mnt, &before); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mount", db, mnt)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var st syscall.Stat_t
		if err := syscall.Stat(mnt, &st); err == nil && st.Dev != before.Dev {
			return cmd
		}
	}
	cmd.Process.Kill()
	skip(t, "mount did not come up (FUSE not usable here)")
	return nil
}

// TestKillDuringWrites kills the file system process with SIGKILL while eight
// writers are busy, then checks what the database holds: every invariant must
// hold, fsynced data must be there, and every file must be either empty or
// complete, because one write is one transaction.
func TestKillDuringWrites(t *testing.T) {
	umount := fusermount(t)
	dir := t.TempDir()
	bin, db, mnt := filepath.Join(dir, "tuplefs"), filepath.Join(dir, "fs.db"), filepath.Join(dir, "mnt")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	os.Mkdir(mnt, 0o755)
	cmd := startMount(t, bin, db, mnt)

	// A file that is fsynced before the crash.
	durable := bytes.Repeat([]byte("durable!"), 10000)
	f, err := os.Create(filepath.Join(mnt, "durable"))
	if err != nil {
		t.Fatal(err)
	}
	f.Write(durable)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// A file deleted while open: the crash leaves it as an orphan.
	held, _ := os.Create(filepath.Join(mnt, "held"))
	held.Write(make([]byte, 100000))
	os.Remove(filepath.Join(mnt, "held"))

	payload := func(w, i int) []byte { return bytes.Repeat([]byte{byte('a' + w)}, 2000+i%1000) }
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				name := filepath.Join(mnt, fmt.Sprintf("w%d-%d", w, i))
				if err := os.WriteFile(name, payload(w, i), 0o644); err != nil {
					return // the file system is gone
				}
				if i%3 == 0 {
					os.Rename(name, name+".renamed")
				}
			}
		}(w)
	}
	time.Sleep(400 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()
	close(stop)
	wg.Wait()
	held.Close()
	exec.Command(umount, "-u", "-z", mnt).Run()

	ctx := context.Background()
	st, err := store.Open(db, store.SyncOnFsync)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Load(ctx); err != nil {
		t.Fatal(err)
	}
	problems, orphans, err := st.Fsck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("fsck after crash: %s: %s", p.Check, p.Detail)
	}
	if orphans != 1 {
		t.Errorf("orphans after crash = %d, want 1 (the file deleted while open)", orphans)
	}
	st.Close()

	// Mount again: no repair step, just reclaiming the orphan.
	cmd = startMount(t, bin, db, mnt)
	defer func() {
		exec.Command(umount, "-u", mnt).Run()
		cmd.Wait()
	}()
	if got, err := os.ReadFile(filepath.Join(mnt, "durable")); err != nil || !bytes.Equal(got, durable) {
		t.Errorf("fsynced file after crash: %d bytes, %v", len(got), err)
	}
	entries, err := os.ReadDir(mnt)
	if err != nil {
		t.Fatal(err)
	}
	complete, empty := 0, 0
	for _, e := range entries {
		var w, i int
		if n, _ := fmt.Sscanf(e.Name(), "w%d-%d", &w, &i); n != 2 {
			continue
		}
		got, err := os.ReadFile(filepath.Join(mnt, e.Name()))
		switch {
		case err != nil:
			t.Errorf("%s: %v", e.Name(), err)
		case len(got) == 0:
			empty++ // created, killed before the write
		case bytes.Equal(got, payload(w, i)):
			complete++
		default:
			t.Errorf("%s holds %d bytes: a torn write", e.Name(), len(got))
		}
	}
	if complete == 0 {
		t.Error("no file survived the crash; the writers never ran")
	}
	t.Logf("after SIGKILL: %d complete files, %d empty, 0 torn", complete, empty)

	st, _ = store.Open(db, store.SyncOnFsync)
	defer st.Close()
	st.Load(ctx)
	if _, orphans, _ := st.Fsck(ctx); orphans != 0 {
		t.Errorf("orphans after remount = %d, want 0", orphans)
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
