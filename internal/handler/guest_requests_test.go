package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guestRequestTypes and housekeeping_task_type_check are one rule written in two
// places — Go and SQL. If they drift, the API accepts a request the database
// then rejects, and the guest is told their wake-up call was booked when the
// insert failed.
//
// Reading the migration is deliberate. Restating the list here would just be a
// third copy to keep in step.
func TestGuestRequestTypesAreAcceptedByTheCheckConstraint(t *testing.T) {
	path := filepath.Join("..", "..", "migrations", "028_guest_services.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	sql := string(raw)

	start := strings.Index(sql, "housekeeping_task_type_check\n  CHECK (task_type IN (")
	if start < 0 {
		t.Fatal("could not find the task_type CHECK constraint — has the migration been renamed or reshaped?")
	}
	end := strings.Index(sql[start:], "))")
	if end < 0 {
		t.Fatal("could not find the end of the task_type CHECK constraint")
	}
	clause := sql[start : start+end]

	for typ := range guestRequestTypes {
		if !strings.Contains(clause, "'"+typ+"'") {
			t.Errorf("guestRequestTypes accepts %q but housekeeping_task_type_check does not — "+
				"the API would accept the request and the insert would fail", typ)
		}
	}
}

// A guest can ask for a wake-up call. A guest cannot ask for a room to be
// marked inspected — those task types are raised by the system for itself, and
// letting a request set one would let the guest app sign off its own room.
func TestGuestRequestTypesExcludeOperationalTasks(t *testing.T) {
	for _, operational := range []string{
		"checkout_clean", "daily_clean", "deep_clean", "inspection", "maintenance",
	} {
		if guestRequestTypes[operational] {
			t.Errorf("%q is an operational task type and must not be requestable by a guest", operational)
		}
	}
}

// The error message shown to a caller is built from this list, so it has to
// stay in step with the map rather than drift into a stale hardcoded string.
func TestGuestRequestTypeListIsSortedAndComplete(t *testing.T) {
	list := guestRequestTypeList()
	for typ := range guestRequestTypes {
		if !strings.Contains(list, typ) {
			t.Errorf("guestRequestTypeList() omits %q", typ)
		}
	}
	parts := strings.Split(list, ", ")
	for i := 1; i < len(parts); i++ {
		if parts[i-1] > parts[i] {
			t.Errorf("guestRequestTypeList() is not sorted: %q before %q — map iteration order "+
				"is random in Go, so an unsorted list would make the error message differ run to run",
				parts[i-1], parts[i])
		}
	}
}
