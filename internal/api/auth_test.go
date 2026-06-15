package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenAuth(t *testing.T) {
	const token = "s3cr3t"

	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := TokenAuth(token)(next)

	tests := []struct {
		name       string
		path       string
		header     string
		wantStatus int
		wantNext   bool
	}{
		{"healthz bypasses auth", "/healthz", "", http.StatusOK, true},
		{"no auth header", "/api/v1/jobs", "", http.StatusUnauthorized, false},
		{"missing Bearer prefix", "/api/v1/jobs", "s3cr3t", http.StatusUnauthorized, false},
		{"wrong token", "/api/v1/jobs", "Bearer nope", http.StatusUnauthorized, false},
		{"correct token", "/api/v1/jobs", "Bearer s3cr3t", http.StatusOK, true},
		{"empty bearer token", "/api/v1/jobs", "Bearer ", http.StatusUnauthorized, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if reached != tt.wantNext {
				t.Errorf("next handler reached = %v, want %v", reached, tt.wantNext)
			}
		})
	}
}
