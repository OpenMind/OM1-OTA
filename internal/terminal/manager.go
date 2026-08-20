package terminal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// maxSessionDuration bounds how long a single terminal session may stay open.
const maxSessionDuration = 10 * time.Minute

// Manager triggers and runs terminal sessions on demand.
type Manager struct {
	serverURL string
	apiKey    string
	shell     string
}

// NewManager creates a terminal Manager. shell defaults to /bin/bash.
func NewManager(serverURL, apiKey, shell string) *Manager {
	if shell == "" {
		shell = "/bin/bash"
	}
	return &Manager{serverURL: serverURL, apiKey: apiKey, shell: shell}
}

type resizeMessage struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// HandleStart handles a terminal_start message by starting a new terminal session.
func (m *Manager) HandleStart(data map[string]any) {
	sessionID, _ := data["session_id"].(string)
	if sessionID == "" {
		zap.S().Errorw("Invalid terminal_start message: missing session_id")
		return
	}

	go m.runSession(sessionID)
}

// runSession runs a terminal session for the given session ID. It connects to the relay server
func (m *Manager) runSession(sessionID string) {
	url := fmt.Sprintf("%s?session_id=%s&api_key=%s", m.serverURL, sessionID, m.apiKey)

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		zap.S().Errorw("Failed to dial terminal relay", "session_id", sessionID, "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	cmd := exec.Command(m.shell)
	cmd.Env = append(os.Environ(), "BASH_SILENCE_DEPRECATION_WARNING=1")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		zap.S().Errorw("Failed to start PTY", "session_id", sessionID, "error", err)
		return
	}
	defer func() { _ = ptmx.Close() }()

	zap.S().Infow("Terminal session started", "session_id", sessionID, "shell", m.shell)

	if data, err := json.Marshal(map[string]string{"type": "status", "status": "active"}); err == nil {
		if werr := conn.WriteMessage(websocket.TextMessage, data); werr != nil {
			zap.S().Warnw("Failed to send active status", "session_id", sessionID, "error", werr)
		}
	}

	var closeOnce sync.Once
	done := make(chan struct{})
	closeDone := func() { closeOnce.Do(func() { close(done) }) }

	// PTY -> WS: shell output becomes binary frames.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					zap.S().Warnw("Failed to write terminal output to WebSocket", "session_id", sessionID, "error", werr)
					closeDone()
					return
				}
			}
			if err != nil {
				closeDone()
				return
			}
		}
	}()

	// WS -> PTY: binary frames are keystrokes, text frames are resize control messages.
	go func() {
		for {
			messageType, msg, err := conn.ReadMessage()
			if err != nil {
				closeDone()
				return
			}

			switch messageType {
			case websocket.BinaryMessage:
				if _, werr := ptmx.Write(msg); werr != nil {
					zap.S().Warnw("Failed to write to PTY", "session_id", sessionID, "error", werr)
					closeDone()
					return
				}
			case websocket.TextMessage:
				var ctrl resizeMessage
				if err := json.Unmarshal(msg, &ctrl); err == nil && ctrl.Type == "resize" {
					if err := pty.Setsize(ptmx, &pty.Winsize{Cols: ctrl.Cols, Rows: ctrl.Rows}); err != nil {
						zap.S().Warnw("Failed to resize PTY", "session_id", sessionID, "error", err)
					}
				}
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(maxSessionDuration):
		zap.S().Infow("Terminal session reached max duration", "session_id", sessionID)
		closeDone()
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	zap.S().Infow("Terminal session ended", "session_id", sessionID)
}
