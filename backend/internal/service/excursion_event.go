package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/constants"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/dto"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/repository"
)

type ExcursionEventService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.ExcursionEvent], error)
	Get(context.Context, uint) (model.ExcursionEvent, error)
	Create(context.Context, dto.CreateExcursionEvent, string, string) (model.ExcursionEvent, error)
	Update(context.Context, uint, dto.UpdateExcursionEvent, string, string) (model.ExcursionEvent, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string) (model.ExcursionEvent, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type excursionEventService struct {
	repository  repository.ExcursionEventRepository
	disposition repository.DispositionDecisionRepository
	evidence    repository.SensorEvidenceRepository
	windows     repository.TemperatureWindowRepository
	containers  repository.TransportContainerRepository
	security    SecurityService
}

func NewExcursionEventService(repo repository.ExcursionEventRepository, disposition repository.DispositionDecisionRepository, evidence repository.SensorEvidenceRepository, windows repository.TemperatureWindowRepository, containers repository.TransportContainerRepository, security SecurityService) ExcursionEventService {
	return &excursionEventService{repository: repo, disposition: disposition, evidence: evidence, windows: windows, containers: containers, security: security}
}

func (s *excursionEventService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.ExcursionEvent], error) {
	return s.repository.List(ctx, query)
}

func (s *excursionEventService) Get(ctx context.Context, id uint) (model.ExcursionEvent, error) {
	return s.repository.Get(ctx, id)
}

func (s *excursionEventService) Create(ctx context.Context, input dto.CreateExcursionEvent, actor, requestID string) (model.ExcursionEvent, error) {
	if err := validateExcursionEventBusinessFields(input.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.ExcursionEvent{}, err
	}
	item := model.ExcursionEvent{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.ExcursionEventInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: strings.TrimSpace(input.Facility), Owner: strings.TrimSpace(input.Owner),
		Category: strings.TrimSpace(input.Category), RiskLevel: input.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode:   strings.ToUpper(strings.TrimSpace(input.RelatedCode)),
		ContainerCode: strings.ToUpper(strings.TrimSpace(firstNonEmpty(input.ContainerCode, input.RelatedCode))),
		WindowCode:    strings.ToUpper(strings.TrimSpace(input.WindowCode)),
		ObservedTempC: input.ObservedTempC, DurationMinutes: input.DurationMinutes,
		DetectedAt:     fallbackTime(input.DetectedAt, input.EffectiveAt),
		SensorEvidence: strings.TrimSpace(firstNonEmpty(input.SensorEvidence, input.Evidence)),
		Reviewer:       strings.TrimSpace(input.Reviewer),
	}
	if item.ObservedTempC == 0 {
		item.ObservedTempC = input.MetricValue
	}
	if item.ContainerCode == "" || item.WindowCode == "" || item.SensorEvidence == "" || item.DurationMinutes < 1 {
		return model.ExcursionEvent{}, fmt.Errorf("%w: container, temperature window, duration and sensor evidence are required", ErrInvalidInput)
	}
	s.assessExcursion(ctx, &item, actor, requestID)
	if err := s.repository.Create(ctx, &item); err != nil {
		return model.ExcursionEvent{}, fmt.Errorf("create 偏差事件: %w", err)
	}
	_ = s.security.Audit(ctx, actor, requestID, "create", "ExcursionEvent", item.ID, "", item.Status, firstNonEmpty(item.AssessmentNote, "created 偏差事件"))
	return item, nil
}

