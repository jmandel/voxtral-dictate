package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"path/filepath"
)

// XaiBatchBackend accumulates the entire dictation session's audio and posts
// it to xAI's batch STT endpoint (https://api.x.ai/v1/stt) when the audio
// stream ends. No realtime streaming, no chunk-finals, no fragmentation —
// xAI sees one continuous WAV and produces one transcript.
type XaiBatchBackend struct {
	apiKey     string
	url        string
	sampleRate int
	language   string
}

func NewXaiBatchBackend(apiKey, url string, sampleRate int, language string) *XaiBatchBackend {
	if url == "" {
		url = "https://api.x.ai/v1/stt"
	}
	return &XaiBatchBackend{
		apiKey:     apiKey,
		url:        url,
		sampleRate: sampleRate,
		language:   language,
	}
}

func (b *XaiBatchBackend) Transcribe(ctx context.Context, audioCh <-chan []byte, textCh chan<- string) error {
	// Accumulate the whole burst's audio.
	var pcm []byte
	for {
		select {
		case <-ctx.Done():
			// Toggle-off / shutdown: still try to transcribe what we have.
			return b.send(context.Background(), pcm, textCh)
		case chunk, ok := <-audioCh:
			if !ok {
				// VAD trail closed the burst — send what we accumulated.
				return b.send(ctx, pcm, textCh)
			}
			pcm = append(pcm, chunk...)
		}
	}
}

func (b *XaiBatchBackend) send(ctx context.Context, pcm []byte, textCh chan<- string) error {
	if len(pcm) == 0 {
		return nil
	}
	wavData := pcmToWAV(pcm, b.sampleRate)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	// xAI is order-sensitive: the `file` field must come LAST. Anything after
	// it is silently dropped. So write metadata first.
	_ = w.WriteField("format", "true")
	if b.language != "" {
		_ = w.WriteField("language", b.language)
	}
	part, err := w.CreateFormFile("file", filepath.Base("audio.wav"))
	if err != nil {
		return fmt.Errorf("xai batch form: %w", err)
	}
	if _, err := part.Write(wavData); err != nil {
		return fmt.Errorf("xai batch write file: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("xai batch close form: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", b.url, &body)
	if err != nil {
		return fmt.Errorf("xai batch request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+b.apiKey)

	log.Printf("xAI batch: posting %d bytes WAV (%.1fs of audio)", len(wavData), float64(len(pcm))/float64(b.sampleRate*2))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("xai batch do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("xai batch %d: %s", resp.StatusCode, data)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("xai batch decode: %w", err)
	}
	if result.Text == "" {
		return nil
	}
	select {
	case textCh <- result.Text:
	case <-ctx.Done():
	}
	return nil
}
