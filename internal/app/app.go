package app

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
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
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/cterence/scrobble-deduplicator/internal/cache"
	"github.com/cterence/scrobble-deduplicator/internal/helpers"
	"github.com/goccy/go-yaml"
)

type scrobble struct {
	artist          string
	track           string
	timestamp       time.Time
	timestampString string
	duration        time.Duration
}

type durationByTrackByArtist map[string]map[string]string

const (
	customTrackDurationsFile = "track-durations.yaml"
	browserOperationsTimeout = 30 * time.Second
	InputDayFormat           = "02-01-2006"
	LastFMQueryDayFormat     = "2006-01-02"
)

func clickConsentBanner(ctx context.Context) error {
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 15*time.Second)
	defer timeoutCancel()

	cookies, err := getCookies(ctx)
	if err != nil {
		return fmt.Errorf("failed to get cookies: %w", err)
	}
	slog.Debug("Got cookies", "cookieCount", len(cookies))

	consentBoxCookieIndex := slices.IndexFunc(cookies, func(cookie *network.Cookie) bool {
		return cookie.Name == "OptanonAlertBoxClosed"
	})

	cookieExpired := false
	if consentBoxCookieIndex != -1 {
		slog.Debug("Consent cookie found")
		consentBoxCookie := cookies[consentBoxCookieIndex]
		cookieExpiry := cdp.TimeSinceEpoch(time.Unix(int64(consentBoxCookie.Expires), 0))
		if cookieExpiry.Time().Before(time.Now()) {
			slog.Info("Consent cookie expired, clicking on banner")
			cookieExpired = true
		}
	}

	if consentBoxCookieIndex == -1 || cookieExpired {
		err = chromedp.Run(timeoutCtx,
			chromedp.WaitVisible(`#onetrust-reject-all-handler`, chromedp.ByID),
			chromedp.Sleep(1*time.Second), // Cookie banner takes a while to come up, we don't want to miss the click
			chromedp.Click(`#onetrust-reject-all-handler`, chromedp.ByID),
			chromedp.Sleep(500*time.Millisecond), // Wait for cookie banner to disappear
		)
		if err != nil {
			if strings.Contains(err.Error(), "context deadline exceeded") {
				slog.Warn("Failed to click on cookie banner, ignoring")
				return nil
			}
			return err
		}
		slog.Debug("Clicked on cookie banner")
	} else {
		slog.Debug("Skipped clicking on cookie banner")
	}
	return nil
}

