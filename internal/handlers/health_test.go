package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestLiveAlwaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	(&HealthHandler{}).Live(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestReady(t *testing.T) {
	cases := []struct {
		name   string
		dbErr  error
		want   int
		wantDB string
	}{
		{"database up, optional deps unset", nil, http.StatusOK, "ok"},
		{"database down", errors.New("down"), http.StatusServiceUnavailable, "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &HealthHandler{DB: fakePinger{err: tc.dbErr}}
			rec := httptest.NewRecorder()
			h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			var body struct{ Checks map[string]string }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"database": tc.wantDB, "pterodactyl": "skipped", "smtp": "skipped"}
			for k, v := range want {
				if body.Checks[k] != v {
					t.Errorf("checks[%s] = %q, want %q", k, body.Checks[k], v)
				}
			}
		})
	}
}
