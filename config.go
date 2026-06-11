package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Daemon    DaemonConfig      `toml:"daemon"`
	Audio     AudioConfig       `toml:"audio"`
	Typing    TypingConfig      `toml:"typing"`
	Backend   BackendConfig     `toml:"backend"`
	Indicator []IndicatorConfig `toml:"indicator"`
	Debug     bool              `toml:"debug"`
}

type IndicatorConfig struct {
	Type string `toml:"type"` // "led", "dunstify", "command"
	// LED options
	LEDNumber int    `toml:"led_number"` // /proc/acpi/ibm/led number (0=power)
	Mode      string `toml:"mode"`       // "on" or "blink"
	// Dunstify options
	Message string `toml:"message"`
	Urgency string `toml:"urgency"` // low | normal | critical
	// Command options
	StartCmd string `toml:"start_cmd"`
	StopCmd  string `toml:"stop_cmd"`
}

type DaemonConfig struct {
	Socket string `toml:"socket"`
}

type AudioConfig struct {
	SampleRate int       `toml:"sample_rate"`
	ChunkMs    int       `toml:"chunk_ms"`
	Device     string    `toml:"device"`
	Method     string    `toml:"method"`
	VAD        VADConfig `toml:"vad"`
}

type VADConfig struct {
	Enabled     bool    `toml:"enabled"`
	Threshold   float64 `toml:"threshold"`
	PreBufferN  int     `toml:"pre_buffer_chunks"`
	TrailChunks int     `toml:"trail_chunks"`
}

type TypingConfig struct {
	Method string `toml:"method"`
}

type BackendConfig struct {
	Name         string             `toml:"name"`
	Mistral      MistralConfig      `toml:"mistral"`
	MistralRT    MistralRTConfig    `toml:"mistral-realtime"`
	MistralBatch MistralBatchConfig `toml:"mistral-batch"`
	VllmRT       VllmRTConfig       `toml:"vllm-realtime"`
	LlamaCpp     LlamaCppConfig     `toml:"llamacpp"`
	XaiRT        XaiRTConfig        `toml:"xai-realtime"`
	XaiBatch     XaiBatchConfig     `toml:"xai-batch"`
}

type MistralConfig struct {
	APIKey string `toml:"api_key"`
}

type MistralRTConfig struct {
	APIKey string `toml:"api_key"`
	Model  string `toml:"model"`
}

type MistralBatchConfig struct {
	APIKey         string  `toml:"api_key"`
	Model          string  `toml:"model"`
	ChunkSeconds   int     `toml:"chunk_seconds"`
	SilenceSeconds float64 `toml:"silence_seconds"`
	SilenceRMS     float64 `toml:"silence_rms"`
}

type VllmRTConfig struct {
	URL   string `toml:"url"`
	Model string `toml:"model"`
}

type LlamaCppConfig struct {
	URL          string `toml:"url"`
	ChunkSeconds int    `toml:"chunk_seconds"`
}

type XaiRTConfig struct {
	APIKey        string `toml:"api_key"`
	URL           string `toml:"url"`
	EndpointingMs int    `toml:"endpointing_ms"`
	Language      string `toml:"language"`
}

type XaiBatchConfig struct {
	APIKey   string `toml:"api_key"`
	URL      string `toml:"url"`
	Language string `toml:"language"`
}

func defaultConfig() *Config {
	return &Config{
		Daemon: DaemonConfig{Socket: "/tmp/dictate.sock"},
		Audio: AudioConfig{
			SampleRate: 16000,
			ChunkMs:    480,
			VAD: VADConfig{
				Enabled:     true,
				Threshold:   200,
				PreBufferN:  3,
				TrailChunks: 21, // ~10s trailing silence before disconnecting
			},
		},
		Typing: TypingConfig{Method: "xdotool"},
		Backend: BackendConfig{
			Name: "mistral-realtime",
			MistralRT: MistralRTConfig{
				Model: "voxtral-mini-transcribe-realtime-2602",
			},
			MistralBatch: MistralBatchConfig{
				Model:          "voxtral-mini-latest",
				ChunkSeconds:   5,
				SilenceSeconds: 2.0,
			},
			VllmRT: VllmRTConfig{
				URL:   "ws://localhost:8000/v1/realtime",
				Model: "mistralai/Voxtral-Mini-4B-Realtime-2602",
			},
			LlamaCpp: LlamaCppConfig{
				URL:          "http://localhost:8080/v1/chat/completions",
				ChunkSeconds: 3,
			},
			XaiRT: XaiRTConfig{
				URL:           "wss://api.x.ai/v1/stt",
				EndpointingMs: 1000,
			},
			XaiBatch: XaiBatchConfig{
				URL: "https://api.x.ai/v1/stt",
			},
		},
	}
}

func mustLoadConfig() *Config {
	cfg := defaultConfig()

	// Find config file
	path := os.Getenv("DICTATE_CONFIG")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "dictate", "config.toml")
	}

	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Bad config %s: %v\n", path, err)
			os.Exit(1)
		}
	}

	// A MISTRAL_API_KEY env var or [backend.mistral] api_key fills per-backend blanks.
	envKey := os.Getenv("MISTRAL_API_KEY")
	if envKey != "" {
		cfg.Backend.Mistral.APIKey = envKey
	}
	if cfg.Backend.MistralRT.APIKey == "" {
		cfg.Backend.MistralRT.APIKey = cfg.Backend.Mistral.APIKey
	}
	if cfg.Backend.MistralBatch.APIKey == "" {
		cfg.Backend.MistralBatch.APIKey = cfg.Backend.Mistral.APIKey
	}

	return cfg
}
