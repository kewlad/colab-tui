package runtime

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

// sshPath is the runtime-proxy endpoint that fronts the VM's sshd. Unlike the
// kernel and TTY sockets, it is NOT present on every runtime image — a 404 here
// means this runtime was created without SSH support, not that anything is
// misconfigured locally.
const sshPath = "/colab/ssh"

// pubkeyHeader carries the public key verbatim; the server matches it against
// the key the local ssh client then authenticates with.
const pubkeyHeader = "X-Colab-Ssh-Pubkey"

// DialSSH opens the SSH-over-WebSocket connection for a runtime. The proxy
// token rides in the query string (not a header) on this endpoint, and the
// public key is sent verbatim so the bytes the user controls are exactly what
// the server receives.
func DialSSH(baseURL, token, pubkey string) (*websocket.Conn, error) {
	wsURL := fmt.Sprintf("%s%s?colab-runtime-proxy-token=%s",
		toWS(baseURL), sshPath, url.QueryEscape(token))
	hdr := http.Header{}
	hdr.Set(pubkeyHeader, pubkey)

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		return nil, sshDialError(err, resp)
	}
	return conn, nil
}

// sshDialError turns a websocket upgrade failure into an actionable message.
// The HTTP status is the only signal the server gives, so each one is mapped
// explicitly rather than surfaced as a bare "bad handshake".
func sshDialError(err error, resp *http.Response) error {
	if resp == nil {
		return fmt.Errorf("ssh connect: %w (check your network, or `colab health`)", err)
	}
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return fmt.Errorf("ssh connect: server rejected the public key (HTTP 400). " +
			"Only ssh-ed25519 and ecdsa-sha2-nistp* keys are accepted — ssh-rsa is not. " +
			"Generate one with `ssh-keygen -t ed25519`, or pass --identity")
	case http.StatusUnauthorized:
		return fmt.Errorf("ssh connect: proxy token invalid or expired (HTTP 401). Try `colab resume`")
	case http.StatusForbidden:
		return fmt.Errorf("ssh connect: forbidden (HTTP 403) — the token lacks permission for this runtime")
	case http.StatusNotFound:
		return fmt.Errorf("ssh connect: this runtime does not expose %s (HTTP 404).\n"+
			"SSH is baked into the runtime image at creation, so an already-running runtime "+
			"cannot gain it — start a fresh one (`colab start`) and retry. If a fresh runtime "+
			"also 404s, the image in your region does not carry sshd yet; use `colab exec` instead", sshPath)
	case http.StatusTooManyRequests:
		return fmt.Errorf("ssh connect: another SSH session is already attached (HTTP 429) — disconnect it first")
	case http.StatusBadGateway:
		return fmt.Errorf("ssh connect: the runtime's sshd is unreachable (HTTP 502) — the runtime may be unhealthy (`colab health`)")
	default:
		return fmt.Errorf("ssh connect: WebSocket upgrade rejected (HTTP %d): %w", resp.StatusCode, err)
	}
}

// BridgeStdio pumps the SSH byte stream between the WebSocket and stdin/stdout
// so `colab ssh --proxy-mode` can be used as an OpenSSH ProxyCommand. It
// returns when either side closes; a close from either direction is a normal
// disconnect, not an error.
func BridgeStdio(conn *websocket.Conn) error {
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		conn.Close()
	}()

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			if isNormalWSClose(err) {
				return nil
			}
			return err
		}
		if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
			continue
		}
		if _, err := os.Stdout.Write(data); err != nil {
			if err == io.ErrClosedPipe {
				return nil
			}
			return err
		}
	}
}

// isNormalWSClose reports whether a read error is just the far side hanging up.
// OpenSSH tears the transport down abruptly at the end of a session, so 1005
// and 1006 are expected here rather than failures worth reporting.
func isNormalWSClose(err error) bool {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure) {
		return true
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}
