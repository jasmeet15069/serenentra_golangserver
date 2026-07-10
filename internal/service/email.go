package service

import (
	"fmt"
	"net/smtp"
	"strings"

	"github.com/hotelharmony/api/internal/config"
	"go.uber.org/zap"
)

type EmailService struct {
	cfg *config.Config
	log *zap.Logger
}

func NewEmailService(cfg *config.Config, log *zap.Logger) *EmailService {
	return &EmailService{cfg: cfg, log: log}
}

func (s *EmailService) Send(to, subject, body string) error {
	if s.cfg.Email.Password == "" || s.cfg.Email.Username == "" {
		s.log.Warn("email not configured, skipping send")
		return nil
	}

	auth := smtp.PlainAuth("", s.cfg.Email.Username, s.cfg.Email.Password, s.cfg.Email.Host)

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=\"UTF-8\"\r\n\r\n%s",
		s.cfg.Email.From, to, subject, body)

	addr := fmt.Sprintf("%s:%d", s.cfg.Email.Host, s.cfg.Email.Port)
	if err := smtp.SendMail(addr, auth, s.cfg.Email.From, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("email send: %w", err)
	}
	return nil
}

func (s *EmailService) SendBookingConfirmation(guestEmail, guestName, hotelName, roomNumber, checkIn, checkOut string) error {
	subject := fmt.Sprintf("Booking Confirmed - %s", hotelName)
	body := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;max-width:600px;margin:0 auto">
<h2>Booking Confirmed!</h2>
<p>Dear %s,</p>
<p>Your reservation at <strong>%s</strong> is confirmed.</p>
<table style="border-collapse:collapse;width:100%%">
<tr><td style="padding:8px;border:1px solid #ddd;font-weight:bold">Room</td><td style="padding:8px;border:1px solid #ddd">%s</td></tr>
<tr><td style="padding:8px;border:1px solid #ddd;font-weight:bold">Check-in</td><td style="padding:8px;border:1px solid #ddd">%s</td></tr>
<tr><td style="padding:8px;border:1px solid #ddd;font-weight:bold">Check-out</td><td style="padding:8px;border:1px solid #ddd">%s</td></tr>
</table>
<p>Thank you for choosing %s!</p>
</div>`, guestName, hotelName, roomNumber, checkIn, checkOut, hotelName)
	return s.Send(guestEmail, subject, body)
}

func (s *EmailService) SendInvoice(guestEmail, guestName, hotelName, invoiceID, amount, dueDate string) error {
	subject := fmt.Sprintf("Invoice %s - %s", invoiceID, hotelName)
	body := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;max-width:600px;margin:0 auto">
<h2>Invoice</h2>
<p>Dear %s,</p>
<p>Invoice <strong>%s</strong> from %s.</p>
<table style="border-collapse:collapse;width:100%%">
<tr><td style="padding:8px;border:1px solid #ddd;font-weight:bold">Amount</td><td style="padding:8px;border:1px solid #ddd">%s</td></tr>
<tr><td style="padding:8px;border:1px solid #ddd;font-weight:bold">Due Date</td><td style="padding:8px;border:1px solid #ddd">%s</td></tr>
</table>
</div>`, guestName, invoiceID, hotelName, amount, dueDate)
	return s.Send(guestEmail, subject, body)
}

func (s *EmailService) SendNotification(guestEmail, guestName, subjectLine, message string) error {
	subject := subjectLine
	body := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;max-width:600px;margin:0 auto">
<p>Dear %s,</p>
<p>%s</p>
</div>`, guestName, message)
	return s.Send(guestEmail, subject, body)
}

type SMSBody struct {
	From string `json:"From"`
	To   string `json:"To"`
	Body string `json:"Body"`
}

func validateEmail(email string) bool {
	return strings.Contains(email, "@") && strings.Contains(email, ".")
}
