package gmailapi

import (
	"context"
	"testing"
	"time"
)

func TestNewBudgetRejectsNonPositiveLimits(t *testing.T) {
	for _, limits := range [][2]int{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		if budget, err := NewBudget(limits[0], limits[1]); err == nil || budget != nil {
			t.Fatalf("NewBudget(%d, %d) = %v, %v", limits[0], limits[1], budget, err)
		}
	}
}

func TestBudgetWeightedLimits(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		t.Run(scope, func(t *testing.T) {
			userUnits, projectUnits := 1, 60000000
			if scope == "project" {
				userUnits, projectUnits = projectUnits, userUnits
			}
			budget, err := NewBudget(userUnits, projectUnits)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for _, units := range []int{1, 5, 19} {
				if err := budget.Wait(ctx, "first@gmail.com", units); err != nil {
					t.Fatalf("initial burst of 25 denied at cost %d: %v", units, err)
				}
			}
			target := "first@gmail.com"
			if scope == "project" {
				target = "second@gmail.com"
			}
			if err := budget.Wait(ctx, target, 1); err == nil {
				t.Fatalf("excess %s quota admitted", scope)
			}
			if scope == "user" {
				if err := budget.Wait(ctx, "second@gmail.com", 25); err != nil {
					t.Fatalf("one user exhausted another user's quota: %v", err)
				}
			}
		})
	}
}

func TestBudgetCancellationAndBurst(t *testing.T) {
	budget, err := NewBudget(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := budget.Wait(ctx, "first@gmail.com", 25); err == nil {
		t.Fatal("canceled wait consumed quota")
	}
	live, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := budget.Wait(live, "first@gmail.com", 26); err == nil {
		t.Fatal("cost above burst admitted")
	}
	if err := budget.Wait(live, "first@gmail.com", 25); err != nil {
		t.Fatalf("denied requests consumed the initial burst: %v", err)
	}
	if err := budget.Wait(ctx, "first@gmail.com", 1); err == nil {
		t.Fatal("canceled excess quota admitted")
	}
}
