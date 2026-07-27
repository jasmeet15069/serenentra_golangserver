package handler

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Accounting auto-posting — connects operational events (POS sales, etc.) to the
// general ledger the way D365 posts a retail statement: a balanced double-entry
// journal that flows into the trial balance and thus P&L + Balance Sheet.

// defaultAccount is one row of the standard chart of accounts seeded per tenant.
type defaultAccount struct {
	code, name, typ, subType string
	order                    int
}

// standardChart is a minimal hospitality chart of accounts. It is seeded lazily
// (idempotent) the first time a tenant needs to post, so blank/new tenants still
// get a working ledger without manual setup.
var standardChart = []defaultAccount{
	{"1000", "Cash", "asset", "current_asset", 1},
	{"1010", "Bank", "asset", "current_asset", 2},
	{"1200", "Accounts Receivable", "asset", "current_asset", 3},
	{"1400", "Inventory", "asset", "current_asset", 4},
	{"2000", "Accounts Payable", "liability", "current_liability", 5},
	{"2100", "GST Payable", "liability", "current_liability", 6},
	{"3000", "Retained Earnings", "equity", "equity", 7},
	{"4000", "Sales Revenue", "revenue", "operating_revenue", 8},
	{"4100", "Food & Beverage Revenue", "revenue", "operating_revenue", 9},
	{"5000", "Cost of Goods Sold", "expense", "cogs", 10},
}

// ensureChartOfAccounts inserts any missing standard accounts and returns a
// code -> account_id map for the tenant. Safe to call on every post.
func ensureChartOfAccounts(ctx context.Context, pool *pgxpool.Pool, hotelID uuid.UUID) (map[string]uuid.UUID, error) {
	for _, a := range standardChart {
		if _, err := pool.Exec(ctx, `
			INSERT INTO accounting_accounts (id, hotel_id, code, name, type, sub_type, is_active, display_order, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,true,$7,now(),now())
			ON CONFLICT (hotel_id, code) DO NOTHING`,
			uuid.New(), hotelID, a.code, a.name, a.typ, a.subType, a.order); err != nil {
			return nil, err
		}
	}
	rows, err := pool.Query(ctx, `SELECT code, id FROM accounting_accounts WHERE hotel_id = $1`, hotelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]uuid.UUID{}
	for rows.Next() {
		var code string
		var id uuid.UUID
		if err := rows.Scan(&code, &id); err == nil {
			m[code] = id
		}
	}
	return m, nil
}

// journalLine is one debit/credit posting.
type journalLine struct {
	accountCode string
	debit       float64
	credit      float64
	memo        string
}

// postJournal writes a balanced journal entry + lines for the tenant. Lines whose
// account code is unknown are skipped; a zero-amount line is skipped. Returns the
// entry id. The caller is responsible for balance (debits == credits).
func postJournal(ctx context.Context, pool *pgxpool.Pool, hotelID uuid.UUID, description, reference string, lines []journalLine) (uuid.UUID, error) {
	accounts, err := ensureChartOfAccounts(ctx, pool, hotelID)
	if err != nil {
		return uuid.Nil, err
	}
	entryID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO accounting_journal_entries (id, hotel_id, entry_date, description, reference, created_at)
		VALUES ($1,$2,CURRENT_DATE,$3,$4,now())`,
		entryID, hotelID, description, reference); err != nil {
		return uuid.Nil, err
	}
	for _, l := range lines {
		if l.debit == 0 && l.credit == 0 {
			continue
		}
		acct, ok := accounts[l.accountCode]
		if !ok {
			continue
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO accounting_journal_lines (id, entry_id, hotel_id, account_id, debit, credit, memo, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,now())`,
			uuid.New(), entryID, hotelID, acct, l.debit, l.credit, l.memo); err != nil {
			return uuid.Nil, err
		}
	}
	return entryID, nil
}

// postSalesToLedger posts a POS/hotel sale as a balanced journal:
//
//	Dr Cash (cash) or Bank (card/upi)   total          (asset up)
//	  Cr Food & Beverage Revenue        subtotal        (revenue up  -> P&L)
//	  Cr GST Payable                    tax             (liability up)
//
// COGS/Inventory are posted separately (needs item cost — Phase 2). round2 keeps it
// balanced when subtotal+tax != total (cash rounding): any residual lands on revenue.
func postSalesToLedger(ctx context.Context, pool *pgxpool.Pool, hotelID uuid.UUID, method string, subtotal, tax, total float64, reference, description string) (uuid.UUID, error) {
	cash := "1010" // bank for card/upi/other
	if method == "cash" {
		cash = "1000"
	}
	// Keep the entry balanced against the amount actually banked (total).
	rev := round2(total - tax)
	_ = subtotal
	return postJournal(ctx, pool, hotelID, description, reference, []journalLine{
		{accountCode: cash, debit: round2(total), memo: "Sale settlement (" + method + ")"},
		{accountCode: "4100", credit: rev, memo: "F&B revenue"},
		{accountCode: "2100", credit: round2(tax), memo: "GST on sale"},
	})
}
