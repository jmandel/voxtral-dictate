package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
)

// Recorder captures microphone audio as PCM s16le via arecord or pw-record.
// No CGo needed — we pipe from a subprocess.
type Recorder struct {
	sampleRate int
	chunkBytes int // bytes per chunk to read
	device     string
	method     string
	cmd        *exec.Cmd
	stdout     io.ReadCloser
}

func NewRecorder(cfg AudioConfig) *Recorder {
	// Each sample is 2 bytes (s16le), mono
	chunkSamples := cfg.SampleRate * cfg.ChunkMs / 1000
	return &Recorder{
		sampleRate: cfg.SampleRate,
		chunkBytes: chunkSamples * 2,
		device:     cfg.Device,
		method:     cfg.Method,
	}
}

// Start begins recording. Returns a channel of PCM chunks.
// Closes the channel when ctx is cancelled or recording stops.
func (r *Recorder) Start(ctx context.Context) (<-chan []byte, error) {
	args := r.buildArgs()
	r.cmd = exec.CommandContext(ctx, args[0], args[1:]...)

	var err error
	r.stdout, err = r.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := r.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start recorder: %w", err)
	}

	ch := make(chan []byte, 16)
	go func() {
		defer close(ch)
		defer r.cmd.Wait()
		buf := make([]byte, r.chunkBytes)
		for {
			n, err := io.ReadFull(r.stdout, buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case ch <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF && ctx.Err() == nil {
					log.Printf("recorder read: %v", err)
				}
				return
			}
		}
	}()

	log.Printf("Recording started (%s, %d Hz, chunk=%d bytes)",
		args[0], r.sampleRate, r.chunkBytes)
	return ch, nil
}

func (r *Recorder) buildArgs() []string {
	switch r.method {
	case "", "auto":
		if _, err := exec.LookPath("pw-record"); err == nil {
			return r.pwRecordArgs()
		}
		return r.arecordArgs()
	case "pw-record":
		return r.pwRecordArgs()
	case "arecord":
		return r.arecordArgs()
	default:
		log.Printf("unknown audio method %q, using auto", r.method)
		if _, err := exec.LookPath("pw-record"); err == nil {
			return r.pwRecordArgs()
		}
		return r.arecordArgs()
	}
}

func (r *Recorder) pwRecordArgs() []string {
	args := []string{
		"pw-record",
		"--format=s16",
		fmt.Sprintf("--rate=%d", r.sampleRate),
		"--channels=1",
		"-", // stdout
	}
	if r.device != "" {
		args = append([]string{args[0], "--target=" + r.device}, args[1:]...)
	}
	return args
}

func (r *Recorder) arecordArgs() []string {
	args := []string{
		"arecord",
		"-f", "S16_LE",
		"-r", fmt.Sprintf("%d", r.sampleRate),
		"-c", "1",
		"-t", "raw",
		"-q", // quiet
		"-",  // stdout
	}
	if r.device != "" {
		args = append([]string{args[0], "-D", r.device}, args[1:]...)
	}
	return args
}
