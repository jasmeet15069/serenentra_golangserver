package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/domain"
)

type ChatRequest struct {
	Messages []domain.ChatMessage `json:"messages"`
}

type MenuSuggestionsRequest struct {
	MenuItems   []map[string]interface{} `json:"menuItems"`
	Preferences map[string]interface{}   `json:"preferences"`
	PastOrders  []interface{}            `json:"pastOrders"`
	TimeOfDay   string                   `json:"timeOfDay"`
}

type ComplaintAnalysisRequest struct {
	Description  string                 `json:"description"`
	Category     string                 `json:"category"`
	GuestHistory map[string]interface{} `json:"guestHistory"`
}

type SmartCheckinRequest struct {
	GuestName string `json:"guest_name"`
	Phone     string `json:"phone"`
}

// circuitBreaker is a minimal open/half-open/closed implementation.
type circuitBreaker struct {
	failures      int64
	lastFailure   int64
	threshold     int64
	recoveryNanos int64
}

func newCircuitBreaker(threshold int, recovery time.Duration) *circuitBreaker {
	return &circuitBreaker{
		threshold:     int64(threshold),
		recoveryNanos: int64(recovery),
	}
}

func (cb *circuitBreaker) isOpen() bool {
	f := atomic.LoadInt64(&cb.failures)
	if f < cb.threshold {
		return false
	}
	last := atomic.LoadInt64(&cb.lastFailure)
	return time.Now().UnixNano()-last < cb.recoveryNanos
}

func (cb *circuitBreaker) recordFailure() {
	atomic.AddInt64(&cb.failures, 1)
	atomic.StoreInt64(&cb.lastFailure, time.Now().UnixNano())
}

func (cb *circuitBreaker) recordSuccess() {
	atomic.StoreInt64(&cb.failures, 0)
}

// AIService wraps all Groq LLM calls with retries, circuit breaking,
// response caching, and deterministic fallbacks.
type AIService interface {
	Chat(ctx context.Context, rooms []domain.Room, activeOrders, pendingComplaints int, msgs []domain.ChatMessage) (string, []string, error)
	MenuSuggestions(ctx context.Context, req MenuSuggestionsRequest) (map[string]interface{}, error)
	ComplaintAnalysis(ctx context.Context, req ComplaintAnalysisRequest) (map[string]interface{}, error)
	InventoryAlerts(ctx context.Context, items []domain.InventoryItem) (map[string]interface{}, error)
}

type aiService struct {
	httpClient *http.Client
	cache      cache.Cache
	cfg        *config.Config
	log        *zap.Logger
	cb         *circuitBreaker
}

func NewAIService(c cache.Cache, cfg *config.Config, log *zap.Logger) AIService {
	return &aiService{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		cache:      c,
		cfg:        cfg,
		log:        log,
		cb:         newCircuitBreaker(5, 60*time.Second),
	}
}

func (s *aiService) Chat(ctx context.Context, rooms []domain.Room, activeOrders, pendingComplaints int, msgs []domain.ChatMessage) (string, []string, error) {
	roomsJSON, _ := json.Marshal(rooms)
	system := fmt.Sprintf(
		"You are the AI concierge for Hotel Harmony. Answer in the user's language. "+
			"Use only the hotel data included here; do not invent room numbers, prices, orders, or complaints. "+
			"Rooms: %s. Active orders: %d. Pending complaints: %d.",
		string(roomsJSON), activeOrders, pendingComplaints,
	)

	messages := make([]groqMessage, 0, len(msgs)+1)
	messages = append(messages, groqMessage{Role: "system", Content: system})
	for _, m := range msgs {
		messages = append(messages, groqMessage{Role: m.Role, Content: m.Content})
	}

	reply, _, err := s.callAI(ctx, messages, "text")
	if err != nil {
		var available []string
		for _, r := range rooms {
			if r.Status == domain.RoomStatusAvailable {
				available = append(available, fmt.Sprintf("Room %s (%s, $%.0f/night)", r.RoomNumber, r.RoomType, r.PricePerNight))
			}
		}
		if len(available) > 0 {
			reply = fmt.Sprintf("I can help with that. Available rooms right now: %s.", strings.Join(available[:min(5, len(available))], ", "))
		} else {
			reply = "I can help with hotel services, but no available rooms are listed right now."
		}
		return reply, []string{"local_fallback"}, nil
	}
	return reply, []string{"cerebras_chat_completion"}, nil
}

