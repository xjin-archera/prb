package runner

import "sync"

// Broadcaster fans lines out to subscribers per key, with a ring buffer for late joiners.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[string]map[chan string]struct{}
	buf  map[string][]string
	keep int
}

func NewBroadcaster(keep int) *Broadcaster {
	return &Broadcaster{subs: map[string]map[chan string]struct{}{}, buf: map[string][]string{}, keep: keep}
}

func (b *Broadcaster) Publish(key, line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.keep > 0 {
		buf := append(b.buf[key], line)
		if len(buf) > b.keep {
			buf = buf[len(buf)-b.keep:]
		}
		b.buf[key] = buf
	}
	for ch := range b.subs[key] {
		select {
		case ch <- line:
		default: // slow subscriber: drop rather than block the job
		}
	}
}

func (b *Broadcaster) Reset(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.buf, key)
}

// Subscribe returns a channel, the buffered backlog, and an unsubscribe func.
func (b *Broadcaster) Subscribe(key string) (<-chan string, []string, func()) {
	ch := make(chan string, 512)
	b.mu.Lock()
	if b.subs[key] == nil {
		b.subs[key] = map[chan string]struct{}{}
	}
	b.subs[key][ch] = struct{}{}
	backlog := append([]string(nil), b.buf[key]...)
	b.mu.Unlock()
	return ch, backlog, func() {
		b.mu.Lock()
		delete(b.subs[key], ch)
		b.mu.Unlock()
	}
}
