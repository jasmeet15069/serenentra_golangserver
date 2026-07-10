package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hotelharmony/api/internal/domain"
)

// ErrNotFound is returned when a query produces zero rows.
var ErrNotFound = errors.New("record not found")

// ErrConflict is returned on unique constraint violations.
var ErrConflict = errors.New("record already exists")

// UserRepository allows mocking in tests.
type UserRepository interface {
	Create(ctx context.Context, email, passwordHash string) (*domain.User, error)
	FindByEmail(ctx context.Context, email string) (*domain.User, error)
	FindByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
	IsHotelActive(ctx context.Context, hotelID uuid.UUID) (bool, error)
	UpdatePassword(ctx context.Context, id uuid.UUID, passwordHash string) error
	CreateProfile(ctx context.Context, userID uuid.UUID, fullName string, phone *string) (*domain.Profile, error)
	FindProfileByUserID(ctx context.Context, userID uuid.UUID) (*domain.Profile, error)
	UpdateProfile(ctx context.Context, userID uuid.UUID, fields map[string]interface{}) error
	AddRole(ctx context.Context, userID uuid.UUID, role domain.UserRole) error
	RemoveRole(ctx context.Context, userID uuid.UUID, role domain.UserRole) error
	GetRoles(ctx context.Context, userID uuid.UUID) ([]domain.UserRole, error)
	List(ctx context.Context, hotelID *uuid.UUID) ([]domain.User, error)
	CreateForHotel(ctx context.Context, hotelID uuid.UUID, email, passwordHash string) (*domain.User, error)
	RemoveAllRoles(ctx context.Context, userID uuid.UUID) error
	Delete(ctx context.Context, userID uuid.UUID) error
	SetUserActive(ctx context.Context, userID uuid.UUID, active bool) error
	CreatePreferences(ctx context.Context, prefs *domain.GuestPreferences) error
	UpsertPreferences(ctx context.Context, prefs *domain.GuestPreferences) error
	FindPreferencesByUserID(ctx context.Context, userID uuid.UUID) (*domain.GuestPreferences, error)
}

type userRepository struct {
	db *DB
}

// NewUserRepository constructs a production UserRepository backed by pgx.
func NewUserRepository(db *DB) UserRepository {
	return &userRepository{db: db}
}

func (r *userRepository) Create(ctx context.Context, email, passwordHash string) (*domain.User, error) {
	const q = `
		INSERT INTO users (id, hotel_id, email, password_hash, platform_admin, created_at, updated_at)
		VALUES ($1, $2, $3, $4, false, $5, $5)
		RETURNING id, hotel_id, email, password_hash, platform_admin, created_at, updated_at`

	id := uuid.New()
	now := time.Now().UTC()

	u := &domain.User{}
	err := r.db.Pool.QueryRow(ctx, q, id, DemoHotelID, email, passwordHash, now).
		Scan(&u.ID, &u.HotelID, &u.Email, &u.PasswordHash, &u.PlatformAdmin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("userRepo.Create: %w", err)
	}
	return u, nil
}

func (r *userRepository) FindByEmail(ctx context.Context, email string) (*domain.User, error) {
	const q = `SELECT id, hotel_id, email, password_hash, platform_admin, created_at, updated_at FROM users WHERE email = $1`
	u := &domain.User{}
	err := r.db.Pool.QueryRow(ctx, q, email).
		Scan(&u.ID, &u.HotelID, &u.Email, &u.PasswordHash, &u.PlatformAdmin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("userRepo.FindByEmail: %w", err)
	}
	return u, nil
}

func (r *userRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	const q = `SELECT id, hotel_id, email, password_hash, platform_admin, created_at, updated_at FROM users WHERE id = $1`
	u := &domain.User{}
	err := r.db.Pool.QueryRow(ctx, q, id).
		Scan(&u.ID, &u.HotelID, &u.Email, &u.PasswordHash, &u.PlatformAdmin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("userRepo.FindByID: %w", err)
	}
	return u, nil
}

