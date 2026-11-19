// Package fusefs exposes a store.Store as a FUSE file system.
//
// It is a thin translation layer: each FUSE request becomes one store call,
// which is one database transaction. The only state kept here is the count of
// open handles per inode, needed to delay deleting a file that is unlinked
// while still open.
package fusefs

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/ChadiDridi/tuplefs/internal/store"
)

// Options configures a mount.
type Options struct {
	AllowOther bool
	ReadOnly   bool
	Debug      bool
}

// FS is a mounted file system.
type FS struct {
	st *store.Store

	mu   sync.Mutex
	open map[uint64]int // open handles per inode
}

// Mount mounts the file system held in st at dir. The caller must have loaded
// the store. Wait on the returned server to block until it is unmounted.
func Mount(st *store.Store, dir string, opts Options) (*fuse.Server, error) {
	fsys := &FS{st: st, open: map[uint64]int{}}
	// One process owns the database (see Store.LockMount), so every change
	// goes through this mount and the kernel may cache what it is told.
	timeout := time.Second
	mountOpts := []string{"default_permissions"} // the kernel checks mode bits
	if opts.ReadOnly {
		mountOpts = append(mountOpts, "ro")
	}
	return fs.Mount(dir, &node{fsys: fsys}, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:     "tuplefs:" + st.Backend(),
			Name:       "tuplefs",
			AllowOther: opts.AllowOther,
			Options:    mountOpts,
			MaxWrite:   1 << 20,
			Debug:      opts.Debug,
		},
		EntryTimeout:    &timeout,
		AttrTimeout:     &timeout,
		RootStableAttr:  &fs.StableAttr{Ino: store.RootIno},
		NullPermissions: true, // report mode 000 as stored, not as a default
	})
}

// node is one inode of the mounted tree. All its state lives in the database;
// the inode number is the key.
type node struct {
	fs.Inode
	fsys *FS
}

var (
	_ fs.NodeLookuper      = (*node)(nil)
	_ fs.NodeGetattrer     = (*node)(nil)
	_ fs.NodeSetattrer     = (*node)(nil)
	_ fs.NodeMkdirer       = (*node)(nil)
	_ fs.NodeMknoder       = (*node)(nil)
	_ fs.NodeCreater       = (*node)(nil)
	_ fs.NodeSymlinker     = (*node)(nil)
	_ fs.NodeReadlinker    = (*node)(nil)
	_ fs.NodeLinker        = (*node)(nil)
	_ fs.NodeUnlinker      = (*node)(nil)
	_ fs.NodeRmdirer       = (*node)(nil)
	_ fs.NodeRenamer       = (*node)(nil)
	_ fs.NodeReaddirer     = (*node)(nil)
	_ fs.NodeOpener        = (*node)(nil)
	_ fs.NodeReader        = (*node)(nil)
	_ fs.NodeWriter        = (*node)(nil)
	_ fs.NodeFsyncer       = (*node)(nil)
	_ fs.NodeReleaser      = (*node)(nil)
	_ fs.NodeAllocater     = (*node)(nil)
	_ fs.NodeStatfser      = (*node)(nil)
	_ fs.NodeGetxattrer    = (*node)(nil)
	_ fs.NodeSetxattrer    = (*node)(nil)
	_ fs.NodeListxattrer   = (*node)(nil)
	_ fs.NodeRemovexattrer = (*node)(nil)
)

func (n *node) ino() uint64 { return n.StableAttr().Ino }

// bg detaches a request from kernel interrupts. Operations are short, and
// letting a signal abort one would surface EINTR to programs that do not
// expect it from a local file system.
func bg(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// errno converts a store error to what the kernel should see.
func errno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var e syscall.Errno
	if errors.As(err, &e) {
		return e
	}
	log.Printf("tuplefs: database error: %v", err)
	return syscall.EIO
}

func caller(ctx context.Context) (uid, gid uint32) {
	if c, ok := fuse.FromContext(ctx); ok {
		return c.Uid, c.Gid
	}
	return 0, 0
}

func fillAttr(a store.Attr, out *fuse.Attr) {
	out.Ino = a.Ino
	out.Mode = a.Mode
	out.Nlink = a.Nlink
	out.Size = a.Size
	out.Blocks = (a.Size + 511) / 512
	out.Blksize = 4096
	out.Owner = fuse.Owner{Uid: a.Uid, Gid: a.Gid}
	out.Rdev = a.Rdev
	out.Atime, out.Atimensec = uint64(a.AtimeNs/1e9), uint32(a.AtimeNs%1e9)
	out.Mtime, out.Mtimensec = uint64(a.MtimeNs/1e9), uint32(a.MtimeNs%1e9)
	out.Ctime, out.Ctimensec = uint64(a.CtimeNs/1e9), uint32(a.CtimeNs%1e9)
}

