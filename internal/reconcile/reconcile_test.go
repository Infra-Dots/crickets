package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	gwfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"

	"github.com/Infra-Dots/crickets/internal/config"
	"github.com/Infra-Dots/crickets/internal/discovery"
	"github.com/Infra-Dots/crickets/internal/prom"
	"github.com/Infra-Dots/crickets/internal/providers"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

const window = 2 * time.Hour

type fakeProm struct {
	mu      sync.Mutex
	vals    map[string]float64 // query -> value; missing => empty
	healthy map[string]bool    // health expr -> has data now
	pastOK  map[string]bool    // health expr -> has data at now-window
	err     error
	calls   map[string]int
}

func (f *fakeProm) Query(_ context.Context, expr string, at time.Time) (float64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[expr]++
	if f.err != nil {
		return 0, false, f.err
	}
	if strings.HasPrefix(expr, "up") {
		ok := f.healthy[expr]
		if at.Before(now) {
			ok = ok && f.pastOK[expr]
		}
		return 1, !ok, nil
	}
	v, ok := f.vals[expr]
	return v, !ok, nil
}

type call struct {
	kind, ns, name string
	labels, ann    map[string]string
}
type fakeWriter struct {
	calls []call
	err   error
}

func (w *fakeWriter) Apply(_ context.Context, kind, ns, name string, l, a map[string]string) error {
	w.calls = append(w.calls, call{kind, ns, name, l, a})
	return w.err
}

func profs(t *testing.T) *providers.Set {
	t.Helper()
	mk := func(name, class, health string) providers.Profile {
		return providers.Profile{Name: name, Supported: true, Query: "q:" + name + ":{{.Service}}:{{.Window}}", Health: health,
			Match: providers.Match{Kinds: []string{"Ingress"}, IngressClasses: []string{class}}}
	}
	s, err := providers.Load([]providers.Profile{mk("a", "a", "up_a"), mk("b", "b", "up_b")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type wl struct {
	name string
	svcs map[string]string // service name -> ingress class ("" = no ingress)
	mod  func(*appsv1.Deployment)
}

func objects(ws ...wl) []runtime.Object {
	var out []runtime.Object
	for _, w := range ws {
		lbl := map[string]string{"app": w.name}
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: w.name, CreationTimestamp: metav1.NewTime(now.Add(-100 * time.Hour))},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}}}}
		if w.mod != nil {
			w.mod(d)
		}
		out = append(out, d)
		i := 0
		for svc, class := range w.svcs {
			i++
			out = append(out, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: svc}, Spec: corev1.ServiceSpec{Selector: lbl}})
			if class == "" {
				continue
			}
			c := class
			out = append(out, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ing-" + svc},
				Spec: networkingv1.IngressSpec{IngressClassName: &c, DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: svc}}}})
		}
	}
	return out
}

