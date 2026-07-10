package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// BillInvoice renders a GST "Tax Invoice" for a dine-in bill as print-ready HTML
// (print to PDF from the browser). Layout mirrors a standard Indian restaurant
// tax invoice: restaurant legal/GSTIN/FSSAI header, a per-line table with the
// CGST/SGST split, Item(s) Total, Total Value, amount-in-words, and the
// "settled against Order ID … reverse charge: No" footer.
//
// The bill stores a single tax_rate; per the agreed model it is split 50/50 into
// CGST and SGST (intrastate GST). Restaurant GST identifiers come from the
// hotel's settings JSONB (gstin/fssai/legal_name/state/place_of_supply), with
// safe fallbacks so the invoice always renders.
func (h *POSHandler) BillInvoice(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid bill id")
	}
	hotelID := h.hotelID(c)
	ctx := c.Context()

	b, err := scanBill(h.db(c).QueryRow(ctx, `SELECT `+billCols+` FROM bills WHERE id = $1 AND hotel_id = $2`, id, hotelID))
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "bill not found")
	}

	// Session + table context.
	var sessionNumber, tableNumber string
	var guestName *string
	var billedAt *time.Time
	_ = h.db(c).QueryRow(ctx, `
		SELECT ds.session_number, ds.guest_name, ds.billed_at, COALESCE(rt.table_number, '')
		FROM dining_sessions ds LEFT JOIN restaurant_tables rt ON rt.id = ds.table_id
		WHERE ds.id = $1`, b.SessionID).Scan(&sessionNumber, &guestName, &billedAt, &tableNumber)

	// Hotel / restaurant identity. Prefer the dedicated GST columns (migration
	// 010); fall back to the settings JSONB, then to the hotel name/address.
	var hotelName string
	var hotelAddr *string
	var settingsRaw []byte
	var colLegal, colRestName, colRestAddr, colGSTIN, colFSSAI, colState, colPlace, colHSN *string
	_ = h.db(c).QueryRow(ctx, `
		SELECT name, address, settings, legal_entity_name, restaurant_name, restaurant_address,
		       gstin, fssai, gst_state, place_of_supply, hsn_code
		FROM hotels WHERE id = $1`, hotelID).
		Scan(&hotelName, &hotelAddr, &settingsRaw, &colLegal, &colRestName, &colRestAddr,
			&colGSTIN, &colFSSAI, &colState, &colPlace, &colHSN)
	var settings map[string]interface{}
	_ = json.Unmarshal(settingsRaw, &settings)
	settingsStr := func(key string) string {
		if v, ok := settings[key].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	addr := ""
	if hotelAddr != nil {
		addr = *hotelAddr
	}
	// Outlet identity (highest precedence) when the bill belongs to an outlet.
	var oLegal, oName, oAddr, oGSTIN, oFSSAI, oPlace, oHSN *string
	_ = h.db(c).QueryRow(ctx, `
		SELECT o.legal_entity_name, o.name, o.address, o.gstin, o.fssai, o.place_of_supply, o.hsn_code
		FROM bills bl JOIN outlets o ON o.id = bl.outlet_id WHERE bl.id = $1`, id).
		Scan(&oLegal, &oName, &oAddr, &oGSTIN, &oFSSAI, &oPlace, &oHSN)
	coalesce := func(a, b *string) *string {
		if a != nil && strings.TrimSpace(*a) != "" {
			return a
		}
		return b
	}
	// pick resolves a field as: outlet/dedicated column > settings key > default.
	pick := func(col *string, settingsKey, def string) string {
		if col != nil && strings.TrimSpace(*col) != "" {
			return strings.TrimSpace(*col)
		}
		if v := settingsStr(settingsKey); v != "" {
			return v
		}
		return def
	}

	// Line items (non-cancelled), with proportional discount allocation and the
	// CGST/SGST split derived from the bill's tax_rate.
	rows, err := h.db(c).Query(ctx, `
		SELECT ki.item_name, ki.quantity, ki.unit_price, ki.line_total
		FROM kot_items ki JOIN kots k ON k.id = ki.kot_id
		WHERE k.dining_session_id = $1 AND ki.status <> 'cancelled'
		ORDER BY k.round_no, ki.created_at`, b.SessionID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to load items")
	}
	type rawLine struct {
		name  string
		qty   int
		unit  float64
		gross float64
	}
	var raws []rawLine
	var grossSum float64
	for rows.Next() {
		var rl rawLine
		if err := rows.Scan(&rl.name, &rl.qty, &rl.unit, &rl.gross); err != nil {
			rows.Close()
			return response.Error(c, fiber.StatusInternalServerError, "failed to scan item")
		}
		grossSum += rl.gross
		raws = append(raws, rl)
	}
	rows.Close()

	halfRate := round2(b.TaxRate / 2) // CGST rate == SGST rate

	lines := make([]invoiceLine, 0, len(raws))
	var sumGross, sumDisc, sumNet, sumCGST, sumSGST, sumTotal float64
	for _, rl := range raws {
		// Allocate the bill-level discount across lines proportionally to gross.
		disc := 0.0
		if grossSum > 0 && b.DiscountAmount > 0 {
			disc = round2(b.DiscountAmount * (rl.gross / grossSum))
		}
		net := round2(rl.gross - disc)
		cgst := round2(net * halfRate / 100)
		sgst := cgst
		total := round2(net + cgst + sgst)
		lines = append(lines, invoiceLine{
			Particulars: fmt.Sprintf("%d x %s", rl.qty, rl.name),
			Gross:       rl.gross, Discount: disc, Net: net,
			CGSTRate: halfRate, CGSTAmt: cgst, SGSTRate: halfRate, SGSTAmt: sgst, Total: total,
		})
		sumGross += rl.gross
		sumDisc += disc
		sumNet += net
		sumCGST += cgst
		sumSGST += sgst
		sumTotal += total
	}

	// Total Value row uses the bill's authoritative tax/total so the printed
	// grand total matches exactly what was charged.
	totalCGST := round2(b.TaxAmount / 2)
	totalSGST := round2(b.TaxAmount - totalCGST)

	invDate := time.Now()
	if billedAt != nil {
		invDate = *billedAt
	}
	settledDate := invDate.Format("2006-01-02")

	data := invoiceData{
		LegalName:         pick(coalesce(oLegal, colLegal), "legal_name", hotelName),
		RestaurantName:    pick(coalesce(oName, colRestName), "restaurant_name", hotelName),
		RestaurantAddress: pick(coalesce(oAddr, colRestAddr), "restaurant_address", addr),
		GSTIN:             pick(coalesce(oGSTIN, colGSTIN), "gstin", "—"),
		FSSAI:             pick(coalesce(oFSSAI, colFSSAI), "fssai", "—"),
		InvoiceNo:         b.BillNumber,
		InvoiceDate:       invDate.Format("02/01/2006"),
		CustomerName:      derefOr(guestName, "Walk-in Guest"),
		TableLabel:        tableLabel(tableNumber),
		PlaceOfSupply:     pick(coalesce(oPlace, colPlace), "place_of_supply", pick(colState, "state", "—")),
		HSN:               pick(coalesce(oHSN, colHSN), "hsn_code", "996331"),
		ServiceDesc:       "Restaurant Service",
		Lines:             lines,
		ItemsGross:        round2(sumGross),
		ItemsDiscount:     round2(sumDisc),
		ItemsNet:          round2(sumNet),
		ItemsCGST:         round2(sumCGST),
		ItemsSGST:         round2(sumSGST),
		ItemsTotal:        round2(sumTotal),
		HasTip:            b.TipAmount > 0,
		TipAmount:         b.TipAmount,
		TotalNet:          round2(b.Subtotal - b.DiscountAmount),
		TotalCGST:         totalCGST,
		TotalSGST:         totalSGST,
		GrandTotal:        b.TotalAmount,
		Currency:          b.Currency,
		AmountWords:       amountInWords(b.TotalAmount, b.Currency),
		OrderRef:          firstNonEmpty(sessionNumber, b.BillNumber),
		SettledDate:       settledDate,
		PaymentMode:       h.invoicePaymentMode(ctx, c, id),
	}

	var buf bytes.Buffer
	if err := invoiceTmpl.Execute(&buf, data); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to render invoice")
	}
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.Send(buf.Bytes())
}

