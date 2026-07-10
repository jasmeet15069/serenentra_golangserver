// Package domain contains all domain entities, value objects, and business
// logic types for Hotel Harmony. Every entity maps 1-to-1 with a PostgreSQL
// table; JSON fields are represented as typed slices/maps rather than raw
// strings so the application layer never has to deserialise them manually.
package domain

import (
	"time"

	"github.com/google/uuid"
)

type RoomStatus string

const (
	RoomStatusAvailable   RoomStatus = "available"
	RoomStatusOccupied    RoomStatus = "occupied"
	RoomStatusCleaning    RoomStatus = "cleaning"
	RoomStatusMaintenance RoomStatus = "maintenance"
)

type OrderStatus string

const (
	OrderStatusPending   OrderStatus = "pending"
	OrderStatusPreparing OrderStatus = "preparing"
	OrderStatusReady     OrderStatus = "ready"
	OrderStatusDelivered OrderStatus = "delivered"
	OrderStatusCancelled OrderStatus = "cancelled"
)

type ComplaintStatus string

const (
	ComplaintStatusOpen       ComplaintStatus = "open"
	ComplaintStatusInProgress ComplaintStatus = "in_progress"
	ComplaintStatusResolved   ComplaintStatus = "resolved"
)

type ComplaintPriority string

const (
	ComplaintPriorityLow      ComplaintPriority = "low"
	ComplaintPriorityMedium   ComplaintPriority = "medium"
	ComplaintPriorityHigh     ComplaintPriority = "high"
	ComplaintPriorityCritical ComplaintPriority = "critical"
)

type PaymentStatus string

const (
	PaymentStatusPending   PaymentStatus = "pending"
	PaymentStatusCompleted PaymentStatus = "completed"
	PaymentStatusFailed    PaymentStatus = "failed"
	PaymentStatusRefunded  PaymentStatus = "refunded"
)

type UserRole string

const (
	RolePlatformAdmin   UserRole = "platform_admin"
	RoleHotelAdmin      UserRole = "hotel_admin"
	RolePropertyManager UserRole = "property_manager"
	RoleReceptionist    UserRole = "receptionist"
	RoleHousekeeping    UserRole = "housekeeping"
	RoleMaintenance     UserRole = "maintenance"
	RoleAdmin           UserRole = "admin"
	RoleSuperAdmin      UserRole = "super_admin"
	RoleFoodManager     UserRole = "food_manager"
	RoleKitchenManager  UserRole = "kitchen_manager"
	RoleWaiter          UserRole = "waiter"
	RoleGuest           UserRole = "guest"
)

type Hotel struct {
	ID                   uuid.UUID              `db:"id" json:"id"`
	Name                 string                 `db:"name" json:"name"`
	Slug                 string                 `db:"slug" json:"slug"`
	PlanTier             string                 `db:"plan_tier" json:"plan_tier"`
	IsActive             bool                   `db:"is_active" json:"is_active"`
	Settings             map[string]interface{} `db:"settings" json:"settings"`
	LogoURL              *string                `db:"logo_url" json:"logo_url,omitempty"`
	PrimaryColor         *string                `db:"primary_color" json:"primary_color,omitempty"`
	Address              *string                `db:"address" json:"address,omitempty"`
	Country              *string                `db:"country" json:"country,omitempty"`
	Timezone             *string                `db:"timezone" json:"timezone,omitempty"`
	Currency             *string                `db:"currency" json:"currency,omitempty"`
	Phone                *string                `db:"phone" json:"phone,omitempty"`
	Email                *string                `db:"email" json:"email,omitempty"`
	Website              *string                `db:"website" json:"website,omitempty"`
	StripeAccountID      *string                `db:"stripe_account_id" json:"stripe_account_id,omitempty"`
	StripeEnabled        bool                   `db:"stripe_enabled" json:"stripe_enabled"`
	RazorpayKeyID        *string                `db:"razorpay_key_id" json:"razorpay_key_id,omitempty"`
	RazorpayEnabled      bool                   `db:"razorpay_enabled" json:"razorpay_enabled"`
	ActivePaymentGateway string                 `db:"active_payment_gateway" json:"active_payment_gateway"`
	CreatedAt            time.Time              `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time              `db:"updated_at" json:"updated_at"`
}

type Property struct {
	ID         uuid.UUID `db:"id" json:"id"`
	HotelID    uuid.UUID `db:"hotel_id" json:"hotel_id"`
	Name       string    `db:"name" json:"name"`
	Address    *string   `db:"address" json:"address,omitempty"`
	StarRating *int      `db:"star_rating" json:"star_rating,omitempty"`
	TotalRooms *int      `db:"total_rooms" json:"total_rooms,omitempty"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time `db:"updated_at" json:"updated_at"`
}

