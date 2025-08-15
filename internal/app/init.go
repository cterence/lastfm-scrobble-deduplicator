package app

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/cterence/scrobble-deduplicator/internal/cache"
	"github.com/michiwend/gomusicbrainz"
	"github.com/redis/go-redis/v9"
)

func initApp(ctx context.Context, c *Config) error {
	var slogLogLevel slog.Level

	switch c.LogLevel {
	case "debug":
		slogLogLevel = slog.LevelDebug
	case "info":
		slogLogLevel = slog.LevelInfo
	case "warn":
		slogLogLevel = slog.LevelWarn
	case "error":
		slogLogLevel = slog.LevelError
	default:
		return fmt.Errorf("unknown log level: %s", c.LogLevel)
	}
	logOpts := slog.HandlerOptions{
		Level: slogLogLevel,
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &logOpts)))

	c.startTime = time.Now()

	switch c.CacheType {
	case "redis":
		slog.Info("Using Redis cache")
		redisURLParts, err := url.Parse(c.RedisURL)
		if err != nil {
			return fmt.Errorf("failed to parse Redis URL: %w", err)
		}

		redisPassword, _ := redisURLParts.User.Password()
		redisDB, err := strconv.Atoi(strings.Split(redisURLParts.Path, "/")[1])
		if err != nil {
			return fmt.Errorf("failed to extract Redis DB from URL: %w", err)
		}

		rdb := redis.NewClient(&redis.Options{
			Addr:     redisURLParts.Host,
			Username: redisURLParts.User.Username(),
			Password: redisPassword,
			DB:       redisDB,
		})

		// Test the connection
		status := rdb.Ping(ctx)
		if status.Err() != nil {
			return fmt.Errorf("failed to connect to Redis: %w", status.Err())
		}
		c.cache = cache.NewRedis(rdb)
	case "inmemory":
		slog.Info("Using in-memory cache")
		c.cache = cache.NewInMemory()
	default:
		return fmt.Errorf("unsupported cache type: %s", c.CacheType)
	}

	mb, err := gomusicbrainz.NewWS2Client("https://musicbrainz.org", "lastfm-scrobble-deduplicator", "1.0", "https://github.com/cterence")
	if err != nil {
		return fmt.Errorf("failed to create MusicBrainz client: %w", err)
	}
	c.mb = mb

	var (
		allocCtx    context.Context
		allocCancel context.CancelFunc
	)
	if c.BrowserURL != "" {
		allocCtx, allocCancel = chromedp.NewRemoteAllocator(ctx, c.BrowserURL, chromedp.NoModifyURL)
	} else {
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", !c.BrowserHeadful),
		)
		allocCtx, allocCancel = chromedp.NewExecAllocator(ctx, opts...)
	}
	c.allocCancel = allocCancel

	taskCtx, taskCancel := chromedp.NewContext(
		allocCtx,
		chromedp.WithLogf(log.Printf),
	)

	slog.Info("Starting browser")
	// ensure that the browser process is started
	if err := chromedp.Run(taskCtx); err != nil {
		return fmt.Errorf("failed to start ChromeDP: %w", err)
	}

	c.taskCtx = taskCtx
	c.taskCancel = taskCancel

	return nil
}
