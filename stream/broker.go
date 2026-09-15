// Package stream fans server-sent events out to topic subscribers.
//
// Long work is started by a short request and observed over a stream. A
// producer publishes an Event to a topic, and every subscriber joined to
// that topic receives it framed as a server-sent event. Cloudflare ends a
// proxied request at 100 seconds, so nothing may hold a response open
// while work computes.
package stream

import (
	"context"
	"sync"
	"time"
)

// DefaultHeartbeat is the quiet period after which a stream writes a ping
// comment. It is short enough that an intermediary keeps the connection
// open and long enough that a ping rarely joins real traffic.
const DefaultHeartbeat = 15 * time.Second

// DefaultBuffer is the per-subscriber event buffer a Broker built from the
// zero Config gets. It holds a burst of progress events without dropping.
const DefaultBuffer = 16

// DefaultRetain is how long a Broker built from the zero Config keeps a
// topic's last terminal event for a late subscriber. It is long enough for
// a page reload to reconnect and short enough to release the topic soon.
const DefaultRetain = time.Minute

// Config carries the Broker's knobs. The zero value is usable and means
// DefaultHeartbeat, DefaultBuffer and DefaultRetain.
type Config struct {
	// Heartbeat is the quiet period between ping comments on an open
	// connection. Zero or negative means DefaultHeartbeat.
	Heartbeat time.Duration

	// Buffer is the per-subscriber event buffer. Zero or negative means
	// DefaultBuffer.
	Buffer int

	// Retain is how long a topic keeps its last terminal event for
	// replay to a subscriber that joins late. Zero or negative means
	// DefaultRetain.
	Retain time.Duration
}

// withDefaults returns cfg with its zero and negative fields substituted.
func (cfg Config) withDefaults() Config {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultBuffer
	}
	if cfg.Retain <= 0 {
		cfg.Retain = DefaultRetain
	}
	return cfg
}

// Event is one server-sent event. Name is the frame's event field. Data is
// the payload, which the frame writer encodes as one compact JSON line.
//
// Terminal marks an event that ends the topic's work. The Broker keeps the
// last terminal event for its configured retention, so a subscriber that
// joins after it still receives it.
//
// ID is assigned by the Broker at publish time and increases per topic. A
// publisher leaves it at zero, and the Broker overwrites any value.
type Event struct {
	Name     string
	Data     any
	Terminal bool
	ID       uint64
}

// subscriber is one registered consumer. ch carries framed events to its
// writer loop. gone closes the moment the subscriber is removed, so the
// context watcher has a defined exit path.
type subscriber struct {
	ch   chan Event
	gone chan struct{}
}

// topicState holds the subscribers of one topic, the id counter for its
// frames, and its retained terminal event.
type topicState struct {
	subs     map[uint64]*subscriber
	nextID   uint64
	terminal *retained
}

// retained is one topic's last terminal event plus the timer that clears
// it. The timer fires once after the retention, and a newer terminal stops
// it early.
type retained struct {
	event   Event
	expires time.Time
	timer   *time.Timer
}

// Broker fans events out to topic subscribers without ever blocking on a
// slow one.
//
// A subscriber has a bounded buffer. When it is full, the OLDEST buffered
// event is dropped to make room for the new one, so Publish never blocks
// and never disconnects the subscriber. The newest event always fits,
// because progress supersedes its own earlier reports. A subscriber that
// must not miss an event reads the authoritative state instead of the
// stream.
type Broker struct {
	cfg     Config
	mu      sync.Mutex
	nextSub uint64
	topics  map[string]*topicState
}

// Subscription is one consumer's registration on one topic. Events is
// closed when the subscription ends, so a range loop over it terminates.
// Cancel is safe to call more than once and safe on a nil Subscription.
type Subscription struct {
	Events <-chan Event

	once   sync.Once
	remove func()
}

// Cancel removes the subscription. A stream calls it when the response
// ends, and the context watcher calls it on disconnect. Either path
// removes the subscriber exactly once.
func (s *Subscription) Cancel() {
	if s == nil {
		return
	}
	s.once.Do(func() { s.remove() })
}

// New returns an empty Broker. The zero Config means the package defaults
// for heartbeat, buffer and retention.
func New(cfg Config) *Broker {
	return &Broker{
		cfg:    cfg.withDefaults(),
		topics: make(map[string]*topicState),
	}
}

