package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// WebSocketBackend works with both Mistral Realtime API and local vLLM Realtime.
type WebSocketBackend struct {
	url        string
	model      string
	apiKey     string
	sampleRate int
}

func NewWebSocketBackend(url, model, apiKey string, sampleRate int) *WebSocketBackend {
	return &WebSocketBackend{
		url:        url,
		model:      model,
		apiKey:     apiKey,
		sampleRate: sampleRate,
	}
}

type wsEvent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (b *WebSocketBackend) Transcribe(ctx context.Context, audioCh <-chan []byte, textCh chan<- string) error {
	opts := &websocket.DialOptions{}
	if b.apiKey != "" {
		opts.HTTPHeader = http.Header{
			"Authorization": {"Bearer " + b.apiKey},
		}
	}

	conn, _, err := websocket.Dial(ctx, b.url, opts)
	if err != nil {
		return fmt.Errorf("ws dial %s: %w", b.url, err)
	}
	defer func() {
		// Send a proper WS close frame so the server retires the gRPC
		// stream promptly. CloseNow does a forceful TCP close which can
		// leave the server-side stream in a "may still be alive" state and
		// inflate the apparent concurrent-stream count for our key.
		_ = conn.Close(websocket.StatusNormalClosure, "")
		log.Printf("WebSocket disconnected from %s", b.url)
	}()

	// Increase read limit for large responses
	conn.SetReadLimit(10 * 1024 * 1024)

	// Wait for session.created from server
	var initEv wsEvent
	if err := wsjson.Read(ctx, conn, &initEv); err != nil {
		return fmt.Errorf("ws read session.created: %w", err)
	}
	log.Printf("WebSocket connected to %s (model=%s, init=%s)", b.url, b.model, initEv.Type)

	// Tell server our audio format
	sessionUpdate, _ := json.Marshal(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"audio_format": map[string]any{
				"encoding":    "pcm_s16le",
				"sample_rate": b.sampleRate,
			},
		},
	})
	if err := conn.Write(ctx, websocket.MessageText, sessionUpdate); err != nil {
		return fmt.Errorf("ws session update: %w", err)
	}

	// Send audio in background
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		defer func() {
			// Signal end of audio
			msg, _ := json.Marshal(map[string]string{"type": "input_audio.end"})
			conn.Write(ctx2, websocket.MessageText, msg)
			cancel()
		}()
		for {
			select {
			case <-ctx2.Done():
				return
			case chunk, ok := <-audioCh:
				if !ok {
					return
				}
				b64 := base64.StdEncoding.EncodeToString(chunk)
				msg, _ := json.Marshal(map[string]string{
					"type":  "input_audio.append",
					"audio": b64,
				})
				if err := conn.Write(ctx2, websocket.MessageText, msg); err != nil {
					log.Printf("ws write error: %v", err)
					return
				}
			}
		}
	}()

	// Read text events
	for {
		_, data, err := conn.Read(ctx2)
		if err != nil {
			if ctx2.Err() != nil {
				return nil // normal shutdown
			}
			return fmt.Errorf("ws read: %w", err)
		}
		var ev wsEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Printf("ws unmarshal: %v", err)
			continue
		}
		switch ev.Type {
		case "transcription.text.delta":
			if ev.Text != "" {
				select {
				case textCh <- ev.Text:
				case <-ctx2.Done():
					return nil
				}
			}
		case "transcription.done":
			return nil
		case "error":
			log.Printf("ws error event: %s", data)
		}
	}
}
