# voxtral-dictate

System-wide speech-to-text dictation for Linux. A single Go binary runs as a
long-running daemon (near-zero idle CPU) and a toggle client. Press a hotkey,
speak, press again to stop. Text appears wherever your cursor is.

Built for i3/Sway/X11/Wayland. Supports Mistral's Voxtral models — both local
(llama.cpp, vLLM) and cloud (Mistral API).

## Quick Start

```bash
# Build
go build -o dictate .

# Configure
mkdir -p ~/.config/dictate
cp config.example.toml ~/.config/dictate/config.toml
# Edit config.toml — see detailed comments inside

# Start a model server (pick one, see "Model Servers" below)

# Run the daemon as a systemd user service (recommended):
mkdir -p ~/.config/systemd/user
cp systemd/dictate.service ~/.config/systemd/user/
# Edit ExecStart path in the service file to match your install location
systemctl --user daemon-reload
systemctl --user enable --now dictate

# Test toggle:
./dictate toggle   # → "started" (recording + transcribing)
./dictate toggle   # → "stopped"

# Add to i3 config (~/.config/i3/config):
bindsym $mod+d exec /path/to/dictate toggle
```

## Architecture

```
i3 keybinding
    │
    ▼
┌──────────┐   Unix socket    ┌─────────────────────────────────┐
│ dictate   │──── toggle ────▶│ dictate daemon (always running)  │
│ toggle    │◀── started ─────│                                  │
└──────────┘                  │  ┌──────────┐                    │
                              │  │ Recorder │ pw-record/arecord  │
                              │  │ (PCM)    │ subprocess pipe    │
                              │  └────┬─────┘                    │
                              │       │ []byte chunks            │
                              │  ┌────▼──────┐                   │
                              │  │ VAD       │ energy-based       │
                              │  │ (bursts)  │ connect on speech  │
                              │  └────┬──────┘                   │
                              │       │ speech bursts            │
                              │  ┌────▼──────┐                   │
                              │  │ Backend   │ WebSocket / HTTP  │
                              │  │ (STT)     │ to model server   │
                              │  └────┬──────┘                   │
                              │       │ text fragments           │
                              │  ┌────▼──────┐                   │
                              │  │ Typist    │ xdotool/wtype/etc │
                              │  │ (inject)  │ into focused app  │
                              │  └───────────┘                   │
                              └─────────────────────────────────┘
```

**Key design decisions:**
- Single binary for both daemon and client (subcommands: `daemon`, `toggle`, `test`)
- Unix socket IPC — instant toggle, no HTTP port, no conflicts
- No CGo — audio captured via `pw-record` or `arecord` subprocess (pipe stdout)
- Model server is separate — always-resident with weights hot in VRAM/RAM
- Daemon is idle at ~5MB RSS until toggled, then spawns goroutines for the session

## Commands

| Command | Purpose |
|---|---|
| `dictate daemon` | Start the long-running daemon (listens on Unix socket) |
| `dictate toggle` | Toggle dictation on/off (sends command to daemon) |
| `dictate test FILE.pcm` | Feed a raw PCM s16le file through the pipeline to stdout |

## Configuration

Config lives at `~/.config/dictate/config.toml` (override with `DICTATE_CONFIG` env var).

See `config.example.toml` for the full annotated config. Key sections:

### `[audio]`
```toml
sample_rate = 16000   # Must be 16000 for Voxtral
chunk_ms = 480        # Audio chunk size (480ms recommended for Realtime)
method = "auto"       # auto | pw-record | arecord
device = ""           # ALSA/PipeWire device name; empty = system default mic
```

### `[audio.vad]`
```toml
enabled = true        # Energy-based voice activity detection
threshold = 200       # RMS energy threshold (silence ~50-100, speech ~500-5000)
pre_buffer_chunks = 3 # Chunks kept before speech onset (~1.4s at 480ms)
trail_chunks = 21     # Chunks after speech stops before disconnecting (~10s)
```

When enabled, audio is split into speech bursts. Each burst gets its own backend
connection — silence means no connection and no API billing.

### `[[indicator]]`

Visual/hardware feedback when dictation is active. Multiple indicators can be
enabled simultaneously.

```toml
# ThinkPad LED blink (needs write access to /proc/acpi/ibm/led)
[[indicator]]
type = "led"
led_number = 0        # 0=power LED
mode = "blink"        # "on" or "blink"

# Desktop notification via dunstify
[[indicator]]
type = "dunstify"
message = "Dictating..."
urgency = "critical"  # low | normal | critical

# Arbitrary shell commands
[[indicator]]
type = "command"
start_cmd = "echo 1 > /sys/class/leds/platform::micmute/brightness"
stop_cmd = "echo 0 > /sys/class/leds/platform::micmute/brightness"
```

