package stream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/nrynss/keel/stream"
	"github.com/nrynss/keel/wire"
)

// ExampleBroker publishes a terminal event and shows a subscriber that joins
// afterwards still receiving it before the subscription ends.
func ExampleBroker() {
	broker := stream.New(stream.Config{})
	broker.Publish("job:abc", stream.Event{
		Name:     wire.EventDone,
		Data:     wire.StatusEvent{JobID: "abc", Status: wire.EventDone},
		Terminal: true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscription := broker.Subscribe(ctx, "job:abc")
	defer subscription.Cancel()

	for event := range subscription.Events {
		payload, err := json.Marshal(event.Data)
		if err != nil {
			fmt.Println("encode failed:", err)
			return
		}
		fmt.Println(event.Name, event.ID)
		fmt.Println(string(payload))
	}
	// Output:
	// done 1
	// {"job_id":"abc","status":"done"}
}

// ExampleBroker_ServeTopic writes a topic as a server-sent-events response and
// shows the framed bytes. The topic already holds a terminal event, so the
// replay ends the response without waiting on a clock.
func ExampleBroker_ServeTopic() {
	broker := stream.New(stream.Config{Heartbeat: time.Hour, Retain: time.Hour})
	broker.Publish("job:abc", stream.Event{
		Name:     wire.EventDone,
		Data:     wire.StatusEvent{JobID: "abc", Status: wire.EventDone},
		Terminal: true,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/jobs/abc/events", nil)
	broker.ServeTopic(recorder, request, "job:abc")

	fmt.Println(recorder.Code)
	fmt.Println(recorder.Header().Get("Content-Type"))
	fmt.Println(recorder.Body.String())
	// Output:
	// 200
	// text/event-stream
	// event: done
	// id: 1
	// data: {"job_id":"abc","status":"done"}
}
