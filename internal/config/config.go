package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	App         AppConfig
	HTTP        HTTPConfig
	Database    DatabaseConfig
	Redis       RedisConfig
	Auth        AuthConfig
	Stripe      StripeConfig
	Groq        GroqConfig
	Cerebras    CerebrasConfig
	Email       EmailConfig
	Twilio      TwilioConfig
	OTel        OTelConfig
	Provisioning ProvisioningConfig
}

type AppConfig struct {
	Name        string `mapstructure:"APP_NAME"`
	Env         string `mapstructure:"APP_ENV"`
	LogLevel    string `mapstructure:"LOG_LEVEL"`
	FrontendURL string `mapstructure:"FRONTEND_URL"`
}

type HTTPConfig struct {
	Host         string        `mapstructure:"HTTP_HOST"`
	Port         int           `mapstructure:"HTTP_PORT"`
	ReadTimeout  time.Duration `mapstructure:"HTTP_READ_TIMEOUT"`
	WriteTimeout time.Duration `mapstructure:"HTTP_WRITE_TIMEOUT"`
	IdleTimeout  time.Duration `mapstructure:"HTTP_IDLE_TIMEOUT"`
}

type DatabaseConfig struct {
	DSN             string        `mapstructure:"DATABASE_URL"`
	MaxOpenConns    int           `mapstructure:"DB_MAX_OPEN_CONNS"`
	MaxIdleConns    int           `mapstructure:"DB_MAX_IDLE_CONNS"`
	ConnMaxLifetime time.Duration `mapstructure:"DB_CONN_MAX_LIFETIME"`
	ConnMaxIdleTime time.Duration `mapstructure:"DB_CONN_MAX_IDLE_TIME"`
}

type RedisConfig struct {
	URL          string        `mapstructure:"REDIS_URL"`
	Password     string        `mapstructure:"REDIS_PASSWORD"`
	DB           int           `mapstructure:"REDIS_DB"`
	DialTimeout  time.Duration `mapstructure:"REDIS_DIAL_TIMEOUT"`
	ReadTimeout  time.Duration `mapstructure:"REDIS_READ_TIMEOUT"`
	WriteTimeout time.Duration `mapstructure:"REDIS_WRITE_TIMEOUT"`
	PoolSize     int           `mapstructure:"REDIS_POOL_SIZE"`
	MinIdleConns int           `mapstructure:"REDIS_MIN_IDLE_CONNS"`
}

type AuthConfig struct {
	AccessTokenSecret  string        `mapstructure:"JWT_ACCESS_SECRET"`
	RefreshTokenSecret string        `mapstructure:"JWT_REFRESH_SECRET"`
	AccessTokenTTL     time.Duration `mapstructure:"JWT_ACCESS_TTL"`
	RefreshTokenTTL    time.Duration `mapstructure:"JWT_REFRESH_TTL"`
	BcryptCost         int           `mapstructure:"BCRYPT_COST"`
}

type StripeConfig struct {
	SecretKey      string `mapstructure:"STRIPE_SECRET_KEY"`
	PublishableKey string `mapstructure:"STRIPE_PUBLISHABLE_KEY"`
	WebhookSecret  string `mapstructure:"STRIPE_WEBHOOK_SECRET"`
}

type GroqConfig struct {
	APIKey string `mapstructure:"GROQ_API_KEY"`
	Model  string `mapstructure:"GROQ_MODEL"`
}

type CerebrasConfig struct {
	APIKey string `mapstructure:"CEREBRAS_API_KEY"`
	Model  string `mapstructure:"CEREBRAS_MODEL"`
}

type EmailConfig struct {
	Provider string `mapstructure:"EMAIL_PROVIDER"`
	Host     string `mapstructure:"SMTP_HOST"`
	Port     int    `mapstructure:"SMTP_PORT"`
	Username string `mapstructure:"SMTP_USERNAME"`
	Password string `mapstructure:"SMTP_PASSWORD"`
	From     string `mapstructure:"EMAIL_FROM"`
}

