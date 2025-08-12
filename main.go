package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antchfx/htmlquery"
	"github.com/cenkalti/backoff/v5"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"github.com/michiwend/gomusicbrainz"
	"github.com/redis/go-redis/v9"
)

type Scrobble struct {
	Artist          string
	Track           string
	Timestamp       time.Time
	TimestampString string // For delete
	Length          time.Duration
}

func main() {
	rdb := redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "", // no password set
		DB:       0,  // use default DB
	})

	mb, err := gomusicbrainz.NewWS2Client("https://musicbrainz.org", "lastfm-scrobble-deduplicator", "1.0", "https://github.com/cterence")
	if err != nil {
		slog.Error("Failed to create MusicBrainz client", "error", err)
		os.Exit(1)
	}

	username, password, delete := os.Getenv("LASTFM_USERNAME"), os.Getenv("LASTFM_PASSWORD"), os.Getenv("LASTFM_DELETE") == "true"
	if username == "" || password == "" {
		log.Fatal("Environment variables LASTFM_USERNAME and LASTFM_PASSWORD must be set")
	}

	startPage := 0
	startPageStr := os.Getenv("LASTFM_START_PAGE")
	if startPageStr != "" {
		var err error
		startPage, err = strconv.Atoi(startPageStr)
		if err != nil {
			log.Fatalf("Invalid LASTFM_START_PAGE value: %v", err)
		}
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", os.Getenv("LASTFM_HEADLESS") == "true"),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(
		allocCtx,
		chromedp.WithLogf(log.Printf),
	)
	defer taskCancel()

	slog.Info("Starting ChromeDP...")
	// ensure that the browser process is started
	if err := chromedp.Run(taskCtx); err != nil {
		log.Fatal(err)
	}

	loginURL := "https://www.last.fm/login"
	slog.Info("Navigating to Last.fm login page", "url", loginURL)

	err = chromedp.Run(taskCtx,
		chromedp.Navigate(loginURL),
		chromedp.WaitVisible(`#onetrust-accept-btn-handler`, chromedp.ByID),
		chromedp.Click(`#onetrust-accept-btn-handler`, chromedp.ByID),
		chromedp.WaitVisible(`id_username_or_email`, chromedp.ByID),
		// Wait for a second
		chromedp.Sleep(1*time.Second), // 1 second
		chromedp.SendKeys(`id_username_or_email`, strings.ToLower(username), chromedp.ByID),
		chromedp.SendKeys(`id_password`, password, chromedp.ByID),
		chromedp.Click(`//div[@class='form-submit']/button[@class='btn-primary']`, chromedp.BySearch),
		chromedp.WaitVisible(`//h1[@class='header-title']/a`, chromedp.BySearch),
	)
	if err != nil {
		slog.Error("Failed to login to Last.fm", "error", err)
		os.Exit(1)
	}

	slog.Info("Successfully logged in to Last.fm")

	var pageNumbers []string
	err = chromedp.Run(taskCtx,
		chromedp.Navigate("https://www.last.fm/user/"+username+"/library"),
		chromedp.WaitVisible(`//h1[@class='header-title']/a`, chromedp.BySearch),
		chromedp.Evaluate(`[...document.querySelectorAll('.pagination-page')].map((e) => e.innerText)`, &pageNumbers),
	)
	if err != nil {
		slog.Error("Failed to retrieve page amount", "error", err)
		os.Exit(1)
	}

	if len(pageNumbers) == 0 {
		slog.Error("No pagination found on the page")
		os.Exit(1)
	}
	totalPages, err := strconv.Atoi(strings.TrimSpace(strings.Split(pageNumbers[len(pageNumbers)-1], " ")[0]))

	slog.Info("Total pages found", "pages", totalPages)

	currentPage := totalPages
	if startPage != 0 {
		if startPage > totalPages {
			slog.Error("Start page exceeds total pages", "startPage", startPage, "totalPages", totalPages)
			os.Exit(1)
		}
		slog.Info("Starting from page", "page", startPage)
		currentPage = startPage
		totalPages = startPage
	}

	slog.Info("Starting on page", "currentPage", currentPage)

	var previousScrobble *Scrobble
	var deletedScrobbles int

	for i := currentPage; i > 0; i-- {
		scrobbles := []Scrobble{}
		slog.Info("Processing page", "page", i)
		err = chromedp.Run(taskCtx,
			chromedp.Navigate("https://www.last.fm/user/"+username+"/library?page="+strconv.Itoa(i)),
			chromedp.WaitVisible(`.top-bar`, chromedp.ByQuery),
			// Remove the top bar to avoid clicking on it by accident when deleting scrobbles
			chromedp.Evaluate("let node1 = document.querySelector('.top-bar'); node1.parentNode.removeChild(node1)", nil),
			chromedp.Evaluate("let node2 = document.querySelector('.masthead'); node2.parentNode.removeChild(node2)", nil),
		)
		if err != nil {
			slog.Error("Failed to navigate to page", "page", i, "error", err)
			continue
		}

		var scrobbleRows []string
		var scrobbleNodes []*cdp.Node
		err = chromedp.Run(taskCtx,
			chromedp.Evaluate(`[...document.querySelectorAll('.chartlist-row')].map((e) => e.innerHTML)`, &scrobbleRows),
			chromedp.Nodes(`//div[@class='chartlist-row']`, &scrobbleNodes, chromedp.ByQueryAll),
		)

		slog.Info("Scrobbles found on page", "page", i, "count", len(scrobbleRows))
		for _, row := range scrobbleRows {
			scrobble, err := generateScrobble(row)
			if err != nil {
				slog.Error("Failed to generate scrobble", "error", err)
				continue
			}
			slog.Debug("Generated scrobble", "artist", scrobble.Artist, "track", scrobble.Track, "timestamp", scrobble.Timestamp)
			scrobbles = append(scrobbles, scrobble)
		}

		slices.Reverse(scrobbles)

		for _, s := range scrobbles {
			query := fmt.Sprintf(`artist:"%s" AND recording:"%s"`, s.Artist, s.Track)
			var songLength int

			// Hash the query
			queryHasher := sha256.New()
			queryHasher.Write([]byte(query))
			queryHash := queryHasher.Sum(nil)

			val, err := rdb.Get(context.Background(), fmt.Sprintf("mbquery:%x", queryHash)).Result()
			if err != nil {
				if err == redis.Nil {
					songLength, err = backoff.Retry(context.TODO(), func() (int, error) {
						return getRecordingLength(mb, rdb, query, queryHash)
					}, backoff.WithBackOff(backoff.NewExponentialBackOff()), backoff.WithMaxTries(10))
					if err != nil {
						fmt.Println("Error:", err)
						continue
					}
				} else {
					slog.Error("Failed to get cached length", "query", query, "error", err)
					continue
				}
			} else {
				songLength, err = strconv.Atoi(val)
				if err != nil {
					slog.Error("Failed to parse cached length", "query", query, "error", err)
					continue
				}
				slog.Debug("Cache hit for recording length query", "artist", s.Artist, "track", s.Track, "length", songLength)
			}
			if songLength == -1 {
				slog.Warn("No recording length found for query, skipping", "query", query, "artist", s.Artist, "track", s.Track)
				continue
			}

			s.Length = time.Duration(songLength) * time.Millisecond

			slog.Debug("Found recording length", "artist", s.Artist, "track", s.Track, "timestamp", s.Timestamp, "length", s.Length)

			if previousScrobble != nil {
				// Check if the current scrobble is a duplicate of the previous one
				if s.Artist == previousScrobble.Artist && s.Track == previousScrobble.Track {
					timeDiff := s.Timestamp.Sub(previousScrobble.Timestamp)
					if timeDiff < s.Length {
						slog.Info("🎯 Duplicate scrobble detected!", "artist", s.Artist, "track", s.Track, "timestamp", s.Timestamp)
						err = deleteScrobble(taskCtx, s.TimestampString, delete)
						if err != nil {
							slog.Error("Failed to delete duplicate scrobble", "artist", s.Artist, "track", s.Track, "timestamp", s.Timestamp, "error", err)
						}
						if delete {
							slog.Info("Scrobble deleted", "artist", s.Artist, "track", s.Track, "timestamp", s.Timestamp)
						}
						deletedScrobbles++
					}
				}
			}
			previousScrobble = &s
		}
	}

	if delete {
		slog.Info("Scrobble deletion completed!", "deletedScrobbles", deletedScrobbles, "totalPages", totalPages)
	} else {
		slog.Info("Scrobble dry-run deletion completed!", "wouldBeDeletedScrobbles", deletedScrobbles, "totalPages", totalPages)
	}
}

