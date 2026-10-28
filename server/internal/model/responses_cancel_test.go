package model

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
)

type cancelAwareResponseBody struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (b *cancelAwareResponseBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*cancelAwareResponseBody) Close() error { return nil }

func TestCompleteStopsReadingProviderStreamOnCancellation(t *testing.T) {
	started := make(chan struct{})
	client := newTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &cancelAwareResponseBody{ctx: request.Context(), started: started},
			Request:    request,
		}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.Complete(ctx, app.ModelRequest{
			Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}},
			MaxOutputTokens: 10, OnTextDelta: func(string) error { return nil },
		})
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider response stream was not read")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("provider response read did not stop after cancellation")
	}
}
