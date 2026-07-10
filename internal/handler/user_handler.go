package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/internal/domain"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/pkg/response"
	"github.com/hotelharmony/api/pkg/validator"
)

type UserHandler struct {
	userRepo postgres.UserRepository
	authSvc  service.AuthService
	validate *validator.Validator
	secret   string
}

func NewUserHandler(userRepo postgres.UserRepository, authSvc service.AuthService, validate *validator.Validator, secret string) *UserHandler {
	return &UserHandler{userRepo: userRepo, authSvc: authSvc, validate: validate, secret: secret}
}

// adminOnly is inline middleware that restricts the route to hotel_admin /
// super_admin / platform_admin callers. Other authenticated users get 403.
func (h *UserHandler) adminOnly(c *fiber.Ctx) error {
	claims, err := jwtClaimsFromRequest(c, h.secret)
	if err != nil {
		_ = response.Error(c, fiber.StatusUnauthorized, "authentication is required")
		return nil
	}
	if pa, _ := claims["platform_admin"].(bool); pa {
		return c.Next()
	}
	rawRoles, _ := claims["roles"].([]interface{})
	for _, rr := range rawRoles {
		if role, _ := rr.(string); role == "hotel_admin" || role == "super_admin" || role == "platform_admin" {
			return c.Next()
		}
	}
	_ = response.Error(c, fiber.StatusForbidden, "hotel admin role required")
	return nil
}

func (h *UserHandler) Register(r fiber.Router) {
	r.Get("/users", h.List)
	r.Post("/users", h.adminOnly, h.CreateStaff)
	r.Get("/users/:id", h.Get)
	r.Patch("/users/:id", h.adminOnly, h.Update)
	r.Delete("/users/:id", h.adminOnly, h.Delete)
	r.Put("/users/:id/role", h.adminOnly, h.SetRole)
	r.Post("/users/:id/password", h.adminOnly, h.ResetPassword)
	r.Post("/users/:id/roles", h.adminOnly, h.AddRole)
	r.Delete("/users/:id/roles/:role", h.adminOnly, h.RemoveRole)
}

type userListItem struct {
	ID       uuid.UUID         `json:"id"`
	Email    string            `json:"email"`
	FullName string            `json:"full_name"`
	Phone    *string           `json:"phone,omitempty"`
	Roles    []domain.UserRole `json:"roles"`
	JoinedAt string            `json:"joined_at"`
}

// userInTenant fetches a user and returns 404 if it doesn't belong to the
// caller's hotel, preventing cross-tenant enumeration via user IDs.
func (h *UserHandler) userInTenant(c *fiber.Ctx, userID uuid.UUID) (*domain.User, error) {
	user, err := h.userRepo.FindByID(c.Context(), userID)
	if err != nil {
		return nil, response.Error(c, fiber.StatusNotFound, "user not found")
	}
	callerHotel := tenantHotelID(c)
	if user.HotelID == nil || *user.HotelID != callerHotel {
		return nil, response.Error(c, fiber.StatusNotFound, "user not found")
	}
	return user, nil
}

type createStaffRequest struct {
	Email    string          `json:"email" validate:"required,email"`
	Password string          `json:"password" validate:"required,min=8"`
	FullName string          `json:"full_name"`
	Role     domain.UserRole `json:"role"`
}

func (h *UserHandler) CreateStaff(c *fiber.Ctx) error {
	var req createStaffRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	hotelID := tenantHotelID(c)
	user, err := h.authSvc.CreateStaffMember(c.Context(), hotelID, req.Email, req.Password, req.FullName, req.Role)
	if err != nil {
		if errors.Is(err, service.ErrEmailTaken) {
			return response.Error(c, fiber.StatusConflict, "a user with this email already exists")
		}
		return response.Error(c, fiber.StatusInternalServerError, "failed to create user")
	}
	return response.OK(c, map[string]interface{}{
		"id":    user.ID,
		"email": user.Email,
	})
}

