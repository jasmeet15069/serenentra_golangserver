package pagination

import "strconv"

type Page struct {
	Limit  int
	Offset int
}

func FromQuery(limitRaw, offsetRaw string) Page {
	limit, _ := strconv.Atoi(limitRaw)
	offset, _ := strconv.Atoi(offsetRaw)
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return Page{Limit: limit, Offset: offset}
}
