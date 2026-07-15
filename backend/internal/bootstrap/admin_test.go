package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/bootstrap"
	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/security"
	"golang.org/x/crypto/bcrypt"
)

type fakeAdminBootstrapRepository struct {
	count       int
	status      repository.BootstrapStatus
	createErr   error
	createdUser adminauth.AdminUser
}

func (f *fakeAdminBootstrapRepository) CountAdminUsers(context.Context) (int, error) {
	return f.count, nil
}

func (f *fakeAdminBootstrapRepository) BootstrapAdmin(_ context.Context, user adminauth.AdminUser) (repository.BootstrapStatus, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.createdUser = user
	if f.status != "" {
		return f.status, nil
	}
	return repository.BootstrapCreated, nil
}

func fixedBootstrapClock() time.Time {
	return time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
}

func TestAdminBootstrapServiceCreatesHashedAdministrator(t *testing.T) {
	repo := &fakeAdminBootstrapRepository{}
	service := bootstrap.NewAdminBootstrapService(repo, fixedBootstrapClock, bcrypt.MinCost,
		bootstrap.WithAdminBootstrapRandom(func(buffer []byte) error {
			for i := range buffer {
				buffer[i] = byte(i + 1)
			}
			return nil
		}),
	)

	result, err := service.Bootstrap(context.Background(), "  long-enough-password  ")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if result.Status != repository.BootstrapCreated {
		t.Fatalf("status=%q", result.Status)
	}
	if repo.createdUser.Username != "admin" || repo.createdUser.ID == "" {
		t.Fatalf("created user=%+v", repo.createdUser)
	}
	if !security.VerifyAdminPassword(repo.createdUser.Password, "  long-enough-password  ") {
		t.Fatal("stored password does not verify")
	}
	if security.VerifyAdminPassword(repo.createdUser.Password, "long-enough-password") {
		t.Fatal("password whitespace was unexpectedly trimmed")
	}
	if strings.Contains(repo.createdUser.Password.Hash, "long-enough-password") {
		t.Fatal("plaintext password leaked into credential")
	}
}

func TestAdminBootstrapServiceRejectsWeakPasswordsWithoutWriting(t *testing.T) {
	for _, password := range []string{"", "            ", "short", strings.Repeat("x", 1025)} {
		t.Run(password, func(t *testing.T) {
			repo := &fakeAdminBootstrapRepository{}
			service := bootstrap.NewAdminBootstrapService(repo, fixedBootstrapClock, bcrypt.MinCost)
			if _, err := service.Bootstrap(context.Background(), password); !errors.Is(err, bootstrap.ErrWeakPassword) {
				t.Fatalf("err=%v", err)
			}
			if repo.createdUser.ID != "" {
				t.Fatalf("weak password wrote user=%+v", repo.createdUser)
			}
		})
	}
}

func TestAdminBootstrapServiceMapsExistingAndCompletedStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status repository.BootstrapStatus
		want   error
	}{
		{name: "existing", status: repository.BootstrapExisting, want: bootstrap.ErrAdminAlreadyExists},
		{name: "completed", status: repository.BootstrapAlreadyCompleted, want: bootstrap.ErrBootstrapAlreadyCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeAdminBootstrapRepository{status: tc.status}
			service := bootstrap.NewAdminBootstrapService(repo, fixedBootstrapClock, bcrypt.MinCost)
			if _, err := service.Bootstrap(context.Background(), "long-enough-password"); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
}

func TestAdminBootstrapServicePropagatesRepositoryFailure(t *testing.T) {
	repo := &fakeAdminBootstrapRepository{createErr: errors.New("write failed")}
	service := bootstrap.NewAdminBootstrapService(repo, fixedBootstrapClock, bcrypt.MinCost)
	if _, err := service.Bootstrap(context.Background(), "long-enough-password"); err == nil || err.Error() != "write failed" {
		t.Fatalf("err=%v", err)
	}
}

func TestReadPasswordStdinAcceptsSingleLineAndPreservesSpaces(t *testing.T) {
	for _, input := range []string{"  long-enough-password  \n", "  long-enough-password  \r\n", "  long-enough-password  "} {
		got, err := bootstrap.ReadPasswordStdin(strings.NewReader(input))
		if err != nil || got != "  long-enough-password  " {
			t.Fatalf("input=%q got=%q err=%v", input, got, err)
		}
	}
}

func TestReadPasswordStdinRejectsUnsafeInput(t *testing.T) {
	for _, input := range []string{
		"long-enough-password\nsecond-line\n",
		"long-enough-password\x00\n",
		strings.Repeat("x", 1025),
	} {
		if _, err := bootstrap.ReadPasswordStdin(strings.NewReader(input)); !errors.Is(err, bootstrap.ErrInvalidPasswordInput) {
			t.Fatalf("input=%q err=%v", input, err)
		}
	}
}
