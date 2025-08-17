package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cterence/scrobble-deduplicator/internal/cache"
	"github.com/michiwend/gomusicbrainz"
)

type Config struct {
	// Inputs
	FilePath           string
	CacheType          string
	LastFMUsername     string
	LastFMPassword     string
	Delete             bool
	StartPage          int
	From               time.Time
	To                 time.Time
	BrowserHeadful     bool
	RedisURL           string
	BrowserURL         string
	LogLevel           string
	DuplicateThreshold int

	// Internal dependencies
	startTime time.Time
	cache     cache.Cache
	runStats  stats
	mb        *gomusicbrainz.WS2Client
	taskCtx   context.Context

	// Closing functions
	allocCancel context.CancelFunc
	taskCancel  context.CancelFunc
}

type stats struct {
	cacheHits                      int
	cacheMisses                    int
	processedScrobbles             int
	deletedScrobbles               []scrobble
	unknownTrackDurationsCount     int
	skippedScrobbleUnknownDuration int
	scrobbleDeleteFails            int
	elapsedTime                    time.Duration
}

func (c *Config) checkConfig() error {
	slog.Debug("Validating config")

	if c.CacheType == "redis" && c.RedisURL == "" {
		return errors.New("must set redis-url if cache-type is redis")
	}

	if c.StartPage != 0 && (!c.From.IsZero() || !c.To.IsZero()) {
		return errors.New(`start-page and "from" / "to" dates must not be set at the same time`)
	}

	if !c.From.IsZero() && !c.To.IsZero() && c.From.After(c.To) {
		return errors.New(`"to" date must be after "from" date`)
	}

	return nil
}

func (c *Config) close() {
	c.allocCancel()
	c.taskCancel()
	c.cache.Close()
}

func (c *Config) handleInterrupts() {
	sigInterrupt := make(chan os.Signal, 1)
	signal.Notify(sigInterrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigInterrupt
		slog.Warn("Closing due to interrupt")
		c.close()

		os.Exit(1)
	}()
}