**ThinkPad LED permissions:** The `led` type writes to `/proc/acpi/ibm/led`,
which is root-only by default. Set up a udev rule for persistent access:

```bash
sudo tee /etc/udev/rules.d/99-thinkpad-led.rules <<'EOF'
ACTION=="add", SUBSYSTEM=="leds", DEVPATH=="*tpacpi*", RUN+="/bin/chmod 0666 /proc/acpi/ibm/led"
EOF
sudo udevadm control --reload-rules
# Apply immediately (takes effect on next boot automatically):
sudo chmod 0666 /proc/acpi/ibm/led
```

### `[typing]`
```toml
method = "xdotool"    # How to inject text into the focused window
```

| Method | Works on | Notes |
|---|---|---|
| `xdotool` | X11 (i3, etc.) | Most reliable for X11. Default. |
| `ydotool` | X11 + Wayland | Universal. Needs `ydotoold` daemon + `input` group. |
| `wtype` | Wayland (wlroots) | Sway, Hyprland only. Best Wayland option for wlroots. |
| `dotool` | X11 + Wayland | Universal alternative to ydotool. |

### `[backend]`
```toml
name = "llamacpp"     # Which STT backend to use

[backend.mistral]
api_key = ""          # Shared by mistral-realtime and mistral-batch; or set MISTRAL_API_KEY
```

| Backend | Latency | Needs | Cost |
|---|---|---|---|
| `mistral-realtime` | <500ms streaming | Internet + API key | $0.006/min |
| `mistral-batch` | 2-5s per chunk | Internet + API key | $0.003/min |
| `vllm-realtime` | <500ms streaming | Local GPU ≥16GB | Free |
| `llamacpp` | 2-5s per chunk | Local GPU or CPU | Free |
| `mock` | Instant (fake) | Nothing | For testing |

## Model Servers

The dictate daemon does **not** run the model. It connects to a separately-running
model server. This is intentional — model loading is slow (10-30s), so you want
the server always-resident with weights hot.

### Option A: llama.cpp + Voxtral Mini 3B (easiest local)

Smallest footprint. Runs on CPU (slow) or any GPU with ≥3GB VRAM.

```bash
# Download model + audio encoder
huggingface-cli download bartowski/mistralai_Voxtral-Mini-3B-2507-GGUF \
    mistralai_Voxtral-Mini-3B-2507-Q4_K_M.gguf --local-dir ~/models
huggingface-cli download ggml-org/Voxtral-Mini-3B-2507-GGUF \
    mmproj-Voxtral-Mini-3B-2507-Q8_0.gguf --local-dir ~/models

# Run (GPU accelerated with -ngl 99; omit for CPU-only)
llama-server \
    -m ~/models/mistralai_Voxtral-Mini-3B-2507-Q4_K_M.gguf \
    --mmproj ~/models/mmproj-Voxtral-Mini-3B-2507-Q8_0.gguf \
    --host 127.0.0.1 --port 8080 -ngl 99
```

Available quants (from bartowski):
| Quant | Size | Quality | Min VRAM |
|---|---|---|---|
| Q8_0 | 4.3GB | Near-lossless | ~6GB |
| Q6_K | 3.3GB | Excellent | ~5GB |
| **Q4_K_M** | **2.5GB** | **Good (recommended)** | **~4GB** |
| IQ3_M | 2.0GB | Acceptable | ~3GB |
| IQ2_M | 1.6GB | Usable | ~3GB |

Plus ~700MB for the mmproj audio encoder (always Q8).

### Option B: llama.cpp + Voxtral Small 24B (best local quality)

Needs a 24GB+ VRAM GPU (RTX 4090, A5000, etc.) for Q4_K_M.

```bash
huggingface-cli download bartowski/mistralai_Voxtral-Small-24B-2507-GGUF \
    mistralai_Voxtral-Small-24B-2507-Q4_K_M.gguf --local-dir ~/models
# Get matching mmproj from ggml-org or bartowski

llama-server \
    -m ~/models/mistralai_Voxtral-Small-24B-2507-Q4_K_M.gguf \
    --mmproj ~/models/mmproj-Voxtral-Small-24B-2507-Q8_0.gguf \
    --host 127.0.0.1 --port 8080 -ngl 99
```

### Option C: vLLM + Voxtral Realtime 4B (true streaming)

Best local experience. True streaming transcription with <500ms latency.
Requires ≥16GB VRAM GPU and vLLM nightly.

