package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

var always = corev1.ContainerRestartPolicyAlways

func ctr(name string) corev1.Container {
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
	}}}
}

func nativeCtr(name string) corev1.Container {
	c := ctr(name)
	c.RestartPolicy = &always
	return c
}

func classesOf(cs []Container) map[string]Container {
	out := map[string]Container{}
	for _, c := range cs {
		out[c.Name] = c
	}
	return out
}

func TestClassifyPodContainers(t *testing.T) {
	istioAnn := map[string]string{AnnIstioStatus: `{"containers":["istio-proxy"]}`}
	cases := []struct {
		name  string
		pod   corev1.Pod
		extra []string
		want  map[string]ContainerClass
		mesh  map[string]string
		nat   map[string]bool
	}{
		{
			name: "app + istio-proxy injected (classic)",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: istioAnn}, Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{ctr("istio-init")},
				Containers:     []corev1.Container{ctr("app"), ctr("istio-proxy")},
			}},
			want: map[string]ContainerClass{"app": ClassApp, "istio-proxy": ClassInjectedSidecar, "istio-init": ClassInit},
			mesh: map[string]string{"istio-proxy": MeshIstio},
		},
		{
			name: "app + istio-proxy as native sidecar (Istio >= 1.27)",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: istioAnn}, Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{ctr("istio-init"), nativeCtr("istio-proxy")},
				Containers:     []corev1.Container{ctr("app")},
			}},
			want: map[string]ContainerClass{"app": ClassApp, "istio-proxy": ClassInjectedSidecar, "istio-init": ClassInit},
			mesh: map[string]string{"istio-proxy": MeshIstio},
			nat:  map[string]bool{"istio-proxy": true},
		},
		{
			name: "istio-proxy name without injection annotation: sidecar, but no mesh",
			pod:  corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("app"), ctr("istio-proxy")}}},
			want: map[string]ContainerClass{"app": ClassApp, "istio-proxy": ClassInjectedSidecar},
			mesh: map[string]string{"istio-proxy": ""},
		},
		{
			name: "linkerd proxy with proxy-version annotation",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnLinkerdProxyVersion: "edge-25.1.1"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("app"), ctr("linkerd-proxy")}}},
			want: map[string]ContainerClass{"app": ClassApp, "linkerd-proxy": ClassInjectedSidecar},
			mesh: map[string]string{"linkerd-proxy": MeshLinkerd},
		},
		{
			name: "user native sidecar (log shipper) + plain init",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{ctr("migrate"), nativeCtr("log-shipper")},
				Containers:     []corev1.Container{ctr("app")},
			}},
			want: map[string]ContainerClass{"app": ClassApp, "log-shipper": ClassNativeSidecar, "migrate": ClassInit},
			nat:  map[string]bool{"log-shipper": true},
		},
		{
			name: "ephemeral debug container",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers:          []corev1.Container{ctr("app")},
				EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger"}}},
			}},
			want: map[string]ContainerClass{"app": ClassApp, "debugger": ClassEphemeral},
		},
		{
			name:  "configured glob extends the built-in list",
			pod:   corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{ctr("app"), ctr("acme-agent-v2")}}},
			extra: []string{"acme-agent-*"},
			want:  map[string]ContainerClass{"app": ClassApp, "acme-agent-v2": ClassInjectedSidecar},
		},
		{
			name: "vault-agent, cloud-sql-proxy, queue-proxy, otel by name",
			pod: corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
				ctr("app"), ctr("vault-agent"), ctr("cloud-sql-proxy"), ctr("queue-proxy"), ctr("otc-container"),
			}}},
			want: map[string]ContainerClass{"app": ClassApp, "vault-agent": ClassInjectedSidecar,
				"cloud-sql-proxy": ClassInjectedSidecar, "queue-proxy": ClassInjectedSidecar, "otc-container": ClassInjectedSidecar},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := NewClassifier(c.extra)
			got := classesOf(cl.PodContainers(&c.pod))
			if len(got) != len(c.want) {
				t.Fatalf("got %d containers %v, want %d", len(got), got, len(c.want))
			}
			for n, want := range c.want {
				if got[n].Class != want {
					t.Errorf("%s: class %q, want %q", n, got[n].Class, want)
				}
				if got[n].Native != c.nat[n] {
					t.Errorf("%s: native %v, want %v", n, got[n].Native, c.nat[n])
				}
				if m, ok := c.mesh[n]; ok && got[n].Mesh != m {
					t.Errorf("%s: mesh %q, want %q", n, got[n].Mesh, m)
				}
			}
		})
	}
}

