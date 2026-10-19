// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"math"
	"runtime"
	"sort"
	"sync"
)

// matrix is a dense row-major float32 matrix: the embedding table as loaded.
type matrix struct {
	rows, cols int
	data       []float32
}

func (m matrix) row(i int) []float32 { return m.data[i*m.cols : (i+1)*m.cols] }

// principalAxes returns the top k principal axes of m's rows: eigenvectors of
// the mean-centered covariance (accumulated in float64), by eigenvalue
// descending. Each axis is sign-fixed so its largest-magnitude component is
// positive, which makes the derived table independent of the solver's sign
// choices.
func principalAxes(m matrix, k int) ([][]float64, error) {
	if k <= 0 || k > m.cols || m.rows == 0 {
		return nil, errors.New("pca: bad dimensions")
	}
	vals, vecs, err := symEigen(covariance(m), m.cols)
	if err != nil {
		return nil, err
	}
	order := make([]int, m.cols)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return vals[order[a]] > vals[order[b]] })
	axes := make([][]float64, k)
	for a := range axes {
		col := order[a]
		ax := make([]float64, m.cols)
		best := 0
		for i := range ax {
			ax[i] = vecs[i*m.cols+col]
			if math.Abs(ax[i]) > math.Abs(ax[best]) {
				best = i
			}
		}
		if ax[best] < 0 {
			for i := range ax {
				ax[i] = -ax[i]
			}
		}
		axes[a] = ax
	}
	return axes, nil
}

// covariance returns sum_i (x_i - mean)(x_i - mean)^T as a dense d*d
// row-major matrix (unscaled: scaling does not change the eigenvectors).
func covariance(m matrix) []float64 {
	d := m.cols
	mean := make([]float64, d)
	for i := range m.rows {
		for j, v := range m.row(i) {
			mean[j] += float64(v)
		}
	}
	for j := range mean {
		mean[j] /= float64(m.rows)
	}
	parts := parallelRows(m.rows, func(lo, hi int) []float64 {
		c := make([]float64, d*d)
		x := make([]float64, d)
		for i := lo; i < hi; i++ {
			for j, v := range m.row(i) {
				x[j] = float64(v) - mean[j]
			}
			for a := range d { // upper triangle
				xa, ca := x[a], c[a*d:(a+1)*d]
				for b := a; b < d; b++ {
					ca[b] += xa * x[b]
				}
			}
		}
		return c
	})
	cov := make([]float64, d*d)
	for _, p := range parts { // fixed order: the sum is deterministic
		for i, v := range p {
			cov[i] += v
		}
	}
	for a := range d {
		for b := a + 1; b < d; b++ {
			cov[b*d+a] = cov[a*d+b]
		}
	}
	return cov
}

// project maps every RAW (uncentered) row of m onto axes. Projecting raw rows
// is what the evaluated recipe did; centering would only shift every row by
// the same vector.
func project(m matrix, axes [][]float64) matrix {
	k := len(axes)
	out := matrix{rows: m.rows, cols: k, data: make([]float32, m.rows*k)}
	parallelRows(m.rows, func(lo, hi int) struct{} {
		for i := lo; i < hi; i++ {
			x, y := m.row(i), out.row(i)
			for a, ax := range axes {
				var s float64
				for j, v := range x {
					s += float64(v) * ax[j]
				}
				y[a] = float32(s)
			}
		}
		return struct{}{}
	})
	return out
}

// quantize is symmetric per-row int8: scale = max|x|/127 (1 for an all-zero
// row), q = x/scale rounded half to even and clipped to [-127, 127]. Done in
// float32, like the reference.
func quantize(m matrix) (q []int8, scales []float32) {
	q = make([]int8, len(m.data))
	scales = make([]float32, m.rows)
	for i := range m.rows {
		x := m.row(i)
		var hi float32
		for _, v := range x {
			hi = max(hi, float32(math.Abs(float64(v))))
		}
		s := hi / 127
		if s == 0 {
			s = 1
		}
		scales[i] = s
		for j, v := range x {
			r := math.RoundToEven(float64(v / s))
			q[i*m.cols+j] = int8(min(max(r, -127), 127))
		}
	}
	return q, scales
}

// rowChunks is how many pieces parallelRows cuts the rows into. Fixed rather
// than per-CPU, so that summing the pieces in order gives the same bits on any
// machine.
const rowChunks = 64

// parallelRows splits [0, n) into contiguous chunks, runs f on each (at most
// GOMAXPROCS at a time), and returns the results in chunk order.
func parallelRows[T any](n int, f func(lo, hi int) T) []T {
	chunks := max(1, min(rowChunks, n/256))
	out := make([]T, chunks)
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for c := range chunks {
		lo, hi := c*n/chunks, (c+1)*n/chunks
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			out[c] = f(lo, hi)
		})
	}
	wg.Wait()
	return out
}

