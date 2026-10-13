# Systemd User Service — llama.cpp Server

This directory contains a systemd user service template for running the llama.cpp
OpenAI-compatible server that k8s-monitor uses for AI-powered analysis.

## Setup

1. **Install llama.cpp** — ensure the `llama-server` binary is available:

   ```bash
   # Example: install to ~/.local/bin
   mkdir -p ~/.local/bin
   cp /path/to/llama-server ~/.local/bin/llama-server
   chmod +x ~/.local/bin/llama-server
   ```

2. **Place your model** — update the model path in the service file to match your setup:

   ```bash
   # Edit the ExecStart line in llama-server.service
   # Change: -m %h/models/qwen3.5-9b/Qwen3.5-9B-Q4_K_M.gguf
   # To your actual model path
   ```

3. **Install the service**:

   ```bash
   mkdir -p ~/.config/systemd/user
   cp llama-server.service ~/.config/systemd/user/
   systemctl --user daemon-reload
   ```

4. **Enable and start**:

   ```bash
   systemctl --user enable --now llama-server.service
   ```

5. **Verify**:

   ```bash
   systemctl --user status llama-server.service
   curl -s http://localhost:8080/v1/models | head -20
   ```

## Configuration

Edit `llama-server.service` and adjust these values in the `ExecStart` line:

| Parameter | Description | Example |
| --- | --- | --- |
| `-m` | Path to GGUF model file | `%h/models/qwen3.5-9b/Qwen3.5-9B-Q4_K_M.gguf` |
| `--host` | Bind address | `0.0.0.0` |
| `--port` | HTTP port | `8080` |
| `-c` | Context size | `16384` |
| `-ngl` | GPU layers (0 = CPU only) | `48` |
| `-t` | CPU threads | `20` |
| `-np` | Parallel requests | `2` |

## Management

```bash
# Stop
systemctl --user stop llama-server.service

# Restart
systemctl --user restart llama-server.service

# View logs
journalctl --user -u llama-server.service -f

# Disable auto-start
systemctl --user disable llama-server.service
```

## Notes

- The service runs as a **user service** (not system-wide). It starts when you log in.
- For GPU acceleration, ensure `--n-gpu-layers` is set to a value > 0.
- Monitor VRAM usage with `nvidia-smi` while tuning `-ngl`.
