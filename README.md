# Last.fm Scrobble Deduplicator

A Go-based tool that automatically detects and removes duplicate scrobbles from your Last.fm profile using intelligent duplicate detection algorithms and browser automation.

## 🎯 Why This Tool Exists

Many music scrobblers create duplicate entries due to:

- Network interruptions during scrobbling
- Multiple devices scrobbling the same track simultaneously
- Scrobblers retrying failed requests
- Manual corrections that create duplicates

Instead of manually fixing these issues or dealing with problematic scrobblers, this tool periodically cleans up your profile by intelligently identifying and removing duplicate scrobbles.

## ✨ Features

- **Smart Duplicate Detection**: Based on track duration and timing thresholds
- **Incomplete Scrobble Detection**: Finds scrobbles that were interrupted before completion
- **Multiple Cache Backends**: Redis, file-based, and in-memory caching
- **MusicBrainz Integration**: Automatic track duration fetching
- **Browser Automation**: Chrome/Chromium interaction with Last.fm
- **Telegram Notifications**: Optional completion notifications
- **Docker Support**: Full containerization with Redis and Chromium
- **Cross-platform**: Linux, macOS, and Windows binaries

## 🚀 Quick Start

### Prerequisites

- **Binary releases**: Chrome or Chromium browser (required for chromedp)
- **Source builds**: Go 1.24+, Chrome or Chromium browser
- Docker setup (alternative to local browser)
- Last.fm account credentials

### Using Pre-built Binaries (Recommended)

1. **Download from [GitHub Releases](https://github.com/cterence/scrobble-deduplicator/releases):**
   - Linux: `scrobble-deduplicator-linux-amd64` or `scrobble-deduplicator-linux-arm64`
   - macOS: `scrobble-deduplicator-darwin-amd64` or `scrobble-deduplicator-darwin-arm64`
   - Windows: `scrobble-deduplicator-windows-amd64.exe`

2. **Setup:**

   ```bash
   chmod +x scrobble-deduplicator-linux-amd64  # Linux/macOS
   curl -O https://raw.githubusercontent.com/cterence/scrobble-deduplicator/main/config.example.yaml
   mv config.example.yaml config.yaml
   # Edit config.yaml with your credentials
   ```

3. **Run:**

   ```bash
   ./scrobble-deduplicator-linux-amd64 -c config.yaml
   ```

### Using Docker

```bash
# Download the production compose file
curl -O https://raw.githubusercontent.com/cterence/scrobble-deduplicator/main/compose.yaml

# Download and edit configuration
curl -O https://raw.githubusercontent.com/cterence/scrobble-deduplicator/main/config.example.yaml
mv config.example.yaml config.yaml
# Edit config.yaml with your credentials

# Run with Docker
docker compose up
```

### Building from Source

```bash
nix develop
go build -o scrobble-deduplicator .
./scrobble-deduplicator -c config.yaml
```

## 📋 Configuration

Create `config.yaml`:

```yaml
cacheType: inmemory  # redis|file|inmemory
lastfm:
  username: your_username
  password: your_password
from: 01-01-2025  # Optional date range
to: 01-03-2025
browserHeadful: false # Set to true to open a browser window
canDelete: false  # Set to true to enable deletion
duplicateThreshold: 90  # Percentage threshold
completeThreshold: 50   # Completion threshold
dataDir: ./data
telegramBotToken: ""  # Optional
telegramChatID: ""    # Optional
```

### Command Line Options

```bash
# Basic usage
./scrobble-deduplicator -u username -p password

# With date range
./scrobble-deduplicator -u username -p password --from 01-01-2025 --to 01-03-2025

# Enable deletion
./scrobble-deduplicator -u username -p password --delete

# Custom thresholds
./scrobble-deduplicator -u username -p password --duplicate-threshold 85
```

## 🔧 How It Works

### Duplicate Detection

- Compares consecutive scrobbles of the same track
- Calculates time difference between scrobbles
- Determines if time difference is less than configurable percentage of track duration
- Uses MusicBrainz API for accurate track durations

**Example**: If a 4-minute track has two scrobbles 2 minutes apart, the second is considered a duplicate if threshold is 90% (since 2 minutes is only 50% of track duration).

### Browser Automation

- Navigates through Last.fm library pages
- Extracts scrobble information from HTML
- Automatically deletes identified duplicates
- Handles cookie management and consent banners

### Caching Strategy

- **In-Memory**: Fastest, data lost on restart
- **File**: Persistent, good for single instances
- **Redis**: Distributed, ideal for production

## 🐳 Docker Development

```bash
# Development (with hot reload)
docker compose -f compose.dev.yaml up --build

# Production
docker compose up --build
```

## 🔍 Development

```bash
nix develop
go test ./...
```

### Code Quality

- Pre-commit hooks for formatting, linting, and static analysis
- Automated testing and CI/CD pipeline

## 📊 Output and Reporting

- **CSV Export**: Deleted scrobbles with timestamps
- **Statistics**: Cache hits/misses, processing time, error counts
- **Telegram Notifications**: Optional completion reports
- **Logging**: Comprehensive audit trail

## 🚨 Safety Features

- **Dry-run by default**: Set `canDelete: true` to enable deletion
- **Configurable thresholds**: Fine-tune detection sensitivity
- **Date range limits**: Process only specific time periods
- **Comprehensive logging**: Full audit trail
- **Error handling**: Robust retry mechanisms

## 🤝 Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests and quality checks
5. Submit a pull request

## 🆘 Troubleshooting

### Common Issues

- **Browser Connection**: Ensure Chrome/Chromium is accessible, use `browserHeadful: true` for debugging
- **Authentication**: Verify Last.fm credentials
- **Cache Issues**: Verify Redis connection, check file permissions
- **Performance**: Use Redis cache for large libraries, adjust date ranges

### Getting Help

- Check logs for detailed error information
- Review configuration options
- Open an issue with detailed error logs

---

**Note**: This tool modifies your Last.fm profile data. Always test with `canDelete: false` first.
