package client

import "testing"

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{100 * 1024, "100.00 KB"},
		{1024 * 1024, "1.00 MB"},
		{5*1024*1024 + 512*1024, "5.50 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
		{3 * 1024 * 1024 * 1024, "3.00 GB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
