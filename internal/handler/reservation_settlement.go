package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
)

// Settlement of a front-office reservation into the accounting module.
//
// A walk-in or manual reservation taken with payment has to land in four places
// at once: the customer master, a sales invoice, the general ledger, and a
// numbered voucher. Room revenue previously reached none of them — only POS
// sales posted, so the hotel's primary income was invisible to accounting.
//
// Everything here runs inside the caller's transaction. The repository and
// accounting helpers take a postgres.Querier, which both a pool and a pgx.Tx
// satisfy, so the whole settlement either lands or rolls back as one unit. That
// matters more here than anywhere else in the system: a reservation with no
// journal understates revenue, and a journal with no reservation is money
// attributed to a stay that does not exist.

// settlementResult is what the settlement produced, for the API response.
type settlementResult struct {
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	InvoiceID  uuid.UUID `json:"invoice_id,omitempty"`
	InvoiceNo  string    `json:"invoice_number,omitempty"`
	JournalID  uuid.UUID `json:"journal_entry_id,omitempty"`
	PaymentID  uuid.UUID `json:"payment_id,omitempty"`
	VoucherRef string    `json:"voucher_reference,omitempty"`
}

// settleReservation records the payment and raises the accounting entries.
//
// The reference is the stay's confirmation number prefixed "FOLIO ", which
// voucherTypeFor already maps to the SAL voucher series — so room revenue joins
// the same numbered sales sequence as POS bills without that mapping needing to
// change.
func settleReservation(
	ctx context.Context,
	db postgres.Querier,
	hotelID uuid.UUID,
	stay *domain.GuestStay,
	pay *reservationPaymentRequest,
	quote stayQuote,
) (settlementResult, error) {
	var out settlementResult

	method := strings.ToLower(strings.TrimSpace(pay.Method))
	reference := "FOLIO " + derefStr(stay.ConfirmationNo)
	out.VoucherRef = reference

	// Idempotency on the reference, the same guard the legacy POS path uses. A
	// retried submission must not book the sale twice — a missing journal can
	// be reposted, double-counted revenue corrupts the books.
	if alreadyPosted(ctx, db, hotelID, reference) {
		return out, nil
	}

	// --- 1. Customer master -------------------------------------------------
	//
	// Reuses the POS resolver, so a guest who has eaten in the restaurant and
	// now books a room is one customer, not two. It matches on the normalised
	// phone and returns Nil when there is nothing worth recording.
	customerID, err := ensureAccountingCustomer(ctx, db, hotelID, CustomerDetails{
		Name:  stay.GuestName,
		Phone: derefStr(stay.GuestPhone),
		Email: derefStr(stay.GuestEmail),
	})
	if err != nil {
		return out, fmt.Errorf("settle: customer: %w", err)
	}
	out.CustomerID = customerID

	// A credit sale with nobody to bill is refused rather than posted to a
	// receivable that can never be collected — the existing POS policy, applied
	// consistently to rooms.
	if isCreditSettlement(method) && customerID == uuid.Nil {
		return out, fmt.Errorf("a credit booking requires a phone or email to bill")
	}

	// --- 2. Payment record --------------------------------------------------
	paymentID := uuid.New()
	paymentNo := fmt.Sprintf("PAY-%s-%s", time.Now().UTC().Format("20060102"),
		strings.ToUpper(uuid.New().String()[:5]))
	change := round2(pay.CashReceived - quote.Payable)
	if method != "cash" || change < 0 {
		change = 0
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO payments (
			id, hotel_id, payment_number, guest_stay_id, amount, payment_method, status,
			upi_id, transaction_ref, card_last4, auth_code, cash_received, change_given, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,
		          NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),$12,$13,now())`,
		paymentID, hotelID, paymentNo, stay.ID, quote.Payable, method, "completed",
		strings.TrimSpace(pay.UPIID), strings.TrimSpace(pay.TransactionRef),
		strings.TrimSpace(pay.CardLast4), strings.TrimSpace(pay.AuthCode),
		pay.CashReceived, change,
	); err != nil {
		return out, fmt.Errorf("settle: payment: %w", err)
	}
	out.PaymentID = paymentID

	// --- 3. Sales invoice ---------------------------------------------------
	invoiceID := uuid.New()
	invoiceNo := fmt.Sprintf("INV-%s-%d", derefStr(stay.ConfirmationNo), time.Now().UnixMilli())
	nights := quote.Nights
	if _, err := db.Exec(ctx, `
		INSERT INTO accounting_sales_invoices (
			id, hotel_id, customer_id, invoice_number, invoice_date, due_date, reference,
			subtotal, discount_total, tax_total, total, status, notes, created_at, updated_at
		) VALUES ($1,$2,$3,$4,CURRENT_DATE,$5,$6,$7,$8,$9,$10,$11,$12,now(),now())`,
		invoiceID, hotelID, nilAsNull(customerID), invoiceNo,
		stay.CheckOutDate, reference,
		quote.BaseTotal, quote.Discount, quote.TaxAmount, quote.Payable,
		"posted",
		fmt.Sprintf("Room %s, %d night(s), %s to %s",
			roomLabel(stay), nights,
			stay.CheckInDate.Format("2006-01-02"), stay.CheckOutDate.Format("2006-01-02")),
	); err != nil {
		return out, fmt.Errorf("settle: invoice: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO accounting_sales_invoice_lines (
			id, invoice_id, hotel_id, description, quantity, unit_price,
			discount, tax_rate, tax_amount, total, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())`,
		uuid.New(), invoiceID, hotelID,
		fmt.Sprintf("Accommodation — room %s", roomLabel(stay)),
		nights, quote.RoomRate, quote.Discount, quote.TaxRate, quote.TaxAmount, quote.Payable,
	); err != nil {
		return out, fmt.Errorf("settle: invoice line: %w", err)
	}
	out.InvoiceID = invoiceID
	out.InvoiceNo = invoiceNo

	// --- 4. Ledger + voucher -----------------------------------------------
	//
	// postJournal numbers the voucher at its single chokepoint, so this posting
	// joins the SAL series automatically. Room revenue books to 4000 Sales
	// Revenue, keeping it distinguishable from 4100 F&B in the P&L.
	description := fmt.Sprintf("Room booking — %s (%s)", stay.GuestName, derefStr(stay.ConfirmationNo))
	revenue := round2(quote.BaseTotal - quote.Discount)

	debitAccount := cashAccount(method)
	debitMemo := "Room booking settlement (" + method + ")"
	if isCreditSettlement(method) {
		debitAccount = "1200"
		debitMemo = "Receivable — room booking on credit"
	}

	journalID, err := postJournal(ctx, db, hotelID, description, reference, []journalLine{
		{accountCode: debitAccount, debit: quote.Payable, memo: debitMemo},
		{accountCode: "4000", credit: revenue, memo: "Room revenue"},
		{accountCode: "2100", credit: quote.TaxAmount, memo: "GST on room revenue"},
	})
	if err != nil {
		return out, fmt.Errorf("settle: ledger: %w", err)
	}
	out.JournalID = journalID

	return out, nil
}

// roomLabel is the room number for invoice text, falling back to something
// printable when the stay was loaded without its room joined.
func roomLabel(stay *domain.GuestStay) string {
	if stay.Room != nil && stay.Room.RoomNumber != "" {
		return stay.Room.RoomNumber
	}
	return "—"
}
