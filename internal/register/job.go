package register

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/config"
	"github.com/AokiAx/grok2api/internal/register/mail"
	"github.com/AokiAx/grok2api/internal/register/turnstile"
)

var (
	ErrJobRunning = errors.New("register job already running")
	ErrNoJob      = errors.New("no active register job")
)

type JobState string

const (
	JobIdle     JobState = "idle"
	JobRunning  JobState = "running"
	JobStopping JobState = "stopping"
	JobFinished JobState = "finished"
	JobFailed   JobState = "failed"
)

type JobStatus struct {
	State      JobState         `json:"state"`
	JobID      string           `json:"job_id,omitempty"`
	Requested  int              `json:"requested"`
	OK         int              `json:"ok"`
	Failed     int              `json:"failed"`
	DryRun     bool             `json:"dry_run"`
	StartedAt  time.Time        `json:"started_at,omitempty"`
	FinishedAt time.Time        `json:"finished_at,omitempty"`
	Error      string           `json:"error,omitempty"`
	Logs       []string         `json:"logs"`
	Accounts   []AccountOutcome `json:"accounts,omitempty"`
}

type JobManager struct {
	mu       sync.Mutex
	settings SettingsSource
	pipeline *Pipeline
	status   JobStatus
	cancel   context.CancelFunc
	seq      int
}

func NewJobManager(settings config.Config, pipeline *Pipeline) *JobManager {
	return NewJobManagerFromSource(staticSettings{cfg: settings}, pipeline)
}

func NewJobManagerFromSource(settings SettingsSource, pipeline *Pipeline) *JobManager {
	return &JobManager{
		settings: settings,
		pipeline: pipeline,
		status: JobStatus{
			State: JobIdle,
			Logs:  []string{},
		},
	}
}

func (m *JobManager) Status() JobStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := m.status
	cp.Logs = append([]string(nil), m.status.Logs...)
	cp.Accounts = append([]AccountOutcome(nil), m.status.Accounts...)
	return cp
}

func (m *JobManager) Start(cfg RunConfig) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.State == JobRunning || m.status.State == JobStopping {
		return "", ErrJobRunning
	}
	m.seq++
	jobID := fmt.Sprintf("reg-%d", m.seq)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.status = JobStatus{
		State:     JobRunning,
		JobID:     jobID,
		Requested: cfg.Count,
		DryRun:    cfg.DryRun,
		StartedAt: time.Now().UTC(),
		Logs:      []string{fmt.Sprintf("job %s started", jobID)},
	}
	go m.run(ctx, jobID, cfg)
	return jobID, nil
}

func (m *JobManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel == nil || (m.status.State != JobRunning && m.status.State != JobStopping) {
		return ErrNoJob
	}
	m.status.State = JobStopping
	m.appendLogLocked("stop requested")
	m.cancel()
	return nil
}

func (m *JobManager) run(ctx context.Context, jobID string, cfg RunConfig) {
	summary, err := m.pipeline.Run(ctx, cfg, func(message string) {
		m.mu.Lock()
		m.appendLogLocked(message)
		m.mu.Unlock()
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.OK = summary.OK
	m.status.Failed = summary.Failed
	m.status.Requested = summary.Requested
	m.status.Accounts = summary.Accounts
	m.status.FinishedAt = time.Now().UTC()
	m.cancel = nil
	if err != nil && !errors.Is(err, context.Canceled) {
		m.status.State = JobFailed
		m.status.Error = err.Error()
		m.appendLogLocked("job failed: " + err.Error())
		return
	}
	if errors.Is(err, context.Canceled) {
		m.status.State = JobFinished
		m.appendLogLocked("job canceled")
		return
	}
	m.status.State = JobFinished
	m.appendLogLocked(fmt.Sprintf("job %s finished ok=%d failed=%d", jobID, summary.OK, summary.Failed))
}

func (m *JobManager) appendLogLocked(message string) {
	m.status.Logs = append(m.status.Logs, fmt.Sprintf("%s %s", time.Now().UTC().Format(time.RFC3339), message))
	if len(m.status.Logs) > 200 {
		m.status.Logs = m.status.Logs[len(m.status.Logs)-200:]
	}
}

type HealthReport struct {
	Turnstile    string `json:"turnstile"`
	Email        string `json:"email"`
	Proxy        string `json:"proxy"`
	FlareSolverr string `json:"flaresolverr"`
	OK           bool   `json:"ok"`
	Detail       string `json:"detail,omitempty"`
}

func (m *JobManager) Health(ctx context.Context) HealthReport {
	settings := config.Defaults()
	if m.settings != nil {
		settings = m.settings.Get()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	report := HealthReport{
		Turnstile:    "unconfigured",
		Email:        "unconfigured",
		Proxy:        "direct",
		FlareSolverr: "disabled",
	}
	var problems []string

	if settings.Proxy != "" || len(settings.ProxyPool) > 0 {
		report.Proxy = "configured"
	}
	if settings.FlareSolverrEnabled && strings.TrimSpace(settings.FlareSolverrURL) != "" {
		// Optional side-car; not on the register hot path.
		client := &http.Client{Timeout: 4 * time.Second}
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(settings.FlareSolverrURL, "/")+"/", nil)
		if err != nil {
			report.FlareSolverr = "error"
			problems = append(problems, "flaresolverr: "+err.Error())
		} else if resp, err := client.Do(req); err != nil {
			report.FlareSolverr = "unreachable"
			problems = append(problems, "flaresolverr unreachable")
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				report.FlareSolverr = "unhealthy"
			} else {
				report.FlareSolverr = "ok"
			}
		}
	}

	// Email: config completeness (do not create mailboxes in health).
	providerName := strings.ToLower(strings.TrimSpace(settings.EmailProvider))
	if providerName == "" {
		providerName = "cfmail"
	}
	switch providerName {
	case "cfmail":
		if len(settings.CfmailAccounts) == 0 {
			report.Email = "missing_cfmail"
			problems = append(problems, "no cfmail accounts")
		} else {
			report.Email = "ok:cfmail"
		}
	case "mailtm":
		report.Email = "ok:mailtm"
	default:
		report.Email = "unsupported"
		problems = append(problems, "email provider "+providerName)
	}
	// Soft-check provider factory so misconfig surfaces early.
	if client, err := HTTPClient("", 5*time.Second, false); err == nil {
		if _, err := mail.NewProvider(settings, client); err != nil {
			report.Email = "error"
			problems = append(problems, "email: "+err.Error())
		}
	}

	// Turnstile: real probe against local solver or CapMonster key presence.
	mode := strings.ToLower(strings.TrimSpace(settings.TurnstileSolver))
	if mode == "" {
		mode = "auto"
	}
	solverClient, _ := HTTPClient("", 5*time.Second, false)
	solver, err := turnstile.NewFromMode(
		mode,
		settings.TurnstileSolverURL,
		settings.CapMonsterAPIBase,
		settings.CapMonsterAPIKey,
		8*time.Second,
		solverClient,
	)
	if err != nil {
		report.Turnstile = "error"
		problems = append(problems, "turnstile: "+err.Error())
	} else if err := solver.Healthy(probeCtx); err != nil {
		report.Turnstile = "unreachable:" + solver.Name()
		problems = append(problems, "turnstile: "+err.Error())
	} else {
		report.Turnstile = "ok:" + solver.Name()
	}

	report.OK = len(problems) == 0
	if len(problems) > 0 {
		report.Detail = strings.Join(problems, "; ")
	}
	return report
}

func (m *JobManager) Settings() config.Config {
	if m.settings == nil {
		return config.Defaults()
	}
	return m.settings.Get()
}
