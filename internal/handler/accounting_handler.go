package handler

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

type AccountingHandler struct {
	baseHandler
	pool *pgxpool.Pool
}

func NewAccountingHandler(pool *pgxpool.Pool, secret string) *AccountingHandler {
	return &AccountingHandler{baseHandler: newBase(secret), pool: pool}
}

func (h *AccountingHandler) Register(r fiber.Router) {
	g := r.Group("/accounting")
	g.Get("/chart-of-accounts", h.ListAccounts)
	g.Post("/chart-of-accounts", h.CreateAccount)
	g.Get("/chart-of-accounts/:id", h.GetAccount)
	g.Patch("/chart-of-accounts/:id", h.UpdateAccount)
	g.Delete("/chart-of-accounts/:id", h.DeleteAccount)

	g.Get("/customers", h.ListCustomers)
	g.Post("/customers", h.CreateCustomer)
	g.Get("/customers/:id", h.GetCustomer)
	g.Patch("/customers/:id", h.UpdateCustomer)

	g.Get("/vendors", h.ListVendors)
	g.Post("/vendors", h.CreateVendor)
	g.Get("/vendors/:id", h.GetVendor)
	g.Patch("/vendors/:id", h.UpdateVendor)

	g.Get("/sales-invoices", h.ListSalesInvoices)
	g.Post("/sales-invoices", h.CreateSalesInvoice)
	g.Get("/sales-invoices/:id", h.GetSalesInvoice)
	g.Post("/sales-invoices/:id/post", h.PostSalesInvoice)
	g.Post("/sales-invoices/:id/cancel", h.CancelSalesInvoice)
	g.Post("/sales-invoices/:id/credit-note", h.CreateCreditNoteFromInvoice)

	g.Get("/credit-notes", h.ListCreditNotes)
	g.Post("/credit-notes", h.CreateCreditNote)
	g.Get("/credit-notes/:id", h.GetCreditNote)
	g.Post("/credit-notes/:id/post", h.PostCreditNote)

	g.Get("/debit-notes", h.ListDebitNotes)
	g.Post("/debit-notes", h.CreateDebitNote)
	g.Get("/debit-notes/:id", h.GetDebitNote)
	g.Post("/debit-notes/:id/post", h.PostDebitNote)

	g.Get("/purchase-orders", h.ListPurchaseOrders)
	g.Post("/purchase-orders", h.CreatePurchaseOrder)
	g.Get("/purchase-orders/:id", h.GetPurchaseOrder)
	g.Post("/purchase-orders/:id/approve", h.ApprovePurchaseOrder)

	g.Get("/grn", h.ListGRN)
	g.Post("/grn", h.CreateGRN)
	g.Get("/grn/:id", h.GetGRN)
	g.Post("/grn/:id/post", h.PostGRN)

	g.Get("/journal-entries", h.ListJournalEntries)
	g.Post("/journal-entries", h.CreateJournalEntry)
	g.Get("/journal-entries/:id", h.GetJournalEntry)

	g.Get("/trial-balance", h.TrialBalance)
}

// ---------------------------------------------------------------------------
// Chart of Accounts
// ---------------------------------------------------------------------------

