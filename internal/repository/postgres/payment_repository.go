package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hotelharmony/api/internal/domain"
)

// PaymentRepository manages all payment records.
type PaymentRepository interface {
	Create(ctx context.Context, p *domain.Payment) (*domain.Payment, error)
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.PaymentStatus, method string) error
	UpdateAmountAndNotes(ctx context.Context, id uuid.UUID, amount float64, method, notes string) error
	Delete(ctx context.Context, id uuid.UUID) error
	ListSettings(ctx context.Context) ([]domain.PaymentSetting, error)
	FindGatewayConfig(ctx context.Context) (*domain.PaymentGatewayConfig, error)
}

type paymentRepository struct {
	db *DB
}

func NewPaymentRepository(db *DB) PaymentRepository {
	return &paymentRepository{db: db}
}

func (r *paymentRepository) Create(ctx context.Context, p *domain.Payment) (*domain.Payment, error) {
	const q = `
		INSERT INTO payments (id, hotel_id, payment_number, guest_stay_id, order_id, amount, payment_method, status, processed_by, notes, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, hotel_id, payment_number, guest_stay_id, order_id, amount, payment_method, status, processed_by, notes, created_at`
	p.ID = uuid.New()
	p.HotelID = DemoHotelID
	row := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q,
		p.ID, p.HotelID, p.PaymentNumber, p.GuestStayID, p.OrderID,
		p.Amount, p.PaymentMethod, p.Status, p.ProcessedBy, p.Notes,
		time.Now().UTC(),
	)
	out := &domain.Payment{}
	err := row.Scan(
		&out.ID, &out.HotelID, &out.PaymentNumber, &out.GuestStayID, &out.OrderID,
		&out.Amount, &out.PaymentMethod, &out.Status, &out.ProcessedBy,
		&out.Notes, &out.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("paymentRepo.Create: %w", err)
	}
	return out, nil
}

func (r *paymentRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	const q = `
		SELECT p.id, p.hotel_id, p.payment_number, p.guest_stay_id, p.order_id, p.amount,
		       p.payment_method, p.status, p.processed_by, p.notes, p.created_at,
		       gs.guest_name, gs.guest_id, gs.room_id,
		       rm.room_number
		FROM payments p
		LEFT JOIN guest_stays gs ON gs.id = p.guest_stay_id
		LEFT JOIN rooms rm ON rm.id = gs.room_id
		WHERE p.hotel_id = $1 AND p.id = $2`
	row := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q, DemoHotelID, id)
	p := &domain.Payment{}
	var guestName, roomNumber *string
	var guestID, guestStayID, orderID, processedBy, roomID *uuid.UUID
	err := row.Scan(
		&p.ID, &p.HotelID, &p.PaymentNumber, &guestStayID, &orderID, &p.Amount,
		&p.PaymentMethod, &p.Status, &processedBy, &p.Notes, &p.CreatedAt,
		&guestName, &guestID, &roomID, &roomNumber,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("paymentRepo.FindByID: %w", err)
	}
	p.GuestStayID = guestStayID
	p.OrderID = orderID
	p.ProcessedBy = processedBy
	if guestName != nil {
		p.GuestStay = &domain.PaymentGuestStaySummary{
			GuestName: *guestName,
			GuestID:   guestID,
			RoomID:    roomID,
		}
		if roomNumber != nil {
			p.GuestStay.Room = &domain.RoomSummary{RoomNumber: *roomNumber}
		}
	}
	return p, nil
}

func (r *paymentRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.PaymentStatus, method string) error {
	const q = `UPDATE payments SET status = $1, payment_method = $2 WHERE hotel_id = $3 AND id = $4`
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, status, method, DemoHotelID, id)
	return err
}

func (r *paymentRepository) UpdateAmountAndNotes(ctx context.Context, id uuid.UUID, amount float64, method, notes string) error {
	const q = `UPDATE payments SET amount = $1, payment_method = $2, status = 'pending', notes = $3 WHERE hotel_id = $4 AND id = $5`
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, amount, method, notes, DemoHotelID, id)
	return err
}

func (r *paymentRepository) Delete(ctx context.Context, id uuid.UUID) error {
	const q = `DELETE FROM payments WHERE hotel_id = $1 AND id = $2`
	_, err := poolFromContext(ctx, r.db.Pool).Exec(ctx, q, DemoHotelID, id)
	return err
}

func (r *paymentRepository) ListSettings(ctx context.Context) ([]domain.PaymentSetting, error) {
	const q = `SELECT id, hotel_id, gateway_name, webhook_url, is_active, created_by, created_at, updated_at
	           FROM payment_settings WHERE hotel_id = $1 ORDER BY gateway_name`
	rows, err := poolFromContext(ctx, r.db.Pool).Query(ctx, q, DemoHotelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var settings []domain.PaymentSetting
	for rows.Next() {
		var s domain.PaymentSetting
		if err := rows.Scan(&s.ID, &s.HotelID, &s.GatewayName, &s.WebhookURL, &s.IsActive, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		settings = append(settings, s)
	}
	return settings, rows.Err()
}

func (r *paymentRepository) FindGatewayConfig(ctx context.Context) (*domain.PaymentGatewayConfig, error) {
	const q = `
		SELECT hotel_id, active_gateway, COALESCE(default_currency, 'USD'), COALESCE(gateway_mode, 'test'),
		       stripe_enabled, stripe_account_id, stripe_publishable_key,
		       stripe_secret_key_encrypted, stripe_webhook_secret_encrypted,
		       razorpay_enabled, razorpay_key_id, razorpay_key_secret_encrypted,
		       cash_enabled, card_enabled, bank_transfer_enabled
		FROM payment_configs
		WHERE hotel_id = $1`
	cfg := &domain.PaymentGatewayConfig{}
	err := poolFromContext(ctx, r.db.Pool).QueryRow(ctx, q, DemoHotelID).Scan(
		&cfg.HotelID, &cfg.ActiveGateway, &cfg.DefaultCurrency, &cfg.GatewayMode,
		&cfg.StripeEnabled, &cfg.StripeAccountID, &cfg.StripePublishableKey,
		&cfg.StripeSecretKeyEncrypted, &cfg.StripeWebhookSecretEncrypted,
		&cfg.RazorpayEnabled, &cfg.RazorpayKeyID, &cfg.RazorpayKeySecretEncrypted,
		&cfg.CashEnabled, &cfg.CardEnabled, &cfg.BankTransferEnabled,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("paymentRepo.FindGatewayConfig: %w", err)
	}
	return cfg, nil
}
