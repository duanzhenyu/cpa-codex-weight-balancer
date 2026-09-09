package balancer

import (
	"math"
	"testing"
)

func qz() Quantizer { return DefaultQuantizer() }

// posOf builds a rate that lands exactly on bucket coordinate x.
func posOf(q Quantizer, x float64) float64 { return q.R0 * math.Pow(1+q.Q, x) }

func TestQuantizerDefaults(t *testing.T) {
	q := qz()
	if q.Q != DefaultQ || q.Margin != DefaultMargin || q.MinWeight != DefaultMinWeight || q.MaxWeight != DefaultMaxWeight {
		t.Fatalf("defaults not applied: %+v", q)
	}
}

func TestPositionAnchor(t *testing.T) {
	q := qz()
	got, ok := q.Position(q.R0)
	if !ok {
		t.Fatal("anchor rate must be quantizable")
	}
	near(t, got+1, 1) // position 0
}

func TestPositionRejectsNonPositive(t *testing.T) {
	q := qz()
	for _, r := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, ok := q.Position(r); ok {
			t.Fatalf("rate %v must not be quantizable", r)
		}
	}
}

func TestWeightAtAnchor(t *testing.T) {
	if got := qz().Weight(0); got != int64(DefaultW0) {
		t.Fatalf("got %d, want %d", got, int64(DefaultW0))
	}
}

func TestWeightLadderStep(t *testing.T) {
	q := qz()
	// one bucket up is ~Q larger
	near(t, float64(q.Weight(1)), math.Round(q.W0*(1+q.Q)))
}

func TestWeightSaturatesWithoutOverflow(t *testing.T) {
	q := qz()
	for _, b := range []int{math.MaxInt32, 10000, -10000, math.MinInt32} {
		w := q.Weight(b)
		if w < q.MinWeight || w > q.MaxWeight {
			t.Fatalf("bucket %d -> weight %d out of [%d,%d]", b, w, q.MinWeight, q.MaxWeight)
		}
	}
	if q.Weight(math.MaxInt32) != q.MaxWeight || q.Weight(math.MinInt32) != q.MinWeight {
		t.Fatal("extreme buckets must saturate at the bounds")
	}
}

func TestBoundsMapInsideWeightRange(t *testing.T) {
	q := qz()
	lo, hi := q.Bounds()
	if lo >= hi {
		t.Fatalf("empty bucket range %d..%d", lo, hi)
	}
	if w := q.Weight(lo); w < q.MinWeight || w > q.MaxWeight {
		t.Fatalf("lo bucket %d -> %d, outside the limits", lo, w)
	}
	if w := q.Weight(hi); w < q.MinWeight || w > q.MaxWeight {
		t.Fatalf("hi bucket %d -> %d, outside the limits", hi, w)
	}
	// One step beyond the bounds must saturate rather than overflow.
	if q.Weight(lo-1) != q.MinWeight || q.Weight(hi+1) != q.MaxWeight {
		t.Fatal("buckets past the bounds must saturate")
	}
}

func TestCommitFirstObservation(t *testing.T) {
	q := qz()
	b, changed := q.Commit(Bucket{}, posOf(q, 12.4))
	if !changed || !b.Set || b.Index != 12 {
		t.Fatalf("got %+v changed=%v, want bucket 12 committed", b, changed)
	}
}

func TestCommitHoldsInsideDeadband(t *testing.T) {
	q := qz()
	prev := Bucket{Index: 12, Set: true}
	// Anywhere in [12-margin, 13+margin) the retained bucket must not move,
	// so the weight map stays byte-identical and the WRR accumulator survives.
	for _, x := range []float64{11.7, 12.0, 12.5, 12.99, 13.3} {
		b, changed := q.Commit(prev, posOf(q, x))
		if changed || b != prev {
			t.Fatalf("x=%v: moved to %+v (changed=%v)", x, b, changed)
		}
	}
}

func TestCommitMovesOnOvershoot(t *testing.T) {
	q := qz()
	prev := Bucket{Index: 12, Set: true}
	up, changed := q.Commit(prev, posOf(q, 13.4))
	if !changed || up.Index != 13 {
		t.Fatalf("up: got %+v changed=%v, want 13", up, changed)
	}
	down, changed := q.Commit(prev, posOf(q, 11.6))
	if !changed || down.Index != 11 {
		t.Fatalf("down: got %+v changed=%v, want 11", down, changed)
	}
}

func TestCommitClampsIntoBounds(t *testing.T) {
	q := qz()
	lo, hi := q.Bounds()
	b, _ := q.Commit(Bucket{}, posOf(q, float64(hi)+50))
	if b.Index != hi {
		t.Fatalf("got %d, want clamp to %d", b.Index, hi)
	}
	b, _ = q.Commit(Bucket{}, posOf(q, float64(lo)-50))
	if b.Index != lo {
		t.Fatalf("got %d, want clamp to %d", b.Index, lo)
	}
}

func TestCommitKeepsBucketOnUnquantizableRate(t *testing.T) {
	q := qz()
	prev := Bucket{Index: 12, Set: true}
	// rate 0 means drained; that is the state machine's business, not a bucket move.
	if b, changed := q.Commit(prev, 0); changed || b != prev {
		t.Fatalf("got %+v changed=%v, want retained", b, changed)
	}
}