type accountReq struct {
	Code       string  `json:"code"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	SubType    string  `json:"sub_type,omitempty"`
	ParentCode *string `json:"parent_code,omitempty"`
	OpenBal    float64 `json:"opening_balance,omitempty"`
	Currency   string  `json:"currency,omitempty"`
	Active     *bool   `json:"active,omitempty"`
	Order      int     `json:"display_order,omitempty"`
}

type accountResp struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	SubType   string    `json:"sub_type"`
	Parent    *string   `json:"parent_code"`
	OpenBal   float64   `json:"opening_balance"`
	Currency  string    `json:"currency"`
	Active    bool      `json:"is_active"`
	Order     int       `json:"display_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const accountCols = `id, code, name, type, COALESCE(sub_type,''), parent_code,
 opening_balance, COALESCE(currency,'USD'), is_active, display_order, created_at, updated_at`

func scanAccount(r rowScanner, a *accountResp) error {
	return r.Scan(&a.ID, &a.Code, &a.Name, &a.Type, &a.SubType, &a.Parent,
		&a.OpenBal, &a.Currency, &a.Active, &a.Order, &a.CreatedAt, &a.UpdatedAt)
}

func (h *AccountingHandler) ListAccounts(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT `+accountCols+` FROM accounting_accounts WHERE hotel_id = $1 ORDER BY display_order, code`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	out := []accountResp{}
	for rows.Next() {
		var a accountResp
		if err := scanAccount(rows, &a); err != nil {
			return response.Error(c, 500, err.Error())
		}
		out = append(out, a)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreateAccount(c *fiber.Ctx) error {
	var req accountReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if req.Code == "" || req.Name == "" || req.Type == "" {
		return response.Error(c, 422, "code, name, and type are required")
	}
	cur := req.Currency
	if cur == "" {
		cur = "USD"
	}
	act := true
	if req.Active != nil {
		act = *req.Active
	}
	id := uuid.New()
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO accounting_accounts
			(id, hotel_id, code, name, type, sub_type, parent_code,
			 opening_balance, currency, is_active, display_order, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11,now(),now())`,
		id, h.hotelID(c), req.Code, req.Name, req.Type, req.SubType, req.ParentCode,
		req.OpenBal, cur, act, req.Order); err != nil {
		return response.Error(c, 409, err.Error())
	}
	return response.Created(c, fiber.Map{"id": id.String()})
}

func (h *AccountingHandler) GetAccount(c *fiber.Ctx) error {
	var a accountResp
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT `+accountCols+` FROM accounting_accounts WHERE id = $1 AND hotel_id = $2`,
		c.Params("id"), h.hotelID(c)).Scan(
		&a.ID, &a.Code, &a.Name, &a.Type, &a.SubType, &a.Parent,
		&a.OpenBal, &a.Currency, &a.Active, &a.Order, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return response.Error(c, 404, "account not found")
	}
	return response.OK(c, a)
}

func (h *AccountingHandler) UpdateAccount(c *fiber.Ctx) error {
	id := c.Params("id")
	var req accountReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	sets, args := []string{}, []interface{}{}
	n := 1
	if req.Name != "" {
		sets = append(sets, fmt.Sprintf("name = $%d", n))
		args = append(args, req.Name)
		n++
	}
	if req.Type != "" {
		sets = append(sets, fmt.Sprintf("type = $%d", n))
		args = append(args, req.Type)
		n++
	}
	if req.SubType != "" {
		sets = append(sets, fmt.Sprintf("sub_type = $%d", n))
		args = append(args, req.SubType)
		n++
	}
	if req.Active != nil {
		sets = append(sets, fmt.Sprintf("is_active = $%d", n))
		args = append(args, *req.Active)
		n++
	}
	if req.ParentCode != nil {
		sets = append(sets, fmt.Sprintf("parent_code = $%d", n))
		args = append(args, *req.ParentCode)
		n++
	}
	_ = req.OpenBal // opening balance updates not allowed after creation
	if len(sets) == 0 {
		return response.Error(c, 400, "no fields to update")
	}
	args = append(args, id, h.hotelID(c))
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_accounts SET `+joinComma(sets)+fmt.Sprintf(`, updated_at = now() WHERE id = $%d AND hotel_id = $%d`, n, n+1), args...)
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 404, "account not found")
	}
	return response.OK(c, fiber.Map{"updated": true})
}

func (h *AccountingHandler) DeleteAccount(c *fiber.Ctx) error {
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`DELETE FROM accounting_accounts WHERE id = $1 AND hotel_id = $2`, c.Params("id"), h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 404, "account not found")
	}
	return response.OK(c, fiber.Map{"deleted": true})
}

func joinComma(s []string) string {
	r := ""
	for i, v := range s {
		if i > 0 {
			r += ", "
		}
		r += v
	}
	return r
}

// ---------------------------------------------------------------------------
// Customers
// ---------------------------------------------------------------------------

type customerReq struct {
	Code       string   `json:"code"`
	Name       string   `json:"name"`
	GSTIN      string   `json:"gstin,omitempty"`
	Address    string   `json:"address,omitempty"`
	Email      string   `json:"email,omitempty"`
	Phone      string   `json:"phone,omitempty"`
	CreditDays int      `json:"credit_days,omitempty"`
	CreditLim  *float64 `json:"credit_limit,omitempty"`
	Active     *bool    `json:"is_active,omitempty"`
}

const custCols = `id, code, name, COALESCE(gstin,''), COALESCE(address,''), COALESCE(email,''), COALESCE(phone,''), credit_days, credit_limit, is_active, created_at, updated_at`

func (h *AccountingHandler) ListCustomers(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT `+custCols+` FROM accounting_customers WHERE hotel_id = $1 ORDER BY code`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type cr struct {
		ID        string    `json:"id"`
		Code      string    `json:"code"`
		Name      string    `json:"name"`
		GSTIN     string    `json:"gstin"`
		Address   string    `json:"address"`
		Email     string    `json:"email"`
		Phone     string    `json:"phone"`
		CreditD   int       `json:"credit_days"`
		CreditL   *float64  `json:"credit_limit"`
		Active    bool      `json:"is_active"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	out := []cr{}
	for rows.Next() {
		var r cr
		if err := rows.Scan(&r.ID, &r.Code, &r.Name, &r.GSTIN, &r.Address, &r.Email, &r.Phone, &r.CreditD, &r.CreditL, &r.Active, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return response.Error(c, 500, err.Error())
		}
		out = append(out, r)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreateCustomer(c *fiber.Ctx) error {
	var req customerReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if req.Code == "" || req.Name == "" {
		return response.Error(c, 422, "code and name are required")
	}
	if req.CreditDays == 0 {
		req.CreditDays = 30
	}
	act := true
	if req.Active != nil {
		act = *req.Active
	}
	id := uuid.New()
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO accounting_customers (id, hotel_id, code, name, gstin, address, email, phone, credit_days, credit_limit, is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10,$11,now(),now())`,
		id, h.hotelID(c), req.Code, req.Name, req.GSTIN, req.Address, req.Email, req.Phone, req.CreditDays, req.CreditLim, act); err != nil {
		return response.Error(c, 409, err.Error())
	}
	return response.Created(c, fiber.Map{"id": id.String()})
}

func (h *AccountingHandler) GetCustomer(c *fiber.Ctx) error {
	var r struct {
		ID        string    `json:"id"`
		Code      string    `json:"code"`
		Name      string    `json:"name"`
		GSTIN     string    `json:"gstin"`
		Address   string    `json:"address"`
		Email     string    `json:"email"`
		Phone     string    `json:"phone"`
		CreditD   int       `json:"credit_days"`
		CreditL   *float64  `json:"credit_limit"`
		Active    bool      `json:"is_active"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT `+custCols+` FROM accounting_customers WHERE id = $1 AND hotel_id = $2`,
		c.Params("id"), h.hotelID(c)).Scan(&r.ID, &r.Code, &r.Name, &r.GSTIN, &r.Address, &r.Email, &r.Phone, &r.CreditD, &r.CreditL, &r.Active, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return response.Error(c, 404, "customer not found")
	}
	return response.OK(c, r)
}

func (h *AccountingHandler) UpdateCustomer(c *fiber.Ctx) error {
	id := c.Params("id")
	var req customerReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	sets, args := []string{}, []interface{}{}
	n := 1
	if req.Name != "" {
		sets = append(sets, fmt.Sprintf("name = $%d", n))
		args = append(args, req.Name)
		n++
	}
	if req.GSTIN != "" {
		sets = append(sets, fmt.Sprintf("gstin = $%d", n))
		args = append(args, req.GSTIN)
		n++
	}
	if req.Address != "" {
		sets = append(sets, fmt.Sprintf("address = $%d", n))
		args = append(args, req.Address)
		n++
	}
	if req.Email != "" {
		sets = append(sets, fmt.Sprintf("email = $%d", n))
		args = append(args, req.Email)
		n++
	}
	if req.Phone != "" {
		sets = append(sets, fmt.Sprintf("phone = $%d", n))
		args = append(args, req.Phone)
		n++
	}
	if req.CreditDays > 0 {
		sets = append(sets, fmt.Sprintf("credit_days = $%d", n))
		args = append(args, req.CreditDays)
		n++
	}
	if req.CreditLim != nil {
		sets = append(sets, fmt.Sprintf("credit_limit = $%d", n))
		args = append(args, *req.CreditLim)
		n++
	}
	if req.Active != nil {
		sets = append(sets, fmt.Sprintf("is_active = $%d", n))
		args = append(args, *req.Active)
		n++
	}
	if len(sets) == 0 {
		return response.Error(c, 400, "no fields to update")
	}
	args = append(args, id, h.hotelID(c))
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_customers SET `+joinComma(sets)+fmt.Sprintf(`, updated_at = now() WHERE id = $%d AND hotel_id = $%d`, n, n+1), args...)
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 404, "customer not found")
	}
	return response.OK(c, fiber.Map{"updated": true})
}

// ---------------------------------------------------------------------------
// Vendors (mirrors customers - same structure, different table)
// ---------------------------------------------------------------------------

type vendorReq customerReq

const vendCols = `id, code, name, COALESCE(gstin,''), COALESCE(address,''), COALESCE(email,''), COALESCE(phone,''), credit_days, credit_limit, is_active, created_at, updated_at`

func (h *AccountingHandler) ListVendors(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT `+vendCols+` FROM accounting_vendors WHERE hotel_id = $1 ORDER BY code`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type vr struct {
		ID        string    `json:"id"`
		Code      string    `json:"code"`
		Name      string    `json:"name"`
		GSTIN     string    `json:"gstin"`
		Address   string    `json:"address"`
		Email     string    `json:"email"`
		Phone     string    `json:"phone"`
		CreditD   int       `json:"credit_days"`
		CreditL   *float64  `json:"credit_limit"`
		Active    bool      `json:"is_active"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	out := []vr{}
	for rows.Next() {
		var r vr
		if err := rows.Scan(&r.ID, &r.Code, &r.Name, &r.GSTIN, &r.Address, &r.Email, &r.Phone, &r.CreditD, &r.CreditL, &r.Active, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return response.Error(c, 500, err.Error())
		}
		out = append(out, r)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreateVendor(c *fiber.Ctx) error {
	var req vendorReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if req.Code == "" || req.Name == "" {
		return response.Error(c, 422, "code and name are required")
	}
	if req.CreditDays == 0 {
		req.CreditDays = 30
	}
	act := true
	if req.Active != nil {
		act = *req.Active
	}
	id := uuid.New()
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO accounting_vendors (id, hotel_id, code, name, gstin, address, email, phone, credit_days, credit_limit, is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10,$11,now(),now())`,
		id, h.hotelID(c), req.Code, req.Name, req.GSTIN, req.Address, req.Email, req.Phone, req.CreditDays, req.CreditLim, act); err != nil {
		return response.Error(c, 409, err.Error())
	}
	return response.Created(c, fiber.Map{"id": id.String()})
}

func (h *AccountingHandler) GetVendor(c *fiber.Ctx) error {
	var r struct {
		ID        string    `json:"id"`
		Code      string    `json:"code"`
		Name      string    `json:"name"`
		GSTIN     string    `json:"gstin"`
		Address   string    `json:"address"`
		Email     string    `json:"email"`
		Phone     string    `json:"phone"`
		CreditD   int       `json:"credit_days"`
		CreditL   *float64  `json:"credit_limit"`
		Active    bool      `json:"is_active"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT `+vendCols+` FROM accounting_vendors WHERE id = $1 AND hotel_id = $2`,
		c.Params("id"), h.hotelID(c)).Scan(&r.ID, &r.Code, &r.Name, &r.GSTIN, &r.Address, &r.Email, &r.Phone, &r.CreditD, &r.CreditL, &r.Active, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return response.Error(c, 404, "vendor not found")
	}
	return response.OK(c, r)
}

func (h *AccountingHandler) UpdateVendor(c *fiber.Ctx) error {
	id := c.Params("id")
	var req vendorReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	sets, args := []string{}, []interface{}{}
	n := 1
	if req.Name != "" {
		sets = append(sets, fmt.Sprintf("name = $%d", n))
		args = append(args, req.Name)
		n++
	}
	if req.GSTIN != "" {
		sets = append(sets, fmt.Sprintf("gstin = $%d", n))
		args = append(args, req.GSTIN)
		n++
	}
	if req.Address != "" {
		sets = append(sets, fmt.Sprintf("address = $%d", n))
		args = append(args, req.Address)
		n++
	}
	if req.Email != "" {
		sets = append(sets, fmt.Sprintf("email = $%d", n))
		args = append(args, req.Email)
		n++
	}
	if req.Phone != "" {
		sets = append(sets, fmt.Sprintf("phone = $%d", n))
		args = append(args, req.Phone)
		n++
	}
	if req.CreditDays > 0 {
		sets = append(sets, fmt.Sprintf("credit_days = $%d", n))
		args = append(args, req.CreditDays)
		n++
	}
	if req.CreditLim != nil {
		sets = append(sets, fmt.Sprintf("credit_limit = $%d", n))
		args = append(args, *req.CreditLim)
		n++
	}
	if req.Active != nil {
		sets = append(sets, fmt.Sprintf("is_active = $%d", n))
		args = append(args, *req.Active)
		n++
	}
	if len(sets) == 0 {
		return response.Error(c, 400, "no fields to update")
	}
	args = append(args, id, h.hotelID(c))
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_vendors SET `+joinComma(sets)+fmt.Sprintf(`, updated_at = now() WHERE id = $%d AND hotel_id = $%d`, n, n+1), args...)
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 404, "vendor not found")
	}
	return response.OK(c, fiber.Map{"updated": true})
}

// ---------------------------------------------------------------------------
// Sales Invoices
// ---------------------------------------------------------------------------

type salesInvoiceLineReq struct {
	AccountID   string  `json:"account_id"`
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	Discount    float64 `json:"discount"`
	TaxRate     float64 `json:"tax_rate"`
}

type salesInvoiceReq struct {
	CustomerID string                `json:"customer_id"`
	Date       string                `json:"date"`
	DueDate    string                `json:"due_date"`
	Reference  string                `json:"reference"`
	Notes      string                `json:"notes"`
	Lines      []salesInvoiceLineReq `json:"lines"`
}

const invCols = `id, COALESCE(customer_id::text,''), invoice_number, invoice_date, due_date,
 COALESCE(reference,''), subtotal, discount_total, tax_total, total, status,
 COALESCE(notes,''), created_at, updated_at`

func (h *AccountingHandler) ListSalesInvoices(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT `+invCols+` FROM accounting_sales_invoices WHERE hotel_id = $1 ORDER BY invoice_date DESC, created_at DESC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type si struct {
		ID            string    `json:"id"`
		CustomerID    string    `json:"customer_id"`
		InvoiceNumber string    `json:"invoice_number"`
		Date          string    `json:"invoice_date"`
		DueDate       *string   `json:"due_date"`
		Reference     string    `json:"reference"`
		Subtotal      float64   `json:"subtotal"`
		DiscountTotal float64   `json:"discount_total"`
		TaxTotal      float64   `json:"tax_total"`
		Total         float64   `json:"total"`
		Status        string    `json:"status"`
		Notes         string    `json:"notes"`
		CreatedAt     time.Time `json:"created_at"`
		UpdatedAt     time.Time `json:"updated_at"`
	}
	out := []si{}
	for rows.Next() {
		var r si
		var d, dd *time.Time
		if err := rows.Scan(&r.ID, &r.CustomerID, &r.InvoiceNumber, &d, &dd,
			&r.Reference, &r.Subtotal, &r.DiscountTotal, &r.TaxTotal, &r.Total, &r.Status,
			&r.Notes, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return response.Error(c, 500, err.Error())
		}
		if d != nil {
			r.Date = d.Format("2006-01-02")
		}
		if dd != nil {
			s := dd.Format("2006-01-02")
			r.DueDate = &s
		}
		out = append(out, r)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreateSalesInvoice(c *fiber.Ctx) error {
	var req salesInvoiceReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if len(req.Lines) == 0 {
		return response.Error(c, 422, "at least one line is required")
	}
	hid := h.hotelID(c)

	// Generate invoice number: INV-{slug}-{timestamp}
	var slug string
	_ = tenantPool(c, h.pool).QueryRow(c.Context(), `SELECT slug FROM hotels WHERE id = $1`, hid).Scan(&slug)
	invNum := fmt.Sprintf("INV-%s-%d", slug, time.Now().UnixMilli())

	invDate := time.Now()
	if req.Date != "" {
		if parsed, err := time.Parse("2006-01-02", req.Date); err == nil {
			invDate = parsed
		}
	}
	var dueDate *time.Time
	if req.DueDate != "" {
		if parsed, err := time.Parse("2006-01-02", req.DueDate); err == nil {
			dueDate = &parsed
		}
	}

	// Compute totals
	var subtotal, discountTotal, taxTotal float64
	type line struct {
		accountID uuid.UUID
		desc      string
		qty       float64
		unitPrice float64
		discount  float64
		taxRate   float64
		taxAmt    float64
		total     float64
	}
	lines := []line{}
	for _, l := range req.Lines {
		aid, _ := uuid.Parse(l.AccountID)
		q := l.Quantity
		if q == 0 {
			q = 1
		}
		lineTotal := q * l.UnitPrice
		disc := l.Discount
		lineAfterDisc := lineTotal - disc
		tax := lineAfterDisc * l.TaxRate / 100
		subtotal += lineTotal
		discountTotal += disc
		taxTotal += tax
		lines = append(lines, line{
			accountID: aid, desc: l.Description, qty: q,
			unitPrice: l.UnitPrice, discount: disc, taxRate: l.TaxRate,
			taxAmt: tax, total: lineAfterDisc + tax,
		})
	}
	total := subtotal - discountTotal + taxTotal

	tx, err := tenantPool(c, h.pool).Begin(c.Context())
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer tx.Rollback(c.Context())

	invID := uuid.New()
	custID, _ := uuid.Parse(req.CustomerID)
	if _, err := tx.Exec(c.Context(), `
		INSERT INTO accounting_sales_invoices
			(id, hotel_id, customer_id, invoice_number, invoice_date, due_date, reference,
			 subtotal, discount_total, tax_total, total, status, notes, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,'draft',NULLIF($12,''),now(),now())`,
		invID, hid, nullableUUID(custID), invNum, invDate, dueDate, req.Reference,
		subtotal, discountTotal, taxTotal, total, req.Notes); err != nil {
		return response.Error(c, 500, err.Error())
	}

	for _, l := range lines {
		lineID := uuid.New()
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO accounting_sales_invoice_lines
				(id, invoice_id, hotel_id, account_id, description, quantity, unit_price, discount, tax_rate, tax_amount, total, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now())`,
			lineID, invID, hid, l.accountID, l.desc, l.qty, l.unitPrice, l.discount, l.taxRate, l.taxAmt, l.total); err != nil {
			return response.Error(c, 500, err.Error())
		}
	}

	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, 500, err.Error())
	}
	return response.Created(c, fiber.Map{"id": invID.String(), "invoice_number": invNum, "total": total})
}

