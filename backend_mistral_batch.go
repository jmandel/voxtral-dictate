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
)

// MistralBatchBackend sends accumulated audio to Mistral's
// /v1/audio/transcriptions endpoint. Not true streaming but
// works well for testing and for chunk-based dictation.
type MistralBatchBackend struct {
	apiKey       string
	model        string
	sampleRate   int
	chunkSeconds int
}

func NewMistralBatchBackend(apiKey, model string, sampleRate, chunkSeconds int) *MistralBatchBackend {
	if chunkSeconds <= 0 {
		chunkSeconds = 5
	}
	if model == "" {
		model = "voxtral-mini-latest"
	}
	return &MistralBatchBackend{
		apiKey:       apiKey,
		model:        model,
		sampleRate:   sampleRate,
		chunkSeconds: chunkSeconds,
	}
}

func (b *MistralBatchBackend) Transcribe(ctx context.Context, audioCh <-chan []byte, textCh chan<- string) error {
	bytesPerPeriod := b.sampleRate * 2 * b.chunkSeconds
	var accum []byte

	for {
		select {
		case <-ctx.Done():
			if len(accum) > 0 {
				b.sendChunk(ctx, accum, textCh)
			}
			return nil
		case chunk, ok := <-audioCh:
			if !ok {
				if len(accum) > 0 {
					b.sendChunk(ctx, accum, textCh)
				}
				return nil
			}
			accum = append(accum, chunk...)
			if len(accum) >= bytesPerPeriod {
				b.sendChunk(ctx, accum, textCh)
				accum = nil
			}
		}
	}
}

func (b *MistralBatchBackend) sendChunk(ctx context.Context, pcm []byte, textCh chan<- string) {
	wavData := pcmToWAV(pcm, b.sampleRate)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("model", b.model)

	part, err := w.CreateFormFile("file", "audio.wav")
	if err != nil {
		log.Printf("mistral batch: create form: %v", err)
		return
	}
	part.Write(wavData)
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.mistral.ai/v1/audio/transcriptions", &body)
	if err != nil {
		log.Printf("mistral batch: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+b.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("mistral batch: request: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		log.Printf("mistral batch %d: %s", resp.StatusCode, data)
		return
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("mistral batch: decode: %v", err)
		return
	}

	if result.Text != "" {
		select {
		case textCh <- result.Text:
		case <-ctx.Done():
		}
	}
}

func getMistralAPIKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("set MISTRAL_API_KEY or api_key in config")
	}
	return key, nil
}
