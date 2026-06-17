import { useCallback, useEffect, useMemo, useState } from 'react'
import ReactEChartsCore from 'echarts-for-react/lib/core'
import { BarChart, LineChart as EChartsLineChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import * as echarts from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'
import {
  Activity,
  BarChart3,
  BookOpen,
  ChevronLeft,
  ChevronRight,
  Copy,
  Download,
  ExternalLink,
  LoaderCircle,
  RefreshCw,
  RotateCcw,
  Save,
  Settings,
  Sparkles,
  Upload,
  Users,
  XCircle
} from 'lucide-react'

echarts.use([BarChart, EChartsLineChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

const REDACTED = '***set***'

const FIELDS = [
  'gitea_url',
  'gitea_token',
  'gitea_timeout',
  'webhook_secret',
  'model',
  'trigger_keywords',
  'concurrency',
  'codex_auth_mode',
  'codex_api_key',
  'codex_sandbox_mode',
  'claude_enabled',
  'claude_model',
  'claude_api_key',
  'claude_base_url',
  'claude_home',
  'cc_switch_config_dir',
  'cc_switch_provider_id',
  'claude_max_budget_usd',
  'minimax_enabled',
  'minimax_model',
  'minimax_provider_id',
  'minimax_api_key',
  'minimax_base_url',
  'minimax_max_budget_usd',
  'repo_allowlist',
  'timeout'
]

const FIELD_GROUPS = {
  common: ['gitea_url', 'gitea_token', 'gitea_timeout', 'webhook_secret', 'trigger_keywords', 'repo_allowlist', 'concurrency', 'timeout'],
  codex: ['model', 'codex_auth_mode', 'codex_sandbox_mode', 'codex_api_key'],
  claude: ['claude_enabled', 'claude_model', 'claude_api_key', 'claude_base_url', 'claude_home', 'cc_switch_config_dir', 'cc_switch_provider_id', 'claude_max_budget_usd'],
  minimax: ['minimax_enabled', 'minimax_model', 'minimax_provider_id', 'minimax_api_key', 'minimax_base_url', 'minimax_max_budget_usd']
}

const DEFAULT_SETTINGS = {
  claude_model: 'sonnet',
  claude_home: '/claude-home',
  cc_switch_config_dir: '/cc-switch',
  claude_max_budget_usd: '0.3',
  minimax_max_budget_usd: '0.3'
}

const SECRET_FIELDS = new Set(['gitea_token', 'webhook_secret', 'codex_api_key', 'claude_api_key', 'minimax_api_key'])

const SETTING_META = {
  gitea_url: { label: 'Gitea URL', placeholder: 'https://gcode.example.com' },
  gitea_token: { label: 'Gitea Token', secret: true },
  gitea_timeout: { label: 'Gitea Timeout', placeholder: '90s' },
  webhook_secret: { label: 'Webhook Secret', secret: true },
  model: { label: 'Codex Model', placeholder: 'gpt-5-codex' },
  trigger_keywords: { label: '触发关键词', placeholder: '/review,@review' },
  repo_allowlist: { label: '仓库白名单', placeholder: 'owner/repo,owner/repo2' },
  concurrency: { label: 'Worker 并发', placeholder: '5' },
  timeout: { label: 'Review Timeout', placeholder: '30m' },
  codex_auth_mode: { label: 'Codex Auth Mode', placeholder: 'authfile' },
  codex_sandbox_mode: { label: 'Codex Sandbox', placeholder: 'read-only' },
  codex_api_key: { label: 'Codex API Key', secret: true },
  claude_enabled: { label: '启用 Claude', type: 'select', options: ['false', 'true'] },
  claude_model: { label: 'Claude Model' },
  claude_api_key: { label: 'Claude API Key', secret: true },
  claude_base_url: { label: 'Claude Base URL' },
  claude_home: { label: 'Claude Home' },
  cc_switch_config_dir: { label: 'cc-switch 配置目录' },
  cc_switch_provider_id: { label: 'cc-switch Provider' },
  claude_max_budget_usd: { label: 'Claude 预算 USD' },
  minimax_enabled: { label: '启用 MiniMax', type: 'select', options: ['false', 'true'] },
  minimax_model: { label: 'MiniMax Model' },
  minimax_provider_id: { label: 'MiniMax Provider' },
  minimax_api_key: { label: 'MiniMax API Key', secret: true },
  minimax_base_url: { label: 'MiniMax Base URL' },
  minimax_max_budget_usd: { label: 'MiniMax 预算 USD' }
}

const TABS = [
  { id: 'jobs', label: '任务', icon: Activity },
  { id: 'analytics', label: '分析', icon: BarChart3 },
  { id: 'skills', label: 'Skill', icon: BookOpen },
  { id: 'config', label: '配置', icon: Settings }
]

async function fetchJSON(url, options = {}, timeoutMs = 10000) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await fetch(url, { ...options, signal: controller.signal })
    const text = await response.text()
    let body = null
    if (text.trim()) {
      try {
        body = JSON.parse(text)
      } catch {
        body = { error: text }
      }
    }
    if (!response.ok) {
      throw new Error(body?.error || body?.status || `${response.status} ${response.statusText}`)
    }
    return body
  } finally {
    clearTimeout(timer)
  }
}

function prettyTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return String(value).replace('T', ' ').replace('Z', '')
  return date.toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false
  }).replaceAll('/', '-')
}

function percent(value) {
  if (typeof value !== 'number') return '--'
  return `${Math.round(value * 1000) / 10}%`
}

function byPreferredOrder(order) {
  const rank = new Map(order.map((item, index) => [item, index]))
  return ([a], [b]) => (rank.get(a) ?? 999) - (rank.get(b) ?? 999) || a.localeCompare(b)
}

function encodePath(path) {
  return String(path || '').split('/').map(encodeURIComponent).join('/')
}

function sourceURL(baseURL, finding) {
  if (!baseURL || !finding?.owner || !finding?.repo || !finding?.path || !finding?.last_seen_sha) return ''
  let url = `${String(baseURL).replace(/\/+$/, '')}/${encodeURIComponent(finding.owner)}/${encodeURIComponent(finding.repo)}/src/commit/${encodeURIComponent(finding.last_seen_sha)}/${encodePath(finding.path)}`
  if (finding.line) url += `#L${encodeURIComponent(finding.line)}`
  return url
}

