package routing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/store"
)

// TestBuildRoutesToTheLiveColour: a blue/green app is proxied to the port
// of the colour the database names, with the hold settings of chapter 8
// (ARCHITECTURE 7.2).
func TestBuildRoutesToTheLiveColour(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	blog := createApp(t, st, "blog") // 20000
	markDeployed(t, st, blog)
	alt, err := st.AllocateAltHostPort(ctx, blog.ID, 20000, 20999)
	if err != nil {
		t.Fatal(err)
	}
	upstreams := func() []string {
		t.Helper()
		config, err := newRouter(st, &fakeLoader{}).Build(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var dials []string
		var walk func(v any)
		walk = func(v any) {
			switch v := v.(type) {
			case map[string]any:
				if d, ok := v["dial"].(string); ok && strings.HasPrefix(d, "127.0.0.1:2") {
					dials = append(dials, d)
				}
				if lb, ok := v["load_balancing"].(map[string]any); ok && lb["try_duration"] == nil {
					t.Errorf("an app upstream without the hold settings: %v", lb)
				}
				for _, x := range v {
					walk(x)
				}
			case []any:
				for _, x := range v {
					walk(x)
				}
			}
		}
		var doc any
		if err := json.Unmarshal(config, &doc); err != nil {
			t.Fatal(err)
		}
		walk(doc)
		return dials
	}
	for _, tc := range []struct {
		color string
		want  string
	}{
		{"", "127.0.0.1:20000"},
		{store.ColorGreen, "127.0.0.1:20001"},
		{store.ColorBlue, "127.0.0.1:20000"},
	} {
		if err := st.SetAppWebColor(ctx, blog.ID, tc.color); err != nil {
			t.Fatal(err)
		}
		if got := upstreams(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("colour %q: upstreams %v, want %s (second port %d)", tc.color, got, tc.want, alt)
		}
	}
}
