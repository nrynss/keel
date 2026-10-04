// Package stream fans server-sent events out to topic subscribers.
//
// Long work is started by a short request and observed over a stream. A
// producer publishes an Event to a topic, and every subscriber joined to
// that topic receives it framed as a server-sent event. Cloudflare ends a
// proxied request at 100 seconds, so nothing may hold a response open
// while work computes.
//
// Each topic keeps a bounded ring of recent events. ServeTopic reads the
// Last-Event-ID header and, when that id is still in the ring, writes
// every later event before joining the live subscription. The ring is
// best-effort memory, not a durable log. A subscriber whose id has fallen
// out of the ring is a fresh subscriber, and a consumer that cannot
// tolerate a miss reads the authoritative state on reconnect. Comment
// frames are never stored, so a ping never replays and never moves the
// cursor.
package stream

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultHeartbeat is the quiet period after which a stream writes a ping
// comment. It is short enough that an intermediary keeps the connection
// open and long enough that a ping rarely joins real traffic.
const defaultHeartbeat = 15 * time.Second

// defaultBuffer is the per-subscriber event buffer a Broker built from the
// zero Config gets. It holds a burst of progress events without dropping.
const defaultBuffer = 16

// defaultRetain is how long a Broker built from the zero Config keeps a
// topic's last terminal event for a late subscriber. It is long enough for
// a page reload to reconnect and short enough to release the topic soon.
const defaultRetain = time.Minute

// defaultReplay is how many recent events a topic keeps for a reconnect
// that sends Last-Event-ID. The ring is counted in events, not bytes.
const defaultReplay = 512

// Config carries the Broker's knobs. The zero value is usable and means
// defaultHeartbeat, defaultBuffer, defaultRetain and defaultReplay.
type Config struct {
	// Heartbeat is the quiet period between ping comments on an open
	// connection. Zero or negative means defaultHeartbeat.
	Heartbeat time.Duration

	// Buffer is the per-subscriber event buffer. Zero or negative means
	// defaultBuffer.
	Buffer int

	// Retain is how long a topic keeps its last terminal event for
	// replay to a subscriber that joins late. Zero or negative means
	// defaultRetain. The same bound applies to a topic that has no
	// terminal event and no subscriber: the replay ring is dropped
	// with the topic once Retain has passed since the last publish.
	Retain time.Duration

	// Replay is how many recent events a topic keeps so a reconnect can
	// resume after Last-Event-ID. Zero or negative means defaultReplay.
	// The oldest event is dropped to stay inside the bound. Replay does
	// not change Retain, which still governs only the terminal event a
	// late subscriber receives when it sends no cursor.
	Replay int
}