func (r *userRepository) IsHotelActive(ctx context.Context, hotelID uuid.UUID) (bool, error) {
	var active bool
	err := r.db.Pool.QueryRow(ctx, `SELECT is_active FROM hotels WHERE id = $1`, hotelID).Scan(&active)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("userRepo.IsHotelActive: %w", err)
	}
	return active, nil
}

func (r *userRepository) UpdatePassword(ctx context.Context, id uuid.UUID, passwordHash string) error {
	const q = `UPDATE users SET password_hash = $1, updated_at = $2 WHERE id = $3`
	_, err := r.db.Pool.Exec(ctx, q, passwordHash, time.Now().UTC(), id)
	return err
}

func (r *userRepository) CreateProfile(ctx context.Context, userID uuid.UUID, fullName string, phone *string) (*domain.Profile, error) {
	const q = `
		INSERT INTO profiles (id, hotel_id, user_id, full_name, phone, created_at, updated_at)
		VALUES ($1, (SELECT hotel_id FROM users WHERE id = $2), $2, $3, $4, $5, $5)
		RETURNING id, hotel_id, user_id, full_name, phone, avatar_url, created_at, updated_at`
	id := uuid.New()
	now := time.Now().UTC()
	p := &domain.Profile{}
	err := r.db.Pool.QueryRow(ctx, q, id, userID, fullName, phone, now).
		Scan(&p.ID, &p.HotelID, &p.UserID, &p.FullName, &p.Phone, &p.AvatarURL, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("userRepo.CreateProfile: %w", err)
	}
	return p, nil
}

func (r *userRepository) FindProfileByUserID(ctx context.Context, userID uuid.UUID) (*domain.Profile, error) {
	const q = `SELECT id, hotel_id, user_id, full_name, phone, avatar_url, created_at, updated_at FROM profiles WHERE user_id = $1`
	p := &domain.Profile{}
	err := r.db.Pool.QueryRow(ctx, q, userID).
		Scan(&p.ID, &p.HotelID, &p.UserID, &p.FullName, &p.Phone, &p.AvatarURL, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("userRepo.FindProfileByUserID: %w", err)
	}
	return p, nil
}

func (r *userRepository) UpdateProfile(ctx context.Context, userID uuid.UUID, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	fields["updated_at"] = time.Now().UTC()
	i := 1
	setClauses := ""
	args := make([]interface{}, 0, len(fields)+1)
	for k, v := range fields {
		if i > 1 {
			setClauses += ", "
		}
		setClauses += fmt.Sprintf("%s = $%d", k, i)
		args = append(args, v)
		i++
	}
	args = append(args, userID)
	q := fmt.Sprintf("UPDATE profiles SET %s WHERE user_id = $%d", setClauses, i)
	_, err := r.db.Pool.Exec(ctx, q, args...)
	return err
}

func (r *userRepository) AddRole(ctx context.Context, userID uuid.UUID, role domain.UserRole) error {
	const q = `
		INSERT INTO user_roles (id, hotel_id, user_id, role, created_at)
		VALUES ($1, (SELECT hotel_id FROM users WHERE id = $2), $2, $3, $4)
		ON CONFLICT (user_id, role) DO NOTHING`
	_, err := r.db.Pool.Exec(ctx, q, uuid.New(), userID, role, time.Now().UTC())
	return err
}

