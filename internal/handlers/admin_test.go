package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/KentFarr/kuu-api/internal/database"
	"github.com/KentFarr/kuu-api/internal/pterodactyl"
)

const testAdminToken = "admin-secret"

type fakeAdminStore struct {
	enqueued   []string // recipients
	retried    []int
	pending    []database.PendingEmail
	nodeServer map[int][]int // node ID -> pterodactyl server IDs in our DB
	err        error
}

func (f *fakeAdminStore) EnqueueEmail(to, _, _ string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.enqueued = append(f.enqueued, to)
	return len(f.enqueued), nil
}

func (f *fakeAdminStore) ResetEmailForRetry(id int) error {
	if f.err != nil {
		return f.err
	}
	f.retried = append(f.retried, id)
	return nil
}

func (f *fakeAdminStore) ListPendingEmails(int) ([]database.PendingEmail, error) {
	return f.pending, f.err
}

func (f *fakeAdminStore) ListServerPteroIDs(nodeID int) ([]int, error) {
	return f.nodeServer[nodeID], f.err
}

type fakeAdminPtero struct {
	servers map[int][]pterodactyl.ServerSummary
	err     error
}

func (f *fakeAdminPtero) ListServersOnNode(_ context.Context, nodeID int) ([]pterodactyl.ServerSummary, error) {
	return f.servers[nodeID], f.err
}

// adminMux registers the admin routes the way main does.
func adminMux(h *AdminHandler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func adminRequest(mux *http.ServeMux, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAdminRequiresBearerToken(t *testing.T) {
	mux := adminMux(&AdminHandler{Token: testAdminToken, DB: &fakeAdminStore{}})

	for name, token := range map[string]string{"missing": "", "wrong": "nope"} {
		t.Run(name, func(t *testing.T) {
			if rec := adminRequest(mux, http.MethodGet, "/admin/emails", token); rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAdminRoutesDisabledWithoutToken(t *testing.T) {
	mux := adminMux(&AdminHandler{DB: &fakeAdminStore{}})
	if rec := adminRequest(mux, http.MethodGet, "/admin/emails", ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (routes not registered)", rec.Code)
	}
}

func TestAdminEmailEndpoints(t *testing.T) {
	store := &fakeAdminStore{pending: []database.PendingEmail{{ID: 1}, {ID: 2}}}
	mux := adminMux(&AdminHandler{Token: testAdminToken, DB: store})

	if rec := adminRequest(mux, http.MethodGet, "/admin/emails", testAdminToken); rec.Code != http.StatusOK {
		t.Errorf("list: status = %d, want 200", rec.Code)
	} else {
		var body struct{ Count int }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Count != 2 {
			t.Errorf("list: count = %d, want 2", body.Count)
		}
	}

	if rec := adminRequest(mux, http.MethodPost, "/admin/emails/7/retry", testAdminToken); rec.Code != http.StatusOK || len(store.retried) != 1 || store.retried[0] != 7 {
		t.Errorf("retry: status = %d, retried = %v; want 200, [7]", rec.Code, store.retried)
	}
	if rec := adminRequest(mux, http.MethodPost, "/admin/emails/abc/retry", testAdminToken); rec.Code != http.StatusBadRequest {
		t.Errorf("retry bad id: status = %d, want 400", rec.Code)
	}

	if rec := adminRequest(mux, http.MethodPost, "/admin/test-email?to=ops@example.com", testAdminToken); rec.Code != http.StatusOK || len(store.enqueued) != 1 {
		t.Errorf("test-email: status = %d, enqueued = %v", rec.Code, store.enqueued)
	}
	if rec := adminRequest(mux, http.MethodPost, "/admin/test-email", testAdminToken); rec.Code != http.StatusBadRequest {
		t.Errorf("test-email without to: status = %d, want 400", rec.Code)
	}
	if rec := adminRequest(mux, http.MethodPost, "/admin/test-email?to=a@b.c&immediate=true", testAdminToken); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("immediate test-email without SMTP: status = %d, want 503", rec.Code)
	}
}

func TestAdminEmailEndpointErrors(t *testing.T) {
	mux := adminMux(&AdminHandler{Token: testAdminToken, DB: &fakeAdminStore{err: errors.New("db")}})
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/admin/emails", http.StatusInternalServerError},
		{http.MethodPost, "/admin/emails/7/retry", http.StatusNotFound},
		{http.MethodPost, "/admin/test-email?to=a@b.c", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if rec := adminRequest(mux, tc.method, tc.path, testAdminToken); rec.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

func TestAdminReconciliationClassifiesServers(t *testing.T) {
	ptero := &fakeAdminPtero{servers: map[int][]pterodactyl.ServerSummary{
		1: {{ID: 10, Name: "tracked"}, {ID: 11, Name: "orphan"}},
	}}
	store := &fakeAdminStore{nodeServer: map[int][]int{1: {10, 12}}}
	mux := adminMux(&AdminHandler{Token: testAdminToken, DB: store, Ptero: ptero, NodeIDs: []int{1}})

	rec := adminRequest(mux, http.MethodGet, "/admin/reconciliation", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Counts map[string]int
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// 10 is in both, 11 only on the panel, 12 only in our DB.
	want := map[string]int{"tracked": 1, "orphaned": 1, "dangling": 1}
	for k, v := range want {
		if body.Counts[k] != v {
			t.Errorf("%s = %d, want %d (body %s)", k, body.Counts[k], v, rec.Body.String())
		}
	}
}

func TestAdminReconciliationErrors(t *testing.T) {
	t.Run("panel down", func(t *testing.T) {
		mux := adminMux(&AdminHandler{Token: testAdminToken, DB: &fakeAdminStore{}, Ptero: &fakeAdminPtero{err: errors.New("down")}, NodeIDs: []int{1}})
		if rec := adminRequest(mux, http.MethodGet, "/admin/reconciliation", testAdminToken); rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", rec.Code)
		}
	})
	t.Run("db down", func(t *testing.T) {
		mux := adminMux(&AdminHandler{Token: testAdminToken, DB: &fakeAdminStore{err: errors.New("db")}, Ptero: &fakeAdminPtero{}, NodeIDs: []int{1}})
		if rec := adminRequest(mux, http.MethodGet, "/admin/reconciliation", testAdminToken); rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
}