func generateScrobble(row string) (Scrobble, error) {
	// Execute xpath on the row
	artist, track, timestamp, timestampStr := "", "", time.Time{}, ""

	doc, err := htmlquery.Parse(strings.NewReader(row))
	if err != nil {
		return Scrobble{}, fmt.Errorf("failed to parse row HTML: %w", err)
	}

	artistNode := htmlquery.FindOne(doc, `.//input[@name='artist_name']`)
	if artistNode != nil {
		artist = strings.TrimSpace(htmlquery.SelectAttr(artistNode, "value"))
	} else {
		return Scrobble{}, fmt.Errorf("artist not found in row: %v", row)
	}

	trackNode := htmlquery.FindOne(doc, `.//input[@name='track_name']`)
	if trackNode != nil {
		track = strings.TrimSpace(htmlquery.SelectAttr(trackNode, "value"))
	} else {
		return Scrobble{}, fmt.Errorf("track not found in row: %v", row)
	}

	timestampNode := htmlquery.FindOne(doc, `.//input[@name='timestamp']`)
	if timestampNode != nil {
		timestampStr = strings.TrimSpace(htmlquery.SelectAttr(timestampNode, "value"))
		// Timestamp is 1754948517
		timestampInt, err := strconv.ParseInt(timestampStr, 10, 64)
		if err != nil {
			return Scrobble{}, fmt.Errorf("failed to parse timestamp: %w", err)
		}
		timestamp = time.Unix(timestampInt, 0)
	} else {
		return Scrobble{}, fmt.Errorf("timestamp not found in row: %v", row)
	}

	return Scrobble{
		Artist:          artist,
		Track:           track,
		Timestamp:       timestamp,
		TimestampString: timestampStr,
	}, nil
}

