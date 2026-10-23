// Package leader gates the ACTUATING half of the engine (the reconciler) behind
// a coordination.k8s.io Lease, so the Deployment can run >1 replica for
// availability of the API/UI/webhook while exactly one replica resizes pods.
// Recommendation, the API and the webhook are read-only / stateless and run on
// every replica.
package leader

import (
	"context"
	"log"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/nicacioliveira/k8s-tailor/internal/metrics"
)

// Options configures the election. Zero durations take client-go's customary
// controller defaults (15s / 10s / 2s).
type Options struct {
	Namespace, Name, Identity                 string
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	// StopTimeout bounds how long shutdown waits for work to return before the
	// Lease is released anyway (default 10s). Must fit, together with the
	// servers' shutdown, inside the pod's terminationGracePeriodSeconds.
	StopTimeout time.Duration
}

func (o *Options) defaults() {
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = 15 * time.Second
	}
	if o.RenewDeadline <= 0 {
		o.RenewDeadline = 10 * time.Second
	}
	if o.RetryPeriod <= 0 {
		o.RetryPeriod = 2 * time.Second
	}
	if o.StopTimeout <= 0 {
		o.StopTimeout = 10 * time.Second
	}
}

// Run contends for the Lease until ctx is done. Each time this replica becomes
// leader, work runs with a context that is cancelled when leadership is lost;
// Run waits for work to RETURN before contending again, so this process never
// runs two copies of work.
//
// Shutdown (ctx cancelled) is ordered: (1) work's context is cancelled, (2) Run
// waits for work to return (bounded by StopTimeout), (3) only then the elector
// is cancelled, which RELEASES the Lease (ReleaseOnCancel), and (4) Run returns.
// So a standby takes over in ~RetryPeriod instead of a full LeaseDuration, and
// never while this replica is still finishing a pass. client-go alone would
// release as soon as ctx is done, concurrently with a still-running work — two
// actuators for a moment. Callers must wait for Run to return before exiting.
func Run(ctx context.Context, cs kubernetes.Interface, o Options, work func(ctx context.Context)) error {
	o.defaults()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metaFor(o),
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: o.Identity},
	}
	metrics.Leader.Set(0)
	for ctx.Err() == nil {
		// OnStartedLeading runs on its own goroutine. Track it so we never return
		// (or re-contend) while a previous term's work is still running, and so a
		// callback that fires after le.Run returned doesn't start work at all.
		var (
			mu      sync.Mutex
			stopped bool
			running sync.WaitGroup
		)
		// The elector gets its OWN context: cancelling it releases the Lease, so
		// it is cancelled only after work has stopped (see shutdown below).
		leCtx, leCancel := context.WithCancel(context.WithoutCancel(ctx))
		le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   o.LeaseDuration,
			RenewDeadline:   o.RenewDeadline,
			RetryPeriod:     o.RetryPeriod,
			ReleaseOnCancel: true,
			Name:            o.Name,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(lctx context.Context) {
					mu.Lock()
					if stopped || ctx.Err() != nil {
						mu.Unlock()
						return
					}
					running.Add(1)
					mu.Unlock()
					defer running.Done()
					// Work stops on lost leadership OR on shutdown — the latter
					// while the Lease is still held.
					wctx, wcancel := context.WithCancel(lctx)
					defer wcancel()
					stop := context.AfterFunc(ctx, wcancel)
					defer stop()
					log.Printf("leader election: %s acquired lease %s/%s — actuation enabled", o.Identity, o.Namespace, o.Name)
					metrics.Leader.Set(1)
					work(wctx)
				},
				OnStoppedLeading: func() {
					metrics.Leader.Set(0)
					log.Printf("leader election: %s lost/released lease %s/%s — actuation stopped", o.Identity, o.Namespace, o.Name)
				},
				OnNewLeader: func(id string) {
					if id != o.Identity {
						log.Printf("leader election: current leader is %s (this replica is standby)", id)
					}
				},
			},
		})
		if err != nil {
			leCancel()
			return err
		}
		elected := make(chan struct{})
		go func() {
			defer close(elected)
			le.Run(leCtx) // blocks: acquire → lead until the lease is lost or leCtx is done
		}()
		select {
		case <-elected: // lost the lease (renew failed): work's lctx is already cancelled
		case <-ctx.Done(): // shutdown: stop work FIRST, then release
			mu.Lock()
			stopped = true
			mu.Unlock()
			if !waitTimeout(&running, o.StopTimeout) {
				log.Printf("leader election: work did not stop within %s — releasing lease %s/%s anyway", o.StopTimeout, o.Namespace, o.Name)
			}
			leCancel() // → ReleaseOnCancel: the standby can acquire immediately
			<-elected
			metrics.Leader.Set(0)
			return nil // never block exit on a work that ignored its context
		}
		leCancel()
		mu.Lock()
		stopped = true
		mu.Unlock()
		running.Wait()
		metrics.Leader.Set(0)
		select { // brief pause before re-contending after losing the lease
		case <-ctx.Done():
		case <-time.After(o.RetryPeriod):
		}
	}
	return nil
}

// waitTimeout waits for wg, giving up after d. Reports whether wg finished.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func metaFor(o Options) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: o.Namespace, Name: o.Name}
}
