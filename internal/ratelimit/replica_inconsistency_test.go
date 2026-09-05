package ratelimit

// Week 9 Slice A regression test: two independent in-memory limiter registries
// (one per gateway replica) each admit well below the configured per-key limit,
// while the cluster as a whole exceeds that limit. This locks in the root cause
// the HTTP-level experiment demonstrates end to end: rate-limit quota state is
// per-process and not shared, so "each replica is under limit" does not imply
// "the cluster is under limit". The HTTP-level evidence lives in
// internal/store/postgres (integration build tag); this test is the fast,
// deterministic registry-level regression that runs in the normal suite.
//
// Clock semantics: a frozen clock makes the outcome exact. With KeyRPM=20
// (burst 20, Week 8 x/time/rate policy) and zero elapsed time, a registry can
// admit exactly 20 requests and then reject every further request. Split 24
// requests deterministically between two registries (12 each), and neither
// replica ever rejects, although the intended cluster limit is 20.
import (
	"testing"
	"time"
)

const (
	replicaLimitRPM = 20 // per-replica, per-key configured limit (burst 20)
	replicaTotal    = 24 // cluster requests, split 12 / 12
	// replicaControlAllowed is what a single registry admits out of 24 under a
	// frozen clock: exactly burst 20, then 4 rejections.
	replicaControlAllowed  = 20
	replicaControlRejected = 4
)

func TestReplicaInconsistencyTwoRegistriesAllowBeyondClusterLimit(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = replicaLimitRPM
	cfg.ProjectRPM = 0 // project scope disabled: isolate the per-virtual-key claim

	registryA := testRegistry(t, cfg, clock)
	registryB := testRegistry(t, cfg, clock)

	allowedA, rejectedA := 0, 0
	allowedB, rejectedB := 0, 0
	for i := 0; i < replicaTotal; i++ {
		var decision Decision
		if i%2 == 0 {
			decision = registryA.Admit(testKeyID, testProjID)
			if decision.Allowed {
				allowedA++
			} else {
				rejectedA++
			}
		} else {
			decision = registryB.Admit(testKeyID, testProjID)
			if decision.Allowed {
				allowedB++
			} else {
				rejectedB++
			}
		}
		if !decision.Allowed {
			t.Fatalf("request %d rejected by %s at frozen clock with %d requests admitted so far", i, decision.BlockingScope, allowedA+allowedB)
		}
	}

	clusterAllowed := allowedA + allowedB
	t.Logf("replica A admitted=%d rejected=%d (limit %d)", allowedA, rejectedA, replicaLimitRPM)
	t.Logf("replica B admitted=%d rejected=%d (limit %d)", allowedB, rejectedB, replicaLimitRPM)
	t.Logf("cluster admitted=%d > intended cluster limit=%d", clusterAllowed, replicaLimitRPM)

	if clusterAllowed != replicaTotal {
		t.Fatalf("cluster admitted = %d, want %d", clusterAllowed, replicaTotal)
	}
	if rejectedA != 0 || rejectedB != 0 {
		t.Fatalf("per-replica rejections A=%d B=%d, want 0/0: local limiter state duplicated across replicas must each stay under its own limit", rejectedA, rejectedB)
	}
	if clusterAllowed <= replicaLimitRPM {
		t.Fatalf("cluster admitted = %d, want > %d to demonstrate the distributed inconsistency", clusterAllowed, replicaLimitRPM)
	}
}

func TestReplicaInconsistencySingleRegistryControlRejectsBeyondBurst(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = replicaLimitRPM
	cfg.ProjectRPM = 0

	registry := testRegistry(t, cfg, clock)

	allowed, rejected := 0, 0
	for i := 0; i < replicaTotal; i++ {
		decision := registry.Admit(testKeyID, testProjID)
		if decision.Allowed {
			allowed++
			continue
		}
		rejected++
		if decision.RetryAfter <= 0 {
			t.Fatalf("rejection %d carried no Retry-After", i)
		}
		if decision.BlockingScope != ScopeVirtualKey {
			t.Fatalf("rejection %d blocking scope = %q, want %q", i, decision.BlockingScope, ScopeVirtualKey)
		}
	}

	t.Logf("single registry admitted=%d rejected=%d (limit %d)", allowed, rejected, replicaLimitRPM)
	if allowed != replicaControlAllowed {
		t.Fatalf("single registry admitted = %d, want %d (frozen clock, burst limit)", allowed, replicaControlAllowed)
	}
	if rejected != replicaControlRejected {
		t.Fatalf("single registry rejected = %d, want %d", rejected, replicaControlRejected)
	}
}
