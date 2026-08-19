package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// wsBetaHeaderValue is the OpenAI-Beta value that opts into the Codex
	// WebSocket responses transport (pi: OPENAI_BETA_RESPONSES_WEBSOCKETS).
	wsBetaHeaderValue = "responses_websockets=2026-02-06"
	wsSessionMaxAge   = 55 * time.Minute
	wsIdleTTL         = 5 * time.Minute
	wsConnectTimeout  = 15 * time.Second
	wsMaxMessageBytes = 64 << 20
)

// wsURL is the WebSocket upgrade of the SSE upstream URL.
func wsURL() string {
	u, err := url.Parse(upstreamResponsesURL)
	if err != nil {
		return upstreamResponsesURL
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	return u.String()
}

// ---------------------------------------------------------------------------
// Minimal RFC 6455 client (text frames only, client-side masking)
// ---------------------------------------------------------------------------

type wsConn struct {
	conn   net.Conn
	br     *bufio.Reader
	wmu    sync.Mutex
	closed bool
}

func wsDial(ctx context.Context, dialURL string, headers http.Header) (*wsConn, error) {
	u, err := url.Parse(dialURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if host == "" {
		return nil, errors.New("websocket URL missing host")
	}
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(wsConnectTimeout)
	}
	var nc net.Conn
	if u.Scheme == "wss" {
		nc, err = tls.DialWithDialer(&net.Dialer{Deadline: deadline}, "tcp", host, &tls.Config{ServerName: u.Hostname()})
	} else {
		nc, err = net.DialTimeout("tcp", host, time.Until(deadline))
	}
	if err != nil {
		return nil, err
	}
	key := wsHandshakeKey()
	var req strings.Builder
	req.WriteString("GET " + u.RequestURI() + " HTTP/1.1\r\n")
	req.WriteString("Host: " + u.Host + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	for k, vs := range headers {
		for _, v := range vs {
			req.WriteString(k + ": " + v + "\r\n")
		}
	}
	req.WriteString("\r\n")
	if _, err := nc.Write([]byte(req.String())); err != nil {
		nc.Close()
		return nil, err
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		nc.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		nc.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %s", resp.Status)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAcceptKey(key) {
		nc.Close()
		return nil, errors.New("websocket accept key mismatch")
	}
	return &wsConn{conn: nc, br: br}, nil
}

func wsHandshakeKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *wsConn) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errors.New("websocket closed")
	}
	return c.writeFrame(0x1, data)
}

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	var hdr [14]byte
	hdr[0] = 0x80 | opcode // FIN + opcode
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	pos := 2
	n := len(payload)
	switch {
	case n < 126:
		hdr[1] = 0x80 | byte(n) // client-to-server frames must be masked
	case n < 65536:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		pos = 4
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		pos = 10
	}
	if _, err := c.conn.Write(hdr[:pos]); err != nil {
		return err
	}
	if _, err := c.conn.Write(mask); err != nil {
		return err
	}
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := c.conn.Write(masked)
	return err
}

// ReadMessage reads one complete (defragmented) text/binary message.
// Ping/close/pong control frames are handled internally.
func (c *wsConn) ReadMessage() ([]byte, error) {
	var acc []byte
	for {
		payload, opcode, fin, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x8: // close
			return nil, io.EOF
		case 0x9: // ping -> pong
			_ = c.writeFrame(0xA, payload)
			continue
		case 0xA: // pong
			continue
		case 0x1, 0x2: // text / binary
			if fin {
				return payload, nil
			}
			acc = append(acc, payload...)
		case 0x0: // continuation
			acc = append(acc, payload...)
			if fin {
				return acc, nil
			}
		default:
			return nil, fmt.Errorf("unexpected websocket opcode 0x%x", opcode)
		}
	}
}

func (c *wsConn) readFrame() (payload []byte, opcode byte, fin bool, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return nil, 0, false, err
	}
	fin = hdr[0]&0x80 != 0
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return nil, 0, false, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return nil, 0, false, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length > wsMaxMessageBytes {
		return nil, 0, false, errors.New("websocket message exceeds 64 MiB")
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return nil, 0, false, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return nil, 0, false, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, opcode, fin, nil
}

func (c *wsConn) Close() error {
	c.wmu.Lock()
	c.closed = true
	_ = c.writeFrame(0x8, nil)
	c.wmu.Unlock()
	return c.conn.Close()
}

// ---------------------------------------------------------------------------
// Per-session connection pool with continuation state
// ---------------------------------------------------------------------------

type wsSession struct {
	mu           sync.Mutex
	socket       *wsConn
	busy         bool
	createdAt    time.Time
	continuation *continuationState
	idleTimer    *time.Timer
}

