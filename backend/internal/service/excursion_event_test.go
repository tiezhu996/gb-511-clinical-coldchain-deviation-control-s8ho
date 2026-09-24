package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/config"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/dto"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/repository"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type excursionFixture struct {
	service ExcursionEventService
	db      *gorm.DB
}

func newExcursionFixture(t *testing.T) *excursionFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("unwrap sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.Role{}, &model.User{}, &model.AuditLog{}, &model.SensorEvidence{}, &model.TransportContainer{}, &model.TemperatureWindow{}, &model.ExcursionEvent{}, &model.DispositionDecision{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	windows := []model.TemperatureWindow{
		{BaseModel: model.BaseModel{Code: "TW-ACTIVE", Name: "冷藏 2-8C", Status: "active", Version: 1}, ProductClass: "临床样本", MinimumCelsius: 2, MaximumCelsius: 8, MaxExcursionMinutes: 15, QualityOwner: "reviewer", Facility: "QA", Owner: "reviewer", Category: "临床样本", RiskLevel: "high", MetricUnit: "C", EffectiveAt: now.Add(-24 * time.Hour)},
		{BaseModel: model.BaseModel{Code: "TW-DRAFT", Name: "草案规则", Status: "draft", Version: 1}, ProductClass: "辅材", MinimumCelsius: 15, MaximumCelsius: 25, MaxExcursionMinutes: 60, QualityOwner: "reviewer", Facility: "QA", Owner: "reviewer", Category: "辅材", RiskLevel: "medium", MetricUnit: "C", EffectiveAt: now.Add(-24 * time.Hour)},
	}
	if err := db.Create(&windows).Error; err != nil {
		t.Fatalf("seed windows: %v", err)
	}
	containers := []model.TransportContainer{
		{BaseModel: model.BaseModel{Code: "TC-MOVING", Name: "在途容器", Status: "in_transit", Version: 1}, SensorID: "SN-1", Facility: "上海", Owner: "李运输", Category: "主动制冷箱", RiskLevel: "low", MetricUnit: "C", EffectiveAt: now},
		{BaseModel: model.BaseModel{Code: "TC-FROZEN", Name: "已隔离容器", Status: "quarantine", Version: 3}, SensorID: "SN-2", Facility: "杭州", Owner: "赵收货", Category: "干冰罐", RiskLevel: "critical", MetricUnit: "C", EffectiveAt: now},
	}
	if err := db.Create(&containers).Error; err != nil {
		t.Fatalf("seed containers: %v", err)
	}
	security := NewSecurityService(repository.NewSecurityRepository(db), config.Config{})
	service := NewExcursionEventService(
		repository.NewExcursionEventRepository(db),
		repository.NewDispositionDecisionRepository(db),
		repository.NewSensorEvidenceRepository(db),
		repository.NewTemperatureWindowRepository(db),
		repository.NewTransportContainerRepository(db),
		security,
	)
	return &excursionFixture{service: service, db: db}
}

func (f *excursionFixture) createInput(code, containerCode, windowCode string, temp float64, duration int) dto.CreateExcursionEvent {
	return dto.CreateExcursionEvent{
		Code: code, Name: "温度偏差 " + code, Facility: "上海配送中心", Owner: "operator",
		Category: "高温偏差", RiskLevel: "high", MetricValue: temp, MetricUnit: "C",
		EffectiveAt: time.Now().UTC(), Evidence: "minio://sensor/trace.csv",
		ContainerCode: containerCode, WindowCode: windowCode, ObservedTempC: temp,
		DurationMinutes: duration, DetectedAt: time.Now().UTC(), SensorEvidence: "minio://sensor/trace.csv",
	}
}

func (f *excursionFixture) containerStatus(t *testing.T, code string) (string, uint) {
	t.Helper()
	var container model.TransportContainer
	if err := f.db.Where("code = ?", code).First(&container).Error; err != nil {
		t.Fatalf("load container %s: %v", code, err)
	}
	return container.Status, container.Version
}

func (f *excursionFixture) transitionAudits(t *testing.T, entityID uint) int64 {
	t.Helper()
	var count int64
	if err := f.db.Model(&model.AuditLog{}).Where("entity_type = ? AND entity_id = ? AND action = ?", "TransportContainer", entityID, "transition").Count(&count).Error; err != nil {
		t.Fatalf("count audits: %v", err)
	}
	return count
}

