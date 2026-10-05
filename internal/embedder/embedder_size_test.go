package embedder

import "testing"

func TestResolveEmbedContextSize(t *testing.T) {
	cases := []struct{ in, want int }{
		{8192, 8192},
		{512, 512},
		{0, 0},
		{-1, 0},
	}
	for _, tc := range cases {
		if got := resolveEmbedContextSize(tc.in); got != tc.want {
			t.Errorf("resolveEmbedContextSize(%d)=%d want %d", tc.in, got, tc.want)
		}
	}
}