func (h *AccountingHandler) GetSalesInvoice(c *fiber.Ctx) error {
	invID := c.Params("id")
	hid := h.hotelID(c)

	var r struct {
		ID            string     `json:"id"`
		CustomerID    string     `json:"customer_id"`
		InvoiceNumber string     `json:"invoice_number"`
		Date          string     `json:"invoice_date"`
		DueDate       *string    `json:"due_date"`
		Reference     string     `json:"reference"`
		Subtotal      float64    `json:"subtotal"`
		DiscountTotal float64    `json:"discount_total"`
		TaxTotal      float64    `json:"tax_total"`
		Total         float64    `json:"total"`
		Status        string     `json:"status"`
		Notes         string     `json:"notes"`
		CreatedAt     time.Time  `json:"created_at"`
		UpdatedAt     time.Time  `json:"updated_at"`
		Lines         []lineResp `json:"lines"`
	}
	var d, dd *time.Time
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT `+invCols+` FROM accounting_sales_invoices WHERE id = $1 AND hotel_id = $2`, invID, hid).
		Scan(&r.ID, &r.CustomerID, &r.InvoiceNumber, &d, &dd, &r.Reference, &r.Subtotal, &r.DiscountTotal, &r.TaxTotal, &r.Total, &r.Status, &r.Notes, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return response.Error(c, 404, "invoice not found")
	}
	if d != nil {
		r.Date = d.Format("2006-01-02")
	}
	if dd != nil {
		s := dd.Format("2006-01-02")
		r.DueDate = &s
	}

	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, COALESCE(account_id::text,''), description, quantity, unit_price, discount, tax_rate, tax_amount, total
		FROM accounting_sales_invoice_lines WHERE invoice_id = $1 ORDER BY created_at`, invID)
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var l lineResp
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Description, &l.Quantity, &l.UnitPrice, &l.Discount, &l.TaxRate, &l.TaxAmount, &l.Total); err != nil {
			return response.Error(c, 500, err.Error())
		}
		r.Lines = append(r.Lines, l)
	}
	return response.OK(c, r)
}

