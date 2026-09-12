package hosthelper

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientReturnsHelperErrorWithoutEchoingRequest(t *testing.T) {
	client := &Client{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"error":"Keycloak is unavailable"}`)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}}
	err := client.do(context.Background(), http.MethodPost, "/v1/keycloak/domain",
		map[string]string{"bind_password": "top-secret"}, &struct{}{})
	if err == nil || err.Error() != "Keycloak is unavailable" {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "top-secret") {
		t.Fatal("request secret leaked into helper error")
	}
}