// withDefaults returns cfg with its zero and negative fields substituted.
func (cfg Config) withDefaults() Config {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = defaultHeartbeat
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = defaultBuffer
	}
	if cfg.Retain <= 0 {
		cfg.Retain = defaultRetain
	}
	if cfg.Replay <= 0 {
		cfg.Replay = defaultReplay
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
// ID is assigned by the Broker at publish time and increases for the
// life of the Broker, not per topic. A topic that is dropped and created
// again does not reuse an earlier id, so a Last-Event-ID from the old
// incarnation cannot select a frame in the new ring. A publisher leaves
// ID at zero, and the Broker overwrites any value.
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

// topicState holds the subscribers of one topic, its retained terminal
// event, and the replay ring. Event ids come from the broker, so a new
// topic does not start them over.
type topicState struct {
	subs     map[uint64]*subscriber
	terminal *retained
	ring     *eventRing
	idle     *time.Timer
}

// eventRing is a topic's recent events, oldest first, capped at limit.
type eventRing struct {
	buf   []Event
	limit int
}

// add appends event, dropping the oldest when the ring is full.
func (r *eventRing) add(event Event) {
	if r == nil || r.limit <= 0 {
		return
	}
	if len(r.buf) == r.limit {
		copy(r.buf, r.buf[1:])
		r.buf[len(r.buf)-1] = event
		return
	}
	r.buf = append(r.buf, event)
}

// has reports whether id is still in the ring.
func (r *eventRing) has(id uint64) bool {
	if r == nil {
		return false
	}
	for _, event := range r.buf {
		if event.ID == id {
			return true
		}
	}
	return false
}

// after copies every event published after id. id must be in the ring.
func (r *eventRing) after(id uint64) []Event {
	for i, event := range r.buf {
		if event.ID == id {
			out := make([]Event, len(r.buf)-i-1)
			copy(out, r.buf[i+1:])
			return out
		}
	}
	return nil
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
//
// Publish also records the event on the topic's ring, even when nobody is
// listening, so a reconnect can catch up. The ring holds Config.Replay
// events and is dropped with the topic.
type Broker struct {
	cfg       Config
	mu        sync.Mutex
	nextSub   uint64
	nextEvent uint64
	topics    map[string]*topicState
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
// for heartbeat, buffer, retention and replay.
func New(cfg Config) *Broker {
	return &Broker{
		cfg:    cfg.withDefaults(),
		topics: make(map[string]*topicState),
	}
}

// newTopic returns an empty topic whose ring holds cfgReplay events.
func newTopic(replay int) *topicState {
	return &topicState{
		subs: make(map[uint64]*subscriber),
		ring: &eventRing{limit: replay},
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
	_, sub := b.catchUp(ctx, topic, 0, false)
	return sub
}

// liveSub is a subscriber that was registered under the lock, plus the
// subscription handed to the caller.
type liveSub struct {
	subscription *Subscription
	gone         chan struct{}
}

// catchUp is Subscribe, plus an optional replay of the ring. resume is
// true when the caller sent a Last-Event-ID that parsed as a positive id.
// When that id is still in the ring, replay is every event after it, in
// order, and sub receives only events published after the snapshot. When
// the id is missing, replay is nil and sub is an ordinary Subscribe.
//
// A replay that ends on a terminal event does not join the live
// subscription. The returned subscription is already closed, so the
// response ends after that frame the way a late subscribe to a finished
// topic does.
func (b *Broker) catchUp(ctx context.Context, topic string, after uint64, resume bool) ([]Event, *Subscription) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	t := b.topics[topic]
	if resume && t != nil && t.ring.has(after) {
		replay := t.ring.after(after)
		// A replay that already ends on the terminal is finished. So is
		// an empty replay whose cursor is the retained terminal: the
		// client has that frame, and holding the response open would
		// keep the topic alive for a subscriber that has nothing to
		// receive.
		finished := len(replay) > 0 && replay[len(replay)-1].Terminal
		cursorIsTerminal := len(replay) == 0 && t.terminal != nil &&
			t.terminal.event.ID == after && time.Now().Before(t.terminal.expires)
		if !finished && cursorIsTerminal {
			finished = true
		}
		if finished {
			b.mu.Unlock()
			ch := make(chan Event)
			close(ch)
			return replay, &Subscription{Events: ch, remove: func() {}}
		}
		live := b.register(topic, t)
		b.mu.Unlock()
		b.watch(ctx, live)
		return replay, live.subscription
	}
	if t != nil && t.terminal != nil && time.Now().Before(t.terminal.expires) {
		event := t.terminal.event
		b.mu.Unlock()
		ch := make(chan Event, 1)
		ch <- event
		close(ch)
		return nil, &Subscription{Events: ch, remove: func() {}}
	}
	if t == nil {
		t = newTopic(b.cfg.Replay)
		b.topics[topic] = t
	}
	live := b.register(topic, t)
	b.mu.Unlock()
	b.watch(ctx, live)
	return nil, live.subscription
}

// register adds one subscriber to t. Callers hold b.mu. An idle-drop timer
// is stopped, because the topic has a listener again.
func (b *Broker) register(topic string, t *topicState) liveSub {
	b.stopIdle(t)
	id := b.nextSub
	b.nextSub++
	gone := make(chan struct{})
	sub := &subscriber{
		ch:   make(chan Event, b.cfg.Buffer),
		gone: gone,
	}
	t.subs[id] = sub
	return liveSub{
		gone: gone,
		subscription: &Subscription{
			Events: sub.ch,
			remove: func() { b.unsubscribe(topic, id) },
		},
	}
}

// watch removes the subscriber when ctx ends. Its exit path is ctx.Done
// or the subscription ending, whichever comes first, so it never outlives
// the subscription.
func (b *Broker) watch(ctx context.Context, live liveSub) {
	go func() {
		select {
		case <-ctx.Done():
			live.subscription.Cancel()
		case <-live.gone:
		}
	}()
}

// Publish delivers event to every current subscriber on topic without
// blocking. A subscriber whose buffer is full has its oldest buffered
// event dropped, per the Broker policy.
//
// The event is recorded on the topic's ring whether or not anyone is
// listening, and it receives the broker's next id. A topic with no
// subscriber and no terminal event is kept for Retain after this publish
// so a reconnect can still find the id.
//
// A terminal event is retained on its topic so a later subscriber still
// receives it. Publishing on a nil Broker is always a no-op.
func (b *Broker) Publish(topic string, event Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.topics[topic]
	if t == nil {
		t = newTopic(b.cfg.Replay)
		b.topics[topic] = t
	}
	b.nextEvent++
	event.ID = b.nextEvent
	t.ring.add(event)
	for _, sub := range t.subs {
		deliver(sub, event)
	}
	if event.Terminal {
		b.retain(topic, t, event)
		b.stopIdle(t)
		return
	}
	if len(t.subs) == 0 {
		b.armIdle(topic, t)
		return
	}
	b.stopIdle(t)
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

// armIdle keeps t until Retain has passed without a subscriber, then drops
// the topic and its ring. A newer publish or a new subscriber stops the
// previous timer. Callers hold b.mu.
func (b *Broker) armIdle(topic string, t *topicState) {
	b.stopIdle(t)
	t.idle = time.AfterFunc(b.cfg.Retain, func() { b.dropIdle(topic, t) })
}

// stopIdle cancels a topic's idle-drop timer. Callers hold b.mu.
func (b *Broker) stopIdle(t *topicState) {
	if t.idle == nil {
		return
	}
	t.idle.Stop()
	t.idle = nil
}

// dropIdle removes a topic that still has no subscriber and no terminal
// event. A subscriber that arrived after the timer fired keeps the topic.
func (b *Broker) dropIdle(topic string, t *topicState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.topics[topic] != t || len(t.subs) != 0 || t.terminal != nil {
		return
	}
	delete(b.topics, topic)
}

// expire clears a topic's terminal event once its retention passes. It
// drops the topic when it holds no subscriber, so a finished topic does
// not live until the process ends. The replay ring goes with it.
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
// watcher racing Cancel is a no-op. A topic with no subscriber and no
// terminal event is kept for Retain so its ring can still satisfy a
// Last-Event-ID. Callers do not hold b.mu.
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
		b.armIdle(topic, t)
	}
	close(sub.ch)
	close(sub.gone)
}

// subscribers reports how many live subscriptions topic has. It serves
// lifecycle tests and diagnostics, and it says nothing about subscriber
// identities.
func (b *Broker) subscribers(topic string) int {
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

// lastEventID reads the Last-Event-ID header. The boolean is false when
// the header is missing, blank, unparsable, or zero. Zero is the cursor
// an empty id line resets a client to, and this broker's ids start at
// one, so it never selects a ring entry. A false result means ServeTopic
// behaves as a subscribe with no cursor.
func lastEventID(r *http.Request) (uint64, bool) {
	if r == nil {
		return 0, false
	}
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}
