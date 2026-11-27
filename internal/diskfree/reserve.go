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
	volume string
	need   int64
	temp   string // the file being written; its current size is already off the free space
}

var (
	mu   sync.Mutex
	held = map[*reservation]bool{}
)

// Reserve claims need bytes on the volume holding dir for a write into temp,
// so concurrent jobs in this process cannot each count the same free space.
// The free space the OS reports already excludes what the jobs have written
// so far; each reservation therefore only counts what its temp file may
// still grow by. Release once the file is committed or removed.
func Reserve(dir, temp string, need int64) (release func(), err error) {
	vol, err := volumeKey(dir)
	if err != nil {
		return nil, err
	}
	mu.Lock()
	defer mu.Unlock()
	free, err := Available(dir)
	if err != nil {
		return nil, err
	}
	var pending int64
	for r := range held {
		if r.volume != vol {
			continue
		}
		left := r.need
		if st, err := os.Stat(r.temp); err == nil {
			left -= st.Size()
		}
		if left > 0 {
			pending += left
		}
	}
	if need < 0 || free < uint64(pending)+uint64(need)+headroom {
		return nil, ErrNoSpace
	}
	r := &reservation{volume: vol, need: need, temp: temp}
	held[r] = true
	return func() {
		mu.Lock()
		delete(held, r)
		mu.Unlock()
	}, nil
}