// assessExcursion evaluates a freshly registered deviation against the effective
// temperature window. A breach that outlasts the allowed duration quarantines the
// container immediately (repeat reports for an already quarantined container are
// not migrated again); shorter breaches only flag the record for review. Missing
// or inactive windows are registered as-is and routed to manual judgement.
func (s *excursionEventService) assessExcursion(ctx context.Context, item *model.ExcursionEvent, actor, requestID string) {
	window, err := s.windows.FindByCode(ctx, item.WindowCode)
	if err != nil {
		item.AssessmentOutcome = model.AssessmentManualReview
		item.AssessmentNote = fmt.Sprintf("温控规则 %s 缺失，偏差已照常登记；结果：请人工判断", item.WindowCode)
		return
	}
	if window.Status != string(constants.WindowStateActive) {
		item.AssessmentOutcome = model.AssessmentManualReview
		item.AssessmentNote = fmt.Sprintf("温控规则 %s 未生效（当前状态 %s），偏差已照常登记；结果：请人工判断", window.Code, window.Status)
		return
	}
	breach := item.ObservedTempC < window.MinimumCelsius || item.ObservedTempC > window.MaximumCelsius
	if !breach {
		item.AssessmentOutcome = model.AssessmentWithinLimits
		item.AssessmentNote = fmt.Sprintf("触发温度 %.1f°C 位于规则 %s 窗口 [%.1f, %.1f]°C 内，允许时长 %d 分钟；结果：按常规流程复核",
			item.ObservedTempC, window.Code, window.MinimumCelsius, window.MaximumCelsius, window.MaxExcursionMinutes)
		return
	}
	if item.DurationMinutes <= window.MaxExcursionMinutes {
		item.AssessmentOutcome = model.AssessmentReviewOnly
		item.AssessmentNote = fmt.Sprintf("触发温度 %.1f°C 越出规则 %s 窗口 [%.1f, %.1f]°C，持续 %d 分钟未超过允许时长 %d 分钟；结果：提示复核，容器 %s 状态不变",
			item.ObservedTempC, window.Code, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes, window.MaxExcursionMinutes, item.ContainerCode)
		return
	}
	container, err := s.containers.FindByCode(ctx, item.ContainerCode)
	if err != nil {
		item.AssessmentOutcome = model.AssessmentManualReview
		item.AssessmentNote = fmt.Sprintf("触发温度 %.1f°C 越出规则 %s 窗口 [%.1f, %.1f]°C，持续 %d 分钟超过允许时长 %d 分钟，但容器 %s 未找到；结果：请人工判断",
			item.ObservedTempC, window.Code, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes, window.MaxExcursionMinutes, item.ContainerCode)
		return
	}
	if container.Status == string(constants.ContainerStateQuarantine) {
		item.AssessmentOutcome = model.AssessmentAlreadyQuarantined
		item.AssessmentNote = fmt.Sprintf("触发温度 %.1f°C 越出规则 %s 窗口 [%.1f, %.1f]°C，持续 %d 分钟超过允许时长 %d 分钟；结果：容器 %s 已处于隔离状态，重复上报不再迁移",
			item.ObservedTempC, window.Code, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes, window.MaxExcursionMinutes, item.ContainerCode)
		return
	}
	if !constants.CanTransition(constants.TransportContainerTransitions, container.Status, string(constants.ContainerStateQuarantine)) {
		item.AssessmentOutcome = model.AssessmentManualReview
		item.AssessmentNote = fmt.Sprintf("触发温度 %.1f°C 越出规则 %s 窗口 [%.1f, %.1f]°C，持续 %d 分钟超过允许时长 %d 分钟，但容器 %s 当前状态 %s 不允许自动隔离；结果：请人工判断",
			item.ObservedTempC, window.Code, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes, window.MaxExcursionMinutes, item.ContainerCode, container.Status)
		return
	}
	before := container.Status
	expectedVersion := container.Version
	container.Status = string(constants.ContainerStateQuarantine)
	container.Version = expectedVersion + 1
	container.UpdatedAt = time.Now().UTC()
	reason := fmt.Sprintf("偏差 %s 自动隔离：触发温度 %.1f°C 持续 %d 分钟超过允许时长 %d 分钟",
		item.Code, item.ObservedTempC, item.DurationMinutes, window.MaxExcursionMinutes)
	audit := auditLog(actor, requestID, "transition", "TransportContainer", container.ID, before, container.Status, reason)
	if err := s.containers.Update(ctx, container.ID, expectedVersion, &container, audit); err != nil {
		item.AssessmentOutcome = model.AssessmentManualReview
		item.AssessmentNote = fmt.Sprintf("触发温度 %.1f°C 越出规则 %s 窗口 [%.1f, %.1f]°C，持续 %d 分钟超过允许时长 %d 分钟，但容器 %s 自动隔离失败；结果：请人工判断",
			item.ObservedTempC, window.Code, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes, window.MaxExcursionMinutes, item.ContainerCode)
		return
	}
	item.AssessmentOutcome = model.AssessmentAutoQuarantine
	item.AssessmentNote = fmt.Sprintf("触发温度 %.1f°C 越出规则 %s 窗口 [%.1f, %.1f]°C，持续 %d 分钟超过允许时长 %d 分钟；结果：容器 %s 已立即隔离",
		item.ObservedTempC, window.Code, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes, window.MaxExcursionMinutes, item.ContainerCode)
}

