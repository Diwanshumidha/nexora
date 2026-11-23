package services

import "testing"

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.2", 1}, {"0.1.0", "0.1.0", 0}, {"v1.0", "1.0.0", 0},
		{"1.0.0-beta", "1.0.0", -1}, {"0.2.0", "0.10.0", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
