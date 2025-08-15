package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

const lastFMLoginURL = "https://www.last.fm/login"

func login(ctx context.Context, c *Config) error {
	slog.Info("Navigating to Last.fm login page", "url", lastFMLoginURL)

	timeoutCtx, cancel := context.WithTimeout(ctx, browserOperationsTimeout)
	defer cancel()

	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(lastFMLoginURL),
		chromedp.WaitVisible(`#onetrust-accept-btn-handler`, chromedp.ByID),
		chromedp.Sleep(1*time.Second), // Cookie banner takes a while to come up, we don't want to miss the click
		chromedp.Click(`#onetrust-accept-btn-handler`, chromedp.ByID),
		chromedp.Sleep(500*time.Millisecond), // Wait for cookie banner to disappear
		chromedp.SendKeys(`id_username_or_email`, strings.ToLower(c.LastFMUsername), chromedp.ByID),
		chromedp.SendKeys(`id_password`, c.LastFMPassword, chromedp.ByID),
		chromedp.Click(`//div[@class='form-submit']/button[@class='btn-primary']`, chromedp.BySearch),
		chromedp.WaitVisible(`//h1[@class='header-title']/a`, chromedp.BySearch),
	)
	if err != nil {
		return fmt.Errorf("failed to login to Last.fm: %w", err)
	}

	slog.Info("Successfully logged in!")
	return nil
}
