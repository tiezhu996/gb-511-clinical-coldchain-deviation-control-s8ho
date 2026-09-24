package service

import (
	"fmt"

	"github.com/blueship581/clinical-coldchain-deviation-control/backend/internal/model"
)

// Automatic assessment outcomes recorded on 偏差事件 at registration time.
const (
	AssessmentAutoQuarantine = "auto_quarantine" // 越界且超允许时长：立即隔离容器
	AssessmentReviewRequired = "review_required" // 越界但未超允许时长：仅提示复核
	AssessmentManualReview   = "manual_review"   // 规则缺失/过期或容器未登记：人工判断
	AssessmentWithinLimits   = "within_limits"   // 未越界：无需动作
)

// excursionAssessment is the structured result of evaluating a freshly
// registered excursion against its effective temperature window.
type excursionAssessment struct {
	outcome        string
	basis          string // 判定依据：触发温度、规则窗口、持续时长与允许时长
	allowedMinutes int
	quarantine     bool // true 表示登记后必须立即隔离容器
}

// assessExcursion is a pure function so every branch stays unit-testable. A nil
// window means the referenced rule does not exist at all.
func assessExcursion(window *model.TemperatureWindow, item *model.ExcursionEvent) excursionAssessment {
	if window == nil {
		return excursionAssessment{
			outcome: AssessmentManualReview,
			basis:   fmt.Sprintf("温控规则 %s 未登记，无法自动判定", item.WindowCode),
		}
	}
	if window.Status != "active" {
		return excursionAssessment{
			outcome: AssessmentManualReview,
			basis:   fmt.Sprintf("温控规则 %s 当前状态为 %s，非生效(active)状态", window.Code, window.Status),
		}
	}
	assessment := excursionAssessment{allowedMinutes: window.MaxExcursionMinutes}
	outOfRange := item.ObservedTempC < window.MinimumCelsius || item.ObservedTempC > window.MaximumCelsius
	if !outOfRange {
		assessment.outcome = AssessmentWithinLimits
		assessment.basis = fmt.Sprintf("触发温度 %.1f°C 位于规则窗口 %.1f~%.1f°C 内，持续 %d 分钟",
			item.ObservedTempC, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes)
		return assessment
	}
	if item.DurationMinutes > window.MaxExcursionMinutes {
		assessment.outcome = AssessmentAutoQuarantine
		assessment.quarantine = true
		assessment.basis = fmt.Sprintf("触发温度 %.1f°C 越出规则窗口 %.1f~%.1f°C，持续 %d 分钟超过允许 %d 分钟",
			item.ObservedTempC, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes, window.MaxExcursionMinutes)
		return assessment
	}
	assessment.outcome = AssessmentReviewRequired
	assessment.basis = fmt.Sprintf("触发温度 %.1f°C 越出规则窗口 %.1f~%.1f°C，持续 %d 分钟未超过允许 %d 分钟",
		item.ObservedTempC, window.MinimumCelsius, window.MaximumCelsius, item.DurationMinutes, window.MaxExcursionMinutes)
	return assessment
}
