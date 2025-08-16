package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antchfx/htmlquery"
	"github.com/cenkalti/backoff/v5"
	"github.com/chromedp/chromedp"
	"github.com/cterence/scrobble-deduplicator/internal/cache"
	"github.com/goccy/go-yaml"
	"github.com/michiwend/gomusicbrainz"
)

type Config struct {
	// Inputs
	FilePath       string
	CacheType      string
	LastFMUsername string
	LastFMPassword string
	Delete         bool
	StartPage      int
	StartDay       time.Time
	EndDay         time.Time
	BrowserHeadful bool
	RedisURL       string
	BrowserURL     string
	LogLevel       string

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
	cacheHits                  int
	cacheMisses                int
	processedScrobbles         int
	deletedScrobbles           []scrobble
	unknownTrackDurationsCount int
	elapsedTime                time.Duration
}

type scrobble struct {
	artist          string
	track           string
	timestamp       time.Time
	timestampString string
	duration        time.Duration
}

const (
	customTrackDurationsFile = "track-durations.yaml"
	browserOperationsTimeout = 30 * time.Second
	InputDayFormat           = "02-01-2006"
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
		return fmt.Errorf("failed to get starting page: %w", err)
	}

	userTrackDurations, err := getUserTrackDurations()
	if err != nil {
		return fmt.Errorf("failed to get user track durations: %w", err)
	}
	unknownTrackDurations := make(map[string]map[string]string, 0)

	var previousScrobble *scrobble
	var lastScrobbleDeleted bool

	for currentPage := startingPage; currentPage > 0; currentPage-- {
		slog.Info("Processing page", "page", currentPage)
		scrobbles, err := getScrobbles(c, currentPage)
		if err != nil {
			return err
		}

		for _, s := range scrobbles {
			if userTrackDurations != nil && userTrackDurations[s.artist] != nil && userTrackDurations[s.artist][s.track] != "" {
				// Convert to duration with 4m0s format
				trackDuration, err := time.ParseDuration(userTrackDurations[s.artist][s.track])
				if err != nil {
					slog.Error("Failed to parse user duration", "artist", s.artist, "track", s.track, "error", err)
				}
				s.duration = trackDuration
				slog.Debug("Found track duration in user track durations", "artist", s.artist, "track", s.track, "duration", s.duration)
			} else {
				query := fmt.Sprintf(`artist:"%s" AND recording:"%s"`, s.artist, s.track)
				// Hash the query
				queryHasher := sha256.New()
				queryHasher.Write([]byte(query))
				queryHash := queryHasher.Sum(nil)

				val, err := c.cache.Get(ctx, fmt.Sprintf("mbquery:%x", queryHash))
				if err != nil {
					if errors.Is(err, cache.ErrCacheMiss) {
						c.runStats.cacheMisses++
						slog.Debug("Cache miss for track duration query", "artist", s.artist, "track", s.track, "duration", s.duration)
						trackDurations, err := backoff.Retry(ctx, func() (time.Duration, error) {
							return getTrackDurationsFromMusicBrainz(ctx, c, query, queryHash)
						}, backoff.WithBackOff(backoff.NewExponentialBackOff()), backoff.WithMaxTries(10))
						if err != nil {
							slog.Error("failed to get track duration from MusicBrainz API", "error", err)
							continue
						}
						s.duration = trackDurations
						slog.Debug("Found track duration from MusicBrainz API", "artist", s.artist, "track", s.track, "duration", s.duration)
					} else {
						slog.Error("Failed to get cached track duration", "query", query, "error", err)
						continue
					}
				} else {
					c.runStats.cacheHits++
					cachedTrackDurations, err := strconv.Atoi(val)
					if err != nil {
						slog.Error("Failed to parse cached duration", "query", query, "error", err)
						continue
					}
					s.duration = time.Duration(cachedTrackDurations) * time.Millisecond
					slog.Debug("Cache hit for track duration query", "artist", s.artist, "track", s.track, "duration", s.duration)
				}
				if s.duration < 0 {
					if unknownTrackDurations[s.artist] == nil {
						unknownTrackDurations[s.artist] = make(map[string]string)
					}
					if _, found := unknownTrackDurations[s.artist][s.track]; !found {
						unknownTrackDurations[s.artist][s.track] = ""
						c.runStats.unknownTrackDurationsCount++
						slog.Warn("No track duration found for query, saved to unknown track durations", "query", query, "artist", s.artist, "track", s.track)
					}
					continue
				}
			}

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

	slog.Info("Completed!")

	c.runStats.elapsedTime = time.Since(c.startTime)
	logStats(c)

	err = writeUnknownTrackDurations(unknownTrackDurations)
	if err != nil {
		return err
	}

	return nil
}

func generateScrobble(row string) (scrobble, error) {
	// Execute xpath on the row
	var (
		artist       string
		track        string
		timestamp    time.Time
		timestampStr string
	)

	doc, err := htmlquery.Parse(strings.NewReader(row))
	if err != nil {
		return scrobble{}, fmt.Errorf("failed to parse row HTML: %w", err)
	}

	artistNode := htmlquery.FindOne(doc, `.//input[@name='artist_name']`)
	if artistNode != nil {
		artist = strings.TrimSpace(htmlquery.SelectAttr(artistNode, "value"))
	} else {
		return scrobble{}, fmt.Errorf("artist not found in row: %s", row)
	}

	trackNode := htmlquery.FindOne(doc, `.//input[@name='track_name']`)
	if trackNode != nil {
		track = strings.TrimSpace(htmlquery.SelectAttr(trackNode, "value"))
	} else {
		return scrobble{}, fmt.Errorf("track not found in row: %s", row)
	}

	timestampNode := htmlquery.FindOne(doc, `.//input[@name='timestamp']`)
	if timestampNode != nil {
		timestampStr = strings.TrimSpace(htmlquery.SelectAttr(timestampNode, "value"))
		// Timestamp is 1754948517
		timestampInt, err := strconv.ParseInt(timestampStr, 10, 64)
		if err != nil {
			return scrobble{}, fmt.Errorf("failed to parse timestamp: %w", err)
		}
		timestamp = time.Unix(timestampInt, 0)
	} else {
		return scrobble{}, fmt.Errorf("timestamp not found in row: %s", row)
	}

	return scrobble{
		artist:          artist,
		track:           track,
		timestamp:       timestamp,
		timestampString: timestampStr,
	}, nil
}

func getTrackDurationsFromMusicBrainz(ctx context.Context, c *Config, query string, queryHash []byte) (time.Duration, error) {
	duration := -1

	resp, err := c.mb.SearchRecording(query, -1, -1)
	if err != nil {
		return 0, fmt.Errorf("failed to search MusicBrainz: %w", err)
	}

	if len(resp.Recordings) == 0 {
		slog.Warn("No MusicBrainz recordings found for query", "query", query)
	} else {
		if len(resp.Recordings) > 1 {
			slog.Debug("Multiple MusicBrainz recordings found, using the first one", "query", query, "count", len(resp.Recordings))
			for i, rec := range resp.Recordings {
				slog.Debug("Recording", "index", i, "artist", rec.ArtistCredit.NameCredits, "track", rec.Title, "duration", rec.Length)
			}
		}
		duration = resp.Recordings[0].Length
	}

	err = c.cache.Set(ctx, fmt.Sprintf("mbquery:%x", queryHash), fmt.Sprintf("%d", duration))
	if err != nil {
		slog.Error("Failed to cache track duration", "error", err)
	}
	return time.Duration(duration) * time.Millisecond, nil
}

func deleteScrobble(c *Config, timestamp string, delete bool) error {
	timeoutCtx, cancel := context.WithTimeout(c.taskCtx, browserOperationsTimeout)
	defer cancel()

	xpathPrefix := `//input[@value='` + timestamp + `']`

	slog.Debug("Attempting to delete scrobble", "timestamp", timestamp)
	err := chromedp.Run(timeoutCtx,
		// Click away to close any previous popup
		chromedp.MouseClickXY(0, 0),
		chromedp.Click(xpathPrefix+`/../../../../button`, chromedp.BySearch),
		chromedp.WaitVisible(`//tr[contains(@class,'show-focus-controls')]`, chromedp.BySearch),
		chromedp.Click(xpathPrefix+`/../../../../button`, chromedp.BySearch),
		chromedp.WaitVisible(xpathPrefix+`/../button`, chromedp.BySearch),
	)
	if err != nil {
		return fmt.Errorf("failed to hover or find delete button: %w", err)
	}

	if !delete {
		slog.Info("Scrobble deletion skipped")
		return nil
	}
	err = chromedp.Run(timeoutCtx,
		chromedp.Click(xpathPrefix+`/../button`, chromedp.BySearch),
	)
	if err != nil {
		return fmt.Errorf("failed to click delete button: %w", err)
	}

	return nil
}

func logStats(c *Config) {
	slog.Info(fmt.Sprintf("MusicBrainz API cache hits: %d", c.runStats.cacheHits))
	slog.Info(fmt.Sprintf("MusicBrainz API cache misses: %d", c.runStats.cacheMisses))
	slog.Info(fmt.Sprintf("Scrobbles processed: %d", c.runStats.processedScrobbles))
	slog.Info(fmt.Sprintf("Scrobbles deleted (if delete enabled): %d", len(c.runStats.deletedScrobbles)))
	slog.Info(fmt.Sprintf("Unknown duration track count: %d", c.runStats.unknownTrackDurationsCount))
	slog.Info(fmt.Sprintf("Elapsed time: %s", c.runStats.elapsedTime.Truncate(time.Millisecond/10)))

	slog.Info("Deleted scrobbles:")
	for _, s := range c.runStats.deletedScrobbles {
		fmt.Printf("Artist: %s - Track: %s - Scrobble time: %s\n", s.artist, s.track, s.timestamp)
	}
}

func writeUnknownTrackDurations(unknownTrackDurations map[string]map[string]string) error {
	userTrackDurations, err := getUserTrackDurations()
	if err != nil {
		return err
	}

	for artist, tracks := range userTrackDurations {
		if unknownTrackDurations[artist] == nil {
			unknownTrackDurations[artist] = make(map[string]string)
		}
		maps.Copy(unknownTrackDurations[artist], tracks)
	}

	bytes, err := yaml.Marshal(unknownTrackDurations)
	if err != nil {
		return fmt.Errorf("failed to marshal unknown track durations to YAML: %w", err)
	}

	bytes = append([]byte("# This file lists tracks that the program could not find a duration for using the MusicBrainz API\n# If a track has an unknown duration, this program will never delete its duplicate scrobbles\n# Specify the duration of each track using the Go time ParseDuration format (ex: 5m06s), then rerun the program\n# You may use it to override a track length, but you must strictly match the scrobble's artist and track name\n\n"), bytes...)

	err = os.WriteFile(customTrackDurationsFile, bytes, 0666)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			slog.Warn(fmt.Sprintf("Failed to save unknown track durations in %s", customTrackDurationsFile), "error", err)
			slog.Info(fmt.Sprintf(`Save the following YAML in a file named "%s" in this program's directory and follow the instructions`, customTrackDurationsFile))
			fmt.Println("\n" + string(bytes))
			return nil
		}
		return fmt.Errorf("failed to save unknown track durations file: %w", err)
	}
	return nil
}

