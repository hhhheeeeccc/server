package nova_sms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestTriggerNovaContract(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/events/trigger" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "ApiKey test-key" {
			t.Error("wrong authorization header")
		}
		var body struct {
			Name string `json:"name"`
			To   struct {
				SubscriberID string `json:"subscriberId"`
				Phone        string `json:"phone"`
			} `json:"to"`
			Payload       map[string]string `json:"payload"`
			TransactionID string            `json:"transactionId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Name != "urgent-sms" || body.To.SubscriberID != "user" || body.To.Phone != "+967737328812" || body.Payload["message"] != "عاجل" || body.Payload["phone"] != body.To.Phone || body.TransactionID != "urgent-post-user" {
			t.Errorf("unexpected Nova request: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	config := deliverySettings{endpoint: srv.URL, workflow: "urgent-sms", secret: "test-key"}
	if err := triggerNova(context.Background(), config, delivery{postID: "post", userID: "user"}, "عاجل", "+967737328812"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("expected one request, got %d", requests)
	}
}

func TestTriggerNovaRejectsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	err := triggerNova(context.Background(), deliverySettings{endpoint: srv.URL, workflow: "urgent-sms", secret: "test-key"}, delivery{postID: "post", userID: "user"}, "message", "+967737328812")
	if err == nil {
		t.Fatal("provider rejection must be retried")
	}
}

func TestTriggerNovaDoesNotForwardSMSOnRedirect(t *testing.T) {
	var forwarded atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer destination.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer provider.Close()
	err := triggerNova(context.Background(), deliverySettings{endpoint: provider.URL, workflow: "urgent-sms", secret: "test-key"},
		delivery{postID: "post", userID: "user"}, "private message", "+967737328812")
	if err == nil {
		t.Fatal("a provider redirect must be treated as a failed delivery")
	}
	if forwarded.Load() != 0 {
		t.Fatal("the message and phone must not reach the redirect destination")
	}
}
