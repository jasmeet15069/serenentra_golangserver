ALTER TABLE payment_configs ADD COLUMN IF NOT EXISTS stripe_publishable_key TEXT;
ALTER TABLE payment_configs ADD COLUMN IF NOT EXISTS stripe_secret_key_encrypted TEXT;
ALTER TABLE payment_configs ADD COLUMN IF NOT EXISTS stripe_webhook_secret_encrypted TEXT;
ALTER TABLE payment_configs ADD COLUMN IF NOT EXISTS default_currency VARCHAR(10) DEFAULT 'USD';
ALTER TABLE payment_configs ADD COLUMN IF NOT EXISTS allowed_currencies JSONB NOT NULL DEFAULT '["USD"]';
ALTER TABLE payment_configs ADD COLUMN IF NOT EXISTS gateway_mode VARCHAR(20) NOT NULL DEFAULT 'test';

UPDATE payment_configs pc
SET default_currency = COALESCE(h.currency, 'USD')
FROM hotels h
WHERE pc.hotel_id = h.id
  AND (pc.default_currency IS NULL OR pc.default_currency = '');

INSERT INTO payment_configs (hotel_id, default_currency)
SELECT id, COALESCE(currency, 'USD')
FROM hotels
ON CONFLICT (hotel_id) DO NOTHING;
