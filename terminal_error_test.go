package main

import (
	"errors"
	"strings"
	"testing"

	"serval/locales"
)

func TestFriendlyTerminalError(t *testing.T) {
	if got := friendlyTerminalError(nil); got != "" {
		t.Fatalf("friendlyTerminalError(nil) = %q, want empty", got)
	}

	winErr := errors.New("terminal: ghostty-vt.dll is missing from the app: build it with mygo build")
	gotWin := friendlyTerminalError(winErr)
	wantWin := locales.AppTerminalMissingLibrary("ghostty-vt.dll")
	if gotWin != wantWin {
		t.Fatalf("friendlyTerminalError(winErr) = %q, want %q", gotWin, wantWin)
	}
	if !strings.Contains(gotWin, "ghostty-vt.dll") {
		t.Fatalf("expected dll name in %q", gotWin)
	}
	if strings.Contains(gotWin, "build it with mygo build") {
		t.Fatalf("unexpected raw hint in %q", gotWin)
	}

	macErr := errors.New("ffi: libghostty-vt.dylib is missing from the app: build it with mygo build")
	if gotMac, wantMac := friendlyTerminalError(macErr), locales.AppTerminalMissingLibrary("libghostty-vt.dylib"); gotMac != wantMac {
		t.Fatalf("friendlyTerminalError(macErr) = %q, want %q", gotMac, wantMac)
	}

	other := errors.New("pty failed")
	if got := friendlyTerminalError(other); got != "pty failed" {
		t.Fatalf("friendlyTerminalError(other) = %q, want %q", got, "pty failed")
	}
}
