package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

type streamThenErrorBody struct {
	ctx   context.Context
	data  []byte
	err   error
	block bool
}

func (b *streamThenErrorBody) Read(dst []byte) (int, error) {
	if len(b.data) != 0 {
		n := copy(dst, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	if b.block {
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	if b.err != nil {
		err := b.err
		b.err = nil
		return 0, err
	}
	return 0, io.EOF
}

func (*streamThenErrorBody) Close() error { return nil }

func TestCompletePreservesVisibleDeltaAndClassifiesInterruptedProviderStreams(t *testing.T) {
	for _, test := range []struct {
		name      string
		endpoint  app.ModelEndpoint
		newClient func(*testing.T, http.RoundTripper) *ResponsesClient
		chunk     string
	}{
		{
			name:      "responses API",
			endpoint:  testEndpoint(),
			newClient: newTestClient,
			chunk:     "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
		},
		{
			name:      "z.ai API",
			endpoint:  zaiTestEndpoint(),
			newClient: zaiTestClient,
			chunk:     "data: {\"model\":\"glm-5.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := test.newClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK,
					Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:   &streamThenErrorBody{data: []byte(test.chunk), err: io.ErrUnexpectedEOF}, Request: request}, nil
			}))
			var visible strings.Builder
			response, err := client.Complete(context.Background(), app.ModelRequest{Endpoint: test.endpoint,
				Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
				OnTextDelta: func(delta string) error { _, _ = visible.WriteString(delta); return nil },
			})
			if got := modelErrorKind(t, err); got != app.ModelErrorTransport || visible.String() != "partial" || response.Text != "" {
				t.Fatalf("interrupted stream response=%#v visible=%q error=%v", response, visible.String(), err)
			}
		})
	}
}

func TestCompleteClassifiesStreamingDeadlineAfterVisibleDelta(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := &streamThenErrorBody{ctx: request.Context(), data: []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"), block: true}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: body, Request: request}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var visible strings.Builder
	response, err := client.Complete(ctx, app.ModelRequest{Endpoint: testEndpoint(),
		Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
		OnTextDelta: func(delta string) error { _, _ = visible.WriteString(delta); return nil },
	})
	var timeoutErr *app.ModelClientError
	if !errors.As(err, &timeoutErr) || timeoutErr.Kind != app.ModelErrorTimeout || visible.String() != "partial" || response.Text != "" {
		t.Fatalf("deadline response=%#v visible=%q error=%v", response, visible.String(), err)
	}
}
