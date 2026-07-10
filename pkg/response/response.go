package response

import "github.com/gofiber/fiber/v2"

type Envelope struct {
	Data  interface{} `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
	Code  string      `json:"code,omitempty"`
}

func OK(c *fiber.Ctx, data interface{}) error {
	return c.JSON(Envelope{Data: data})
}

func Created(c *fiber.Ctx, data interface{}) error {
	return c.Status(fiber.StatusCreated).JSON(Envelope{Data: data})
}

func Error(c *fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(Envelope{Error: message})
}
