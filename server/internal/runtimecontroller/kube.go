package runtimecontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

// SandboxInfo is the slice of a Sandbox the reconciler needs.
type SandboxInfo struct {
	Name      string
	Mode      string
	Ready     bool
	Labels    map[string]string
	CreatedAt time.Time
}

// SecretInfo is a Secret's data and annotations.
type SecretInfo struct {
	Data        map[string]string
	Annotations map[string]string
}

// Kube is the Kubernetes surface the controller needs.
type Kube interface {
	GetSandbox(ctx context.Context, name string) (*SandboxInfo, error) // nil, nil when absent
	ListManagedSandboxes(ctx context.Context) ([]SandboxInfo, error)
	CreateSandbox(ctx context.Context, s SandboxSpec) error
	SetMode(ctx context.Context, name, mode string) error
	DeleteSandbox(ctx context.Context, name string) error
	GetSecret(ctx context.Context, name string) (*SecretInfo, error) // nil, nil when absent
	PutSecret(ctx context.Context, name string, labels, annotations, data map[string]string) error
	DeleteSecret(ctx context.Context, name string) error
}

// KubeClient implements Kube with client-go.
type KubeClient struct {
	Cfg     Config
	Dynamic dynamic.Interface
	Core    kubernetes.Interface
}

func (k *KubeClient) sandboxes() dynamic.ResourceInterface {
	return k.Dynamic.Resource(sandboxGVR).Namespace(k.Cfg.Namespace)
}

func sandboxInfo(u *unstructured.Unstructured) SandboxInfo {
	mode, _, _ := unstructured.NestedString(u.Object, "spec", "operatingMode")
	if mode == "" {
		mode = ModeRunning
	}
	info := SandboxInfo{Name: u.GetName(), Mode: mode, Labels: u.GetLabels(), CreatedAt: u.GetCreationTimestamp().Time}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" && m["status"] == "True" {
			info.Ready = true
		}
	}
	return info
}

func (k *KubeClient) GetSandbox(ctx context.Context, name string) (*SandboxInfo, error) {
	u, err := k.sandboxes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info := sandboxInfo(u)
	return &info, nil
}

func (k *KubeClient) ListManagedSandboxes(ctx context.Context) ([]SandboxInfo, error) {
	list, err := k.sandboxes().List(ctx, metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue})
	if err != nil {
		return nil, err
	}
	out := make([]SandboxInfo, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, sandboxInfo(&list.Items[i]))
	}
	return out, nil
}

func (k *KubeClient) CreateSandbox(ctx context.Context, s SandboxSpec) error {
	_, err := k.sandboxes().Create(ctx, BuildSandbox(k.Cfg, s), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (k *KubeClient) SetMode(ctx context.Context, name, mode string) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"operatingMode":%q}}`, mode))
	_, err := k.sandboxes().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (k *KubeClient) DeleteSandbox(ctx context.Context, name string) error {
	err := k.sandboxes().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (k *KubeClient) GetSecret(ctx context.Context, name string) (*SecretInfo, error) {
	s, err := k.Core.CoreV1().Secrets(k.Cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data := make(map[string]string, len(s.Data))
	for key, v := range s.Data {
		data[key] = string(v)
	}
	return &SecretInfo{Data: data, Annotations: s.Annotations}, nil
}

func (k *KubeClient) PutSecret(ctx context.Context, name string, labels, annotations, data map[string]string) error {
	secrets := k.Core.CoreV1().Secrets(k.Cfg.Namespace)
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.Cfg.Namespace, Labels: labels, Annotations: annotations},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
	if _, err := secrets.Create(ctx, want, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	_, err := secrets.Update(ctx, want, metav1.UpdateOptions{})
	return err
}

func (k *KubeClient) DeleteSecret(ctx context.Context, name string) error {
	err := k.Core.CoreV1().Secrets(k.Cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
