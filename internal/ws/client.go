package ws

import (
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/gorilla/websocket"
)

const reconnectDelay = 5 * time.Second

// sendQueueSize bounds the outbound message buffer.
const sendQueueSize = 256

// MessageCallback handles a single inbound message.
type MessageCallback func(message []byte)

// Client is a reconnecting WebSocket client.
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

// NewClient creates a client for the given WebSocket URL.
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
	zap.S().Infow("WebSocket client thread started")
}

// runLoop maintains the connection, reconnecting with a fixed delay on failure.
func (c *Client) runLoop() {
	for c.running.Load() {
		conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
		if err != nil {
			zap.S().Errorw("Connection failed, retrying", "delay", reconnectDelay, "error", err)
			if c.sleepOrStop(reconnectDelay) {
				return
			}
			continue
		}

		c.setConn(conn)
		zap.S().Infow("Connection established", "url", c.url)

		connDone := make(chan struct{})
		go c.readLoop(conn, connDone)
		c.writeLoop(conn, connDone)

		// writeLoop returned: the connection is dead or we are stopping.
		c.clearConn(conn)
		<-connDone // wait for the reader to exit before reconnecting

		if !c.running.Load() {
			return
		}
	}
}

// readLoop reads inbound messages and dispatches them to the callback.
func (c *Client) readLoop(conn *websocket.Conn, connDone chan struct{}) {
	defer close(connDone)
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				zap.S().Infow("WebSocket connection closed normally")
			} else {
				zap.S().Warnw("WebSocket connection closed", "error", err)
			}
			c.setConnected(false)
			return
		}
		if c.callback != nil {
			c.callback(message)
		}
	}
}

// writeLoop drains the send queue.
func (c *Client) writeLoop(conn *websocket.Conn, connDone chan struct{}) {
	for {
		select {
		case <-connDone:
			return
		case <-c.stopCh:
			return
		case msg := <-c.sendCh:
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				zap.S().Warnw("Failed to send message, re-queuing", "error", err)
				c.setConnected(false)
				c.requeue(msg)
				return
			}
		}
	}
}

// SendMessage queues a message for delivery.
func (c *Client) SendMessage(message []byte) {
	if !c.IsConnected() {
		zap.S().Warnw("Cannot queue message: WebSocket client is not connected or not running")
		return
	}
	select {
	case c.sendCh <- message:
	default:
		zap.S().Warnw("Cannot queue message: send queue is full")
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

// Stop tears down the client and signals its background goroutines to exit.
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
			zap.S().Warnw("Error during WebSocket close", "error", err)
		}
	}

	// Drain the send queue.
	for {
		select {
		case <-c.sendCh:
		default:
			zap.S().Infow("WebSocket client stopped")
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