func getStartPage(c *Config) (int, error) {
	timeoutCtx, cancel := context.WithTimeout(c.taskCtx, browserOperationsTimeout)
	defer cancel()

	var (
		pageNumbers   []string
		scrobbleCount int
	)
	noScrobbles := false
	err := chromedp.Run(timeoutCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			err := chromedp.Navigate("https://www.last.fm/user/" + c.LastFMUsername + "/library").Do(ctx)
			if err != nil {
				return fmt.Errorf("failed to navigate to user library: %w", err)
			}

			if c.noLogin {
				err := clickConsentBanner(ctx)
				if err != nil {
					return fmt.Errorf("failed to click on consent banner: %w", err)
				}
			}

			if !c.From.IsZero() {
				fromExpr := c.From.Format(LastFMQueryDayFormat)
				query := fmt.Sprintf("https://www.last.fm/user/%s/library?from=%s", c.LastFMUsername, fromExpr)
				if !c.To.IsZero() {
					toExpr := c.To.Format(LastFMQueryDayFormat)
					query += fmt.Sprintf("&to=%s", toExpr)
				}
				err := chromedp.Navigate(query).Do(ctx)
				if err != nil {
					return fmt.Errorf("failed to navigate to user library with from / to dates: %w", err)
				}
			}

			err = chromedp.WaitVisible(`//h1[@class='content-top-header']`, chromedp.BySearch).Do(ctx)
			if err != nil {
				return fmt.Errorf("failed to wait for h1 with content-top-header class: %w", err)
			}

			var noDataNodes []*cdp.Node
			err = chromedp.Nodes(`//p[@class='no-data-message']`, &noDataNodes, chromedp.AtLeast(0)).Do(ctx)
			if err != nil {
				return fmt.Errorf("failed to get no-data-message p element: %w", err)
			}
			// Early return if we find no scrobble
			if len(noDataNodes) == 1 {
				noScrobbles = true
				return nil
			}

			var scrobbleCountStr string
			err = chromedp.Text(`//h2[@class='metadata-title' and text()='Scrobbles']/../p`, &scrobbleCountStr, chromedp.BySearch).Do(ctx)
			if err != nil {
				return fmt.Errorf("failed to get scrobble count: %w", err)
			}

			scrobbleCountStr = strings.ReplaceAll(scrobbleCountStr, ",", "")
			scrobbleCount, err = strconv.Atoi(scrobbleCountStr)
			if err != nil {
				return fmt.Errorf("failed to convert scrobble count to int: %w", err)
			}

			slog.Info("Scrobbles to process", "count", scrobbleCountStr)

			if scrobbleCount > 50 {
				err = chromedp.Evaluate(`[...document.querySelectorAll('.pagination-page')].map((e) => e.innerText)`, &pageNumbers).Do(ctx)
				if err != nil {
					return fmt.Errorf("failed to get page numbers: %w", err)
				}
			}

			return nil
		}),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve total pages: %w", err)
	}
	if noScrobbles {
		return 0, ErrNoScrobbles
	}
	if len(pageNumbers) == 0 {
		// There is only one page with less than 50 scrobbles
		if scrobbleCount > 0 {
			return 1, nil
		}
		return 0, errors.New("no pagination found on the page")
	}
	totalPages, err := strconv.Atoi(strings.TrimSpace(strings.Split(pageNumbers[len(pageNumbers)-1], " ")[0]))
	if err != nil {
		return 0, fmt.Errorf("failed to convert total pages number: %w", err)
	}

	slog.Info("Total pages found", "pages", totalPages)

	startPage := totalPages
	if c.StartPage != 0 {
		if c.StartPage > totalPages {
			return 0, fmt.Errorf("start page %d exceeds total pages %d", c.StartPage, totalPages)
		}
		slog.Info("Starting from page", "page", c.StartPage)
		startPage = c.StartPage
	}

	slog.Info("Starting on page", "currentPage", startPage)

	return startPage, nil
}