func (h *UserHandler) List(c *fiber.Ctx) error {
	hotelID := tenantHotelID(c)
	users, err := h.userRepo.List(c.Context(), &hotelID)
	if err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to list users")
	}

	items := make([]userListItem, 0, len(users))
	for _, u := range users {
		profile, _ := h.userRepo.FindProfileByUserID(c.Context(), u.ID)
		roles, _ := h.userRepo.GetRoles(c.Context(), u.ID)
		fullName := ""
		if profile != nil {
			fullName = profile.FullName
		}
		items = append(items, userListItem{
			ID:       u.ID,
			Email:    u.Email,
			FullName: fullName,
			Phone:    nil,
			Roles:    roles,
			JoinedAt: u.CreatedAt.Format("2006-01-02"),
		})
	}
	return response.OK(c, items)
}

func (h *UserHandler) Get(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id")
	}

	user, err := h.userInTenant(c, id)
	if err != nil {
		return err
	}

	profile, _ := h.userRepo.FindProfileByUserID(c.Context(), id)
	roles, _ := h.userRepo.GetRoles(c.Context(), id)

	return response.OK(c, map[string]interface{}{
		"id":             user.ID,
		"email":          user.Email,
		"platform_admin": user.PlatformAdmin,
		"profile":        profile,
		"roles":          roles,
		"created_at":     user.CreatedAt,
	})
}

type updateUserRequest struct {
	FullName *string `json:"full_name,omitempty"`
	Phone    *string `json:"phone,omitempty"`
}

func (h *UserHandler) Update(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id")
	}
	if _, err := h.userInTenant(c, id); err != nil {
		return err
	}

	var req updateUserRequest
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	fields := make(map[string]interface{})
	if req.FullName != nil {
		fields["full_name"] = *req.FullName
	}
	if req.Phone != nil {
		fields["phone"] = *req.Phone
	}

	if len(fields) > 0 {
		if err := h.userRepo.UpdateProfile(c.Context(), id, fields); err != nil {
			return response.Error(c, fiber.StatusInternalServerError, "failed to update user")
		}
	}

	profile, _ := h.userRepo.FindProfileByUserID(c.Context(), id)
	return response.OK(c, profile)
}

func (h *UserHandler) AddRole(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id")
	}
	if _, err := h.userInTenant(c, id); err != nil {
		return err
	}

	var req struct {
		Role domain.UserRole `json:"role"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Role == "" {
		return response.Error(c, fiber.StatusBadRequest, "role is required")
	}

	if err := h.userRepo.AddRole(c.Context(), id, req.Role); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to add role")
	}

	roles, _ := h.userRepo.GetRoles(c.Context(), id)
	return response.OK(c, roles)
}

// SetRole replaces all non-guest roles with a single new role.
func (h *UserHandler) SetRole(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id")
	}
	if _, err := h.userInTenant(c, id); err != nil {
		return err
	}
	var req struct {
		Role domain.UserRole `json:"role"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if req.Role == "" {
		return response.Error(c, fiber.StatusBadRequest, "role is required")
	}
	if err := h.userRepo.RemoveAllRoles(c.Context(), id); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update role")
	}
	if err := h.userRepo.AddRole(c.Context(), id, req.Role); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to update role")
	}
	roles, _ := h.userRepo.GetRoles(c.Context(), id)
	return response.OK(c, roles)
}

// Delete permanently removes a staff user from this tenant.
func (h *UserHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id")
	}
	if _, err := h.userInTenant(c, id); err != nil {
		return err
	}
	if err := h.userRepo.Delete(c.Context(), id); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to delete user")
	}
	return response.OK(c, map[string]string{"status": "deleted"})
}

// ResetPassword lets a hotel admin set a new password for any tenant user without
// knowing the current password.
func (h *UserHandler) ResetPassword(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id")
	}
	if _, err := h.userInTenant(c, id); err != nil {
		return err
	}
	var req struct {
		Password string `json:"password" validate:"required,min=8"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}
	if err := h.validate.Struct(req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, err.Error())
	}
	if err := h.authSvc.UpdatePassword(c.Context(), id, req.Password); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to reset password")
	}
	return response.OK(c, map[string]string{"status": "updated"})
}

func (h *UserHandler) RemoveRole(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid user id")
	}
	if _, err := h.userInTenant(c, id); err != nil {
		return err
	}

	role := domain.UserRole(c.Params("role"))
	if role == "" {
		return response.Error(c, fiber.StatusBadRequest, "role is required")
	}

	if err := h.userRepo.RemoveRole(c.Context(), id, role); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to remove role")
	}

	roles, _ := h.userRepo.GetRoles(c.Context(), id)
	return response.OK(c, roles)
}
