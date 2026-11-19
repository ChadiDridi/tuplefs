package fusefs

import (
	"context"
	"fmt"

	"github.com/ChadiDridi/tuplefs/internal/backing"
	"github.com/ChadiDridi/tuplefs/internal/store"
)

// Reconcile brings the database and the backing directory back in line after
// a mount that did not shut down cleanly. The backing files are the authority
// on size and modification time, and the database is the authority on which
// inodes exist: stale sizes are corrected, and backing files whose inode is
// gone are removed. It returns how many of each it fixed.
func Reconcile(ctx context.Context, st *store.Store, dir *backing.Dir) (sizes, removed int, err error) {
	recorded, err := st.RegularFiles(ctx)
	if err != nil {
		return 0, 0, err
	}
	exists := make(map[uint64]bool, len(recorded))
	var stale []store.FileState
	for _, r := range recorded {
		exists[r.Ino] = true
		actual, ok, err := dir.Stat(r.Ino)
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			actual = backing.State{MtimeNs: r.MtimeNs} // never written: empty
		}
		if actual.Size != r.Size || actual.MtimeNs != r.MtimeNs {
			stale = append(stale, store.FileState{Ino: r.Ino, Size: actual.Size, MtimeNs: actual.MtimeNs})
		}
	}
	if err := st.UpdateFileStates(ctx, stale); err != nil {
		return 0, 0, err
	}
	err = dir.Inodes(func(ino uint64) error {
		if exists[ino] {
			return nil
		}
		removed++
		return dir.Remove(ino)
	})
	return len(stale), removed, err
}

// CheckFiles is the part of fsck that compares the database with the backing
// directory. It only makes sense for a cleanly unmounted file system: while
// one is mounted, sizes in the database trail the backing files by design.
func CheckFiles(ctx context.Context, st *store.Store, dir *backing.Dir) ([]store.Problem, error) {
	recorded, err := st.RegularFiles(ctx)
	if err != nil {
		return nil, err
	}
	var problems []store.Problem
	exists := make(map[uint64]bool, len(recorded))
	for _, r := range recorded {
		exists[r.Ino] = true
		actual, _, err := dir.Stat(r.Ino)
		if err != nil {
			return nil, err
		}
		if actual.Size != r.Size {
			problems = append(problems, store.Problem{
				Check:  "recorded size differs from the backing file",
				Detail: fmt.Sprintf("ino %d recorded %d actual %d", r.Ino, r.Size, actual.Size),
			})
		}
	}
	err = dir.Inodes(func(ino uint64) error {
		if !exists[ino] {
			problems = append(problems, store.Problem{Check: "backing file without an inode", Detail: fmt.Sprintf("ino %d", ino)})
		}
		return nil
	})
	return problems, err
}
