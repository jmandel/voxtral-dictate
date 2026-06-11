package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// drainAfterCloseTimeout is how long we keep the WS open after sending
// audio.done, waiting for xAI to flush any buffered final transcript.
// Without this, audio captured between the last chunk-final and the user's
// hotkey-off is silently discarded by xAI when we cut the connection.
const drainAfterCloseTimeout = 1500 * time.Millisecond

// XaiRealtimeBackend streams to xAI's WebSocket STT (wss://api.x.ai/v1/stt).
//
// Protocol differs from Mistral: audio is sent as raw binary PCM frames,
// session params come from URL query, and the end-of-audio signal is
// a JSON {"type":"audio.done"} message.
type XaiRealtimeBackend struct {
	url           string
	apiKey        string
	sampleRate    int
	endpointingMs int
	language      string
	debug         bool
}

func NewXaiRealtimeBackend(rawURL, apiKey string, sampleRate, endpointingMs int, language string, debug bool) *XaiRealtimeBackend {
	if rawURL == "" {
		rawURL = "wss://api.x.ai/v1/stt"
	}
	if endpointingMs <= 0 {
		endpointingMs = 1000
	}
	return &XaiRealtimeBackend{
		url:           rawURL,
		apiKey:        apiKey,
		sampleRate:    sampleRate,
		endpointingMs: endpointingMs,
		language:      language,
		debug:         debug,
	}
}

