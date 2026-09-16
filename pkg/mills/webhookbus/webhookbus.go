package webhookbus

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Event is the routing information extracted from a GitLab webhook. Payload
// state is deliberately omitted: consumers must re-read GitLab as the source
// of truth.
type Event struct {
	Project string
	SHA     string
	MRIID   int64
}

var EventsDropped = promauto.NewCounter(prometheus.CounterOpts{
	Name: "mills_webhook_events_dropped_total",
	Help: "GitLab webhook events dropped from full subscriber buffers.",
})

type subscription struct {
	project string
	sha     string
	mrIID   int64
	ch      chan Event
}

// Bus is a concurrency-safe, bounded, in-process webhook hint bus.
type Bus struct {
	mu     sync.RWMutex
	nextID uint64
	buffer int
	subs   map[uint64]*subscription
}

func New(buffer int) *Bus {
	if buffer < 1 {
		buffer = 1
	}
	return &Bus{buffer: buffer, subs: make(map[uint64]*subscription)}
}

// Subscribe registers a subscriber matching either the non-empty SHA or the
// positive MR IID within project. Unsubscribe is safe to call repeatedly.
func (b *Bus) Subscribe(project, sha string, mrIID int64) (<-chan Event, func()) {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	s := &subscription{project: project, sha: sha, mrIID: mrIID, ch: make(chan Event, b.buffer)}
	b.subs[id] = s
	b.mu.Unlock()
	var once sync.Once
	return s.ch, func() { once.Do(func() { b.mu.Lock(); delete(b.subs, id); b.mu.Unlock() }) }
}

// Publish never blocks. When a matching buffer is full it discards its oldest
// hint, counts the overflow, and retains the newest hint.
func (b *Bus) Publish(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		if s.project != event.Project || ((s.sha == "" || s.sha != event.SHA) && (s.mrIID <= 0 || s.mrIID != event.MRIID)) {
			continue
		}
		select {
		case s.ch <- event:
		default:
			select {
			case <-s.ch:
				EventsDropped.Inc()
			default:
			}
			select {
			case s.ch <- event:
			default:
				EventsDropped.Inc()
			}
		}
	}
}

func (b *Bus) SubscriberCount() int { b.mu.RLock(); defer b.mu.RUnlock(); return len(b.subs) }

var Default = New(8)
