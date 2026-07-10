ALTER TABLE hotels ALTER COLUMN plan_tier SET DEFAULT 'basic';

UPDATE hotels
SET plan_tier = CASE
  WHEN plan_tier IN ('starter', 'basic') THEN 'basic'
  WHEN plan_tier IN ('growth', 'pro') THEN 'pro'
  WHEN plan_tier IN ('enterprise', 'premium') THEN 'premium'
  ELSE 'basic'
END
WHERE plan_tier IS DISTINCT FROM CASE
  WHEN plan_tier IN ('starter', 'basic') THEN 'basic'
  WHEN plan_tier IN ('growth', 'pro') THEN 'pro'
  WHEN plan_tier IN ('enterprise', 'premium') THEN 'premium'
  ELSE 'basic'
END;

UPDATE hotels
SET settings = CASE plan_tier
  WHEN 'pro' THEN jsonb_build_object(
    'max_rooms', 200,
    'max_users', 50,
    'max_properties', 3,
    'allowed_roles', jsonb_build_array('hotel_admin','property_manager','receptionist','housekeeping','maintenance','food_manager','kitchen_manager','waiter','guest'),
    'ai_addon', true,
    'ai_text_concierge', true,
    'ai_voice_agent', false,
    'ai_voice_booking', false,
    'database_strategy', 'tenant_isolated',
    'database_name', regexp_replace(lower(slug), '[^a-z0-9]+', '_', 'g') || '_hotelops',
    'billing_plan_locked', false
  )
  WHEN 'premium' THEN jsonb_build_object(
    'max_rooms', NULL,
    'max_users', NULL,
    'max_properties', NULL,
    'allowed_roles', jsonb_build_array('hotel_admin','property_manager','receptionist','housekeeping','maintenance','food_manager','kitchen_manager','waiter','guest'),
    'ai_addon', true,
    'ai_text_concierge', true,
    'ai_voice_agent', true,
    'ai_voice_booking', true,
    'database_strategy', 'tenant_dedicated_db_ready',
    'database_name', regexp_replace(lower(slug), '[^a-z0-9]+', '_', 'g') || '_hotelops',
    'billing_plan_locked', false
  )
  ELSE jsonb_build_object(
    'max_rooms', 50,
    'max_users', 10,
    'max_properties', 1,
    'allowed_roles', jsonb_build_array('hotel_admin','receptionist','housekeeping','maintenance','guest'),
    'ai_addon', false,
    'ai_text_concierge', false,
    'ai_voice_agent', false,
    'ai_voice_booking', false,
    'database_strategy', 'tenant_isolated',
    'database_name', regexp_replace(lower(slug), '[^a-z0-9]+', '_', 'g') || '_hotelops',
    'billing_plan_locked', false
  )
END
WHERE NOT (settings ? 'max_users')
   OR NOT (settings ? 'allowed_roles')
   OR NOT (settings ? 'ai_voice_agent')
   OR NOT (settings ? 'database_name');
