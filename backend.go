package main

import (
	"context"
	"fmt"
)

// Backend streams audio to an STT service and returns text fragments.
type Backend interface {
	// Transcribe reads PCM chunks from audioCh and sends text fragments to textCh.
	// It returns when audioCh is closed or ctx is cancelled.
	Transcribe(ctx context.Context, audioCh <-chan []byte, textCh chan<- string) error
}

func NewBackend(cfg *Config) (Backend, error) {
	switch cfg.Backend.Name {
	case "mistral-realtime":
		apiKey, err := getMistralAPIKey(cfg.Backend.MistralRT.APIKey)
		if err != nil {
			return nil, err
		}
		return NewWebSocketBackend(
			"wss://api.mistral.ai/v1/audio/transcriptions/realtime?model="+cfg.Backend.MistralRT.Model,
			cfg.Backend.MistralRT.Model,
			apiKey,
			cfg.Audio.SampleRate,
		), nil
	case "mistral-batch":
		mb := cfg.Backend.MistralBatch
		apiKey, err := getMistralAPIKey(mb.APIKey)
		if err != nil {
			return nil, err
		}
		model := mb.Model
		if model == "" {
			model = "voxtral-mini-latest"
		}
		chunkSeconds := mb.ChunkSeconds
		if chunkSeconds <= 0 {
			chunkSeconds = 5
		}
		silenceRMS := mb.SilenceRMS
		if silenceRMS <= 0 {
			silenceRMS = cfg.Audio.VAD.Threshold
		}
		return NewMistralBatchBackend(
			apiKey,
			model,
			cfg.Audio.SampleRate,
			cfg.Audio.ChunkMs,
			chunkSeconds,
			mb.SilenceSeconds,
			silenceRMS,
			cfg.Debug,
		), nil
	case "vllm-realtime":
		return NewWebSocketBackend(
			cfg.Backend.VllmRT.URL,
			cfg.Backend.VllmRT.Model,
			"", // no API key for local
			cfg.Audio.SampleRate,
		), nil
	case "llamacpp":
		return NewLlamaCppBackend(
			cfg.Backend.LlamaCpp.URL,
			cfg.Audio.SampleRate,
			cfg.Backend.LlamaCpp.ChunkSeconds,
		), nil
	case "xai-realtime":
		return NewXaiRealtimeBackend(
			cfg.Backend.XaiRT.URL,
			mustGetXaiAPIKey(cfg),
			cfg.Audio.SampleRate,
			cfg.Backend.XaiRT.EndpointingMs,
			cfg.Backend.XaiRT.Language,
			cfg.Debug,
		), nil
	case "xai-batch":
		return NewXaiBatchBackend(
			mustGetXaiAPIKey(cfg),
			cfg.Backend.XaiBatch.URL,
			cfg.Audio.SampleRate,
			cfg.Backend.XaiBatch.Language,
		), nil
	case "mock":
		return NewMockBackend(cfg.Audio.SampleRate), nil
	default:
		return nil, fmt.Errorf("unknown backend: %q", cfg.Backend.Name)
	}
}
