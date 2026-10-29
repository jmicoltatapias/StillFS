package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type healthStub struct{ err error }

func (s healthStub) HealthCheck(context.Context) error { return s.err }

func TestReadinessReportsDatabaseState(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{{"ready", nil, http.StatusOK}, {"database unavailable", errors.New("offline"), http.StatusServiceUnavailable}} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewHandler(healthStub{err: tt.err}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.want)
			}
		})
	}
}

func TestUnknownRouteIsNotImplemented(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(healthStub{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}
