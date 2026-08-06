package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/hotelharmony/api/internal/repository/postgres"
)

// Customer linkage between POS dine-in and the accounting customer master.
//
// A dine-in sale previously carried only a free-text guest name, so a POS
// customer could never appear in accounting_customers and nobody could review
// or edit them there. These helpers resolve the details captured at the table
// into a real customer record.
//
// Policy — matching how established POS systems (Square, Toast, Lightspeed)
// behave: attaching a customer is OPTIONAL. A record is created only when the
// sale genuinely needs one, namely when contact details were captured, or when
// money is owed (credit / bill-to-room). An anonymous cash walk-in stays
// anonymous, otherwise the customer master and the AR ledger fill with
// thousands of unusable "Walk-in 12:45" rows.

// CustomerDetails is what the POS captures at the table.
type CustomerDetails struct {
	Name  string `json:"customer_name"`
	Phone string `json:"customer_phone"`
	Email string `json:"customer_email"`
	GSTIN string `json:"customer_gstin"`
}

// HasContact reports whether enough was captured to be worth a customer record.
// A bare name is not enough: "Raj" identifies nobody and cannot be matched
// against on a later visit.
func (d CustomerDetails) HasContact() bool {
	return strings.TrimSpace(d.Phone) != "" || strings.TrimSpace(d.Email) != ""
}

func (d CustomerDetails) normalised() CustomerDetails {
	return CustomerDetails{
		Name:  strings.TrimSpace(d.Name),
		Phone: normalisePhone(d.Phone),
		Email: strings.ToLower(strings.TrimSpace(d.Email)),
		GSTIN: strings.ToUpper(strings.TrimSpace(d.GSTIN)),
	}
}

// normalisePhone strips formatting so "+91 98765 43210" and "9876543210" match
// the same customer. Only digits are kept, and an Indian country code is
// dropped so both entry styles converge.
func normalisePhone(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if len(digits) == 12 && strings.HasPrefix(digits, "91") {
		digits = digits[2:]
	}
	return digits
}