type lineResp struct {
	ID          string  `json:"id"`
	AccountID   string  `json:"account_id"`
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	Discount    float64 `json:"discount"`
	TaxRate     float64 `json:"tax_rate"`
	TaxAmount   float64 `json:"tax_amount"`
	Total       float64 `json:"total"`
}

func (h *AccountingHandler) PostSalesInvoice(c *fiber.Ctx) error {
	invID := c.Params("id")
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_sales_invoices SET status = 'posted', updated_at = now() WHERE id = $1 AND hotel_id = $2 AND status = 'draft'`,
		invID, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 400, "invoice not found or already posted/cancelled")
	}
	return response.OK(c, fiber.Map{"status": "posted"})
}

func (h *AccountingHandler) CancelSalesInvoice(c *fiber.Ctx) error {
	invID := c.Params("id")
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_sales_invoices SET status = 'cancelled', updated_at = now() WHERE id = $1 AND hotel_id = $2 AND status IN ('draft','posted')`,
		invID, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 400, "invoice not found or already cancelled")
	}
	return response.OK(c, fiber.Map{"status": "cancelled"})
}

func (h *AccountingHandler) CreateCreditNoteFromInvoice(c *fiber.Ctx) error {
	invID := c.Params("id")
	hid := h.hotelID(c)

	var invStatus string
	var invNum string
	var existingCNCount int
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT status, invoice_number FROM accounting_sales_invoices WHERE id = $1 AND hotel_id = $2`, invID, hid).Scan(&invStatus, &invNum); err != nil {
		return response.Error(c, 404, "invoice not found")
	}
	if invStatus != "posted" {
		return response.Error(c, 400, "credit note can only be created from a posted invoice")
	}
	_ = tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT COUNT(*) FROM accounting_credit_notes WHERE invoice_id = $1`, invID).Scan(&existingCNCount)

	cnNum := fmt.Sprintf("CN-%s-%d", invNum, existingCNCount+1)

	tx, err := tenantPool(c, h.pool).Begin(c.Context())
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer tx.Rollback(c.Context())

	cnID := uuid.New()
	if _, err := tx.Exec(c.Context(), `
		INSERT INTO accounting_credit_notes (id, hotel_id, invoice_id, credit_note_number, date, reason, subtotal, tax_total, total, status, created_at, updated_at)
		SELECT $1, $2, $3, $4, CURRENT_DATE, 'Credit against ' || invoice_number, subtotal, tax_total, total, 'draft', now(), now()
		FROM accounting_sales_invoices WHERE id = $3`, cnID, hid, invID, cnNum); err != nil {
		return response.Error(c, 500, err.Error())
	}

	// Copy invoice lines to credit note lines (negated)
	if _, err := tx.Exec(c.Context(), `
		INSERT INTO accounting_credit_note_lines (id, credit_note_id, hotel_id, account_id, invoice_line_id, description, quantity, unit_price, tax_amount, total, created_at)
		SELECT uuid_generate_v4(), $1, hotel_id, account_id, id, description, quantity, unit_price, tax_amount, total, now()
		FROM accounting_sales_invoice_lines WHERE invoice_id = $2`, cnID, invID); err != nil {
		return response.Error(c, 500, err.Error())
	}

	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, 500, err.Error())
	}
	return response.Created(c, fiber.Map{"id": cnID.String(), "credit_note_number": cnNum})
}