func getRecordingLength(mb *gomusicbrainz.WS2Client, rdb *redis.Client, query string, queryHash []byte) (int, error) {
	length := -1

	resp, err := mb.SearchRecording(query, -1, -1)
	if err != nil {
		return 0, fmt.Errorf("failed to search MusicBrainz: %w", err)
	}

	if len(resp.Recordings) == 0 {
		slog.Warn("No recordings found for query", "query", query)
	} else {
		if len(resp.Recordings) > 1 {
			slog.Warn("Multiple recordings found for query, using the first one", "query", query, "count", len(resp.Recordings))
			for i, rec := range resp.Recordings {
				slog.Debug("Recording found", "artist", rec.ArtistCredit.NameCredits, "track", rec.Title, "length", rec.Length, "index", i)
			}
		}
		length = resp.Recordings[0].Length
	}

	rdb.Set(context.Background(), fmt.Sprintf("mbquery:%x", queryHash), length, 24*time.Hour)
	time.Sleep(200 * time.Millisecond) // Sleep to avoid hitting the API too fast
	return length, nil
}

func deleteScrobble(ctx context.Context, timestamp string, delete bool) error {
	xpathPrefix := `//input[@value='` + timestamp + `']`
	err := chromedp.Run(ctx,
		chromedp.Click(xpathPrefix+`/../../../../button`, chromedp.BySearch),
		chromedp.Sleep(200*time.Millisecond), // Wait for the hover effect to take place
		chromedp.Click(xpathPrefix+`/../../../../button`, chromedp.BySearch),
		chromedp.WaitVisible(xpathPrefix+`/../button`, chromedp.BySearch),
	)
	if err != nil {
		return fmt.Errorf("failed to hover or find delete button: %w", err)
	}

	if !delete {
		slog.Info("Scrobble deletion skipped", "timestamp", timestamp)
		return nil
	}
	err = chromedp.Run(ctx,
		chromedp.Click(xpathPrefix+`/../button`, chromedp.BySearch),
	)
	if err != nil {
		return fmt.Errorf("failed to click delete button: %w", err)
	}

	return nil
}
