package store

import (
	"context"
	"testing"
	"time"
)

func TestActivityRecordListAndPrune(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	blog := mustCreateApp(t, s, "blog")
	shop := mustCreateApp(t, s, "shop")

	// Without an actor in the context the entry is koroku's own.
	a, err := s.RecordActivity(ctx, blog.ID, "app.started", nil, testNow)
	if err != nil {
		t.Fatalf("RecordActivity: %v", err)
	}
	if a.Actor != nil || string(a.Details) != "{}" {
		t.Errorf("entry without actor = %+v", a)
	}
	acting := WithActor(ctx, Actor{UserID: 7, Login: "mia"})
	if _, err := s.RecordActivity(acting, blog.ID, "domain.added", map[string]any{"hostname": "example.com"}, testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordActivity(acting, shop.ID, "app.stopped", nil, testNow); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListActivity(ctx, blog.ID, 0, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListActivity = %+v, %v", list, err)
	}
	if list[0].Action != "domain.added" || list[0].Actor == nil || list[0].Actor.Login != "mia" ||
		string(list[0].Details) != `{"hostname":"example.com"}` || !list[0].At.Equal(testNow.Add(time.Minute)) {
		t.Errorf("newest entry = %+v", list[0])
	}
	if page, _ := s.ListActivity(ctx, blog.ID, list[0].ID, 10); len(page) != 1 || page[0].Action != "app.started" {
		t.Errorf("page before %d = %+v", list[0].ID, page)
	}

	// Only the newest ActivityKeep entries stay, per app.
	for i := 0; i < ActivityKeep+5; i++ {
		if _, err := s.RecordActivity(ctx, blog.ID, "app.restarted", nil, testNow); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_activity WHERE app_id = ?`, blog.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != ActivityKeep {
		t.Errorf("blog keeps %d entries, want %d", n, ActivityKeep)
	}
	if rest, _ := s.ListActivity(ctx, shop.ID, 0, 10); len(rest) != 1 {
		t.Errorf("shop's entries were pruned with blog's: %+v", rest)
	}

	// Deleting the app deletes its entries.
	if err := s.DeleteApp(ctx, blog.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_activity WHERE app_id = ?`, blog.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("entries after deleting the app = %d, %v", n, err)
	}
}
