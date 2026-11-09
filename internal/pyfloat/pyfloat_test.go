package pyfloat

import "testing"

func TestRound(t *testing.T) {
	for _, c := range []struct {
		x    float64
		n    int
		want float64
	}{{2.675, 2, 2.67}, {0.125, 2, 0.12}, {2.5, 0, 2}, {-1.5, 0, -2}, {1.00005, 4, 1.0001}, {123.45678, 4, 123.4568}} {
		if got := Round(c.x, c.n); got != c.want {
			t.Errorf("Round(%v,%d) = %v, want %v", c.x, c.n, got, c.want)
		}
	}
	if Sum([]float64{1, 1e100, 1, -1e100}) != 2 {
		t.Error("Sum not compensated")
	}
}
