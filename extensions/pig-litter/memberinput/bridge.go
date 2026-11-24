package memberinput

import (
	"context"
	"errors"
	"sync"
)

type Input struct {
	Mode Mode
	Text string
}

type Mode string
type WindowID string
type TransferID string
type Disposition string

const (
	Steer    Mode = "steer"
	FollowUp Mode = "follow_up"
)

const (
	Queued      Disposition = "queued"
	Handled     Disposition = "handled"
	Cancelled   Disposition = "cancelled"
	Stale       Disposition = "stale"
	Unavailable Disposition = "unavailable"
	Failed      Disposition = "failed"
)

type Result struct {
	Disposition Disposition
	Error       string
}

type Window interface {
	Admit(context.Context, Input) (Ticket, error)
}

type Ticket interface {
	Await(context.Context) (Result, error)
	Close() Result
}

type Bridge struct {
	mu       sync.Mutex
	windows  map[WindowID]*window
	receipts map[TransferID]*Receipt
	closed   bool
}

type window struct {
	value Window
	ctx   context.Context
}

type Receipt struct {
	windowID WindowID
	input    Input
	ready    chan struct{}
	mu       sync.Mutex
	cancel   context.CancelFunc
	ticket   Ticket
	err      error
	closed   bool
	result   Result
	release  sync.Once
}

func New() *Bridge {
	return &Bridge{windows: map[WindowID]*window{}, receipts: map[TransferID]*Receipt{}}
}

func (bridge *Bridge) Open(ctx context.Context, id WindowID, value Window) error {
	if ctx == nil || id == "" || len(id) > 128 || value == nil {
		return errors.New("invalid native input window")
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.closed || ctx.Err() != nil {
		return errors.New("native input bridge is closed")
	}
	if bridge.windows[id] != nil {
		return errors.New("native input window ID is already in use")
	}
	bridge.windows[id] = &window{value: value, ctx: ctx}
	return nil
}

func (bridge *Bridge) Admit(ctx context.Context, windowID WindowID, id TransferID, input Input) (*Receipt, error) {
	if ctx == nil || id == "" || len(id) > 128 || windowID == "" || len(windowID) > 128 || input.Mode != Steer && input.Mode != FollowUp || len(input.Text) == 0 || len(input.Text) > 16384 {
		return nil, errors.New("invalid native input request")
	}
	bridge.mu.Lock()
	if previous := bridge.receipts[id]; previous != nil {
		bridge.mu.Unlock()
		if previous.windowID != windowID || previous.input != input {
			return nil, errors.New("native input request ID conflicts with its first use")
		}
		select {
		case <-previous.ready:
			return previous, previous.admissionError()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	selected := bridge.windows[windowID]
	if bridge.closed || selected == nil || selected.ctx.Err() != nil {
		bridge.mu.Unlock()
		return nil, errors.New("native input window is unavailable")
	}
	admitCtx, cancel := context.WithCancel(ctx)
	receipt := &Receipt{windowID: windowID, input: input, ready: make(chan struct{}), cancel: cancel}
	bridge.receipts[id] = receipt
	bridge.mu.Unlock()
	ticket, err := selected.value.Admit(admitCtx, input)
	if ticket == nil && err == nil {
		err = errors.New("native input owner returned no ticket")
	}
	if err != nil {
		cancel()
	}
	receipt.mu.Lock()
	receipt.ticket, receipt.err = ticket, err
	closed := receipt.closed
	close(receipt.ready)
	receipt.mu.Unlock()
	if closed {
		receipt.releaseTicket()
	}
	return receipt, err
}

func (receipt *Receipt) admissionError() error {
	receipt.mu.Lock()
	defer receipt.mu.Unlock()
	return receipt.err
}

func (receipt *Receipt) Await(ctx context.Context) (Result, error) {
	if receipt == nil || ctx == nil {
		return Result{}, errors.New("invalid native input receipt")
	}
	select {
	case <-receipt.ready:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	receipt.mu.Lock()
	if receipt.err != nil {
		err := receipt.err
		receipt.mu.Unlock()
		return Result{}, err
	}
	if receipt.closed {
		receipt.mu.Unlock()
		receipt.releaseTicket()
		receipt.mu.Lock()
		result := receipt.result
		receipt.mu.Unlock()
		return result, nil
	}
	ticket := receipt.ticket
	receipt.mu.Unlock()
	return ticket.Await(ctx)
}

func (receipt *Receipt) Close() Result {
	if receipt == nil {
		return Result{Disposition: Unavailable}
	}
	receipt.mu.Lock()
	receipt.closed = true
	cancel := receipt.cancel
	ready := false
	select {
	case <-receipt.ready:
		ready = true
	default:
	}
	receipt.mu.Unlock()
	cancel()
	if ready {
		receipt.releaseTicket()
	}
	receipt.mu.Lock()
	defer receipt.mu.Unlock()
	return receipt.result
}

func (receipt *Receipt) releaseTicket() {
	receipt.release.Do(func() {
		receipt.mu.Lock()
		ticket := receipt.ticket
		receipt.mu.Unlock()
		if ticket != nil {
			result := ticket.Close()
			receipt.mu.Lock()
			receipt.result = result
			receipt.mu.Unlock()
		}
	})
}

func (bridge *Bridge) Close() {
	bridge.mu.Lock()
	if bridge.closed {
		bridge.mu.Unlock()
		return
	}
	bridge.closed = true
	receipts := make([]*Receipt, 0, len(bridge.receipts))
	for _, receipt := range bridge.receipts {
		receipts = append(receipts, receipt)
	}
	bridge.mu.Unlock()
	for _, receipt := range receipts {
		receipt.Close()
	}
}
