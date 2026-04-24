# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ZS3Server is a decentralized S3-compatible storage gateway built on the Züs (0chain) blockchain network. It wraps MinIO as a gateway to route S3 API calls to the Züs decentralized storage network via GoSDK.

**Three main components:**
1. **zs3server** (MinIO gateway) — S3 API on port 9000
2. **logsearchapi** — Audit log storage/retrieval backed by PostgreSQL
3. **client-api** — REST API proxy with API key auth on port 3001

## Build Commands

```bash
make build          # Build minio binary (runs in Docker: golang:1.20)
make install        # Build and install to $GOPATH/bin
make docker         # Build production Docker image
make lint           # Run golangci-lint
make verifiers      # Run linters (getdeps + lint + check-gen)
make clean          # Remove generated assets
```

**Dev environment (from `environment/` directory):**
```bash
make dev            # docker-compose -f docker-compose-dev.yaml up -d --build
make prd            # docker-compose up (production)
make build-client   # Build client API Docker image
```

## Running Tests

```bash
# Unit tests
CGO_ENABLED=0 go test -tags kqueue ./...

# With race detector
CGO_ENABLED=1 go test -race -tags kqueue ./...

# Full test suite (build + lint + tests)
make test

# Race condition tests
make test-race

# IAM-specific tests
make test-iam
```

## Runtime Configuration

ZS3Server reads configuration at startup from `~/.zcn/`:
- `config.yaml` — Network and blockchain config
- `wallet.json` — User wallet/credentials
- `allocation.txt` — Züs allocation ID
- `zs3server.json` — Server tuning options (optional, uses defaults if missing)
- `network.yaml` — Network config (optional)

**`zs3server.json` tuning parameters** (defaults vs. high-throughput):

| Parameter | Default | High-Throughput |
|---|---|---|
| `max_batch_size` | 25 | 200 |
| `batch_wait_time` | 500ms | 50ms |
| `batch_workers` | 5 | 20 |
| `upload_workers` | 4 | 20 |
| `download_workers` | 6 | 20 |
| `max_concurrent_requests` | 150 | 2000 |

## Architecture

### ZCN Gateway (`cmd/gateway/zcn/`)

The core logic. Key files:

- **`gateway-zcn.go`** — Registers the ZCN backend with MinIO; implements all S3 operations (GetObject, PutObject, ListObjects, DeleteObject, etc.)
- **`initSDK.go`** — Loads `~/.zcn/` config, initializes Züs SDK and wallet, reads server options
- **`dStorage.go`** — Low-level Züs decentralized storage operations: file listing (pageLimit=500, numBlocks=100), uploads/downloads, directory management
- **`multipart.go`** — Multipart upload handling using a sequential priority queue; batches parts for efficient upload
- **`worker.go`** — Worker pool management for upload/download concurrency
- **`helper.go`** — Utility functions
- **`cb.go`** — Callback handlers for async SDK operations
- **`seqpriorityqueue/`** — Custom sequential priority queue for ordered multipart part assembly

### Data Flow

```
S3 Client → MinIO Gateway (port 9000) → ZCN Gateway → Züs GoSDK → 0chain network
                    ↓
            LogSearch API (port 8085) → PostgreSQL (port 5434)
```

### Key Constants (`dStorage.go`)
- `pageLimit = 500` — Files per listing page
- `numBlocks = 100` — Blockchain blocks per request
- `maxSizeForMemoryFile = 16MB` — Files above this write to disk

## Docker Compose Services

| Service | Image | Port |
|---|---|---|
| postgres-db | postgres:13-alpine | 5434 |
| logsearchapi | 0chaindev/blimp-logsearchapi | 8085 |
| minioserver | 0chaindev/blimp-minioserver | 9000 |
| minioclient | 0chaindev/blimp-clientapi | 3001 |

## Go Module Structure

- **Main module:** `github.com/minio/minio` (Go 1.22.5)
- **Key dependency:** `github.com/0chain/gosdk` — Züs SDK for all blockchain/storage ops
- **Workspace** (`go.work`): includes `.`, `./client-api`, `./cmd/gateway/zcn/multipart-test/s3gateway`

## NFS Gateway (S3 Files equivalent)

NFSv3 filesystem access to the same blobber data as S3. Both protocols share the WAL, writeback cache, and batch workers.

**Enable in `zs3server.json`:**
```json
{ "enable_nfs": true, "nfs_port": 2049 }
```

**Mount:** `mount -t nfs -o vers=3,tcp,nolock <host>:/ /mnt/zs3`

**Files:** `nfs_server.go`, `nfs_fs.go`, `nfs_file.go` in `cmd/gateway/zcn/`

See [NFS_GATEWAY_ARCHITECTURE.md](./NFS_GATEWAY_ARCHITECTURE.md) for full design.

## Important Notes

- MinIO is used as a gateway framework only — actual storage is on Züs network
- The `feat/enterprise-timings` branch targets `staging` (not main)
- Stop all Docker containers before rebuilding images
- Always check Docker logs (`docker logs <container>`) before making changes
