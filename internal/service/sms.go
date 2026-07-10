package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hotelharmony/api/internal/config"
	"go.uber.org/zap"
)

type SMSService struct {
	cfg        *config.Config
	log        *zap.Logger
	httpClient *http.Client
}

func NewSMSService(cfg *config.Config, log *zap.Logger) *SMSService {
	return &SMSService{
		cfg:        cfg,
		log:        log,
		httpClient: &http.Client{},
	}
}

func (s *SMSService) Send(to, message string) error {
	if s.cfg.Twilio.AccountSID == "" || s.cfg.Twilio.AuthToken == "" {
		s.log.Warn("twilio not configured, skipping sms")
		return nil
	}

	apiURL := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", s.cfg.Twilio.AccountSID)

	data := url.Values{}
	data.Set("From", s.cfg.Twilio.PhoneNumber)
	data.Set("To", to)
	data.Set("Body", message)

	req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("sms request: %w", err)
	}
	req.SetBasicAuth(s.cfg.Twilio.AccountSID, s.cfg.Twilio.AuthToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sms send: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("twilio status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	}
	if json.Unmarshal(respBody, &result) == nil {
		s.log.Info("sms sent", zap.String("sid", result.SID), zap.String("status", result.Status))
	}

	return nil
}

func (s *SMSService) SendBookingConfirmation(phone, guestName, hotelName, roomNumber, checkIn, checkOut string) error {
	msg := fmt.Sprintf("Booking Confirmed at %s! Room %s | Check-in: %s | Check-out: %s. Thank you for choosing %s.",
		hotelName, roomNumber, checkIn, checkOut, hotelName)
	return s.Send(phone, msg)
}

func (s *SMSService) SendAlert(phone, alertType, message string) error {
	msg := fmt.Sprintf("[%s] %s", strings.ToUpper(alertType), message)
	return s.Send(phone, msg)
}
