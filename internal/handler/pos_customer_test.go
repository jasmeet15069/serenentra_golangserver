package handler

import "testing"

// Phone is the key an accounting customer is matched on, so the normaliser
// decides whether a returning guest is recognised or duplicated. Staff type the
// same number many ways.
func TestNormalisePhone(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{"9876543210", "9876543210", "plain ten digits"},
		{"+91 98765 43210", "9876543210", "country code and spaces stripped"},
		{"+919876543210", "9876543210", "country code without spaces"},
		{"98765-43210", "9876543210", "hyphens stripped"},
		{"(098) 765 43210", "09876543210", "leading zero kept — not a country code"},
		{"", "", "empty stays empty"},
		{"not a phone", "", "letters yield nothing"},
	}
	for _, c := range cases {
		if got := normalisePhone(c.in); got != c.want {
			t.Errorf("normalisePhone(%q) = %q, want %q (%s)", c.in, got, c.want, c.why)
		}
	}
}

// Two spellings of the same number must collapse to one customer, otherwise a
// regular accumulates a new record on every visit.
func TestNormalisePhoneMatchesAcrossFormats(t *testing.T) {
	a := normalisePhone("+91 98765 43210")
	b := normalisePhone("9876543210")
	if a != b {
		t.Fatalf("same number did not converge: %q vs %q", a, b)
	}
}

// A bare name must NOT create a customer: "Raj" identifies nobody and cannot be
// matched on a later visit, so it would only pollute the AR ledger.
func TestHasContactRequiresPhoneOrEmail(t *testing.T) {
	cases := []struct {
		name string
		in   CustomerDetails
		want bool
	}{
		{"nothing", CustomerDetails{}, false},
		{"name only", CustomerDetails{Name: "Raj"}, false},
		{"whitespace only", CustomerDetails{Name: "Raj", Phone: "   "}, false},
		{"phone", CustomerDetails{Phone: "9876543210"}, true},
		{"email", CustomerDetails{Email: "raj@example.com"}, true},
		{"gstin only", CustomerDetails{GSTIN: "27AABCU9603R1ZV"}, false},
	}
	for _, c := range cases {
		if got := c.in.HasContact(); got != c.want {
			t.Errorf("%s: HasContact() = %v, want %v", c.name, got, c.want)
		}
	}
}

// The settlement method decides the debit side of the journal: money received
// now (Cash/Bank) versus money owed (Accounts Receivable). Getting this wrong
// either overstates cash or loses a receivable.
func TestIsCreditSettlement(t *testing.T) {
	credit := []string{"credit", "room", "room_charge", "bill_to_room", "account", "CREDIT", " Room_Charge "}
	for _, m := range credit {
		if !isCreditSettlement(m) {
			t.Errorf("%q should be a credit settlement (posts to AR)", m)
		}
	}
	cash := []string{"cash", "card", "upi", "bank", "other", ""}
	for _, m := range cash {
		if isCreditSettlement(m) {
			t.Errorf("%q should settle to cash/bank, not AR", m)
		}
	}
}

// A credit sale with no customer has no one to collect from, so it must be
// refused rather than posted to an anonymous receivable.
func TestCreditSaleRequiresCustomer(t *testing.T) {
	if !isCreditSettlement("credit") {
		t.Fatal("precondition: credit must be a credit settlement")
	}
	// postSalesToLedgerFor returns an error before touching the database when
	// customerID is nil; a nil pool would panic if it got that far.
	_, err := postSalesToLedgerFor(nil, nil, [16]byte{}, "credit", 100, 18, 118, "BILL X", "test", [16]byte{})
	if err == nil {
		t.Fatal("credit sale without a customer should be refused")
	}
}

func TestNormalisedTrimsAndCases(t *testing.T) {
	d := CustomerDetails{
		Name:  "  Raj Kumar  ",
		Phone: " +91 98765 43210 ",
		Email: "  Raj@Example.COM ",
		GSTIN: " 27aabcu9603r1zv ",
	}.normalised()

	if d.Name != "Raj Kumar" {
		t.Errorf("name = %q", d.Name)
	}
	if d.Phone != "9876543210" {
		t.Errorf("phone = %q", d.Phone)
	}
	if d.Email != "raj@example.com" {
		t.Errorf("email = %q, want lowercased", d.Email)
	}
	if d.GSTIN != "27AABCU9603R1ZV" {
		t.Errorf("gstin = %q, want uppercased", d.GSTIN)
	}
}
