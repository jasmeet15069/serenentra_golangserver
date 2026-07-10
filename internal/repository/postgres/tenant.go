package postgres

import "github.com/google/uuid"

// DemoHotelID keeps the existing single-hotel install usable while the SaaS
// onboarding flow is rolled in. New enterprise endpoints should pass hotel_id
// explicitly instead of relying on this fallback.
var DemoHotelID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
