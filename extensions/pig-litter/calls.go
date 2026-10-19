package piglitter

import "sync"

type callScope struct {
	mu     sync.Mutex
	calls  map[string]*callTicket
	closed bool
}

type callTicket struct {
	scope     *callScope
	caller    *run
	callID    string
	operation string
}

func newCallScope() *callScope {
	return &callScope{calls: map[string]*callTicket{}}
}

func (s *callScope) begin(caller *run, callID, operation string) (*callTicket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || callID == "" {
		return nil, reject("rejected", "the tool call has no active turn")
	}
	if s.calls[callID] != nil {
		return nil, reject("rejected", "duplicate tool call ID in the current turn")
	}
	ticket := &callTicket{scope: s, caller: caller, callID: callID, operation: operation}
	s.calls[callID] = ticket
	return ticket, nil
}

func (s *callScope) capture(callID, operation string) *callTicket {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ticket := s.calls[callID]
	if ticket == nil || ticket.operation != operation {
		return nil
	}
	return ticket
}

func (s *callScope) forget(ticket *callTicket) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.calls[ticket.callID] == ticket {
		delete(s.calls, ticket.callID)
	}
	s.mu.Unlock()
}

func (o *owner) callScope(caller *run) *callScope {
	if caller == nil {
		o.state.mu.Lock()
		defer o.state.mu.Unlock()
		if o.rootCalls == nil {
			o.rootCalls = newCallScope()
			if o.state.owner != o || o.state.epoch != o.epoch {
				o.rootCalls.closed = true
			}
		}
		return o.rootCalls
	}
	caller.mu.Lock()
	defer caller.mu.Unlock()
	return caller.calls
}

func (o *owner) retireScope(scope *callScope) {
	if scope == nil {
		return
	}
	scope.mu.Lock()
	scope.closed = true
	tickets := scope.calls
	scope.calls = map[string]*callTicket{}
	scope.mu.Unlock()
	for _, ticket := range tickets {
		o.settleAck(ticket, true)
		o.settleOutcomeAck(ticket, false)
	}
}