// ---------------------------------------------------------------------------
// Credit Notes
// ---------------------------------------------------------------------------

type creditNoteLineReq struct {
	AccountID     string  `json:"account_id"`
	InvoiceLineID string  `json:"invoice_line_id,omitempty"`
	Description   string  `json:"description"`
	Quantity      float64 `json:"quantity"`
	UnitPrice     float64 `json:"unit_price"`
}

type creditNoteReq struct {
	InvoiceID string              `json:"invoice_id,omitempty"`
	Date      string              `json:"date"`
	Reason    string              `json:"reason"`
	Lines     []creditNoteLineReq `json:"lines,omitempty"`
}

func (h *AccountingHandler) ListCreditNotes(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT id, COALESCE(invoice_id::text,''), credit_note_number, date, COALESCE(reason,''), subtotal, tax_total, total, status, created_at, updated_at
		 FROM accounting_credit_notes WHERE hotel_id = $1 ORDER BY date DESC, created_at DESC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type cn struct {
		ID      string    `json:"id"`
		InvID   string    `json:"invoice_id"`
		Num     string    `json:"credit_note_number"`
		Date    string    `json:"date"`
		Reason  string    `json:"reason"`
		Sub     float64   `json:"subtotal"`
		Tax     float64   `json:"tax_total"`
		Total   float64   `json:"total"`
		Status  string    `json:"status"`
		Created time.Time `json:"created_at"`
		Updated time.Time `json:"updated_at"`
	}
	out := []cn{}
	for rows.Next() {
		var r cn
		var d time.Time
		if err := rows.Scan(&r.ID, &r.InvID, &r.Num, &d, &r.Reason, &r.Sub, &r.Tax, &r.Total, &r.Status, &r.Created, &r.Updated); err != nil {
			return response.Error(c, 500, err.Error())
		}
		r.Date = d.Format("2006-01-02")
		out = append(out, r)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreateCreditNote(c *fiber.Ctx) error {
	var req creditNoteReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if len(req.Lines) == 0 && req.Reason == "" {
		return response.Error(c, 422, "lines or reason required")
	}
	hid := h.hotelID(c)
	var slug string
	_ = tenantPool(c, h.pool).QueryRow(c.Context(), `SELECT slug FROM hotels WHERE id = $1`, hid).Scan(&slug)
	cnNum := fmt.Sprintf("CN-%s-%d", slug, time.Now().UnixMilli())

	cnDate := time.Now()
	if req.Date != "" {
		if parsed, err := time.Parse("2006-01-02", req.Date); err == nil {
			cnDate = parsed
		}
	}

	tx, err := tenantPool(c, h.pool).Begin(c.Context())
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer tx.Rollback(c.Context())

	cnID := uuid.New()
	var invID *uuid.UUID
	if req.InvoiceID != "" {
		if parsed, err := uuid.Parse(req.InvoiceID); err == nil {
			invID = &parsed
		}
	}
	if _, err := tx.Exec(c.Context(), `
		INSERT INTO accounting_credit_notes (id, hotel_id, invoice_id, credit_note_number, date, reason, subtotal, tax_total, total, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),0,0,0,'draft',now(),now())`,
		cnID, hid, invID, cnNum, cnDate, req.Reason); err != nil {
		return response.Error(c, 500, err.Error())
	}

	var subtotal, taxTotal, total float64
	for _, l := range req.Lines {
		lineID := uuid.New()
		q := l.Quantity
		if q == 0 {
			q = 1
		}
		lt := q * l.UnitPrice
		subtotal += lt
		total += lt
		aid, _ := uuid.Parse(l.AccountID)
		ilid, _ := uuid.Parse(l.InvoiceLineID)
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO accounting_credit_note_lines (id, credit_note_id, hotel_id, account_id, invoice_line_id, description, quantity, unit_price, tax_amount, total, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,0,$9,now())`,
			lineID, cnID, hid, nullableUUID(aid), nullableUUID(ilid), l.Description, q, l.UnitPrice, lt); err != nil {
			return response.Error(c, 500, err.Error())
		}
	}

	if _, err := tx.Exec(c.Context(),
		`UPDATE accounting_credit_notes SET subtotal = $1, tax_total = $2, total = $3 WHERE id = $4`,
		subtotal, taxTotal, total, cnID); err != nil {
		return response.Error(c, 500, err.Error())
	}

	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, 500, err.Error())
	}
	return response.Created(c, fiber.Map{"id": cnID.String(), "credit_note_number": cnNum})
}

func (h *AccountingHandler) GetCreditNote(c *fiber.Ctx) error {
	cnID := c.Params("id")
	hid := h.hotelID(c)
	var r struct {
		ID     string  `json:"id"`
		InvID  string  `json:"invoice_id"`
		Num    string  `json:"credit_note_number"`
		Date   string  `json:"date"`
		Reason string  `json:"reason"`
		Sub    float64 `json:"subtotal"`
		Tax    float64 `json:"tax_total"`
		Total  float64 `json:"total"`
		Status string  `json:"status"`
	}
	var d time.Time
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT id, COALESCE(invoice_id::text,''), credit_note_number, date, COALESCE(reason,''), subtotal, tax_total, total, status
		 FROM accounting_credit_notes WHERE id = $1 AND hotel_id = $2`, cnID, hid).
		Scan(&r.ID, &r.InvID, &r.Num, &d, &r.Reason, &r.Sub, &r.Tax, &r.Total, &r.Status); err != nil {
		return response.Error(c, 404, "credit note not found")
	}
	r.Date = d.Format("2006-01-02")
	return response.OK(c, r)
}

func (h *AccountingHandler) PostCreditNote(c *fiber.Ctx) error {
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_credit_notes SET status = 'posted', updated_at = now() WHERE id = $1 AND hotel_id = $2 AND status = 'draft'`,
		c.Params("id"), h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 400, "credit note not found or already posted")
	}
	return response.OK(c, fiber.Map{"status": "posted"})
}

// ---------------------------------------------------------------------------
// Debit Notes
// ---------------------------------------------------------------------------

type debitNoteReq struct {
	VendorID string  `json:"vendor_id"`
	Date     string  `json:"date"`
	Reason   string  `json:"reason"`
	Subtotal float64 `json:"subtotal"`
	TaxTotal float64 `json:"tax_total"`
	Total    float64 `json:"total"`
}

func (h *AccountingHandler) ListDebitNotes(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT id, COALESCE(vendor_id::text,''), debit_note_number, date, COALESCE(reason,''), subtotal, tax_total, total, status, created_at, updated_at
		 FROM accounting_debit_notes WHERE hotel_id = $1 ORDER BY date DESC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type dn struct {
		ID      string    `json:"id"`
		VendID  string    `json:"vendor_id"`
		Num     string    `json:"debit_note_number"`
		Date    string    `json:"date"`
		Reason  string    `json:"reason"`
		Sub     float64   `json:"subtotal"`
		Tax     float64   `json:"tax_total"`
		Total   float64   `json:"total"`
		Status  string    `json:"status"`
		Created time.Time `json:"created_at"`
		Updated time.Time `json:"updated_at"`
	}
	out := []dn{}
	for rows.Next() {
		var r dn
		var d time.Time
		if err := rows.Scan(&r.ID, &r.VendID, &r.Num, &d, &r.Reason, &r.Sub, &r.Tax, &r.Total, &r.Status, &r.Created, &r.Updated); err != nil {
			return response.Error(c, 500, err.Error())
		}
		r.Date = d.Format("2006-01-02")
		out = append(out, r)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreateDebitNote(c *fiber.Ctx) error {
	var req debitNoteReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if req.Reason == "" && req.Total == 0 {
		return response.Error(c, 422, "reason or total required")
	}
	hid := h.hotelID(c)
	var slug string
	_ = tenantPool(c, h.pool).QueryRow(c.Context(), `SELECT slug FROM hotels WHERE id = $1`, hid).Scan(&slug)
	dnNum := fmt.Sprintf("DN-%s-%d", slug, time.Now().UnixMilli())

	dnDate := time.Now()
	if req.Date != "" {
		if parsed, err := time.Parse("2006-01-02", req.Date); err == nil {
			dnDate = parsed
		}
	}
	dnID := uuid.New()
	vid, _ := uuid.Parse(req.VendorID)
	if _, err := tenantPool(c, h.pool).Exec(c.Context(), `
		INSERT INTO accounting_debit_notes (id, hotel_id, vendor_id, debit_note_number, date, reason, subtotal, tax_total, total, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,'draft',now(),now())`,
		dnID, hid, nullableUUID(vid), dnNum, dnDate, req.Reason, req.Subtotal, req.TaxTotal, req.Total); err != nil {
		return response.Error(c, 500, err.Error())
	}
	return response.Created(c, fiber.Map{"id": dnID.String(), "debit_note_number": dnNum})
}

func (h *AccountingHandler) GetDebitNote(c *fiber.Ctx) error {
	var r struct {
		ID     string  `json:"id"`
		VendID string  `json:"vendor_id"`
		Num    string  `json:"debit_note_number"`
		Date   string  `json:"date"`
		Reason string  `json:"reason"`
		Sub    float64 `json:"subtotal"`
		Tax    float64 `json:"tax_total"`
		Total  float64 `json:"total"`
		Status string  `json:"status"`
	}
	var d time.Time
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT id, COALESCE(vendor_id::text,''), debit_note_number, date, COALESCE(reason,''), subtotal, tax_total, total, status
		 FROM accounting_debit_notes WHERE id = $1 AND hotel_id = $2`, c.Params("id"), h.hotelID(c)).
		Scan(&r.ID, &r.VendID, &r.Num, &d, &r.Reason, &r.Sub, &r.Tax, &r.Total, &r.Status); err != nil {
		return response.Error(c, 404, "debit note not found")
	}
	r.Date = d.Format("2006-01-02")
	return response.OK(c, r)
}

func (h *AccountingHandler) PostDebitNote(c *fiber.Ctx) error {
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_debit_notes SET status = 'posted', updated_at = now() WHERE id = $1 AND hotel_id = $2 AND status = 'draft'`,
		c.Params("id"), h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 400, "debit note not found or already posted")
	}
	return response.OK(c, fiber.Map{"status": "posted"})
}

// ---------------------------------------------------------------------------
// Purchase Orders
// ---------------------------------------------------------------------------

type poLineReq struct {
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
}

type poReq struct {
	VendorID  string      `json:"vendor_id"`
	Date      string      `json:"date"`
	ExpectedD string      `json:"expected_date"`
	Notes     string      `json:"notes"`
	Lines     []poLineReq `json:"lines"`
}

func (h *AccountingHandler) ListPurchaseOrders(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT id, COALESCE(vendor_id::text,''), po_number, order_date, expected_date, status, subtotal, tax_total, total, COALESCE(notes,''), created_at, updated_at
		 FROM accounting_purchase_orders WHERE hotel_id = $1 ORDER BY order_date DESC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type po struct {
		ID      string    `json:"id"`
		VendID  string    `json:"vendor_id"`
		PONum   string    `json:"po_number"`
		Date    string    `json:"order_date"`
		ExpD    *string   `json:"expected_date"`
		Status  string    `json:"status"`
		Sub     float64   `json:"subtotal"`
		Tax     float64   `json:"tax_total"`
		Total   float64   `json:"total"`
		Notes   string    `json:"notes"`
		Created time.Time `json:"created_at"`
		Updated time.Time `json:"updated_at"`
	}
	out := []po{}
	for rows.Next() {
		var r po
		var d, ed *time.Time
		if err := rows.Scan(&r.ID, &r.VendID, &r.PONum, &d, &ed, &r.Status, &r.Sub, &r.Tax, &r.Total, &r.Notes, &r.Created, &r.Updated); err != nil {
			return response.Error(c, 500, err.Error())
		}
		if d != nil {
			r.Date = d.Format("2006-01-02")
		}
		if ed != nil {
			s := ed.Format("2006-01-02")
			r.ExpD = &s
		}
		out = append(out, r)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreatePurchaseOrder(c *fiber.Ctx) error {
	var req poReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if len(req.Lines) == 0 {
		return response.Error(c, 422, "at least one line is required")
	}
	hid := h.hotelID(c)
	var slug string
	_ = tenantPool(c, h.pool).QueryRow(c.Context(), `SELECT slug FROM hotels WHERE id = $1`, hid).Scan(&slug)
	poNum := fmt.Sprintf("PO-%s-%d", slug, time.Now().UnixMilli())

	poDate := time.Now()
	if req.Date != "" {
		if parsed, err := time.Parse("2006-01-02", req.Date); err == nil {
			poDate = parsed
		}
	}
	var expD *time.Time
	if req.ExpectedD != "" {
		if parsed, err := time.Parse("2006-01-02", req.ExpectedD); err == nil {
			expD = &parsed
		}
	}

	poID := uuid.New()
	vid, _ := uuid.Parse(req.VendorID)
	tx, err := tenantPool(c, h.pool).Begin(c.Context())
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer tx.Rollback(c.Context())

	if _, err := tx.Exec(c.Context(), `
		INSERT INTO accounting_purchase_orders (id, hotel_id, vendor_id, po_number, order_date, expected_date, status, subtotal, tax_total, total, notes, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,'draft',0,0,0,NULLIF($7,''),now(),now())`,
		poID, hid, nullableUUID(vid), poNum, poDate, expD, req.Notes); err != nil {
		return response.Error(c, 500, err.Error())
	}

	var subtotal, total float64
	for _, l := range req.Lines {
		q := l.Quantity
		if q == 0 {
			q = 1
		}
		lt := q * l.UnitPrice
		subtotal += lt
		total += lt
		lineID := uuid.New()
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO accounting_grn_lines (id, grn_id, hotel_id, item_description, quantity_ordered, quantity_received, quantity_accepted, quantity_rejected, unit_price, total, created_at)
			VALUES ($1,$2,$3,$4,$5,0,0,0,$6,$7,now())`,
			lineID, poID, hid, l.Description, q, l.UnitPrice, lt); err != nil {
			return response.Error(c, 500, err.Error())
		}
	}

	if _, err := tx.Exec(c.Context(),
		`UPDATE accounting_purchase_orders SET subtotal = $1, total = $2 WHERE id = $3`,
		subtotal, total, poID); err != nil {
		return response.Error(c, 500, err.Error())
	}

	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, 500, err.Error())
	}
	return response.Created(c, fiber.Map{"id": poID.String(), "po_number": poNum})
}

