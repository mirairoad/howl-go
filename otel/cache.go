package otel

import (
	"context"
	"fmt"

	"github.com/mirairoad/howl-go/db"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Collection is what WatchCache needs of a db.Service: its name, and the
// counters it has been keeping since it was built. *db.Service[T, PT]
// satisfies it as it stands.
type Collection interface {
	Collection() string
	CacheStats() db.CacheStats
}

// WatchCache exports the two cache outcomes a trace cannot show.
//
//	otel.WatchCache(users, orders, invoices)
//
// Hits and misses are already on every db span — attributable to the request
// that caused them — and become howl.cache from there. The other two are
// decisions the service makes with no operation to hang them on:
//
//	howl.db.cache.bypassed    reads that could not use the cache at all,
//	                          because the shared version was unreadable; a
//	                          rising count is a broken Versioner, and it looks
//	                          exactly like a cache that is merely cold
//	howl.db.cache.too_large   results returned but not stored, over
//	                          Cache.MaxEntryBytes; an unbounded Find that
//	                          never gets cached however often it is asked for
//
// Both are observable counters read at collection time, so this costs nothing
// per operation — the service is already counting them for CacheStats.
func WatchCache(collections ...Collection) error {
	if len(collections) == 0 {
		return nil
	}
	meter := Meter("github.com/mirairoad/howl-go/otel")
	bypassed, err := meter.Int64ObservableCounter("howl.db.cache.bypassed",
		metric.WithDescription("Document reads that could not consult the cache, because the shared version was unreadable."))
	if err != nil {
		return fmt.Errorf("otel: %w", err)
	}
	tooLarge, err := meter.Int64ObservableCounter("howl.db.cache.too_large",
		metric.WithDescription("Results returned but not cached, being larger than Cache.MaxEntryBytes."))
	if err != nil {
		return fmt.Errorf("otel: %w", err)
	}
	watched := append([]Collection(nil), collections...) // the caller's slice is theirs to reuse
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, c := range watched {
			at := metric.WithAttributes(attribute.String("db.collection.name", c.Collection()))
			stats := c.CacheStats()
			o.ObserveInt64(bypassed, stats.Bypassed, at)
			o.ObserveInt64(tooLarge, stats.TooLarge, at)
		}
		return nil
	}, bypassed, tooLarge)
	if err != nil {
		return fmt.Errorf("otel: %w", err)
	}
	return nil
}