// Subscribe registers a subscriber on topic. Events receives every event
// published to the topic from now on.
//
// When the topic already holds a live terminal event, Subscribe returns a
// subscription that carries that one event and then closes, so a late
// reader learns the outcome without waiting. The retained event keeps the
// id it was published under, so a client can still deduplicate it.
//
// The subscription ends when ctx is done or Cancel runs. Both paths remove
// the subscriber, so a topic holds no writer for a closed connection. Pass
// a context that ends with the connection, such as a request context. A
// caller that does not cancel must still let that context end.
func (b *Broker) Subscribe(ctx context.Context, topic string) *Subscription {
	if ctx == nil {
		ctx = context.Background()
	}

	b.mu.Lock()
	t := b.topics[topic]
	if t != nil && t.terminal != nil && time.Now().Before(t.terminal.expires) {
		event := t.terminal.event
		b.mu.Unlock()
		ch := make(chan Event, 1)
		ch <- event
		close(ch)
		return &Subscription{Events: ch, remove: func() {}}
	}
	if t == nil {
		t = &topicState{subs: make(map[uint64]*subscriber)}
		b.topics[topic] = t
	}
	id := b.nextSub
	b.nextSub++
	sub := &subscriber{
		ch:   make(chan Event, b.cfg.Buffer),
		gone: make(chan struct{}),
	}
	t.subs[id] = sub
	b.mu.Unlock()

	s := &Subscription{
		Events: sub.ch,
		remove: func() { b.unsubscribe(topic, id) },
	}

	// Watcher removes the subscriber when the caller's context ends. Its
	// exit path is ctx.Done or the subscription ending, whichever comes
	// first, so it never outlives the subscription.
	go func() {
		select {
		case <-ctx.Done():
			s.Cancel()
		case <-sub.gone:
		}
	}()

	return s
}

// Publish delivers event to every current subscriber on topic without
// blocking. A subscriber whose buffer is full has its oldest buffered
// event dropped, per the Broker policy.
//
// A terminal event is retained on its topic so a later subscriber still
// receives it. Publishing a non-terminal event to a topic with no
// subscribers is a no-op. Publishing on a nil Broker is always a no-op.
func (b *Broker) Publish(topic string, event Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.topics[topic]
	if t == nil {
		if !event.Terminal {
			return
		}
		t = &topicState{subs: make(map[uint64]*subscriber)}
		b.topics[topic] = t
	}
	t.nextID++
	event.ID = t.nextID
	for _, sub := range t.subs {
		deliver(sub, event)
	}
	if event.Terminal {
		b.retain(topic, t, event)
	}
}

// deliver hands event to one subscriber. A full buffer drops its oldest
// event first. Callers hold b.mu, so no other publisher races the
// drain-and-send pair.
func deliver(sub *subscriber, event Event) {
	select {
	case sub.ch <- event:
	default:
		select {
		case <-sub.ch:
		default:
		}
		select {
		case sub.ch <- event:
		default:
		}
	}
}

// retain stores event as the topic's terminal event and arms its expiry
// timer. It replaces any earlier terminal and stops that timer. Callers
// hold b.mu.
func (b *Broker) retain(topic string, t *topicState, event Event) {
	if t.terminal != nil && t.terminal.timer != nil {
		t.terminal.timer.Stop()
	}
	t.terminal = &retained{event: event, expires: time.Now().Add(b.cfg.Retain)}
	// The timer fires once after the retention. A newer terminal stops it
	// early, and a removed topic drops the timer with the topic.
	t.terminal.timer = time.AfterFunc(b.cfg.Retain, func() { b.expire(topic, t) })
}

// expire clears a topic's terminal event once its retention passes. It
// drops the topic when it holds no subscriber, so a finished topic does
// not live until the process ends.
func (b *Broker) expire(topic string, t *topicState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.topics[topic] != t {
		return
	}
	if t.terminal != nil && time.Now().Before(t.terminal.expires) {
		return
	}
	t.terminal = nil
	if len(t.subs) == 0 {
		delete(b.topics, topic)
	}
}

// unsubscribe removes the subscriber and closes its channel. It is
// idempotent: the map lookup is the once-guard, so a second call or the
// watcher racing Cancel is a no-op. Callers hold b.mu.
func (b *Broker) unsubscribe(topic string, id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.topics[topic]
	if t == nil {
		return
	}
	sub, ok := t.subs[id]
	if !ok {
		return
	}
	delete(t.subs, id)
	if len(t.subs) == 0 && t.terminal == nil {
		delete(b.topics, topic)
	}
	close(sub.ch)
	close(sub.gone)
}

// Subscribers reports how many live subscriptions topic has. It serves
// lifecycle tests and diagnostics, and it says nothing about subscriber
// identities.
func (b *Broker) Subscribers(topic string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.topics[topic]
	if t == nil {
		return 0
	}
	return len(t.subs)
}
