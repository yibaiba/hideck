package modemvoice

import (
	"errors"
	"fmt"
)

// Select from a fresh CLCC snapshot; CHUP/ATA are only safe for a single call.
func controlCommand(calls []Call, expected Call, answer bool) (string, error) {
	calls = voiceControlCalls(calls)
	target, err := expectedControlCall(calls, expected)
	if err != nil {
		return "", err
	}
	if len(calls) == 1 {
		return singleControlCommand(*target, answer)
	}
	return targetedControlCommand(calls, *target, answer)
}

func targetedControlCommand(calls []Call, target Call, answer bool) (string, error) {
	if target.State == Active || target.State == Held {
		if !answer {
			return fmt.Sprintf("AT+CHLD=1%d", target.Index), nil
		}
	}
	if target.Inbound && target.State == Waiting && singleWaitingCall(calls) {
		if answer {
			return "AT+CHLD=2", nil
		}
		return "AT+CHLD=0", nil
	}
	return "", errors.New("modem voice: call state does not permit targeted control")
}

func singleWaitingCall(calls []Call) bool {
	waiting := 0
	for _, call := range calls {
		switch call.State {
		case Waiting:
			waiting++
		case Active, Held:
		default:
			return false
		}
	}
	return waiting == 1
}

func expectedControlCall(calls []Call, expected Call) (*Call, error) {
	var target *Call
	for i := range calls {
		if calls[i].Mode != 0 || calls[i].Multiparty {
			return nil, errors.New("modem voice: unsupported call composition")
		}
		if sameCall(expected, calls[i]) {
			target = &calls[i]
		}
	}
	if target == nil {
		return nil, ErrCallChanged
	}
	return target, nil
}

func singleControlCommand(target Call, answer bool) (string, error) {
	if !answer {
		return "AT+CHUP", nil
	}
	if target.Inbound && (target.State == Incoming || target.State == Waiting) {
		return "ATA", nil
	}
	return "", errors.New("modem voice: call is not an incoming call")
}
