package memberinput

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeWindow struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
	ticket  *fakeTicket
	value   any
}

func (window *fakeWindow) Admit(ctx context.Context, _ Input) (Ticket, error) {
	window.calls.Add(1)
	window.value = ctx.Value(causeKey{})
	if window.started != nil {
		close(window.started)
		<-window.release
	}
	return window.ticket, nil
}

type fakeTicket struct {
	done   chan struct{}
	closed atomic.Int64
}

func (ticket *fakeTicket) Await(ctx context.Context) (Result, error) {
	select {
	case <-ticket.done:
		return Result{Disposition: "queued"}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (ticket *fakeTicket) Close() Result {
	ticket.closed.Add(1)
	return Result{Disposition: "queued"}
}

type causeKey struct{}

func TestReplayKeepsTheSameTicketAndWindow(t *testing.T) {
	bridge := New()
	windowCtx, cancelWindow := context.WithCancel(context.Background())
	defer cancelWindow()
	ticket := &fakeTicket{done: make(chan struct{})}
	first := &fakeWindow{started: make(chan struct{}), release: make(chan struct{}), ticket: ticket}
	if err := bridge.Open(windowCtx, "window-one", first); err != nil {
		t.Fatal(err)
	}
	input := Input{Mode: "follow_up", Text: "message"}
	ctx := context.WithValue(context.Background(), causeKey{}, "callback")
	var one, two *Receipt
	var firstErr, secondErr error
	var group sync.WaitGroup
	group.Add(2)
	go func() { defer group.Done(); one, firstErr = bridge.Admit(ctx, "window-one", "transfer", input) }()
	<-first.started
	go func() { defer group.Done(); two, secondErr = bridge.Admit(ctx, "window-one", "transfer", input) }()
	if _, err := bridge.Admit(ctx, "window-one", "transfer", Input{Mode: "follow_up", Text: "different"}); err == nil {
		t.Fatal("conflicting transfer ID was accepted")
	}
	close(first.release)
	group.Wait()
	if firstErr != nil || secondErr != nil || one != two || first.calls.Load() != 1 || first.value != "callback" {
		t.Fatalf("replayed admission = %p %p, calls=%d, cause=%v, errors=%v %v", one, two, first.calls.Load(), first.value, firstErr, secondErr)
	}
	wait, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := one.Await(wait); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted wait = %v", err)
	}
	close(ticket.done)
	if result, err := two.Await(context.Background()); err != nil || result.Disposition != "queued" {
		t.Fatalf("retained result = %#v, %v", result, err)
	}
	cancelWindow()
	if err := bridge.Open(context.Background(), "window-two", &fakeWindow{ticket: &fakeTicket{done: make(chan struct{})}}); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Admit(context.Background(), "window-two", "transfer", input); err == nil {
		t.Fatal("stale transfer was retargeted to a new window")
	}
	if _, err := bridge.Admit(context.Background(), "window-one", "new-transfer", input); err == nil {
		t.Fatal("closed window accepted a new transfer")
	}
	if result := one.Close(); result.Disposition != "queued" {
		t.Fatalf("closed result = %#v", result)
	}
	two.Close()
	bridge.Close()
	if ticket.closed.Load() != 1 {
		t.Fatalf("ticket closed %d times", ticket.closed.Load())
	}
}

func TestCloseDuringAdmissionReleasesTheRetainedTicket(t *testing.T) {
	bridge := New()
	ticket := &fakeTicket{done: make(chan struct{})}
	window := &fakeWindow{started: make(chan struct{}), release: make(chan struct{}), ticket: ticket}
	if err := bridge.Open(context.Background(), "window", window); err != nil {
		t.Fatal(err)
	}
	done := make(chan *Receipt, 1)
	go func() {
		receipt, _ := bridge.Admit(context.Background(), "window", "transfer", Input{Mode: "steer", Text: "message"})
		done <- receipt
	}()
	<-window.started
	bridge.Close()
	close(window.release)
	receipt := <-done
	if result, err := receipt.Await(context.Background()); err != nil || result.Disposition != "queued" || ticket.closed.Load() != 1 {
		t.Fatalf("closed admission = %#v, closes=%d, %v", result, ticket.closed.Load(), err)
	}
}
