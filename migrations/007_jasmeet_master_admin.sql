-- Make Jasmeet the only HotelOps platform master admin.
INSERT INTO users (id, hotel_id, email, password_hash, platform_admin, created_at, updated_at)
VALUES (
  '00000000-0000-0000-0000-00000000f001',
  '00000000-0000-0000-0000-000000000001',
  'jasmeet.15069@gmail.com',
  '$2a$10$/CG7sSFsLPZdb07W1vL6me5Cac0052O4tUjZV0SQaPnPVPn.qIyWS',
  TRUE,
  NOW(),
  NOW()
)
ON CONFLICT (email) DO UPDATE
SET platform_admin = TRUE,
    updated_at = NOW();

UPDATE users
SET platform_admin = FALSE,
    updated_at = NOW()
WHERE email <> 'jasmeet.15069@gmail.com'
  AND platform_admin = TRUE;

INSERT INTO profiles (id, hotel_id, user_id, full_name, created_at, updated_at)
SELECT
  '00000000-0000-0000-0000-00000000f002',
  COALESCE(u.hotel_id, '00000000-0000-0000-0000-000000000001'),
  u.id,
  'Jasmeet Sethi',
  NOW(),
  NOW()
FROM users u
WHERE u.email = 'jasmeet.15069@gmail.com'
ON CONFLICT (user_id) DO UPDATE
SET full_name = 'Jasmeet Sethi',
    updated_at = NOW();

DELETE FROM user_roles
WHERE role = 'platform_admin'
  AND user_id NOT IN (SELECT id FROM users WHERE email = 'jasmeet.15069@gmail.com');

INSERT INTO user_roles (id, hotel_id, user_id, role, created_at)
SELECT
  '00000000-0000-0000-0000-00000000f003',
  COALESCE(u.hotel_id, '00000000-0000-0000-0000-000000000001'),
  u.id,
  'platform_admin',
  NOW()
FROM users u
WHERE u.email = 'jasmeet.15069@gmail.com'
ON CONFLICT (user_id, role) DO UPDATE
SET hotel_id = EXCLUDED.hotel_id;

