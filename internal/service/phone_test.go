package service

import "testing"

func TestNormalizeIndianPhone(t *testing.T) {
	ok := map[string]string{
		"9876543210":       "+919876543210",
		"+91 98765 43210":  "+919876543210",
		"098765-43210":     "+919876543210",
		"+919876543210":    "+919876543210",
	}
	for in, want := range ok {
		got, err := normalizeIndianPhone(in)
		if err != nil || got != want {
			t.Errorf("normalizeIndianPhone(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "12345", "abc"} {
		if _, err := normalizeIndianPhone(bad); err == nil {
			t.Errorf("normalizeIndianPhone(%q): want error, got nil", bad)
		}
	}
}
