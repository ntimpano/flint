package app

import (
	"strings"
	"testing"
)

// doctorFakeStore extends fakeStore with the DoctorStore capability.
type doctorFakeStore struct {
	fakeStore

	calls   int
	report  DoctorReport
	failErr error
}

func (f *doctorFakeStore) Doctor() (DoctorReport, error) {
	f.calls++
	if f.failErr != nil {
		return DoctorReport{}, f.failErr
	}
	return f.report, nil
}

func TestService_Doctor_ForwardsReport(t *testing.T) {
	fake := &doctorFakeStore{
		report: DoctorReport{
			SchemaVersion:    3,
			FTSHealthy:       true,
			IntegrityOK:      true,
			MemoryItemsCount: 5,
			SessionsCount:    1,
			Summary:          "schema_version=3 fts=healthy integrity=ok memory_items=5 sessions=1",
		},
	}
	svc := NewService(fake, nil)
	report, err := svc.Doctor()
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 store call, got %d", fake.calls)
	}
	if report.SchemaVersion != 3 || report.MemoryItemsCount != 5 {
		t.Fatalf("report not forwarded verbatim: %+v", report)
	}
}

func TestService_Doctor_CapabilityError(t *testing.T) {
	svc := NewService(&fakeStore{}, nil)
	_, err := svc.Doctor()
	if err == nil {
		t.Fatalf("expected capability error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "doctor") {
		t.Fatalf("expected doctor-capability error, got %q", err)
	}
}