function StatusBadge({ status }) {
  return <span className={`badge status-${status || 'unknown'}`}>{status || '-'}</span>
}

function Message({ message }) {
  if (!message) return null
  return <div className={`message ${message.ok ? 'ok' : 'err'}`}>{message.text}</div>
}

function IconButton({ icon: Icon, children, className = '', ...props }) {
  return (
    <button className={`button ${className}`.trim()} type="button" {...props}>
      {Icon ? <Icon size={17} strokeWidth={2.2} /> : null}
      <span>{children}</span>
    </button>
  )
}

function StatCard({ label, value, hint }) {
  return (
    <div className="stat-card">
      <span>{label}</span>
      <strong>{value}</strong>
      <small>{hint}</small>
    </div>
  )
}

function App() {
  const [activeTab, setActiveTab] = useState('jobs')

  return (
    <div className="app-shell">
      <header className="topbar">
        <div>
          <p className="eyebrow">gitea-review-agent</p>
          <h1>控制台</h1>
        </div>
        <nav className="tabs" aria-label="控制台导航">
          {TABS.map((tab) => {
            const Icon = tab.icon
            return (
              <button key={tab.id} className={activeTab === tab.id ? 'active' : ''} type="button" onClick={() => setActiveTab(tab.id)}>
                <Icon size={17} />
                <span>{tab.label}</span>
              </button>
            )
          })}
        </nav>
        <a className="logout" href="/admin/logout">退出</a>
      </header>

      <main>
        {activeTab === 'jobs' ? <JobsPanel /> : null}
        {activeTab === 'analytics' ? <AnalyticsPanel /> : null}
        {activeTab === 'skills' ? <SkillsPanel /> : null}
        {activeTab === 'config' ? <ConfigPanel /> : null}
      </main>
    </div>
  )
}

