package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hotelharmony/api/internal/domain"
)

type HotelRepository interface {
	CreateHotel(ctx context.Context, hotel *domain.Hotel) (*domain.Hotel, error)
	CreateProperty(ctx context.Context, property *domain.Property) (*domain.Property, error)
	UpsertBranding(ctx context.Context, branding *domain.HotelBranding) (*domain.HotelBranding, error)
	FindBrandingBySlug(ctx context.Context, slug string) (*domain.HotelBranding, error)
	FindBrandingByHotelID(ctx context.Context, hotelID uuid.UUID) (*domain.HotelBranding, error)
}

type hotelRepository struct {
	db *DB
}

func NewHotelRepository(db *DB) HotelRepository {
	return &hotelRepository{db: db}
}

func (r *hotelRepository) CreateHotel(ctx context.Context, hotel *domain.Hotel) (*domain.Hotel, error) {
	if hotel.ID == uuid.Nil {
		hotel.ID = uuid.New()
	}
	if hotel.PlanTier == "" {
		hotel.PlanTier = "basic"
	}
	if hotel.Settings == nil {
		hotel.Settings = map[string]interface{}{"max_rooms": 50, "max_users": 10, "max_properties": 1, "ai_addon": false}
	}
	settings, _ := json.Marshal(hotel.Settings)
	slug := strings.ToLower(strings.TrimSpace(hotel.Slug))
	now := time.Now().UTC()

	const q = `
		INSERT INTO hotels (
			id, name, slug, plan_tier, is_active, settings, logo_url, primary_color,
			address, country, timezone, currency, phone, email, website, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
		RETURNING id, name, slug, plan_tier, is_active, settings, logo_url, primary_color,
		          address, country, timezone, currency, phone, email, website,
		          stripe_account_id, stripe_enabled, razorpay_key_id, razorpay_enabled,
		          active_payment_gateway, created_at, updated_at`
	row := r.db.Pool.QueryRow(ctx, q,
		hotel.ID, hotel.Name, slug, hotel.PlanTier, true, settings, hotel.LogoURL, hotel.PrimaryColor,
		hotel.Address, hotel.Country, hotel.Timezone, hotel.Currency, hotel.Phone, hotel.Email, hotel.Website, now,
	)
	out, err := scanHotel(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("hotelRepo.CreateHotel: %w", err)
	}
	return out, nil
}

func (r *hotelRepository) CreateProperty(ctx context.Context, property *domain.Property) (*domain.Property, error) {
	if property.ID == uuid.Nil {
		property.ID = uuid.New()
	}
	const q = `
		INSERT INTO properties (id, hotel_id, name, address, star_rating, total_rooms, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,now(),now())
		RETURNING id, hotel_id, name, address, star_rating, total_rooms, created_at, updated_at`
	row := r.db.Pool.QueryRow(ctx, q, property.ID, property.HotelID, property.Name, property.Address, property.StarRating, property.TotalRooms)
	out := &domain.Property{}
	err := row.Scan(&out.ID, &out.HotelID, &out.Name, &out.Address, &out.StarRating, &out.TotalRooms, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("hotelRepo.CreateProperty: %w", err)
	}
	return out, nil
}

func (r *hotelRepository) UpsertBranding(ctx context.Context, branding *domain.HotelBranding) (*domain.HotelBranding, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if strings.TrimSpace(branding.HotelName) != "" {
		if _, err := tx.Exec(ctx, `UPDATE hotels SET name = $1, updated_at = now() WHERE id = $2`, strings.TrimSpace(branding.HotelName), branding.HotelID); err != nil {
			return nil, err
		}
	}

	const q = `
		INSERT INTO hotel_branding (hotel_id, logo_url, primary_color, client_primary_color, admin_primary_color, welcome_message, footer_text, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (hotel_id) DO UPDATE
		  SET logo_url = EXCLUDED.logo_url,
		      primary_color = EXCLUDED.primary_color,
		      client_primary_color = EXCLUDED.client_primary_color,
		      admin_primary_color = EXCLUDED.admin_primary_color,
		      welcome_message = EXCLUDED.welcome_message,
		      footer_text = EXCLUDED.footer_text,
		      updated_at = now()`
	clientColor := strings.TrimSpace(branding.ClientPrimaryColor)
	if clientColor == "" {
		clientColor = branding.PrimaryColor
	}
	adminColor := strings.TrimSpace(branding.AdminPrimaryColor)
	if adminColor == "" {
		adminColor = branding.PrimaryColor
	}
	if _, err := tx.Exec(ctx, q, branding.HotelID, branding.LogoURL, branding.PrimaryColor, clientColor, adminColor, branding.WelcomeMessage, branding.FooterText); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.FindBrandingByHotelID(ctx, branding.HotelID)
}