func getStartingPage(c *Config) (int, error) {
	timeoutCtx, cancel := context.WithTimeout(c.taskCtx, browserOperationsTimeout)
	defer cancel()

	var pageNumbers []string
	err := chromedp.Run(timeoutCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			if c.StartPage != 0 {
				err := chromedp.Navigate("https://www.last.fm/user/" + c.LastFMUsername + "/library").Do(ctx)
				if err != nil {
					return err
				}
			}
			if !c.StartDay.IsZero() && !c.EndDay.IsZero() {
				startDayExpr, endDayExpr := c.StartDay.Format("2006-01-02"), c.EndDay.Format("2006-01-02")
				err := chromedp.Navigate(fmt.Sprintf("https://www.last.fm/user/%s/library?from=%s&to=%s", c.LastFMUsername, startDayExpr, endDayExpr)).Do(ctx)
				if err != nil {
					return err
				}
			}
			err := chromedp.WaitVisible(`//h1[@class='header-title']/a`, chromedp.BySearch).Do(ctx)
			if err != nil {
				return err
			}
			err = chromedp.Evaluate(`[...document.querySelectorAll('.pagination-page')].map((e) => e.innerText)`, &pageNumbers).Do(ctx)
			if err != nil {
				return err
			}

			return nil
		}),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve total pages: %w", err)
	}
	if len(pageNumbers) == 0 {
		return 0, errors.New("no pagination found on the page")
	}
	totalPages, err := strconv.Atoi(strings.TrimSpace(strings.Split(pageNumbers[len(pageNumbers)-1], " ")[0]))
	if err != nil {
		return 0, fmt.Errorf("failed to convert total pages number: %w", err)
	}

	slog.Info("Total pages found", "pages", totalPages)

	startingPage := totalPages
	if c.StartPage != 0 {
		if c.StartPage > totalPages {
			return 0, fmt.Errorf("start page %d exceeds total pages %d", c.StartPage, totalPages)
		}
		slog.Info("Starting from page", "page", c.StartPage)
		startingPage = c.StartPage
	}

	slog.Info("Starting on page", "currentPage", startingPage)

	return startingPage, nil
}

