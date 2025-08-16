package cache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

type Cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key string, value string) error
	Close()
}

type InMemory struct {
	cache map[string]string
}

type Redis struct {
	client *redis.Client
}

type File struct {
	mu   sync.Mutex
	file *os.File
	path string
	data map[string]string
}

var ErrCacheMiss = errors.New("cache miss")

func NewInMemory() Cache {
	return &InMemory{
		cache: make(map[string]string),
	}
}

func (c *InMemory) Get(_ context.Context, key string) (string, error) {
	value, exists := c.cache[key]
	if !exists {
		return "", ErrCacheMiss
	}
	return value, nil
}

func (c *InMemory) Set(_ context.Context, key string, value string) error {
	c.cache[key] = value
	return nil
}

func (c *InMemory) Close() {}

func NewRedis(redisClient *redis.Client) Cache {
	return &Redis{
		client: redisClient,
	}
}

func (c *Redis) Get(ctx context.Context, key string) (string, error) {
	val, err := c.client.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return "", ErrCacheMiss
		}
		return "", err
	}
	return val, nil
}

func (c *Redis) Set(ctx context.Context, key string, value string) error {
	return c.client.Set(ctx, key, value, 0).Err()
}

func (c *Redis) Close() {
	if err := c.client.Close(); err != nil {
		slog.Error("Failed to close redis client", "error", err)
	}
}

const CacheFileName = "cache.db"

func NewFile(path string) (Cache, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, err
	}

	cache := &File{
		file: f,
		path: path,
		data: make(map[string]string),
	}

	if err := cache.load(); err != nil {
		if err := f.Close(); err != nil {
			return nil, err
		}
		return nil, err
	}
	return cache, nil
}

// load reads the file into memory (simple, no partial reads)
func (c *File) load() error {
	c.data = make(map[string]string)
	_, err := c.file.Seek(0, 0)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(c.file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			c.data[parts[0]] = parts[1]
		}
	}
	return scanner.Err()
}

func (c *File) Get(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	val, ok := c.data[key]
	if !ok {
		return "", ErrCacheMiss
	}
	return val, nil
}

func (c *File) Set(ctx context.Context, key string, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// update in-memory map
	c.data[key] = value

	// truncate and rewrite file
	if err := c.file.Truncate(0); err != nil {
		return err
	}
	if _, err := c.file.Seek(0, 0); err != nil {
		return err
	}
	for k, v := range c.data {
		if _, err := fmt.Fprintf(c.file, "%s=%s\n", k, v); err != nil {
			return err
		}
	}
	return c.file.Sync()
}

func (c *File) Close() {
	if err := c.file.Close(); err != nil {
		slog.Error("failed to close file cache", "error", err)
	}
}
