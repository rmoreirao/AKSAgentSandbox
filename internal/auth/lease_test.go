package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesLeaseLockerSerializesSameReplica(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	locker := &KubernetesLeaseLocker{
		Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
		Namespace: "system", HolderID: "same-broker-pod",
		LeaseDuration: time.Minute, PollInterval: time.Millisecond,
	}
	unlock, err := locker.Lock(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := locker.Lock(waitCtx, "user-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second same-replica lock error = %v", err)
	}
	if err := unlock(context.Background()); err != nil {
		t.Fatal(err)
	}
	unlockAgain, err := locker.Lock(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := unlockAgain(context.Background()); err != nil {
		t.Fatal(err)
	}
}
