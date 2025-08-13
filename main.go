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
		redisURL       string
		delete         bool
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
				Sources:     cli.NewValueSourceChain(yaml.YAML("redisURL", altsrc.NewStringPtrSourcer(&configFilePath))),
				Value:       "redis://localhost:6379/0",
				Destination: &redisURL,
			},
			&cli.BoolFlag{
				Name:        "delete",
				Usage:       "Delete duplicate scrobbles",
				Value:       false,
				Sources:     cli.NewValueSourceChain(yaml.YAML("delete", altsrc.NewStringPtrSourcer(&configFilePath))),
				Destination: &delete,
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
				Delete:         delete,
			}
			return app.Run(ctx, config)
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}
