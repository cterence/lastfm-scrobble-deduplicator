package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
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

			lastScrobbleDeleted := false
			if previousScrobble != nil {
				// Check if the current scrobble is a duplicate of the previous one
				lastScrobbleDeleted, err = detectAndDeleteDuplicateScrobble(ctx, c, previousScrobble, s)
				if err != nil {
					slog.Warn("failed to detect and delete duplicated scrobble", "error", err)
				}
			}
			if !lastScrobbleDeleted || previousScrobble == nil {
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

	slog.Info("Exiting")

	return nil
}
