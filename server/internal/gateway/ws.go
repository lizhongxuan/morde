package gateway

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/host"
)

type trackedBody struct {
	io.ReadWriteCloser
	owner  *gateway
	hostID string
	once   sync.Once
}

func (g *gateway) trackWebSocket(body io.ReadWriteCloser, hostID string) *trackedBody {
	tracked := &trackedBody{ReadWriteCloser: body, owner: g, hostID: hostID}
	g.wsMu.Lock()
	g.wsBodies[tracked] = struct{}{}
	g.wsGauge.Inc()
	g.wsMu.Unlock()
	return tracked
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Read(p)
	if n > 0 {
		b.owner.markActive(b.hostID)
	}
	return n, err
}

func (b *trackedBody) Write(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Write(p)
	if n > 0 {
		b.owner.markActive(b.hostID)
	}
	return n, err
}

func (b *trackedBody) Close() error {
	err := b.ReadWriteCloser.Close()
	b.once.Do(func() {
		b.owner.wsMu.Lock()
		delete(b.owner.wsBodies, b)
		b.owner.wsGauge.Dec()
		b.owner.wsMu.Unlock()
	})
	return err
}

type hostControlInputBody struct {
	io.ReadWriteCloser
	ctx        context.Context
	permits    app.HostInputPermitStore
	ref        host.Ref
	actorID    string
	permission host.Permission
}

const controlInputWriteTimeout = 5 * time.Second

type hostControlRoundTripper struct {
	base       http.RoundTripper
	permits    app.HostInputPermitStore
	ref        host.Ref
	actorID    string
	permission host.Permission
	timeout    time.Duration
}

const controlRoundTripTimeout = 30 * time.Second

func (t *hostControlRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	timeout := t.timeout
	if timeout <= 0 {
		timeout = controlRoundTripTimeout
	}
	ctx, cancel := context.WithCancel(request.Context())
	var timerMu sync.Mutex
	finished, timedOut := false, false
	timer := time.AfterFunc(timeout, func() {
		timerMu.Lock()
		defer timerMu.Unlock()
		if !finished {
			timedOut = true
			cancel()
		}
	})

	var response *http.Response
	request = request.WithContext(ctx)
	err := t.permits.WithHostControlPermit(ctx, t.ref, t.actorID, t.permission, func() error {
		var err error
		// The response body remains owned by the HTTP caller.
		//nolint:bodyclose
		response, err = t.base.RoundTrip(request)
		return err
	})
	timerMu.Lock()
	finished = true
	timer.Stop()
	expired := timedOut
	timerMu.Unlock()
	if expired {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cancel()
		return nil, context.DeadlineExceeded
	}
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cancel()
		return nil, err
	}
	if response == nil || response.Body == nil || response.Body == http.NoBody {
		cancel()
		return response, nil
	}
	if body, ok := response.Body.(io.ReadWriteCloser); ok {
		response.Body = &controlLeaseReadWriteBody{ReadWriteCloser: body, cancel: cancel}
	} else {
		response.Body = &controlLeaseReadBody{ReadCloser: response.Body, cancel: cancel}
	}
	return response, nil
}

type controlLeaseReadBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *controlLeaseReadBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}

type controlLeaseReadWriteBody struct {
	io.ReadWriteCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *controlLeaseReadWriteBody) Close() error {
	err := b.ReadWriteCloser.Close()
	b.once.Do(b.cancel)
	return err
}

func (b *hostControlInputBody) Write(p []byte) (int, error) {
	written := 0
	ctx, cancel := context.WithTimeout(b.ctx, controlInputWriteTimeout)
	defer cancel()
	timer := time.AfterFunc(controlInputWriteTimeout, func() { _ = b.Close() })
	defer timer.Stop()
	if err := b.permits.WithHostControlPermit(ctx, b.ref, b.actorID, b.permission, func() error {
		for written < len(p) {
			n, err := b.ReadWriteCloser.Write(p[written:])
			written += n
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
		}
		return nil
	}); err != nil {
		return written, err
	}
	return written, nil
}