func newRec(t *testing.T, p prom.Querier, w Writer, dry bool, objs ...runtime.Object) *Reconciler {
	t.Helper()
	return &Reconciler{
		Disc:      &discovery.Discoverer{Kube: kfake.NewSimpleClientset(objs...), GW: gwfake.NewSimpleClientset()},
		Providers: profs(t), Prom: p, Writer: w, Now: func() time.Time { return now },
		Cfg: &config.Config{Window: window, MinAge: time.Hour, Threshold: 0, DryRun: dry, Label: "crickets.io/idle", Namespaces: config.Namespaces{Exclude: []string{"kube-system"}}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func q(provider, svc string) string { return "q:" + provider + ":" + svc + ":7200s" }

func TestDecisionMatrix(t *testing.T) {
	healthyA := func() *fakeProm {
		return &fakeProm{vals: map[string]float64{}, healthy: map[string]bool{"up_a": true, "up_b": true}, pastOK: map[string]bool{"up_a": true, "up_b": true}}
	}
	tests := []struct {
		name      string
		w         wl
		prom      func() *fakeProm
		verdict   Verdict
		labelCall bool // a label write is expected
		counts    func(Summary) bool
	}{
		{"idle: empty result from healthy source", wl{name: "w", svcs: map[string]string{"s": "a"}}, healthyA, Idle, true, func(s Summary) bool { return s.Idle == 1 }},
		{"idle: explicit zero", wl{name: "w", svcs: map[string]string{"s": "a"}}, func() *fakeProm {
			p := healthyA()
			p.vals[q("a", "s")] = 0
			return p
		}, Idle, true, func(s Summary) bool { return s.Idle == 1 }},
		{"active: traffic", wl{name: "w", svcs: map[string]string{"s": "a"}}, func() *fakeProm {
			p := healthyA()
			p.vals[q("a", "s")] = 5
			return p
		}, Active, false, func(s Summary) bool { return s.Active == 1 }},
		{"unknown: no profile", wl{name: "w", svcs: map[string]string{"s": "zzz"}}, healthyA, Unknown, false, func(s Summary) bool { return s.Unknown == 1 }},
		{"unknown: unhealthy now", wl{name: "w", svcs: map[string]string{"s": "a"}}, func() *fakeProm {
			p := healthyA()
			p.healthy["up_a"] = false
			return p
		}, Unknown, false, func(s Summary) bool { return s.Unknown == 1 }},
		{"unknown: short retention", wl{name: "w", svcs: map[string]string{"s": "a"}}, func() *fakeProm {
			p := healthyA()
			p.pastOK["up_a"] = false
			return p
		}, Unknown, false, func(s Summary) bool { return s.Unknown == 1 }},
		{"skip: unexposed", wl{name: "w", svcs: map[string]string{"s": ""}}, healthyA, "", false, func(s Summary) bool { return s.Unexposed == 1 && s.Workloads == 0 }},
		{"skip: ignored", wl{name: "w", svcs: map[string]string{"s": "a"}, mod: func(d *appsv1.Deployment) { d.Annotations = map[string]string{"crickets.io/ignore": "true"} }}, healthyA, "", false, func(s Summary) bool { return s.Ignored == 1 && s.Workloads == 0 }},
		{"skip: young", wl{name: "w", svcs: map[string]string{"s": "a"}, mod: func(d *appsv1.Deployment) { d.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute)) }}, healthyA, "", false, func(s Summary) bool { return s.Young == 1 && s.Workloads == 0 }},
		{"mixed: one idle, one busy => active", wl{name: "w", svcs: map[string]string{"s1": "a", "s2": "b"}}, func() *fakeProm {
			p := healthyA()
			p.vals[q("b", "s2")] = 1
			return p
		}, Active, false, func(s Summary) bool { return s.Active == 1 }},
		{"mixed: one idle, one unknown => unknown", wl{name: "w", svcs: map[string]string{"s1": "a", "s2": "zzz"}}, healthyA, Unknown, false, func(s Summary) bool { return s.Unknown == 1 }},
		{"mixed: both idle", wl{name: "w", svcs: map[string]string{"s1": "a", "s2": "b"}}, healthyA, Idle, true, func(s Summary) bool { return s.Idle == 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &fakeWriter{}
			r := newRec(t, tt.prom(), w, false, objects(tt.w)...)
			s, err := r.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !tt.counts(s) {
				t.Fatalf("summary %+v", s)
			}
			if tt.labelCall {
				if len(w.calls) != 1 || w.calls[0].labels["crickets.io/idle"] != "true" {
					t.Fatalf("calls %+v", w.calls)
				}
			} else if len(w.calls) != 0 {
				t.Fatalf("unexpected writes %+v", w.calls)
			}
		})
	}
}

func TestEvidenceAndStableSince(t *testing.T) {
	p := &fakeProm{vals: map[string]float64{}, healthy: map[string]bool{"up_a": true}, pastOK: map[string]bool{"up_a": true}}
	w := &fakeWriter{}
	r := newRec(t, p, w, false, objects(wl{name: "w", svcs: map[string]string{"s": "a"}})...)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ev Evidence
	if err := json.Unmarshal([]byte(w.calls[0].ann["crickets.io/idle-evidence"]), &ev); err != nil {
		t.Fatal(err)
	}
	if !ev.Since.Equal(now) || ev.Window != "7200s" || len(ev.Exposures) != 1 || ev.Exposures[0].Provider != "a" ||
		ev.Exposures[0].Kind != "Ingress" || ev.Exposures[0].Name != "ing-s" || ev.Exposures[0].Query != q("a", "s") {
		t.Fatalf("%+v", ev)
	}

	// Second pass an hour later against the labeled object: same since, no change => no write.
	later := now.Add(time.Hour)
	labeled := objects(wl{name: "w", svcs: map[string]string{"s": "a"}, mod: func(d *appsv1.Deployment) {
		d.Labels = map[string]string{"crickets.io/idle": "true"}
		d.Annotations = w.calls[0].ann
	}})
	w2 := &fakeWriter{}
	r2 := newRec(t, p, w2, false, labeled...)
	r2.Now = func() time.Time { return later }
	if _, err := r2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The query and evidence are identical, so nothing to write.
	if len(w2.calls) != 0 {
		t.Fatalf("expected no-op, got %+v", w2.calls)
	}

	// Value changes but stays under threshold: rewrite with the original since.
	r2.Cfg.Threshold = 10
	p.vals[q("a", "s")] = 3
	if _, err := r2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w2.calls) != 1 {
		t.Fatalf("calls %+v", w2.calls)
	}
	var ev2 Evidence
	_ = json.Unmarshal([]byte(w2.calls[0].ann["crickets.io/idle-evidence"]), &ev2)
	if !ev2.Since.Equal(now) || ev2.Exposures[0].Value != 3 {
		t.Fatalf("since moved or value missing: %+v", ev2)
	}
}

