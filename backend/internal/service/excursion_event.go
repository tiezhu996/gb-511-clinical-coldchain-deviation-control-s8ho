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
	containers  repository.TransportContainerRepository
	windows     repository.TemperatureWindowRepository
	disposition repository.DispositionDecisionRepository
	evidence    repository.SensorEvidenceRepository
	security    SecurityService
}

func NewExcursionEventService(repo repository.ExcursionEventRepository, containers repository.TransportContainerRepository, windows repository.TemperatureWindowRepository, disposition repository.DispositionDecisionRepository, evidence repository.SensorEvidenceRepository, security SecurityService) ExcursionEventService {
	return &excursionEventService{repository: repo, containers: containers, windows: windows, disposition: disposition, evidence: evidence, security: security}
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
	// 读取对应温控规则并自动评估；规则缺失或过期时照常登记并提示人工判断。
	var window *model.TemperatureWindow
	if rule, err := s.windows.GetByCode(ctx, item.WindowCode); err == nil {
		window = &rule
	}
	assessment := assessExcursion(window, &item)
	resultNote := ""
	switch assessment.outcome {
	case AssessmentAutoQuarantine:
		note, downgraded, err := s.quarantineContainer(ctx, &item, assessment, actor, requestID)
		if err != nil {
			return model.ExcursionEvent{}, err
		}
		resultNote = note
		if downgraded {
			assessment.outcome = AssessmentManualReview
		}
	case AssessmentReviewRequired:
		resultNote = "结果：未超允许时长，容器状态不变，请复核"
	case AssessmentWithinLimits:
		resultNote = "结果：温度在允许范围内，容器继续运输"
	default:
		resultNote = "结果：已照常登记，请人工判断"
	}
	item.AssessmentOutcome = assessment.outcome
	item.AllowedMinutes = assessment.allowedMinutes
	item.AssessmentNote = assessment.basis + "；" + resultNote + "。"
	if err := s.repository.Create(ctx, &item); err != nil {
		return model.ExcursionEvent{}, fmt.Errorf("create 偏差事件: %w", err)
	}
	detail, _ := json.Marshal(map[string]any{
		"assessment": item.AssessmentOutcome, "observedTempC": item.ObservedTempC,
		"durationMinutes": item.DurationMinutes, "allowedMinutes": item.AllowedMinutes,
		"containerCode": item.ContainerCode, "windowCode": item.WindowCode,
	})
	_ = s.security.Audit(ctx, actor, requestID, "create", "ExcursionEvent", item.ID, "", item.Status, string(detail))
	return item, nil
}

// quarantineContainer moves the container to quarantine before the excursion is
// persisted, so a failed move never leaves a registered-but-still-shipping
// container behind. A container already in quarantine is left untouched. The
// returned flag reports whether the assessment must degrade to manual review.
func (s *excursionEventService) quarantineContainer(ctx context.Context, item *model.ExcursionEvent, assessment excursionAssessment, actor, requestID string) (string, bool, error) {
	container, err := s.containers.GetByCode(ctx, item.ContainerCode)
	if err != nil {
		return fmt.Sprintf("容器 %s 未登记，无法自动隔离，请人工处理", item.ContainerCode), true, nil
	}
	if container.Status == string(constants.ContainerStateQuarantine) {
		return fmt.Sprintf("容器 %s 已处于隔离状态，本次不重复迁移", container.Code), false, nil
	}
	if !constants.CanTransition(constants.TransportContainerTransitions, container.Status, string(constants.ContainerStateQuarantine)) {
		return fmt.Sprintf("容器 %s 当前状态 %s 不允许自动隔离，请人工处理", container.Code, container.Status), true, nil
	}
	before := container.Status
	expectedVersion := container.Version
	container.Status = string(constants.ContainerStateQuarantine)
	container.Version = expectedVersion + 1
	container.UpdatedAt = time.Now().UTC()
	detail, _ := json.Marshal(map[string]any{
		"reason": "excursion auto quarantine", "excursionCode": item.Code,
		"observedTempC": item.ObservedTempC, "durationMinutes": item.DurationMinutes,
		"allowedMinutes": assessment.allowedMinutes,
	})
	audit := auditLog(actor, requestID, "transition", "TransportContainer", container.ID, before, container.Status, string(detail))
	if err := s.containers.Update(ctx, container.ID, expectedVersion, &container, audit); err != nil {
		return "", false, fmt.Errorf("auto quarantine 运输容器: %w", err)
	}
	return fmt.Sprintf("容器 %s 已自动隔离（%s → quarantine）", container.Code, before), false, nil
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