func (s *excursionEventService) Update(ctx context.Context, id uint, input dto.UpdateExcursionEvent, actor, requestID string) (model.ExcursionEvent, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ExcursionEvent{}, err
	}
	if err := validateExcursionEventBusinessFields(current.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.ExcursionEvent{}, err
	}
	current.Name = strings.TrimSpace(input.Name)
	current.Description = strings.TrimSpace(input.Description)
	current.Facility = strings.TrimSpace(input.Facility)
	current.Owner = strings.TrimSpace(input.Owner)
	current.Category = strings.TrimSpace(input.Category)
	current.RiskLevel = input.RiskLevel
	current.MetricValue = input.MetricValue
	current.MetricUnit = strings.TrimSpace(input.MetricUnit)
	current.EffectiveAt = input.EffectiveAt.UTC()
	current.Evidence = strings.TrimSpace(input.Evidence)
	current.RelatedCode = strings.ToUpper(strings.TrimSpace(input.RelatedCode))
	current.ContainerCode = strings.ToUpper(strings.TrimSpace(firstNonEmpty(input.ContainerCode, input.RelatedCode)))
	current.WindowCode = strings.ToUpper(strings.TrimSpace(input.WindowCode))
	current.ObservedTempC = input.ObservedTempC
	if current.ObservedTempC == 0 {
		current.ObservedTempC = input.MetricValue
	}
	current.DurationMinutes = input.DurationMinutes
	current.DetectedAt = fallbackTime(input.DetectedAt, input.EffectiveAt)
	current.SensorEvidence = strings.TrimSpace(firstNonEmpty(input.SensorEvidence, input.Evidence))
	current.Reviewer = strings.TrimSpace(input.Reviewer)
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current); err != nil {
		return model.ExcursionEvent{}, fmt.Errorf("update 偏差事件: %w", err)
	}
	_ = s.security.Audit(ctx, actor, requestID, "update", "ExcursionEvent", id, current.Status, current.Status, "updated business fields")
	return s.repository.Get(ctx, id)
}

func (s *excursionEventService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, requestID string) (model.ExcursionEvent, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ExcursionEvent{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.ExcursionEventTransitions, current.Status, target) {
		return model.ExcursionEvent{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	before := current.Status
	evidence := strings.TrimSpace(input.Evidence)
	if evidence == "" {
		evidence = strings.TrimSpace(firstNonEmpty(current.SensorEvidence, current.Evidence))
	}
	if target == string(constants.ExcursionStateDecided) && evidence == "" {
		return model.ExcursionEvent{}, fmt.Errorf("%w: sensor evidence is required before deciding an excursion", ErrInvalidInput)
	}
	if target == string(constants.ExcursionStateDecided) {
		if count, err := s.evidence.CountForExcursion(ctx, current.Code); err != nil || count == 0 {
			return model.ExcursionEvent{}, fmt.Errorf("%w: registered sensor evidence is required before deciding an excursion", ErrInvalidInput)
		}
	}
	if target == string(constants.ExcursionStateClosed) {
		final, err := s.disposition.HasFinalForExcursion(ctx, current.Code)
		if err != nil || !final {
			return model.ExcursionEvent{}, fmt.Errorf("%w: a final disposition is required before closing an excursion", ErrInvalidInput)
		}
	}
	current.Status = target
	current.SensorEvidence = evidence
	current.Evidence = evidence
	if target == string(constants.ExcursionStateInReview) || target == string(constants.ExcursionStateDecided) {
		current.Reviewer = actor
	}
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	detail, _ := json.Marshal(map[string]any{"reason": input.Reason, "sensorEvidence": evidence, "containerCode": current.ContainerCode})
	if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current, auditLog(actor, requestID, "transition", "ExcursionEvent", id, before, target, string(detail))); err != nil {
		return model.ExcursionEvent{}, fmt.Errorf("transition 偏差事件: %w", err)
	}
	return s.repository.Get(ctx, id)
}

func (s *excursionEventService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.repository.Delete(ctx, id); err != nil {
		return err
	}
	return s.security.Audit(ctx, actor, requestID, "delete", "ExcursionEvent", id, current.Status, "deleted", "soft deleted 偏差事件")
}

func (s *excursionEventService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

func validateExcursionEventBusinessFields(code, name, facility, owner string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" {
		return ErrInvalidInput
	}
	return nil
}