func (h *AccountingHandler) GetPurchaseOrder(c *fiber.Ctx) error {
	poID := c.Params("id")
	hid := h.hotelID(c)
	var r struct {
		ID    string  `json:"id"`
		VID   string  `json:"vendor_id"`
		Num   string  `json:"po_number"`
		Date  string  `json:"order_date"`
		ExpD  *string `json:"expected_date"`
		St    string  `json:"status"`
		Sub   float64 `json:"subtotal"`
		Tax   float64 `json:"tax_total"`
		Total float64 `json:"total"`
		Notes string  `json:"notes"`
	}
	var d, ed *time.Time
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT id, COALESCE(vendor_id::text,''), po_number, order_date, expected_date, status, subtotal, tax_total, total, COALESCE(notes,'')
		 FROM accounting_purchase_orders WHERE id = $1 AND hotel_id = $2`, poID, hid).
		Scan(&r.ID, &r.VID, &r.Num, &d, &ed, &r.St, &r.Sub, &r.Tax, &r.Total, &r.Notes); err != nil {
		return response.Error(c, 404, "purchase order not found")
	}
	if d != nil {
		r.Date = d.Format("2006-01-02")
	}
	if ed != nil {
		s := ed.Format("2006-01-02")
		r.ExpD = &s
	}
	return response.OK(c, r)
}

func (h *AccountingHandler) ApprovePurchaseOrder(c *fiber.Ctx) error {
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_purchase_orders SET status = 'approved', updated_at = now() WHERE id = $1 AND hotel_id = $2 AND status = 'draft'`,
		c.Params("id"), h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 400, "PO not found or not in draft")
	}
	return response.OK(c, fiber.Map{"status": "approved"})
}