func (h *POSHandler) invoicePaymentMode(ctx context.Context, c *fiber.Ctx, billID uuid.UUID) string {
	rows, err := h.db(c).Query(ctx, `SELECT DISTINCT method FROM bill_payments WHERE bill_id = $1 AND status = 'completed'`, billID)
	if err != nil {
		return "digital mode"
	}
	defer rows.Close()
	var methods []string
	for rows.Next() {
		var m string
		if rows.Scan(&m) == nil {
			methods = append(methods, strings.ToUpper(m))
		}
	}
	if len(methods) == 0 {
		return "digital mode"
	}
	return strings.Join(methods, " + ")
}

type invoiceLine struct {
	Particulars                          string
	Gross, Discount, Net                 float64
	CGSTRate, CGSTAmt, SGSTRate, SGSTAmt float64
	Total                                float64
}

type invoiceData struct {
	LegalName, RestaurantName, RestaurantAddress, GSTIN, FSSAI            string
	InvoiceNo, InvoiceDate                                                string
	CustomerName, TableLabel, PlaceOfSupply, HSN, ServiceDesc             string
	Lines                                                                 []invoiceLine
	ItemsGross, ItemsDiscount, ItemsNet, ItemsCGST, ItemsSGST, ItemsTotal float64
	HasTip                                                                bool
	TipAmount                                                             float64
	TotalNet, TotalCGST, TotalSGST, GrandTotal                            float64
	Currency, AmountWords, OrderRef, SettledDate, PaymentMode             string
}

