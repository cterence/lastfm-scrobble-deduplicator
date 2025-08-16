package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v5"
)

func Run(ctx context.Context, c *Config) error {
	err := checkConfig(c)
	if err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	err = initApp(ctx, c)
	if err != nil {
		return fmt.Errorf("failed to init app: %w", err)
	}
	defer c.allocCancel()
	defer c.taskCancel()
	defer c.cache.Close()

	err = login(c.taskCtx, c)
	if err != nil {
		return fmt.Errorf("failed to login to Last.fm: %w", err)
	}

	startingPage, err := getStartingPage(c)
	if err != nil {
		if errors.Is(err, ErrNoScrobbles) {
			return err
		}
		return fmt.Errorf("failed to get starting page: %w", err)
	}

	userTrackDurations, err := getUserTrackDurations()
	if err != nil {
		return fmt.Errorf("failed to get user track durations: %w", err)
	}
	unknownTrackDurations := make(durationByTrackByArtist, 0)

	var previousScrobble *scrobble
	var lastScrobbleDeleted bool

	for currentPage := startingPage; currentPage > 0; currentPage-- {
		slog.Info("Processing page", "page", currentPage)
		scrobbles, err := getScrobbles(c, currentPage)
		if err != nil {
			return err
		}

		for _, s := range scrobbles {
			// Check if the track is in the user specified track durations file
			err := getTrackDuration(ctx, c, userTrackDurations, unknownTrackDurations, &s)
			if err != nil {
				slog.Warn("failed to get track duration, skipping scrobble", "error", err)
				continue
			}
			slog.Debug("Track duration found", "artist", s.artist, "track", s.track, "duration", s.duration)

			lastScrobbleDeleted = false
			if previousScrobble != nil {
				// Check if the current scrobble is a duplicate of the previous one
				if s.artist == previousScrobble.artist && s.track == previousScrobble.track && s.timestamp != previousScrobble.timestamp {
					timeDiff := s.timestamp.Sub(previousScrobble.timestamp)
					if timeDiff < s.duration {
						slog.Info("🎯 Duplicate scrobble detected!", "artist", s.artist, "track", s.track, "timestamp", s.timestamp)
						c.runStats.deletedScrobbles = append(c.runStats.deletedScrobbles, s)
						lastScrobbleDeleted = true
						_, err = backoff.Retry(ctx, func() (struct{}, error) {
							return struct{}{}, deleteScrobble(c, s.timestampString, c.Delete)
						}, backoff.WithMaxTries(5))
						if err != nil {
							slog.Error("Failed to delete duplicate scrobble", "artist", s.artist, "track", s.track, "timestamp", s.timestamp, "error", err)
						}
						if c.Delete {
							slog.Info("Scrobble deleted", "artist", s.artist, "track", s.track, "timestamp", s.timestamp)
						}
					}
				}
			}
			if !lastScrobbleDeleted {
				previousScrobble = &s
			}
			c.runStats.processedScrobbles++
		}
	}

	slog.Info("Processing complete!")

	c.runStats.elapsedTime = time.Since(c.startTime)
	logStats(c)

	err = writeUnknownTrackDurations(unknownTrackDurations)
	if err != nil {
		return err
	}

	return nil
}
