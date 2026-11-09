// Package pyfloat reproduces the Python float operations whose results land
// in the ledger or in model-visible payloads, so Go writes the same numbers.
package pyfloat

import (
	"math"
	"strconv"
)

// Round is Python's round(x, n): half-to-even on the exact binary value
// (round(2.675, 2) == 2.67). strconv rounds the exact decimal expansion the
// same way.
func Round(x float64, n int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return v
}

// RoundPtr rounds a nullable value (Python's `round(x, n) if x is not None`).
func RoundPtr(x *float64, n int) *float64 {
	if x == nil {
		return nil
	}
	v := Round(*x, n)
	return &v
}

// Sum is CPython ≥ 3.12 sum() over floats: Neumaier-compensated.
func Sum(xs []float64) float64 {
	var s, c float64
	for _, x := range xs {
		t := s + x
		if math.Abs(s) >= math.Abs(x) {
			c += (s - t) + x
		} else {
			c += (x - t) + s
		}
		s = t
	}
	return s + c
}