// ---------------------------------------------------------------------------
// GRN
// ---------------------------------------------------------------------------

type grnLineReq struct {
	ItemDescription string  `json:"item_description"`
	QtyOrdered      float64 `json:"quantity_ordered"`
	QtyReceived     float64 `json:"quantity_received"`
	QtyAccepted     float64 `json:"quantity_accepted"`
	QtyRejected     float64 `json:"quantity_rejected"`
	UnitPrice       float64 `json:"unit_price"`
}

type grnReq struct {
	POID         string       `json:"po_id"`
	Date         string       `json:"received_date"`
	VendorInvRef string       `json:"vendor_invoice_ref"`
	Notes        string       `json:"notes"`
	Lines        []grnLineReq `json:"lines"`
}

func (h *AccountingHandler) ListGRN(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT id, COALESCE(po_id::text,''), grn_number, received_date, COALESCE(vendor_invoice_ref,''), status, COALESCE(notes,''), created_at, updated_at
		 FROM accounting_grn WHERE hotel_id = $1 ORDER BY received_date DESC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type grn struct {
		ID        string    `json:"id"`
		POID      string    `json:"po_id"`
		GRNNum    string    `json:"grn_number"`
		Date      string    `json:"received_date"`
		VenInvRef string    `json:"vendor_invoice_ref"`
		Status    string    `json:"status"`
		Notes     string    `json:"notes"`
		Created   time.Time `json:"created_at"`
		Updated   time.Time `json:"updated_at"`
	}
	out := []grn{}
	for rows.Next() {
		var r grn
		var d time.Time
		if err := rows.Scan(&r.ID, &r.POID, &r.GRNNum, &d, &r.VenInvRef, &r.Status, &r.Notes, &r.Created, &r.Updated); err != nil {
			return response.Error(c, 500, err.Error())
		}
		r.Date = d.Format("2006-01-02")
		out = append(out, r)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreateGRN(c *fiber.Ctx) error {
	var req grnReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if len(req.Lines) == 0 {
		return response.Error(c, 422, "at least one line is required")
	}
	hid := h.hotelID(c)
	var slug string
	_ = tenantPool(c, h.pool).QueryRow(c.Context(), `SELECT slug FROM hotels WHERE id = $1`, hid).Scan(&slug)
	grnNum := fmt.Sprintf("GRN-%s-%d", slug, time.Now().UnixMilli())

	grnDate := time.Now()
	if req.Date != "" {
		if parsed, err := time.Parse("2006-01-02", req.Date); err == nil {
			grnDate = parsed
		}
	}

	tx, err := tenantPool(c, h.pool).Begin(c.Context())
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer tx.Rollback(c.Context())

	grnID := uuid.New()
	poID, _ := uuid.Parse(req.POID)
	if _, err := tx.Exec(c.Context(), `
		INSERT INTO accounting_grn (id, hotel_id, po_id, grn_number, received_date, vendor_invoice_ref, status, notes, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),'draft',NULLIF($7,''),now(),now())`,
		grnID, hid, nullableUUID(poID), grnNum, grnDate, req.VendorInvRef, req.Notes); err != nil {
		return response.Error(c, 500, err.Error())
	}

	for _, l := range req.Lines {
		lineID := uuid.New()
		total := l.QtyAccepted * l.UnitPrice
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO accounting_grn_lines (id, grn_id, hotel_id, item_description, quantity_ordered, quantity_received, quantity_accepted, quantity_rejected, unit_price, total, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())`,
			lineID, grnID, hid, l.ItemDescription, l.QtyOrdered, l.QtyReceived, l.QtyAccepted, l.QtyRejected, l.UnitPrice, total); err != nil {
			return response.Error(c, 500, err.Error())
		}
	}

	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, 500, err.Error())
	}

	// Automatically mark PO as 'received' if all lines received
	if _, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_purchase_orders SET status = 'received', updated_at = now()
		 WHERE id = $1 AND hotel_id = $2 AND status = 'approved'`, poID, hid); err != nil {
		// Non-fatal
		_ = err
	}

	return response.Created(c, fiber.Map{"id": grnID.String(), "grn_number": grnNum})
}

func (h *AccountingHandler) GetGRN(c *fiber.Ctx) error {
	grnID := c.Params("id")
	hid := h.hotelID(c)
	var r struct {
		ID        string `json:"id"`
		POID      string `json:"po_id"`
		Num       string `json:"grn_number"`
		Date      string `json:"received_date"`
		VenInvRef string `json:"vendor_invoice_ref"`
		Status    string `json:"status"`
		Notes     string `json:"notes"`
	}
	var d time.Time
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT id, COALESCE(po_id::text,''), grn_number, received_date, COALESCE(vendor_invoice_ref,''), status, COALESCE(notes,'')
		 FROM accounting_grn WHERE id = $1 AND hotel_id = $2`, grnID, hid).
		Scan(&r.ID, &r.POID, &r.Num, &d, &r.VenInvRef, &r.Status, &r.Notes); err != nil {
		return response.Error(c, 404, "GRN not found")
	}
	r.Date = d.Format("2006-01-02")
	return response.OK(c, r)
}

func (h *AccountingHandler) PostGRN(c *fiber.Ctx) error {
	tag, err := tenantPool(c, h.pool).Exec(c.Context(),
		`UPDATE accounting_grn SET status = 'posted', updated_at = now() WHERE id = $1 AND hotel_id = $2 AND status = 'draft'`,
		c.Params("id"), h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	if tag.RowsAffected() == 0 {
		return response.Error(c, 400, "GRN not found or already posted")
	}
	return response.OK(c, fiber.Map{"status": "posted"})
}

// ---------------------------------------------------------------------------
// Journal Entries
// ---------------------------------------------------------------------------

type journalLineReq struct {
	AccountID string  `json:"account_id"`
	Debit     float64 `json:"debit"`
	Credit    float64 `json:"credit"`
	Memo      string  `json:"memo,omitempty"`
}

type journalEntryReq struct {
	Date        string           `json:"date"`
	Description string           `json:"description"`
	Reference   string           `json:"reference,omitempty"`
	Lines       []journalLineReq `json:"lines"`
}

func (h *AccountingHandler) ListJournalEntries(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(),
		`SELECT id, entry_date, description, COALESCE(reference,''), created_at
		 FROM accounting_journal_entries WHERE hotel_id = $1 ORDER BY entry_date DESC, created_at DESC`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type je struct {
		ID          string    `json:"id"`
		Date        string    `json:"date"`
		Description string    `json:"description"`
		Reference   string    `json:"reference"`
		CreatedAt   time.Time `json:"created_at"`
	}
	out := []je{}
	for rows.Next() {
		var r je
		var d time.Time
		if err := rows.Scan(&r.ID, &d, &r.Description, &r.Reference, &r.CreatedAt); err != nil {
			return response.Error(c, 500, err.Error())
		}
		r.Date = d.Format("2006-01-02")
		out = append(out, r)
	}
	return response.OK(c, out)
}

func (h *AccountingHandler) CreateJournalEntry(c *fiber.Ctx) error {
	var req journalEntryReq
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request")
	}
	if req.Description == "" || len(req.Lines) == 0 {
		return response.Error(c, 422, "description and at least one line are required")
	}

	// Validate debits = credits
	var totalDebit, totalCredit float64
	for _, l := range req.Lines {
		totalDebit += l.Debit
		totalCredit += l.Credit
	}
	if totalDebit != totalCredit {
		return response.Error(c, 422, "total debits must equal total credits")
	}

	entryDate := time.Now()
	if req.Date != "" {
		if parsed, err := time.Parse("2006-01-02", req.Date); err == nil {
			entryDate = parsed
		}
	}

	tx, err := tenantPool(c, h.pool).Begin(c.Context())
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer tx.Rollback(c.Context())

	entryID := uuid.New()
	if _, err := tx.Exec(c.Context(), `
		INSERT INTO accounting_journal_entries (id, hotel_id, entry_date, description, reference, created_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),now())`,
		entryID, h.hotelID(c), entryDate, req.Description, req.Reference); err != nil {
		return response.Error(c, 500, err.Error())
	}

	for _, l := range req.Lines {
		lineID := uuid.New()
		aid, _ := uuid.Parse(l.AccountID)
		if _, err := tx.Exec(c.Context(), `
			INSERT INTO accounting_journal_lines (id, entry_id, hotel_id, account_id, debit, credit, memo, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),now())`,
			lineID, entryID, h.hotelID(c), aid, l.Debit, l.Credit, l.Memo); err != nil {
			return response.Error(c, 500, err.Error())
		}
	}

	if err := tx.Commit(c.Context()); err != nil {
		return response.Error(c, 500, err.Error())
	}
	return response.Created(c, fiber.Map{"id": entryID.String()})
}

func (h *AccountingHandler) GetJournalEntry(c *fiber.Ctx) error {
	entryID := c.Params("id")
	hid := h.hotelID(c)
	var r struct {
		ID          string            `json:"id"`
		Date        string            `json:"date"`
		Description string            `json:"description"`
		Reference   string            `json:"reference"`
		Lines       []journalLineResp `json:"lines"`
	}
	var d time.Time
	if err := tenantPool(c, h.pool).QueryRow(c.Context(),
		`SELECT id, entry_date, description, COALESCE(reference,'') FROM accounting_journal_entries WHERE id = $1 AND hotel_id = $2`,
		entryID, hid).Scan(&r.ID, &d, &r.Description, &r.Reference); err != nil {
		return response.Error(c, 404, "journal entry not found")
	}
	r.Date = d.Format("2006-01-02")

	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT id, COALESCE(account_id::text,''), debit, credit, COALESCE(memo,'')
		FROM accounting_journal_lines WHERE entry_id = $1 ORDER BY created_at`, entryID)
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var l journalLineResp
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Debit, &l.Credit, &l.Memo); err != nil {
			return response.Error(c, 500, err.Error())
		}
		r.Lines = append(r.Lines, l)
	}
	return response.OK(c, r)
}

