// Command tuplefs creates, mounts and checks file systems stored in a SQL
// database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ChadiDridi/tuplefs/internal/fusefs"
	"github.com/ChadiDridi/tuplefs/internal/store"
)

var version = "dev"

const usage = `tuplefs stores a POSIX file system in a SQL database.

Usage:
  tuplefs mount [flags] <database> <mountpoint>   mount it (creates it if the database is empty)
  tuplefs mkfs  [flags] <database>                create an empty file system
  tuplefs fsck  <database>                        verify every invariant, read-only
  tuplefs info  <database>                        show size and usage
  tuplefs version

<database> is the path of a SQLite file, or a postgres:// URL.
Run "tuplefs <command> -h" for the flags of a command.
`

func main() {
	log.SetFlags(0)
	log.SetPrefix("tuplefs: ")
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "mount":
		err = mount(ctx, args)
	case "mkfs":
		err = mkfs(ctx, args)
	case "fsck":
		err = fsck(ctx, args)
	case "info":
		err = info(ctx, args)
	case "version", "-v", "--version":
		fmt.Println("tuplefs", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func parse(fs *flag.FlagSet, args []string, operands string, n int) []string {
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: tuplefs %s [flags] %s\n", fs.Name(), operands)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != n {
		fs.Usage()
		os.Exit(2)
	}
	return fs.Args()
}

// open connects to an existing file system.
func open(ctx context.Context, dsn string, mode store.SyncMode) (*store.Store, error) {
	st, err := store.Open(dsn, mode)
	if err != nil {
		return nil, err
	}
	if err := st.Load(ctx); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

func mkfs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mkfs", flag.ExitOnError)
	blockSize := fs.Int64("block-size", store.DefaultBlockSize, "bytes per data block row, a power of two")
	mode := fs.Uint("mode", 0o755, "permission bits of the root directory")
	rest := parse(fs, args, "<database>", 1)
	st, err := store.Open(rest[0], store.SyncAlways)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Format(ctx, *blockSize, uint32(os.Getuid()), uint32(os.Getgid()), uint32(*mode)); err != nil {
		return err
	}
	fmt.Printf("created a %s file system with %d-byte blocks\n", st.Backend(), *blockSize)
	return nil
}

func mount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	syncMode := fs.String("sync", "fsync", `when changes reach stable storage: "fsync" (on fsync, like a local file system) or "always" (every operation)`)
	allowOther := fs.Bool("allow-other", false, "let other users access the mount (needs user_allow_other in /etc/fuse.conf)")
	readOnly := fs.Bool("ro", false, "mount read-only")
	debug := fs.Bool("debug", false, "log every FUSE request")
	rest := parse(fs, args, "<database> <mountpoint>", 2)

	var mode store.SyncMode
	switch *syncMode {
	case "fsync":
		mode = store.SyncOnFsync
	case "always":
		mode = store.SyncAlways
	default:
		return fmt.Errorf(`-sync must be "fsync" or "always", not %q`, *syncMode)
	}
	st, err := store.Open(rest[0], mode)
	if err != nil {
		return err
	}
	defer st.Close()
	if ok, err := st.Formatted(ctx); err != nil {
		return err
	} else if !ok {
		if err := st.Format(ctx, store.DefaultBlockSize, uint32(os.Getuid()), uint32(os.Getgid()), 0o755); err != nil {
			return err
		}
		log.Printf("database was empty, created a new file system")
	}
	if err := st.Load(ctx); err != nil {
		return err
	}
	if err := st.LockMount(ctx); err != nil {
		return err
	}
	// The only recovery there is: files unlinked while open when the last
	// mount died. Everything else was committed or never happened.
	if n, err := st.ReapOrphans(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Printf("reclaimed %d files that were deleted while open", n)
	}

	server, err := fusefs.Mount(st, rest[1], fusefs.Options{AllowOther: *allowOther, ReadOnly: *readOnly, Debug: *debug})
	if err != nil {
		return err
	}
	log.Printf("%s mounted on %s (%s backend, sync=%s)", rest[0], rest[1], st.Backend(), *syncMode)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for range sig {
			if err := server.Unmount(); err != nil {
				log.Printf("unmount: %v (still in use? try again or run: fusermount -u %s)", err, rest[1])
			}
		}
	}()
	server.Wait()
	return st.Sync(ctx)
}

func fsck(ctx context.Context, args []string) error {
	rest := parse(flag.NewFlagSet("fsck", flag.ExitOnError), args, "<database>", 1)
	st, err := open(ctx, rest[0], store.SyncOnFsync)
	if err != nil {
		return err
	}
	defer st.Close()
	problems, orphans, err := st.Fsck(ctx)
	if err != nil {
		return err
	}
	for _, p := range problems {
		fmt.Printf("%s: %s\n", p.Check, p.Detail)
	}
	if orphans > 0 {
		fmt.Printf("%d files deleted while open are waiting to be reclaimed (normal while mounted or after a crash)\n", orphans)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d problems found", len(problems))
	}
	fmt.Println("clean")
	return nil
}

func info(ctx context.Context, args []string) error {
	rest := parse(flag.NewFlagSet("info", flag.ExitOnError), args, "<database>", 1)
	st, err := open(ctx, rest[0], store.SyncOnFsync)
	if err != nil {
		return err
	}
	defer st.Close()
	i, err := st.Info(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("backend:     %s\ncreated:     %s\nblock size:  %d\ninodes:      %d (%d directories, %d files)\nfile bytes:  %d\nstored:      %d bytes in data blocks\n",
		i.Backend, i.Created, i.BlockSize, i.Inodes, i.Dirs, i.Files, i.Bytes, i.Stored)
	return nil
}
