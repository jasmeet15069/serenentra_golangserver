package handler

import "strings"

type planTierSpec struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	MaxRooms         *int     `json:"max_rooms"`
	MaxUsers         *int     `json:"max_users"`
	MaxProperties    *int     `json:"max_properties"`
	AllowedRoles     []string `json:"allowed_roles"`
	AIAddon          bool     `json:"ai_addon"`
	AITextConcierge  bool     `json:"ai_text_concierge"`
	AIVoiceAgent     bool     `json:"ai_voice_agent"`
	AIVoiceBooking   bool     `json:"ai_voice_booking"`
	DatabaseStrategy string   `json:"database_strategy"`
}

var allOperationalRoles = []string{
	"hotel_admin",
	"property_manager",
	"receptionist",
	"housekeeping",
	"maintenance",
	"food_manager",
	"kitchen_manager",
	"waiter",
	"guest",
}

var planTierSpecs = []planTierSpec{
	{
		ID:               "basic",
		Name:             "Basic",
		Description:      "Starter hotel operations with limited rooms, users, and core roles.",
		MaxRooms:         intPtr(50),
		MaxUsers:         intPtr(10),
		MaxProperties:    intPtr(1),
		AllowedRoles:     []string{"hotel_admin", "receptionist", "housekeeping", "maintenance", "guest"},
		AIAddon:          false,
		AITextConcierge:  false,
		AIVoiceAgent:     false,
		AIVoiceBooking:   false,
		DatabaseStrategy: "tenant_isolated",
	},
	{
		ID:               "pro",
		Name:             "Pro",
		Description:      "More rooms, staff seats, department portals, and AI text assistance.",
		MaxRooms:         intPtr(200),
		MaxUsers:         intPtr(50),
		MaxProperties:    intPtr(3),
		AllowedRoles:     allOperationalRoles,
		AIAddon:          true,
		AITextConcierge:  true,
		AIVoiceAgent:     false,
		AIVoiceBooking:   false,
		DatabaseStrategy: "tenant_isolated",
	},
	{
		ID:               "premium",
		Name:             "Premium",
		Description:      "Unlimited hotel group scale with AI voice agent and AI-assisted bookings.",
		MaxRooms:         nil,
		MaxUsers:         nil,
		MaxProperties:    nil,
		AllowedRoles:     allOperationalRoles,
		AIAddon:          true,
		AITextConcierge:  true,
		AIVoiceAgent:     true,
		AIVoiceBooking:   true,
		DatabaseStrategy: "tenant_dedicated_db_ready",
	},
}

func intPtr(v int) *int {
	return &v
}

func normalizePlanTier(plan string) string {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "starter", "basic":
		return "basic"
	case "growth", "pro":
		return "pro"
	case "enterprise", "premium":
		return "premium"
	default:
		return "basic"
	}
}

func planTierByID(plan string) planTierSpec {
	id := normalizePlanTier(plan)
	for _, spec := range planTierSpecs {
		if spec.ID == id {
			return spec
		}
	}
	return planTierSpecs[0]
}

func settingsForPlanTier(plan, slug string) map[string]interface{} {
	spec := planTierByID(plan)
	dbName := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(slug)), "-", "_")
	dbName = strings.Trim(dbName, "_")
	if dbName == "" {
		dbName = "hotelops_tenant"
	}
	dbName += "_hotelops"

	return map[string]interface{}{
		"max_rooms":           spec.MaxRooms,
		"max_users":           spec.MaxUsers,
		"max_properties":      spec.MaxProperties,
		"allowed_roles":       spec.AllowedRoles,
		"ai_addon":            spec.AIAddon,
		"ai_text_concierge":   spec.AITextConcierge,
		"ai_voice_agent":      spec.AIVoiceAgent,
		"ai_voice_booking":    spec.AIVoiceBooking,
		"database_strategy":   spec.DatabaseStrategy,
		"database_name":       dbName,
		"billing_plan_locked": false,
	}
}
