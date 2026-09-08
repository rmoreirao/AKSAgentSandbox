package operator

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type managedLeasePredicate struct {
	predicate.Funcs
}

func (managedLeasePredicate) Create(event event.CreateEvent) bool {
	return event.Object.GetLabels()[ManagedLabel] == "true" &&
		event.Object.GetLabels()[SandboxLabel] != ""
}

func (managedLeasePredicate) Update(event event.UpdateEvent) bool {
	return event.ObjectNew.GetLabels()[ManagedLabel] == "true" &&
		event.ObjectNew.GetLabels()[SandboxLabel] != ""
}

func (managedLeasePredicate) Delete(event event.DeleteEvent) bool {
	return event.Object.GetLabels()[ManagedLabel] == "true" &&
		event.Object.GetLabels()[SandboxLabel] != ""
}

func (managedLeasePredicate) Generic(event event.GenericEvent) bool {
	return event.Object.GetLabels()[ManagedLabel] == "true" &&
		event.Object.GetLabels()[SandboxLabel] != ""
}
