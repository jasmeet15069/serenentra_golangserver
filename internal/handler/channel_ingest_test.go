package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func sign(t *testing.T, body []byte, secret string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// The webhook is registered before the auth gate, so this signature is the only
// thing standing between the open internet and the ability to create
// reservations and post to the ledger. It is worth pinning hard.
func TestVerifySignature(t *testing.T) {
	body := []byte(`{"external_ref":"BDC-991","guest_name":"Asha"}`)
	const secret = "s3cr3t-channel-key"
	good := sign(t, body, secret)

	cases := []struct {
		name     string
		body     []byte
		secret   string
		provided string
		want     bool
	}{
		{"a correct signature", body, secret, good, true},
		{"the sha256= prefix GitHub-style senders add", body, secret, "sha256=" + good, true},
		{"leading and trailing whitespace is tolerated", body, secret, "  " + good + "\n", true},

		{"a signature for a different body", []byte(`{"external_ref":"BDC-992"}`), secret, good, false},
		{"a signature made with a different secret", body, "another-key", good, false},
		{"an empty signature", body, secret, "", false},
		{"no configured secret cannot be satisfied", body, "", good, false},
		{"no secret and no signature is still a refusal", body, "", "", false},
		{"a truncated signature", body, secret, good[:32], false},
		{"a signature that is not hex", body, secret, "not-a-signature", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verifySignature(c.body, c.secret, c.provided); got != c.want {
				t.Errorf("verifySignature() = %v, want %v", got, c.want)
			}
		})
	}
}

// An empty secret must never authenticate anything. A channel_connections row
// with no api_key is unconfigured, and treating that as "no signature required"
// would silently open the endpoint the moment somebody created a connection
// without a key.
func TestVerifySignatureRefusesUnconfiguredChannel(t *testing.T) {
	body := []byte(`{"external_ref":"X"}`)
	if verifySignature(body, "", sign(t, body, "")) {
		t.Error("an unconfigured channel (no api_key) authenticated a request signed with the empty secret")
	}
}

// Each OTA names the same fields differently. The point of the aliases is that
// a new provider works without a code change, so the mapping is worth a test.
func TestChannelBookingPayloadAliases(t *testing.T) {
	cases := []struct {
		name                                 string
		raw                                  string
		wantRef, wantName, wantEmail         string
		wantPhone, wantCheckIn, wantCheckOut string
		wantTotal                            float64
	}{
		{
			name:        "Booking.com style",
			raw:         `{"reservation_id":"BDC-4471","first_name":"Asha","last_name":"Iyer","email":"asha@example.com","phone":"+91 98765 43210","check_in":"2026-09-01","check_out":"2026-09-04","total_price":18000}`,
			wantRef:     "BDC-4471",
			wantName:    "Asha Iyer",
			wantEmail:   "asha@example.com",
			wantPhone:   "+91 98765 43210",
			wantCheckIn: "2026-09-01", wantCheckOut: "2026-09-04",
			wantTotal: 18000,
		},
		{
			name:        "Agoda style",
			raw:         `{"booking_id":"AGD-88","guest_name":"Rahul Menon","guest_email":"r@example.com","check_in_date":"2026-09-10","check_out_date":"2026-09-12","total":9500}`,
			wantRef:     "AGD-88",
			wantName:    "Rahul Menon",
			wantEmail:   "r@example.com",
			wantCheckIn: "2026-09-10", wantCheckOut: "2026-09-12",
			wantTotal: 9500,
		},
		{
			name:     "OYO style, short ref and phone only",
			raw:      `{"ref":"OYO-1201","guest_name":"Meera","guest_phone":"9811122333","check_in":"2026-09-15","check_out":"2026-09-16","total":2400}`,
			wantRef:  "OYO-1201",
			wantName: "Meera", wantPhone: "9811122333",
			wantCheckIn: "2026-09-15", wantCheckOut: "2026-09-16",
			wantTotal: 2400,
		},
		{
			name:    "explicit external_ref wins over the aliases",
			raw:     `{"external_ref":"CANON-1","booking_id":"IGNORED","guest_name":"X","check_in":"2026-09-01","check_out":"2026-09-02"}`,
			wantRef: "CANON-1", wantName: "X",
			wantCheckIn: "2026-09-01", wantCheckOut: "2026-09-02",
		},
		{
			name:    "whitespace is trimmed so a redelivery matches its own key",
			raw:     `{"external_ref":"  MMT-7\n","guest_name":" Priya ","check_in":"2026-09-01","check_out":"2026-09-02"}`,
			wantRef: "MMT-7", wantName: "Priya",
			wantCheckIn: "2026-09-01", wantCheckOut: "2026-09-02",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p channelBookingPayload
			if err := json.Unmarshal([]byte(c.raw), &p); err != nil {
				t.Fatalf("payload did not parse: %v", err)
			}
			if got := p.ref(); got != c.wantRef {
				t.Errorf("ref() = %q, want %q", got, c.wantRef)
			}
			if got := p.name(); got != c.wantName {
				t.Errorf("name() = %q, want %q", got, c.wantName)
			}
			if got := p.email(); got != c.wantEmail {
				t.Errorf("email() = %q, want %q", got, c.wantEmail)
			}
			if got := p.phone(); got != c.wantPhone {
				t.Errorf("phone() = %q, want %q", got, c.wantPhone)
			}
			if got := p.checkIn(); got != c.wantCheckIn {
				t.Errorf("checkIn() = %q, want %q", got, c.wantCheckIn)
			}
			if got := p.checkOut(); got != c.wantCheckOut {
				t.Errorf("checkOut() = %q, want %q", got, c.wantCheckOut)
			}
			if got := p.total(); got != c.wantTotal {
				t.Errorf("total() = %v, want %v", got, c.wantTotal)
			}
		})
	}
}

// A payload with no usable reference has no idempotency key, so a redelivery
// would create a second reservation holding a second room. Ingest refuses it,
// and this is the check that decides that.
func TestChannelBookingPayloadRejectsMissingRef(t *testing.T) {
	var p channelBookingPayload
	if err := json.Unmarshal([]byte(`{"guest_name":"No Ref"}`), &p); err != nil {
		t.Fatalf("payload did not parse: %v", err)
	}
	if p.ref() != "" {
		t.Errorf("ref() = %q, want empty so Ingest refuses the delivery", p.ref())
	}
}
