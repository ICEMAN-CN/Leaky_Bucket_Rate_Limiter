package main

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed advance.lua
var advanceScript string

//go:embed check.lua
var checkScript string

var decideScript = redis.NewScript(advanceScript + "\n" + checkScript)

type bucket struct {
	Level     float64 `json:"level"`
	UpdatedAt float64 `json:"updated_at"`
	Capacity  int     `json:"capacity"`
	LeakRate  float64 `json:"leak_rate"`
}

type bucketStore interface {
	check(context.Context, string) (bool, bucket, error)
	get(context.Context, string) (*bucket, error)
	ready(context.Context) error
}

type redisStore struct {
	client   *redis.Client
	capacity int
	rate     float64
	period   leakPeriod
}

type leakPeriod struct {
	units, micros float64
	compact       int
}

// Reduce the decimal leak rate to whole units per microsecond period.
func periodFor(rate float64) leakPeriod {
	ratio, _ := new(big.Rat).SetString(strconv.FormatFloat(rate, 'g', -1, 64))
	ratio.Quo(ratio, big.NewRat(1_000_000, 1))
	if ratio.Num().BitLen() <= 53 && ratio.Denom().BitLen() <= 53 {
		return leakPeriod{float64(ratio.Num().Int64()), float64(ratio.Denom().Int64()), 1}
	}
	// If the ratio exceeds Lua's exact integer range, keep the original rate and calculate leakage from the busy period's start.
	return leakPeriod{rate, 1e6, 0}
}

const storeTimeout = 500 * time.Millisecond

func newRedisStore(addr string, capacity int, rate float64) *redisStore {
	client := redis.NewClient(&redis.Options{
		Addr: addr, MaxRetries: -1, DialerRetries: 1,
		DialTimeout: storeTimeout, ReadTimeout: storeTimeout, WriteTimeout: storeTimeout,
		PoolTimeout: storeTimeout, ContextTimeoutEnabled: true,
	})
	return &redisStore{client, capacity, rate, periodFor(rate)}
}

func bucketKey(user string) string { return "leakybucket:limiter:v1:" + user }

func (s *redisStore) ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	return s.client.Ping(ctx).Err()
}

func (s *redisStore) check(ctx context.Context, user string) (bool, bucket, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	// Fall back from EVALSHA to EVAL only when Redis explicitly returns NOSCRIPT.
	values, err := decideScript.Run(ctx, s.client, []string{bucketKey(user)}, s.capacity,
		s.period.units, s.period.micros, s.period.compact, s.rate).Slice()
	if err != nil {
		return false, bucket{}, err
	}
	if len(values) != 5 {
		return false, bucket{}, fmt.Errorf("Redis returned an incomplete decision")
	}
	allowed, ok := values[0].(int64)
	if !ok || (allowed != 0 && allowed != 1) {
		return false, bucket{}, fmt.Errorf("Redis returned an invalid decision")
	}
	b, err := s.decode(values[1:])
	return allowed == 1, b, err
}

func (s *redisStore) get(ctx context.Context, user string) (*bucket, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	values, err := s.client.HMGet(ctx, bucketKey(user), "level", "at_us", "capacity", "leak_rate").Result()
	if err != nil {
		return nil, err
	}
	if values[0] == nil && values[1] == nil {
		return nil, nil
	}
	b, err := s.decode(values)
	return &b, err
}

func (s *redisStore) decode(values []any) (bucket, error) {
	water, okWater := values[0].(string)
	at, okTime := values[1].(string)
	if !okWater || !okTime {
		return bucket{}, fmt.Errorf("Redis bucket fields are incomplete")
	}
	level, err := strconv.ParseFloat(water, 64)
	if err != nil {
		return bucket{}, err
	}
	micros, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return bucket{}, err
	}
	b := bucket{level, float64(micros) / 1e6, s.capacity, s.rate}
	// Legacy buckets have no policy snapshot. Upgrade all replicas with the original settings first.
	if len(values) == 4 && (values[2] != nil || values[3] != nil) {
		capacity, okCapacity := values[2].(string)
		rate, okRate := values[3].(string)
		if !okCapacity || !okRate {
			return bucket{}, fmt.Errorf("Redis bucket policy fields are incomplete")
		}
		b.Capacity, err = strconv.Atoi(capacity)
		if err != nil || b.Capacity < 1 {
			return bucket{}, fmt.Errorf("Redis bucket capacity is invalid")
		}
		b.LeakRate, err = strconv.ParseFloat(rate, 64)
		if err != nil || b.LeakRate <= 0 || math.IsNaN(b.LeakRate) || math.IsInf(b.LeakRate, 0) {
			return bucket{}, fmt.Errorf("Redis bucket leak rate is invalid")
		}
	}
	return b, nil
}
