//go:build !linux

package rtl2832u

import (
	"errors"
	"testing"
)

func TestOpenBackendUnsupportedPlatform(t *testing.T) {
	t.Parallel()

	got, err := openBackend(defaultConfig())
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("err = %v, want ErrUnsupportedPlatform", err)
	}

	if got != nil {
		t.Errorf("backend = %v, want nil", got)
	}
}
