package cost

import (
	"errors"
	"sync"
	"testing"
)

// mustKeyed builds a keyed budget or ends the test.
func mustKeyed(t *testing.T, limit Price) *KeyedBudget {
	t.Helper()
	k, err := NewKeyedBudget(limit)
	if err != nil {
		t.Fatalf("NewKeyedBudget(%d): %v", limit, err)
	}
	return k
}

// mustOwnerLimit gives an owner its ceiling or ends the test.
func mustOwnerLimit(t *testing.T, k *KeyedBudget, owner string, limit Price) {
	t.Helper()
	if err := k.SetLimit(owner, limit); err != nil {
		t.Fatalf("SetLimit(%q, %d): %v", owner, limit, err)
	}
}

// mustKeyedRemaining reads an owner's headroom or ends the test.
func mustKeyedRemaining(t *testing.T, k *KeyedBudget, owner string) Price {
	t.Helper()
	remaining, err := k.Remaining(owner)
	if err != nil {
		t.Fatalf("Remaining(%q): %v", owner, err)
	}
	return remaining
}

// TestKeyedBudgetDividesOnePoolBetweenOwners drives two owners through one
// keyed budget and pins the three bounds a consumer relies on. Exhausting
// one owner leaves the other's headroom untouched, and the global ceiling
// refuses when the owners' sum passes it. A released reservation returns
// headroom to the owner that released it and to nobody else.
func TestKeyedBudgetDividesOnePoolBetweenOwners(t *testing.T) {
	tests := []struct {
		name   string
		global Price
		aLimit Price
		bLimit Price
		run    func(t *testing.T, k *KeyedBudget)
		wantA  Price
		wantB  Price
	}{
		{
			name:   "exhausting one owner leaves the other untouched",
			global: 100 * Cent,
			aLimit: 30 * Cent,
			bLimit: 100 * Cent,
			run: func(t *testing.T, k *KeyedBudget) {
				if err := k.Reserve("a", 30*Cent); err != nil {
					t.Fatalf("reserve a 30c: %v", err)
				}
				if err := k.Reserve("b", 40*Cent); err != nil {
					t.Fatalf("reserve b 40c: %v", err)
				}
				if err := k.Reserve("a", Cent); !errors.Is(err, ErrOverBudget) {
					t.Fatalf("reserve a past its ceiling error = %v, want ErrOverBudget", err)
				}
				// The refusal committed nothing, so b can still draw its
				// share of the shared pool even though a is exhausted.
				if err := k.Reserve("b", 30*Cent); err != nil {
					t.Fatalf("reserve b after a was refused: %v", err)
				}
			},
			wantA: 0,
			wantB: 30 * Cent,
		},
		{
			name:   "the global ceiling refuses when the sum passes it",
			global: 100 * Cent,
			aLimit: 100 * Cent,
			bLimit: 100 * Cent,
			run: func(t *testing.T, k *KeyedBudget) {
				if got := k.Limit(); got != 100*Cent {
					t.Fatalf("Limit() = %d, want %d", got, 100*Cent)
				}
				if err := k.Reserve("a", 60*Cent); err != nil {
					t.Fatalf("reserve a 60c: %v", err)
				}
				if err := k.Reserve("b", 40*Cent); err != nil {
					t.Fatalf("reserve b 40c: %v", err)
				}
				// Both owners sit far under their own ceilings, but the
				// shared pool is full, so the next reserve anywhere fails.
				if err := k.Reserve("b", Cent); !errors.Is(err, ErrOverBudget) {
					t.Fatalf("reserve past the global ceiling error = %v, want ErrOverBudget", err)
				}
			},
			wantA: 40 * Cent,
			wantB: 60 * Cent,
		},
		{
			name:   "a released reservation returns headroom to the right owner",
			global: 100 * Cent,
			aLimit: 50 * Cent,
			bLimit: 50 * Cent,
			run: func(t *testing.T, k *KeyedBudget) {
				if err := k.Reserve("a", 30*Cent); err != nil {
					t.Fatalf("reserve a 30c: %v", err)
				}
				if err := k.Reserve("b", 30*Cent); err != nil {
					t.Fatalf("reserve b 30c: %v", err)
				}
				if err := k.Release("a", 30*Cent); err != nil {
					t.Fatalf("release a: %v", err)
				}
				// a is back at its full ceiling and b kept exactly the
				// headroom it had before the release.
				if err := k.Reserve("a", 50*Cent); err != nil {
					t.Fatalf("reserve a after its release: %v", err)
				}
				if err := k.Reserve("b", Cent); err != nil {
					t.Fatalf("reserve b after a's release: %v", err)
				}
			},
			wantA: 0,
			wantB: 19 * Cent,
		},
		{
			name:   "a settle books the actual price on the owner's own account",
			global: 100 * Cent,
			aLimit: 50 * Cent,
			bLimit: 50 * Cent,
			run: func(t *testing.T, k *KeyedBudget) {
				if err := k.Reserve("a", 30*Cent); err != nil {
					t.Fatalf("reserve a 30c: %v", err)
				}
				// The call cost less than the estimate, so only the actual
				// price stays booked on a's account.
				if err := k.Settle("a", 30*Cent, 25*Cent); err != nil {
					t.Fatalf("settle a: %v", err)
				}
				if err := k.Reserve("b", 30*Cent); err != nil {
					t.Fatalf("reserve b 30c: %v", err)
				}
			},
			wantA: 25 * Cent,
			wantB: 20 * Cent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := mustKeyed(t, tt.global)
			mustOwnerLimit(t, k, "a", tt.aLimit)
			mustOwnerLimit(t, k, "b", tt.bLimit)
			tt.run(t, k)
			if got := mustKeyedRemaining(t, k, "a"); got != tt.wantA {
				t.Errorf("remaining a = %s, want %s", got, tt.wantA)
			}
			if got := mustKeyedRemaining(t, k, "b"); got != tt.wantB {
				t.Errorf("remaining b = %s, want %s", got, tt.wantB)
			}
		})
	}
}

