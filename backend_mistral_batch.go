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
	"os"
	"time"
)

// MistralBatchBackend sends accumulated audio to Mistral's
// /v1/audio/transcriptions endpoint. Not true streaming but
// works well for testing and for chunk-based dictation.
type MistralBatchBackend struct {
	apiKey         string
	model          string
	sampleRate     int
	chunkMs        int     // size of one PCM chunk from the recorder (ms)
	chunkSeconds   int     // max accumulate-before-flush window (s)
	silenceSeconds float64 // if >0, flush early after this much trailing silence
	silenceRMS     float64 // RMS threshold below which a chunk counts as silent
	debug          bool
}

func NewMistralBatchBackend(apiKey, model string, sampleRate, chunkMs, chunkSeconds int, silenceSeconds, silenceRMS float64, debug bool) *MistralBatchBackend {
	if chunkSeconds <= 0 {
		chunkSeconds = 5
	}
	if model == "" {
		model = "voxtral-mini-latest"
	}
	return &MistralBatchBackend{
		apiKey:         apiKey,
		model:          model,
		sampleRate:     sampleRate,
		chunkMs:        chunkMs,
		chunkSeconds:   chunkSeconds,
		silenceSeconds: silenceSeconds,
		silenceRMS:     silenceRMS,
		debug:          debug,
	}
}

func (b *MistralBatchBackend) Transcribe(ctx context.Context, audioCh <-chan []byte, textCh chan<- string) error {
	bytesPerPeriod := b.sampleRate * 2 * b.chunkSeconds

	// Number of consecutive silent chunks that triggers an early flush.
	// 0 disables the silence-flush behavior.
	silentChunksLimit := 0
	if b.silenceSeconds > 0 && b.chunkMs > 0 {
		silentChunksLimit = int(b.silenceSeconds*1000/float64(b.chunkMs) + 0.5)
		if silentChunksLimit < 1 {
			silentChunksLimit = 1
		}
	}

	var accum []byte
	silentChunks := 0

	// flushFinal sends whatever's accumulated as a last chunk. Uses a fresh
	// context with its own deadline so a parent-ctx cancel (toggle-off) can't
	// abort the HTTP POST mid-upload — that would silently drop short
	// utterances under chunkSeconds long.
	flushFinal := func(pcm []byte) {
		if len(pcm) == 0 {
			return
		}
		fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer fcancel()
		b.sendChunk(fctx, pcm, textCh)
	}

	for {
		select {
		case <-ctx.Done():
			flushFinal(accum)
			return nil
		case chunk, ok := <-audioCh:
			if !ok {
				flushFinal(accum)
				return nil
			}
			accum = append(accum, chunk...)

			// Track trailing silence and flush early if it goes long enough,
			// so the user sees results without waiting the full chunkSeconds
			// window or having to toggle off.
			if silentChunksLimit > 0 {
				rms := rmsEnergy(chunk)
				silent := rms < b.silenceRMS
				if silent {
					silentChunks++
				} else {
					silentChunks = 0
				}
				if b.debug {
					log.Printf("mistral-batch chunk rms=%.0f silent=%v silentChunks=%d/%d accumBytes=%d",
						rms, silent, silentChunks, silentChunksLimit, len(accum))
				}
				if silentChunks >= silentChunksLimit && len(accum) > 0 {
					if b.debug {
						log.Printf("mistral-batch: silence-flush triggered (%d chunks of <%.0f RMS)", silentChunks, b.silenceRMS)
					}
					b.sendChunk(ctx, accum, textCh)
					accum = nil
					silentChunks = 0
					continue
				}
			}

			if len(accum) >= bytesPerPeriod {
				b.sendChunk(ctx, accum, textCh)
				accum = nil
				silentChunks = 0
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

func mustGetXaiAPIKey(cfg *Config) string {
	key := cfg.Backend.XaiRT.APIKey
	if key == "" {
		key = cfg.Backend.XaiBatch.APIKey
	}
	if key == "" {
		key = os.Getenv("XAI_API_KEY")
	}
	if key == "" {
		key = os.Getenv("GROK_API_KEY")
	}
	if key == "" {
		fmt.Fprintf(os.Stderr, "Set XAI_API_KEY (or GROK_API_KEY) or api_key in [backend.xai-realtime|xai-batch]\n")
		os.Exit(1)
	}
	return key
}
