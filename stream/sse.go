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
func (b *Broker) ServeTopic(w http.ResponseWriter, r *http.Request, topic string) {
	sub := b.Subscribe(r.Context(), topic)
	defer sub.Cancel()

	wire.SetEventHeaders(w)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}

	heartbeat := time.NewTicker(b.cfg.Heartbeat)
	defer heartbeat.Stop()

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
