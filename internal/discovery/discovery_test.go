package discovery

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func dep(ns, name string, podLabels map[string]string, mod ...func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour))},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: podLabels}}},
	}
	for _, m := range mod {
		m(d)
	}
	return d
}

func svc(ns, name string, sel map[string]string, typ corev1.ServiceType) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.ServiceSpec{Selector: sel, Type: typ, Ports: []corev1.ServicePort{{Port: 80}}}}
}

func ing(ns, name, class string, backends ...string) *networkingv1.Ingress {
	in := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if class != "" {
		in.Spec.IngressClassName = &class
	}
	var paths []networkingv1.HTTPIngressPath
	for _, b := range backends {
		paths = append(paths, networkingv1.HTTPIngressPath{Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: b}}})
	}
	in.Spec.Rules = []networkingv1.IngressRule{{Host: "a.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}}}}
	return in
}

// The fake guesses "gatewaies" for kind Gateway, so objects are added under the real resource name.
func gvr(res string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: gwv1.GroupName, Version: "v1", Resource: res}
}

func newGW(t *testing.T, objs ...runtime.Object) *gwfake.Clientset {
	t.Helper()
	c := gwfake.NewSimpleClientset()
	for _, o := range objs {
		var g schema.GroupVersionResource
		ns := ""
		switch x := o.(type) {
		case *gwv1.Gateway:
			g, ns = gvr("gateways"), x.Namespace
		case *gwv1.GatewayClass:
			g = gvr("gatewayclasses")
		case *gwv1.HTTPRoute:
			g, ns = gvr("httproutes"), x.Namespace
		}
		if err := c.Tracker().Create(g, o, ns); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func run(t *testing.T, o Options, kobjs []runtime.Object, gobjs ...runtime.Object) *Result {
	t.Helper()
	o.Now = now
	d := &Discoverer{Kube: kfake.NewSimpleClientset(kobjs...), GW: newGW(t, gobjs...)}
	res, err := d.Discover(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestExposures(t *testing.T) {
	web := map[string]string{"app": "web", "tier": "fe"}
	lb := svc("a", "lb", web, corev1.ServiceTypeLoadBalancer)
	lb.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "x-1.elb.amazonaws.com"}}
	otherNS := svc("b", "web", web, corev1.ServiceTypeClusterIP)
	tests := []struct {
		name  string
		objs  []runtime.Object
		check func(*testing.T, []Exposure)
	}{
		{"LB service", []runtime.Object{dep("a", "web", web), lb}, func(t *testing.T, e []Exposure) {
			if len(e) != 1 || e[0].ExposureKind != "Service" || e[0].LBHostname != "x-1.elb.amazonaws.com" || e[0].Facts.LBHostnames[0] != "x-1.elb.amazonaws.com" || e[0].ServicePorts[0] != 80 {
				t.Fatalf("%+v", e)
			}
		}},
		{"ClusterIP only is not exposed", []runtime.Object{dep("a", "web", web), svc("a", "web", web, corev1.ServiceTypeClusterIP)}, func(t *testing.T, e []Exposure) {
			if len(e) != 0 {
				t.Fatalf("%+v", e)
			}
		}},
		{"service subset selector", []runtime.Object{dep("a", "web", web), svc("a", "s", map[string]string{"app": "web"}, "LoadBalancer")}, func(t *testing.T, e []Exposure) {
			if len(e) != 1 {
				t.Fatal(e)
			}
		}},
		{"service selector not subset", []runtime.Object{dep("a", "web", web), svc("a", "s", map[string]string{"app": "web", "x": "y"}, "LoadBalancer")}, func(t *testing.T, e []Exposure) {
			if len(e) != 0 {
				t.Fatal(e)
			}
		}},
		{"empty selector never matches", []runtime.Object{dep("a", "web", web), svc("a", "s", nil, "LoadBalancer")}, func(t *testing.T, e []Exposure) {
			if len(e) != 0 {
				t.Fatal(e)
			}
		}},
		{"service in another namespace", []runtime.Object{dep("a", "web", web), otherNS, ing("b", "i", "traefik", "web")}, func(t *testing.T, e []Exposure) {
			if len(e) != 0 {
				t.Fatal(e)
			}
		}},
		{"ingress rule backend", []runtime.Object{dep("a", "web", web), svc("a", "s", web, "ClusterIP"), ing("a", "i", "traefik", "s", "s")}, func(t *testing.T, e []Exposure) {
			if len(e) != 1 || e[0].Ingress != "i" || e[0].Facts.IngressClass != "traefik" || e[0].Hosts[0] != "a.example.com" {
				t.Fatalf("%+v", e)
			}
		}},
		{"ingress default backend", []runtime.Object{dep("a", "web", web), svc("a", "s", web, "ClusterIP"), func() runtime.Object {
			i := ing("a", "i", "nginx")
			i.Spec.Rules = nil
			i.Spec.DefaultBackend = &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "s"}}
			return i
		}()}, func(t *testing.T, e []Exposure) {
			if len(e) != 1 || e[0].Facts.IngressClass != "nginx" {
				t.Fatalf("%+v", e)
			}
		}},
		{"ingress for a different service", []runtime.Object{dep("a", "web", web), svc("a", "s", web, "ClusterIP"), ing("a", "i", "nginx", "other")}, func(t *testing.T, e []Exposure) {
			if len(e) != 0 {
				t.Fatal(e)
			}
		}},
		{"legacy class annotation", []runtime.Object{dep("a", "web", web), svc("a", "s", web, "ClusterIP"), func() runtime.Object {
			i := ing("a", "i", "", "s")
			i.Annotations = map[string]string{"kubernetes.io/ingress.class": "kong"}
			return i
		}()}, func(t *testing.T, e []Exposure) {
			if len(e) != 1 || e[0].Facts.IngressClass != "kong" {
				t.Fatalf("%+v", e)
			}
		}},
		{"default IngressClass", []runtime.Object{dep("a", "web", web), svc("a", "s", web, "ClusterIP"), ing("a", "i", "", "s"),
			&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "traefik", Annotations: map[string]string{"ingressclass.kubernetes.io/is-default-class": "true"}}}}, func(t *testing.T, e []Exposure) {
			if len(e) != 1 || e[0].Facts.IngressClass != "traefik" {
				t.Fatalf("%+v", e)
			}
		}},
		{"ingress and LB are two exposures", []runtime.Object{dep("a", "web", web), svc("a", "s", web, "LoadBalancer"), ing("a", "i", "nginx", "s")}, func(t *testing.T, e []Exposure) {
			if len(e) != 2 {
				t.Fatalf("%+v", e)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := run(t, Options{}, tt.objs)
			if len(res.Workloads) != 1 {
				t.Fatalf("workloads=%d", len(res.Workloads))
			}
			tt.check(t, res.Workloads[0].Exposures)
		})
	}
}