func getUserTrackDurations() (map[string]map[string]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current working directory: %w", err)
	}
	customTrackDurationsBytes, err := os.ReadFile(path.Join(cwd, customTrackDurationsFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var userTrackDurations map[string]map[string]string
	if len(customTrackDurationsBytes) > 0 {
		err = yaml.Unmarshal(customTrackDurationsBytes, &userTrackDurations)
		if err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
	}
	return userTrackDurations, nil
}

func getScrobbles(c *Config, currentPage int) ([]scrobble, error) {
	timeoutCtx, timeoutCancel := context.WithTimeout(c.taskCtx, browserOperationsTimeout)
	defer timeoutCancel()

	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate("https://www.last.fm/user/"+c.LastFMUsername+"/library?page="+strconv.Itoa(currentPage)),
		chromedp.WaitVisible(`.top-bar`, chromedp.ByQuery),
		// Remove the top bar to avoid clicking on it by accident when deleting scrobbles
		chromedp.Evaluate("let node1 = document.querySelector('.top-bar'); node1.parentNode.removeChild(node1)", nil),
		chromedp.Evaluate("let node2 = document.querySelector('.masthead'); node2.parentNode.removeChild(node2)", nil),
	)
	if err != nil {
		slog.Error("Failed to navigate to page", "page", currentPage, "error", err)
	}

	var scrobbleRows []string
	err = chromedp.Run(timeoutCtx,
		chromedp.Evaluate(`[...document.querySelectorAll('.chartlist-row')].map((e) => e.innerHTML)`, &scrobbleRows),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve scrobble rows: %w", err)
	}

	scrobbles := []scrobble{}

	slog.Info("Scrobbles found on page", "count", len(scrobbleRows))
	for _, row := range scrobbleRows {
		scrobble, err := generateScrobble(row)
		if err != nil {
			slog.Error("Failed to generate scrobble", "error", err)
			continue
		}
		slog.Debug("Generated scrobble", "artist", scrobble.artist, "track", scrobble.track, "timestamp", scrobble.timestamp)
		scrobbles = append(scrobbles, scrobble)
	}

	slices.Reverse(scrobbles)
	return scrobbles, nil
}

func checkConfig(c *Config) error {
	slog.Debug("Validating config", "config", *c)

	if c.CacheType == "redis" && c.RedisURL == "" {
		return errors.New("must set redis-url if cache-type is redis")
	}

	if c.StartPage != 0 && (!c.StartDay.IsZero() || !c.EndDay.IsZero()) {
		return errors.New("start-page and start-day / end-day must not be set at the same time")
	}

	if !c.StartDay.IsZero() && !c.EndDay.IsZero() && c.StartDay.After(c.EndDay) {
		return errors.New("end-day must be after start-day")
	}

	return nil
}
