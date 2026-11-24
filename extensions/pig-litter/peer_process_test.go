//go:build linux || darwin

package piglitter

import (
	"errors"
	"testing"
	"time"
)

type testLocalProcessWatch struct {
	exit   bool
	closed bool
}

func (w *testLocalProcessWatch) exited() (bool, error)                { return w.exit, nil }
func (w *testLocalProcessWatch) wait(<-chan struct{}, time.Time) bool { return w.exit }
func (w *testLocalProcessWatch) close() error                         { w.closed = true; return nil }

func TestLocalProcessWatchRejectsReuseScopeAndExitDuringArming(t *testing.T) {
	identity := localProcessIdentity{PID: 42, Birth: "birth-one", Scope: "scope-one"}
	for _, test := range []struct {
		name   string
		before localProcessIdentity
		after  localProcessIdentity
		exit   bool
		armed  bool
	}{
		{"same process", identity, identity, false, true},
		{"reused before open", localProcessIdentity{PID: 42, Birth: "birth-two", Scope: "scope-one"}, identity, false, false},
		{"reused after open", identity, localProcessIdentity{PID: 42, Birth: "birth-two", Scope: "scope-one"}, false, false},
		{"scope changed", identity, localProcessIdentity{PID: 42, Birth: "birth-one", Scope: "scope-two"}, false, false},
		{"exited after open", identity, identity, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			watch := &testLocalProcessWatch{exit: test.exit}
			reads := 0
			observe := func(int) (localProcessIdentity, error) {
				reads++
				if reads == 1 {
					return test.before, nil
				}
				return test.after, nil
			}
			opened := false
			got, err := armLocalProcessWatch(identity, observe, func(int) (localProcessWatch, error) {
				opened = true
				return watch, nil
			})
			if test.armed {
				if err != nil || got == nil || watch.closed {
					t.Fatalf("healthy watch = %v, %v, closed %t", got, err, watch.closed)
				}
				watch.exit = true
				if confirmLocalWatch(identity, watch, observe) {
					t.Fatal("watch accepted an exit before acknowledgment")
				}
				return
			}
			if err == nil || got != nil || opened && !watch.closed {
				t.Fatalf("unproven watch = %v, %v, opened %t, closed %t", got, err, opened, watch.closed)
			}
		})
	}
	if watch, err := armLocalProcessWatch(identity, func(int) (localProcessIdentity, error) {
		return localProcessIdentity{}, errors.New("unavailable")
	}, func(int) (localProcessWatch, error) { return &testLocalProcessWatch{}, nil }); err == nil || watch != nil {
		t.Fatalf("unobservable process = %v, %v", watch, err)
	}
}