func TestRemoval(t *testing.T) {
	p := &fakeProm{vals: map[string]float64{q("a", "s"): 9}, healthy: map[string]bool{"up_a": true}, pastOK: map[string]bool{"up_a": true}}
	w := &fakeWriter{}
	labeled := objects(wl{name: "w", svcs: map[string]string{"s": "a"}, mod: func(d *appsv1.Deployment) { d.Labels = map[string]string{"crickets.io/idle": "true"} }})
	s, err := newRec(t, p, w, false, labeled...).Run(context.Background())
	if err != nil || s.Unlabeled != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	if len(w.calls) != 1 || w.calls[0].labels != nil || w.calls[0].ann != nil || w.calls[0].name != "w" {
		t.Fatalf("%+v", w.calls)
	}
}

func TestUnknownKeepsExistingLabel(t *testing.T) {
	p := &fakeProm{healthy: map[string]bool{}, pastOK: map[string]bool{}}
	w := &fakeWriter{}
	labeled := objects(wl{name: "w", svcs: map[string]string{"s": "a"}, mod: func(d *appsv1.Deployment) { d.Labels = map[string]string{"crickets.io/idle": "true"} }})
	if _, err := newRec(t, p, w, false, labeled...).Run(context.Background()); err != nil || len(w.calls) != 0 {
		t.Fatalf("%v %+v", err, w.calls)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	p := &fakeProm{vals: map[string]float64{q("a", "busy"): 9}, healthy: map[string]bool{"up_a": true}, pastOK: map[string]bool{"up_a": true}}
	w := &fakeWriter{}
	objs := objects(
		wl{name: "idle", svcs: map[string]string{"s": "a"}},
		wl{name: "back", svcs: map[string]string{"busy": "a"}, mod: func(d *appsv1.Deployment) { d.Labels = map[string]string{"crickets.io/idle": "true"} }},
	)
	s, err := newRec(t, p, w, true, objs...).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 0 || s.Labeled != 1 || s.Unlabeled != 1 {
		t.Fatalf("%+v %+v", s, w.calls)
	}
}

func TestPrometheusDownWritesNothing(t *testing.T) {
	w := &fakeWriter{}
	p := &fakeProm{err: errors.New("connection refused")}
	_, err := newRec(t, p, w, false, objects(wl{name: "w", svcs: map[string]string{"s": "a"}})...).Run(context.Background())
	if err == nil || len(w.calls) != 0 {
		t.Fatalf("err=%v calls=%+v", err, w.calls)
	}
}

// Idle workload processed before the failing one must not be written either.
func TestPrometheusFailsMidPass(t *testing.T) {
	w := &fakeWriter{}
	p := &flaky{fakeProm{healthy: map[string]bool{"up_a": true, "up_b": true}, pastOK: map[string]bool{"up_a": true, "up_b": true}}}
	objs := objects(wl{name: "a", svcs: map[string]string{"s1": "a"}}, wl{name: "b", svcs: map[string]string{"s2": "b"}})
	if _, err := newRec(t, p, w, false, objs...).Run(context.Background()); err == nil || len(w.calls) != 0 {
		t.Fatalf("err=%v calls=%+v", err, w.calls)
	}
}

type flaky struct{ fakeProm }

func (f *flaky) Query(ctx context.Context, expr string, at time.Time) (float64, bool, error) {
	if strings.HasPrefix(expr, "q:b") {
		return 0, false, errors.New("boom")
	}
	return f.fakeProm.Query(ctx, expr, at)
}

func TestHealthCachedPerProfile(t *testing.T) {
	p := &fakeProm{healthy: map[string]bool{"up_a": true}, pastOK: map[string]bool{"up_a": true}}
	objs := objects(wl{name: "w1", svcs: map[string]string{"s1": "a"}}, wl{name: "w2", svcs: map[string]string{"s2": "a"}}, wl{name: "w3", svcs: map[string]string{"s3": "a"}})
	if _, err := newRec(t, p, &fakeWriter{}, false, objs...).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls["up_a"] != 2 {
		t.Fatalf("health queried %d times, want 2 (now and now-window)", p.calls["up_a"])
	}
}

func TestWriteErrorExitsAfterOtherWorkloads(t *testing.T) {
	p := &fakeProm{healthy: map[string]bool{"up_a": true}, pastOK: map[string]bool{"up_a": true}}
	w := &fakeWriter{err: errors.New("denied")}
	objs := objects(wl{name: "w1", svcs: map[string]string{"s1": "a"}}, wl{name: "w2", svcs: map[string]string{"s2": "a"}})
	s, err := newRec(t, p, w, false, objs...).Run(context.Background())
	if err == nil || s.WriteErrors != 2 || len(w.calls) != 2 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestKubeWriterApply(t *testing.T) {
	c := kfake.NewSimpleClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db"}})
	var got []ktesting.PatchAction
	c.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		got = append(got, a.(ktesting.PatchAction))
		return true, nil, nil
	})
	w := KubeWriter{c}
	ctx := context.Background()
	if err := w.Apply(ctx, "Deployment", "ns", "w", map[string]string{"crickets.io/idle": "true"}, map[string]string{"crickets.io/idle-evidence": "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(ctx, "StatefulSet", "ns", "db", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(ctx, "Job", "ns", "x", nil, nil); err == nil {
		t.Fatal("unsupported kind must error")
	}
	if len(got) != 2 || got[0].GetPatchType() != types.ApplyPatchType || got[0].GetResource().Resource != "deployments" || got[1].GetResource().Resource != "statefulsets" {
		t.Fatalf("%+v", got)
	}
	var body map[string]any
	_ = json.Unmarshal(got[0].GetPatch(), &body)
	meta := body["metadata"].(map[string]any)
	if body["kind"] != "Deployment" || body["apiVersion"] != "apps/v1" || body["spec"] != nil ||
		meta["labels"].(map[string]any)["crickets.io/idle"] != "true" || meta["annotations"] == nil {
		t.Fatalf("%s", got[0].GetPatch())
	}
	// Removal is the same object without our fields, and still metadata-only.
	body = nil
	_ = json.Unmarshal(got[1].GetPatch(), &body)
	meta = body["metadata"].(map[string]any)
	if _, ok := meta["labels"]; ok || meta["name"] != "db" || body["spec"] != nil {
		t.Fatalf("%s", got[1].GetPatch())
	}
}

// A label set before the opt-out (or before the exposure went away) must come off,
// or a downstream cleaner still acts on it.
func TestReleasedRemovesOurLabel(t *testing.T) {
	labeled := func(extra map[string]string) func(*appsv1.Deployment) {
		return func(d *appsv1.Deployment) {
			d.Labels = map[string]string{"crickets.io/idle": "true"}
			d.Annotations = extra
		}
	}
	tests := map[string]wl{
		"ignored":   {name: "w", svcs: map[string]string{"s": "a"}, mod: labeled(map[string]string{"crickets.io/ignore": "true"})},
		"unexposed": {name: "w", svcs: map[string]string{"s": ""}, mod: labeled(nil)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, dry := range []bool{false, true} {
				w := &fakeWriter{}
				s, err := newRec(t, &fakeProm{}, w, dry, objects(tt)...).Run(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				wantCalls := 1
				if dry {
					wantCalls = 0
				}
				if s.Unlabeled != 1 || len(w.calls) != wantCalls || (wantCalls == 1 && w.calls[0].labels != nil) {
					t.Fatalf("dry=%v summary %+v calls %+v", dry, s, w.calls)
				}
			}
		})
	}
}
