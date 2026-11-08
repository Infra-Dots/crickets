// Package discovery turns cluster objects into workloads and their traffic exposures.
package discovery

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	"github.com/Infra-Dots/crickets/internal/providers"
)

const IgnoreAnnotation = "crickets.io/ignore"

type Options struct {
	Include, Exclude []string
	Selector         string
	MinAge           time.Duration
	Now              time.Time
}

// Exposure is the template data plus the facts used to pick a profile.
type Exposure struct {
	providers.Exposure
	Facts providers.Facts
}

type Workload struct {
	Kind, Namespace, Name string
	Created               time.Time
	Labels, Annotations   map[string]string
	Exposures             []Exposure
}

// Result counts what was left out before exposures were considered.
type Result struct {
	Workloads []Workload
	Ignored   int
	Young     int
}

type Discoverer struct {
	Kube kubernetes.Interface
	GW   gwclient.Interface
}

type key struct{ ns, name string }

func (d *Discoverer) Discover(ctx context.Context, o Options) (*Result, error) {
	sel, err := labels.Parse(o.Selector)
	if err != nil {
		return nil, fmt.Errorf("selector: %w", err)
	}
	lo := metav1.ListOptions{LabelSelector: sel.String()}

	deps, err := d.Kube.AppsV1().Deployments("").List(ctx, lo)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	sts, err := d.Kube.AppsV1().StatefulSets("").List(ctx, lo)
	if err != nil {
		return nil, fmt.Errorf("list statefulsets: %w", err)
	}
	type wl struct {
		Workload
		podLabels map[string]string
	}
	var all []wl
	add := func(kind string, m metav1.ObjectMeta, tmpl corev1.PodTemplateSpec) {
		all = append(all, wl{Workload{kind, m.Namespace, m.Name, m.CreationTimestamp.Time, m.Labels, m.Annotations, nil}, tmpl.Labels})
	}
	for _, x := range deps.Items {
		add("Deployment", x.ObjectMeta, x.Spec.Template)
	}
	for _, x := range sts.Items {
		add("StatefulSet", x.ObjectMeta, x.Spec.Template)
	}

	res := &Result{}
	var cands []wl
	for _, w := range all {
		switch {
		case !nsAllowed(w.Namespace, o):
		case w.Annotations[IgnoreAnnotation] == "true":
			res.Ignored++
		case o.Now.Sub(w.Created) < o.MinAge:
			res.Young++
		default:
			cands = append(cands, w)
		}
	}
	if len(cands) == 0 {
		return res, nil
	}

	idx, err := d.index(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range cands {
		for _, svc := range idx.services[w.Namespace] {
			if len(svc.Spec.Selector) == 0 || !labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(w.podLabels)) {
				continue
			}
			base := providers.Exposure{Namespace: w.Namespace, Kind: w.Kind, Workload: w.Name, Service: svc.Name}
			for _, p := range svc.Spec.Ports {
				base.ServicePorts = append(base.ServicePorts, p.Port)
			}
			w.Exposures = append(w.Exposures, idx.exposures(svc, base)...)
		}
		res.Workloads = append(res.Workloads, w.Workload)
	}
	return res, nil
}

func nsAllowed(ns string, o Options) bool {
	if len(o.Include) > 0 && !slices.Contains(o.Include, ns) {
		return false
	}
	return !slices.Contains(o.Exclude, ns)
}

type index struct {
	services       map[string][]corev1.Service
	ingresses      map[key][]networkingv1.Ingress // by backend service
	routes         map[key][]gwv1.HTTPRoute
	gateways       map[key]gwv1.Gateway
	controllers    map[string]string // GatewayClass name -> controllerName
	defaultIngress string
}

func (d *Discoverer) index(ctx context.Context) (*index, error) {
	ix := &index{services: map[string][]corev1.Service{}, ingresses: map[key][]networkingv1.Ingress{},
		routes: map[key][]gwv1.HTTPRoute{}, gateways: map[key]gwv1.Gateway{}, controllers: map[string]string{}}

	svcs, err := d.Kube.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	for _, s := range svcs.Items {
		ix.services[s.Namespace] = append(ix.services[s.Namespace], s)
	}

	ings, err := d.Kube.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ingresses: %w", err)
	}
	for _, in := range ings.Items {
		seen := map[string]bool{}
		note := func(b *networkingv1.IngressBackend) {
			if b != nil && b.Service != nil && !seen[b.Service.Name] {
				seen[b.Service.Name] = true
				k := key{in.Namespace, b.Service.Name}
				ix.ingresses[k] = append(ix.ingresses[k], in)
			}
		}
		note(in.Spec.DefaultBackend)
		for _, r := range in.Spec.Rules {
			if r.HTTP != nil {
				for _, p := range r.HTTP.Paths {
					note(&p.Backend)
				}
			}
		}
	}
	classes, err := d.Kube.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ingressclasses: %w", err)
	}
	for _, c := range classes.Items {
		if c.Annotations["ingressclass.kubernetes.io/is-default-class"] == "true" {
			ix.defaultIngress = c.Name
		}
	}

	return ix, d.loadGateway(ctx, ix)
}