func TestClassResizable(t *testing.T) {
	for cl, want := range map[ContainerClass]bool{
		ClassApp: true, ClassInjectedSidecar: true, ClassNativeSidecar: true, ClassInit: false, ClassEphemeral: false, "": true,
	} {
		if cl.Resizable() != want {
			t.Errorf("%q.Resizable() = %v, want %v", cl, !want, want)
		}
	}
}

// The workload template is the source of truth for what a rollout may patch:
// template containers (and native sidecars declared in the template) are
// InTemplate; an istio-proxy only appears on the running pods.
func TestListWorkloadsTemplateContainers(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{ctr("migrate"), nativeCtr("log-shipper")},
			Containers:     []corev1.Container{ctr("app"), ctr("cloud-sql-proxy")},
		}}},
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{ksvcGVR: "ServiceList"})
	c := NewFromClients(fake.NewSimpleClientset(dep), dyn, nil)
	wls, err := c.ListWorkloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(wls) != 1 {
		t.Fatalf("workloads = %d", len(wls))
	}
	got := classesOf(wls[0].Containers)
	if len(got) != 3 {
		t.Fatalf("containers = %v, want app, cloud-sql-proxy, log-shipper (init excluded)", got)
	}
	for _, n := range []string{"app", "cloud-sql-proxy", "log-shipper"} {
		if !got[n].InTemplate {
			t.Errorf("%s must be InTemplate", n)
		}
	}
	if got["log-shipper"].Class != ClassNativeSidecar || !got["log-shipper"].Native {
		t.Errorf("log-shipper = %+v, want native-sidecar", got["log-shipper"])
	}
	if got["cloud-sql-proxy"].Class != ClassInjectedSidecar {
		t.Errorf("cloud-sql-proxy class = %q (name list)", got["cloud-sql-proxy"].Class)
	}
}

// Running pods carry every container (incl. init and native sidecars) so the
// reconciler can reason about QoS over the whole pod, with live resources read
// from initContainerStatuses too.
func TestSnapshotRunningPodIncludesSidecars(t *testing.T) {
	pod := snapPod("p", func(p *corev1.Pod) {
		p.Annotations = map[string]string{AnnIstioStatus: "{}"}
		p.Spec.InitContainers = []corev1.Container{ctr("istio-init"), nativeCtr("istio-proxy")}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "istio-proxy", Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
		}}}
		p.Spec.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}
	})
	cs := fake.NewSimpleClientset(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}, pod)
	c := NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rp := snap.Running["apps"][0]
	got := classesOf(rp.Containers)
	if got["istio-proxy"].Class != ClassInjectedSidecar || !got["istio-proxy"].Native || got["istio-proxy"].Mesh != MeshIstio {
		t.Fatalf("istio-proxy = %+v", got["istio-proxy"])
	}
	if got["istio-proxy"].CPURequest != 0.05 || got["istio-proxy"].SpecCPURequest != 0.1 {
		t.Fatalf("istio-proxy live/spec cpu = %v/%v, want 0.05/0.1", got["istio-proxy"].CPURequest, got["istio-proxy"].SpecCPURequest)
	}
	if got["istio-init"].Class != ClassInit {
		t.Fatalf("istio-init class = %q", got["istio-init"].Class)
	}
	if rp.PodResources == nil || rp.PodResources.CPURequest != 2 {
		t.Fatalf("pod-level resources = %+v", rp.PodResources)
	}
}

// A native sidecar's OOMKill (initContainerStatuses) feeds the OOM floor too.
func TestSnapshotOOMNativeSidecar(t *testing.T) {
	pod := snapPod("p", func(p *corev1.Pod) {
		sc := nativeCtr("istio-proxy")
		sc.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
		p.Spec.InitContainers = []corev1.Container{sc}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "istio-proxy",
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}}
	})
	cs := fake.NewSimpleClientset(&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "apps",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}, pod)
	c := NewFromClients(cs, dynfake.NewSimpleDynamicClient(runtime.NewScheme()), nil)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o, ok := snap.OOM["apps/Deployment/web/istio-proxy"]
	if !ok || o.MemBytes != 256*1024*1024 {
		t.Fatalf("OOM = %+v ok=%v", o, ok)
	}
}
