//go:build linux

package volte

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIsALSAXrunUnwrapsPathError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"bare EPIPE", unix.EPIPE, true},
		{"bare ESTRPIPE", unix.ESTRPIPE, true},
		{"PathError EPIPE", &os.PathError{Op: "write", Path: "/dev/snd/pcmC3D0p", Err: unix.EPIPE}, true},
		{"wrapped PathError", fmt.Errorf("volte: %w", &os.PathError{Op: "write", Path: "/dev/snd/pcmC3D0p", Err: unix.EPIPE}), true},
		{"ENODEV not xrun", &os.PathError{Op: "write", Path: "/dev/snd/pcmC3D0p", Err: unix.ENODEV}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isALSAXrun(tc.err); got != tc.want {
			t.Errorf("%s: isALSAXrun(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
