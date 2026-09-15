package stream

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestSubscribeDefaultConfigSubstituted pins the zero-Config defaults:
// New(Config{}) must substitute DefaultHeartbeat, DefaultBuffer and
// DefaultRetain. A default nobody executes is a default nobody tests.
func TestSubscribeDefaultConfigSubstituted(t *testing.T) {
	b := New(Config{})
	if b.cfg.Heartbeat != DefaultHeartbeat {
		t.Errorf("heartbeat = %v, want %v (the default must be substituted, not zero)", b.cfg.Heartbeat, DefaultHeartbeat)
	}
	if b.cfg.Buffer != DefaultBuffer {
		t.Errorf("buffer = %d, want %d", b.cfg.Buffer, DefaultBuffer)
	}
	if b.cfg.Retain != DefaultRetain {
		t.Errorf("retain = %v, want %v", b.cfg.Retain, DefaultRetain)
	}
	neg := Config{Heartbeat: -1, Buffer: -5, Retain: -1}
	if got := neg.withDefaults(); got.Heartbeat != DefaultHeartbeat || got.Buffer != DefaultBuffer || got.Retain != DefaultRetain {
		t.Errorf("withDefaults on negative values = %+v, want the defaults", got)
	}
}

// TestPublishReachesTopicSubscriber: a subscriber receives events published
// to its topic, and only its topic.
func TestPublishReachesTopicSubscriber(t *testing.T) {
	b := New(Config{})
	sub := b.Subscribe(context.Background(), "job:1")
	defer sub.Cancel()

	b.Publish("job:2", Event{Name: "elsewhere", Data: "x"})
	b.Publish("job:1", Event{Name: "progress", Data: "page 1"})

	select {
	case ev := <-sub.Events:
		if ev.Name != "progress" || ev.Data != "page 1" {
			t.Errorf("event = %+v, want the job:1 progress event", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the event")
	}
}

// TestSlowSubscriberDropOldest pins the bounded-buffer policy: a
// subscriber that stops reading never blocks Publish, and the oldest
// buffered events are the ones lost, so the newest state wins.
func TestSlowSubscriberDropOldest(t *testing.T) {
	b := New(Config{Buffer: 2})
	sub := b.Subscribe(context.Background(), "t")
	defer sub.Cancel()

	// Five events (b..f) into a two-slot buffer nobody reads: the
	// surviving pair must be the two newest, e and f.
	for i := 1; i <= 5; i++ {
		b.Publish("t", Event{Name: "progress", Data: string(rune('a' + i))})
	}

	drain := make([]string, 0, 2)
	for len(drain) < 2 {
		select {
		case ev := <-sub.Events:
			drain = append(drain, ev.Data.(string))
		case <-time.After(time.Second):
			t.Fatalf("timed out after %d events: %v", len(drain), drain)
		}
	}
	if got, want := drain[0]+drain[1], "ef"; got != want {
		t.Errorf("buffered events = %v, want the two newest %q (oldest dropped)", drain, want)
	}
}

// TestPublishAssignsIncreasingIDsPerTopic: every published event carries
// the topic's next id, and a second topic numbers from its own first event.
func TestPublishAssignsIncreasingIDsPerTopic(t *testing.T) {
	b := New(Config{})
	sub := b.Subscribe(context.Background(), "t")
	defer sub.Cancel()

	for i := 0; i < 3; i++ {
		b.Publish("t", Event{Name: "progress", Data: i})
	}
	for want := uint64(1); want <= 3; want++ {
		select {
		case ev := <-sub.Events:
			if ev.ID != want {
				t.Errorf("id = %d, want %d", ev.ID, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for id %d", want)
		}
	}

	other := b.Subscribe(context.Background(), "u")
	defer other.Cancel()
	b.Publish("u", Event{Name: "progress", Data: 0})
	select {
	case ev := <-other.Events:
		if ev.ID != 1 {
			t.Errorf("second topic id = %d, want 1", ev.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the second topic event")
	}
}

// TestSubscribeAfterTerminalReplaysIt: a subscriber that joins after the
// terminal event receives it at once, with the id it was published under,
// and the subscription then ends because the topic is finished.
func TestSubscribeAfterTerminalReplaysIt(t *testing.T) {
	b := New(Config{})
	b.Publish("t", Event{Name: "done", Data: "finished", Terminal: true})

	sub := b.Subscribe(context.Background(), "t")
	defer sub.Cancel()

	select {
	case ev, ok := <-sub.Events:
		if !ok {
			t.Fatal("Events closed before the retained terminal arrived")
		}
		if ev.Name != "done" || ev.Data != "finished" || ev.ID != 1 {
			t.Errorf("replayed event = %+v, want the retained terminal with id 1", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the retained terminal")
	}
	if _, ok := <-sub.Events; ok {
		t.Error("a replay subscription delivered more than the terminal")
	}
	if got := b.Subscribers("t"); got != 0 {
		t.Errorf("Subscribers = %d, want 0 (a replay registers no subscriber)", got)
	}
}

// TestTerminalReplayExpires: once the retention passes, a late subscriber
// is a normal live subscription and no longer receives the terminal.
func TestTerminalReplayExpires(t *testing.T) {
	b := New(Config{Retain: 20 * time.Millisecond})
	b.Publish("t", Event{Name: "done", Data: "finished", Terminal: true})

	time.Sleep(60 * time.Millisecond)

	sub := b.Subscribe(context.Background(), "t")
	defer sub.Cancel()

	select {
	case ev := <-sub.Events:
		t.Fatalf("replayed a terminal past its retention: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	if got := b.Subscribers("t"); got != 1 {
		t.Errorf("Subscribers = %d, want 1 (a live subscription, not a replay)", got)
	}
}

// TestCancelRemovesSubscriberAndClosesEvents: Cancel removes the
// subscriber and closes Events, so a range loop terminates.
func TestCancelRemovesSubscriberAndClosesEvents(t *testing.T) {
	b := New(Config{})
	sub := b.Subscribe(context.Background(), "t")

	sub.Cancel()
	if got := b.Subscribers("t"); got != 0 {
		t.Errorf("Subscribers after Cancel = %d, want 0", got)
	}
	for range sub.Events {
		t.Error("Events received a value after Cancel")
	}

	// Idempotent, and safe on a nil Subscription.
	sub.Cancel()
	var nilSub *Subscription
	nilSub.Cancel()
}

// TestSubscribeCtxDoneRemovesSubscriber pins the disconnect contract: a
// subscriber whose context ends is removed without anyone calling Cancel,
// because the watcher goroutine does it. A topic then holds no writer for
// a connection that went away.
func TestSubscribeCtxDoneRemovesSubscriber(t *testing.T) {
	b := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	sub := b.Subscribe(ctx, "t")
	defer sub.Cancel()

	cancel()
	deadline := time.Now().Add(time.Second)
	for b.Subscribers("t") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscriber still registered 1s after its context ended")
		}
		time.Sleep(time.Millisecond)
	}
	for range sub.Events {
		t.Error("Events received a value after the context ended")
	}
}

// TestSubscribeNilCtxUsesBackground: Subscribe documents nil ctx as
// background, and the subscription must still be removable via Cancel.
func TestSubscribeNilCtxUsesBackground(t *testing.T) {
	b := New(Config{})
	//lint:ignore SA1012 the nil context pins the documented background default
	sub := b.Subscribe(nil, "t")
	sub.Cancel()
	if got := b.Subscribers("t"); got != 0 {
		t.Errorf("Subscribers = %d, want 0", got)
	}
}

// TestNilBrokerIsUsable: a nil Broker accepts Publish and reports no
// subscribers, so a caller may hold one without a nil check.
func TestNilBrokerIsUsable(t *testing.T) {
	var b *Broker
	b.Publish("t", Event{Name: "progress", Data: "x", Terminal: true})
	if got := b.Subscribers("t"); got != 0 {
		t.Errorf("Subscribers on a nil Broker = %d, want 0", got)
	}
}

// TestPublishConcurrentWithSubscribe shakes the broker's locking under the
// race detector: publishers, subscribers joining and leaving, all at once.
// It asserts no delivery counts, because the drop policy makes them
// nondeterministic by design. It asserts only that nothing deadlocks or
// panics.
func TestPublishConcurrentWithSubscribe(t *testing.T) {
	b := New(Config{})
	stop := make(chan struct{})

	// Publishers run until stop closes. They are NOT part of the
	// completion signal, or the wait could never finish.
	var publishers sync.WaitGroup
	for i := 0; i < 4; i++ {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b.Publish("t", Event{Name: "progress", Data: "x"})
			}
		}()
	}

	var joiners sync.WaitGroup
	for i := 0; i < 4; i++ {
		joiners.Add(1)
		go func() {
			defer joiners.Done()
			for i := 0; i < 100; i++ {
				sub := b.Subscribe(context.Background(), "t")
				b.Publish("t", Event{Name: "progress", Data: "y"})
				// The just-published event may itself be
				// dropped under load, which is the policy and
				// not a bug, so the receive is bounded.
				select {
				case <-sub.Events:
				case <-time.After(50 * time.Millisecond):
				}
				sub.Cancel()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		joiners.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("joiners did not finish under concurrent publish and subscribe")
	}
	close(stop)
	publishers.Wait()
}
