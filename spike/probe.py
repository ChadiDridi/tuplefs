import os, sys, mmap, time
mnt, log = sys.argv[1], sys.argv[2]
f = mnt + "/f"
def mark(s):
    time.sleep(0.2)
    open(log, "a").write("--- " + s + "\n")
def attempt(label, fn):
    try:
        r = fn(); print(f"{label}: ok {r if r is not None else ''}")
    except OSError as e:
        print(f"{label}: ERROR {e.strerror}")

mark("create")
fd = os.open(f, os.O_RDWR | os.O_CREAT, 0o644)
mark("write 1MiB through the created handle")
os.pwrite(fd, b"a" * (1 << 20), 0)
mark("stat after write")
print("size seen after write:", os.stat(f).st_size)
mark("read back")
os.pread(fd, 1 << 20, 0)
mark("fsync"); os.fsync(fd)
mark("ftruncate 4096"); os.ftruncate(fd, 4096)
mark("fallocate 8192"); attempt("fallocate", lambda: os.posix_fallocate(fd, 0, 8192))
mark("mmap write")
def mm():
    m = mmap.mmap(fd, 4096); m[0:4] = b"MMAP"; m.flush(); m.close()
attempt("shared mmap write", mm)
mark("second passthrough open while first is open")
attempt("second passthrough open", lambda: os.close(os.open(f, os.O_RDWR)))
mark("plain (O_SYNC) open while a passthrough handle is open")
attempt("plain open beside passthrough", lambda: os.close(os.open(f, os.O_RDWR | os.O_SYNC)))
os.close(fd)
mark("plain open alone, then write and read")
fd = os.open(f, os.O_RDWR | os.O_SYNC)
os.pwrite(fd, b"b" * 8192, 0); os.pread(fd, 8192, 0)
mark("passthrough open while a plain handle is open")
attempt("passthrough open beside plain", lambda: os.close(os.open(f, os.O_RDWR)))
os.close(fd)
mark("end")
