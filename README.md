# Instagram Monitor

A Kubernetes-native CronJob that monitors public Instagram accounts for new posts
and sends webhook notifications when posts match configurable keywords.

Designed for tattoo artists (or anyone) who announce appointment openings via
Instagram — get alerted the moment they post.

## Features

- **Multi-user monitoring** — track any number of Instagram accounts in one config
- **Keyword matching** — filter alerts by keywords (e.g., "book", "open", "appointment")
- **Catch-all mode** — get notified on ANY new post, not just keyword matches
- **Persistent state** — remembers what posts it's already seen (PVC-backed JSON file)
- **Webhook notifications** — POSTs alerts to any HTTP endpoint (Slack, Discord, etc.)
- **Dry-run mode** — test configuration without sending real alerts
- **Minimal footprint** — ~12MB Alpine container, no external dependencies

## Quick Start

### Local Testing

```bash
# Build (the Dockerfile uses fully qualified image names for podman compatibility)
docker build -t instagram-monitor:latest .

# Run with sample config
docker run --rm -v $(pwd)/config.sample.json:/app/config/config.json:ro \
  instagram-monitor:latest --dry-run

# Monitor a real artist
cat > my-config.json << 'EOF'
{
  "monitors": [
    {
      "username": "tattooartist_username",
      "displayName": "@tattooartist_username",
      "keywords": ["book", "booking", "open", "appointment"],
      "notifyOnAny": false
    }
  ],
  "notificationURL": "https://hooks.slack.com/services/...",
  "httpTimeout": 30
}
EOF

docker run --rm -v $(pwd)/my-config.json:/app/config/config.json:ro \
  instagram-monitor:latest --dry-run
```

### Building & Pushing for Kubernetes

```bash
# Build and push to GHCR (podman login ghcr.io first)
docker build -t ghcr.io/bdfrost/instagram-monitor:latest .
docker push ghcr.io/bdfrost/instagram-monitor:latest
```

**Podman note:** If you get `short-name did not resolve` errors, the Dockerfile already uses fully qualified image names (`docker.io/library/...`). If you need to add unqualified search registries, add this to `/etc/containers/registries.conf`:

```toml
unqualified-search-registries = ["docker.io", "quay.io"]
```

### Kubernetes Deployment (Helm)

```bash
helm install instagram-monitor ./charts/instagram-monitor \
  --set "monitors[0].username=my_artist" \
  --set "monitors[0].displayName=My Artist" \
  --set "monitors[0].keywords={book,booking,open}" \
  --set notificationURL="https://hooks.slack.com/services/..."
```

### Via ArgoCD

The chart follows the standard ArgoCD app-of-apps pattern. Add to your repo:

```yaml
# apps/templates/instagram-monitor.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: instagram-monitor
  namespace: argocd
spec:
  source:
    path: charts/instagram-monitor
    repoURL: https://github.com/bdfrost/argocd.git
    targetRevision: HEAD
  destination:
    server: https://kubernetes.default.svc
    namespace: instagram-monitor
  project: default
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

## Configuration

### Monitors Array

Each entry in `monitors` defines one Instagram account to watch:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `username` | string | yes | Instagram username (without @) |
| `displayName` | string | no | Friendly name for alerts (defaults to @username) |
| `keywords` | []string | no | Keywords to match in post captions (case-insensitive) |
| `notifyOnAny` | bool | no | If true, alert on ANY new post (ignores keywords) |

### Global Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `notificationURL` | string | "" | Webhook URL to POST alerts to |
| `httpTimeout` | int | 30 | HTTP request timeout in seconds |
| `stateFile` | string | /app/state/state.json | Path for persisting seen-post state |

### Environment Variable Overrides

| Variable | Description |
|----------|-------------|
| `NOTIFICATION_URL` | Overrides notificationURL from config |
| `HTTP_TIMEOUT` | Overrides httpTimeout from config |

## How It Works

1. On each CronJob run, the binary reads the JSON config from ConfigMap
2. For each configured monitor, it fetches the Instagram profile page
3. It extracts the latest posts from the embedded GraphQL data
4. New posts (shortcodes not seen before) are checked against keywords
5. Matching posts generate formatted alert messages
6. Alerts are POSTed to the notification webhook as JSON
7. State (last seen post per user) is persisted to a PVC-backed volume
8. On next run, only truly NEW posts since the last check trigger alerts

### First-Run Behavior

On the first run for a given username, all existing posts are treated as
baseline — no alerts are generated. Only posts created AFTER the first run
will trigger notifications. This prevents alert spam on initial deployment.

### Alert Payload

Webhook POST body:

```json
{
  "service": "instagram-monitor",
  "alerts": 2,
  "messages": [
    "📸 New post from @artist (matched keyword: \"book\")\nhttps://www.instagram.com/p/ABC123/\nPosted: 2025-01-15 14:30 UTC\nCaption: Books are now open! DM me...",
    "..."
  ],
  "timestamp": "2025-01-15T15:00:00Z"
}
```

## Scraping Notes

This tool uses Instagram's public web interface (no API key required).
Instagram periodically changes their page structure, which can break the
parser. If you see "could not extract embedded data" errors:

1. Check if the page HTML structure has changed
2. Update the regex patterns in `main.go`
3. Consider switching to a more robust scraper as a fallback

Private accounts are not supported — only public Instagram profiles work.

## Project Structure

```
.
├── cmd/monitor/main.go      # Main application
├── internal/                 # Reserved for future shared libraries
├── charts/
│   └── instagram-monitor/   # Helm chart
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
├── config.sample.json       # Sample configuration
├── Dockerfile               # Multi-stage build (fully-qualified image names for podman)
├── .dockerignore
├── LICENSE
└── README.md
```

## License

MIT
