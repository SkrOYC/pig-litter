package piglitter

import (
	"context"
	"strings"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type humanActionKind uint8

const (
	humanMessage humanActionKind = iota
	humanPause
	humanResume
)

type humanAction struct {
	kind       humanActionKind
	mode, text string
}

type humanResult struct {
	selected *run
	feedback string
	err      error
}

type ownerAction struct {
	ctx    context.Context
	scope  *callScope
	finish func()
}

func (o *owner) beginAction(ctx context.Context) (ownerAction, error) {
	o.state.mu.Lock()
	o.actionMu.Lock()
	if o.state.owner != o || o.state.epoch != o.epoch || o.actionsClosed {
		o.actionMu.Unlock()
		o.state.mu.Unlock()
		return ownerAction{}, reject("unavailable", "the root Session retired")
	}
	if o.actionContext == nil {
		o.actionContext, o.actionCancel = context.WithCancel(o.tree.ctx)
	}
	lifetime := o.actionContext
	o.actionWG.Add(1)
	scope := o.rootCalls
	o.actionMu.Unlock()
	o.state.mu.Unlock()
	input, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	return ownerAction{ctx: input, scope: scope, finish: func() {
		stop()
		cancel()
		o.actionWG.Done()
	}}, nil
}

func (s *extensionState) observe(ctx sdk.Context, handler func(*owner) (any, error)) (any, error) {
	s.mu.Lock()
	o := s.owner
	s.mu.Unlock()
	if o == nil {
		return nil, nil
	}
	signal := ctx.Signal()
	if signal == nil {
		signal = o.tree.ctx
	}
	action, err := o.beginAction(signal)
	if err != nil {
		return nil, nil
	}
	defer action.finish()
	return handler(o)
}

func (o *owner) humanAction(ctx context.Context, selected *run, action humanAction) <-chan humanResult {
	result := make(chan humanResult, 1)
	admitted, err := o.beginAction(ctx)
	if err != nil {
		result <- humanResult{err: err}
		return result
	}
	go func() {
		defer admitted.finish()
		result <- o.performHuman(admitted.ctx, selected, action)
	}()
	return result
}

func (o *owner) performHuman(ctx context.Context, selected *run, action humanAction) humanResult {
	failed := func(err error) humanResult { return humanResult{selected: selected, err: err} }
	if err := o.current(); err != nil {
		return failed(err)
	}
	if err := ctx.Err(); err != nil {
		return failed(err)
	}
	authority := controlAuthority{human: func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		o.state.mu.Lock()
		defer o.state.mu.Unlock()
		if o.state.owner != o || o.state.epoch != o.epoch {
			return reject("unavailable", "the root Session retired")
		}
		return nil
	}}
	o.tree.mu.Lock()
	target, err := o.tree.controlAsLocked(authority, selected)
	model := ""
	if err == nil {
		model = target.model
	}
	o.tree.mu.Unlock()
	if err != nil {
		return failed(err)
	}
	if action.kind == humanPause {
		_, _, err := o.tree.stopAs(authority, selected)
		return humanResult{selected: selected, feedback: "Paused subtree", err: err}
	}
	if len(action.text) > 16384 || strings.TrimSpace(action.text) == "" {
		return failed(reject("rejected", "enter bounded, nonempty instructions"))
	}
	if action.kind == humanMessage {
		_, _, err := o.tree.messageAs(ctx, authority, selected, action.mode, action.text)
		return humanResult{selected: selected, feedback: "Message queued", err: err}
	}
	if action.kind != humanResume {
		return failed(reject("rejected", "unknown human action"))
	}
	o.rootMu.Lock()
	generation := o.root
	o.rootMu.Unlock()
	if _, err := o.modelFor(ctx, model, o.services.ModelRuntime()); err != nil {
		return failed(err)
	}
	if _, err := o.rootToolNames(); err != nil {
		return failed(err)
	}
	o.drainRootAbort(generation)
	o.rootMu.Lock()
	defer o.rootMu.Unlock()
	if o.root != generation {
		return failed(reject("rejected", "the root run changed while preparing resume"))
	}
	accept := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if generation != nil && generation.signal.Err() != nil && !generation.applied {
			return reject("cancelled", "the root was cancelled while preparing resume")
		}
		return o.current()
	}
	if err := accept(); err != nil {
		return failed(err)
	}
	_, next, err := o.tree.messageAs(ctx, authority, selected, "resume", action.text)
	if err != nil {
		return failed(err)
	}
	if err := o.tree.activateChecked(next, accept); err != nil {
		o.tree.cancelPending(next)
		return humanResult{selected: next, err: err}
	}
	return humanResult{selected: next, feedback: "Resumed"}
}