func (r *userRepository) GetRoles(ctx context.Context, userID uuid.UUID) ([]domain.UserRole, error) {
	const q = `SELECT role FROM user_roles WHERE user_id = $1 ORDER BY created_at`
	rows, err := r.db.Pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("userRepo.GetRoles: %w", err)
	}
	defer rows.Close()

	var roles []domain.UserRole
	for rows.Next() {
		var role domain.UserRole
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

func (r *userRepository) RemoveRole(ctx context.Context, userID uuid.UUID, role domain.UserRole) error {
	const q = `DELETE FROM user_roles WHERE user_id = $1 AND role = $2`
	_, err := r.db.Pool.Exec(ctx, q, userID, role)
	return err
}

func (r *userRepository) List(ctx context.Context, hotelID *uuid.UUID) ([]domain.User, error) {
	q := `SELECT id, hotel_id, email, password_hash, platform_admin, created_at, updated_at FROM users`
	var args []interface{}
	if hotelID != nil {
		q += ` WHERE hotel_id = $1`
		args = append(args, *hotelID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := r.db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("userRepo.List: %w", err)
	}
	defer rows.Close()

	var users []domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.HotelID, &u.Email, &u.PasswordHash, &u.PlatformAdmin, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (r *userRepository) CreateForHotel(ctx context.Context, hotelID uuid.UUID, email, passwordHash string) (*domain.User, error) {
	const q = `
		INSERT INTO users (id, hotel_id, email, password_hash, platform_admin, created_at, updated_at)
		VALUES ($1, $2, $3, $4, false, $5, $5)
		RETURNING id, hotel_id, email, password_hash, platform_admin, created_at, updated_at`
	id := uuid.New()
	now := time.Now().UTC()
	u := &domain.User{}
	err := r.db.Pool.QueryRow(ctx, q, id, hotelID, email, passwordHash, now).
		Scan(&u.ID, &u.HotelID, &u.Email, &u.PasswordHash, &u.PlatformAdmin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("userRepo.CreateForHotel: %w", err)
	}
	return u, nil
}

func (r *userRepository) RemoveAllRoles(ctx context.Context, userID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`DELETE FROM user_roles WHERE user_id = $1 AND role != 'guest'`, userID)
	return err
}

func (r *userRepository) Delete(ctx context.Context, userID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	return err
}

func (r *userRepository) SetUserActive(ctx context.Context, userID uuid.UUID, active bool) error {
	const q = `UPDATE users SET updated_at = $1 WHERE id = $2`
	_, err := r.db.Pool.Exec(ctx, q, time.Now().UTC(), userID)
	return err
}

func (r *userRepository) CreatePreferences(ctx context.Context, prefs *domain.GuestPreferences) error {
	const q = `
		INSERT INTO guest_preferences (id, hotel_id, user_id, dietary_restrictions, allergies, favorite_categories, notes, created_at, updated_at)
		VALUES ($1, (SELECT hotel_id FROM users WHERE id = $2), $2, $3, $4, $5, $6, $7, $7)`
	_, err := r.db.Pool.Exec(ctx, q,
		uuid.New(), prefs.UserID,
		prefs.DietaryRestrictions, prefs.Allergies, prefs.FavoriteCategories,
		prefs.Notes, time.Now().UTC(),
	)
	return err
}

func (r *userRepository) UpsertPreferences(ctx context.Context, prefs *domain.GuestPreferences) error {
	const q = `
		INSERT INTO guest_preferences (id, hotel_id, user_id, dietary_restrictions, allergies, favorite_categories, notes, created_at, updated_at)
		VALUES ($1, (SELECT hotel_id FROM users WHERE id = $2), $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (user_id) DO UPDATE
		  SET dietary_restrictions = EXCLUDED.dietary_restrictions,
		      allergies = EXCLUDED.allergies,
		      favorite_categories = EXCLUDED.favorite_categories,
		      notes = EXCLUDED.notes,
		      updated_at = EXCLUDED.updated_at`
	_, err := r.db.Pool.Exec(ctx, q,
		uuid.New(), prefs.UserID,
		prefs.DietaryRestrictions, prefs.Allergies, prefs.FavoriteCategories,
		prefs.Notes, time.Now().UTC(),
	)
	return err
}

func (r *userRepository) FindPreferencesByUserID(ctx context.Context, userID uuid.UUID) (*domain.GuestPreferences, error) {
	const q = `
		SELECT id, user_id, dietary_restrictions, allergies, favorite_categories, notes, created_at, updated_at
		FROM guest_preferences WHERE user_id = $1`
	p := &domain.GuestPreferences{}
	var diet, allerg, favCats pgtype.Array[string]
	err := r.db.Pool.QueryRow(ctx, q, userID).Scan(
		&p.ID, &p.UserID, &diet, &allerg, &favCats, &p.Notes, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.DietaryRestrictions = diet.Elements
	p.Allergies = allerg.Elements
	p.FavoriteCategories = favCats.Elements
	return p, nil
}

// isUniqueViolation returns true for PostgreSQL error code 23505.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23505")
}