type sessionPool struct {
	mu       sync.Mutex
	sessions map[string]*wsSession
}

func newSessionPool() *sessionPool {
	return &sessionPool{sessions: map[string]*wsSession{}}
}

type acquiredSession struct {
	session *wsSession
	reused  bool
	release func(keep bool)
}

// acquire returns a WebSocket session for the given session ID, reusing a
// cached idle connection when available. When sessionID is empty, or the
// cached session is busy with another in-flight request, a one-off connection
// is opened (not cached). The release callback must be called when the
// response stream has been fully consumed.
func (p *sessionPool) acquire(ctx context.Context, sessionID string, buildHeaders func() (http.Header, error)) (*acquiredSession, error) {
	if sessionID != "" {
		p.mu.Lock()
		s := p.sessions[sessionID]
		p.mu.Unlock()
		if s != nil {
			s.mu.Lock()
			if s.idleTimer != nil {
				s.idleTimer.Stop()
				s.idleTimer = nil
			}
			if !s.busy && time.Since(s.createdAt) >= wsSessionMaxAge {
				_ = s.socket.Close()
				s.mu.Unlock()
				p.mu.Lock()
				if cur := p.sessions[sessionID]; cur == s {
					delete(p.sessions, sessionID)
				}
				p.mu.Unlock()
				// Fall through to create a fresh cached session.
			} else if !s.busy {
				s.busy = true
				s.mu.Unlock()
				return &acquiredSession{session: s, reused: true, release: p.releaser(sessionID, s)}, nil
			} else {
				// Busy: open a one-off so the cached connection is not shared
				// between concurrent requests on the same session.
				s.mu.Unlock()
				return p.newOneOff(ctx, buildHeaders)
			}
		}
		return p.newCached(ctx, sessionID, buildHeaders)
	}
	return p.newOneOff(ctx, buildHeaders)
}

func (p *sessionPool) newOneOff(ctx context.Context, buildHeaders func() (http.Header, error)) (*acquiredSession, error) {
	headers, err := buildHeaders()
	if err != nil {
		return nil, err
	}
	socket, err := wsDial(ctx, wsURL(), headers)
	if err != nil {
		return nil, err
	}
	s := &wsSession{socket: socket, busy: true, createdAt: time.Now()}
	return &acquiredSession{session: s, reused: false, release: func(bool) { _ = socket.Close() }}, nil
}

func (p *sessionPool) newCached(ctx context.Context, sessionID string, buildHeaders func() (http.Header, error)) (*acquiredSession, error) {
	headers, err := buildHeaders()
	if err != nil {
		return nil, err
	}
	socket, err := wsDial(ctx, wsURL(), headers)
	if err != nil {
		return nil, err
	}
	s := &wsSession{socket: socket, busy: true, createdAt: time.Now()}
	p.mu.Lock()
	p.sessions[sessionID] = s
	p.mu.Unlock()
	return &acquiredSession{session: s, reused: false, release: p.releaser(sessionID, s)}, nil
}

func (p *sessionPool) releaser(sessionID string, s *wsSession) func(bool) {
	return func(keep bool) {
		s.mu.Lock()
		if !keep {
			_ = s.socket.Close()
			s.continuation = nil
			s.mu.Unlock()
			p.mu.Lock()
			if cur := p.sessions[sessionID]; cur == s {
				delete(p.sessions, sessionID)
			}
			p.mu.Unlock()
			return
		}
		s.busy = false
		s.idleTimer = time.AfterFunc(wsIdleTTL, func() {
			s.mu.Lock()
			if s.busy {
				s.mu.Unlock()
				return
			}
			_ = s.socket.Close()
			s.continuation = nil
			s.mu.Unlock()
			p.mu.Lock()
			if cur := p.sessions[sessionID]; cur == s {
				delete(p.sessions, sessionID)
			}
			p.mu.Unlock()
		})
		s.mu.Unlock()
	}
}

// readWebSocket parses JSON event frames from the server into sseEvent values
// until a terminal response event is received, at which point it returns so the
// underlying socket can be returned to the pool for reuse by the next turn.
func readWebSocket(socket *wsConn, fn func(sseEvent) error) error {
	for {
		msg, err := socket.ReadMessage()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(msg) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(msg, &obj); err != nil {
			return fmt.Errorf("invalid websocket event JSON: %w", err)
		}
		name, _ := obj["type"].(string)
		if name == "" {
			continue
		}
		if err := fn(sseEvent{name, obj}); err != nil {
			return err
		}
		switch name {
		case "response.completed", "response.done", "response.incomplete", "response.failed", "error":
			return nil
		}
	}
}
