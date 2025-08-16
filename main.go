package main

import (
	"context"
	"log"
	"os"

	"github.com/cterence/scrobble-deduplicator/internal/app"
	altsrc "github.com/urfave/cli-altsrc/v3"
	"github.com/urfave/cli-altsrc/v3/yaml"
	"github.com/urfave/cli/v3"
)

func main() {
	var (
		configFilePath string
		cacheType      string
		lastFMUsername string
		lastFMPassword string
		startPage      int
		browserHeadful bool
		browserURL     string
		redisURL       string
		delete         bool
		logLevel       string
	)

	cmd := &cli.Command{
		Name:  "scrobble-deduplicator",
		Usage: "Deduplicate Last.fm scrobbles",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "config",
				Aliases:     []string{"c"},
				Value:       "config.yaml",
				Usage:       "Path to the configuration file",
				Destination: &configFilePath,
			},
			&cli.StringFlag{
				Name:        "lastfm-username",
				Aliases:     []string{"u"},
				Usage:       "Last.fm username",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("LASTFM_USERNAME"), yaml.YAML("lastfm.username", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &lastFMUsername,
			},
			&cli.StringFlag{
				Name:        "lastfm-password",
				Aliases:     []string{"p"},
				Usage:       "Last.fm password",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("LASTFM_PASSWORD"), yaml.YAML("lastfm.password", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &lastFMPassword,
			},
			&cli.BoolFlag{
				Name:        "delete",
				Usage:       "Delete duplicate scrobbles",
				Value:       false,
				Sources:     cli.NewValueSourceChain(cli.EnvVar("DELETE"), yaml.YAML("delete", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &delete,
			},
			&cli.IntFlag{
				Name:        "start-page",
				Aliases:     []string{"s"},
				Usage:       "Last.fm scrobble library page to start from",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("START_PAGE"), yaml.YAML("startPage", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &startPage,
			},
			&cli.StringFlag{
				Name:        "cache-type",
				Usage:       "Cache type for MusicBrainz API queries (inmemory, file, redis) (must specify redis-url flag for redis)",
				Value:       "inmemory",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("CACHE_TYPE"), yaml.YAML("cacheType", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &cacheType,
			},
			&cli.BoolFlag{
				Name:        "browser-headful",
				Usage:       "Run with a visible browser UI",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("BROWSER_HEADFUL"), yaml.YAML("browserHeadful", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &browserHeadful,
			},
			&cli.StringFlag{
				Name:        "browser-url",
				Usage:       "Remote browser URL",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("BROWSER_URL"), yaml.YAML("browserURL", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &browserURL,
			},
			&cli.StringFlag{
				Name:        "redis-url",
				Usage:       "Redis URL for redis cache type",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("REDIS_URL"), yaml.YAML("redisURL", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &redisURL,
			},
			&cli.StringFlag{
				Name:        "log-level",
				Usage:       "Log level (debug, info, warn, error)",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("LOG_LEVEL"), yaml.YAML("logLevel", altsrc.NewStringPtrSourcer(&configFilePath))),
				Value:       "info",
				Destination: &logLevel,
			},
		},
		Action: func(context.Context, *cli.Command) error {
			ctx := context.Background()

			config := app.Config{
				FilePath:       configFilePath,
				CacheType:      cacheType,
				LastFMUsername: lastFMUsername,
				LastFMPassword: lastFMPassword,
				StartPage:      startPage,
				BrowserHeadful: browserHeadful,
				RedisURL:       redisURL,
				BrowserURL:     browserURL,
				Delete:         delete,
				LogLevel:       logLevel,
			}

			return app.Run(ctx, &config)
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}
