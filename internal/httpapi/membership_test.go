package httpapi

import (
	"context"
	"smart-credit-backend/internal/company"
)

func (r *creditRepoStub) ResolveCompany(_ context.Context, actor, selected string, _ bool) (string, error) {
	if actor != demoUser().ID {
		return "", company.ErrForbidden
	}
	if selected != "" && selected != ownCompanyID {
		return "", company.ErrForbidden
	}
	return ownCompanyID, nil
}
