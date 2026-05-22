package app

import (
	"testing"
)

// TestDoctorReport_AutopilotSessionCloseRateField pins the spec
// requirement: the DoctorReport surface MUST include
// `autopilot.session_close_rate ∈ [0,1]` and `threshold=0.9`.
//
// We assert the public type contract (fields exist + threshold default)
// and that the report can be carried verbatim through Service.Doctor.
func TestDoctorReport_AutopilotSessionCloseRateField(t *testing.T) {
	r := DoctorReport{
		AutopilotSessionCloseRate: 0.75,
		AutopilotThreshold:        AutopilotSessionCloseThreshold,
	}
	if r.AutopilotSessionCloseRate != 0.75 {
		t.Fatalf("expected close_rate=0.75; got %v", r.AutopilotSessionCloseRate)
	}
	if r.AutopilotThreshold != 0.9 {
		t.Fatalf("expected threshold=0.9; got %v", r.AutopilotThreshold)
	}
}

// TestService_Doctor_ForwardsAutopilotFields — triangulation: the
// doctor surface must carry the autopilot rate end-to-end (store →
// service) without dropping or rewriting it.
func TestService_Doctor_ForwardsAutopilotFields(t *testing.T) {
	fake := &doctorFakeStore{
		report: DoctorReport{
			SchemaVersion:             4,
			IntegrityOK:               true,
			FTSHealthy:                true,
			AutopilotSessionCloseRate: 0.42,
			AutopilotThreshold:        AutopilotSessionCloseThreshold,
		},
	}
	svc := NewService(fake, nil)

	got, err := svc.Doctor()
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if got.AutopilotSessionCloseRate != 0.42 {
		t.Fatalf("AutopilotSessionCloseRate not forwarded: got %v", got.AutopilotSessionCloseRate)
	}
	if got.AutopilotThreshold != 0.9 {
		t.Fatalf("AutopilotThreshold not forwarded: got %v", got.AutopilotThreshold)
	}
}

// TestComputeAutopilotSessionCloseRate_HappyAndEdge — the rate is the
// fraction of distinct sessions that have BOTH a summary row and an
// end row. Empty input → 0. All complete → 1.0.
func TestComputeAutopilotSessionCloseRate_HappyAndEdge(t *testing.T) {
	cases := []struct {
		name   string
		events []SessionEvent
		want   float64
	}{
		{"empty", nil, 0.0},
		{
			name: "all closed cleanly",
			events: []SessionEvent{
				{SessionID: "a", Kind: "start"},
				{SessionID: "a", Kind: "summary", Summary: "x"},
				{SessionID: "a", Kind: "end"},
				{SessionID: "b", Kind: "start"},
				{SessionID: "b", Kind: "summary", Summary: "y"},
				{SessionID: "b", Kind: "end"},
			},
			want: 1.0,
		},
		{
			name: "half closed (one missing summary)",
			events: []SessionEvent{
				{SessionID: "a", Kind: "start"},
				{SessionID: "a", Kind: "summary", Summary: "x"},
				{SessionID: "a", Kind: "end"},
				{SessionID: "b", Kind: "start"},
				{SessionID: "b", Kind: "end"},
			},
			want: 0.5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeAutopilotSessionCloseRate(tc.events)
			if got != tc.want {
				t.Fatalf("rate: want %v, got %v", tc.want, got)
			}
		})
	}
}
