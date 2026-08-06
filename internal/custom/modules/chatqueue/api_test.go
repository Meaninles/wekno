package chatqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type admissionAPITestHarness struct {
	manager *Manager
	policy  atomic.Pointer[queuePolicy]
	tickets sync.Map
}

func newAdmissionAPITestHarness() *admissionAPITestHarness {
	h := &admissionAPITestHarness{manager: newLocalTestManager()}
	h.setPolicy(2, 1)
	return h
}

func (h *admissionAPITestHarness) setPolicy(concurrent, waiting int) {
	h.policy.Store(&queuePolicy{
		Surface: SurfaceIM, Enabled: true, MaxConcurrent: concurrent,
		MaxWaiting: waiting, MaxPerUser: 10,
	})
}

func (h *admissionAPITestHarness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/policy":
		var body struct {
			Concurrent int `json:"concurrent"`
			Waiting    int `json:"waiting"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Concurrent < 1 || body.Waiting < 0 {
			http.Error(w, "invalid policy", http.StatusBadRequest)
			return
		}
		h.setPolicy(body.Concurrent, body.Waiting)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/admit":
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		ticket := newLocalSurfaceTicket(h.manager, body.ID, body.ID, SurfaceIM)
		policy := *h.policy.Load()
		result, err := h.manager.admitLocal(ticket, policy)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if result.code < 0 {
			http.Error(w, "queue full", http.StatusTooManyRequests)
			return
		}
		if result.code == 0 {
			ticket.queued = true
			for {
				select {
				case <-r.Context().Done():
					ticket.Cancel(context.Background())
					return
				case <-time.After(20 * time.Millisecond):
				}
				policy = *h.policy.Load()
				promoted, promoteErr := h.manager.promoteLocal(ticket, policy)
				if promoteErr != nil {
					http.Error(w, promoteErr.Error(), http.StatusServiceUnavailable)
					return
				}
				if promoted.code == 1 {
					break
				}
			}
		}
		ticket.admitted.Store(true)
		h.tickets.Store(body.ID, ticket)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": body.ID, "surface": SurfaceIM})
	case r.Method == http.MethodDelete:
		id := r.URL.Path[len("/admit/"):]
		value, ok := h.tickets.LoadAndDelete(id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		value.(*Ticket).Release(r.Context())
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func TestIMAdmissionAPIConcurrencyAndHotAdjustment(t *testing.T) {
	harness := newAdmissionAPITestHarness()
	server := httptest.NewServer(harness)
	defer server.Close()
	client := server.Client()

	setPolicy := func(concurrent, waiting int) {
		t.Helper()
		body := fmt.Sprintf(`{"concurrent":%d,"waiting":%d}`, concurrent, waiting)
		request, _ := http.NewRequest(http.MethodPut, server.URL+"/policy", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil || response.StatusCode != http.StatusNoContent {
			t.Fatalf("set policy: response=%v err=%v", response, err)
		}
		_ = response.Body.Close()
	}
	admitAsync := func(id string) <-chan int {
		result := make(chan int, 1)
		go func() {
			response, err := client.Post(server.URL+"/admit", "application/json", bytes.NewBufferString(`{"id":"`+id+`"}`))
			if err != nil {
				result <- 0
				return
			}
			_ = response.Body.Close()
			result <- response.StatusCode
		}()
		return result
	}
	wantNow := func(result <-chan int, expected int, label string) {
		t.Helper()
		select {
		case status := <-result:
			if status != expected {
				t.Fatalf("%s status=%d want=%d", label, status, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not complete", label)
		}
	}
	wantWaiting := func(result <-chan int, label string) {
		t.Helper()
		select {
		case status := <-result:
			t.Fatalf("%s completed early with status %d", label, status)
		case <-time.After(120 * time.Millisecond):
		}
	}
	release := func(id string) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodDelete, server.URL+"/admit/"+id, nil)
		response, err := client.Do(request)
		if err != nil || response.StatusCode != http.StatusNoContent {
			t.Fatalf("release %s: response=%v err=%v", id, response, err)
		}
		_ = response.Body.Close()
	}

	// Below and exactly at the configured concurrency both enter immediately.
	setPolicy(2, 1)
	wantNow(admitAsync("exact-a"), http.StatusOK, "below limit")
	wantNow(admitAsync("exact-b"), http.StatusOK, "exact limit")

	// One over-limit request waits; another exceeds max_waiting and is rejected.
	over := admitAsync("over-wait")
	wantWaiting(over, "over-limit waiter")
	wantNow(admitAsync("over-reject"), http.StatusTooManyRequests, "waiting overflow")
	release("exact-a")
	wantNow(over, http.StatusOK, "promoted waiter")
	release("exact-b")
	release("over-wait")

	// Lowering 2 -> 1 never interrupts either active request. A waiter enters
	// only after the active count naturally drains below the new limit.
	setPolicy(2, 2)
	wantNow(admitAsync("lower-a"), http.StatusOK, "lower active a")
	wantNow(admitAsync("lower-b"), http.StatusOK, "lower active b")
	setPolicy(1, 2)
	loweredWaiter := admitAsync("lower-wait")
	wantWaiting(loweredWaiter, "lowered waiter before release")
	release("lower-a")
	wantWaiting(loweredWaiter, "lowered waiter while one active remains")
	release("lower-b")
	wantNow(loweredWaiter, http.StatusOK, "lowered waiter after natural drain")
	release("lower-wait")

	// Raising capacity takes effect for an already-waiting request without
	// cancelling the active conversation.
	setPolicy(1, 2)
	wantNow(admitAsync("raise-a"), http.StatusOK, "raise active")
	raisedWaiter := admitAsync("raise-wait")
	wantWaiting(raisedWaiter, "raise waiter before update")
	setPolicy(2, 2)
	wantNow(raisedWaiter, http.StatusOK, "raise waiter after update")
	release("raise-a")
	release("raise-wait")
}
