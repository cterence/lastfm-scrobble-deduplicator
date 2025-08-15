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
		noHeadless     bool
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
				Name:        "cache",
				Usage:       "Cache type (redis|inmemory)",
				Sources:     cli.NewValueSourceChain(yaml.YAML("cache", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &cacheType,
			},
			&cli.StringFlag{
				Name:        "lastfm-username",
				Aliases:     []string{"u"},
				Usage:       "Last.fm username",
				Sources:     cli.NewValueSourceChain(yaml.YAML("lastfm.username", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &lastFMUsername,
			},
			&cli.StringFlag{
				Name:        "lastfm-password",
				Aliases:     []string{"p"},
				Usage:       "Last.fm password",
				Sources:     cli.NewValueSourceChain(yaml.YAML("lastfm.password", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &lastFMPassword,
			},
			&cli.IntFlag{
				Name:        "start-page",
				Aliases:     []string{"s"},
				Usage:       "Page to start from",
				Sources:     cli.NewValueSourceChain(yaml.YAML("startPage", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &startPage,
			},
			&cli.BoolFlag{
				Name:        "no-headless",
				Usage:       "Run with browser UI",
				Sources:     cli.NewValueSourceChain(yaml.YAML("noHeadless", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &noHeadless,
			},
			&cli.StringFlag{
				Name:        "redis-url",
				Usage:       "Redis URL",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("REDIS_URL"), yaml.YAML("redisURL", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &redisURL,
			},
			&cli.BoolFlag{
				Name:        "delete",
				Usage:       "Delete duplicate scrobbles",
				Value:       false,
				Sources:     cli.NewValueSourceChain(yaml.YAML("delete", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &delete,
			},
			&cli.StringFlag{
				Name:        "browser-url",
				Usage:       "Browser URL",
				Sources:     cli.NewValueSourceChain(cli.EnvVar("BROWSER_URL"), yaml.YAML("browserURL", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &browserURL,
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
				NoHeadless:     noHeadless,
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