function JobsPanel() {
  const [jobs, setJobs] = useState([])
  const [stats, setStats] = useState(null)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(false)
  const [message, setMessage] = useState(null)
  const [selectedId, setSelectedId] = useState(null)
  const [selectedJob, setSelectedJob] = useState(null)
  const [detailLoading, setDetailLoading] = useState(false)

  const loadStats = useCallback(async () => {
    const payload = await fetchJSON('/admin/api/jobs/stats', {}, 12000)
    setStats(payload)
  }, [])

  const loadJobs = useCallback(async (showBusy = false) => {
    if (showBusy) setLoading(true)
    try {
      const params = new URLSearchParams({ page: String(page), page_size: String(pageSize) })
      const payload = await fetchJSON(`/admin/api/jobs?${params.toString()}`, {}, 8000)
      setJobs(payload.jobs || [])
      setHasMore(Boolean(payload.has_more))
      if (!selectedId && payload.jobs?.length) setSelectedId(payload.jobs[0].id)
      setMessage(null)
    } catch (error) {
      setMessage({ ok: false, text: `加载任务失败：${error.message}` })
    } finally {
      if (showBusy) setLoading(false)
    }
  }, [page, pageSize, selectedId])

  useEffect(() => {
    loadJobs(true)
    loadStats().catch((error) => setMessage({ ok: false, text: `加载统计失败：${error.message}` }))
  }, [loadJobs, loadStats])

  useEffect(() => {
    const timer = setInterval(() => {
      loadJobs(false)
      loadStats().catch(() => {})
    }, 10000)
    return () => clearInterval(timer)
  }, [loadJobs, loadStats])

  useEffect(() => {
    if (!selectedId) {
      setSelectedJob(null)
      return
    }
    let cancelled = false
    setDetailLoading(true)
    fetchJSON(`/admin/api/jobs/${encodeURIComponent(selectedId)}`, {}, 8000)
      .then((payload) => {
        if (!cancelled) setSelectedJob(payload)
      })
      .catch((error) => {
        if (!cancelled) setMessage({ ok: false, text: `加载任务详情失败：${error.message}` })
      })
      .finally(() => {
        if (!cancelled) setDetailLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [selectedId])

  const refresh = async () => {
    await Promise.all([loadJobs(true), loadStats()])
    if (selectedId) {
      const payload = await fetchJSON(`/admin/api/jobs/${encodeURIComponent(selectedId)}`, {}, 8000)
      setSelectedJob(payload)
    }
  }

  const rerunJob = async (id) => {
    try {
      const payload = await fetchJSON(`/admin/api/jobs/${encodeURIComponent(id)}/rerun`, { method: 'POST' }, 8000)
      setMessage({ ok: true, text: `已重新运行任务 #${payload.job?.id || id}` })
      setSelectedId(payload.job?.id || id)
      await refresh()
    } catch (error) {
      setMessage({ ok: false, text: `重新运行失败：${error.message}` })
    }
  }

  const cancelJob = async (id) => {
    try {
      const payload = await fetchJSON(`/admin/api/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST' }, 8000)
      setMessage({ ok: true, text: `已取消任务 #${payload.job?.id || id}` })
      setSelectedJob(payload.job || null)
      await refresh()
    } catch (error) {
      setMessage({ ok: false, text: `取消失败：${error.message}` })
    }
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>近期任务</h2>
          <p>当前页 {jobs.length} 条，自动刷新中</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={refresh} disabled={loading}>立即刷新</IconButton>
          <IconButton icon={ChevronLeft} onClick={() => setPage((v) => Math.max(1, v - 1))} disabled={page <= 1}>上一页</IconButton>
          <span className="pager">{page}</span>
          <IconButton icon={ChevronRight} onClick={() => setPage((v) => v + 1)} disabled={!hasMore}>下一页</IconButton>
          <select value={pageSize} onChange={(event) => { setPage(1); setPageSize(Number(event.target.value)) }}>
            <option value="20">20 / 页</option>
            <option value="50">50 / 页</option>
            <option value="100">100 / 页</option>
          </select>
        </div>
      </div>

      <JobStats stats={stats} />
      <Message message={message} />

      <div className="table-shell">
        <table className="data-table job-table">
          <thead>
            <tr>
              <th>ID</th>
              <th>PR</th>
              <th>事件</th>
              <th>动作</th>
              <th>状态</th>
              <th>次数</th>
              <th>创建时间</th>
              <th>开始时间</th>
              <th>会话</th>
              <th>日志</th>
            </tr>
          </thead>
          <tbody>
            {jobs.length ? jobs.map((job) => (
              <tr key={job.id} className={String(job.id) === String(selectedId) ? 'selected' : ''} onClick={() => setSelectedId(job.id)}>
                <td>{job.id}</td>
                <td>{job.owner}/{job.repo}#{job.number}</td>
                <td>{job.event}</td>
                <td>{job.action || '-'}</td>
                <td><StatusBadge status={job.status} /></td>
                <td>{job.attempts}</td>
                <td>{prettyTime(job.created_at)}</td>
                <td>{prettyTime(job.started_at)}</td>
                <td><code>{job.session_id || '-'}</code></td>
                <td><span className={job.error ? 'log-pill has-error' : 'log-pill'}>{job.log_count || 0} logs{job.error ? ' + error' : ''}</span></td>
              </tr>
            )) : (
              <tr><td colSpan="10" className="empty-cell">{loading ? '加载中...' : '暂无任务'}</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <JobDetail job={selectedJob} loading={detailLoading} onRerun={rerunJob} onCancel={cancelJob} />
    </section>
  )
}

function JobStats({ stats }) {
  const successRate = stats ? percent(stats.success_rate) : '--'
  return (
    <div className="stats-grid">
      <StatCard label="成功率" value={successRate} hint="done / done+failed" />
      <StatCard label="审核总数" value={stats?.review_jobs ?? '-'} hint="pull_request jobs" />
      <StatCard label="成功审核" value={stats?.done ?? '-'} hint="done" />
      <StatCard label="失败" value={stats?.failed ?? '-'} hint="需要排查" />
      <StatCard label="运行/等待" value={(stats?.running ?? 0) + (stats?.pending ?? 0)} hint="running + pending" />
      <StatCard label="可重试等待" value={stats?.retryable_pending ?? '-'} hint="retryable pending" />
      <StatCard label="已替换/取消" value={(stats?.superseded ?? 0) + (stats?.canceled ?? 0)} hint="superseded + canceled" />
    </div>
  )
}

function JobDetail({ job, loading, onRerun, onCancel }) {
  if (loading) return <section className="detail-panel"><h2>任务详情</h2><p className="muted">加载中...</p></section>
  if (!job) return <section className="detail-panel"><h2>任务详情</h2><p className="muted">选择一条任务查看日志和操作。</p></section>
  const logs = [...(job.logs || [])]
  if (job.error) logs.push({ stage: 'error', message: job.error, created_at: job.finished_at || job.created_at })

  return (
    <section className="detail-panel">
      <div className="detail-head">
        <div>
          <h2>任务 #{job.id}</h2>
          <p>{job.owner}/{job.repo}#{job.number}</p>
        </div>
        <div className="toolbar">
          <StatusBadge status={job.status} />
          {job.status === 'pending' ? <IconButton icon={XCircle} className="danger" onClick={() => onCancel(job.id)}>取消</IconButton> : null}
          <IconButton icon={RotateCcw} onClick={() => onRerun(job.id)}>重新运行</IconButton>
        </div>
      </div>

      <div className="detail-grid">
        <Info label="事件" value={job.event} />
        <Info label="动作" value={job.action || '-'} />
        <Info label="次数" value={job.attempts} />
        <Info label="会话" value={job.session_id || '-'} code />
        <Info label="错误类型" value={job.error_type || '-'} />
        <Info label="下次重试" value={prettyTime(job.next_attempt_at)} />
      </div>

      <div className="timeline">
        {logs.length ? logs.map((log, index) => (
          <div className="log-row" key={`${log.stage}-${log.created_at}-${index}`}>
            <span>{prettyTime(log.created_at)}</span>
            <strong>{log.stage}</strong>
            <pre>{log.message}</pre>
          </div>
        )) : <p className="muted">暂无日志</p>}
      </div>
    </section>
  )
}

function Info({ label, value, code }) {
  return (
    <div className="info-card">
      <span>{label}</span>
      {code ? <code>{value}</code> : <strong>{value}</strong>}
    </div>
  )
}

function SkillsPanel() {
  const [projects, setProjects] = useState([])
  const [selected, setSelected] = useState(null)
  const [detail, setDetail] = useState(null)
  const [loading, setLoading] = useState(false)
  const [generating, setGenerating] = useState(false)
  const [generationTask, setGenerationTask] = useState(null)
  const [message, setMessage] = useState(null)
  const [copied, setCopied] = useState(false)

  const loadProjects = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/skills/projects', {}, 10000)
      const list = payload.projects || []
      setProjects(list)
      setSelected((current) => current || list[0] || null)
      setMessage(null)
    } catch (error) {
      setMessage({ ok: false, text: `加载失败：${error.message}` })
    }
  }, [])

  useEffect(() => {
    loadProjects()
  }, [loadProjects])

  useEffect(() => {
    if (!selected) {
      setDetail(null)
      return
    }
    let cancelled = false
    async function loadDetail() {
      setLoading(true)
      try {
        const payload = await fetchJSON(`/admin/api/skills/${encodeURIComponent(selected.owner)}/${encodeURIComponent(selected.repo)}`, {}, 10000)
        if (!cancelled) setDetail(payload.skill || null)
      } catch (error) {
        if (!cancelled) setMessage({ ok: false, text: `读取 Skill 失败：${error.message}` })
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    loadDetail()
    return () => {
      cancelled = true
    }
  }, [selected])

  const generate = async () => {
    if (!selected) return
    setGenerating(true)
    setGenerationTask(null)
    setMessage({ ok: true, text: '已提交 Codex Skill 生成任务，正在等待结果...' })
    try {
      const payload = await fetchJSON(`/admin/api/skills/${encodeURIComponent(selected.owner)}/${encodeURIComponent(selected.repo)}/generate`, { method: 'POST' }, 15000)
      const task = payload.task || null
      if (!task?.id) throw new Error('生成任务没有返回 task id')
      setGenerationTask(task)
    } catch (error) {
      setMessage({ ok: false, text: `生成失败：${error.message}` })
      setGenerationTask(null)
      setGenerating(false)
    }
  }

  useEffect(() => {
    if (!generationTask || generationTask.status !== 'running') return undefined
    let cancelled = false
    const poll = async () => {
      try {
        const payload = await fetchJSON(`/admin/api/skills/${encodeURIComponent(generationTask.owner)}/${encodeURIComponent(generationTask.repo)}/generate/${encodeURIComponent(generationTask.id)}`, {}, 10000)
        if (cancelled) return
        const task = payload.task || null
        setGenerationTask(task)
        if (task?.status === 'done') {
          if (task.skill) setDetail(task.skill)
          await loadProjects()
          if (!cancelled) {
            setMessage({ ok: true, text: 'Skill 已更新' })
            setGenerating(false)
            setGenerationTask(null)
          }
        } else if (task?.status === 'failed') {
          setMessage({ ok: false, text: `生成失败：${task.error || '未知错误'}` })
          setGenerating(false)
          setGenerationTask(null)
        }
      } catch (error) {
        if (!cancelled) setMessage({ ok: false, text: `轮询生成状态失败：${error.message}` })
      }
    }
    const timer = setInterval(poll, 2000)
    poll()
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [generationTask?.id, generationTask?.owner, generationTask?.repo, generationTask?.status, loadProjects])

  const skill = detail || {}
  const ctx = skill.context || {}
  const busy = loading || generating
  const downloadPath = selected ? `/skills/${encodeURIComponent(selected.owner)}/${encodeURIComponent(selected.repo)}/SKILL.md` : ''
  const origin = typeof window === 'undefined' ? '' : window.location.origin
  const installCommand = selected
    ? `请为 ${selected.owner}/${selected.repo} 安装并使用这个项目缺陷预防 Skill：${origin}${downloadPath}`
    : ''

  const copyCommand = async () => {
    if (!installCommand) return
    try {
      await navigator.clipboard.writeText(installCommand)
    } catch {
      const input = document.createElement('textarea')
      input.value = installCommand
      input.setAttribute('readonly', 'true')
      input.style.position = 'fixed'
      input.style.opacity = '0'
      document.body.appendChild(input)
      input.select()
      document.execCommand('copy')
      document.body.removeChild(input)
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 1200)
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>Skill</h2>
          <p>按项目把常见缺陷沉淀成可下载、可演进的 Codex Skill。</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={loadProjects} disabled={generating}>刷新</IconButton>
          <IconButton icon={generating ? LoaderCircle : Sparkles} className={generating ? 'busy' : ''} onClick={generate} disabled={!selected || generating}>
            {generating ? 'Codex 生成中' : '生成/进化'}
          </IconButton>
        </div>
      </div>
      <Message message={message} />

      <div className="skills-layout">
        <aside className="skill-projects">
          <h3>项目</h3>
          <div className="skill-project-list">
            {projects.length ? projects.map((project) => {
              const active = selected?.owner === project.owner && selected?.repo === project.repo
              return (
                <button className={active ? 'skill-project active' : 'skill-project'} type="button" key={`${project.owner}/${project.repo}`} onClick={() => setSelected(project)} disabled={generating}>
                  <strong>{project.owner}/{project.repo}</strong>
                  <span>{project.findings} findings · {project.open_findings} open</span>
                  {project.skill_version ? <small>v{project.skill_version}</small> : <small>未生成</small>}
                </button>
              )
            }) : <div className="empty-state compact">暂无可沉淀的项目缺陷</div>}
          </div>
        </aside>

        <div className="skill-workspace" aria-busy={busy ? 'true' : 'false'}>
          {selected ? (
            <>
              <div className="skill-hero">
                <div>
                  <span>{selected.owner}/{selected.repo}</span>
                  <h3>{skill.title || '尚未生成 Skill'}</h3>
                  <p>{ctx.findings || selected.findings || 0} 个历史缺陷，{ctx.high_critical_open || selected.high_critical_open || 0} 个严重 Open。</p>
                </div>
                <div className="skill-actions">
                  <a className={generating || !skill.content ? 'button disabled-link' : 'button'} href={generating || !skill.content ? undefined : downloadPath} target="_blank" rel="noreferrer" aria-disabled={generating || !skill.content ? 'true' : 'false'}><Download size={17} />下载</a>
                  <button className="button" type="button" onClick={copyCommand} disabled={!skill.content || generating}><Copy size={17} />{copied ? '已复制' : '复制 Skill 指令'}</button>
                </div>
              </div>

              {generating ? (
                <div className="skill-generating-banner" role="status">
                  <LoaderCircle size={18} />
                  <div>
                    <strong>Codex 正在生成/进化 Skill</strong>
                    <span>任务 {generationTask?.id || '-'} 正在后台运行，完成后自动刷新版本与下载链接。</span>
                  </div>
                </div>
              ) : null}

              <div className="skill-metrics">
                <StatCard label="PR" value={ctx.pull_requests ?? selected.pull_requests ?? '-'} hint="project PRs" />
                <StatCard label="Review Runs" value={ctx.review_runs ?? selected.review_runs ?? '-'} hint="agent runs" />
                <StatCard label="Findings" value={ctx.findings ?? selected.findings ?? '-'} hint="source defects" />
                <StatCard label="版本" value={skill.version ? `v${skill.version}` : '-'} hint={skill.updated_at ? prettyTime(skill.updated_at) : 'not generated'} />
              </div>

              <section className="subsection">
                <div className="subsection-title">
                  <div>
                    <h3>Skill 指令</h3>
                    <span>按项目区分，链接不需要控制台鉴权</span>
                  </div>
                  <button className="button compact-button" type="button" onClick={copyCommand} disabled={!skill.content || generating}><Copy size={15} />{copied ? '已复制' : '复制指令'}</button>
                </div>
                <pre className="command-box">{installCommand}</pre>
              </section>

              <section className="subsection">
                <div className="subsection-title">
                  <div>
                    <h3>证据摘要</h3>
                    <span>生成时会把这些缺陷模式交给 Codex</span>
                  </div>
                </div>
                <div className="pattern-list">
                  {(ctx.patterns || []).slice(0, 6).map((pattern) => (
                    <article className="pattern-card" key={`${pattern.title}-${pattern.severity}-${pattern.status}`}>
                      <strong>{pattern.title}</strong>
                      <span>{pattern.severity} · {pattern.status} · {pattern.count} 次</span>
                      <small>{pattern.sample_path}{pattern.sample_line ? `:${pattern.sample_line}` : ''}</small>
                    </article>
                  ))}
                  {!(ctx.patterns || []).length ? <div className="empty-state compact">暂无模式摘要</div> : null}
                </div>
              </section>

              <section className="subsection">
                <div className="subsection-title">
                  <div>
                    <h3>SKILL.md</h3>
                    <span>{generating ? 'Codex 正在生成...' : loading ? '加载中...' : skill.content ? '可直接下载使用' : '点击生成后出现内容'}</span>
                  </div>
                </div>
                <pre className={generating ? 'skill-preview generating' : 'skill-preview'}>{generating ? '正在基于项目历史缺陷和已有 Skill 生成，请稍候...' : skill.content || '还没有生成 Skill。'}</pre>
              </section>
            </>
          ) : (
            <div className="empty-state">暂无项目数据。</div>
          )}
        </div>
      </div>
    </section>
  )
}

function AnalyticsPanel() {
  const [report, setReport] = useState(null)
  const [trend, setTrend] = useState([])
  const [loading, setLoading] = useState(false)
  const [message, setMessage] = useState(null)

  const loadLatest = useCallback(async () => {
    try {
      const [latestPayload, trendPayload] = await Promise.all([
        fetchJSON('/admin/api/analytics/reports/latest', {}, 10000),
        fetchJSON('/admin/api/analytics/trend?limit=12', {}, 10000)
      ])
      setReport(latestPayload.report || null)
      setTrend(trendPayload.points || [])
      setMessage(null)
    } catch (error) {
      setMessage({ ok: false, text: `加载失败：${error.message}` })
    }
  }, [])

  useEffect(() => {
    loadLatest()
  }, [loadLatest])

  const createReport = async () => {
    setLoading(true)
    setMessage({ ok: true, text: '正在生成全量历史分析...' })
    try {
      const payload = await fetchJSON('/admin/api/analytics/reports', { method: 'POST' }, 30000)
      setReport(payload.report || null)
      const trendPayload = await fetchJSON('/admin/api/analytics/trend?limit=12', {}, 10000)
      setTrend(trendPayload.points || [])
      setMessage({ ok: true, text: '分析报告已生成' })
    } catch (error) {
      setMessage({ ok: false, text: `生成失败：${error.message}` })
    } finally {
      setLoading(false)
    }
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>分析</h2>
          <p>{report ? `报告 #${report.id} · ${prettyTime(report.created_at)}` : '暂无报告'}</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={loadLatest}>刷新</IconButton>
          <IconButton icon={BarChart3} onClick={createReport} disabled={loading}>生成分析报告</IconButton>
        </div>
      </div>
      <Message message={message} />
      <AnalysisReport report={report} trend={trend} />
    </section>
  )
}

function AnalysisReport({ report, trend = [] }) {
  if (!report) return <div className="empty-state">点击“生成分析报告”后查看全量历史聚合。</div>
  const summary = report.summary || {}
  const completed = (summary.successful_review_runs || 0) + (summary.failed_review_runs || 0)
  const agents = Object.entries(summary.by_agent || {}).sort(([a], [b]) => a.localeCompare(b))
  const severities = Object.entries(summary.by_severity || {}).sort(byPreferredOrder(['critical', 'high', 'medium', 'low', 'info']))
  const statuses = Object.entries(summary.by_status || {}).sort(byPreferredOrder(['open', 'fixed', 'dismissed']))
  const developers = summary.by_developer || []
  const severityChart = severities.map(([label, value]) => ({ label, value }))
  const statusChart = statuses.map(([label, value]) => ({ label, value }))
  const agentChart = agents
    .map(([label, agent]) => ({ label, value: agent.findings || 0, meta: `${agent.open || 0} open` }))
    .sort((a, b) => b.value - a.value || a.label.localeCompare(b.label))
  const developerChart = developers
    .map((developer) => ({ label: developer.developer, value: developer.findings || 0, meta: `${developer.open_findings || 0} open · ${developer.pull_requests || 0} PR` }))
    .sort((a, b) => b.value - a.value || a.label.localeCompare(b.label))

  return (
    <>
      <div className="stats-grid">
        <StatCard label="Review 成功率" value={completed ? percent(summary.success_rate) : '--'} hint={completed ? `${summary.successful_review_runs || 0} / ${completed}` : '暂无运行'} />
        <StatCard label="问题总数" value={summary.total_findings || 0} hint="findings" />
        <StatCard label="Open 问题" value={summary.open_findings || 0} hint="仍需关注" />
        <StatCard label="严重 Open" value={summary.high_critical_open || 0} hint="high + critical" />
        <StatCard label="已修复" value={summary.fixed_findings || 0} hint="fixed" />
      </div>

      <TrendOverview points={trend} />

      <div className="chart-grid">
        <BarChartBlock title="按严重度" label="Risk" items={severityChart} empty="暂无严重度数据" tone="risk" />
        <BarChartBlock title="按状态" label="Lifecycle" items={statusChart} empty="暂无状态数据" tone="status" />
        <BarChartBlock title="按 Reviewer 发现量" label="Agent" items={agentChart} empty="暂无 reviewer 数据" tone="agent" />
        <BarChartBlock title="按研发发现量" label="Owner" items={developerChart} empty="暂无研发数据" tone="developer" />
      </div>

      <div className="two-column">
        <TableBlock title="Top Tags" headers={['Tag', '数量']} empty="暂无 tags">
          {(summary.top_tags || []).map((tag) => <tr key={tag.tag}><td>{tag.tag}</td><td>{tag.count}</td></tr>)}
        </TableBlock>
        <TableBlock title="严重度分布" headers={['严重度', '数量']} empty="暂无 findings">
          {severities.map(([severity, count]) => <tr key={severity}><td>{severity}</td><td>{count}</td></tr>)}
        </TableBlock>
      </div>

      <TableBlock title="近期严重问题" headers={['Reviewer', '严重度', '位置', '标题', '状态']} empty="暂无 high/critical 问题">
        {(summary.recent_severe || []).map((finding, index) => (
          <tr key={`${finding.path}-${finding.line}-${index}`}>
            <td>{finding.agent}</td>
            <td><StatusBadge status={finding.severity} /></td>
            <td><SourceLink baseURL={report.gitea_url} finding={finding} /></td>
            <td>{finding.title}</td>
            <td>{finding.status}</td>
          </tr>
        ))}
      </TableBlock>

      <TableBlock title="Reviewer 对比" headers={['Reviewer', '运行', '成功', '失败', '发现', 'Open']} empty="暂无 reviewer run">
        {agents.map(([name, agent]) => (
          <tr key={name}>
            <td>{name}</td>
            <td>{agent.review_runs || 0}</td>
            <td>{agent.succeeded || 0}</td>
            <td>{agent.failed || 0}</td>
            <td>{agent.findings || 0}</td>
            <td>{agent.open || 0}</td>
          </tr>
        ))}
      </TableBlock>

      <TableBlock title="研发统计" headers={['研发', 'PR', '运行', '成功', '失败', '发现', 'Open', '严重 Open']} empty="暂无研发数据">
        {developers.map((developer) => (
          <tr key={developer.developer}>
            <td>{developer.developer}</td>
            <td>{developer.pull_requests || 0}</td>
            <td>{developer.review_runs || 0}</td>
            <td>{developer.successful_review_runs || 0}</td>
            <td>{developer.failed_review_runs || 0}</td>
            <td>{developer.findings || 0}</td>
            <td>{developer.open_findings || 0}</td>
            <td>{developer.high_critical_open || 0}</td>
          </tr>
        ))}
      </TableBlock>

      <div className="two-column">
        <ListBlock title="重复问题标题" items={(summary.repeated_titles || []).map((item) => `${item.title} (${item.count})`)} empty="暂无重复标题" />
        <MultiAgentOverlap report={report} items={summary.agent_overlap || []} />
      </div>
    </>
  )
}

function TrendOverview({ points }) {
  const history = [...(points || [])].filter((item) => item?.finished_at)
  if (history.length < 2) {
    return (
      <section className="subsection trend-section">
        <div className="subsection-title">
          <h3>趋势</h3>
          <span>至少需要 2 份报告</span>
        </div>
        <div className="empty-state compact">多生成几次分析报告后，这里会展示趋势。</div>
      </section>
    )
  }

  const chartPoints = history.map((item, index) => {
    return {
      id: `${item.finished_at}-${index}`,
      label: prettyTime(item.finished_at).slice(5, 16),
      total: item.total_findings || 0,
      open: item.open_findings || 0,
      severe: item.high_critical_open || 0,
      success: Math.round((item.success_rate || 0) * 1000) / 10
    }
  })

  return (
    <div className="trend-grid">
      <LineChart
        title="问题趋势"
        subtitle={`${chartPoints[0].label} -> ${chartPoints[chartPoints.length - 1].label}`}
        points={chartPoints}
        series={[
          { key: 'total', label: '问题总数', color: '#315f7d' },
          { key: 'open', label: 'Open', color: '#9b5b2a' },
          { key: 'severe', label: '严重 Open', color: '#9a3333' }
        ]}
      />
      <LineChart
        title="Review 成功率趋势"
        subtitle={`最近 ${chartPoints.length} 次完成的 review`}
        points={chartPoints}
        valueSuffix="%"
        series={[
          { key: 'success', label: '成功率', color: '#2f6f55' }
        ]}
      />
    </div>
  )
}

function LineChart({ title, subtitle, points, series, valueSuffix = '' }) {
  const values = series.flatMap((line) => points.map((point) => point[line.key] || 0))
  const maxValue = Math.max(1, ...values)
  const latest = points[points.length - 1]
  const option = {
    color: series.map((line) => line.color),
    tooltip: {
      trigger: 'axis',
      confine: true,
      backgroundColor: 'rgba(28, 28, 26, 0.92)',
      borderWidth: 0,
      textStyle: { color: '#fff', fontWeight: 700 },
      valueFormatter: (value) => `${value}${valueSuffix}`
    },
    legend: {
      bottom: 0,
      left: 0,
      itemWidth: 18,
      itemHeight: 6,
      textStyle: { color: '#555', fontSize: 12, fontWeight: 700 }
    },
    grid: { left: 34, right: 16, top: 18, bottom: 38, containLabel: true },
    xAxis: {
      type: 'category',
      boundaryGap: false,
      data: points.map((point) => point.label),
      axisLine: { lineStyle: { color: '#deded8' } },
      axisTick: { show: false },
      axisLabel: { color: '#777', fontWeight: 700 }
    },
    yAxis: {
      type: 'value',
      min: 0,
      max: Math.ceil(maxValue * 1.12),
      splitLine: { lineStyle: { color: '#ecece7', type: 'dashed' } },
      axisLabel: { color: '#777', fontWeight: 700, formatter: `{value}${valueSuffix}` }
    },
    series: series.map((line, index) => ({
      name: line.label,
      type: 'line',
      smooth: true,
      symbol: 'circle',
      symbolSize: index === 0 ? 7 : 6,
      lineStyle: { width: index === 0 ? 3.5 : 2.5 },
      itemStyle: { borderColor: '#fff', borderWidth: 2 },
      areaStyle: index === 0 ? { opacity: 0.18 } : undefined,
      emphasis: { focus: 'series' },
      data: points.map((point) => point[line.key] || 0)
    }))
  }

  return (
    <section className="subsection trend-card">
      <div className="subsection-title">
        <div>
          <h3>{title}</h3>
          <span>{subtitle}</span>
        </div>
        <div className="trend-latest">
          {series.map((line) => (
            <strong key={line.key} style={{ color: line.color }}>{latest[line.key] || 0}{valueSuffix}</strong>
          ))}
        </div>
      </div>
      <ReactEChartsCore echarts={echarts} className="echart line-echart" style={{ height: 226 }} option={option} notMerge lazyUpdate />
    </section>
  )
}

function BarChartBlock({ title, label, items, empty, tone = 'default' }) {
  const visibleItems = items.slice(0, 4)
  const hiddenCount = Math.max(0, items.length - visibleItems.length)
  const maxValue = Math.max(1, ...visibleItems.map((item) => item.value || 0))
  const total = items.reduce((sum, item) => sum + (item.value || 0), 0)
  const leader = visibleItems[0]
  const metaItems = visibleItems.filter((item) => item.meta).slice(0, 2)
  const colors = {
    risk: ['#a33333', '#c98243', '#587f9c', '#7aa17a', '#777'],
    status: ['#9b5b2a', '#467c60', '#80745f'],
    agent: ['#315f7d', '#4f8b79', '#9b6f3d', '#777'],
    developer: ['#5c7f4f', '#7f8650', '#925f49', '#4f778a']
  }[tone] || ['#4f7d62']
  const option = {
    color: colors,
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'shadow' },
      confine: true,
      backgroundColor: 'rgba(28, 28, 26, 0.92)',
      borderWidth: 0,
      textStyle: { color: '#fff', fontWeight: 700 }
    },
    grid: { left: 8, right: 10, top: 8, bottom: 0, containLabel: true },
    xAxis: { type: 'value', show: false, max: Math.ceil(maxValue * 1.12) },
    yAxis: {
      type: 'category',
      inverse: true,
      data: visibleItems.map((item) => item.label),
      axisLine: { show: false },
      axisTick: { show: false },
      axisLabel: { color: '#2c2c2a', fontSize: 11, fontWeight: 850, width: 68, overflow: 'truncate' }
    },
    series: [{
      type: 'bar',
      data: visibleItems.map((item, index) => ({ value: item.value || 0, itemStyle: { color: colors[index % colors.length] } })),
      barWidth: 8,
      barGap: '40%',
      showBackground: true,
      backgroundStyle: { color: '#eeeee9', borderRadius: 999 },
      itemStyle: { borderRadius: 999 },
      label: {
        show: true,
        position: 'right',
        color: '#202020',
        fontWeight: 900,
        formatter: '{c}'
      }
    }]
  }
  return (
    <section className={`subsection chart-card chart-card-${tone}`}>
      <div className="chart-card-head">
        <div>
          <span>{label}</span>
          <h3>{title}</h3>
        </div>
        <strong><b>{total || '-'}</b><small>findings</small></strong>
      </div>
      {leader ? <p className="chart-card-note">{leader.label} 占比最高</p> : null}
      {visibleItems.length ? (
        <>
          <ReactEChartsCore echarts={echarts} className="echart bar-echart" style={{ height: Math.max(66, visibleItems.length * 24 + 16) }} option={option} notMerge lazyUpdate />
          {(metaItems.length || hiddenCount) ? (
            <div className="bar-meta-list">
              {metaItems.map((item) => <span key={item.label}>{item.label}: {item.meta}</span>)}
              {hiddenCount ? <span>+{hiddenCount} more</span> : null}
            </div>
          ) : null}
        </>
      ) : (
        <EmptyChart label={empty} />
      )}
    </section>
  )
}

function EmptyChart({ label }) {
  return (
    <div className="empty-chart" aria-label={label}>
      <div className="empty-chart-bars" aria-hidden="true">
        <span style={{ '--bar-width': '68%' }} />
        <span style={{ '--bar-width': '46%' }} />
        <span style={{ '--bar-width': '28%' }} />
      </div>
      <strong>{label}</strong>
    </div>
  )
}

function TableBlock({ title, headers, empty, children }) {
  const rows = Array.isArray(children) ? children.filter(Boolean) : children ? [children] : []
  return (
    <section className="subsection">
      <h3>{title}</h3>
      <div className="table-shell compact">
        <table className="data-table">
          <thead><tr>{headers.map((header) => <th key={header}>{header}</th>)}</tr></thead>
          <tbody>{rows.length ? rows : <tr><td colSpan={headers.length} className="empty-cell">{empty}</td></tr>}</tbody>
        </table>
      </div>
    </section>
  )
}

function ListBlock({ title, items, empty }) {
  return (
    <section className="subsection">
      <h3>{title}</h3>
      <ul className="plain-list">
        {items.length ? items.map((item, index) => (
          <li key={item.key || item || index}>{item.node || item}</li>
        )) : <li className="muted">{empty}</li>}
      </ul>
    </section>
  )
}

function MultiAgentOverlap({ report, items }) {
  return (
    <section className="subsection overlap-section">
      <div className="overlap-head">
        <h3>多 Agent 同时发现</h3>
        <span>{items.length} 组</span>
      </div>
      {items.length ? (
        <div className="overlap-list">
          {items.map((item, index) => (
            <article className="overlap-card" key={item.fingerprint || `${item.path}-${item.line}-${index}`}>
              <div className="overlap-card-main">
                <div className="overlap-title">{item.title || '(无标题)'}</div>
                <div className="overlap-meta">
                  <span>{item.owner}/{item.repo}#{item.pull_number}</span>
                  <span>{item.agent_count || item.agents?.length || 0} agents</span>
                </div>
                <div className="agent-pills">
                  {(item.agents || []).map((agent) => (
                    <span className="agent-pill" key={agent}><Users size={13} />{agent}</span>
                  ))}
                </div>
              </div>
              <SourceLink baseURL={report.gitea_url} finding={item} actionLabel="跳到行" prominent />
            </article>
          ))}
        </div>
      ) : (
        <div className="empty-overlap">暂无重叠问题</div>
      )}
    </section>
  )
}

function SourceLink({ baseURL, finding, actionLabel = '打开', prominent = false }) {
  const label = finding?.path ? `${finding.path}${finding.line ? `:${finding.line}` : ''}` : '无位置'
  const url = sourceURL(baseURL, finding)
  if (!url) return <code>{label}</code>
  return (
    <span className={prominent ? 'source-link source-link-prominent' : 'source-link'}>
      <code>{label}</code>
      <a className="line-link" href={url} target="_blank" rel="noreferrer">
        <ExternalLink size={14} />
        <span>{actionLabel}</span>
      </a>
    </span>
  )
}

function ConfigPanel() {
  const [settings, setSettings] = useState(() => ({ ...DEFAULT_SETTINGS }))
  const [effectiveConfig, setEffectiveConfig] = useState(null)
  const [status, setStatus] = useState(null)
  const [settingsMessage, setSettingsMessage] = useState(null)
  const [authMessage, setAuthMessage] = useState(null)
  const [authFile, setAuthFile] = useState(null)

  const loadSettings = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/settings', {}, 10000)
      setSettings({ ...DEFAULT_SETTINGS, ...payload })
      setSettingsMessage({ ok: true, text: '已加载配置' })
    } catch (error) {
      setSettingsMessage({ ok: false, text: `加载失败：${error.message}` })
    }
  }, [])

  const loadEffectiveConfig = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/effective-config', {}, 10000)
      setEffectiveConfig(payload)
    } catch (error) {
      setSettingsMessage({ ok: false, text: `读取 effective config 失败：${error.message}` })
    }
  }, [])

  const checkStatus = useCallback(async () => {
    try {
      const payload = await fetchJSON('/admin/api/status', {}, 20000)
      setStatus(payload)
    } catch (error) {
      setStatus({ ok: false, status: `检查失败：${error.message}` })
    }
  }, [])

  useEffect(() => {
    loadSettings()
    loadEffectiveConfig()
  }, [loadSettings, loadEffectiveConfig])

  const setField = (key, value) => {
    setSettings((current) => ({ ...current, [key]: value }))
  }

  const saveGroup = async (keys, label) => {
    const payload = {}
    keys.forEach((key) => {
      const value = settings[key] ?? ''
      if (SECRET_FIELDS.has(key) && value === REDACTED) return
      payload[key] = value
    })
    if (!Object.keys(payload).length) {
      setSettingsMessage({ ok: true, text: '没有需要保存的字段' })
      return
    }
    try {
      const result = await fetchJSON('/admin/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ settings: payload })
      }, 12000)
      setSettingsMessage({ ok: true, text: `已保存 ${label}（${result.updated || 0} 项）` })
      await Promise.all([loadSettings(), loadEffectiveConfig(), checkStatus()])
    } catch (error) {
      setSettingsMessage({ ok: false, text: `保存失败：${error.message}` })
    }
  }

  const uploadAuth = async () => {
    if (!authFile) {
      setAuthMessage({ ok: false, text: '请先选择 auth.json' })
      return
    }
    try {
      const text = await authFile.text()
      const result = await fetchJSON('/admin/api/authfile', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: text
      }, 12000)
      setAuthMessage({ ok: true, text: `已上传到 ${result.path}` })
      await checkStatus()
    } catch (error) {
      setAuthMessage({ ok: false, text: `上传失败：${error.message}` })
    }
  }

  return (
    <section className="panel">
      <div className="section-head">
        <div>
          <h2>配置</h2>
          <p>保存后写入数据库，后续 review 读取最新设置。</p>
        </div>
        <div className="toolbar">
          <IconButton icon={RefreshCw} onClick={() => { loadSettings(); loadEffectiveConfig(); }}>刷新</IconButton>
          <IconButton icon={Activity} onClick={checkStatus}>检测 Reviewer</IconButton>
        </div>
      </div>

      <Message message={settingsMessage} />

      <ConfigGroup title="通用" keys={FIELD_GROUPS.common} settings={settings} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.common, '通用设置')} />
      <ConfigGroup title="Codex" keys={FIELD_GROUPS.codex} settings={settings} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.codex, 'Codex 设置')} />
      <ConfigGroup title="Claude" keys={FIELD_GROUPS.claude} settings={settings} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.claude, 'Claude 设置')} />
      <ConfigGroup title="MiniMax" keys={FIELD_GROUPS.minimax} settings={settings} setField={setField} onSave={() => saveGroup(FIELD_GROUPS.minimax, 'MiniMax 设置')} />

      <section className="config-group">
        <div className="group-head">
          <div>
            <h3>Codex Auth</h3>
            <p>上传本机 `~/.codex/auth.json`，用于 authfile 模式。</p>
          </div>
          <div className="toolbar">
            <input className="file-input" type="file" accept="application/json,.json" onChange={(event) => setAuthFile(event.target.files?.[0] || null)} />
            <IconButton icon={Upload} onClick={uploadAuth}>上传</IconButton>
          </div>
        </div>
        <Message message={authMessage} />
      </section>

      <section className="config-group">
        <div className="group-head">
          <div>
            <h3>Reviewer 状态</h3>
            <p>{status ? (status.ok ? '可用' : '不可用') : '未检测'}</p>
          </div>
        </div>
        {status ? <pre className="status-box">{status.status || JSON.stringify(status, null, 2)}</pre> : null}
      </section>

      <EffectiveConfig data={effectiveConfig} />
    </section>
  )
}

