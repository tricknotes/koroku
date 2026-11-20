package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func int64ptr(v int64) *int64 { return &v }

func TestMemoryUsage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	blog := mustCreateApp(t, s, "blog")
	shop := mustCreateApp(t, s, "shop")
	base := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	limit := int64ptr(512 << 20)

	// Three samplings fall into the 03:00 bucket, one into 03:05.
	record := func(at time.Time, samples ...MemorySample) {
		t.Helper()
		if err := s.RecordMemorySamples(ctx, at, samples); err != nil {
			t.Fatalf("RecordMemorySamples: %v", err)
		}
	}
	record(base.Add(10*time.Second),
		MemorySample{AppID: blog.ID, Service: "web", Bytes: 100, LimitBytes: limit},
		MemorySample{AppID: blog.ID, Service: "db", Bytes: 50, LimitBytes: limit},
		MemorySample{AppID: shop.ID, Service: "web", Bytes: 999, LimitBytes: limit})
	record(base.Add(70*time.Second),
		MemorySample{AppID: blog.ID, Service: "web", Bytes: 300, LimitBytes: limit, Restarts: 1})
	record(base.Add(130*time.Second),
		MemorySample{AppID: blog.ID, Service: "web", Bytes: 200, LimitBytes: int64ptr(1 << 30)})
	record(base.Add(5*time.Minute),
		MemorySample{AppID: blog.ID, Service: "web", Bytes: 400})
	// A deleted app's samples are dropped without failing the others.
	record(base.Add(6*time.Minute),
		MemorySample{AppID: 9999, Service: "web", Bytes: 1},
		MemorySample{AppID: blog.ID, Service: "db", Bytes: 60, LimitBytes: limit})

	got, err := s.ListMemoryUsage(ctx, blog.ID, base)
	if err != nil {
		t.Fatal(err)
	}
	want := []MemoryBucket{
		{Service: "db", Start: base, MaxBytes: 50, SumBytes: 50, Samples: 1, LimitBytes: limit},
		{Service: "db", Start: base.Add(5 * time.Minute), MaxBytes: 60, SumBytes: 60, Samples: 1, LimitBytes: limit},
		{Service: "web", Start: base, MaxBytes: 300, SumBytes: 600, Samples: 3, LimitBytes: int64ptr(1 << 30), Restarts: 1},
		{Service: "web", Start: base.Add(5 * time.Minute), MaxBytes: 400, SumBytes: 400, Samples: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListMemoryUsage =\n%+v\nwant\n%+v", got, want)
	}
	if later, _ := s.ListMemoryUsage(ctx, blog.ID, base.Add(time.Minute)); len(later) != 2 {
		t.Errorf("since filter: %d buckets, want 2", len(later))
	}

	n, err := s.DeleteMemoryUsageBefore(ctx, base.Add(5*time.Minute))
	if err != nil || n != 3 {
		t.Errorf("DeleteMemoryUsageBefore = %d, %v, want 3", n, err)
	}
	if err := s.DeleteApp(ctx, blog.ID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_usage`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("rows after deleting the app = %d, %v", rows, err)
	}
}
