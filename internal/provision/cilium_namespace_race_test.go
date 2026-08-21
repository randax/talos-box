package provision

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestMissingDefaultNamespaceOnlyMatchesTheAPIServerStartupRace(t *testing.T) {
	notFound := func(kind, name string) error {
		return apierrors.NewNotFound(schema.GroupResource{Resource: kind}, name)
	}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "kube-system", err: notFound("namespaces", "kube-system"), want: true},
		{name: "default", err: notFound("namespaces", "default"), want: true},
		{name: "wrapped", err: fmt.Errorf(`server-side apply ServiceAccount "cilium": %w`, notFound("namespaces", "kube-node-lease")), want: true},
		{name: "attendee namespace", err: notFound("namespaces", "team-a"), want: false},
		{name: "missing object", err: notFound("deployments", "cilium-operator"), want: false},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "cilium", errors.New("nope")), want: false},
		{name: "unrelated", err: errors.New("connection refused"), want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := missingDefaultNamespace(tt.err); got != tt.want {
				t.Fatalf("missingDefaultNamespace(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

// A substrate-only create followed straight by a CNI reconcile can beat the API
// server to its own default namespaces. That NotFound used to abort the create
// ~49s in and cost a full destroy + recreate; it is a startup transient and
// belongs inside the pass's existing budget (#389).
func TestCiliumApplyRetriesTheMissingDefaultNamespace(t *testing.T) {
	objects, err := decodeObjects([]byte(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: cilium
  namespace: kube-system
`))
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := ciliumTestMapper(objects)
	attempts := 0
	client.PrependReactor("patch", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		if attempts++; attempts < 3 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "kube-system")
		}
		return true, &objects[0], nil
	})

	if err := applyAllAwaitingDefaultNamespaces(context.Background(), client, mapper, objects, time.Millisecond); err != nil {
		t.Fatalf("apply through the namespace race = %v, want it retried to success", err)
	}
	if attempts != 3 {
		t.Fatalf("apply attempts = %d, want the transient retried twice", attempts)
	}
}

// Only that one race is transient. Every other apply failure must still fail
// the pass immediately rather than spin out the provisioning budget.
func TestCiliumApplyDoesNotRetryOtherFailures(t *testing.T) {
	objects, err := decodeObjects([]byte(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: cilium
  namespace: kube-system
`))
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	mapper := ciliumTestMapper(objects)
	attempts := 0
	client.PrependReactor("patch", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "cilium", errors.New("denied"))
	})

	err = applyAllAwaitingDefaultNamespaces(context.Background(), client, mapper, objects, time.Millisecond)
	if err == nil || !apierrors.IsForbidden(errors.Unwrap(err)) {
		t.Fatalf("apply error = %v, want the forbidden failure surfaced immediately", err)
	}
	if attempts != 1 {
		t.Fatalf("apply attempts = %d, want a permanent failure tried once", attempts)
	}
}
