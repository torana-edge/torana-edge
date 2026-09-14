package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLocalCacheContractTTLAndAdmission(t *testing.T) {
	now := time.Unix(100, 0)
	l := NewLocalCacheWithClock(time.Minute, 10, 10, func() time.Time { return now })
	defer l.Close()
	if err := l.Set(context.Background(), "k", "old", 0); err != nil {
		t.Fatal(err)
	}
	if err := l.Set(context.Background(), "k", "new", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if _, ok, err := l.Get(context.Background(), "k"); err != nil || ok {
		t.Fatalf("exact deadline: ok=%v err=%v", ok, err)
	}
	if err := l.Set(context.Background(), "k", "old", 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Set(ctx, "k", "cancelled", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled set err=%v", err)
	}
	if v, ok, _ := l.Get(context.Background(), "k"); !ok || v != "old" {
		t.Fatalf("cancelled set changed value %q", v)
	}
	if err := l.Set(context.Background(), "k", "this value is too large", 0); err != ErrTooLarge {
		t.Fatalf("oversize err=%v", err)
	}
	if v, ok, _ := l.Get(context.Background(), "k"); !ok || v != "old" {
		t.Fatalf("oversize changed value %q", v)
	}
}

func TestRedisClosedPropagatesErrors(t *testing.T) {
	s, _ := newTestRedis(t, time.Minute)
	s.Close()
	ctx := context.Background()
	if err := s.Set(ctx, "k", "v", 0); err == nil {
		t.Error("closed Redis Set succeeded")
	}
	if _, _, err := s.Get(ctx, "k"); err == nil {
		t.Error("closed Redis Get succeeded")
	}
	if err := s.Delete(ctx, "k"); err == nil {
		t.Error("closed Redis Delete succeeded")
	}
}
