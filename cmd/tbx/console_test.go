package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDetachReader(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"passes plain bytes", "hello", "hello"},
		{"stops at ctrl-]", "ab\x1dcd", "ab"},
		{"ctrl-] first byte", "\x1dxyz", ""},
		{"ctrl-] at chunk boundary survives", strings.Repeat("a", 8192) + "\x1d" + "tail", strings.Repeat("a", 8192)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDetachReader(strings.NewReader(tt.input))
			got, err := io.ReadAll(r)
			if err != nil && !errors.Is(err, errDetached) {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("read %q, want %q", got, tt.want)
			}
		})
	}
	// after the detach byte the reader must report errDetached, not io.EOF
	r := newDetachReader(strings.NewReader("x\x1d"))
	_, _ = io.ReadAll(r)
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, errDetached) {
		t.Errorf("post-detach read error = %v, want errDetached", err)
	}
}
