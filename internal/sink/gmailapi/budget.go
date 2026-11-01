package gmailapi

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/time/rate"
)

type Budget struct {
	mu      sync.Mutex
	user    rate.Limit
	project *rate.Limiter
	targets map[string]*rate.Limiter
}

func NewBudget(userUnits, projectUnits int) (*Budget, error) {
	if userUnits <= 0 || projectUnits <= 0 {
		return nil, errors.New("gmail api: quota budgets must be positive")
	}
	return &Budget{user: rate.Limit(float64(userUnits) / 60),
		project: rate.NewLimiter(rate.Limit(float64(projectUnits)/60), 25), targets: map[string]*rate.Limiter{}}, nil
}

func (b *Budget) Wait(ctx context.Context, target string, units int) error {
	b.mu.Lock()
	limiter := b.targets[target]
	if limiter == nil {
		limiter = rate.NewLimiter(b.user, 25)
		b.targets[target] = limiter
	}
	b.mu.Unlock()
	if err := limiter.WaitN(ctx, units); err != nil {
		return err
	}
	return b.project.WaitN(ctx, units)
}
