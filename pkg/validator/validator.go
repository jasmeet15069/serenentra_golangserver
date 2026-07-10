package validator

import (
	"fmt"

	"github.com/go-playground/validator/v10"
)

type Validator struct {
	validate *validator.Validate
}

func New() *Validator {
	return &Validator{validate: validator.New()}
}

func (v *Validator) Struct(value interface{}) error {
	if err := v.validate.Struct(value); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	return nil
}