func derefOr(s *string, def string) string {
	if s != nil && strings.TrimSpace(*s) != "" {
		return *s
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func tableLabel(num string) string {
	if strings.TrimSpace(num) == "" {
		return "Dine-In"
	}
	return "Table " + num
}

// amountInWords renders an INR amount as "<Rupees> Rupees And <Paisa> Paisa Only"
// using the Indian numbering system (crore/lakh/thousand).
func amountInWords(amount float64, currency string) string {
	unit, sub := "Rupees", "Paisa"
	if strings.EqualFold(currency, "USD") {
		unit, sub = "Dollars", "Cents"
	}
	rupees := int64(math.Floor(amount + 1e-9))
	paisa := int64(math.Round((amount - float64(rupees)) * 100))
	if paisa == 100 {
		rupees++
		paisa = 0
	}
	s := intToIndianWords(rupees) + " " + unit
	if paisa > 0 {
		s += " And " + intToIndianWords(paisa) + " " + sub
	}
	return s + " Only"
}

var invOnes = []string{"", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine", "Ten",
	"Eleven", "Twelve", "Thirteen", "Fourteen", "Fifteen", "Sixteen", "Seventeen", "Eighteen", "Nineteen"}
var invTens = []string{"", "", "Twenty", "Thirty", "Forty", "Fifty", "Sixty", "Seventy", "Eighty", "Ninety"}

func invTwo(n int64) string {
	if n < 20 {
		return invOnes[n]
	}
	return strings.TrimSpace(invTens[n/10] + " " + invOnes[n%10])
}

func intToIndianWords(n int64) string {
	if n == 0 {
		return "Zero"
	}
	res := ""
	if crore := n / 10000000; crore > 0 {
		res += intToIndianWords(crore) + " Crore "
		n %= 10000000
	}
	if lakh := n / 100000; lakh > 0 {
		res += invTwo(lakh) + " Lakh "
		n %= 100000
	}
	if thousand := n / 1000; thousand > 0 {
		res += invTwo(thousand) + " Thousand "
		n %= 1000
	}
	if hundred := n / 100; hundred > 0 {
		res += invOnes[hundred] + " Hundred "
		n %= 100
	}
	if n > 0 {
		res += invTwo(n)
	}
	return strings.TrimSpace(res)
}

var invoiceTmpl = template.Must(template.New("invoice").Funcs(template.FuncMap{
	"money": func(f float64) string { return fmt.Sprintf("%.2f", f) },
	"pct":   func(f float64) string { return fmt.Sprintf("%.2f%%", f) },
}).Parse(invoiceHTML))

const invoiceHTML = `<!doctype html>
<html><head><meta charset="utf-8">
<title>Tax Invoice {{.InvoiceNo}}</title>
<style>
  * { box-sizing: border-box; }
  body { font-family: Arial, Helvetica, sans-serif; color:#000; font-size:12px; margin:24px; }
  .top { display:flex; justify-content:space-between; align-items:flex-start; }
  .brand { font-size:26px; font-weight:700; letter-spacing:-0.5px; }
  .hdr-right { text-align:right; font-weight:700; }
  .muted { color:#000; }
  .sec { margin-top:14px; }
  .kv b { display:inline-block; }
  table { border-collapse:collapse; width:100%; margin-top:10px; }
  th, td { border:1px solid #000; padding:5px 6px; text-align:right; }
  th { background:#f2f2f2; text-align:center; font-weight:700; }
  td.l, th.l { text-align:left; }
  .tot td { font-weight:700; }
  .foot { margin-top:14px; }
  .sign { margin-top:48px; text-align:right; }
  @media print { body { margin:0; } }
</style></head>
<body>
  <div class="top">
    <div class="brand">{{.RestaurantName}}</div>
    <div class="hdr-right">Tax Invoice<br>ORIGINAL FOR RECIPIENT</div>
  </div>

  <div class="sec kv">
    <div><b>Legal Entity Name:</b> {{.LegalName}}</div>
    <div><b>Restaurant Name:</b> {{.RestaurantName}}</div>
    <div><b>Restaurant Address:</b> {{.RestaurantAddress}}</div>
    <div><b>Restaurant GSTIN:</b> {{.GSTIN}}</div>
    <div><b>Restaurant FSSAI:</b> {{.FSSAI}}</div>
    <div><b>Invoice No.:</b> {{.InvoiceNo}}</div>
    <div><b>Invoice Date:</b> {{.InvoiceDate}}</div>
  </div>

  <div class="sec kv">
    <div><b>Customer Name:</b> {{.CustomerName}}</div>
    <div><b>Service Location:</b> {{.TableLabel}}</div>
    <div><b>State name and Place of Supply:</b> {{.PlaceOfSupply}}</div>
  </div>

  <div class="sec kv">
    <div><b>HSN Code:</b> {{.HSN}}</div>
    <div><b>Service Description:</b> {{.ServiceDesc}}</div>
  </div>

  <table>
    <thead>
      <tr>
        <th class="l">Particulars</th>
        <th>Gross value</th><th>Discount</th><th>Net value</th>
        <th>CGST (Rate)</th><th>CGST (INR)</th>
        <th>SGST (Rate)</th><th>SGST (INR)</th>
        <th>Total</th>
      </tr>
    </thead>
    <tbody>
      {{range .Lines}}
      <tr>
        <td class="l">{{.Particulars}}</td>
        <td>{{money .Gross}}</td><td>{{money .Discount}}</td><td>{{money .Net}}</td>
        <td>{{pct .CGSTRate}}</td><td>{{money .CGSTAmt}}</td>
        <td>{{pct .SGSTRate}}</td><td>{{money .SGSTAmt}}</td>
        <td>{{money .Total}}</td>
      </tr>
      {{end}}
      <tr class="tot">
        <td class="l">Item(s) Total</td>
        <td>{{money .ItemsGross}}</td><td>{{money .ItemsDiscount}}</td><td>{{money .ItemsNet}}</td>
        <td></td><td>{{money .ItemsCGST}}</td><td></td><td>{{money .ItemsSGST}}</td>
        <td>{{money .ItemsTotal}}</td>
      </tr>
      {{if .HasTip}}
      <tr>
        <td class="l">Tip / Gratuity</td>
        <td>{{money .TipAmount}}</td><td>0.00</td><td>{{money .TipAmount}}</td>
        <td></td><td>0.00</td><td></td><td>0.00</td>
        <td>{{money .TipAmount}}</td>
      </tr>
      {{end}}
      <tr class="tot">
        <td class="l">Total Value</td>
        <td></td><td></td><td>{{money .TotalNet}}</td>
        <td></td><td>{{money .TotalCGST}}</td><td></td><td>{{money .TotalSGST}}</td>
        <td>{{money .GrandTotal}}</td>
      </tr>
    </tbody>
  </table>

  <div class="foot">
    <div><b>Amount (in words):</b> {{.AmountWords}}</div>
    <div style="margin-top:6px;">Amount of {{.Currency}} {{money .GrandTotal}} settled via {{.PaymentMode}} against Order ID {{.OrderRef}} dated {{.SettledDate}}.</div>
    <div style="margin-top:6px;">Supply attracts reverse charge : No</div>
  </div>

  <div class="sign">
    For {{.LegalName}}<br><br>
    Authorised Signatory
  </div>
</body></html>`