// symEigen returns the eigenvalues and eigenvectors (columns of a row-major
// n*n matrix) of the symmetric matrix a: Householder tridiagonalization then
// implicit QL, after the public-domain JAMA EigenvalueDecomposition.
func symEigen(a []float64, n int) (vals, vecs []float64, err error) {
	v := append([]float64(nil), a...)
	d := make([]float64, n)
	e := make([]float64, n)
	tred2(v, d, e, n)
	if err := tql2(v, d, e, n); err != nil {
		return nil, nil, err
	}
	return d, v, nil
}

func tred2(v, d, e []float64, n int) {
	at := func(i, j int) *float64 { return &v[i*n+j] }
	for j := range n {
		d[j] = *at(n-1, j)
	}
	for i := n - 1; i > 0; i-- {
		var scale, h float64
		for k := range i {
			scale += math.Abs(d[k])
		}
		if scale == 0 {
			e[i] = d[i-1]
			for j := range i {
				d[j] = *at(i-1, j)
				*at(i, j) = 0
				*at(j, i) = 0
			}
		} else {
			for k := range i {
				d[k] /= scale
				h += d[k] * d[k]
			}
			f := d[i-1]
			g := math.Sqrt(h)
			if f > 0 {
				g = -g
			}
			e[i] = scale * g
			h -= f * g
			d[i-1] = f - g
			for j := range i {
				e[j] = 0
			}
			for j := range i {
				f = d[j]
				*at(j, i) = f
				g = e[j] + *at(j, j)*f
				for k := j + 1; k <= i-1; k++ {
					g += *at(k, j) * d[k]
					e[k] += *at(k, j) * f
				}
				e[j] = g
			}
			f = 0
			for j := range i {
				e[j] /= h
				f += e[j] * d[j]
			}
			hh := f / (h + h)
			for j := range i {
				e[j] -= hh * d[j]
			}
			for j := range i {
				f = d[j]
				g = e[j]
				for k := j; k <= i-1; k++ {
					*at(k, j) -= f*e[k] + g*d[k]
				}
				d[j] = *at(i-1, j)
				*at(i, j) = 0
			}
		}
		d[i] = h
	}
	for i := 0; i < n-1; i++ {
		*at(n-1, i) = *at(i, i)
		*at(i, i) = 1
		h := d[i+1]
		if h != 0 {
			for k := 0; k <= i; k++ {
				d[k] = *at(k, i+1) / h
			}
			for j := 0; j <= i; j++ {
				var g float64
				for k := 0; k <= i; k++ {
					g += *at(k, i+1) * *at(k, j)
				}
				for k := 0; k <= i; k++ {
					*at(k, j) -= g * d[k]
				}
			}
		}
		for k := 0; k <= i; k++ {
			*at(k, i+1) = 0
		}
	}
	for j := range n {
		d[j] = *at(n-1, j)
		*at(n-1, j) = 0
	}
	*at(n-1, n-1) = 1
	e[0] = 0
}

func tql2(v, d, e []float64, n int) error {
	at := func(i, j int) *float64 { return &v[i*n+j] }
	for i := 1; i < n; i++ {
		e[i-1] = e[i]
	}
	e[n-1] = 0
	var f, tst1 float64
	eps := math.Pow(2, -52)
	for l := range n {
		tst1 = max(tst1, math.Abs(d[l])+math.Abs(e[l]))
		m := l
		for m < n && math.Abs(e[m]) > eps*tst1 {
			m++
		}
		if m > l {
			for iter := 0; ; iter++ {
				if iter > 100 {
					return errors.New("pca: eigendecomposition did not converge")
				}
				g := d[l]
				p := (d[l+1] - g) / (2 * e[l])
				r := math.Hypot(p, 1)
				if p < 0 {
					r = -r
				}
				d[l] = e[l] / (p + r)
				d[l+1] = e[l] * (p + r)
				dl1 := d[l+1]
				h := g - d[l]
				for i := l + 2; i < n; i++ {
					d[i] -= h
				}
				f += h
				p = d[m]
				c, c2, c3 := 1.0, 1.0, 1.0
				el1 := e[l+1]
				var s, s2 float64
				for i := m - 1; i >= l; i-- {
					c3 = c2
					c2 = c
					s2 = s
					g = c * e[i]
					h = c * p
					r = math.Hypot(p, e[i])
					e[i+1] = s * r
					s = e[i] / r
					c = p / r
					p = c*d[i] - s*g
					d[i+1] = h + s*(c*g+s*d[i])
					for k := range n {
						h = *at(k, i+1)
						*at(k, i+1) = s**at(k, i) + c*h
						*at(k, i) = c**at(k, i) - s*h
					}
				}
				p = -s * s2 * c3 * el1 * e[l] / dl1
				e[l] = s * p
				d[l] = c * p
				if math.Abs(e[l]) <= eps*tst1 {
					break
				}
			}
		}
		d[l] += f
		e[l] = 0
	}
	return nil
}
