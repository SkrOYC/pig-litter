package piglitter

import "context"

type rootGeneration struct {
	signal  context.Context
	watch   chan struct{}
	applied bool
	drained chan struct{}
}

func (o *owner) watchRoot(signal context.Context) {
	if signal == nil {
		return
	}
	o.rootMu.Lock()
	if o.rootClosed {
		o.rootMu.Unlock()
		return
	}
	if o.root != nil && o.root.signal == signal {
		o.rootMu.Unlock()
		return
	}
	if o.root != nil && o.root.watch != nil {
		close(o.root.watch)
		o.root.watch = nil
	}
	generation := &rootGeneration{signal: signal, watch: make(chan struct{})}
	o.root = generation
	o.rootWG.Add(1)
	done := generation.watch
	o.tree.mu.Lock()
	if signal.Err() == nil {
		o.tree.root.paused = false
	}
	o.tree.mu.Unlock()
	o.rootMu.Unlock()
	go func() {
		defer o.rootWG.Done()
		select {
		case <-signal.Done():
			o.drainRootAbort(generation)
		case <-done:
		case <-o.tree.ctx.Done():
		}
	}()
}

func (o *owner) settleRoot() {
	o.rootMu.Lock()
	generation := o.root
	if generation != nil && generation.watch != nil {
		close(generation.watch)
		generation.watch = nil
	}
	o.rootMu.Unlock()
	o.drainRootAbort(generation)
}

func (o *owner) drainRootAbort(generation *rootGeneration) {
	o.rootMu.Lock()
	if o.rootClosed || generation == nil || o.root != generation || generation.signal.Err() == nil {
		o.rootMu.Unlock()
		return
	}
	if generation.applied {
		done := generation.drained
		o.rootMu.Unlock()
		<-done
		return
	}
	generation.applied = true
	generation.drained = make(chan struct{})
	o.rootWG.Add(1)
	o.tree.mu.Lock()
	var pending, fresh []*run
	if !o.tree.closed {
		pending, fresh = o.tree.pauseLocked(o.tree.root)
	}
	o.tree.mu.Unlock()
	o.rootMu.Unlock()
	o.tree.joinPaused(pending, fresh)
	close(generation.drained)
	o.rootWG.Done()
}