func (b *XaiRealtimeBackend) Transcribe(ctx context.Context, audioCh <-chan []byte, textCh chan<- string) error {
	u, err := url.Parse(b.url)
	if err != nil {
		return fmt.Errorf("xai url parse: %w", err)
	}
	q := u.Query()
	q.Set("sample_rate", strconv.Itoa(b.sampleRate))
	q.Set("encoding", "pcm")
	// We never type non-final partials (they revise heavily within a
	// sub-phrase), so don't ask for them. Chunk-finals still stream.
	q.Set("interim_results", "false")
	q.Set("endpointing", strconv.Itoa(b.endpointingMs))
	if b.language != "" {
		q.Set("language", b.language)
	}
	u.RawQuery = q.Encode()
	dialURL := u.String()

	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + b.apiKey}},
	}
	conn, _, err := websocket.Dial(ctx, dialURL, opts)
	if err != nil {
		return fmt.Errorf("xai ws dial %s: %w", dialURL, err)
	}
	defer func() {
		conn.CloseNow()
		log.Printf("xAI WebSocket disconnected")
	}()

	conn.SetReadLimit(10 * 1024 * 1024)
	log.Printf("xAI WebSocket connecting to %s", b.url)

	// drainCtx survives parent-ctx cancellation by drainAfterCloseTimeout so
	// the receiver can pick up xAI's flushed final transcript after we send
	// audio.done. The sender uses parent ctx so toggle-off ends streaming
	// promptly.
	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()

	// Sender: stream raw PCM binary frames, then send {"type":"audio.done"}.
	// On either parent-ctx cancel (toggle off, shutdown) or audioCh close
	// (VAD burst end), drain any audio already captured but not yet sent,
	// then flush audio.done. Without this, audio in upstream buffers at the
	// moment of toggle-off is dropped on the floor and xAI transcribes only
	// what made it across, so trailing speech vanishes.
	go func() {
		defer time.AfterFunc(drainAfterCloseTimeout, drainCancel)

		// Each write uses a fresh background-derived context with a deadline
		// so a parent-ctx cancel can't abort writes mid-frame (which would
		// leave xAI with corrupt or partial audio).
		writeBinary := func(chunk []byte) error {
			wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer wcancel()
			return conn.Write(wctx, websocket.MessageBinary, chunk)
		}
		sendDone := func() {
			wctx, wcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer wcancel()
			done, _ := json.Marshal(map[string]string{"type": "audio.done"})
			_ = conn.Write(wctx, websocket.MessageText, done)
		}
		// drainBuffered consumes anything already queued in audioCh (without
		// waiting for new chunks) and forwards it to xAI. Returns once the
		// channel is empty or closed.
		drainBuffered := func() {
			for {
				select {
				case chunk, ok := <-audioCh:
					if !ok {
						return
					}
					if err := writeBinary(chunk); err != nil {
						return
					}
				default:
					return
				}
			}
		}

		for {
			select {
			case <-ctx.Done():
				drainBuffered()
				sendDone()
				return
			case chunk, ok := <-audioCh:
				if !ok {
					sendDone()
					return
				}
				if err := writeBinary(chunk); err != nil {
					log.Printf("xai ws write: %v", err)
					return
				}
			}
		}
	}()

	// Receiver strategy (validated against the live xAI protocol):
	//
	//   chunk-final  (is_final=true,  speech_final=false)  →  stream live
	//   speech-final (is_final=true,  speech_final=true)   →  emit suffix beyond chunk-finals
	//   transcript.done                                    →  emit suffix for last-turn tail
	//
	// Why: chunk-finals fire on the model's internal phrase commits and
	// don't wait for user-pause detection, so they give live streaming.
	// But sometimes a turn ends (or the session ends) with audio that
	// only made it into partials, never a chunk-final — speech-final and
	// transcript.done capture the full turn, so we emit anything they
	// know that we haven't typed yet.
	//
	// xAI returns text with no trailing space, so consecutive emissions
	// would run together ("Hello.Hey"). Prepend a space from the second
	// emission onward — that way the session never ends with a stray
	// trailing space.
	needLeadingSpace := false
	var turnEmitted strings.Builder
	emit := func(text string) bool {
		if text == "" {
			return true
		}
		out := text
		if needLeadingSpace {
			out = " " + out
		}
		needLeadingSpace = true
		select {
		case textCh <- out:
			return true
		case <-drainCtx.Done():
			return false
		}
	}
	emitCorrection := func(cumulative string) bool {
		if cumulative == "" {
			return true
		}
		prior := strings.TrimSpace(turnEmitted.String())
		cum := strings.TrimSpace(cumulative)
		if prior == cum {
			return true
		}
		// Emit only when speech-final / transcript.done strictly EXTENDS
		// the chunk-finals we already typed. If xAI rewrote the wording
		// (capitalization, punctuation, number rendering — e.g. "ten" →
		// "10"), the prefix check fails and we accept the chunk-final
		// version rather than risk a full-paragraph duplication.
		if prior != "" && strings.HasPrefix(cum, prior) {
			return emit(strings.TrimSpace(cum[len(prior):]))
		}
		if prior == "" {
			// Nothing was streamed this turn (short utterance, no
			// chunk-final fired). Emit the cumulative summary in full.
			return emit(cum)
		}
		// xAI rewrote — keep what we already typed, drop the revision.
		return true
	}
	for {
		_, data, err := conn.Read(drainCtx)
		if err != nil {
			if drainCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("xai ws read: %w", err)
		}
		var ev struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			IsFinal     bool   `json:"is_final"`
			SpeechFinal bool   `json:"speech_final"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Printf("xai ws unmarshal: %v", err)
			continue
		}
		// Debug mode: log every event to journal AND type it inline with a
		// bracketed marker. Journal is the authoritative source (typing
		// fidelity drops under fast bursts), but inline typing keeps the
		// debug experience usable from the focused window.
		if b.debug {
			marker, payload := ev.Type, ev.Text
			if ev.Type == "transcript.partial" {
				switch {
				case ev.IsFinal && ev.SpeechFinal:
					marker = "speech-final"
				case ev.IsFinal:
					marker = "chunk-final"
				default:
					marker = "partial"
				}
			} else if ev.Type == "error" {
				payload = string(data)
			}
			log.Printf("xai event [%s]: %q", marker, payload)
			out := fmt.Sprintf("[%s: %s]", marker, payload)
			select {
			case textCh <- out:
			case <-drainCtx.Done():
				return nil
			}
			if ev.Type == "transcript.done" {
				return nil
			}
			continue
		}

		switch ev.Type {
		case "transcript.created":
			// session ack
		case "transcript.partial":
			if !ev.IsFinal || ev.Text == "" {
				continue
			}
			if !ev.SpeechFinal {
				// Chunk-final: stream this phrase live.
				if !emit(ev.Text) {
					return nil
				}
				if turnEmitted.Len() > 0 {
					turnEmitted.WriteString(" ")
				}
				turnEmitted.WriteString(ev.Text)
				continue
			}
			// Speech-final: emit any suffix beyond the chunk-finals we've
			// already typed, then reset the turn buffer.
			if !emitCorrection(ev.Text) {
				return nil
			}
			turnEmitted.Reset()
		case "transcript.done":
			if !emitCorrection(ev.Text) {
				return nil
			}
			return nil
		case "error":
			log.Printf("xai ws error event: %s", data)
		}
	}
}