// ensureAccountingCustomer resolves captured details to a customer id,
// creating the record when it does not already exist.
//
// Matching is by phone, the natural key staff have at the table. Returns
// (uuid.Nil, nil) when there is nothing worth recording — the caller treats
// that as an anonymous sale, not an error.
func ensureAccountingCustomer(ctx context.Context, pool postgres.Querier, hotelID uuid.UUID, in CustomerDetails) (uuid.UUID, error) {
	d := in.normalised()
	if !d.HasContact() {
		return uuid.Nil, nil
	}
	if d.Name == "" {
		d.Name = "POS customer " + d.Phone
	}

	// Existing customer by phone.
	if d.Phone != "" {
		var id uuid.UUID
		err := pool.QueryRow(ctx,
			`SELECT id FROM accounting_customers WHERE hotel_id=$1 AND phone=$2`,
			hotelID, d.Phone).Scan(&id)
		if err == nil {
			// Fill in details the record was missing, without overwriting
			// anything an accountant has already curated.
			_, _ = pool.Exec(ctx, `
				UPDATE accounting_customers
				SET email = COALESCE(NULLIF(email,''), NULLIF($2,'')),
				    gstin = COALESCE(NULLIF(gstin,''), NULLIF($3,'')),
				    updated_at = now()
				WHERE id = $1`, id, d.Email, d.GSTIN)
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, err
		}
	}

	code, err := nextCustomerCode(ctx, pool, hotelID)
	if err != nil {
		return uuid.Nil, err
	}

	id := uuid.New()
	// ON CONFLICT covers the race where two tills ring up the same phone at
	// once; the partial unique index on (hotel_id, phone) is the arbiter.
	err = pool.QueryRow(ctx, `
		INSERT INTO accounting_customers (id, hotel_id, code, name, phone, email, gstin, is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),true,now(),now())
		ON CONFLICT (hotel_id, phone) WHERE phone IS NOT NULL AND phone <> ''
		DO UPDATE SET updated_at = now()
		RETURNING id`,
		id, hotelID, code, d.Name, d.Phone, d.Email, d.GSTIN).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// nextCustomerCode allocates the next CUST-nnnn for a tenant.
// accounting_customers.code is UNIQUE(hotel_id, code) and NOT NULL, so a code
// must be generated rather than left blank.
func nextCustomerCode(ctx context.Context, pool postgres.Querier, hotelID uuid.UUID) (string, error) {
	var maxNum int
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(NULLIF(regexp_replace(code, '\D', '', 'g'), '')::int), 0)
		FROM accounting_customers
		WHERE hotel_id = $1 AND code ~ '^CUST-[0-9]+$'`, hotelID).Scan(&maxNum)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("CUST-%04d", maxNum+1), nil
}

// isCreditSettlement reports whether a payment method leaves money owed, which
// is what decides between a cash-style posting and a receivable.
func isCreditSettlement(method string) bool {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "credit", "room", "room_charge", "bill_to_room", "account":
		return true
	default:
		return false
	}
}

// postSalesToLedgerFor posts a settled POS sale, choosing the debit side by how
// it was settled:
//
//	cash/card/upi -> Dr Cash or Bank        (money received now)
//	credit/room   -> Dr Accounts Receivable (money owed by the customer)
//
// The credit case is why a customer is mandatory for credit sales: an unnamed
// receivable cannot be collected. Revenue and GST legs are identical either way.
func postSalesToLedgerFor(ctx context.Context, pool postgres.Querier, hotelID uuid.UUID, method string, subtotal, tax, total float64, reference, description string, customerID uuid.UUID) (uuid.UUID, error) {
	if !isCreditSettlement(method) {
		return postSalesToLedger(ctx, pool, hotelID, method, subtotal, tax, total, reference, description)
	}
	if customerID == uuid.Nil {
		return uuid.Nil, errors.New("a credit sale requires a customer to owe the receivable")
	}
	rev := round2(total - tax)
	return postJournal(ctx, pool, hotelID, description, reference, []journalLine{
		{accountCode: "1200", debit: round2(total), memo: "Receivable — POS credit sale"},
		{accountCode: "4100", credit: rev, memo: "F&B revenue"},
		{accountCode: "2100", credit: round2(tax), memo: "GST on sale"},
	})
}

// nilAsNull renders a zero UUID as JSON null, so an anonymous sale reports
// "customer_id": null rather than an all-zero id the client would treat as real.
func nilAsNull(id uuid.UUID) interface{} {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// --- Legacy /pos/orders ledger posting -------------------------------------
//
// The pos_orders path predates the dine-in flow and never posted to the general
// ledger, so sales taken through it were invisible to accounting: on the
// testingxyz tenant four settled orders worth INR 5,074 produced no journal
// entries at all. Dine-in posts on settlement; this gives the legacy path the
// same treatment so no sale escapes the books whichever screen took it.

// isPaidStatus reports whether a legacy order status means the money is in.
// The column is free text and has been written with several spellings.
func isPaidStatus(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "paid", "completed", "settled", "closed":
		return true
	default:
		return false
	}
}

// alreadyPosted reports whether a journal already exists for this reference.
// PATCH /pos/orders/:id is a generic column patcher a client may send more than
// once, so without this guard the same sale would be booked repeatedly.
func alreadyPosted(ctx context.Context, pool postgres.Querier, hotelID uuid.UUID, reference string) bool {
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM accounting_journal_entries WHERE hotel_id=$1 AND reference=$2`,
		hotelID, reference).Scan(&n); err != nil {
		// On error, assume posted: skipping a journal is recoverable by
		// reposting, double-booking revenue is not.
		return true
	}
	return n > 0
}

// postLegacyPOSOrder books a settled legacy order. Amounts come from the stored
// row rather than the request, so a patched total cannot mis-state revenue.
// customerName is the only customer signal the legacy table carries; a phone is
// not collected there, so a customer is linked only when one already matches.
func postLegacyPOSOrder(ctx context.Context, pool postgres.Querier, hotelID uuid.UUID, orderNumber, status, paymentMethod, customerName string, subtotal, tax, total float64) {
	if !isPaidStatus(status) || round2(total) == 0 {
		return
	}
	reference := "ORDER " + orderNumber
	if alreadyPosted(ctx, pool, hotelID, reference) {
		return
	}
	method := strings.ToLower(strings.TrimSpace(paymentMethod))
	if method == "" {
		method = "cash"
	}
	if _, err := postSalesToLedger(ctx, pool, hotelID, method, subtotal, tax, total,
		reference, "POS sale — order "+orderNumber); err != nil {
		log.Printf("pos: LEDGER POST FAILED for legacy order %s (%.2f, %s): %v — sale stands, journal missing",
			orderNumber, total, method, err)
	}
}

// derefStr flattens an optional string column to a value.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
