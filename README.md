# K8s Monitor

> Local Kubernetes monitoring with AI-powered insights via local LLMs.

Collects logs and metrics from **multiple kubectl contexts**, stores 15 days of history in a local SQLite database, and uses a local LLM to analyze errors, warnings, and performance trends — all running on your machine with zero cloud dependency.

[![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?style=for-the-badge&logo=go)](https://go.dev/)
[![CI](https://img.shields.io/github/actions/workflow/status/rodrigos0ares/k8s-monitor/ci.yml?branch=main&style=for-the-badge&logo=github)](https://github.com/rodrigos0ares/k8s-monitor/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=for-the-badge)](LICENSE)
[![GitHub last commit](https://img.shields.io/github/last-commit/rodrigos0ares/k8s-monitor?style=for-the-badge)](https://github.com/rodrigos0ares/k8s-monitor)

## Features

- **Multi-context monitoring** — watches all pods across 3+ kubectl contexts simultaneously
- **15-day log retention** — stores all pod logs in a local SQLite DB with automatic purging
- **AI-powered analysis** — uses local LLMs to detect error patterns, provide root cause analysis, and suggest fixes
- **Performance metrics** — collects CPU and memory usage via metrics-server
- **Resource efficiency & cost savings** — compares actual pod usage against configured requests/limits to flag over-provisioned workloads, recommend right-sized requests, and estimate monthly cost savings
- **Web dashboard** — built-in dark-theme dashboard at `http://localhost:3333`
- **Alerts** — sends Slack and Discord notifications for critical/high severity findings
- **CLI interface** — rich terminal output with on-demand analysis

## Requirements

- Go 1.23+ (to build)
- kubectl configured with your contexts
- One local inference runtime:
  - llama.cpp server exposing OpenAI-compatible `/v1/*` endpoints, **or**
  - LangChain Go server exposing OpenAI-compatible `/v1/*` endpoints
- NVIDIA GPU with 8GB VRAM (for running LLM models)
- metrics-server installed in your clusters (optional, for CPU/memory data)

## Quick Start

```bash
# 1. Start your inference runtime (llama.cpp or LangChain Go)
llama-server \
  -m /models/Qwen3.5-9B-Instruct-Q4_K_M.gguf \
  --alias qwen3.5-9b \
  --host 0.0.0.0 --port 8080 \
  -c 8192 \
  -ngl 48 \
  -t 20 \
  -np 2 \
  -fa on \
  -cb

# 2. Build the tool
make build

# 3. Initialize config (auto-detects your kubectl contexts)
./bin/k8s-monitor config init

# 4. Start monitoring
./bin/k8s-monitor start
```

## Installation

### From Source

```bash
git clone https://github.com/rodrigos0ares/k8s-monitor.git
cd k8s-monitor
make build
make install  # installs to ~/go/bin/k8s-monitor
```

### Pre-built Binary

Download the latest release from the [Releases](https://github.com/rodrigos0ares/k8s-monitor/releases) page:

```bash
# Example for Linux amd64
wget https://github.com/rodrigos0ares/k8s-monitor/releases/latest/download/k8s-monitor-linux-amd64
chmod +x k8s-monitor-linux-amd64
sudo mv k8s-monitor-linux-amd64 /usr/local/bin/k8s-monitor
```

## Usage

```bash
# Start the monitoring daemon
k8s-monitor start

# Run an AI analysis on-demand
k8s-monitor analyze

# Benchmark one or more models on built-in SRE prompts
k8s-monitor benchmark qwen3.5-9b qwen3.5-9b-q5

# Benchmark and persist results + winner
k8s-monitor benchmark --profile balanced --output ./benchmarks/latest.json --save-best \
  qwen3.5-9b qwen3.5-9b-q5 phi3.5

# View current status and statistics
k8s-monitor status

# Show pod rightsizing recommendations and estimated cost savings
k8s-monitor costs
k8s-monitor costs --hours 48 --context production --limit 50

# List kubectl contexts (✓ = monitored)
k8s-monitor contexts

# Configuration management
k8s-monitor config show
k8s-monitor config init
k8s-monitor config list-profiles
k8s-monitor config set-profile balanced
k8s-monitor config set-model qwen3.5-9b
k8s-monitor config set-provider llama.cpp
k8s-monitor config set-endpoint http://localhost:8080
k8s-monitor config set-cost 0.0316 0.0042 USD

# Purge old data
k8s-monitor purge
```

## Configuration

Config is stored at `~/.k8s-monitor/config.json`:

```json
{
  "contexts": ["production", "staging", "dev"],
  "retention_days": 15,
  "collect_interval_secs": 60,
  "analysis_interval_mins": 30,
  "inference": {
    "provider": "llama.cpp",
    "endpoint": "http://localhost:8080",
    "model": "qwen3.5-9b",
    "temperature": 0.2,
    "max_tokens": 1024,
    "timeout_secs": 300
  },
  "storage": {
    "db_path": "~/.k8s-monitor/data.db"
  },
  "web": {
    "enabled": true,
    "address": ":3333"
  },
  "cost": {
    "currency": "USD",
    "cpu_core_hour": 0.0316,
    "mem_gb_hour": 0.0042
  },
  "alerts": {
    "slack": { "enabled": false, "webhook_url": "" },
    "discord": { "enabled": false, "webhook_url": "" }
  }
}
```

## Inference Providers

k8s-monitor supports two inference backends, both using the OpenAI-compatible `/v1/*` API:

### llama.cpp

The original backend — runs GGUF models directly via [llama.cpp](https://github.com/ggerganov/llama.cpp).

```bash
k8s-monitor config set-provider llama.cpp
k8s-monitor config set-endpoint http://localhost:8080
k8s-monitor config set-model qwen3.5-9b
```

### LangChain Go

[LangChain Go](https://github.com/tmc/langchaingo) provides a flexible gateway that can serve models via llama.cpp, Ollama, vLLM, or any other backend LangChain supports. This lets you switch inference backends without changing k8s-monitor.

```bash
k8s-monitor config set-provider langchain-go
k8s-monitor config set-endpoint http://localhost:9090  # LangChain Go gateway
k8s-monitor config set-model qwen3.5-9b
```

## Resource Efficiency & Cost Savings

The monitor records each container's **actual** CPU/memory usage (via metrics-server)
alongside its **configured** requests and limits (read from the pod specs). It then
compares the two over a rolling window to surface cost-saving opportunities:

- **Over-provisioned** — requests far exceed observed peak usage. The tool recommends
  a new request based on peak usage plus 25% headroom and estimates the reclaimable
  CPU/memory and the resulting monthly savings.
- **No requests set** — containers with no requests can't be bin-packed or cost-controlled.
- **OOM / throttle risk** — peak usage close to the memory or CPU limit, flagged so you
  fix reliability before cutting resources.

Savings are estimated from a configurable pricing model:

```bash
monthly_savings = (reclaimable_cores × cpu_core_hour + reclaimable_gb × mem_gb_hour) × 730
```

Set your own rates (e.g. your cloud/node cost per vCPU-hour and per GB-hour):

```bash
k8s-monitor config set-cost 0.0316 0.0042 USD
```

View the report from the CLI (`k8s-monitor costs`), the dashboard's
"Resource Efficiency & Cost Savings" section, or the `/api/rightsizing` JSON endpoint.
The AI analyzer also produces a periodic `resource_efficiency` insight with a
prioritized, workload-by-workload optimization plan.

## Architecture

```text
┌──────────────────────────────────────────────────────┐
│                   k8s-monitor                        │
│                                                      │
│  ┌───────────┐  ┌───────────┐  ┌──────────────────┐ │
│  │    Log     │  │  Metrics  │  │     Events       │ │
│  │ Collector  │  │ Collector │  │    Collector      │ │
│  └─────┬─────┘  └─────┬─────┘  └────────┬─────────┘ │
│        │              │                  │           │
│        ▼              ▼                  ▼           │
│  ┌────────────────────────────────────────────────┐  │
│  │              SQLite (15-day store)              │  │
│  └──────────────────┬─────────────────────────────┘  │
│                     │                                │
│        ┌────────────┼────────────┐                   │
│        ▼            ▼            ▼                   │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐             │
│  │ Analyzer │ │ Web UI   │ │  Alerts  │             │
│  │(Provider)│ │ :8080    │ │Slack/Disc│             │
│  └──────────┘ └──────────┘ └──────────┘             │
│                                                      │
│  kubectl context 1 ──┐                               │
│  kubectl context 2 ──┼── K8s API                     │
│  kubectl context 3 ──┘                               │
└──────────────────────────────────────────────────────┘
```

## Recommended Models for 8GB GPU

| Model | Size | Strengths |
| --- | --- | --- |
| `qwen3.5-9b` (Q4_K_M) | ~4-bit quantized | Best quality/speed balance on RTX 4060 |
| `qwen3.5-9b-q5` (Q5_K_M) | ~5-bit quantized | Better issue triage quality, slightly slower |
| `phi3.5` / `phi4-mini` | small quantized | Fast fallback profile |
| `gemma3:4b` | small quantized | Lightweight fallback, Google model family |

When using llama.cpp, prefer Q4_K_M or Q5_K_M GGUF quantization and CPU offload if needed.

## RTX 4060 Tuning (Layer Slicing)

Use layer slicing to fit a larger model by splitting work between GPU and CPU RAM:

- `--n-gpu-layers 48`: good first target for Q4_K_M on 8GB VRAM.
- `--n-gpu-layers 32-40`: safer when running other GPU workloads.
- `--parallel 2`: better throughput for monitor + occasional manual analyze.
- `--parallel 1`: lower memory footprint and less latency jitter.

If latency spikes under concurrent requests, reduce `--parallel` first, then lower `--ctx-size` from `8192` to `4096`.

## API Endpoints

The web server exposes JSON APIs:

- `GET /api/stats` — cluster statistics per context
- `GET /api/logs?hours=6&context=prod&severity=error&limit=100` — query logs
- `GET /api/analyses?limit=10` — recent AI analyses
- `GET /api/errors?hours=24` — error/warning aggregation

## How It Works

1. **Collection** (every 60s): Fetches logs from all running pods and metrics from metrics-server across all configured contexts
2. **Storage**: Logs are classified by severity (error/warning/info/debug) and stored in SQLite with indexes for fast queries
3. **Analysis** (every 30m): Sends recent error/warning logs and performance data to the configured provider for AI analysis
4. **Alerting**: Critical/high severity findings are sent to Slack/Discord
5. **Purging** (daily): Removes data older than 15 days

## Development

```bash
# Build
make build

# Run tests
make test

# Run linter
make lint

# Clean build artifacts
make clean
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License — see the [LICENSE](LICENSE) file for details.
