package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/pkg/response"
)

const (
	LocalUserIDKey        = "user_id"
	LocalHotelIDKey       = "hotel_id"
	LocalRolesKey         = "roles"
	LocalPlatformAdminKey = "platform_admin"
)

func Auth(authSvc service.AuthService) fiber.Handler {
	return func(c *fiber.Ctx) error {
		header := c.Get("Authorization")
		token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		if token == "" || token == header {
			return response.Error(c, fiber.StatusUnauthorized, "missing bearer token")
		}

		user, roles, err := authSvc.GetUserFromToken(c.Context(), token)
		if err != nil {
			return response.Error(c, fiber.StatusUnauthorized, err.Error())
		}
		c.Locals(LocalUserIDKey, user.ID)
		if user.HotelID != nil {
			c.Locals(LocalHotelIDKey, *user.HotelID)
		}
		c.Locals(LocalRolesKey, roles)
		c.Locals(LocalPlatformAdminKey, user.PlatformAdmin)
		return c.Next()
	}
}

func RequireRoles(required ...domain.UserRole) fiber.Handler {
	return func(c *fiber.Ctx) error {
		roles, _ := c.Locals(LocalRolesKey).([]domain.UserRole)
		if !service.HasRole(roles, required...) {
			return response.Error(c, fiber.StatusForbidden, "access denied")
		}
		return c.Next()
	}
}