func getUserTrackDurations() (durationByTrackByArtist, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current working directory: %w", err)
	}
	customTrackDurationsBytes, err := os.ReadFile(path.Join(cwd, customTrackDurationsFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var userTrackDurations durationByTrackByArtist
	if len(customTrackDurationsBytes) > 0 {
		err = yaml.Unmarshal(customTrackDurationsBytes, &userTrackDurations)
		if err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
	}
	return userTrackDurations, nil
}

var ErrNoScrobbles = errors.New("no scrobbles found for the selected period")

func getScrobbles(c *Config, currentPage int) ([]scrobble, error) {
	timeoutCtx, timeoutCancel := context.WithTimeout(c.taskCtx, browserOperationsTimeout)
	defer timeoutCancel()

	libraryPageQuery := fmt.Sprintf("https://www.last.fm/user/%s/library?page=%s", c.LastFMUsername, strconv.Itoa(currentPage))
	if !c.From.IsZero() && !c.To.IsZero() {
		libraryPageQuery += fmt.Sprintf("&from=%s&to=%s", c.From.Format(LastFMQueryDayFormat), c.To.Format(LastFMQueryDayFormat))
	}

	slog.Debug("get scrobble library page", "query", libraryPageQuery)

	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(libraryPageQuery),
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

var ErrUnknownTrackAlreadyInMap = errors.New("no duration found in cache or MusicBrainz API, track already saved in unknown track durations")

func getTrackDuration(ctx context.Context, c *Config, userTrackDurations durationByTrackByArtist, s *scrobble) error {
	// Check if track is in userTrackDurations
	if userTrackDurations != nil && userTrackDurations[s.artist] != nil && userTrackDurations[s.artist][s.track] != "" {
		// Convert to duration with 4m0s format
		trackDuration, err := time.ParseDuration(userTrackDurations[s.artist][s.track])
		if err != nil {
			slog.Error("Failed to parse user duration", "artist", s.artist, "track", s.track, "error", err)
		}
		s.duration = trackDuration
		slog.Debug("Found track duration in user track durations", "artist", s.artist, "track", s.track, "duration", s.duration)

		return nil
	}

	query := fmt.Sprintf(`artist:"%s" AND recording:"%s"`, s.artist, s.track)
	// Hash the query
	queryHasher := sha256.New()
	queryHasher.Write([]byte(query))
	queryHash := queryHasher.Sum(nil)

	cacheGetStartTime := time.Now()
	val, err := c.cache.Get(ctx, fmt.Sprintf("mbquery:%x", queryHash))
	slog.Debug("Cache get", "took", time.Since(cacheGetStartTime))

	if err != nil {
		if errors.Is(err, cache.ErrCacheMiss) {
			c.runStats.cacheMisses++
			slog.Debug("Cache miss for track duration query", "artist", s.artist, "track", s.track)
			trackDurations, err := backoff.Retry(ctx, func() (time.Duration, error) {
				return getTrackDurationsFromMusicBrainz(ctx, c, query, queryHash)
			}, backoff.WithBackOff(backoff.NewExponentialBackOff()), backoff.WithMaxTries(10))
			if err != nil {
				return fmt.Errorf("failed to get track duration from MusicBrainz API: %w", err)
			}
			// TODO: check if track duration is 0s, and go directly to unknown tracks (create a function for that)
			s.duration = trackDurations
			slog.Debug("Found track duration from MusicBrainz API", "artist", s.artist, "track", s.track, "duration", s.duration)
			return nil
		}
		return fmt.Errorf("failed to get cached track duration: %w", err)
	}

	c.runStats.cacheHits++
	cachedTrackDurations, err := strconv.Atoi(val)
	if err != nil {
		return fmt.Errorf("failed to parse cached duration: %w", err)
	}
	// TODO: check if track duration is 0s, delete it from cache, and go directly to unknown tracks (create a function for that)
	s.duration = time.Duration(cachedTrackDurations) * time.Millisecond
	slog.Debug("Cache hit for track duration query", "artist", s.artist, "track", s.track, "duration", s.duration)

	if s.duration < 0 {
		if c.unknownTrackDurations[s.artist] == nil {
			c.unknownTrackDurations[s.artist] = make(map[string]string)
		}
		if _, found := c.unknownTrackDurations[s.artist][s.track]; !found {
			c.unknownTrackDurations[s.artist][s.track] = ""
			c.runStats.unknownTrackDurationsCount++
		} else {
			return ErrUnknownTrackAlreadyInMap
		}
		return fmt.Errorf("no duration found in cache or MusicBrainz API for track %s - %s, saving to unknown track durations", s.artist, s.track)
	}

	return nil
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

	cacheSetStartTime := time.Now()
	err = c.cache.Set(ctx, fmt.Sprintf("mbquery:%x", queryHash), fmt.Sprintf("%d", duration))
	slog.Debug("Cache set", "took", time.Since(cacheSetStartTime))
	if err != nil {
		slog.Error("Failed to cache track duration", "error", err)
	}
	return time.Duration(duration) * time.Millisecond, nil
}

func processScrobblesFromStartToEndPage(ctx context.Context, c *Config, startPage int, endPage int, userTrackDurations durationByTrackByArtist) error {

	for currentPage := startPage; currentPage >= endPage; currentPage-- {
		slog.Info("Processing page", "page", currentPage)
		scrobbles, err := backoff.Retry(ctx, func() ([]scrobble, error) {
			return getScrobbles(c, currentPage)
		}, backoff.WithMaxTries(3))
		if err != nil {
			return err
		}

		var previousScrobble *scrobble
		for _, s := range scrobbles {
			previousScrobble = processPreviousAndCurrentScrobbles(ctx, c, s, previousScrobble, userTrackDurations)
		}
	}
	return nil
}

func processPreviousAndCurrentScrobbles(ctx context.Context, c *Config, s scrobble, previousScrobble *scrobble, userTrackDurations durationByTrackByArtist) *scrobble {
	err := getTrackDuration(ctx, c, userTrackDurations, &s)
	if err != nil {
		if !errors.Is(err, ErrUnknownTrackAlreadyInMap) {
			slog.Warn("failed to get track duration, skipping scrobble", "error", err)
		}
		c.runStats.skippedScrobbleUnknownDuration++
		return previousScrobble
	}
	slog.Debug("Track duration found", "artist", s.artist, "track", s.track, "duration", s.duration)

	lastScrobbleDeleted := false
	if previousScrobble != nil {
		// Check if the current scrobble is a duplicate of the previous one
		lastScrobbleDeleted, err = detectAndDeleteDuplicateScrobble(ctx, c, previousScrobble, s)
		if err != nil {
			c.runStats.scrobbleDeleteFails++
			slog.Warn("failed to detect and delete duplicated scrobble", "error", err)
		}
	}
	if !lastScrobbleDeleted || previousScrobble == nil {
		previousScrobble = &s
	}
	c.runStats.processedScrobbles++

	return previousScrobble
}

func detectAndDeleteDuplicateScrobble(ctx context.Context, c *Config, previousScrobble *scrobble, currentScrobble scrobble) (bool, error) {
	// TODO: detect if 2 scrobbles are too close apart meaning that the first one was scrobbled too quickly
	lastScrobbleDeleted := false
	if currentScrobble.artist == previousScrobble.artist && currentScrobble.track == previousScrobble.track && currentScrobble.timestamp != previousScrobble.timestamp {
		timeBetweenScrobbleStarts := currentScrobble.timestamp.Sub(previousScrobble.timestamp)
		duplicateDurationThreshold := time.Duration(float64(currentScrobble.duration) * float64(c.DuplicateThreshold) / 100.0)
		isDuplicate := timeBetweenScrobbleStarts < duplicateDurationThreshold

		slog.Debug("duplication calculations", "previousScrobbleTimestamp", previousScrobble.timestamp, "currentScrobbleTimestamp", currentScrobble.timestamp, "timeBetweenScrobbleStarts", timeBetweenScrobbleStarts, "duplicateThreshold", c.DuplicateThreshold, "duplicateDurationThreshold", duplicateDurationThreshold, "isDuplicate", isDuplicate)
		if isDuplicate {
			slog.Info("🎯 Duplicate scrobble detected!", "artist", currentScrobble.artist, "track", currentScrobble.track, "timestamp", currentScrobble.timestamp)
			c.deletedScrobbles = append(c.deletedScrobbles, currentScrobble)
			_, err := backoff.Retry(ctx, func() (struct{}, error) {
				return struct{}{}, deleteScrobble(c, currentScrobble.timestampString, c.Delete)
			}, backoff.WithMaxTries(2))
			if err != nil {
				return false, fmt.Errorf("failed to delete duplicate scrobble: %w", err)
			}
			if c.Delete {
				lastScrobbleDeleted = true
				slog.Info("Scrobble deleted", "artist", currentScrobble.artist, "track", currentScrobble.track, "timestamp", currentScrobble.timestamp)
			}
		}
	}
	return lastScrobbleDeleted, nil
}

func deleteScrobble(c *Config, timestamp string, delete bool) error {
	timeoutCtx, cancel := context.WithTimeout(c.taskCtx, 3*time.Second)
	defer cancel()

	// Sometimes multiple scrobble have an identical timestamp
	// We always take the last scrobble because it would be the one next to the previous scrobble
	xpathPrefix := `(//input[@value='` + timestamp + `'])[last()]`

	slog.Debug("Attempting to delete scrobble", "timestamp", timestamp, "xpath", xpathPrefix)
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
	c.runStats.elapsedTime = time.Since(c.startTime)

	slog.Info("Run statistics:")
	slog.Info(fmt.Sprintf("MusicBrainz API cache hits: %d", c.runStats.cacheHits))
	slog.Info(fmt.Sprintf("MusicBrainz API cache misses: %d", c.runStats.cacheMisses))
	slog.Info(fmt.Sprintf("Scrobbles processed: %d", c.runStats.processedScrobbles))
	slog.Info(fmt.Sprintf("Scrobbles deleted (if delete enabled): %d", len(c.deletedScrobbles)))
	slog.Info(fmt.Sprintf("Unknown duration track count: %d", c.runStats.unknownTrackDurationsCount))
	slog.Info(fmt.Sprintf("Scrobbles skipped due to unknown track duration: %d", c.runStats.skippedScrobbleUnknownDuration))
	slog.Info(fmt.Sprintf("Scrobbles not deleted due to error: %d", c.runStats.scrobbleDeleteFails))
	slog.Info(fmt.Sprintf("Elapsed time: %s", c.runStats.elapsedTime.Truncate(time.Millisecond/10)))
}

func writeUnknownTrackDurations(unknownTrackDurations durationByTrackByArtist) error {
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

func exportScrobblesToCSV(baseFilename string, startTime time.Time, delete bool, scrobbles []scrobble) {
	timestamp := startTime.Format("20060102-150405")
	filename := fmt.Sprintf("%s-%s.csv", baseFilename, timestamp)

	slices.SortFunc(scrobbles, func(s1, s2 scrobble) int {
		return s1.timestamp.Compare(s2.timestamp)
	})

	file, err := os.Create(filename)
	if err != nil {
		slog.Warn("⚠️ Could not create deleted scrobble file, falling back to logging scrobbles as CSV", "file", filename, "error", err)
		logScrobblesCSV(scrobbles)
		return
	}
	defer helpers.CloseFile(file)

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// header
	_ = writer.Write([]string{"Artist", "Track", "Timestamp", "TimestampString"})

	for _, s := range scrobbles {
		record := []string{
			s.artist,
			s.track,
			s.timestamp.Format(time.RFC3339),
			s.timestampString,
		}
		_ = writer.Write(record)
	}

	if delete {
		slog.Info("Deleted scrobbles saved to file", "filename", filename)
	} else {
		slog.Info("Would-be deleted scrobbles saved to file", "filename", filename)
	}
}

func logScrobblesCSV(scrobbles []scrobble) {
	var sb strings.Builder

	// header
	sb.WriteString("Artist,Track,Timestamp,TimestampString\n")

	for _, s := range scrobbles {
		sb.WriteString(fmt.Sprintf("%s,%s,%s,%s\n",
			s.artist,
			s.track,
			s.timestamp.Format(time.RFC3339),
			s.timestampString,
		))
	}

	fmt.Printf("Scrobbles CSV:\n%s", sb.String())
}

func finishRun(c *Config) error {
	defer c.close()
	logStats(c)

	if len(c.unknownTrackDurations) > 0 {
		err := writeUnknownTrackDurations(c.unknownTrackDurations)
		if err != nil {
			return err
		}
	}

	if len(c.deletedScrobbles) > 0 {
		exportScrobblesToCSV("deleted-scrobbles", c.startTime, c.Delete, c.deletedScrobbles)
	}

	return nil
}
