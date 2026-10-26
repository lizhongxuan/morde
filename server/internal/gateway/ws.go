package gateway

import (
	"io"
	"sync"
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