func TestFiltering(t *testing.T) {
	l := map[string]string{"app": "x"}
	objs := []runtime.Object{
		dep("keep", "ok", l),
		dep("kube-system", "sys", l),
		dep("other", "elsewhere", l),
		dep("keep", "ignored", l, func(d *appsv1.Deployment) { d.Annotations = map[string]string{IgnoreAnnotation: "true"} }),
		dep("keep", "notignored", l, func(d *appsv1.Deployment) { d.Annotations = map[string]string{IgnoreAnnotation: "false"} }),
		dep("keep", "young", l, func(d *appsv1.Deployment) { d.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour)) }),
		dep("keep", "labelled", map[string]string{"app": "x"}, func(d *appsv1.Deployment) { d.Labels = map[string]string{"opt": "in"} }),
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "keep", Name: "db", CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour))}},
	}
	names := func(r *Result) map[string]bool {
		m := map[string]bool{}
		for _, w := range r.Workloads {
			m[w.Kind+"/"+w.Namespace+"/"+w.Name] = true
		}
		return m
	}
	t.Run("include exclude ignore age", func(t *testing.T) {
		r := run(t, Options{Include: []string{"keep", "kube-system"}, Exclude: []string{"kube-system"}, MinAge: 24 * time.Hour}, objs)
		got := names(r)
		for _, w := range []string{"Deployment/keep/ok", "Deployment/keep/notignored", "Deployment/keep/labelled", "StatefulSet/keep/db"} {
			if !got[w] {
				t.Errorf("missing %s", w)
			}
		}
		if len(got) != 4 || r.Ignored != 1 || r.Young != 1 {
			t.Fatalf("%v ignored=%d young=%d", got, r.Ignored, r.Young)
		}
	})
	t.Run("label selector", func(t *testing.T) {
		r := run(t, Options{Selector: "opt=in"}, objs)
		if got := names(r); len(got) != 1 || !got["Deployment/keep/labelled"] {
			t.Fatal(got)
		}
	})
	t.Run("bad selector", func(t *testing.T) {
		d := &Discoverer{Kube: kfake.NewSimpleClientset(), GW: gwfake.NewSimpleClientset()}
		if _, err := d.Discover(context.Background(), Options{Selector: "!!"}); err == nil {
			t.Fatal("want error")
		}
	})
}

func route(ns, name string, parents []gwv1.ParentReference, refs ...gwv1.BackendObjectReference) *gwv1.HTTPRoute {
	r := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	r.Spec.ParentRefs = parents
	r.Spec.Hostnames = []gwv1.Hostname{"r.example.com"}
	var br []gwv1.HTTPBackendRef
	for _, ref := range refs {
		br = append(br, gwv1.HTTPBackendRef{BackendRef: gwv1.BackendRef{BackendObjectReference: ref}})
	}
	r.Spec.Rules = []gwv1.HTTPRouteRule{{BackendRefs: br}}
	return r
}