type TwilioConfig struct {
	AccountSID  string `mapstructure:"TWILIO_ACCOUNT_SID"`
	AuthToken   string `mapstructure:"TWILIO_AUTH_TOKEN"`
	PhoneNumber string `mapstructure:"TWILIO_PHONE_NUMBER"`
}

type OTelConfig struct {
	ServiceName    string `mapstructure:"OTEL_SERVICE_NAME"`
	ExporterURL    string `mapstructure:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	MetricsEnabled bool   `mapstructure:"OTEL_METRICS_ENABLED"`
	TracingEnabled bool   `mapstructure:"OTEL_TRACING_ENABLED"`
}

// ProvisioningConfig holds credentials and defaults for the DNS + nginx/certbot
// automation that runs after a new client tenant is created.
// All fields are optional at runtime — when a credential is empty the relevant
// service skips its step gracefully and logs a warning.
type ProvisioningConfig struct {
	// GoDaddy DNS API — used to create {slug}.serenentra.com A records.
	GoDaddyAPIKey    string `mapstructure:"GODADDY_API_KEY"`
	GoDaddyAPISecret string `mapstructure:"GODADDY_API_SECRET"`
	// ProvisionerURL is the URL of the host-side hms-provisioner daemon that
	// writes nginx configs and runs certbot. Reachable via host.docker.internal.
	ProvisionerURL    string `mapstructure:"PROVISIONER_URL"`
	ProvisionerSecret string `mapstructure:"PROVISIONER_SECRET"`
	// VpsIP is the public IP of the Hetzner VPS — the A record target.
	VpsIP            string `mapstructure:"VPS_IP"`
	TenantBaseDomain string `mapstructure:"TENANT_BASE_DOMAIN"`
	TenantAPIURL     string `mapstructure:"TENANT_API_URL"`
	// Legacy Cloudflare / Vercel fields kept for zero-breakage — unused when
	// GoDaddy + provisioner are configured.
	CloudflareAPIToken string `mapstructure:"CLOUDFLARE_API_TOKEN"`
	CloudflareZoneID   string `mapstructure:"CLOUDFLARE_ZONE_ID"`
	VercelAPIToken     string `mapstructure:"VERCEL_API_TOKEN"`
	VercelTeamID       string `mapstructure:"VERCEL_TEAM_ID"`
	VercelGitHubOrg    string `mapstructure:"VERCEL_GITHUB_ORG"`
	VercelGitHubRepo   string `mapstructure:"VERCEL_GITHUB_REPO"`
}

func Load() (*Config, error) {
	v := viper.New()

	v.SetConfigName(".env")
	v.SetConfigType("env")
	v.AddConfigPath(".")
	v.AddConfigPath("..")
	_ = v.ReadInConfig()

	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	v.SetDefault("APP_NAME", "HotelHarmony")
	v.SetDefault("APP_ENV", "development")
	v.SetDefault("LOG_LEVEL", "info")
	v.SetDefault("FRONTEND_URL", "http://localhost:8080")
	v.SetDefault("HTTP_HOST", "0.0.0.0")
	v.SetDefault("HTTP_PORT", 8787)
	v.SetDefault("HTTP_READ_TIMEOUT", "30s")
	v.SetDefault("HTTP_WRITE_TIMEOUT", "30s")
	v.SetDefault("HTTP_IDLE_TIMEOUT", "120s")
	v.SetDefault("DB_MAX_OPEN_CONNS", 50)
	v.SetDefault("DB_MAX_IDLE_CONNS", 10)
	v.SetDefault("DB_CONN_MAX_LIFETIME", "1h")
	v.SetDefault("DB_CONN_MAX_IDLE_TIME", "15m")
	v.SetDefault("REDIS_DB", 0)
	v.SetDefault("REDIS_DIAL_TIMEOUT", "5s")
	v.SetDefault("REDIS_READ_TIMEOUT", "3s")
	v.SetDefault("REDIS_WRITE_TIMEOUT", "3s")
	v.SetDefault("REDIS_POOL_SIZE", 20)
	v.SetDefault("REDIS_MIN_IDLE_CONNS", 5)
	v.SetDefault("JWT_ACCESS_TTL", "15m")
	v.SetDefault("JWT_REFRESH_TTL", "168h")
	v.SetDefault("BCRYPT_COST", 12)
	v.SetDefault("GROQ_MODEL", "llama-3.3-70b-versatile")
	v.SetDefault("CEREBRAS_MODEL", "zai-glm-4.7")
	v.SetDefault("EMAIL_PROVIDER", "smtp")
	v.SetDefault("SMTP_HOST", "smtp.gmail.com")
	v.SetDefault("SMTP_PORT", 587)
	v.SetDefault("OTEL_METRICS_ENABLED", true)
	v.SetDefault("OTEL_TRACING_ENABLED", false)

	// Provisioning defaults
	v.SetDefault("TENANT_BASE_DOMAIN", "serenentra.com")
	v.SetDefault("TENANT_API_URL", "https://hmsadmin.serenentra.com")
	v.SetDefault("VPS_IP", "167.233.158.179")
	v.SetDefault("PROVISIONER_URL", "http://host.docker.internal:9001")
	v.SetDefault("VERCEL_GITHUB_ORG", "jasmeet15069")
	v.SetDefault("VERCEL_GITHUB_REPO", "HmsAdminStaffPortal")

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal failed: %w", err)
	}

	cfg.App.Name = v.GetString("APP_NAME")
	cfg.App.Env = v.GetString("APP_ENV")
	cfg.App.LogLevel = v.GetString("LOG_LEVEL")
	cfg.App.FrontendURL = v.GetString("FRONTEND_URL")

	cfg.HTTP.Host = v.GetString("HTTP_HOST")
	cfg.HTTP.Port = v.GetInt("HTTP_PORT")
	cfg.HTTP.ReadTimeout = v.GetDuration("HTTP_READ_TIMEOUT")
	cfg.HTTP.WriteTimeout = v.GetDuration("HTTP_WRITE_TIMEOUT")
	cfg.HTTP.IdleTimeout = v.GetDuration("HTTP_IDLE_TIMEOUT")

	cfg.Database.DSN = v.GetString("DATABASE_URL")
	cfg.Database.MaxOpenConns = v.GetInt("DB_MAX_OPEN_CONNS")
	cfg.Database.MaxIdleConns = v.GetInt("DB_MAX_IDLE_CONNS")
	cfg.Database.ConnMaxLifetime = v.GetDuration("DB_CONN_MAX_LIFETIME")
	cfg.Database.ConnMaxIdleTime = v.GetDuration("DB_CONN_MAX_IDLE_TIME")

	cfg.Redis.URL = v.GetString("REDIS_URL")
	cfg.Redis.Password = v.GetString("REDIS_PASSWORD")
	cfg.Redis.DB = v.GetInt("REDIS_DB")
	cfg.Redis.DialTimeout = v.GetDuration("REDIS_DIAL_TIMEOUT")
	cfg.Redis.ReadTimeout = v.GetDuration("REDIS_READ_TIMEOUT")
	cfg.Redis.WriteTimeout = v.GetDuration("REDIS_WRITE_TIMEOUT")
	cfg.Redis.PoolSize = v.GetInt("REDIS_POOL_SIZE")
	cfg.Redis.MinIdleConns = v.GetInt("REDIS_MIN_IDLE_CONNS")

	cfg.Auth.AccessTokenSecret = v.GetString("JWT_ACCESS_SECRET")
	cfg.Auth.RefreshTokenSecret = v.GetString("JWT_REFRESH_SECRET")
	cfg.Auth.AccessTokenTTL = v.GetDuration("JWT_ACCESS_TTL")
	cfg.Auth.RefreshTokenTTL = v.GetDuration("JWT_REFRESH_TTL")
	cfg.Auth.BcryptCost = v.GetInt("BCRYPT_COST")

	cfg.Stripe.SecretKey = v.GetString("STRIPE_SECRET_KEY")
	cfg.Stripe.PublishableKey = v.GetString("STRIPE_PUBLISHABLE_KEY")
	cfg.Stripe.WebhookSecret = v.GetString("STRIPE_WEBHOOK_SECRET")

	cfg.Groq.APIKey = v.GetString("GROQ_API_KEY")
	cfg.Groq.Model = v.GetString("GROQ_MODEL")

	cfg.Cerebras.APIKey = v.GetString("CEREBRAS_API_KEY")
	cfg.Cerebras.Model = v.GetString("CEREBRAS_MODEL")

	cfg.Email.Provider = v.GetString("EMAIL_PROVIDER")
	cfg.Email.Host = v.GetString("SMTP_HOST")
	cfg.Email.Port = v.GetInt("SMTP_PORT")
	cfg.Email.Username = v.GetString("SMTP_USERNAME")
	cfg.Email.Password = v.GetString("SMTP_PASSWORD")
	cfg.Email.From = v.GetString("EMAIL_FROM")

	cfg.Twilio.AccountSID = v.GetString("TWILIO_ACCOUNT_SID")
	cfg.Twilio.AuthToken = v.GetString("TWILIO_AUTH_TOKEN")
	cfg.Twilio.PhoneNumber = v.GetString("TWILIO_PHONE_NUMBER")

	cfg.OTel.ServiceName = v.GetString("OTEL_SERVICE_NAME")
	cfg.OTel.ExporterURL = v.GetString("OTEL_EXPORTER_OTLP_ENDPOINT")
	cfg.OTel.MetricsEnabled = v.GetBool("OTEL_METRICS_ENABLED")
	cfg.OTel.TracingEnabled = v.GetBool("OTEL_TRACING_ENABLED")

	cfg.Provisioning.GoDaddyAPIKey = v.GetString("GODADDY_API_KEY")
	cfg.Provisioning.GoDaddyAPISecret = v.GetString("GODADDY_API_SECRET")
	cfg.Provisioning.ProvisionerURL = v.GetString("PROVISIONER_URL")
	cfg.Provisioning.ProvisionerSecret = v.GetString("PROVISIONER_SECRET")
	cfg.Provisioning.VpsIP = v.GetString("VPS_IP")
	cfg.Provisioning.TenantBaseDomain = v.GetString("TENANT_BASE_DOMAIN")
	cfg.Provisioning.TenantAPIURL = v.GetString("TENANT_API_URL")
	cfg.Provisioning.CloudflareAPIToken = v.GetString("CLOUDFLARE_API_TOKEN")
	cfg.Provisioning.CloudflareZoneID = v.GetString("CLOUDFLARE_ZONE_ID")
	cfg.Provisioning.VercelAPIToken = v.GetString("VERCEL_API_TOKEN")
	cfg.Provisioning.VercelTeamID = v.GetString("VERCEL_TEAM_ID")
	cfg.Provisioning.VercelGitHubOrg = v.GetString("VERCEL_GITHUB_ORG")
	cfg.Provisioning.VercelGitHubRepo = v.GetString("VERCEL_GITHUB_REPO")

	if cfg.Database.DSN == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}
	if cfg.Auth.AccessTokenSecret == "" {
		return nil, fmt.Errorf("config: JWT_ACCESS_SECRET is required")
	}
	if cfg.Auth.RefreshTokenSecret == "" {
		return nil, fmt.Errorf("config: JWT_REFRESH_SECRET is required")
	}
	return cfg, nil
}

func (c *Config) IsProd() bool {
	return strings.EqualFold(c.App.Env, "production")
}

func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.HTTP.Host, c.HTTP.Port)
}
