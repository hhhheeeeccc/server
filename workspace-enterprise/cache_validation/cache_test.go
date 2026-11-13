package cache_validation

import (
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/platform/services/cache"
	"github.com/stretchr/testify/require"
)

func TestCacheConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, kind, address string
		db                  int
		errorID             string
	}{
		{name: "LRU accepts unused Redis defaults", kind: "lru", db: -1},
		{name: "Redis requires an address", kind: "redis", db: 0, errorID: "model.config.is_valid.empty_redis_address.app_error"},
		{name: "Redis rejects screenshot DB minus one", kind: "redis", address: "127.0.0.1:6379", db: -1, errorID: "model.config.is_valid.invalid_redis_db.app_error"},
		{name: "Redis accepts database zero", kind: "redis", address: "127.0.0.1:6379", db: 0},
		{name: "unknown backend rejected", kind: "invalid", db: 0, errorID: "model.config.is_valid.cache_type.app_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &model.Config{}
			cfg.SetDefaults()
			cfg.CacheSettings.CacheType = new(tc.kind)
			cfg.CacheSettings.RedisAddress = new(tc.address)
			cfg.CacheSettings.RedisDB = new(tc.db)
			err := cfg.IsValid()
			if tc.errorID == "" {
				require.Nil(t, err)
			} else {
				require.NotNil(t, err)
				require.Equal(t, tc.errorID, err.Id)
			}
		})
	}

	t.Run("Redis password sanitized for console responses", func(t *testing.T) {
		cfg := &model.Config{}
		cfg.SetDefaults()
		cfg.CacheSettings.RedisPassword = new("test-only-password")
		cfg.Sanitize(nil, nil)
		require.Equal(t, model.FakeSetting, *cfg.CacheSettings.RedisPassword)
	})
}

func redisProvider(t *testing.T, db int, prefix string, disabled bool) cache.Provider {
	t.Helper()
	address := os.Getenv("WORKSPACE_TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("run run.py for an isolated real Redis")
	}
	host, _, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host, "tests may only connect to disposable local Redis")
	provider, err := cache.NewRedisProvider(&cache.RedisOptions{
		RedisAddr: address, RedisPassword: os.Getenv("WORKSPACE_TEST_REDIS_PASSWORD"),
		RedisDB: db, RedisCachePrefix: prefix, DisableCache: disabled,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })
	pong, err := provider.Connect()
	require.NoError(t, err)
	require.Equal(t, "PONG", pong)
	require.Equal(t, "redis", provider.Type())
	return provider
}

func newCache(t *testing.T, provider cache.Provider, name string) cache.Cache {
	t.Helper()
	c, err := provider.NewCache(&cache.CacheOptions{Name: name, Size: 1024, DefaultExpiry: 2 * time.Second})
	require.NoError(t, err)
	return c
}

