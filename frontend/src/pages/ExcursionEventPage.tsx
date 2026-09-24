
import { useEffect, useMemo, useState } from 'react';
import Button from '@mui/material/Button';
import AddAlertOutlinedIcon from '@mui/icons-material/AddAlertOutlined';
import TaskAltOutlinedIcon from '@mui/icons-material/TaskAltOutlined';
import { useExcursionEventStore } from '../stores/excursion-event';
import { useDispositionDecisionStore } from '../stores/disposition-decision';
import { useTemperatureWindowStore } from '../stores/temperature-window';
import { useTransportContainerStore } from '../stores/transport-container';
import { useSensorEvidenceStore } from '../stores/sensor-evidence';
import { registerSensorEvidence } from '../api/sensor-evidence';
import type { DomainRecord } from '../types/domain';
import { roleAtLeast } from '../types/domain';
import { getSession } from '../api/client';
import { usePolling } from '../hooks/usePolling';
import { MetricCard } from '../components/common/MetricCard';
import { StatusBadge } from '../components/common/StatusBadge';
import { TemperatureBadge } from '../components/common/TemperatureBadge';
import { EvidenceList } from '../components/common/EvidenceList';
import { DecisionPanel } from '../components/common/DecisionPanel';
import { ConfirmDialog } from '../components/common/ConfirmDialog';
import { formatDate } from '../utils/format';

function nextExcursionState(item: DomainRecord) {
  if (item.status === 'open') return 'in_review';
  if (item.status === 'in_review') return 'decided';
  if (item.status === 'decided') return 'closed';
  return '';
}

const OUTCOME_LABELS: Record<string, string> = {
  auto_quarantine: '已自动隔离',
  review_required: '待复核',
  manual_review: '人工判断',
  within_limits: '范围内',
};

function outcomeTone(outcome?: string) {
  if (outcome === 'auto_quarantine') return 'danger';
  if (outcome === 'review_required') return 'warning';
  if (outcome === 'within_limits') return 'success';
  return 'neutral';
}

