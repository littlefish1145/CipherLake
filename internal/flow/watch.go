package flow

import (
	"fmt"
	"sync"
)

type Watcher struct {
	mu          sync.RWMutex
	subscribers map[string]chan Event
	nextID      int
}

func NewWatcher() *Watcher {
	return &Watcher{
		subscribers: make(map[string]chan Event),
	}
}

func (w *Watcher) Watch(buf int) (<-chan Event, func()) {
	if buf <= 0 {
		buf = 64
	}
	ch := make(chan Event, buf)
	w.mu.Lock()
	id := fmt.Sprintf("w%d", w.nextID)
	w.nextID++
	w.subscribers[id] = ch
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.subscribers, id)
		w.mu.Unlock()
		close(ch)
	}
}

func (w *Watcher) Feed(e Event) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, ch := range w.subscribers {
<<<<<<< HEAD
		ch <- e
=======
		select {
		case ch <- e:
		default:
		}
>>>>>>> f0a7576 (feat: implement nexus-flow embedded workflow kernel + pipeline fixes)
	}
}
