package register

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/config"
)

var (
	ErrJobRunning = errors.New("register job already running")
	ErrNoJob      = errors.New("no active register job")
)

type JobState string

const (
	JobIdle    JobState = "idle"
	JobRunning JobState = "running"
	JobStopping JobState = "stopping"
	JobFinished JobState = "finished"
	JobFailed  JobState = "failed"
)

type JobStatus struct {
	State     JobState         `json:"state"`
	JobID     string           `json:"job_id,omitempty"`
	Requested int              `json:"requested"`
	OK        int              `json:"ok"`
	Failed    int              `json:"failed"`
	DryRun    bool             `json:"dry_run"`
	StartedAt time.Time        `json:"started_at,omitempty"`
	FinishedAt time.Time       `json:"finished_at,omitempty"`
	Error     string           `json:"error,omitempty"`
	Logs      []string         `json:"logs"`
	Accounts  []AccountOutcome `json:"accounts,omitempty"`
}

type JobManager struct {
	mu       sync.Mutex
	settings config.Config
	pipeline *Pipeline
	status   JobStatus
	cancel   context.CancelFunc
	seq      int
}

func NewJobManager(settings config.Config, pipeline *Pipeline) *JobManager {
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
}

func (m *JobManager) Health(ctx context.Context) HealthReport {
	report := HealthReport{
		Turnstile:    "unconfigured",
		Email:        "unconfigured",
		Proxy:        "direct",
		FlareSolverr: "disabled",
	}
	if m.settings.Proxy != "" || len(m.settings.ProxyPool) > 0 {
		report.Proxy = "configured"
	}
	if m.settings.FlareSolverrEnabled && m.settings.FlareSolverrURL != "" {
		report.FlareSolverr = "configured"
	}
	// Lightweight config presence checks; deep probes happen at run time.
	if m.settings.TurnstileSolverURL != "" || m.settings.CapMonsterAPIKey != "" {
		report.Turnstile = m.settings.TurnstileSolver
	}
	if len(m.settings.CfmailAccounts) > 0 || m.settings.EmailProvider == "mailtm" {
		report.Email = m.settings.EmailProvider
	}
	return report
}
