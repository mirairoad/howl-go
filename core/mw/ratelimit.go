package mw

import (
	"container/list"
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The rate-limit headers. The IETF draft spells them without the X- and
// changed their meaning twice while it was in draft; the X- spelling is what
// GitHub and Stripe send, what every client library already parses, and what a
// person debugging with curl recognises. Reset is an epoch second, as GitHub's
// is — Retry-After next to it carries the delta, which is what the HTTP spec
// defines and what a retrying client should read.
const (
	HeaderRateLimit          = "X-RateLimit-Limit"
	HeaderRateLimitRemaining = "X-RateLimit-Remaining"
	HeaderRateLimitReset     = "X-RateLimit-Reset"
)

// Rate is how often one key may be let through.
type Rate struct {
	// Requests per Window, once the bucket is empty.
	Requests int
	Window   time.Duration
	// Burst is how many may arrive at once after a quiet spell. Zero means
	// Requests: a window's worth in one go, then one more every
	// Window/Requests. Lower it to smooth traffic out — Requests: 60, Window:
	// time.Minute, Burst: 5 is a steady one per second that tolerates five
	// together, which is what a page firing five fetches at load looks like.
	Burst int
}

// Decision is what a Limiter answered.
type Decision struct {
	// OK is whether this request may proceed. Everything else is for the
	// headers, and is filled either way.
	OK bool
	// Remaining is how many more requests the bucket would allow right now.
	Remaining int
	// Retry is how long until one token exists, and is zero when OK.
	Retry time.Duration
	// Reset is how long until the bucket is full again — the point at which
	// the caller's whole allowance is back.
	Reset time.Duration
}

// Limiter is where the counting happens, so that it can happen somewhere else.
//
// The default implementation counts in this process, which is correct for one
// replica and too permissive for several: three of them behind a load balancer
// allow three times the rate, because each sees a third of the traffic and
// limits against its own share. That is fine for protecting a process from a
// scraper and not fine for protecting a sign-in form from a password spray.
// For the second case implement this interface against something shared —
// Redis with INCR and a Lua script, or a table with an atomic upsert.
//
// One method, and it both counts and decides: a limiter split into "read the
// count" and "write the count" is a race between the two, and the whole point
// of doing this in Redis is that the increment is atomic.
type Limiter interface {
	Take(ctx context.Context, key string, rate Rate) Decision
}

// RateLimit answers 429 when a caller asks for more than Rate allows, and is
// the implementation behind api.Spec.Limit.
//
// It is a token bucket, not a window counter. A counter resets on a boundary,
// so "60 per minute" lets 120 requests through in the two seconds either side
// of it, and the limit it advertises is not the limit it enforces. A bucket
// refills continuously: the burst is whatever Burst says it is and never twice
// that.
//
//	Use: []mw.Middleware{
//	    mw.Only("/api", mw.RateLimit{Requests: 600, Window: time.Minute}.Handler),
//	}
//
// The headers go out before the handler runs, which is also what keeps them
// out of mw.Cache: that middleware stores what the handler added to the header
// map, and these were already in it.
type RateLimit struct {
	// Requests and Window are the rate; Burst is the bucket. Requests or
	// Window at zero disables the limit and returns the handler unwrapped, so
	// a config that forgot to set them does not block every request.
	Requests int
	Window   time.Duration
	Burst    int

	// Key is what is being limited. Nil is the client's IP address — the right
	// default because the requests worth limiting are the ones from a caller
	// who has not signed in. Return "" to let a request past uncounted, the way
	// a health check or an internal caller should be.
	//
	// Whatever it returns, it must not vary with anything the caller chooses
	// freely: a key built from a query parameter or a path id is a limit the
	// caller opts out of by changing one character.
	Key func(*http.Request) string
	// TrustProxy reads the address from X-Forwarded-For for the default Key.
	// Set it only behind a proxy that overwrites that header: anyone can send
	// one, and a limiter keyed by a header the caller controls is not a
	// limiter. Without a proxy, every request would otherwise be keyed by the
	// proxy's own address and share one bucket.
	TrustProxy bool

	// Store does the counting. Nil counts in this process — see Limiter for
	// why that is the wrong choice for more than one replica.
	Store Limiter
	// Max is how many keys the in-process store tracks, least-recently-seen
	// evicted. Default 20000, measured at 3.4 MB — 178 bytes a bucket plus the
	// key string. Ignored when Store is set.
	Max int

	// OnLimit writes the refusal. Nil is a plain-text 429. The rate-limit
	// headers and Retry-After are already set when it runs, so a handler of
	// your own writes only the body it wants — that is how api.Spec.Limit
	// answers with the same JSON envelope as every other endpoint error.
	OnLimit http.Handler
}

func (l RateLimit) Handler(next http.Handler) http.Handler {
	if l.Requests <= 0 || l.Window <= 0 {
		return next
	}
	rate := Rate{Requests: l.Requests, Window: l.Window, Burst: l.Burst}
	store := l.Store
	if store == nil {
		store = NewLimiter(l.Max)
	}
	key := l.Key
	if key == nil {
		trust := l.TrustProxy
		key = func(r *http.Request) string { return "ip\x00" + ClientIP(r, trust) }
	}
	refuse := l.OnLimit
	if refuse == nil {
		refuse = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := key(r)
		if name == "" {
			next.ServeHTTP(w, r)
			return
		}
		d := store.Take(r.Context(), name, rate)

		h := w.Header()
		h.Set(HeaderRateLimit, strconv.Itoa(rate.Requests))
		h.Set(HeaderRateLimitRemaining, strconv.Itoa(d.Remaining))
		h.Set(HeaderRateLimitReset, strconv.FormatInt(time.Now().Add(d.Reset).Unix(), 10))
		if d.OK {
			next.ServeHTTP(w, r)
			return
		}
		// Whole seconds, rounded up and never zero: Retry-After: 0 reads as
		// "now", and a client that believes it retries immediately and is
		// refused again, which is the busy-loop this header exists to prevent.
		h.Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(d.Retry.Seconds())))))
		refuse.ServeHTTP(w, r)
	})
}

