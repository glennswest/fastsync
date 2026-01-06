# FastSync

A high-performance OpenShift container image mirroring utility written in Go. Efficiently mirrors OpenShift release images from a source registry to a private destination registry using configurable parallel workers.

## Features

- Parallel image mirroring with configurable worker pools
- Per-image blob parallelism for maximum throughput
- Smart skip detection (avoids re-transferring existing images)
- Graceful shutdown handling (SIGINT/SIGTERM)
- Retry logic with exponential backoff
- Support for insecure registries
- Progress tracking with real-time statistics

## Installation

```bash
go build -o fastsync .
```

## Usage

```bash
fastsync \
  --version 4.20.8 \
  --dest-registry registry.example.com \
  --pull-secret /path/to/pull-secret.json \
  [--workers 16] \
  [--blob-workers 4] \
  [--dest-repo openshift/release] \
  [--source-registry quay.io/openshift-release-dev/ocp-release] \
  [--insecure]
```

### Required Arguments

| Argument | Description |
|----------|-------------|
| `--version` | OpenShift version to mirror (e.g., `4.20.8`) |
| `--dest-registry` | Target registry URL (e.g., `registry.example.com`) |
| `--pull-secret` | Path to Docker auth JSON file |

### Optional Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--workers` | 16 | Number of parallel image workers |
| `--blob-workers` | 4 | Number of parallel blob uploads per image |
| `--dest-repo` | `openshift/release` | Target repository path |
| `--source-registry` | `quay.io/openshift-release-dev/ocp-release` | Source registry |
| `--insecure` | false | Skip TLS verification |

### Pull Secret Format

Standard Docker config format:

```json
{
  "auths": {
    "registry.example.com": {
      "auth": "base64(username:password)"
    },
    "quay.io": {
      "auth": "base64(username:password)"
    }
  }
}
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        main.go                              │
│                   (CLI & Orchestration)                     │
│  - Parse arguments                                          │
│  - Load pull secrets                                        │
│  - Setup signal handling                                    │
│  - Coordinate mirroring                                     │
└─────────────────┬───────────────────────┬───────────────────┘
                  │                       │
                  ▼                       ▼
┌─────────────────────────────┐  ┌────────────────────────────┐
│      pkg/release            │  │       pkg/mirror           │
│  (Release Metadata)         │  │   (Parallel Mirroring)     │
│                             │  │                            │
│  - GetRelease()             │  │  - Engine struct           │
│  - Parse image-references   │  │  - Worker pool management  │
│  - Extract component list   │  │  - Progress tracking       │
│                             │  │  - Retry logic             │
│                             │  │  - Skip detection          │
└─────────────────────────────┘  └────────────────────────────┘
                  │                       │
                  └───────────┬───────────┘
                              ▼
              ┌───────────────────────────────┐
              │  github.com/google/           │
              │  go-containerregistry         │
              │                               │
              │  Container image operations   │
              └───────────────────────────────┘
```

### Package Responsibilities

**pkg/release**
- Connects to source registry and fetches release image manifest
- Extracts `image-references` file from release image layers
- Parses component image list with full digest references

**pkg/mirror**
- `Engine`: Main orchestrator with configurable parallelism
- Worker pool pattern with channel-based job distribution
- Thread-safe progress tracking with atomic counters
- Custom `PullSecretKeychain` for registry authentication
- Smart skip detection checks destination before pulling

### Data Flow

1. CLI parses arguments and loads Docker auth credentials
2. `release.GetRelease()` fetches release manifest from source
3. `mirror.Engine` creates worker pool (default: 16 workers)
4. Workers pull from job channel, each with blob parallelism (default: 4)
5. For each image:
   - Check if already exists at destination (skip if present)
   - Copy with retry logic (3 attempts, exponential backoff)
   - Report progress via callback
6. Aggregate results and display summary

### Performance Tuning

Default configuration allows up to 64 concurrent blob operations (16 workers × 4 blob-workers). Adjust based on:

- Network bandwidth
- Registry rate limits
- Available memory

## Example

Mirror OpenShift 4.20.8 to a private registry:

```bash
fastsync \
  --version 4.20.8 \
  --dest-registry registry.internal.example.com \
  --pull-secret ~/.docker/config.json \
  --workers 20 \
  --blob-workers 8
```

## License

MIT
