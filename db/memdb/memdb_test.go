package memdb_test

import (
	"testing"
	"time"

	"github.com/mirairoad/howl-go/db"
	"github.com/mirairoad/howl-go/db/conformance"
	"github.com/mirairoad/howl-go/db/memdb"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Service {
		s, err := memdb.NewService[conformance.Doc](db.Options{Collection: "conformance"})
		if err != nil {
			t.Fatalf("new service: %v", err)
		}
		return s
	})
}

// The same suite with the cache on. Every case here already passed uncached,
// so a failure is an invalidation bug and nothing else — which is the only
// way to catch one, since a stale read looks exactly like a correct one until
// something else changed.
func TestConformanceCached(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Service {
		s, err := memdb.NewService[conformance.Doc](db.Options{
			Collection: "conformance",
			Cache:      db.Cache{TTL: time.Minute},
		})
		if err != nil {
			t.Fatalf("new service: %v", err)
		}
		return s
	})
}
