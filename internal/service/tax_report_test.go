package service

import "testing"

func TestSplitTCS(t *testing.T) {
	cases := []struct {
		tcs              int64
		interstate       bool
		cgst, sgst, igst int64
	}{
		{401, false, 201, 200, 0}, // odd paisa goes to CGST
		{400, false, 200, 200, 0},
		{400, true, 0, 0, 400},
		{0, false, 0, 0, 0},
	}
	for _, c := range cases {
		cgst, sgst, igst := SplitTCS(c.tcs, c.interstate)
		if cgst != c.cgst || sgst != c.sgst || igst != c.igst {
			t.Errorf("SplitTCS(%d,%v) = %d/%d/%d, want %d/%d/%d", c.tcs, c.interstate, cgst, sgst, igst, c.cgst, c.sgst, c.igst)
		}
	}
}

func TestPlatformWithdrawable(t *testing.T) {
	// ₹1,000 balance, ₹50 tax stamped, ₹20 already deposited → ₹30 still owed.
	if got := platformWithdrawable(100000, 5000, 2000); got != 97000 {
		t.Errorf("withdrawable = %d, want 97000", got)
	}
	// Owing more than the balance never goes negative.
	if got := platformWithdrawable(1000, 5000, 0); got != 0 {
		t.Errorf("withdrawable = %d, want 0", got)
	}
	// Over-logged deposits never inflate the balance.
	if got := platformWithdrawable(1000, 100, 500); got != 1000 {
		t.Errorf("withdrawable = %d, want 1000", got)
	}
}

func TestValidateStateCode(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, ok := range []*string{nil, s(""), s("27"), s("07")} {
		if err := validateStateCode(ok); err != nil {
			t.Errorf("%v: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"7", "MH", "270", "2a"} {
		if err := validateStateCode(s(bad)); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestResolveStateCode(t *testing.T) {
	s := func(v string) *string { return &v }
	if got := resolveStateCode(s("29"), s("Guwahati, Assam")); *got != "29" {
		t.Errorf("explicit wins: got %q", *got)
	}
	if got := resolveStateCode(nil, s("Guwahati, Assam")); *got != "18" {
		t.Errorf("from address: got %q", *got)
	}
	if got := resolveStateCode(nil, s("London")); *got != "" {
		t.Errorf("unknown address clears: got %q", *got)
	}
	if got := resolveStateCode(nil, nil); got != nil {
		t.Errorf("no location leaves it alone: got %q", *got)
	}
}
