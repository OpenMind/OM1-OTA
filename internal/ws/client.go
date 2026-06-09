// Package ws provides a resilient WebSocket client with automatic
// reconnection, a background receive loop that dispatches to a callback, and a
// buffered send queue. It is a faithful port of the Python WebSocketClient.
package ws

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// reconnectDelay mirrors the Python client's 5-second retry interval.
const reconnectDelay = 5 * time.Second

// sendQueueSize bounds the outbound message buffer. The Python client used an
// unbounded queue; we use a generous buffer and warn if it ever fills.
const sendQueueSize = 256

// MessageCallback handles a single inbound message.
type MessageCallback func(message []byte)

// Client is a reconnecting WebSocket client. The zero value is not usable;
// construct one with NewClient.
type Client struct {
	url      string
	callback MessageCallback

	sendCh chan []byte

	mu        sync.RWMutex
	conn      *websocket.Conn
	connected bool

	running atomic.Bool
	stopCh  chan struct{}
}

// NewClient creates a client for the given WebSocket URL. The URL may carry
// query parameters (e.g. api_key_id / api_key) exactly as the Python client
// expects.
func NewClient(url string) *Client {
	if url == "" {
		panic("ws: WebSocket URL must be provided")
	}
	c := &Client{
		url:    url,
		sendCh: make(chan []byte, sendQueueSize),
		stopCh: make(chan struct{}),
	}
	c.running.Store(true)
	return c
}

// RegisterMessageCallback sets the handler invoked for every inbound message.
func (c *Client) RegisterMessageCallback(cb MessageCallback) {
	c.callback = cb
}

// Start launches the background connection-management goroutine.
func (c *Client) Start() {
	c.running.Store(true)
	go c.runLoop()
	slog.Info("WebSocket client thread started")
}

// runLoop maintains the connection, reconnecting with a fixed delay on failure.
func (c *Client) runLoop() {
	for c.running.Load() {
		conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
		if err != nil {
			slog.Error("Connection failed, retrying", "delay", reconnectDelay, "error", err)
			if c.sleepOrStop(reconnectDelay) {
				return
			}
			continue
		}

		c.setConn(conn)
		slog.Info("Connection established", "url", c.url)

		connDone := make(chan struct{})
		go c.readLoop(conn, connDone)
		c.writeLoop(conn, connDone)

		// writeLoop returned: the connection is dead or we are stopping.
		c.clearConn(conn)
		<-connDone // ensure the reader has exited before reconnecting

		if !c.running.Load() {
			return
		}
	}
}

// readLoop reads inbound messages and dispatches them to the callback. It
// closes connDone when the connection fails or is closed.
func (c *Client) readLoop(conn *websocket.Conn, connDone chan struct{}) {
	defer close(connDone)
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Info("WebSocket connection closed normally")
			} else {
				slog.Warn("WebSocket connection closed", "error", err)
			}
			c.setConnected(false)
			return
		}
		if c.callback != nil {
			c.callback(message)
		}
	}
}

// writeLoop drains the send queue. It returns when the connection dies (the
// reader closed connDone), on a write error (re-queuing the message), or on stop.
func (c *Client) writeLoop(conn *websocket.Conn, connDone chan struct{}) {
	for {
		select {
		case <-connDone:
			return
		case <-c.stopCh:
			return
		case msg := <-c.sendCh:
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				slog.Warn("Failed to send message, re-queuing", "error", err)
				c.setConnected(false)
				c.requeue(msg)
				return
			}
		}
	}
}

// SendMessage queues a message for delivery. It is dropped with a warning if
// the client is not connected or the send queue is full.
func (c *Client) SendMessage(message []byte) {
	if !c.IsConnected() {
		slog.Warn("Cannot queue message: WebSocket client is not connected or not running")
		return
	}
	select {
	case c.sendCh <- message:
	default:
		slog.Warn("Cannot queue message: send queue is full")
	}
}

// IsConnected reports whether the client currently has a live connection.
func (c *Client) IsConnected() bool {
	if !c.running.Load() {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected && c.conn != nil
}

// Stop tears down the client: it stops reconnecting, closes the connection, and
// signals the background goroutines to exit.
func (c *Client) Stop() {
	if !c.running.Swap(false) {
		return // already stopped
	}
	close(c.stopCh)

	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.connected = false
	c.mu.Unlock()

	if conn != nil {
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Client shutdown"))
		if err := conn.Close(); err != nil {
			slog.Warn("Error during WebSocket close", "error", err)
		}
	}

	// Drain the send queue.
	for {
		select {
		case <-c.sendCh:
		default:
			slog.Info("WebSocket client stopped")
			return
		}
	}
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.mu.Unlock()
}

// clearConn closes and clears conn if it is still the active connection.
func (c *Client) clearConn(conn *websocket.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
		c.connected = false
	}
	c.mu.Unlock()
	_ = conn.Close()
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}

// requeue attempts a non-blocking re-enqueue of a message that failed to send.
func (c *Client) requeue(msg []byte) {
	select {
	case c.sendCh <- msg:
	default:
	}
}

// sleepOrStop waits for d or until Stop is called. It returns true if stopped.
func (c *Client) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-c.stopCh:
		return true
	}
}