func TestRealRedisOperations(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		name := "client_cache_enabled"
		if disabled {
			name = "client_cache_disabled"
		}
		t.Run(name, func(t *testing.T) {
			prefix := model.NewId()
			provider := redisProvider(t, 0, prefix, disabled)
			c := newCache(t, provider, "users")
			other := newCache(t, redisProvider(t, 0, prefix, disabled), "users")

			t.Run("serialize user and arbitrary struct", func(t *testing.T) {
				user := &model.User{Id: model.NewId(), Username: "cache-test", Roles: "system_user"}
				require.NoError(t, c.SetWithExpiry("user", user, 0))
				var got model.User
				require.NoError(t, c.Get("user", &got))
				require.Equal(t, user.Id, got.Id)
				require.Equal(t, user.Username, got.Username)
				value := struct{ Message string }{Message: "اختبار التخزين"}
				require.NoError(t, c.SetWithExpiry("value", value, 0))
				var decoded struct{ Message string }
				require.NoError(t, c.Get("value", &decoded))
				require.Equal(t, value, decoded)
			})

			t.Run("multiple reads including missing key", func(t *testing.T) {
				require.NoError(t, c.SetWithExpiry("a", "alpha", 0))
				require.NoError(t, c.SetWithExpiry("b", int64(42), 0))
				var a, missing string
				var b int64
				errs := c.GetMulti([]string{"a", "b", "missing"}, []any{&a, &b, &missing})
				require.Len(t, errs, 3)
				require.NoError(t, errs[0])
				require.NoError(t, errs[1])
				require.ErrorIs(t, errs[2], cache.ErrKeyNotFound)
				require.Equal(t, "alpha", a)
				require.Equal(t, int64(42), b)
			})

			t.Run("cross process cache invalidation on update and delete", func(t *testing.T) {
				require.NoError(t, c.SetWithExpiry("shared", "old", 0))
				var got string
				require.NoError(t, c.Get("shared", &got)) // populate local client cache
				require.Equal(t, "old", got)
				require.NoError(t, other.SetWithExpiry("shared", "new", 0))
				require.Eventually(t, func() bool { return c.Get("shared", &got) == nil && got == "new" }, 2*time.Second, 10*time.Millisecond)
				require.NoError(t, other.Remove("shared"))
				require.Eventually(t, func() bool { return errors.Is(c.Get("shared", &got), cache.ErrKeyNotFound) }, 2*time.Second, 10*time.Millisecond)
			})

			t.Run("expiry respected by both clients", func(t *testing.T) {
				require.NoError(t, c.SetWithDefaultExpiry("expires", "temporary"))
				var got string
				require.NoError(t, other.Get("expires", &got))
				require.Eventually(t, func() bool { return errors.Is(other.Get("expires", &got), cache.ErrKeyNotFound) }, 4*time.Second, 20*time.Millisecond)
			})

			t.Run("atomic increments and decrements", func(t *testing.T) {
				require.NoError(t, c.SetWithExpiry("counter", int64(0), 0))
				var wg sync.WaitGroup
				errs := make(chan error, 8)
				for worker := range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						writer := c.(cache.ExternalCache)
						if worker%2 == 0 {
							writer = other.(cache.ExternalCache)
						}
						for range 20 {
							if err := writer.Increment("counter", 1); err != nil {
								errs <- err
								return
							}
						}
					}()
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					require.NoError(t, err)
				}
				var got int64
				require.NoError(t, c.Get("counter", &got))
				require.Equal(t, int64(160), got)
				require.NoError(t, c.(cache.ExternalCache).Decrement("counter", 10))
				require.Eventually(t, func() bool { return other.Get("counter", &got) == nil && got == 150 }, 2*time.Second, 10*time.Millisecond)
			})

			t.Run("bulk deletion and namespace scoped purge", func(t *testing.T) {
				independent := newCache(t, provider, "sessions")
				require.NoError(t, independent.SetWithExpiry("keep", "safe", 0))
				require.NoError(t, c.RemoveMulti([]string{"a", "b"}))
				require.NoError(t, c.RemoveMulti(nil))
				var got string
				start := time.Now()
				require.Eventually(t, func() bool { return errors.Is(c.Get("a", &got), cache.ErrKeyNotFound) }, 2*time.Second, time.Millisecond)
				t.Logf("bulk deletion visible after %s", time.Since(start))
				require.NoError(t, c.Purge())
				require.Eventually(t, func() bool { return errors.Is(c.Get("user", &model.User{}), cache.ErrKeyNotFound) }, 2*time.Second, time.Millisecond)
				require.NoError(t, independent.Get("keep", &got))
				require.Equal(t, "safe", got)
			})

			t.Run("database and prefix isolation", func(t *testing.T) {
				require.NoError(t, c.SetWithExpiry("isolated", "db-zero", 0))
				otherDB := newCache(t, redisProvider(t, 1, prefix, disabled), "users")
				otherPrefix := newCache(t, redisProvider(t, 0, model.NewId(), disabled), "users")
				var got string
				require.ErrorIs(t, otherDB.Get("isolated", &got), cache.ErrKeyNotFound)
				require.ErrorIs(t, otherPrefix.Get("isolated", &got), cache.ErrKeyNotFound)
			})
		})
	}
}

func TestRedisAuthenticationFailure(t *testing.T) {
	address := os.Getenv("WORKSPACE_TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("run run.py for an isolated real Redis")
	}
	p, err := cache.NewRedisProvider(&cache.RedisOptions{RedisAddr: address, RedisDB: 0, RedisPassword: "incorrect-test-password"})
	if p != nil {
		defer p.Close()
	}
	require.Error(t, err)
}
