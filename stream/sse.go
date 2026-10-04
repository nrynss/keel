package stream

import (
	"net/http"
	"time"

	"github.com/nrynss/keel/wire"
)

// ServeTopic writes topic to w as a server-sent-events stream and returns
// when the response ends. The response ends on a write error, on the
// request context ending, or on the subscription ending.
//
// Headers are set once, before the first byte, and every frame is flushed
// as it is written. During a quiet period a ping comment keeps an
// intermediary from reaping the connection. The heartbeat timer resets on
// every real event, so pings appear only between events.
//
// When the request carries Last-Event-ID and that id is still in the
// topic's ring, the frames published after it are written first, each
// with the id it was published under, and only then does the handler
// follow the live subscription. Ids increase for the life of the broker,
// so a cursor from a topic this process has already dropped cannot match
// a frame in a later ring. A missing header, a cursor this process
// does not remember, or a cursor of zero skips that replay and subscribes
// exactly as before. Ping comments are not part of the ring. A replayed
// terminal frame ends the response, and so does a cursor equal to the
// retained terminal: there is nothing further to send, and the response
// closes instead of waiting.
func (b *Broker) ServeTopic(w http.ResponseWriter, r *http.Request, topic string) {
	after, resume := lastEventID(r)
	replay, sub := b.catchUp(r.Context(), topic, after, resume)
	defer sub.Cancel()

	wire.SetEventHeaders(w)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}

	heartbeat := time.NewTicker(b.cfg.Heartbeat)
	defer heartbeat.Stop()

	for _, ev := range replay {
		if r.Context().Err() != nil {
			return
		}
		if err := wire.WriteEvent(w, ev.Name, ev.ID, ev.Data); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		heartbeat.Reset(b.cfg.Heartbeat)
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.Events:
			if !ok {
				return
			}
			if err := wire.WriteEvent(w, ev.Name, ev.ID, ev.Data); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			heartbeat.Reset(b.cfg.Heartbeat)
		case <-heartbeat.C:
			if err := wire.WriteHeartbeat(w); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