func (r *hotelRepository) FindBrandingBySlug(ctx context.Context, slug string) (*domain.HotelBranding, error) {
	const q = `
		SELECT h.id, COALESCE(hb.logo_url, h.logo_url), COALESCE(hb.primary_color, h.primary_color, '#000000'),
		       COALESCE(hb.client_primary_color, hb.primary_color, h.primary_color, '#000000'),
		       COALESCE(hb.admin_primary_color, hb.primary_color, h.primary_color, '#000000'),
		       hb.welcome_message, hb.footer_text, COALESCE(hb.updated_at, h.updated_at),
		       h.name, h.slug, h.country, h.currency
		FROM hotels h
		LEFT JOIN hotel_branding hb ON hb.hotel_id = h.id
		WHERE h.slug = $1 AND h.is_active = true`
	row := r.db.Pool.QueryRow(ctx, q, strings.ToLower(strings.TrimSpace(slug)))
	return scanEnrichedBranding(row)
}

func (r *hotelRepository) FindBrandingByHotelID(ctx context.Context, hotelID uuid.UUID) (*domain.HotelBranding, error) {
	const q = `
		SELECT h.id, COALESCE(hb.logo_url, h.logo_url), COALESCE(hb.primary_color, h.primary_color, '#000000'),
		       COALESCE(hb.client_primary_color, hb.primary_color, h.primary_color, '#000000'),
		       COALESCE(hb.admin_primary_color, hb.primary_color, h.primary_color, '#000000'),
		       hb.welcome_message, hb.footer_text, COALESCE(hb.updated_at, h.updated_at),
		       h.name, h.slug, h.country, h.currency
		FROM hotels h
		LEFT JOIN hotel_branding hb ON hb.hotel_id = h.id
		WHERE h.id = $1 AND h.is_active = true`
	row := r.db.Pool.QueryRow(ctx, q, hotelID)
	return scanEnrichedBranding(row)
}

func scanHotel(row pgx.Row) (*domain.Hotel, error) {
	h := &domain.Hotel{}
	var settings []byte
	err := row.Scan(
		&h.ID, &h.Name, &h.Slug, &h.PlanTier, &h.IsActive, &settings,
		&h.LogoURL, &h.PrimaryColor, &h.Address, &h.Country, &h.Timezone, &h.Currency,
		&h.Phone, &h.Email, &h.Website, &h.StripeAccountID, &h.StripeEnabled,
		&h.RazorpayKeyID, &h.RazorpayEnabled, &h.ActivePaymentGateway, &h.CreatedAt, &h.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(settings, &h.Settings)
	return h, nil
}

func scanBranding(row pgx.Row) (*domain.HotelBranding, error) {
	b := &domain.HotelBranding{}
	err := row.Scan(&b.HotelID, &b.LogoURL, &b.PrimaryColor, &b.ClientPrimaryColor, &b.AdminPrimaryColor, &b.WelcomeMessage, &b.FooterText, &b.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return b, nil
}

func scanEnrichedBranding(row pgx.Row) (*domain.HotelBranding, error) {
	b := &domain.HotelBranding{}
	err := row.Scan(
		&b.HotelID, &b.LogoURL, &b.PrimaryColor, &b.ClientPrimaryColor, &b.AdminPrimaryColor, &b.WelcomeMessage, &b.FooterText, &b.UpdatedAt,
		&b.HotelName, &b.Slug, &b.Country, &b.Currency,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if b.PrimaryColor == "" {
		b.PrimaryColor = "#000000"
	}
	if b.ClientPrimaryColor == "" {
		b.ClientPrimaryColor = b.PrimaryColor
	}
	if b.AdminPrimaryColor == "" {
		b.AdminPrimaryColor = b.PrimaryColor
	}
	return b, nil
}
