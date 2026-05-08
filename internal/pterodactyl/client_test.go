package pterodactyl

import "testing"

func TestRouteLabelCollapsesNumericIDs(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{"POST", "/api/application/users", "POST /api/application/users"},
		{"DELETE", "/api/application/users/42", "DELETE /api/application/users/:id"},
		{"POST", "/api/application/servers/123/suspend", "POST /api/application/servers/:id/suspend"},
		{"GET", "/api/application/servers?per_page=100&page=2", "GET /api/application/servers"},
		// Non-numeric segments must be preserved verbatim.
		{"GET", "/api/application/nodes/abc", "GET /api/application/nodes/abc"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			got := routeLabel(tc.method, tc.path)
			if got != tc.want {
				t.Fatalf("routeLabel(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestAPIErrorMessage(t *testing.T) {
	with := &APIError{StatusCode: 422, Body: `{"errors":[{"detail":"bad"}]}`}
	if got := with.Error(); got != `pterodactyl API error (status 422): {"errors":[{"detail":"bad"}]}` {
		t.Fatalf("APIError.Error() did not include body: %q", got)
	}
	without := &APIError{StatusCode: 500}
	if got := without.Error(); got != "pterodactyl API error (status 500)" {
		t.Fatalf("APIError.Error() with empty body wrong: %q", got)
	}
}
