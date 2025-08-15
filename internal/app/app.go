package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
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
	NoHeadless     bool
	RedisURL       string
	BrowserURL     string
	LogLevel       string

	// Internal dependencies
	log       *slog.Logger
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
	cacheHits              int
	cacheMisses            int
	processedScrobbles     int
	deletedScrobbles       []scrobble
	unknownLengthSongCount int
	elapsedTime            time.Duration
}

type scrobble struct {
	artist          string
	track           string
	timestamp       time.Time
	timestampString string
	length          time.Duration
}

const customSongLengthFileName = "customsonglengths.yaml"

func Run(ctx context.Context, c *Config) error {
	err := initApp(ctx, c)
	defer c.allocCancel()
	defer c.taskCancel()

	err = login(c.taskCtx, c)
	if err != nil {
		return fmt.Errorf("failed to login to Last.fm: %w", err)
	}

	startingPage, err := getStartingPage(c)
	if err != nil {
		return fmt.Errorf("failed to get starting page: %v", err)
	}

	customSongLengths, err := getCustomSongLengths()
	unknownSongLengths := make(map[string]map[string]string, 0)

	var previousScrobble *scrobble
	var lastScrobbleDeleted bool

	for currentPage := startingPage; currentPage > 0; currentPage-- {
		c.log.Info("Processing page", "page", currentPage)
		scrobbles, err := getScrobbles(c, currentPage)
		if err != nil {
			return err
		}

		for _, s := range scrobbles {
			if customSongLengths != nil && customSongLengths[s.artist] != nil && customSongLengths[s.artist][s.track] != "" {
				// Convert to duration with 4m0s format
				songLength, err := time.ParseDuration(customSongLengths[s.artist][s.track])
				if err != nil {
					c.log.Error("Failed to parse custom length", "artist", s.artist, "track", s.track, "error", err)
				}
				s.length = songLength
				c.log.Debug("Found song length in custom song lengths", "artist", s.artist, "track", s.track, "length", s.length)
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
						songLength, err := backoff.Retry(ctx, func() (time.Duration, error) {
							return getSongLengthFromMusicBrainz(ctx, c, query, queryHash)
						}, backoff.WithBackOff(backoff.NewExponentialBackOff()), backoff.WithMaxTries(10))
						if err != nil {
							c.log.Error("failed to get song length from MusicBrainz API", "error", err)
							continue
						}
						s.length = songLength
						c.log.Debug("Found song length from MusicBrainz API", "artist", s.artist, "track", s.track, "length", s.length)
					} else {
						c.log.Error("Failed to get cached song length", "query", query, "error", err)
						continue
					}
				} else {
					c.runStats.cacheHits++
					cachedSongLength, err := strconv.Atoi(val)
					if err != nil {
						c.log.Error("Failed to parse cached length", "query", query, "error", err)
						continue
					}
					s.length = time.Duration(cachedSongLength) * time.Millisecond
					c.log.Debug("Cache hit for song length query", "artist", s.artist, "track", s.track, "length", s.length)
				}
				if s.length < 0 {
					if unknownSongLengths[s.artist] == nil {
						unknownSongLengths[s.artist] = make(map[string]string)
					}
					if _, found := unknownSongLengths[s.artist][s.track]; !found {
						unknownSongLengths[s.artist][s.track] = ""
						c.runStats.unknownLengthSongCount++
						c.log.Warn("No song length found for query, saved to unknown song lengths", "query", query, "artist", s.artist, "track", s.track)
					}
					continue
				}
			}

			lastScrobbleDeleted = false
			if previousScrobble != nil {
				// Check if the current scrobble is a duplicate of the previous one
				if s.artist == previousScrobble.artist && s.track == previousScrobble.track {
					timeDiff := s.timestamp.Sub(previousScrobble.timestamp)
					if timeDiff < s.length {
						c.log.Info("🎯 Duplicate scrobble detected!", "artist", s.artist, "track", s.track, "timestamp", s.timestamp)
						c.runStats.deletedScrobbles = append(c.runStats.deletedScrobbles, s)
						lastScrobbleDeleted = true
						err = deleteScrobble(c, s.timestampString, c.Delete)
						if err != nil {
							c.log.Error("Failed to delete duplicate scrobble", "artist", s.artist, "track", s.track, "timestamp", s.timestamp, "error", err)
						}
						if c.Delete {
							c.log.Info("Scrobble deleted", "artist", s.artist, "track", s.track, "timestamp", s.timestamp)
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

	c.log.Info("Completed!")

	c.runStats.elapsedTime = time.Since(c.startTime)
	logStats(c)

	err = writeUnknownSongLengths(unknownSongLengths)
	if err != nil {
		return err
	}

	return nil
}

func generateScrobble(row string) (scrobble, error) {
	// Execute xpath on the row
	artist, track, timestamp, timestampStr := "", "", time.Time{}, ""

	doc, err := htmlquery.Parse(strings.NewReader(row))
	if err != nil {
		return scrobble{}, fmt.Errorf("failed to parse row HTML: %w", err)
	}

	artistNode := htmlquery.FindOne(doc, `.//input[@name='artist_name']`)
	if artistNode != nil {
		artist = strings.TrimSpace(htmlquery.SelectAttr(artistNode, "value"))
	} else {
		return scrobble{}, fmt.Errorf("artist not found in row: %v", row)
	}

	trackNode := htmlquery.FindOne(doc, `.//input[@name='track_name']`)
	if trackNode != nil {
		track = strings.TrimSpace(htmlquery.SelectAttr(trackNode, "value"))
	} else {
		return scrobble{}, fmt.Errorf("track not found in row: %v", row)
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
		return scrobble{}, fmt.Errorf("timestamp not found in row: %v", row)
	}

	return scrobble{
		artist:          artist,
		track:           track,
		timestamp:       timestamp,
		timestampString: timestampStr,
	}, nil
}

func getSongLengthFromMusicBrainz(ctx context.Context, c *Config, query string, queryHash []byte) (time.Duration, error) {
	length := -1

	resp, err := c.mb.SearchRecording(query, -1, -1)
	if err != nil {
		return 0, fmt.Errorf("failed to search MusicBrainz: %w", err)
	}

	if len(resp.Recordings) == 0 {
		c.log.Warn("No MusicBrainz recordings found for query", "query", query)
	} else {
		if len(resp.Recordings) > 1 {
			c.log.Info("Multiple MusicBrainz recordings found, using the first one", "query", query, "count", len(resp.Recordings))
			for i, rec := range resp.Recordings {
				c.log.Debug("Recording", "index", i, "artist", rec.ArtistCredit.NameCredits, "track", rec.Title, "length", rec.Length)
			}
		}
		length = resp.Recordings[0].Length
	}

	c.cache.Set(ctx, fmt.Sprintf("mbquery:%x", queryHash), fmt.Sprintf("%d", length))
	time.Sleep(200 * time.Millisecond) // Sleep to avoid hitting the API too fast
	return time.Duration(length) * time.Millisecond, nil
}

func deleteScrobble(c *Config, timestamp string, delete bool) error {
	timeoutCtx, cancel := context.WithTimeout(c.taskCtx, 5*time.Second)
	defer cancel()

	xpathPrefix := `//input[@value='` + timestamp + `']`

	// FIXME: first click is flaky
	err := chromedp.Run(timeoutCtx,
		chromedp.Click(xpathPrefix+`/../../../../button`, chromedp.BySearch),
		chromedp.Sleep(200*time.Millisecond), // Wait for the hover effect to take place
		chromedp.Click(xpathPrefix+`/../../../../button`, chromedp.BySearch),
		chromedp.WaitVisible(xpathPrefix+`/../button`, chromedp.BySearch),
	)
	if err != nil {
		return fmt.Errorf("failed to hover or find delete button: %w", err)
	}

	if !delete {
		c.log.Info("Scrobble deletion skipped")
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
	c.log.Info(fmt.Sprintf("MusicBrainz API cache hits: %d", c.runStats.cacheHits))
	c.log.Info(fmt.Sprintf("MusicBrainz API cache misses: %d", c.runStats.cacheMisses))
	c.log.Info(fmt.Sprintf("Scrobbles processed: %d", c.runStats.processedScrobbles))
	c.log.Info(fmt.Sprintf("Scrobbles deleted (if delete enabled): %d", len(c.runStats.deletedScrobbles)))
	c.log.Info(fmt.Sprintf("Unknown length song count: %d", c.runStats.unknownLengthSongCount))
	c.log.Info(fmt.Sprintf("Elapsed time: %s", c.runStats.elapsedTime.Truncate(time.Millisecond/10)))

	c.log.Info("Deleted scrobbles:")
	for _, s := range c.runStats.deletedScrobbles {
		c.log.Info(fmt.Sprintf("Artist: %s - Song: %s - Scrobble time: %s", s.artist, s.track, s.timestamp))
	}
}

func writeUnknownSongLengths(unknownSongLengths map[string]map[string]string) error {
	customSongLengths, err := getCustomSongLengths()
	if err != nil {
		return err
	}

	for artist, tracks := range customSongLengths {
		if unknownSongLengths[artist] == nil {
			unknownSongLengths[artist] = make(map[string]string)
		}
		maps.Copy(unknownSongLengths[artist], tracks)
	}

	bytes, err := yaml.Marshal(unknownSongLengths)
	if err != nil {
		return fmt.Errorf("failed to marshal unknown song lengths to YAML: %v", err)
	}

	err = os.WriteFile(customSongLengthFileName, bytes, 0666)
	if err != nil {
		return fmt.Errorf("failed to save unknown song lengths file: %v", err)
	}
	return nil
}

func getStartingPage(c *Config) (int, error) {
	timeoutCtx, cancel := context.WithTimeout(c.taskCtx, 10*time.Second)
	defer cancel()

	var pageNumbers []string
	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate("https://www.last.fm/user/"+c.LastFMUsername+"/library"),
		chromedp.WaitVisible(`//h1[@class='header-title']/a`, chromedp.BySearch),
		chromedp.Evaluate(`[...document.querySelectorAll('.pagination-page')].map((e) => e.innerText)`, &pageNumbers),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve total pages: %w", err)
	}
	if len(pageNumbers) == 0 {
		return 0, errors.New("no pagination found on the page")
	}
	totalPages, err := strconv.Atoi(strings.TrimSpace(strings.Split(pageNumbers[len(pageNumbers)-1], " ")[0]))

	c.log.Info("Total pages found", "pages", totalPages)

	startingPage := totalPages
	if c.StartPage != 0 {
		if c.StartPage > totalPages {
			return 0, fmt.Errorf("start page %d exceeds total pages %d", c.StartPage, totalPages)
		}
		c.log.Info("Starting from page", "page", c.StartPage)
		startingPage = c.StartPage
		totalPages = c.StartPage
	}

	c.log.Info("Starting on page", "currentPage", startingPage)

	return startingPage, nil
}

func getCustomSongLengths() (map[string]map[string]string, error) {
	customSongLengthBytes, err := os.ReadFile(customSongLengthFileName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var customSongLengths map[string]map[string]string
	if len(customSongLengthBytes) > 0 {
		err = yaml.Unmarshal(customSongLengthBytes, &customSongLengths)
		if err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
	}
	return customSongLengths, nil
}

func getScrobbles(c *Config, currentPage int) ([]scrobble, error) {
	timeoutCtx, timeoutCancel := context.WithTimeout(c.taskCtx, 15*time.Minute)
	defer timeoutCancel()

	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate("https://www.last.fm/user/"+c.LastFMUsername+"/library?page="+strconv.Itoa(currentPage)),
		chromedp.WaitVisible(`.top-bar`, chromedp.ByQuery),
		// Remove the top bar to avoid clicking on it by accident when deleting scrobbles
		chromedp.Evaluate("let node1 = document.querySelector('.top-bar'); node1.parentNode.removeChild(node1)", nil),
		chromedp.Evaluate("let node2 = document.querySelector('.masthead'); node2.parentNode.removeChild(node2)", nil),
	)
	if err != nil {
		c.log.Error("Failed to navigate to page", "page", currentPage, "error", err)
	}

	var scrobbleRows []string
	err = chromedp.Run(timeoutCtx,
		chromedp.Evaluate(`[...document.querySelectorAll('.chartlist-row')].map((e) => e.innerHTML)`, &scrobbleRows),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve scrobble rows: %w", err)
	}

	scrobbles := []scrobble{}

	c.log.Info("Scrobbles found on page", "count", len(scrobbleRows))
	for _, row := range scrobbleRows {
		scrobble, err := generateScrobble(row)
		if err != nil {
			c.log.Error("Failed to generate scrobble", "error", err)
			continue
		}
		c.log.Debug("Generated scrobble", "artist", scrobble.artist, "track", scrobble.track, "timestamp", scrobble.timestamp)
		scrobbles = append(scrobbles, scrobble)
	}

	slices.Reverse(scrobbles)
	return scrobbles, nil
}