// TestKeyedBudgetRefusesUnregisteredOwners pins that every owner-keyed
// operation refuses an owner no ceiling was set for, so a typo cannot spend
// against an unbounded account. It also pins that a ceiling below zero is
// refused where a budget's own limit is.
func TestKeyedBudgetRefusesUnregisteredOwners(t *testing.T) {
	k := mustKeyed(t, Dollar)
	if err := k.Reserve("ghost", Cent); !errors.Is(err, ErrUnknownOwner) {
		t.Errorf("reserve for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	if err := k.Settle("ghost", Cent, Cent); !errors.Is(err, ErrUnknownOwner) {
		t.Errorf("settle for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	if err := k.Release("ghost", Cent); !errors.Is(err, ErrUnknownOwner) {
		t.Errorf("release for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	if _, err := k.Remaining("ghost"); !errors.Is(err, ErrUnknownOwner) {
		t.Errorf("remaining for an unknown owner error = %v, want ErrUnknownOwner", err)
	}
	if err := k.SetLimit("ghost", -Cent); !errors.Is(err, ErrNegativeLimit) {
		t.Errorf("SetLimit below zero error = %v, want ErrNegativeLimit", err)
	}
}

// TestKeyedBudgetSetLimitKeepsTheOwnerAccount pins that replacing a ceiling
// keeps the owner's booked spend and outstanding holds, so an operator can
// raise or lower a share without wiping what the owner already spent.
func TestKeyedBudgetSetLimitKeepsTheOwnerAccount(t *testing.T) {
	k := mustKeyed(t, 100*Cent)
	mustOwnerLimit(t, k, "a", 50*Cent)
	if err := k.Reserve("a", 30*Cent); err != nil {
		t.Fatalf("reserve a 30c: %v", err)
	}
	if err := k.Settle("a", 30*Cent, 20*Cent); err != nil {
		t.Fatalf("settle a: %v", err)
	}
	mustOwnerLimit(t, k, "a", 40*Cent)
	if got := mustKeyedRemaining(t, k, "a"); got != 20*Cent {
		t.Fatalf("remaining a after the new ceiling = %s, want %s", got, 20*Cent)
	}
}

// TestKeyedBudgetConcurrentReservationsNeverOverspend runs concurrent
// reservations for several owners against one keyed budget and pins that
// neither an owner ceiling nor the global one loses an update. The owners'
// ceilings sum past the global one, so the shared pool binds first, and the
// per-owner headroom must still come out exact.
func TestKeyedBudgetConcurrentReservationsNeverOverspend(t *testing.T) {
	const unit = Cent
	const attempts = 900
	const ownerShare = 250
	owners := []string{"a", "b", "c"}
	k := mustKeyed(t, 500*unit)
	for _, owner := range owners {
		mustOwnerLimit(t, k, owner, ownerShare*unit)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted = map[string]int{}
	)
	for i := 0; i < attempts; i++ {
		owner := owners[i%len(owners)]
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			err := k.Reserve(owner, unit)
			switch {
			case err == nil:
				mu.Lock()
				granted[owner]++
				mu.Unlock()
			case !errors.Is(err, ErrOverBudget):
				t.Errorf("Reserve(%q) error = %v, want nil or ErrOverBudget", owner, err)
			}
		}(owner)
	}
	wg.Wait()

	total := 0
	for _, owner := range owners {
		got := granted[owner]
		if got > ownerShare {
			t.Errorf("owner %s granted %d reservations, want at most %d", owner, got, ownerShare)
		}
		if want := Price(ownerShare-got) * unit; mustKeyedRemaining(t, k, owner) != want {
			t.Errorf("owner %s headroom does not match its %d grants", owner, got)
		}
		total += got
	}
	if total != 500 {
		t.Errorf("granted %d reservations in total, want the full global 500", total)
	}
	// The shared pool is exhausted, so a reserve for any owner fails no
	// matter how much of its own share it never used.
	for _, owner := range owners {
		if err := k.Reserve(owner, unit); !errors.Is(err, ErrOverBudget) {
			t.Errorf("reserve for %s past the global pool error = %v, want ErrOverBudget", owner, err)
		}
	}
}
