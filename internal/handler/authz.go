package handler

import (
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"

	"github.com/hotelharmony/api/pkg/response"
)

func jwtClaimsFromRequest(c *fiber.Ctx, secret string) (jwt.MapClaims, error) {
	tokenString := authTokenFromRequest(c)
	if tokenString == "" || secret == "" {
		return nil, fmt.Errorf("missing bearer token")
	}

	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("invalid signing method")
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return nil, fmt.Errorf("invalid bearer token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("invalid bearer token")
	}
	return claims, nil
}

func authTokenFromRequest(c *fiber.Ctx) string {
	authHeader := c.Get("Authorization")
	tokenString := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if tokenString != "" && tokenString != authHeader {
		return tokenString
	}
	return strings.TrimSpace(c.Cookies("hotelops_session"))
}

func requireAuthenticatedRequest(c *fiber.Ctx, secret string) bool {
	if _, err := jwtClaimsFromRequest(c, secret); err != nil {
		_ = response.Error(c, fiber.StatusUnauthorized, "authentication is required")
		return false
	}
	return true
}

func requireAnyRoleFromToken(c *fiber.Ctx, secret string, allowed ...string) bool {
	claims, err := jwtClaimsFromRequest(c, secret)
	if err != nil {
		_ = response.Error(c, fiber.StatusUnauthorized, "authentication is required")
		return false
	}

	rawRoles, ok := claims["roles"].([]interface{})
	if !ok {
		_ = response.Error(c, fiber.StatusForbidden, "required role is missing")
		return false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, role := range allowed {
		allowedSet[role] = struct{}{}
	}
	for _, rawRole := range rawRoles {
		role, _ := rawRole.(string)
		if _, ok := allowedSet[role]; ok {
			return true
		}
	}
	_ = response.Error(c, fiber.StatusForbidden, "access denied")
	return false
}