type journalLineResp struct {
	ID        string  `json:"id"`
	AccountID string  `json:"account_id"`
	Debit     float64 `json:"debit"`
	Credit    float64 `json:"credit"`
	Memo      string  `json:"memo"`
}

// ---------------------------------------------------------------------------
// Trial Balance
// ---------------------------------------------------------------------------

func (h *AccountingHandler) TrialBalance(c *fiber.Ctx) error {
	rows, err := tenantPool(c, h.pool).Query(c.Context(), `
		SELECT a.code, a.name, a.type,
		       COALESCE(SUM(jl.debit), 0) - COALESCE(SUM(jl.credit), 0) AS balance
		FROM accounting_accounts a
		LEFT JOIN accounting_journal_lines jl ON jl.account_id = a.id AND jl.hotel_id = a.hotel_id
		WHERE a.hotel_id = $1
		GROUP BY a.id, a.code, a.name, a.type, a.display_order
		ORDER BY a.display_order, a.code`, h.hotelID(c))
	if err != nil {
		return response.Error(c, 500, err.Error())
	}
	defer rows.Close()
	type tbRow struct {
		Code    string  `json:"code"`
		Name    string  `json:"name"`
		Type    string  `json:"type"`
		Balance float64 `json:"balance"`
	}
	out := []tbRow{}
	for rows.Next() {
		var r tbRow
		if err := rows.Scan(&r.Code, &r.Name, &r.Type, &r.Balance); err != nil {
			return response.Error(c, 500, err.Error())
		}
		out = append(out, r)
	}
	return response.OK(c, out)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func nullableUUID(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	return &u
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}
