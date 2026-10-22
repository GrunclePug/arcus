package cli

import (
	"slices"
	"testing"
)

func TestLineDiff(t *testing.T) {
	tests := []struct {
		name, a, b string
		want       []string
	}{
		{"changed line", "keep\nversion 1\nend", "keep\nversion 2\nend", []string{"- version 1", "+ version 2"}},
		{"added", "a\nb", "a\nnew\nb", []string{"+ new"}},
		{"removed", "a\nold\nb", "a\nb", []string{"- old"}},
		{"same", "a\nb", "a\nb", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lineDiff(tt.a, tt.b); !slices.Equal(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
