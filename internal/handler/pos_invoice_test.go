package handler

import "testing"

func TestAmountInWords(t *testing.T) {
	cases := []struct {
		amount float64
		want   string
	}{
		{169.21, "One Hundred Sixty Nine Rupees And Twenty One Paisa Only"},
		{17.582, "Seventeen Rupees And Fifty Eight Paisa Only"}, // 0.582 -> 58 paisa (rounded)
		{1000, "One Thousand Rupees Only"},
		{0, "Zero Rupees Only"},
		{125000.50, "One Lakh Twenty Five Thousand Rupees And Fifty Paisa Only"},
		{10000000, "One Crore Rupees Only"},
	}
	for _, tc := range cases {
		if got := amountInWords(tc.amount, "INR"); got != tc.want {
			t.Errorf("amountInWords(%.3f) = %q, want %q", tc.amount, got, tc.want)
		}
	}
}
