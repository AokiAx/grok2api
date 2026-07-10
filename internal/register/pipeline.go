package register

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/admin"
	"github.com/AokiAx/grok2api/internal/config"
	"github.com/AokiAx/grok2api/internal/register/mail"
	"github.com/AokiAx/grok2api/internal/register/mint"
	regproxy "github.com/AokiAx/grok2api/internal/register/proxy"
	"github.com/AokiAx/grok2api/internal/register/turnstile"
)

type Importer interface {
	Import(context.Context, admin.ImportRequest) (admin.ImportResult, error)
}

type RunConfig struct {
	Count    int
	Workers  int
	DryRun   bool
	ProxyURL string
}

type AccountOutcome struct {
	Index     int    `json:"index"`
	OK        bool   `json:"ok"`
	Email     string `json:"email,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	Pool      string `json:"pool,omitempty"`
	Error     string `json:"error,omitempty"`
}

type RunSummary struct {
	Requested int              `json:"requested"`
	OK        int              `json:"ok"`
	Failed    int              `json:"failed"`
	DryRun    bool             `json:"dry_run"`
	Accounts  []AccountOutcome `json:"accounts"`
}

type Pipeline struct {
	settings config.Config
	importer Importer
	proxies  regproxy.Provider
	now      func() time.Time
}

func NewPipeline(settings config.Config, importer Importer) *Pipeline {
	return &Pipeline{
		settings: settings,
		importer: importer,
		proxies:  regproxy.New(settings.Proxy, settings.ProxyPool),
		now:      time.Now,
	}
}

func (p *Pipeline) Run(ctx context.Context, cfg RunConfig, onEvent func(string)) (RunSummary, error) {
	if onEvent == nil {
		onEvent = func(string) {}
	}
	count := cfg.Count
	if count <= 0 {
		count = p.settings.TotalAccounts
	}
	if count <= 0 {
		count = 1
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = p.settings.MaxWorkers
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > count {
		workers = count
	}

	summary := RunSummary{Requested: count, DryRun: cfg.DryRun, Accounts: make([]AccountOutcome, count)}
	jobs := make(chan int)
	var mu sync.Mutex
	var wg sync.WaitGroup

	workerFn := func() {
		defer wg.Done()
		for index := range jobs {
			if err := ctx.Err(); err != nil {
				outcome := AccountOutcome{Index: index + 1, OK: false, Error: err.Error()}
				mu.Lock()
				summary.Accounts[index] = outcome
				summary.Failed++
				mu.Unlock()
				continue
			}
			outcome := p.registerOne(ctx, index+1, cfg, onEvent)
			mu.Lock()
			summary.Accounts[index] = outcome
			if outcome.OK {
				summary.OK++
			} else {
				summary.Failed++
			}
			mu.Unlock()
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go workerFn()
	}
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return summary, ctx.Err()
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return summary, nil
}

func (p *Pipeline) registerOne(ctx context.Context, index int, cfg RunConfig, onEvent func(string)) AccountOutcome {
	onEvent(fmt.Sprintf("[%d] starting", index))
	proxyURL := strings.TrimSpace(cfg.ProxyURL)
	if proxyURL == "" {
		proxyURL = p.proxies.Next()
	}
	httpClient := &http.Client{Timeout: 45 * time.Second}
	mailProvider, err := mail.NewProvider(p.settings, httpClient)
	if err != nil {
		return AccountOutcome{Index: index, Error: err.Error()}
	}
	solver, err := turnstile.NewFromMode(
		p.settings.TurnstileSolver,
		p.settings.TurnstileSolverURL,
		p.settings.CapMonsterAPIBase,
		p.settings.CapMonsterAPIKey,
		p.settings.TurnstileTimeout(),
		httpClient,
	)
	if err != nil {
		return AccountOutcome{Index: index, Error: err.Error()}
	}
	registrar, err := NewRegistrar(RegistrarConfig{
		AccountsBase:     p.settings.AccountsBase,
		TurnstileSitekey: p.settings.TurnstileSitekey,
		ProxyURL:         proxyURL,
		Solver:           solver,
		TurnstileTimeout: p.settings.TurnstileTimeout(),
	})
	if err != nil {
		return AccountOutcome{Index: index, Error: err.Error()}
	}

	email, token, err := mailProvider.CreateMailbox(ctx)
	if err != nil {
		mailProvider.RecordFailure(err.Error())
		return AccountOutcome{Index: index, Error: "mailbox: " + err.Error()}
	}
	password := mail.GeneratePassword(14)
	given, family := randomName()
	onEvent(fmt.Sprintf("[%d] mailbox ready %s", index, maskEmail(email)))

	wait := func(waitCtx context.Context, mailToken, mailEmail string) (string, error) {
		codeCtx, cancel := context.WithTimeout(waitCtx, p.settings.EmailCodeTimeout())
		defer cancel()
		return mailProvider.WaitCode(codeCtx, mailToken, mailEmail)
	}
	regResult, err := registrar.Register(ctx, email, password, given, family, token, wait)
	if err != nil {
		mailProvider.RecordFailure(err.Error())
		return AccountOutcome{Index: index, Email: email, Error: "register: " + err.Error()}
	}
	mailProvider.RecordSuccess()
	onEvent(fmt.Sprintf("[%d] registered, minting CLI tokens", index))

	minter := mint.NewDeviceMinter(nil)
	minted, err := minter.MintFromSSO(ctx, regResult.SSO, email)
	if err != nil {
		return AccountOutcome{Index: index, Email: email, Error: "mint: " + err.Error()}
	}
	if minted.Email == "" {
		minted.Email = email
	}
	if cfg.DryRun || p.importer == nil {
		return AccountOutcome{Index: index, OK: true, Email: minted.Email, AccountID: minted.Email, Pool: "dry-run"}
	}
	result, err := p.importer.Import(ctx, admin.ImportRequest{
		Accounts: []admin.ImportAccount{minted.ToImportAccount()},
	})
	if err != nil {
		return AccountOutcome{Index: index, Email: minted.Email, Error: "import: " + err.Error()}
	}
	accountID := minted.Email
	if len(result.Items) > 0 && result.Items[0].AccountID != "" {
		accountID = result.Items[0].AccountID
	}
	onEvent(fmt.Sprintf("[%d] imported %s", index, maskEmail(accountID)))
	return AccountOutcome{Index: index, OK: true, Email: minted.Email, AccountID: accountID, Pool: "imported"}
}

func (p *Pipeline) MintSSO(ctx context.Context, ssoCookie, email string, dryRun bool) (AccountOutcome, error) {
	minter := mint.NewDeviceMinter(nil)
	minted, err := minter.MintFromSSO(ctx, ssoCookie, email)
	if err != nil {
		return AccountOutcome{OK: false, Error: err.Error()}, err
	}
	if dryRun || p.importer == nil {
		return AccountOutcome{OK: true, Email: minted.Email, AccountID: minted.Email, Pool: "dry-run"}, nil
	}
	result, err := p.importer.Import(ctx, admin.ImportRequest{
		Accounts: []admin.ImportAccount{minted.ToImportAccount()},
	})
	if err != nil {
		return AccountOutcome{OK: false, Email: minted.Email, Error: err.Error()}, err
	}
	accountID := minted.Email
	if len(result.Items) > 0 && result.Items[0].AccountID != "" {
		accountID = result.Items[0].AccountID
	}
	return AccountOutcome{OK: true, Email: minted.Email, AccountID: accountID, Pool: "imported"}, nil
}

func maskEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return "***"
	}
	local := parts[0]
	if len(local) <= 3 {
		return "***@" + parts[1]
	}
	return local[:3] + "***@" + parts[1]
}

var firstNames = []string{"Alex", "Ava", "Ethan", "Emma", "Liam", "Mia", "Noah", "Olivia", "Ryan", "Sophia"}
var lastNames = []string{"Anderson", "Brown", "Clark", "Davis", "Evans", "Garcia", "Harris", "Johnson", "Miller", "Smith"}

func randomName() (string, string) {
	// reuse mail random alphabet helper via password length hack for indices
	seed := mail.GeneratePassword(8)
	fi := int(seed[0]) % len(firstNames)
	li := int(seed[1]) % len(lastNames)
	return firstNames[fi], lastNames[li]
}