func (d *Discoverer) loadGateway(ctx context.Context, ix *index) error {
	routes, err := d.GW.GatewayV1().HTTPRoutes("").List(ctx, metav1.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil // Gateway API CRDs not installed
		}
		return fmt.Errorf("list httproutes: %w", err)
	}
	if len(routes.Items) == 0 {
		return nil
	}
	for _, r := range routes.Items {
		seen := map[key]bool{}
		for _, rule := range r.Spec.Rules {
			for _, b := range rule.BackendRefs {
				ref := b.BackendRef
				if (ref.Group != nil && *ref.Group != "") || (ref.Kind != nil && *ref.Kind != "Service") {
					continue
				}
				ns := r.Namespace
				if ref.Namespace != nil {
					ns = string(*ref.Namespace) // ReferenceGrant is not checked: the route still routes
				}
				if k := (key{ns, string(ref.Name)}); !seen[k] {
					seen[k] = true
					ix.routes[k] = append(ix.routes[k], r)
				}
			}
		}
	}
	gws, err := d.GW.GatewayV1().Gateways("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list gateways: %w", err)
	}
	for _, g := range gws.Items {
		ix.gateways[key{g.Namespace, g.Name}] = g
	}
	gcs, err := d.GW.GatewayV1().GatewayClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list gatewayclasses: %w", err)
	}
	for _, c := range gcs.Items {
		ix.controllers[c.Name] = string(c.Spec.ControllerName)
	}
	return nil
}

func (ix *index) exposures(svc corev1.Service, base providers.Exposure) []Exposure {
	var out []Exposure
	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		e := Exposure{Exposure: base}
		e.ExposureKind, e.Annotations = providers.KindService, svc.Annotations
		e.Facts = providers.Facts{Kind: providers.KindService, Annotations: svc.Annotations}
		for _, i := range svc.Status.LoadBalancer.Ingress {
			e.addLB(i.Hostname, i.IP)
		}
		out = append(out, e)
	}
	for _, in := range ix.ingresses[key{svc.Namespace, svc.Name}] {
		e := Exposure{Exposure: base}
		e.ExposureKind, e.Ingress, e.Annotations = providers.KindIngress, in.Name, in.Annotations
		class := in.Annotations["kubernetes.io/ingress.class"]
		if in.Spec.IngressClassName != nil {
			class = *in.Spec.IngressClassName
		}
		if class == "" {
			class = ix.defaultIngress
		}
		e.Facts = providers.Facts{Kind: providers.KindIngress, IngressClass: class, Annotations: in.Annotations}
		for _, r := range in.Spec.Rules {
			if r.Host != "" {
				e.Hosts = append(e.Hosts, r.Host)
			}
		}
		for _, i := range in.Status.LoadBalancer.Ingress {
			e.addLB(i.Hostname, i.IP)
		}
		out = append(out, e)
	}
	for _, r := range ix.routes[key{svc.Namespace, svc.Name}] {
		parents := r.Spec.ParentRefs
		if len(parents) == 0 {
			parents = []gwv1.ParentReference{{}}
		}
		for _, p := range parents {
			e := Exposure{Exposure: base}
			e.ExposureKind, e.Route, e.Annotations = providers.KindHTTPRoute, r.Name, r.Annotations
			e.Facts = providers.Facts{Kind: providers.KindHTTPRoute, Annotations: r.Annotations}
			for _, h := range r.Spec.Hostnames {
				e.Hosts = append(e.Hosts, string(h))
			}
			if (p.Kind == nil || *p.Kind == "Gateway") && (p.Group == nil || *p.Group == gwv1.GroupName) && p.Name != "" {
				e.Gateway, e.GatewayNamespace = string(p.Name), r.Namespace
				if p.Namespace != nil {
					e.GatewayNamespace = string(*p.Namespace)
				}
				if g, ok := ix.gateways[key{e.GatewayNamespace, e.Gateway}]; ok {
					e.Facts.GatewayClass = string(g.Spec.GatewayClassName)
					e.Facts.GatewayController = ix.controllers[e.Facts.GatewayClass]
					for _, a := range g.Status.Addresses {
						if a.Type != nil && *a.Type == gwv1.HostnameAddressType {
							e.addLB(a.Value, "")
						} else {
							e.addLB("", a.Value)
						}
					}
				}
			}
			out = append(out, e)
		}
	}
	return out
}

func (e *Exposure) addLB(host, ip string) {
	if host != "" {
		e.Facts.LBHostnames = append(e.Facts.LBHostnames, host)
		if e.LBHostname == "" {
			e.LBHostname = host
		}
	}
	if ip != "" && e.LBIP == "" {
		e.LBIP = ip
	}
}
