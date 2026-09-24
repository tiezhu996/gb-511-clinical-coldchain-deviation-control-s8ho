package service

import (
	"testing"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
)

func activeWindow() *model.TemperatureWindow {
	return &model.TemperatureWindow{
		BaseModel:           model.BaseModel{Code: "TW-001", Status: "active"},
		MinimumCelsius:      2,
		MaximumCelsius:      8,
		MaxExcursionMinutes: 15,
	}
}

func excursion(temp float64, minutes int) *model.ExcursionEvent {
	return &model.ExcursionEvent{
		ContainerCode: "TC-002", WindowCode: "TW-001",
		ObservedTempC: temp, DurationMinutes: minutes,
	}
}

func TestAssessExcursionMissingWindowRequiresManualReview(t *testing.T) {
	result := assessExcursion(nil, excursion(9.3, 18))
	if result.outcome != AssessmentManualReview || result.quarantine {
		t.Fatalf("expected manual review without quarantine, got %+v", result)
	}
}

func TestAssessExcursionInactiveWindowRequiresManualReview(t *testing.T) {
	for _, status := range []string{"draft", "expired", "superseded"} {
		window := activeWindow()
		window.Status = status
		result := assessExcursion(window, excursion(9.3, 18))
		if result.outcome != AssessmentManualReview || result.quarantine {
			t.Fatalf("status %s: expected manual review without quarantine, got %+v", status, result)
		}
	}
}

func TestAssessExcursionWithinLimits(t *testing.T) {
	result := assessExcursion(activeWindow(), excursion(7.6, 30))
	if result.outcome != AssessmentWithinLimits || result.quarantine {
		t.Fatalf("expected within limits, got %+v", result)
	}
	if result.allowedMinutes != 15 {
		t.Fatalf("expected allowed minutes snapshot 15, got %d", result.allowedMinutes)
	}
}

func TestAssessExcursionOutOfRangeWithinAllowedDuration(t *testing.T) {
	result := assessExcursion(activeWindow(), excursion(9.3, 15))
	if result.outcome != AssessmentReviewRequired || result.quarantine {
		t.Fatalf("duration equal to the limit must only prompt review, got %+v", result)
	}
}

func TestAssessExcursionOutOfRangeBeyondAllowedDuration(t *testing.T) {
	for _, temp := range []float64{1.9, 8.1} {
		result := assessExcursion(activeWindow(), excursion(temp, 16))
		if result.outcome != AssessmentAutoQuarantine || !result.quarantine {
			t.Fatalf("temp %.1f: expected auto quarantine, got %+v", temp, result)
		}
	}
}
