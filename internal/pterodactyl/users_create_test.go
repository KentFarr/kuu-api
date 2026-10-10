package pterodactyl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The panel only emails a set-password link when the user is created
// without a password, so CreateUser must never send one.
func TestCreateUserSendsNoPassword(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet: // username availability lookup
			_, _ = w.Write([]byte(`{"data": []}`))
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &created); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			_, _ = w.Write([]byte(`{"attributes": {"id": 5, "username": "alice", "email": "alice@example.com"}}`))
		}
	}))
	defer srv.Close()

	user, source, err := NewClient(srv.URL, "key").CreateUser(context.Background(), "alice@example.com", "alice")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.ID != 5 || source != UsernameSourceUserChosen {
		t.Errorf("user = %+v, source = %q", user, source)
	}
	if _, ok := created["password"]; ok {
		t.Errorf("create request included a password: %v", created)
	}
	if created["email"] != "alice@example.com" || created["username"] != "alice" {
		t.Errorf("create request = %v", created)
	}
}