func TestExcursionAssessmentAutoQuarantinesOverLimitContainer(t *testing.T) {
	f := newExcursionFixture(t)
	item, err := f.service.Create(context.Background(), f.createInput("EE-OVER", "TC-MOVING", "TW-ACTIVE", 9.3, 18), "operator", "req-over")
	if err != nil {
		t.Fatalf("create excursion: %v", err)
	}
	if item.AssessmentOutcome != model.AssessmentAutoQuarantine {
		t.Fatalf("expected auto_quarantine outcome, got %q (%s)", item.AssessmentOutcome, item.AssessmentNote)
	}
	for _, fragment := range []string{"9.3", "15", "18", "已立即隔离"} {
		if !strings.Contains(item.AssessmentNote, fragment) {
			t.Fatalf("assessment note %q must mention %q", item.AssessmentNote, fragment)
		}
	}
	status, _ := f.containerStatus(t, "TC-MOVING")
	if status != "quarantine" {
		t.Fatalf("container must be quarantined, got %q", status)
	}
	var container model.TransportContainer
	if err := f.db.Where("code = ?", "TC-MOVING").First(&container).Error; err != nil {
		t.Fatal(err)
	}
	if count := f.transitionAudits(t, container.ID); count != 1 {
		t.Fatalf("expected exactly one container transition audit, got %d", count)
	}
}

func TestExcursionAssessmentReviewOnlyKeepsContainerStatus(t *testing.T) {
	f := newExcursionFixture(t)
	item, err := f.service.Create(context.Background(), f.createInput("EE-SHORT", "TC-MOVING", "TW-ACTIVE", 9.3, 10), "operator", "req-short")
	if err != nil {
		t.Fatalf("create excursion: %v", err)
	}
	if item.AssessmentOutcome != model.AssessmentReviewOnly {
		t.Fatalf("expected review_only outcome, got %q (%s)", item.AssessmentOutcome, item.AssessmentNote)
	}
	if !strings.Contains(item.AssessmentNote, "提示复核") || !strings.Contains(item.AssessmentNote, "状态不变") {
		t.Fatalf("review note must prompt review without touching the container: %q", item.AssessmentNote)
	}
	status, version := f.containerStatus(t, "TC-MOVING")
	if status != "in_transit" || version != 1 {
		t.Fatalf("container must stay in_transit at version 1, got %q v%d", status, version)
	}
}

func TestExcursionAssessmentSkipsRepeatReportForQuarantinedContainer(t *testing.T) {
	f := newExcursionFixture(t)
	item, err := f.service.Create(context.Background(), f.createInput("EE-REPEAT", "TC-FROZEN", "TW-ACTIVE", 9.3, 30), "operator", "req-repeat")
	if err != nil {
		t.Fatalf("create excursion: %v", err)
	}
	if item.AssessmentOutcome != model.AssessmentAlreadyQuarantined {
		t.Fatalf("expected already_quarantined outcome, got %q (%s)", item.AssessmentOutcome, item.AssessmentNote)
	}
	status, version := f.containerStatus(t, "TC-FROZEN")
	if status != "quarantine" || version != 3 {
		t.Fatalf("quarantined container must not migrate again, got %q v%d", status, version)
	}
	var container model.TransportContainer
	if err := f.db.Where("code = ?", "TC-FROZEN").First(&container).Error; err != nil {
		t.Fatal(err)
	}
	if count := f.transitionAudits(t, container.ID); count != 0 {
		t.Fatalf("repeat report must not write another transition audit, got %d", count)
	}
}

func TestExcursionAssessmentFallsBackToManualReview(t *testing.T) {
	f := newExcursionFixture(t)
	missing, err := f.service.Create(context.Background(), f.createInput("EE-NOWIN", "TC-MOVING", "TW-MISSING", 9.3, 30), "operator", "req-missing")
	if err != nil {
		t.Fatalf("missing window must still register: %v", err)
	}
	if missing.AssessmentOutcome != model.AssessmentManualReview || !strings.Contains(missing.AssessmentNote, "缺失") {
		t.Fatalf("missing window must prompt manual judgement, got %q (%s)", missing.AssessmentOutcome, missing.AssessmentNote)
	}
	inactive, err := f.service.Create(context.Background(), f.createInput("EE-DRAFT", "TC-MOVING", "TW-DRAFT", 9.3, 30), "operator", "req-draft")
	if err != nil {
		t.Fatalf("inactive window must still register: %v", err)
	}
	if inactive.AssessmentOutcome != model.AssessmentManualReview || !strings.Contains(inactive.AssessmentNote, "未生效") {
		t.Fatalf("inactive window must prompt manual judgement, got %q (%s)", inactive.AssessmentOutcome, inactive.AssessmentNote)
	}
	status, _ := f.containerStatus(t, "TC-MOVING")
	if status != "in_transit" {
		t.Fatalf("manual review path must not touch the container, got %q", status)
	}
}
