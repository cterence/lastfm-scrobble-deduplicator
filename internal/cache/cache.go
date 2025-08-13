package cache

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

type Cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key string, value string) error
}

type InMemory struct {
	cache map[string]string
}

type Redis struct {
	client *redis.Client
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
