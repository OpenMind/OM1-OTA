package terminal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHandleStart_MissingSessionID(t *testing.T) {
	m := NewManager("ws://unused", "unused-key", "")
	m.HandleStart(map[string]any{})
	m.HandleStart(map[string]any{"session_id": ""})
}

func dialRelayStub(t *testing.T) (serverURL string, relayConnCh chan *websocket.Conn) {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	relayConnCh = make(chan *websocket.Conn, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("relay stub failed to upgrade: %v", err)
			return
		}
		relayConnCh <- conn
	}))
	t.Cleanup(server.Close)

	return "ws" + strings.TrimPrefix(server.URL, "http"), relayConnCh
}

func TestRunSession_PumpsBytesBetweenRelayAndPTY(t *testing.T) {
	serverURL, relayConnCh := dialRelayStub(t)

	m := NewManager(serverURL, "test-api-key", "cat")

	done := make(chan struct{})
	go func() {
		m.runSession("session-under-test")
		close(done)
	}()

	var relayConn *websocket.Conn
	select {
	case relayConn = <-relayConnCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runSession to dial the relay")
	}
	defer func() { _ = relayConn.Close() }()

	input := []byte("hello from the relay\n")
	if err := relayConn.WriteMessage(websocket.BinaryMessage, input); err != nil {
		t.Fatalf("failed to write input to relay stub: %v", err)
	}

	_ = relayConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var received []byte
	for !strings.Contains(string(received), "hello from the relay") {
		messageType, msg, err := relayConn.ReadMessage()
		if err != nil {
			t.Fatalf("failed reading echoed PTY output: %v", err)
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		received = append(received, msg...)
	}

	_ = relayConn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSession did not exit after the relay connection closed")
	}
}

func TestRunSession_ResizeControlMessageDoesNotCrash(t *testing.T) {
	serverURL, relayConnCh := dialRelayStub(t)
	m := NewManager(serverURL, "test-api-key", "cat")

	done := make(chan struct{})
	go func() {
		m.runSession("session-resize")
		close(done)
	}()

	var relayConn *websocket.Conn
	select {
	case relayConn = <-relayConnCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runSession to dial the relay")
	}
	defer func() { _ = relayConn.Close() }()

	resize := []byte(`{"type":"resize","cols":100,"rows":40}`)
	if err := relayConn.WriteMessage(websocket.TextMessage, resize); err != nil {
		t.Fatalf("failed to write resize control message: %v", err)
	}

	input := []byte("still alive\n")
	if err := relayConn.WriteMessage(websocket.BinaryMessage, input); err != nil {
		t.Fatalf("failed to write input after resize: %v", err)
	}

	_ = relayConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var received []byte
	for !strings.Contains(string(received), "still alive") {
		messageType, msg, err := relayConn.ReadMessage()
		if err != nil {
			t.Fatalf("failed reading PTY output after resize: %v", err)
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		received = append(received, msg...)
	}

	_ = relayConn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSession did not exit after the relay connection closed")
	}
}
