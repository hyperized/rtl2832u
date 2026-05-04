//go:build linux

package rtl2832u

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestOpenBackendNoDevicesEmptySysfs(t *testing.T) {
	t.Parallel()

	got, err := openBackendWithSysfs(defaultConfig(), t.TempDir())
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("err = %v, want ErrNoDevice", err)
	}

	if got != nil {
		t.Errorf("backend = %v, want nil", got)
	}
}

func TestOpenBackendMissingSysfsTreatedAsNoDevice(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent")

	_, err := openBackendWithSysfs(defaultConfig(), missing)
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("err = %v, want ErrNoDevice (wrapping fs.ErrNotExist)", err)
	}
	// The wrap must preserve fs.ErrNotExist so callers can diagnose
	// missing /sys mounts (containers, chroots) without having to parse
	// the error string.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, expected to wrap fs.ErrNotExist", err)
	}
}

func TestOpenBackendIndexOutOfRange(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fakeDevice(t, root, "1-2", "0bda", "2838", "5")

	cfg := defaultConfig()
	cfg.deviceIndex = 7

	_, err := openBackendWithSysfs(cfg, root)
	if !errors.Is(err, ErrNoDevice) {
		t.Fatalf("err = %v, want ErrNoDevice", err)
	}
}

func TestOpenBackendOpenFailsWithoutHardware(t *testing.T) {
	t.Parallel()

	// The fake sysfs node points at a /dev path the kernel does not
	// actually expose, so OpenFile (or, on hosts where the path happens
	// to exist, claimInterface) must fail. This exercises the
	// error-wrapping path without requiring a real RTL-SDR.
	root := t.TempDir()
	fakeDevice(t, root, "1-2", "0bda", "2838", "5")

	_, err := openBackendWithSysfs(defaultConfig(), root)
	if err == nil {
		t.Fatal("expected open error for fabricated /dev path, got nil")
	}

	if errors.Is(err, ErrNoDevice) || errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("err = %v, want a wrapped open error, not a sentinel", err)
	}
}

func TestLinuxBackendCloseIdempotent(t *testing.T) {
	t.Parallel()

	// A backend with closed=true short-circuits inside Close, so the
	// release/close ioctls are never invoked even though dev is nil.
	// The test asserts both that the first call succeeds and that a
	// second call still does — the contract for idempotent Close.
	back := &linuxBackend{closed: true}

	if err := back.Close(); err != nil {
		t.Fatalf("first Close on already-closed backend = %v, want nil", err)
	}

	if err := back.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestLinuxBackendReadHonoursContext(t *testing.T) {
	t.Parallel()

	back := &linuxBackend{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count, err := back.Read(ctx, make([]byte, 16))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	if count != 0 {
		t.Errorf("n = %d, want 0", count)
	}
}

// TestLinuxBackendReadStartFailureSurfaces uses a pipe-backed
// backend to exercise the start-stream failure path: the URB
// submit ioctl on a non-USB fd fails, ensureStream records the
// error, and Read returns it without panicking.
func TestLinuxBackendReadStartFailureSurfaces(t *testing.T) {
	t.Parallel()

	back := linuxBackendOnPipe(t)

	count, err := back.Read(context.Background(), make([]byte, 16))
	if err == nil {
		t.Fatal("expected start-stream error on pipe-backed backend, got nil")
	}

	if count != 0 {
		t.Errorf("n = %d, want 0", count)
	}
}
