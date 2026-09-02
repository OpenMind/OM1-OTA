package ota

import (
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"
)

const isoTimestampLayout = "2006-01-02T15:04:05.000000-07:00"

// wsSender is the subset of the WebSocket client used to emit progress frames.
type wsSender interface {
	IsConnected() bool
	SendMessage(message []byte)
}

// progressFrame is the JSON payload sent for each progress update.
type progressFrame struct {
	Type        string `json:"type"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Progress    int    `json:"progress"`
	Timestamp   string `json:"timestamp"`
	ServiceName string `json:"service_name,omitempty"`
	Action      string `json:"action,omitempty"`
}

// ProgressReporter sends OTA progress updates over a WebSocket connection.
type ProgressReporter struct {
	mu        sync.RWMutex
	ws        wsSender
	operation progressFrame
}

// SetOperation records the operation whose progress is reported from now on.
func (p *ProgressReporter) SetOperation(serviceName, action string) {
	p.mu.Lock()
	p.operation = progressFrame{ServiceName: serviceName, Action: action}
	p.mu.Unlock()
}

// NewProgressReporter creates a reporter, optionally bound to a WebSocket client.
func NewProgressReporter(ws wsSender) *ProgressReporter {
	return &ProgressReporter{ws: ws}
}

// SetWSClient sets or replaces the WebSocket client used for sending updates.
func (p *ProgressReporter) SetWSClient(ws wsSender) {
	p.mu.Lock()
	p.ws = ws
	p.mu.Unlock()
}

// SendProgressUpdate emits an ota_progress frame.
func (p *ProgressReporter) SendProgressUpdate(status, message string, progress int) {
	p.mu.RLock()
	ws := p.ws
	frame := p.operation
	p.mu.RUnlock()

	if ws == nil {
		zap.S().Warnw("Cannot send progress update - no WebSocket client",
			"status", status, "message", message, "progress", progress)
		return
	}
	if !ws.IsConnected() {
		zap.S().Warnw("Cannot send progress update - WebSocket not connected",
			"status", status, "message", message, "progress", progress)
		return
	}

	frame.Type = "ota_progress"
	frame.Status = status
	frame.Message = message
	frame.Progress = progress
	frame.Timestamp = time.Now().UTC().Format(isoTimestampLayout)

	payload, err := json.Marshal(frame)
	if err != nil {
		zap.S().Warnw("Failed to encode progress update", "error", err)
		return
	}
	ws.SendMessage(payload)
	zap.S().Infow("Sent progress update", "status", status, "message", message, "progress", progress)
}
