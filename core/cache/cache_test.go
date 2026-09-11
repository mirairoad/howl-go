package cache

import (
	"context"
	"testing"
	"time"
)

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	ctx := context.Background()
	c := NewLRU(2)
	c.Set(ctx, "a", []byte("1"), time.Minute)
	c.Set(ctx, "b", []byte("2"), time.Minute)
	c.Get(ctx, "a") // a is now the most recent
	c.Set(ctx, "c", []byte("3"), time.Minute)

	if _, ok := c.Get(ctx, "b"); ok {
		t.Fatal("b should have been evicted")
	}
	if v, ok := c.Get(ctx, "a"); !ok || string(v) != "1" {
		t.Fatalf("a = %q %v, want 1 true", v, ok)
	}
}

func TestLRUExpires(t *testing.T) {
	ctx := context.Background()
	c := NewLRU(0)
	c.Set(ctx, "a", []byte("1"), -time.Second)
	if _, ok := c.Get(ctx, "a"); ok {
		t.Fatal("an expired entry was served")
	}
	c.Set(ctx, "b", []byte("2"), time.Minute)
	c.Del(ctx, "b")
	if _, ok := c.Get(ctx, "b"); ok {
		t.Fatal("a deleted entry was served")
	}
}

// slow never answers until its context ends — a Redis the network swallowed.
type slow struct{}

func (slow) Get(ctx context.Context, _ string) ([]byte, bool) {
	<-ctx.Done()
	return nil, false
}
func (slow) Set(ctx context.Context, _ string, _ []byte, _ time.Duration) { <-ctx.Done() }
func (slow) Del(ctx context.Context, _ ...string)                         { <-ctx.Done() }

func TestTryFallsBackOnMissAndWritesBoth(t *testing.T) {
	ctx := context.Background()
	primary, fallback := NewLRU(10), NewLRU(10)
	c := Try(primary, fallback, 0)

	fallback.Set(ctx, "only-fallback", []byte("f"), time.Minute)
	if v, ok := c.Get(ctx, "only-fallback"); !ok || string(v) != "f" {
		t.Fatalf("miss on primary did not fall back: %q %v", v, ok)
	}

	c.Set(ctx, "k", []byte("v"), time.Minute)
	if _, ok := primary.Get(ctx, "k"); !ok {
		t.Fatal("Set did not reach the primary")
	}
	if _, ok := fallback.Get(ctx, "k"); !ok {
		t.Fatal("Set did not reach the fallback")
	}
	c.Del(ctx, "k")
	if _, ok := fallback.Get(ctx, "k"); ok {
		t.Fatal("Del did not reach the fallback")
	}
}

func TestTryBoundsASlowPrimary(t *testing.T) {
	ctx := context.Background()
	fallback := NewLRU(10)
	fallback.Set(ctx, "k", []byte("v"), time.Minute)
	c := Try(slow{}, fallback, 20*time.Millisecond)

	start := time.Now()
	v, ok := c.Get(ctx, "k")
	if !ok || string(v) != "v" {
		t.Fatalf("Get = %q %v, want the fallback's value", v, ok)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a hung primary held the request for %s", took)
	}
}
