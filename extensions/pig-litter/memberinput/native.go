//go:build teammate_native

package memberinput

import (
	"context"
	"errors"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

type NativeWindow struct {
	Value *coding.InputWindow
}

func (window NativeWindow) Admit(ctx context.Context, input Input) (Ticket, error) {
	if window.Value == nil {
		return nil, errors.New("native input window is unavailable")
	}
	var mode extension.DeliverAs
	switch input.Mode {
	case Steer:
		mode = extension.DeliverAsSteer
	case FollowUp:
		mode = extension.DeliverAsFollowUp
	default:
		return nil, errors.New("invalid native input mode")
	}
	ticket, err := window.Value.Admit(ctx, coding.AdmissionInput{Text: input.Text, DeliverAs: mode})
	if err != nil {
		return nil, err
	}
	return nativeTicket{value: ticket}, nil
}

type nativeTicket struct {
	value *coding.InputTicket
}

func (ticket nativeTicket) Await(ctx context.Context) (Result, error) {
	result, err := ticket.value.Await(ctx)
	return nativeResult(result), err
}

func (ticket nativeTicket) Close() Result {
	return nativeResult(ticket.value.Close())
}

func nativeResult(result coding.InputResult) Result {
	value := Result{}
	switch result.Disposition {
	case coding.InputQueued:
		value.Disposition = Queued
	case coding.InputHandled:
		value.Disposition = Handled
	case coding.InputCancelled:
		value.Disposition = Cancelled
	case coding.InputStale:
		value.Disposition = Stale
	case coding.InputUnavailable:
		value.Disposition = Unavailable
	case coding.InputFailed:
		value.Disposition = Failed
	default:
		value.Disposition = Unavailable
	}
	if result.Err != nil {
		value.Error = result.Err.Error()
	}
	return value
}
