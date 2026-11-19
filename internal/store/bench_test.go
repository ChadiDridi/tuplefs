package store

import (
	"fmt"
	"syscall"
	"testing"
)

func BenchmarkCreateSmallFile(b *testing.B) {
	s := benchStore(b)
	data := make([]byte, 4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, err := s.Mknod(ctx, RootIno, fmt.Sprintf("f%d", i), syscall.S_IFREG|0o644, 0, 0, 0, "")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := s.Write(ctx, a.Ino, 0, data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLookup(b *testing.B) {
	s := benchStore(b)
	s.Mknod(ctx, RootIno, "f", syscall.S_IFREG|0o644, 0, 0, 0, "")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Lookup(ctx, RootIno, "f"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWrite1MiB(b *testing.B) {
	s := benchStore(b)
	a, _ := s.Mknod(ctx, RootIno, "f", syscall.S_IFREG|0o644, 0, 0, 0, "")
	data := make([]byte, 1<<20)
	b.SetBytes(1 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Write(ctx, a.Ino, int64(i%256)<<20, data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRead1MiB(b *testing.B) {
	s := benchStore(b)
	a, _ := s.Mknod(ctx, RootIno, "f", syscall.S_IFREG|0o644, 0, 0, 0, "")
	s.Write(ctx, a.Ino, 0, make([]byte, 64<<20))
	b.SetBytes(1 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Read(ctx, a.Ino, int64(i%64)<<20, 1<<20); err != nil {
			b.Fatal(err)
		}
	}
}

func benchStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(b.TempDir()+"/fs.db", SyncOnFsync)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	if err := s.Format(ctx, DefaultBlockSize, 0, 0, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := s.Load(ctx); err != nil {
		b.Fatal(err)
	}
	return s
}