// NewLimiter returns an in-process Limiter tracking at most max keys, evicting
// by last use. max <= 0 means 20000.
//
// Exported so that several RateLimits can share one map — a per-IP limit on the
// pages and a stricter one on /api/sign-in want two buckets per caller, not two
// maps. They are two buckets only if their keys differ, and the default Key is
// the address alone: give at least one of them a Key that names it, the way
// api.Spec.Limit includes the endpoint's pattern.
func NewLimiter(max int) Limiter {
	if max <= 0 {
		max = 20000
	}
	return &limiter{max: max, index: make(map[string]*list.Element, max), order: list.New()}
}

type limiter struct {
	mu    sync.Mutex
	max   int
	index map[string]*list.Element
	order *list.List
}

// bucket is one key's allowance. rate and burst are stored per bucket rather
// than per limiter because one limiter serves several RateLimits, each with its
// own Rate, and eviction has to know how full a bucket is without being told.
type bucket struct {
	key   string
	n     float64 // tokens as of last
	last  time.Time
	rate  float64 // tokens per second
	burst float64
}

func (l *limiter) Take(_ context.Context, key string, r Rate) Decision {
	burst := float64(r.Burst)
	if burst <= 0 {
		burst = float64(r.Requests)
	}
	perSecond := float64(r.Requests) / r.Window.Seconds()
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	var b *bucket
	if el, ok := l.index[key]; ok {
		l.order.MoveToFront(el)
		b = el.Value.(*bucket)
		b.n = math.Min(burst, b.n+now.Sub(b.last).Seconds()*perSecond)
	} else {
		// Room is made BEFORE the newcomer goes in, so every candidate below
		// is a bucket whose rate and burst are already known. Evicting after
		// inserting puts a half-built one in the scan, where a zero burst
		// reads as empty: eviction then keeps it for the wrong reason, or —
		// once its burst is set — throws out the caller it just admitted,
		// leaving that caller with no bucket and so no limit.
		if len(l.index) >= l.max {
			l.evict(now)
		}
		// A key first seen arrives with a full bucket. The alternative — start
		// empty and earn tokens — would refuse every caller's first request.
		b = &bucket{key: key, n: burst}
		l.index[key] = l.order.PushFront(b)
	}
	b.last, b.rate, b.burst = now, perSecond, burst

	d := Decision{OK: b.n >= 1}
	if d.OK {
		b.n--
		d.Remaining = int(b.n)
	} else {
		d.Retry = seconds((1 - b.n) / perSecond)
	}
	d.Reset = seconds((burst - b.n) / perSecond)
	return d
}

// evict makes room for one more key. Among the oldest few it prefers a bucket that is NOT
// currently refusing requests, rather than taking the least recently seen
// outright: dropping a bucket that is refusing hands its owner a full allowance
// back, so plain LRU would make "send requests under ten thousand invented
// keys" the way to clear the bucket doing the limiting. Skipping the ones at
// zero closes that, and when every candidate is at zero the oldest goes anyway
// — which is the honest reason a shared Store exists.
func (l *limiter) evict(now time.Time) {
	const scan = 8 // a fixed budget: eviction runs under the lock
	for len(l.index) >= l.max {
		victim := l.order.Back()
		for el, i := victim, 0; el != nil && i < scan; el, i = el.Prev(), i+1 {
			b := el.Value.(*bucket)
			if math.Min(b.burst, b.n+now.Sub(b.last).Seconds()*b.rate) >= 1 {
				victim = el
				break
			}
		}
		if victim == nil {
			return
		}
		l.order.Remove(victim)
		delete(l.index, victim.Value.(*bucket).key)
	}
}

func seconds(f float64) time.Duration {
	if f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}