export default function ExcursionEventPage() {
  const excursions = useExcursionEventStore();
  const dispositions = useDispositionDecisionStore();
  const windows = useTemperatureWindowStore();
  const containers = useTransportContainerStore();
  const evidenceStore = useSensorEvidenceStore();
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [pending, setPending] = useState<{ item: DomainRecord; state: string } | null>(null);
  const [notice, setNotice] = useState<{ code: string; outcome: string; note: string } | null>(null);
  const [form, setForm] = useState({ containerCode: 'TC-002', windowCode: 'TW-001', observedTempC: 9.3, durationMinutes: 18 });
  const session = getSession();
  const canReport = roleAtLeast(session?.role, 'operator');
  const canReview = roleAtLeast(session?.role, 'reviewer');
  const refresh = async () => { await Promise.all([excursions.load('excursions'), dispositions.load('dispositions'), windows.load('windows'), containers.load('containers'), evidenceStore.load()]); };
  useEffect(() => { void refresh(); }, [excursions.load, dispositions.load, windows.load, containers.load, evidenceStore.load]);
  usePolling(refresh, 15000);
  useEffect(() => { if (selectedId === null && excursions.items[0]) setSelectedId(excursions.items[0].id); }, [excursions.items, selectedId]);
  const selected = excursions.items.find((item) => item.id === selectedId) || null;
  const selectedRule = selected ? windows.items.find((window) => window.code === selected.windowCode) : undefined;
  const decision = selected ? dispositions.items.find((item) => (item.excursionCode || item.relatedCode) === selected.code) : null;
  const selectedEvidence = selected ? evidenceStore.items.filter((item) => item.excursionCode === selected.code).map((item) => `${item.code} · ${item.objectKey} · SHA256 ${item.sha256.slice(0, 10)}…`) : [];
  const critical = useMemo(() => excursions.items.filter((item) => item.riskLevel === 'critical').length, [excursions.items]);
  const pendingCount = useMemo(() => excursions.items.filter((item) => ['open', 'in_review'].includes(item.status)).length, [excursions.items]);
  const createExcursion = async () => {
    const suffix = Date.now().toString().slice(-5); const now = new Date().toISOString();
    const code = `EE-UI-${suffix}`; const objectKey = `sensor/${form.containerCode.toLowerCase()}/${code.toLowerCase()}.csv`;
    const created = await excursions.createRecord('excursions', { code, name: `${form.containerCode} 温度越界告警`, description: '工作台登记的传感器温度偏差', facility: '沪杭运输线', owner: '未分配', category: '高温偏差', riskLevel: 'high', metricValue: form.observedTempC, metricUnit: 'C', effectiveAt: now, evidence: `minio://clinical-evidence/${objectKey}`, relatedCode: form.containerCode, containerCode: form.containerCode, windowCode: form.windowCode, observedTempC: form.observedTempC, durationMinutes: form.durationMinutes, detectedAt: now, sensorEvidence: `minio://clinical-evidence/${objectKey}` });
    await registerSensorEvidence({ code: `SE-${suffix}`, excursionCode: code, containerCode: form.containerCode, objectKey, sha256: 'd'.repeat(64), mediaType: 'text/csv', sizeBytes: 2048, capturedAt: now, source: 'ui-logger-import' });
    await Promise.all([evidenceStore.load(), containers.load('containers')]);
    if (created?.assessmentOutcome) setNotice({ code: created.code, outcome: created.assessmentOutcome, note: created.assessmentNote || '' });
    setCreateOpen(false);
  };
  const transition = async () => {
    if (!pending) return;
    await excursions.transition('excursions', pending.item, pending.state, pending.state === 'in_review' ? '质量复核员接收偏差并核对传感器曲线' : pending.state === 'decided' ? '传感器证据完整，偏差影响评估完成' : '关联处置决定已完成', pending.item.sensorEvidence || pending.item.evidence);
    setPending(null);
  };
  return <main className="workspace"><header className="page-header"><div><p className="eyebrow">EXCURSION RESPONSE</p><h1>偏差处理</h1><p>关联运输容器、温控规则和原始传感器证据，完成影响评估。</p></div>{canReport && <Button variant="contained" startIcon={<AddAlertOutlinedIcon />} onClick={() => setCreateOpen(true)}>登记偏差</Button>}</header>
    <section className="metrics"><MetricCard label="偏差事件" value={excursions.meta.total} detail="全量可追溯" /><MetricCard label="待闭环" value={pendingCount} detail="待质量评估" /><MetricCard label="严重偏差" value={critical} detail="优先隔离" /></section>
    {(excursions.error || evidenceStore.error) && <div className="alert" role="alert">{excursions.error || evidenceStore.error}</div>}
    {notice && <div className={`alert alert--${outcomeTone(notice.outcome)}`} role="status"><strong>{notice.code} · {OUTCOME_LABELS[notice.outcome] || notice.outcome}</strong>：{notice.note}<button className="link-button" onClick={() => setNotice(null)}>知道了</button></div>}
    <section className="split-workspace"><div className="record-list">{excursions.items.map((item) => { const rule = windows.items.find((window) => window.code === item.windowCode); return <button key={item.id} className={selectedId === item.id ? 'record-row selected' : 'record-row'} onClick={() => setSelectedId(item.id)}><span><strong>{item.code}</strong><small>{item.containerCode || item.relatedCode} · {item.windowCode || '未绑定规则'}</small></span><TemperatureBadge value={item.observedTempC ?? item.metricValue} minimum={rule?.minimumCelsius} maximum={rule?.maximumCelsius} /><StatusBadge status={item.status} /></button>; })}</div>
      <aside className="detail-pane">{selected ? <><header><div><small>{selected.code}</small><h2>{selected.name}</h2></div><StatusBadge status={selected.status} /></header><div className="detail-grid"><span><small>运输容器</small>{selected.containerCode || selected.relatedCode}</span><span><small>温控规则</small>{selected.windowCode || '-'}</span><span><small>持续时长</small>{selected.durationMinutes || 0} 分钟</span><span><small>检测时间</small>{formatDate(selected.detectedAt || selected.effectiveAt)}</span></div>
        {selected.assessmentOutcome && <section className="assessment-panel"><header><h3>自动评估</h3><span className={`status status--${outcomeTone(selected.assessmentOutcome)}`}>{OUTCOME_LABELS[selected.assessmentOutcome] || selected.assessmentOutcome}</span></header><div className="detail-grid"><span><small>触发温度</small>{selected.observedTempC ?? selected.metricValue}°C{selectedRule ? `（规则 ${selectedRule.minimumCelsius}~${selectedRule.maximumCelsius}°C）` : ''}</span><span><small>允许时长</small>{selected.allowedMinutes ?? 0} 分钟（实际 {selected.durationMinutes || 0} 分钟）</span></div><p>{selected.assessmentNote}</p></section>}
        <h3>传感器证据</h3><EvidenceList evidence={selectedEvidence.length ? selectedEvidence : selected.sensorEvidence || selected.evidence} /><DecisionPanel decision={decision} compact />{canReview && nextExcursionState(selected) && <Button variant="contained" startIcon={<TaskAltOutlinedIcon />} onClick={() => setPending({ item: selected, state: nextExcursionState(selected) })}>{selected.status === 'open' ? '接收复核' : selected.status === 'in_review' ? '完成影响评估' : '关闭事件'}</Button>}</> : <div className="empty">选择一个偏差事件</div>}</aside></section>
    <ConfirmDialog open={createOpen} title="登记温度偏差" onCancel={() => setCreateOpen(false)} onConfirm={() => void createExcursion()}><p>选择容器与温控规则并填写峰值温度、持续时长；登记后系统立即按生效规则评估，越界且超允许时长将自动隔离容器。</p>
      <div className="form-grid"><label>运输容器<select value={form.containerCode} onChange={(event) => setForm({ ...form, containerCode: event.target.value })}>{containers.items.map((item) => <option key={item.code} value={item.code}>{item.code} · {item.name}（{item.status}）</option>)}{!containers.items.some((item) => item.code === form.containerCode) && <option value={form.containerCode}>{form.containerCode}</option>}</select></label>
        <label>温控规则<select value={form.windowCode} onChange={(event) => setForm({ ...form, windowCode: event.target.value })}>{windows.items.map((item) => <option key={item.code} value={item.code}>{item.code} · {item.minimumCelsius}~{item.maximumCelsius}°C / 允许 {item.maxExcursionMinutes} 分钟（{item.status}）</option>)}</select></label>
        <label>峰值温度 °C<input aria-label="峰值温度" type="number" step="0.1" value={form.observedTempC} onChange={(event) => setForm({ ...form, observedTempC: Number.parseFloat(event.target.value) || 0 })} /></label>
        <label>持续时长（分钟）<input aria-label="持续时长" type="number" min="1" max="10080" value={form.durationMinutes} onChange={(event) => setForm({ ...form, durationMinutes: Number.parseInt(event.target.value, 10) || 0 })} /></label></div></ConfirmDialog>
    <ConfirmDialog open={Boolean(pending)} title="确认偏差状态迁移" onCancel={() => setPending(null)} onConfirm={() => void transition()}><p>偏差不能跳过复核；形成影响评估时必须存在传感器证据。</p><strong>{pending?.item.status} → {pending?.state}</strong></ConfirmDialog>
  </main>;
}
