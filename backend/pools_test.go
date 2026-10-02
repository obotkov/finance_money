package main

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestPoolAmountsAndLiquidity(t *testing.T) {
	const lo, hi = 2800.0, 3600.0
	// 1 ETH внутри интервала при цене 3200: USDC к нему считается формулой
	L := poolLiquidity(1, 1e9, 3200, lo, hi)
	a, b := poolAmounts(L, 3200, lo, hi)
	if !near(a, 1) {
		t.Fatalf("A at entry price: %v", a)
	}
	if b < 3600 || b > 3630 {
		t.Fatalf("B at entry price should be about 3 613 USDC, got %v", b)
	}
	// туда и обратно: ликвидность из тех же монет — та же
	if !near(poolLiquidity(a, b, 3200, lo, hi), L) {
		t.Error("round trip changes liquidity")
	}
	// ниже интервала всё в A, выше — всё в B
	if a, b := poolAmounts(L, 2000, lo, hi); b != 0 || !(a > 1) {
		t.Errorf("below range: a=%v b=%v", a, b)
	}
	if a, b := poolAmounts(L, 5000, lo, hi); a != 0 || !(b > 3613) {
		t.Errorf("above range: a=%v b=%v", a, b)
	}
	// ниже интервала вносится только A, выше — только B, внутри нужны обе
	if poolLiquidity(0, 100, 2500, lo, hi) != 0 || !(poolLiquidity(1, 0, 2500, lo, hi) > 0) {
		t.Error("below range needs A only")
	}
	if poolLiquidity(1, 0, 4000, lo, hi) != 0 || !(poolLiquidity(0, 100, 4000, lo, hi) > 0) {
		t.Error("above range needs B only")
	}
	if poolLiquidity(1, 0, 3200, lo, hi) != 0 || poolLiquidity(0, 1000, 3200, lo, hi) != 0 {
		t.Error("inside range needs both coins")
	}
	// лишнее одной монеты не вносится: ликвидность — по меньшей
	if !near(poolLiquidity(1, 1e9, 3200, lo, hi), poolLiquidity(1, b, 3200, lo, hi)) {
		t.Error("excess B must not add liquidity")
	}
}