func gwObjs() []runtime.Object {
	return []runtime.Object{
		&gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "traefik"}, Spec: gwv1.GatewayClassSpec{ControllerName: "traefik.io/gateway-controller"}},
		&gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "gw", Name: "main"}, Spec: gwv1.GatewaySpec{GatewayClassName: "traefik"},
			Status: gwv1.GatewayStatus{Addresses: []gwv1.GatewayStatusAddress{{Type: ptr(gwv1.IPAddressType), Value: "10.0.0.1"}}}},
	}
}

func TestHTTPRoutes(t *testing.T) {
	web := map[string]string{"app": "web"}
	base := func() []runtime.Object {
		return []runtime.Object{dep("a", "web", web), svc("a", "s", web, "ClusterIP")}
	}
	parent := []gwv1.ParentReference{{Name: "main", Namespace: ptr(gwv1.Namespace("gw"))}}

	t.Run("same namespace, class resolved", func(t *testing.T) {
		g := append(gwObjs(), route("a", "r", parent, gwv1.BackendObjectReference{Name: "s"}))
		e := run(t, Options{}, base(), g...).Workloads[0].Exposures
		if len(e) != 1 || e[0].Route != "r" || e[0].RouteNamespace != "a" || e[0].Gateway != "main" || e[0].GatewayNamespace != "gw" ||
			e[0].Facts.GatewayClass != "traefik" || e[0].Facts.GatewayController != "traefik.io/gateway-controller" || e[0].LBIP != "10.0.0.1" {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("gateway defaults to route namespace", func(t *testing.T) {
		g := gwObjs()
		g[1].(*gwv1.Gateway).Namespace = "a"
		g = append(g, route("a", "r", []gwv1.ParentReference{{Name: "main"}}, gwv1.BackendObjectReference{Name: "s"}))
		e := run(t, Options{}, base(), g...).Workloads[0].Exposures
		if len(e) != 1 || e[0].Facts.GatewayClass != "traefik" {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("cross-namespace route, grant ignored", func(t *testing.T) {
		g := append(gwObjs(), route("gw", "r", parent, gwv1.BackendObjectReference{Name: "s", Namespace: ptr(gwv1.Namespace("a"))}))
		e := run(t, Options{}, base(), g...).Workloads[0].Exposures
		if len(e) != 1 || e[0].Route != "r" {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("same name other namespace is not ours", func(t *testing.T) {
		g := append(gwObjs(), route("gw", "r", parent, gwv1.BackendObjectReference{Name: "s"}))
		if e := run(t, Options{}, base(), g...).Workloads[0].Exposures; len(e) != 0 {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("non-Service backends ignored", func(t *testing.T) {
		g := append(gwObjs(), route("a", "r", parent,
			gwv1.BackendObjectReference{Name: "s", Kind: ptr(gwv1.Kind("ServiceImport"))},
			gwv1.BackendObjectReference{Name: "s", Group: ptr(gwv1.Group("example.com"))}))
		if e := run(t, Options{}, base(), g...).Workloads[0].Exposures; len(e) != 0 {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("unresolvable gateway leaves class empty", func(t *testing.T) {
		g := []runtime.Object{route("a", "r", []gwv1.ParentReference{{Name: "ghost"}}, gwv1.BackendObjectReference{Name: "s"})}
		e := run(t, Options{}, base(), g...).Workloads[0].Exposures
		if len(e) != 1 || e[0].Facts.GatewayClass != "" || e[0].Facts.GatewayController != "" {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("one exposure per parent", func(t *testing.T) {
		g := append(gwObjs(), route("a", "r", []gwv1.ParentReference{{Name: "main", Namespace: ptr(gwv1.Namespace("gw"))}, {Name: "other"}}, gwv1.BackendObjectReference{Name: "s"}))
		if e := run(t, Options{}, base(), g...).Workloads[0].Exposures; len(e) != 2 {
			t.Fatalf("%+v", e)
		}
	})
}

func TestMissingGatewayCRDs(t *testing.T) {
	web := map[string]string{"app": "web"}
	gw := gwfake.NewSimpleClientset()
	gw.PrependReactor("list", "httproutes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "gateway.networking.k8s.io", Resource: "httproutes"}, "")
	})
	d := &Discoverer{Kube: kfake.NewSimpleClientset(dep("a", "web", web), svc("a", "s", web, "LoadBalancer")), GW: gw}
	res, err := d.Discover(context.Background(), Options{Now: now})
	if err != nil || len(res.Workloads[0].Exposures) != 1 {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

func TestGatewayListError(t *testing.T) {
	gw := gwfake.NewSimpleClientset()
	gw.PrependReactor("list", "httproutes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "httproutes"}, "", nil)
	})
	d := &Discoverer{Kube: kfake.NewSimpleClientset(dep("a", "web", map[string]string{"a": "b"})), GW: gw}
	if _, err := d.Discover(context.Background(), Options{Now: now}); err == nil {
		t.Fatal("forbidden must be an error")
	}
}
