package diskfree

import (
	"errors"
	"os"
	"sync"
)

// ErrNoSpace is returned by Reserve when the volume cannot hold the worst
// case of every write in progress plus the new one.
var ErrNoSpace = errors.New("Not enough free space for the worst-case NUP size.")

// headroom is kept free on every volume beyond the reservations.
const headroom = 32 << 20

type reservation struct {
	volume  string
	need    int64
	written func() int64 // bytes written so far; they are already off the free space
}

var (
	mu   sync.Mutex
	held = map[*reservation]bool{}
	// available is Available, replaceable in tests: a real volume's free
	// space moves with every other writer on it.
	available = Available
)

// Reserve claims need bytes on the volume holding dir for a write into temp,
// so concurrent jobs in this process cannot each count the same free space.
// The free space the OS reports already excludes what the jobs have written
// so far; each reservation therefore only counts what its temp file may
// still grow by. Release once the file is committed or removed.
func Reserve(dir, temp string, need int64) (release func(), err error) {
	return ReserveFunc(dir, func() int64 {
		if st, err := os.Stat(temp); err == nil {
			return st.Size()
		}
		return 0
	}, need)
}

// ReserveFunc is Reserve for output that is not one file: written reports
// how many bytes the job has written so far. It is called with the
// package's lock held and must not call back into this package.
func ReserveFunc(dir string, written func() int64, need int64) (release func(), err error) {
	vol, err := volumeKey(dir)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	free, err := available(dir)
	if err != nil {
		return nil, err
	}
	var pending int64
	for r := range held {
		if r.volume != vol {
			continue
		}
		left := r.need - r.written()
		if left > 0 {
			pending += left
		}
	}
	if need < 0 || free < uint64(pending)+uint64(need)+headroom {
		return nil, ErrNoSpace
	}
	r := &reservation{volume: vol, need: need, written: written}
	held[r] = true
	return func() {
		mu.Lock()
		delete(held, r)
		mu.Unlock()
	}, nil
}
