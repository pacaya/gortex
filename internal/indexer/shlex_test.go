package indexer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShlexSplit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "quoted include dir with embedded space",
			in:   `clang -I"a b/inc" -isystem /x -c f.c`,
			want: []string{"clang", "-Ia b/inc", "-isystem", "/x", "-c", "f.c"},
		},
		{
			name: "single quotes are literal",
			in:   `cc -D'NAME=a b' main.c`,
			want: []string{"cc", "-DNAME=a b", "main.c"},
		},
		{
			name: "backslash-escaped space outside quotes",
			in:   `cc -I/a\ b -c f.c`,
			want: []string{"cc", "-I/a b", "-c", "f.c"},
		},
		{
			name: "escaped quote inside double quotes",
			in:   `cc -DS="a\"b" f.c`,
			want: []string{"cc", `-DS=a"b`, "f.c"},
		},
		{
			name: "collapses runs of whitespace",
			in:   "  cc \t  -c   f.c  ",
			want: []string{"cc", "-c", "f.c"},
		},
		{
			name: "empty",
			in:   "   ",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shlexSplit(tc.in))
		})
	}
}
