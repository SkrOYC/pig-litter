//go:build linux || darwin

package piglitter

import (
	"errors"
	"time"
)

type localProcessWatch interface {
	exited() (bool, error)
	wait(stop <-chan struct{}, expires time.Time) bool
	close() error
}

func watchLocalProcess(identity localProcessIdentity) (localProcessWatch, error) {
	return armLocalProcessWatch(identity, observeLocalProcess, openLocalProcessWatch)
}

func armLocalProcessWatch(identity localProcessIdentity, observe func(int) (localProcessIdentity, error), open func(int) (localProcessWatch, error)) (localProcessWatch, error) {
	before, err := observe(identity.PID)
	if err != nil || before != identity {
		return nil, errors.New("native host process identity is unavailable")
	}
	watch, err := open(identity.PID)
	if err != nil {
		return nil, err
	}
	if !confirmLocalWatch(identity, watch, observe) {
		return nil, errors.Join(errors.New("native host changed while arming exit observation"), watch.close())
	}
	return watch, nil
}

func confirmLocalWatch(identity localProcessIdentity, watch localProcessWatch, observe func(int) (localProcessIdentity, error)) bool {
	current, err := observe(identity.PID)
	if err != nil || current != identity {
		return false
	}
	exited, err := watch.exited()
	return err == nil && !exited
}