```bash
uv pip install -U vllm --torch-backend=auto \
    --extra-index-url https://wheels.vllm.ai/nightly
uv pip install soxr librosa soundfile

VLLM_DISABLE_COMPILE_CACHE=1 vllm serve mistralai/Voxtral-Mini-4B-Realtime-2602 \
    --host 127.0.0.1 --port 8000 \
    --compilation_config '{"cudagraph_mode": "PIECEWISE"}'
```

Set `backend.name = "vllm-realtime"` in config.

### Option D: vLLM + Voxtral Small 24B

Best overall quality via vLLM. Needs ~55GB VRAM (bf16) or ~28GB (FP8).

```bash
vllm serve mistralai/Voxtral-Small-24B-2507 \
    --tokenizer_mode mistral --config_format mistral --load_format mistral \
    --tensor-parallel-size 2 --host 127.0.0.1 --port 8000
```

### Option E: Mistral Cloud API (no local GPU needed)

Just set your API key:
```bash
export MISTRAL_API_KEY="your-key-here"
# or set api_key in config.toml under [backend.mistral]
```

Set `backend.name = "mistral-realtime"` for streaming or `"mistral-batch"` for chunked.

## i3 Integration

Add to `~/.config/i3/config`:

```bash
# Toggle dictation with $mod+` (backtick)
bindsym $mod+grave exec --no-startup-id /path/to/dictate toggle
```

The daemon itself should be managed by systemd (see below), not started
from the i3 config. This gives you automatic restarts, proper logging,
and clean shutdown.

### Status bar indicator (bumblebee-status)

A bumblebee-status module is included in `contrib/`. It shows **REC** in red
when dictation is active, and is blank otherwise.

```bash
cp contrib/bumblebee-dictate.py ~/.config/bumblebee-status/modules/dictate.py
```

Then add `dictate` to your module list:

```
status_command bumblebee-status -m dictate memory pasink date time battery ...
```

The module queries the daemon's Unix socket (`/tmp/dictate.sock`) every second,
so there's no polling overhead when the daemon isn't running.

### ydotool note

If you use `method = "ydotool"` for text injection, the daemon will
automatically start `ydotoold` if it isn't already running, and stop it on
daemon exit (only if the daemon started it). No separate setup needed.

## systemd User Service (recommended)

Running the daemon as a systemd user service is the recommended approach.
You get automatic restarts on crash, proper log capture via `journalctl`,
and the daemon starts automatically on login.

```bash
# Install the service
mkdir -p ~/.config/systemd/user
cp systemd/dictate.service ~/.config/systemd/user/
# Edit ExecStart path and Environment as needed:
#   nano ~/.config/systemd/user/dictate.service

# Enable and start
systemctl --user daemon-reload
systemctl --user enable --now dictate

# Check status / logs
systemctl --user status dictate
journalctl --user -u dictate -f    # tail logs
journalctl --user -u dictate -e    # jump to end
```

After rebuilding the binary (`go build -o dictate .`), restart with:
```bash
systemctl --user restart dictate
```

Sample units for model servers are also provided in `systemd/`:

| File | Purpose |
|---|---|
| `dictate.service` | The dictation daemon itself |
| `llamacpp.service` | llama.cpp model server (edit for model size) |
| `vllm-voxtral.service` | vLLM model server (edit for model variant) |

## Testing

```bash
# Test with mock backend (no model needed)
echo '[backend]\nname = "mock"' > /tmp/test.toml
DICTATE_CONFIG=/tmp/test.toml ./dictate test some_audio.pcm

# Create test audio from any media file:
ffmpeg -i input.mp3 -ar 16000 -ac 1 -f s16le output.pcm
```

## Source Layout

```
main.go              — CLI entry point (daemon | toggle | test)
config.go            — TOML config loading with defaults
daemon.go            — Unix socket listener, session lifecycle
indicator.go         — Session indicators (LED, dunstify, command)
vad.go               — Voice activity detection, burst-based speech segmentation
audio.go             — Mic capture via pw-record/arecord subprocess
typist.go            — Text injection (xdotool/ydotool/wtype/dotool)
backend.go           — Backend interface + factory
backend_ws.go        — WebSocket backend (Mistral Realtime + vLLM Realtime)
backend_mistral_batch.go — Mistral HTTP batch transcription
backend_llamacpp.go  — llama.cpp HTTP chat completions with audio
backend_mock.go      — Fake backend for testing
test.go              — File-based test harness
mock_server.go       — Standalone mock HTTP STT server (go run)
config.example.toml  — Annotated example configuration
systemd/             — Service unit files
book/                — Documentation site (mdBook)
```
