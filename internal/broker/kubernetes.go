package broker

import (
	"context"
	"errors"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type KubernetesTokenReviewer struct {
	Client authenticationclient.TokenReviewInterface
}

func (r KubernetesTokenReviewer) Review(ctx context.Context, token, audience string) (TokenReviewResult, error) {
	if r.Client == nil || token == "" || audience == "" {
		return TokenReviewResult{}, errors.New("token reviewer is not configured")
	}
	review, err := r.Client.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return TokenReviewResult{}, err
	}
	extra := make(map[string][]string, len(review.Status.User.Extra))
	for key, value := range review.Status.User.Extra {
		extra[key] = append([]string(nil), value...)
	}
	return TokenReviewResult{
		Authenticated:     review.Status.Authenticated,
		Audiences:         append([]string(nil), review.Status.Audiences...),
		Username:          review.Status.User.Username,
		ServiceAccountUID: review.Status.User.UID,
		Extra:             extra,
	}, nil
}

type KubernetesBindingReader struct {
	Client client.Reader
}

func (r KubernetesBindingReader) GetServiceAccount(ctx context.Context, namespace, name string) (ServiceAccountBinding, error) {
	var account corev1.ServiceAccount
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &account); err != nil {
		return ServiceAccountBinding{}, err
	}
	return ServiceAccountBinding{UID: string(account.UID), Labels: cloneLabels(account.Labels)}, nil
}

func (r KubernetesBindingReader) GetPod(ctx context.Context, namespace, name string) (PodBinding, error) {
	var pod corev1.Pod
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod); err != nil {
		return PodBinding{}, err
	}
	return PodBinding{
		UID: string(pod.UID), ServiceAccountName: pod.Spec.ServiceAccountName,
		Labels: cloneLabels(pod.Labels),
	}, nil
}

func (r KubernetesBindingReader) GetDevSandbox(ctx context.Context, namespace, name string) (SandboxBinding, error) {
	var sandbox devsandboxv1alpha1.DevSandbox
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sandbox); err != nil {
		return SandboxBinding{}, err
	}
	return SandboxBinding{UID: string(sandbox.UID), OwnerUserID: sandbox.Spec.Owner.GitHubUserID}, nil
}

func cloneLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}
