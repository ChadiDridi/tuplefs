// Command tuplefs creates, mounts and checks file systems stored in a SQL
// database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/ChadiDridi/tuplefs/internal/backing"
	"github.com/ChadiDridi/tuplefs/internal/fusefs"
	"github.com/ChadiDridi/tuplefs/internal/store"
)

var version = "dev"

const usage = `tuplefs stores a POSIX file system in a SQL database.

Usage:
  tuplefs mount [flags] <database> <mountpoint>   mount it (creates it if the database is empty)
  tuplefs mkfs  [flags] <database>                create an empty file system
  tuplefs fsck  [flags] <database>                verify every invariant, read-only
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
	data := fs.String("data", "db", `where file contents live: "db" (rows in the database, fully transactional) or "files" (backing files next to it, as fast as the disk)`)
	dataDir := fs.String("data-dir", "", `directory for backing files with -data files (default: <database>.data)`)
	rest := parse(fs, args, "<database>", 1)
	if *data != "db" && *data != "files" {
		return fmt.Errorf(`-data must be "db" or "files", not %q`, *data)
	}
	st, err := store.Open(rest[0], store.SyncAlways)
	if err != nil {
		return err
	}
	defer st.Close()
	opts := store.FormatOptions{BlockSize: *blockSize, Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), Mode: uint32(*mode), DataFiles: *data == "files"}
	if !opts.DataFiles {
		if err := st.FormatWith(ctx, opts); err != nil {
			return err
		}
		fmt.Printf("created a %s file system with %d-byte blocks\n", st.Backend(), *blockSize)
		return nil
	}
	dir, err := dataDirPath(st, *dataDir)
	if err != nil {
		return err
	}
	if err := st.FormatWith(ctx, opts); err != nil {
		return err
	}
	if err := st.Load(ctx); err != nil {
		return err
	}
	if _, err := backing.Create(dir, st.UUID()); err != nil {
		return err
	}
	fmt.Printf("created a %s file system with file contents in %s\n", st.Backend(), dir)
	return nil
}

// dataDirPath returns the backing directory of a file system: the one given,
// or the default next to a SQLite database.
func dataDirPath(st *store.Store, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if st.Path() == "" {
		return "", fmt.Errorf("-data-dir is required: a PostgreSQL database has no directory to put backing files next to")
	}
	return st.Path() + ".data", nil
}

// openData opens the backing directory of a file system that uses one.
func openData(st *store.Store, flag string) (*backing.Dir, error) {
	if !st.DataFiles() {
		return nil, nil
	}
	dir, err := dataDirPath(st, flag)
	if err != nil {
		return nil, err
	}
	return backing.Open(dir, st.UUID())
}

func mount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	syncMode := fs.String("sync", "fsync", `when changes reach stable storage: "fsync" (on fsync, like a local file system) or "always" (every operation)`)
	allowOther := fs.Bool("allow-other", false, "let other users access the mount (needs user_allow_other in /etc/fuse.conf)")
	readOnly := fs.Bool("ro", false, "mount read-only")
	dataDir := fs.String("data-dir", "", "directory of backing files, for a file system made with mkfs -data files (default: <database>.data)")
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
		if *readOnly {
			return fmt.Errorf("%s holds no file system, and -ro cannot create one", redact(rest[0]))
		}
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
	data, err := openData(st, *dataDir)
	if err != nil {
		return err
	}
	if data != nil {
		st.OnDelete(func(ino uint64) { data.Remove(ino) })
	}
	// The only recovery there is: files unlinked while open when the last
	// mount died. Everything else was committed or never happened. A
	// read-only mount leaves them for the next writable one.
	if !*readOnly {
		if n, err := st.ReapOrphans(ctx); err != nil {
			return err
		} else if n > 0 {
			log.Printf("reclaimed %d files that were deleted while open", n)
		}
	}
	if data != nil && !*readOnly {
		// Backing files are written outside the database. If the last mount
		// died, sizes recorded in the database may trail them, and a deleted
		// file's data may still be on disk. Fix both from the facts on disk.
		if clean, err := st.Clean(ctx); err != nil {
			return err
		} else if !clean {
			sizes, removed, err := fusefs.Reconcile(ctx, st, data)
			if err != nil {
				return err
			}
			log.Printf("last mount did not shut down cleanly: corrected %d recorded sizes, removed %d leftover backing files", sizes, removed)
		}
		if err := st.SetClean(ctx, false); err != nil {
			return err
		}
	}

	server, fsys, err := fusefs.Mount(st, rest[1], fusefs.Options{AllowOther: *allowOther, ReadOnly: *readOnly, Debug: *debug, Data: data})
	if err != nil {
		return err
	}
	layout := "data in the database"
	if data != nil {
		layout = "data in " + data.Root()
	}
	log.Printf("%s mounted on %s (%s backend, %s, sync=%s)", redact(rest[0]), rest[1], st.Backend(), layout, *syncMode)
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
	if *readOnly {
		return nil
	}
	if err := fsys.Close(ctx); err != nil {
		return err
	}
	if data != nil {
		if err := st.SetClean(ctx, true); err != nil {
			return err
		}
	}
	return st.Sync(ctx)
}

// redact hides the password of a database URL in log output.
func redact(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		return u.Redacted()
	}
	return dsn
}

func fsck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fsck", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "directory of backing files (default: <database>.data)")
	rest := parse(fs, args, "<database>", 1)
	st, err := open(ctx, rest[0], store.SyncOnFsync)
	if err != nil {
		return err
	}
	defer st.Close()
	problems, orphans, err := st.Fsck(ctx)
	if err != nil {
		return err
	}
	if data, err := openData(st, *dataDir); err != nil {
		return err
	} else if data != nil {
		if clean, err := st.Clean(ctx); err != nil {
			return err
		} else if clean {
			more, err := fusefs.CheckFiles(ctx, st, data)
			if err != nil {
				return err
			}
			problems = append(problems, more...)
		} else {
			fmt.Println("mounted, or not cleanly unmounted: backing files are not compared (the next mount brings the database up to date)")
		}
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