function ConfigGroup({ title, keys, settings, setField, onSave }) {
  return (
    <section className="config-group">
      <div className="group-head">
        <h3>{title}</h3>
        <IconButton icon={Save} onClick={onSave}>保存</IconButton>
      </div>
      <div className="form-grid">
        {keys.map((key) => <SettingField key={key} name={key} value={settings[key] ?? ''} onChange={(value) => setField(key, value)} />)}
      </div>
    </section>
  )
}

function SettingField({ name, value, onChange }) {
  const meta = SETTING_META[name] || { label: name }
  const id = `setting-${name}`
  return (
    <label className="field" htmlFor={id}>
      <span>{meta.label}</span>
      {meta.type === 'select' ? (
        <select id={id} value={value} onChange={(event) => onChange(event.target.value)}>
          {meta.options.map((option) => <option key={option} value={option}>{option}</option>)}
        </select>
      ) : (
        <input
          id={id}
          type={meta.secret ? 'password' : 'text'}
          value={value}
          placeholder={meta.placeholder || ''}
          autoComplete="off"
          onChange={(event) => onChange(event.target.value)}
        />
      )}
    </label>
  )
}

function EffectiveConfig({ data }) {
  const rows = useMemo(() => {
    if (!data) return []
    return FIELDS.filter((key) => key in data || `${key}_set` in data).map((key) => {
      const value = key in data ? data[key] : data[`${key}_set`] ? 'set' : 'empty'
      return [key, String(value)]
    })
  }, [data])

  return (
    <section className="config-group">
      <div className="group-head">
        <div>
          <h3>Effective Config</h3>
          <p>{data?.runtime_reload_note || '读取当前运行时配置快照。'}</p>
        </div>
      </div>
      <div className="table-shell compact">
        <table className="data-table">
          <tbody>
            {rows.length ? rows.map(([key, value]) => <tr key={key}><td><code>{key}</code></td><td>{value}</td></tr>) : <tr><td className="empty-cell">暂无配置快照</td></tr>}
          </tbody>
        </table>
      </div>
    </section>
  )
}

export default App