// child returns the tree node for an inode, creating it on first sight. Hard
// links resolve to the same node because the inode number is the identity.
func (n *node) child(ctx context.Context, a store.Attr, out *fuse.EntryOut) *fs.Inode {
	fillAttr(a, &out.Attr)
	return n.NewInode(ctx, &node{fsys: n.fsys}, fs.StableAttr{Mode: a.Mode & syscall.S_IFMT, Ino: a.Ino})
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	a, err := n.fsys.st.Lookup(bg(ctx), n.ino(), name)
	if err != nil {
		return nil, errno(err)
	}
	return n.child(ctx, a, out), 0
}

func (n *node) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	a, err := n.fsys.st.GetAttr(bg(ctx), n.ino())
	if err != nil {
		return errno(err)
	}
	fillAttr(a, &out.Attr)
	return 0
}

func (n *node) Setattr(ctx context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	var req store.SetAttrReq
	if v, ok := in.GetMode(); ok {
		req.Mode = &v
	}
	if v, ok := in.GetUID(); ok {
		req.Uid = &v
	}
	if v, ok := in.GetGID(); ok {
		req.Gid = &v
	}
	if v, ok := in.GetSize(); ok {
		req.Size = &v
	}
	if t, ok := in.GetATime(); ok {
		v := t.UnixNano()
		req.AtimeNs = &v
	}
	if t, ok := in.GetMTime(); ok {
		v := t.UnixNano()
		req.MtimeNs = &v
	}
	a, err := n.fsys.st.SetAttr(bg(ctx), n.ino(), req)
	if err != nil {
		return errno(err)
	}
	fillAttr(a, &out.Attr)
	return 0
}

func (n *node) mknod(ctx context.Context, name string, mode, rdev uint32, target string, out *fuse.EntryOut) (*fs.Inode, store.Attr, syscall.Errno) {
	uid, gid := caller(ctx)
	a, err := n.fsys.st.Mknod(bg(ctx), n.ino(), name, mode, rdev, uid, gid, target)
	if err != nil {
		return nil, a, errno(err)
	}
	return n.child(ctx, a, out), a, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	inode, _, errno := n.mknod(ctx, name, syscall.S_IFDIR|mode&0o7777, 0, "", out)
	return inode, errno
}

func (n *node) Mknod(ctx context.Context, name string, mode uint32, dev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if mode&syscall.S_IFMT == 0 {
		mode |= syscall.S_IFREG
	}
	inode, _, errno := n.mknod(ctx, name, mode, dev, "", out)
	return inode, errno
}

func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	inode, _, errno := n.mknod(ctx, name, syscall.S_IFLNK|0o777, 0, target, out)
	return inode, errno
}

func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	inode, a, errno := n.mknod(ctx, name, syscall.S_IFREG|mode&0o7777, 0, "", out)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	n.fsys.opened(a.Ino)
	return inode, &handle{}, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := n.fsys.st.Readlink(bg(ctx), n.ino())
	return []byte(target), errno(err)
}

func (n *node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	a, err := n.fsys.st.Link(bg(ctx), target.EmbeddedInode().StableAttr().Ino, n.ino(), name)
	if err != nil {
		return nil, errno(err)
	}
	return n.child(ctx, a, out), 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	ino, nlink, err := n.fsys.st.Unlink(bg(ctx), n.ino(), name)
	if err != nil {
		return errno(err)
	}
	if nlink == 0 {
		n.fsys.reapIfClosed(bg(ctx), ino)
	}
	return 0
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errno(n.fsys.st.Rmdir(bg(ctx), n.ino(), name))
}

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	replaced, nlink, err := n.fsys.st.Rename(bg(ctx), n.ino(), name, newParent.EmbeddedInode().StableAttr().Ino, newName, flags)
	if err != nil {
		return errno(err)
	}
	if replaced != 0 && nlink == 0 {
		n.fsys.reapIfClosed(bg(ctx), replaced)
	}
	return 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.fsys.st.ReadDir(bg(ctx), n.ino())
	if err != nil {
		return nil, errno(err)
	}
	parent := n.ino()
	if _, p := n.Parent(); p != nil {
		parent = p.StableAttr().Ino
	}
	list := make([]fuse.DirEntry, 0, len(entries)+2)
	list = append(list,
		fuse.DirEntry{Name: ".", Ino: n.ino(), Mode: syscall.S_IFDIR},
		fuse.DirEntry{Name: "..", Ino: parent, Mode: syscall.S_IFDIR})
	for _, e := range entries {
		list = append(list, fuse.DirEntry{Name: e.Name, Ino: e.Ino, Mode: e.Mode & syscall.S_IFMT})
	}
	return fs.NewListDirStream(list), 0
}

