package runtime

import (
	"time"

	"github.com/gorilla/websocket"
)

// KeepaliveConn is a long-lived kernel-channel WebSocket held open purely to keep
// a Colab runtime alive. The Colab VS Code extension's keep-alive is gated on
// "kernel connections > 0 and activity within the last hour" — so simply holding
// this connection open registers as an attached client, and the periodic Ping
// (a WebSocket control frame) plus the runtime keep-alive HTTP call mimic the
// activity the extension generates. A background reader drains and discards all
// inbound traffic (IOPub broadcasts, pongs) so the socket never stalls, and
// closes Dead() when the connection drops so the daemon can reconnect.
//
// Unlike Execute, the Ping uses a WS control frame, which is delivered out of
// band — it does NOT queue behind a busy training cell, so the heartbeat keeps
// working even while the kernel is running a long job.
type KeepaliveConn struct {
	k    *Kernel
	dead chan struct{}
}

// DialKeepalive opens a held kernel connection for the runtime, ensuring a kernel
// exists (creating a session if the runtime is fresh / was reaped). label names
// any session it creates so the held kernel is attributed to its runtime.
func DialKeepalive(baseURL, token, label string) (*KeepaliveConn, error) {
	k, err := NewKernel(baseURL, token, label)
	if err != nil {
		return nil, err
	}
	kc := &KeepaliveConn{k: k, dead: make(chan struct{})}
	kc.k.conn.SetPongHandler(func(string) error { return nil })
	go kc.readLoop()
	return kc, nil
}

// readLoop discards every inbound message until the connection errors, then
// signals Dead. gorilla requires an active reader for the connection to process
// control frames (pongs) and to surface a dropped peer.
func (kc *KeepaliveConn) readLoop() {
	for {
		if _, _, err := kc.k.conn.ReadMessage(); err != nil {
			close(kc.dead)
			return
		}
	}
}

// Ping sends a WebSocket ping control frame. Safe to call concurrently with the
// reader (gorilla allows WriteControl alongside other methods).
func (kc *KeepaliveConn) Ping() error {
	return kc.k.conn.WriteControl(websocket.PingMessage, []byte("ka"), time.Now().Add(10*time.Second))
}

// Dead is closed when the held connection drops.
func (kc *KeepaliveConn) Dead() <-chan struct{} { return kc.dead }

// Close tears down the held connection.
func (kc *KeepaliveConn) Close() error { return kc.k.conn.Close() }