type RoomType struct {
	ID                uuid.UUID              `db:"id" json:"id"`
	HotelID           uuid.UUID              `db:"hotel_id" json:"hotel_id"`
	PropertyID        *uuid.UUID             `db:"property_id" json:"property_id,omitempty"`
	Name              string                 `db:"name" json:"name"`
	Description       *string                `db:"description" json:"description,omitempty"`
	BasePricePerNight float64                `db:"base_price_per_night" json:"base_price_per_night"`
	MaxCapacity       int                    `db:"max_capacity" json:"max_capacity"`
	Amenities         []string               `db:"-" json:"amenities"`
	IsActive          bool                   `db:"is_active" json:"is_active"`
	CreatedAt         time.Time              `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time              `db:"updated_at" json:"updated_at"`
	RawAmenities      map[string]interface{} `db:"amenities" json:"-"`
}

type TaxConfig struct {
	ID          uuid.UUID `db:"id" json:"id"`
	HotelID     uuid.UUID `db:"hotel_id" json:"hotel_id"`
	Name        string    `db:"name" json:"name"`
	Rate        float64   `db:"rate" json:"rate"`
	AppliesTo   string    `db:"applies_to" json:"applies_to"`
	IsInclusive bool      `db:"is_inclusive" json:"is_inclusive"`
	IsActive    bool      `db:"is_active" json:"is_active"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

type HotelBranding struct {
	HotelID            uuid.UUID `db:"hotel_id" json:"hotel_id"`
	LogoURL            *string   `db:"logo_url" json:"logo_url,omitempty"`
	PrimaryColor       string    `db:"primary_color" json:"primary_color"`
	ClientPrimaryColor string    `db:"client_primary_color" json:"client_primary_color,omitempty"`
	AdminPrimaryColor  string    `db:"admin_primary_color" json:"admin_primary_color,omitempty"`
	WelcomeMessage     *string   `db:"welcome_message" json:"welcome_message,omitempty"`
	FooterText         *string   `db:"footer_text" json:"footer_text,omitempty"`
	HotelName          string    `db:"-" json:"hotel_name,omitempty"`
	Slug               string    `db:"-" json:"slug,omitempty"`
	Country            *string   `db:"-" json:"country,omitempty"`
	Currency           *string   `db:"-" json:"currency,omitempty"`
	UpdatedAt          time.Time `db:"updated_at" json:"updated_at"`
}

// User is the authentication principal.
type User struct {
	ID            uuid.UUID  `db:"id" json:"id"`
	HotelID       *uuid.UUID `db:"hotel_id" json:"hotel_id,omitempty"`
	Email         string     `db:"email" json:"email"`
	PasswordHash  string     `db:"password_hash" json:"-"`
	PlatformAdmin bool       `db:"platform_admin" json:"platform_admin"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`
}

// Profile holds display-level information about a user.
type Profile struct {
	ID        uuid.UUID  `db:"id" json:"id"`
	HotelID   *uuid.UUID `db:"hotel_id" json:"hotel_id,omitempty"`
	UserID    uuid.UUID  `db:"user_id" json:"user_id"`
	FullName  string     `db:"full_name" json:"full_name"`
	Phone     *string    `db:"phone" json:"phone,omitempty"`
	AvatarURL *string    `db:"avatar_url" json:"avatar_url,omitempty"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt time.Time  `db:"updated_at" json:"updated_at"`
}

// UserRoleEntry assigns a role to a user.
type UserRoleEntry struct {
	ID        uuid.UUID  `db:"id" json:"id"`
	HotelID   *uuid.UUID `db:"hotel_id" json:"hotel_id,omitempty"`
	UserID    uuid.UUID  `db:"user_id" json:"user_id"`
	Role      UserRole   `db:"role" json:"role"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
}

// Room represents a physical hotel room.
type Room struct {
	ID            uuid.UUID  `db:"id" json:"id"`
	HotelID       uuid.UUID  `db:"hotel_id" json:"hotel_id"`
	RoomNumber    string     `db:"room_number" json:"room_number"`
	RoomType      string     `db:"room_type" json:"room_type"`
	Floor         int        `db:"floor" json:"floor"`
	Capacity      int        `db:"capacity" json:"capacity"`
	PricePerNight float64    `db:"price_per_night" json:"price_per_night"`
	Status        RoomStatus `db:"status" json:"status"`
	Amenities     []string   `db:"amenities" json:"amenities"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`
}

// GuestStay is a single booking / check-in record.
type GuestStay struct {
	ID             uuid.UUID    `db:"id" json:"id"`
	HotelID        uuid.UUID    `db:"hotel_id" json:"hotel_id"`
	GuestID        *uuid.UUID   `db:"guest_id" json:"guest_id,omitempty"`
	RoomID         uuid.UUID    `db:"room_id" json:"room_id"`
	GuestName      string       `db:"guest_name" json:"guest_name"`
	GuestEmail     *string      `db:"guest_email" json:"guest_email,omitempty"`
	GuestPhone     *string      `db:"guest_phone" json:"guest_phone,omitempty"`
	CheckInDate    time.Time    `db:"check_in_date" json:"check_in_date"`
	CheckOutDate   time.Time    `db:"check_out_date" json:"check_out_date"`
	ActualCheckIn  *time.Time   `db:"actual_check_in" json:"actual_check_in,omitempty"`
	ActualCheckOut *time.Time   `db:"actual_check_out" json:"actual_check_out,omitempty"`
	TotalAmount    *float64     `db:"total_amount" json:"total_amount,omitempty"`
	Notes          *string      `db:"notes" json:"notes,omitempty"`
	Source         *string      `db:"source" json:"source,omitempty"`
	CreatedBy      *uuid.UUID   `db:"created_by" json:"created_by,omitempty"`
	CreatedAt      time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time    `db:"updated_at" json:"updated_at"`
	Room           *RoomSummary `db:"-" json:"rooms,omitempty"`
}

// RoomSummary is a lightweight projection used for enrichment.
type RoomSummary struct {
	RoomNumber string `json:"room_number"`
	RoomType   string `json:"room_type"`
}

// MenuCategory groups menu items.
type MenuCategory struct {
	ID           uuid.UUID `db:"id" json:"id"`
	Name         string    `db:"name" json:"name"`
	Description  *string   `db:"description" json:"description,omitempty"`
	DisplayOrder int       `db:"display_order" json:"display_order"`
	IsActive     bool      `db:"is_active" json:"is_active"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// MenuItem is a single dish or drink on the menu.
type MenuItem struct {
	ID              uuid.UUID               `db:"id" json:"id"`
	CategoryID      *uuid.UUID              `db:"category_id" json:"category_id,omitempty"`
	Name            string                  `db:"name" json:"name"`
	Description     *string                 `db:"description" json:"description,omitempty"`
	Price           float64                 `db:"price" json:"price"`
	ImageURL        *string                 `db:"image_url" json:"image_url,omitempty"`
	IsAvailable     bool                    `db:"is_available" json:"is_available"`
	PreparationTime int                     `db:"preparation_time" json:"preparation_time"`
	CreatedAt       time.Time               `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time               `db:"updated_at" json:"updated_at"`
	Category        *MenuCategorySummary    `db:"-" json:"menu_categories,omitempty"`
	Customizations  []MenuItemCustomization `db:"-" json:"menu_item_customizations,omitempty"`
}

type MenuCategorySummary struct {
	Name string `json:"name"`
}

// MenuItemCustomization is a modifier/add-on for a menu item.
type MenuItemCustomization struct {
	ID          uuid.UUID `db:"id" json:"id"`
	MenuItemID  uuid.UUID `db:"menu_item_id" json:"menu_item_id"`
	Name        string    `db:"name" json:"name"`
	Price       float64   `db:"price" json:"price"`
	IsAvailable bool      `db:"is_available" json:"is_available"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// InventoryItem tracks kitchen / housekeeping stock.
type InventoryItem struct {
	ID           uuid.UUID  `db:"id" json:"id"`
	Name         string     `db:"name" json:"name"`
	Unit         string     `db:"unit" json:"unit"`
	CurrentStock float64    `db:"current_stock" json:"current_stock"`
	MinStock     float64    `db:"min_stock" json:"min_stock"`
	CostPerUnit  *float64   `db:"cost_per_unit" json:"cost_per_unit,omitempty"`
	IsPerishable bool       `db:"is_perishable" json:"is_perishable"`
	ExpiryDate   *time.Time `db:"expiry_date" json:"expiry_date,omitempty"`
	Supplier     *string    `db:"supplier" json:"supplier,omitempty"`
	CreatedAt    time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at" json:"updated_at"`
}

// Recipe links a MenuItem to one or more InventoryItems.
type Recipe struct {
	ID               uuid.UUID `db:"id" json:"id"`
	MenuItemID       uuid.UUID `db:"menu_item_id" json:"menu_item_id"`
	InventoryItemID  uuid.UUID `db:"inventory_item_id" json:"inventory_item_id"`
	QuantityRequired float64   `db:"quantity_required" json:"quantity_required"`
	CreatedAt        time.Time `db:"created_at" json:"created_at"`
}

// Order is a food/beverage order from a guest.
type Order struct {
	ID                  uuid.UUID   `db:"id" json:"id"`
	OrderNumber         string      `db:"order_number" json:"order_number"`
	GuestStayID         *uuid.UUID  `db:"guest_stay_id" json:"guest_stay_id,omitempty"`
	RoomID              *uuid.UUID  `db:"room_id" json:"room_id,omitempty"`
	GuestID             *uuid.UUID  `db:"guest_id" json:"guest_id,omitempty"`
	Status              OrderStatus `db:"status" json:"status"`
	SpecialInstructions *string     `db:"special_instructions" json:"special_instructions,omitempty"`
	TotalAmount         float64     `db:"total_amount" json:"total_amount"`
	AssignedWaiterID    *uuid.UUID  `db:"assigned_waiter_id" json:"assigned_waiter_id,omitempty"`
	CreatedBy           *uuid.UUID  `db:"created_by" json:"created_by,omitempty"`
	KitchenNotes        *string     `db:"kitchen_notes" json:"kitchen_notes,omitempty"`
	PickupTime          *time.Time  `db:"pickup_time" json:"pickup_time,omitempty"`
	DeliveryTime        *time.Time  `db:"delivery_time" json:"delivery_time,omitempty"`
	Rating              *int        `db:"rating" json:"rating,omitempty"`
	Feedback            *string     `db:"feedback" json:"feedback,omitempty"`
	CreatedAt           time.Time   `db:"created_at" json:"created_at"`
	UpdatedAt           time.Time   `db:"updated_at" json:"updated_at"`
	Room                *struct {
		RoomNumber string `json:"room_number"`
	} `db:"-" json:"rooms,omitempty"`
	GuestStay *struct {
		GuestName string `json:"guest_name"`
	} `db:"-" json:"guest_stays,omitempty"`
	Items []OrderItem `db:"-" json:"order_items,omitempty"`
}

// OrderItem is one line of an order.
type OrderItem struct {
	ID         uuid.UUID `db:"id" json:"id"`
	OrderID    uuid.UUID `db:"order_id" json:"order_id"`
	MenuItemID uuid.UUID `db:"menu_item_id" json:"menu_item_id"`
	Quantity   int       `db:"quantity" json:"quantity"`
	UnitPrice  float64   `db:"unit_price" json:"unit_price"`
	Notes      *string   `db:"notes" json:"notes,omitempty"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	MenuItem   *struct {
		Name string `json:"name"`
	} `db:"-" json:"menu_items,omitempty"`
}

// StaffShift tracks clock-in / clock-out for staff.
type StaffShift struct {
	ID        uuid.UUID  `db:"id" json:"id"`
	UserID    uuid.UUID  `db:"user_id" json:"user_id"`
	ClockIn   time.Time  `db:"clock_in" json:"clock_in"`
	ClockOut  *time.Time `db:"clock_out" json:"clock_out,omitempty"`
	Notes     *string    `db:"notes" json:"notes,omitempty"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
}

// Complaint is a guest complaint record.
type Complaint struct {
	ID              uuid.UUID         `db:"id" json:"id"`
	ComplaintNumber string            `db:"complaint_number" json:"complaint_number"`
	GuestStayID     *uuid.UUID        `db:"guest_stay_id" json:"guest_stay_id,omitempty"`
	GuestID         *uuid.UUID        `db:"guest_id" json:"guest_id,omitempty"`
	Category        string            `db:"category" json:"category"`
	Priority        ComplaintPriority `db:"priority" json:"priority"`
	Status          ComplaintStatus   `db:"status" json:"status"`
	Description     string            `db:"description" json:"description"`
	Resolution      *string           `db:"resolution" json:"resolution,omitempty"`
	ResolvedBy      *uuid.UUID        `db:"resolved_by" json:"resolved_by,omitempty"`
	ResolvedAt      *time.Time        `db:"resolved_at" json:"resolved_at,omitempty"`
	GuestFeedback   *string           `db:"guest_feedback" json:"guest_feedback,omitempty"`
	CreatedBy       *uuid.UUID        `db:"created_by" json:"created_by,omitempty"`
	CreatedAt       time.Time         `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time         `db:"updated_at" json:"updated_at"`
	GuestStay       *GuestStaySummary `db:"-" json:"guest_stays,omitempty"`
}

type GuestStaySummary struct {
	GuestName string       `json:"guest_name"`
	RoomID    *uuid.UUID   `json:"room_id,omitempty"`
	Room      *RoomSummary `json:"rooms,omitempty"`
}

// Payment is a financial transaction record.
type Payment struct {
	ID            uuid.UUID     `db:"id" json:"id"`
	HotelID       uuid.UUID     `db:"hotel_id" json:"hotel_id"`
	PaymentNumber string        `db:"payment_number" json:"payment_number"`
	GuestStayID   *uuid.UUID    `db:"guest_stay_id" json:"guest_stay_id,omitempty"`
	OrderID       *uuid.UUID    `db:"order_id" json:"order_id,omitempty"`
	Amount        float64       `db:"amount" json:"amount"`
	PaymentMethod string        `db:"payment_method" json:"payment_method"`
	Status        PaymentStatus `db:"status" json:"status"`
	ProcessedBy   *uuid.UUID    `db:"processed_by" json:"processed_by,omitempty"`
	Notes         *string       `db:"notes" json:"notes,omitempty"`
	CreatedAt     time.Time     `db:"created_at" json:"created_at"`
	Order         *struct {
		OrderNumber string `json:"order_number"`
	} `db:"-" json:"orders,omitempty"`
	GuestStay *PaymentGuestStaySummary `db:"-" json:"guest_stays,omitempty"`
}

type PaymentGuestStaySummary struct {
	GuestName string       `json:"guest_name"`
	GuestID   *uuid.UUID   `json:"guest_id,omitempty"`
	RoomID    *uuid.UUID   `json:"room_id,omitempty"`
	Room      *RoomSummary `json:"rooms,omitempty"`
}

// WasteLog records discarded inventory.
type WasteLog struct {
	ID              uuid.UUID  `db:"id" json:"id"`
	InventoryItemID uuid.UUID  `db:"inventory_item_id" json:"inventory_item_id"`
	Quantity        float64    `db:"quantity" json:"quantity"`
	Reason          string     `db:"reason" json:"reason"`
	LoggedBy        *uuid.UUID `db:"logged_by" json:"logged_by,omitempty"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
}

// AuditLog is an immutable record of every data mutation.
type AuditLog struct {
	ID        uuid.UUID              `db:"id" json:"id"`
	UserID    *uuid.UUID             `db:"user_id" json:"user_id,omitempty"`
	Action    string                 `db:"action" json:"action"`
	TableName string                 `db:"table_name" json:"table_name"`
	RecordID  *uuid.UUID             `db:"record_id" json:"record_id,omitempty"`
	OldData   map[string]interface{} `db:"old_data" json:"old_data,omitempty"`
	NewData   map[string]interface{} `db:"new_data" json:"new_data,omitempty"`
	CreatedAt time.Time              `db:"created_at" json:"created_at"`
}

// GuestPreferences stores dietary and preference data for a guest.
type GuestPreferences struct {
	ID                  uuid.UUID `db:"id" json:"id"`
	UserID              uuid.UUID `db:"user_id" json:"user_id"`
	DietaryRestrictions []string  `db:"dietary_restrictions" json:"dietary_restrictions"`
	Allergies           []string  `db:"allergies" json:"allergies"`
	FavoriteCategories  []string  `db:"favorite_categories" json:"favorite_categories"`
	Notes               *string   `db:"notes" json:"notes,omitempty"`
	CreatedAt           time.Time `db:"created_at" json:"created_at"`
	UpdatedAt           time.Time `db:"updated_at" json:"updated_at"`
}

// PaymentSetting is a gateway configuration record.
type PaymentSetting struct {
	ID          uuid.UUID  `db:"id" json:"id"`
	HotelID     uuid.UUID  `db:"hotel_id" json:"hotel_id"`
	GatewayName string     `db:"gateway_name" json:"gateway_name"`
	WebhookURL  *string    `db:"webhook_url" json:"webhook_url,omitempty"`
	IsActive    bool       `db:"is_active" json:"is_active"`
	CreatedBy   *uuid.UUID `db:"created_by" json:"created_by,omitempty"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
}

// PaymentGatewayConfig stores per-hotel gateway settings. Secret fields are
// encrypted at rest and never serialized to clients.
type PaymentGatewayConfig struct {
	HotelID                      uuid.UUID `json:"hotel_id"`
	ActiveGateway                string    `json:"active_gateway"`
	DefaultCurrency              string    `json:"default_currency"`
	GatewayMode                  string    `json:"gateway_mode"`
	StripeEnabled                bool      `json:"stripe_enabled"`
	StripeAccountID              *string   `json:"stripe_account_id,omitempty"`
	StripePublishableKey         *string   `json:"stripe_publishable_key,omitempty"`
	StripeSecretKeyEncrypted     *string   `json:"-"`
	StripeWebhookSecretEncrypted *string   `json:"-"`
	RazorpayEnabled              bool      `json:"razorpay_enabled"`
	RazorpayKeyID                *string   `json:"razorpay_key_id,omitempty"`
	RazorpayKeySecretEncrypted   *string   `json:"-"`
	CashEnabled                  bool      `json:"cash_enabled"`
	CardEnabled                  bool      `json:"card_enabled"`
	BankTransferEnabled          bool      `json:"bank_transfer_enabled"`
}

// ChartRevenuePoint is one day of revenue breakdown.
type ChartRevenuePoint struct {
	Date  string  `json:"date"`
	Room  float64 `json:"room"`
	FnB   float64 `json:"fnb"`
	Other float64 `json:"other"`
}

// ChartOccupancyPoint is one day of occupancy data.
type ChartOccupancyPoint struct {
	Date      string  `json:"date"`
	Occupied  int     `json:"occupied"`
	Available int     `json:"available"`
	Rate      float64 `json:"rate"`
}

// DeptRevenueItem is a department's current vs previous month revenue.
type DeptRevenueItem struct {
	Department string  `json:"department"`
	Current    float64 `json:"current"`
	Previous   float64 `json:"previous"`
}

// GuestStayItem is a short guest stay summary for dashboard lists.
type GuestStayItem struct {
	GuestName string `json:"guest_name"`
	Room      string `json:"room"`
	Status    string `json:"status"`
}

// PendingPaymentItem is a short payment summary.
type PendingPaymentItem struct {
	GuestName string  `json:"guest_name"`
	Amount    float64 `json:"amount"`
	DueDate   string  `json:"due_date"`
	Status    string  `json:"status"`
}

// ActivityItem is a single audit log entry.
type ActivityItem struct {
	Action    string `json:"action"`
	User      string `json:"user"`
	Details   string `json:"details"`
	CreatedAt string `json:"created_at"`
}

// DashboardChartData is the full dashboard data payload.
type DashboardChartData struct {
	RevenueTrend      []ChartRevenuePoint  `json:"revenue_trend"`
	OccupancyTrend    []ChartOccupancyPoint `json:"occupancy_trend"`
	DepartmentRevenue []DeptRevenueItem    `json:"department_revenue"`
	ArrivalsToday     []GuestStayItem      `json:"arrivals_today"`
	DeparturesToday   []GuestStayItem      `json:"departures_today"`
	PendingPayments   []PendingPaymentItem `json:"pending_payments"`
	RecentActivity    []ActivityItem       `json:"recent_activity"`
}

// DashboardStats is the aggregated read model for the dashboard.
type DashboardStats struct {
	OccupancyRate          float64 `json:"occupancy_rate"`
	RoomsAvailable         int     `json:"rooms_available"`
	RoomsOccupied          int     `json:"rooms_occupied"`
	ActiveOrders           int     `json:"active_orders"`
	PendingComplaints      int     `json:"pending_complaints"`
	RevenueToday           float64 `json:"revenue_today"`
	LowStockItems          int     `json:"low_stock_items"`
	StaffClockedIn         int     `json:"staff_clocked_in"`
	GuestsCheckingInToday  int     `json:"guests_checking_in_today"`
	GuestsCheckingOutToday int     `json:"guests_checking_out_today"`
}

type ChatMessage struct {
	Role    string `json:"role" validate:"required,oneof=user assistant system"`
	Content string `json:"content" validate:"required"`
}

type InventoryAlert struct {
	ItemID           uuid.UUID `json:"item_id"`
	Name             string    `json:"name"`
	CurrentStock     float64   `json:"current_stock"`
	MinStock         float64   `json:"min_stock"`
	Severity         string    `json:"severity"`
	AIRecommendation string    `json:"ai_recommendation"`
	IsPerishable     bool      `json:"is_perishable"`
}
