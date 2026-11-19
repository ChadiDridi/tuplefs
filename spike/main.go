// Command spike answers how FUSE passthrough behaves on a given kernel. It is
// a throwaway experiment and is removed before this branch is merged.
package main

import (
	"context"
	"log"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var backing string

type root struct{ fs.Inode }

type file struct {
	fs.Inode
}

// handle is an open of the backing file. Opens with O_SYNC get a plain handle
// (all I/O through the server); the rest ask for passthrough.
type handle struct {
	fd          int
	passthrough bool
}

func (h *handle) PassthroughFd() (int, bool) { return h.fd, h.passthrough }

func (r *root) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name != "f" {
		return nil, syscall.ENOENT
	}
	var st syscall.Stat_t
	if err := syscall.Stat(backing, &st); err != nil {
		return nil, syscall.ENOENT
	}
	out.Attr.FromStat(&st)
	return r.NewInode(ctx, &file{}, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 2}), 0
}

func (r *root) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	log.Printf("SERVER CREATE %s", name)
	fd, err := syscall.Open(backing, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	var st syscall.Stat_t
	syscall.Fstat(fd, &st)
	out.Attr.FromStat(&st)
	return r.NewInode(ctx, &file{}, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 2}), &handle{fd: fd, passthrough: true}, 0, 0
}

func (f *file) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	var st syscall.Stat_t
	syscall.Stat(backing, &st)
	out.Attr.FromStat(&st)
	log.Printf("SERVER GETATTR size=%d", st.Size)
	return 0
}

func (f *file) Setattr(ctx context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if sz, ok := in.GetSize(); ok {
		log.Printf("SERVER SETATTR size=%d", sz)
		syscall.Truncate(backing, int64(sz))
	}
	var st syscall.Stat_t
	syscall.Stat(backing, &st)
	out.Attr.FromStat(&st)
	return 0
}

func (f *file) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	fd, err := syscall.Open(backing, os.O_RDWR, 0)
	if err != nil {
		return nil, 0, syscall.EIO
	}
	pt := flags&syscall.O_SYNC == 0
	log.Printf("SERVER OPEN passthrough=%v", pt)
	return &handle{fd: fd, passthrough: pt}, 0, 0
}

func (f *file) Read(ctx context.Context, h fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	log.Printf("SERVER READ off=%d len=%d", off, len(dest))
	n, _ := syscall.Pread(h.(*handle).fd, dest, off)
	return fuse.ReadResultData(dest[:max(n, 0)]), 0
}

func (f *file) Write(ctx context.Context, h fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	log.Printf("SERVER WRITE off=%d len=%d", off, len(data))
	n, _ := syscall.Pwrite(h.(*handle).fd, data, off)
	return uint32(n), 0
}

func (f *file) Fsync(ctx context.Context, h fs.FileHandle, flags uint32) syscall.Errno {
	log.Printf("SERVER FSYNC")
	return 0
}

func (f *file) Allocate(ctx context.Context, h fs.FileHandle, off uint64, size uint64, mode uint32) syscall.Errno {
	log.Printf("SERVER FALLOCATE off=%d size=%d mode=%d", off, size, mode)
	return fs.ToErrno(syscall.Fallocate(h.(*handle).fd, mode, int64(off), int64(size)))
}

func (f *file) Release(ctx context.Context, h fs.FileHandle) syscall.Errno {
	log.Printf("SERVER RELEASE passthrough=%v", h.(*handle).passthrough)
	syscall.Close(h.(*handle).fd)
	return 0
}

func main() {
	log.SetFlags(0)
	backing = os.Args[2]
	server, err := fs.Mount(os.Args[1], &root{}, &fs.Options{MountOptions: fuse.MountOptions{AllowOther: true}})
	if err != nil {
		log.Fatal(err)
	}
	server.Wait()
}