func (s *aiService) MenuSuggestions(ctx context.Context, req MenuSuggestionsRequest) (map[string]interface{}, error) {
	fallback := buildMenuFallback(req.MenuItems)

	cacheKey := fmt.Sprintf("ai:menu:%s:%s", req.TimeOfDay, hashJSON(req.Preferences))
	if cached, err := s.cache.Get(ctx, cacheKey); err == nil {
		var result map[string]interface{}
		if json.Unmarshal([]byte(cached), &result) == nil {
			return result, nil
		}
	}

	menuJSON, _ := json.Marshal(req.MenuItems)
	prefJSON, _ := json.Marshal(req.Preferences)
	pastJSON, _ := json.Marshal(req.PastOrders)

	prompt := fmt.Sprintf(
		"Return JSON only with keys recommendations and personalNote. "+
			"recommendations must contain itemId, itemName, reason, confidence(high|medium|low). "+
			"Menu items: %s. Preferences: %s. Past orders: %s. Time of day: %s.",
		string(menuJSON), string(prefJSON), string(pastJSON), req.TimeOfDay,
	)

	raw, _, err := s.callAI(ctx, []groqMessage{{Role: "user", Content: prompt}}, "json")
	if err != nil {
		return fallback, nil
	}

	var result map[string]interface{}
	if json.Unmarshal([]byte(raw), &result) != nil {
		return fallback, nil
	}

	if b, err := json.Marshal(result); err == nil {
		_ = s.cache.Set(ctx, cacheKey, string(b), 10*time.Minute)
	}
	return result, nil
}

func (s *aiService) ComplaintAnalysis(ctx context.Context, req ComplaintAnalysisRequest) (map[string]interface{}, error) {
	fallback := complaintFallback(req.Description, req.Category)

	histJSON, _ := json.Marshal(req.GuestHistory)
	prompt := fmt.Sprintf(
		"Analyze this hotel guest complaint. Return JSON only matching this shape: "+
			`{"analysis":{"sentiment":"","urgency":"","emotionalState":""},"categorization":{"primaryCategory":"","subcategory":"","affectedService":""},`+
			`"suggestedPriority":"","priorityReason":"","resolutionSuggestions":[{"action":"","timeframe":"","owner":""}],`+
			`"compensationSuggestion":"","escalationNeeded":false,"escalationReason":""}. `+
			"Use urgency and suggestedPriority values low, medium, high, or critical. "+
			"Complaint: %s. Guest history: %s.",
		req.Description, string(histJSON),
	)

	raw, _, err := s.callAI(ctx, []groqMessage{{Role: "user", Content: prompt}}, "json")
	if err != nil {
		return fallback, nil
	}

	var result map[string]interface{}
	if json.Unmarshal([]byte(raw), &result) != nil {
		return fallback, nil
	}
	return result, nil
}

func (s *aiService) InventoryAlerts(ctx context.Context, items []domain.InventoryItem) (map[string]interface{}, error) {
	alerts := make([]domain.InventoryAlert, 0, len(items))
	for _, item := range items {
		critical := item.CurrentStock <= item.MinStock
		severity := "warning"
		if critical {
			severity = "critical"
		}
		rec := fmt.Sprintf("Restock %s soon; current stock is at or below the minimum threshold.", item.Name)
		if !critical {
			rec = fmt.Sprintf("Check expiry date for %s and prioritize usage.", item.Name)
		}
		alerts = append(alerts, domain.InventoryAlert{
			ItemID:           item.ID,
			Name:             item.Name,
			CurrentStock:     item.CurrentStock,
			MinStock:         item.MinStock,
			Severity:         severity,
			AIRecommendation: rec,
			IsPerishable:     item.IsPerishable,
		})
	}
	summary := fmt.Sprintf("%d inventory item(s) need attention.", len(alerts))

	if len(alerts) == 0 {
		return map[string]interface{}{"alerts": alerts, "summary": summary}, nil
	}

	alertsJSON, _ := json.Marshal(alerts)
	prompt := fmt.Sprintf("Return JSON with alerts and summary. Improve these hotel inventory recommendations: %s", string(alertsJSON))
	raw, _, err := s.callAI(ctx, []groqMessage{{Role: "user", Content: prompt}}, "json")
	if err != nil {
		return map[string]interface{}{"alerts": alerts, "summary": summary}, nil
	}

	var result map[string]interface{}
	if json.Unmarshal([]byte(raw), &result) != nil {
		return map[string]interface{}{"alerts": alerts, "summary": summary}, nil
	}
	return result, nil
}

type groqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type cerebrasMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type groqRequest struct {
	Model          string            `json:"model"`
	Messages       []groqMessage     `json:"messages"`
	Temperature    float64           `json:"temperature"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
}

type cerebrasRequest struct {
	Model          string                `json:"model"`
	Messages       []cerebrasMessage     `json:"messages"`
	Temperature    float64               `json:"temperature"`
	MaxTokens      int                   `json:"max_completion_tokens,omitempty"`
	ResponseFormat map[string]string     `json:"response_format,omitempty"`
}

func (s *aiService) callGroq(ctx context.Context, messages []groqMessage, format string) (string, error) {
	if s.cfg.Cerebras.APIKey == "" && s.cfg.Groq.APIKey == "" {
		return "", fmt.Errorf("GROQ_API_KEY not configured")
	}
	if s.cb.isOpen() {
		return "", fmt.Errorf("groq circuit breaker open")
	}

	payload := groqRequest{
		Model:       s.cfg.Groq.Model,
		Messages:    messages,
		Temperature: 0.2,
	}
	if format == "json" {
		payload.ResponseFormat = map[string]string{"type": "json_object"}
	}

	body, _ := json.Marshal(payload)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 500 * time.Millisecond):
			}
		}

		reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost,
			"https://api.groq.com/openai/v1/chat/completions",
			bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.cfg.Groq.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "HotelHarmony/2.0")

		resp, err := s.httpClient.Do(req)
		cancel()
		if err != nil {
			lastErr = err
			s.cb.recordFailure()
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("groq status %d: %s", resp.StatusCode, string(respBody))
			s.cb.recordFailure()
			continue
		}

		var result struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil || len(result.Choices) == 0 {
			lastErr = fmt.Errorf("groq: invalid response shape")
			continue
		}

		s.cb.recordSuccess()
		return result.Choices[0].Message.Content, nil
	}

	return "", lastErr
}


func (s *aiService) callAI(ctx context.Context, messages []groqMessage, format string) (string, string, error) {
	if s.cfg.Cerebras.APIKey != "" {
		cm := make([]cerebrasMessage, len(messages))
		for i, m := range messages {
			cm[i] = cerebrasMessage{Role: m.Role, Content: m.Content}
		}
		result, err := s.callCerebras(ctx, cm, format)
		if err == nil {
			return result, "cerebras_chat_completion", nil
		}
		s.log.Warn("cerebras failed, falling back to groq", zap.Error(err))
	}
	result, err := s.callGroq(ctx, messages, format)
	if err != nil {
		return "", "", err
	}
	return result, "groq_chat_completion", nil
}

func (s *aiService) callCerebras(ctx context.Context, messages []cerebrasMessage, format string) (string, error) {
	if s.cfg.Cerebras.APIKey == "" {
		return "", fmt.Errorf("CEREBRAS_API_KEY not configured")
	}
	if s.cb.isOpen() {
		return "", fmt.Errorf("cerebras circuit breaker open")
	}

	payload := cerebrasRequest{
		Model:       s.cfg.Cerebras.Model,
		Messages:    messages,
		Temperature: 0.2,
		MaxTokens:   65000,
	}
	if format == "json" {
		payload.ResponseFormat = map[string]string{"type": "json_object"}
	}

	body, _ := json.Marshal(payload)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 500 * time.Millisecond):
			}
		}

		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost,
			"https://api.cerebras.ai/v1/chat/completions",
			bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.cfg.Cerebras.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "HotelHarmony/2.0")

		resp, err := s.httpClient.Do(req)
		cancel()
		if err != nil {
			lastErr = err
			s.cb.recordFailure()
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("cerebras status %d: %s", resp.StatusCode, string(respBody))
			s.cb.recordFailure()
			continue
		}

		var result struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil || len(result.Choices) == 0 {
			lastErr = fmt.Errorf("cerebras: invalid response shape")
			continue
		}

		s.cb.recordSuccess()
		return result.Choices[0].Message.Content, nil
	}

	return "", lastErr
}
func buildMenuFallback(items []map[string]interface{}) map[string]interface{} {
	limit := 3
	if len(items) < limit {
		limit = len(items)
	}
	recs := make([]map[string]interface{}, limit)
	for i := 0; i < limit; i++ {
		recs[i] = map[string]interface{}{
			"itemId":     items[i]["id"],
			"itemName":   items[i]["name"],
			"reason":     "Popular available option that matches the current menu.",
			"confidence": "medium",
		}
	}
	return map[string]interface{}{
		"recommendations": recs,
		"personalNote":    "Here are a few good options from the available menu.",
	}
}

var urgentWords = []string{"can't sleep", "couldn't sleep", "unsafe", "fire", "flood", "leak", "broken", "angry", "terrible"}
var highWords = []string{"noise", "ac", "air conditioner", "dirty", "not working", "cold", "hot", "delay"}

func complaintFallback(description, category string) map[string]interface{} {
	text := strings.ToLower(description)
	isUrgent := containsAny(text, urgentWords)
	isHigh := isUrgent || containsAny(text, highWords)

	primary := category
	if primary == "" {
		primary = "Other"
	}
	switch {
	case containsAny(text, []string{"ac", "air conditioner", "broken", "leak", "maintenance"}):
		primary = "Maintenance"
	case containsAny(text, []string{"dirty", "clean", "towel", "housekeeping"}):
		primary = "Housekeeping"
	case containsAny(text, []string{"food", "order", "meal", "breakfast", "dinner"}):
		primary = "Food"
	case strings.Contains(text, "noise"):
		primary = "Noise"
	}

	priority := "medium"
	sentiment := "neutral"
	emotional := "concerned"
	if isUrgent {
		priority = "critical"
		sentiment = "very_negative"
		emotional = "angry"
	} else if isHigh {
		priority = "high"
		sentiment = "negative"
		emotional = "frustrated"
	}

	owner := "front_desk"
	switch primary {
	case "Maintenance":
		owner = "maintenance"
	case "Housekeeping":
		owner = "housekeeping"
	case "Food":
		owner = "food_service"
	}

	timeframe := "within_hour"
	if priority == "critical" {
		timeframe = "immediate"
	}

	var comp interface{}
	if isHigh {
		comp = "Offer a room change or service recovery voucher if resolution exceeds 2 hours."
	}

	return map[string]interface{}{
		"analysis": map[string]interface{}{
			"sentiment":      sentiment,
			"urgency":        priority,
			"emotionalState": emotional,
		},
		"categorization": map[string]interface{}{
			"primaryCategory": primary,
			"subcategory":     "Guest-reported issue",
			"affectedService": strings.ToLower(strings.ReplaceAll(primary, " ", "_")),
		},
		"suggestedPriority": priority,
		"priorityReason":    "Priority assigned from local keyword triage because Groq is not configured.",
		"resolutionSuggestions": []map[string]interface{}{{
			"action":    fmt.Sprintf("Assign the %s team and update the guest with an ETA.", strings.ToLower(primary)),
			"timeframe": timeframe,
			"owner":     owner,
		}},
		"compensationSuggestion": comp,
		"escalationNeeded":       priority == "critical" || priority == "high",
		"escalationReason": func() interface{} {
			if priority == "critical" || priority == "high" {
				return "Guest comfort or safety may be affected."
			}
			return nil
		}(),
	}
}

func containsAny(text string, words []string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

func hashJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	h := uint32(2166136261)
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return fmt.Sprintf("%x", h)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