// handle is an open file. It carries no state, but go-fuse only reports the
// close of a file whose open returned a handle.
type handle struct{}

// opened records a new open handle on an inode.
func (f *FS) opened(ino uint64) {
	f.mu.Lock()
	f.open[ino]++
	f.mu.Unlock()
}

// reapIfClosed deletes an inode that lost its last name, unless a handle still
// has it open. In that case the last Release deletes it.
func (f *FS) reapIfClosed(ctx context.Context, ino uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open[ino] > 0 {
		return
	}
	if err := f.st.Reap(ctx, ino); err != nil {
		log.Printf("tuplefs: reclaiming inode %d: %v (the next mount will retry)", ino, err)
	}
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_TRUNC != 0 {
		var zero uint64
		if _, err := n.fsys.st.SetAttr(bg(ctx), n.ino(), store.SetAttrReq{Size: &zero}); err != nil {
			return nil, 0, errno(err)
		}
	}
	n.fsys.opened(n.ino())
	return &handle{}, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *node) Release(ctx context.Context, _ fs.FileHandle) syscall.Errno {
	f, ino := n.fsys, n.ino()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open[ino]--; f.open[ino] > 0 {
		return 0
	}
	delete(f.open, ino)
	// Last handle gone: if the file was unlinked meanwhile, drop its data now.
	if err := f.st.Reap(bg(ctx), ino); err != nil {
		log.Printf("tuplefs: reclaiming inode %d: %v (the next mount will retry)", ino, err)
	}
	return 0
}

func (n *node) Read(ctx context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	buf, err := n.fsys.st.Read(bg(ctx), n.ino(), off, len(dest))
	if err != nil {
		return nil, errno(err)
	}
	return fuse.ReadResultData(buf), 0
}

func (n *node) Write(ctx context.Context, _ fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	written, err := n.fsys.st.Write(bg(ctx), n.ino(), off, data)
	return uint32(written), errno(err)
}

func (n *node) Fsync(ctx context.Context, _ fs.FileHandle, _ uint32) syscall.Errno {
	return errno(n.fsys.st.Sync(bg(ctx)))
}

func (n *node) Allocate(ctx context.Context, _ fs.FileHandle, off uint64, size uint64, mode uint32) syscall.Errno {
	return errno(n.fsys.st.Fallocate(bg(ctx), n.ino(), mode, int64(off), int64(size)))
}

// virtualBlocks is the capacity statfs reports. The real limit is whatever
// the database can hold, which this layer cannot see.
const virtualBytes = 1 << 40

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	u, err := n.fsys.st.StatFS(bg(ctx))
	if err != nil {
		return errno(err)
	}
	bs := uint64(n.fsys.st.BlockSize())
	out.Bsize, out.Frsize = uint32(bs), uint32(bs)
	out.Blocks = virtualBytes / bs
	out.Bfree = out.Blocks - min(u.DataBlocks, out.Blocks)
	out.Bavail = out.Bfree
	out.Files = 1 << 32
	out.Ffree = out.Files - min(u.Inodes, out.Files)
	out.NameLen = store.MaxNameLen
	return 0
}

// POSIX ACLs are not enforced, so storing them would only mislead.
func aclName(name string) bool { return strings.HasPrefix(name, "system.posix_acl_") }

func (n *node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	if aclName(attr) {
		return 0, syscall.ENODATA
	}
	value, err := n.fsys.st.GetXattr(bg(ctx), n.ino(), attr)
	if err != nil {
		return 0, errno(err)
	}
	if len(dest) < len(value) {
		return uint32(len(value)), syscall.ERANGE
	}
	return uint32(copy(dest, value)), 0
}

func (n *node) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	if aclName(attr) {
		return syscall.ENOTSUP
	}
	return errno(n.fsys.st.SetXattr(bg(ctx), n.ino(), attr, data, flags))
}

func (n *node) Removexattr(ctx context.Context, attr string) syscall.Errno {
	return errno(n.fsys.st.RemoveXattr(bg(ctx), n.ino(), attr))
}

func (n *node) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	names, err := n.fsys.st.ListXattr(bg(ctx), n.ino())
	if err != nil {
		return 0, errno(err)
	}
	size := 0
	for _, name := range names {
		size += len(name) + 1
	}
	if len(dest) < size {
		return uint32(size), syscall.ERANGE
	}
	pos := 0
	for _, name := range names {
		pos += copy(dest[pos:], name)
		dest[pos] = 0
		pos++
	}
	return uint32(size), 0
}
