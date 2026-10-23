package company

import (
	"context"
	"errors"
	"testing"
)

type repositoryFunc func(context.Context, string) (Company, error)

func (f repositoryFunc) CompanyByID(ctx context.Context, id string) (Company, error) {
	return f(ctx, id)
}

func TestCompanyServiceScopesBeforeReading(t *testing.T) {
	reads := 0
	s := NewService(repositoryFunc(func(_ context.Context, id string) (Company, error) { reads++; return Company{ID: id}, nil }))
	for _, ids := range [][2]string{{"own", "other"}, {"", "other"}, {"", ""}} {
		if _, err := s.Get(context.Background(), ids[0], ids[1]); !errors.Is(err, ErrForbidden) {
			t.Fatal("foreign company was not forbidden")
		}
	}
	if reads != 0 {
		t.Fatal("foreign company lookup reached repository")
	}
	result, err := s.Get(context.Background(), "own", "own")
	if err != nil || result.ID != "own" || reads != 1 {
		t.Fatal("own company lookup failed")
	}
}

func TestCompanyServicePreservesRepositoryErrors(t *testing.T) {
	for _, want := range []error{ErrNotFound, ErrStorage} {
		s := NewService(repositoryFunc(func(context.Context, string) (Company, error) { return Company{}, want }))
		if _, err := s.Get(context.Background(), "own", "own"); !errors.Is(err, want) {
			t.Fatal("repository error lost")
		}
	}
}
