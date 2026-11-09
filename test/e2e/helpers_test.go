//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ns      = "crickets-e2e"
	sysNS   = "crickets-system"
	idleLbl = "crickets.io/idle"
	evidKey = "crickets.io/idle-evidence"
)

var (
	cs      kubernetes.Interface
	dyn     dynamic.Interface
	mapper  meta.RESTMapper
	trafURL = "http://127.0.0.1:" + envOr("E2E_PORT", "18080")
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func initClients() error {
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return err
	}
	if cs, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}
	if dyn, err = dynamic.NewForConfig(cfg); err != nil {
		return err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	grs, err := restmapper.GetAPIGroupResources(dc)
	mapper = restmapper.NewDiscoveryRESTMapper(grs)
	return err
}

// apply server-side applies every document in test/e2e/manifests/*.yaml into ns.
func apply(ctx context.Context) error {
	_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	files, _ := filepath.Glob("manifests/*.yaml")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
		for {
			u := &unstructured.Unstructured{}
			if err := dec.Decode(&u.Object); err == io.EOF {
				break
			} else if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			if len(u.Object) == 0 {
				continue
			}
			m, err := mapper.RESTMapping(u.GroupVersionKind().GroupKind(), u.GroupVersionKind().Version)
			if err != nil {
				return err
			}
			u.SetNamespace(ns)
			data, _ := json.Marshal(u.Object)
			_, err = dyn.Resource(m.Resource).Namespace(ns).Patch(ctx, u.GetName(), types.ApplyPatchType, data,
				metav1.PatchOptions{FieldManager: "e2e", Force: ptr(true)})
			if err != nil {
				return fmt.Errorf("apply %s/%s: %w", u.GetKind(), u.GetName(), err)
			}
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

// setIngressHostname fakes the status a cloud controller would write.
func setIngressHostname(ctx context.Context, name, host string) error {
	ing, err := cs.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ing.Status.LoadBalancer.Ingress = nil
	ing.Status.LoadBalancer.Ingress = append(ing.Status.LoadBalancer.Ingress, networkingIngress(host))
	_, err = cs.NetworkingV1().Ingresses(ns).UpdateStatus(ctx, ing, metav1.UpdateOptions{})
	return err
}

func poll(ctx context.Context, every, timeout time.Duration, f func() (bool, error)) error {
	return wait.PollUntilContextTimeout(ctx, every, timeout, true, func(context.Context) (bool, error) { return f() })
}

func waitWorkloadsReady(ctx context.Context) error {
	return poll(ctx, 2*time.Second, 5*time.Minute, func() (bool, error) {
		ds, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		ss, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for _, d := range ds.Items {
			if d.Status.ReadyReplicas < 1 {
				return false, nil
			}
		}
		for _, s := range ss.Items {
			if s.Status.ReadyReplicas < 1 {
				return false, nil
			}
		}
		return len(ds.Items)+len(ss.Items) > 0, nil
	})
}

// get sends one request through Traefik with the given Host.
func get(host string) int {
	req, _ := http.NewRequest("GET", trafURL, nil)
	req.Host = host
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// promValue returns the first sample of an instant query, or 0 for an empty result.
func promValue(ctx context.Context, q string) (float64, bool, error) {
	raw, err := cs.CoreV1().Services(sysNS).ProxyGet("http", "prometheus", "9090", "api/v1/query", map[string]string{"query": q}).DoRaw(ctx)
	if err != nil {
		return 0, false, err
	}
	var r struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, false, err
	}
	if len(r.Data.Result) == 0 {
		return 0, false, nil
	}
	v, _ := strconv.ParseFloat(fmt.Sprint(r.Data.Result[0].Value[1]), 64)
	return v, true, nil
}

// waitHistory blocks until both scrape targets have data older than one window.
func waitHistory(ctx context.Context) error {
	return poll(ctx, 3*time.Second, 4*time.Minute, func() (bool, error) {
		for _, j := range []string{"traefik", "fixtures"} {
			_, ok, err := promValue(ctx, fmt.Sprintf(`count(up{job=%q} offset 130s == 1)`, j))
			if err != nil || !ok {
				return false, nil
			}
		}
		return true, nil
	})
}

// sendUntilSeen sends requests until Prometheus counts traffic for the Traefik service svc.
func sendUntilSeen(ctx context.Context, host, svcRegex string) error {
	q := fmt.Sprintf(`sum(increase(traefik_service_requests_total{service=~%q}[2m]))`, svcRegex)
	return poll(ctx, time.Second, 2*time.Minute, func() (bool, error) {
		get(host)
		v, ok, err := promValue(ctx, q)
		return err == nil && ok && v > 0, nil
	})
}

// runJob makes a Job from the CronJob, waits for it and returns whether it succeeded.
func runJob(ctx context.Context, t *testing.T) bool {
	t.Helper()
	cj, err := cs.BatchV1().CronJobs(sysNS).Get(ctx, "crickets", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job, err := cs.BatchV1().Jobs(sysNS).Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "crickets-e2e-",
			Annotations:  map[string]string{"cronjob.kubernetes.io/instantiate": "manual"},
		},
		Spec: cj.Spec.JobTemplate.Spec,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	err = poll(ctx, time.Second, 3*time.Minute, func() (bool, error) {
		j, err := cs.BatchV1().Jobs(sysNS).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, c := range j.Status.Conditions {
			if c.Status == corev1.ConditionTrue && (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) {
				ok = c.Type == batchv1.JobComplete
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("job %s did not finish: %v", job.Name, err)
	}
	logJob(ctx, t, job.Name)
	return ok
}

func logJob(ctx context.Context, t *testing.T, job string) {
	pods, _ := cs.CoreV1().Pods(sysNS).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	for _, p := range pods.Items {
		b, _ := cs.CoreV1().Pods(sysNS).GetLogs(p.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		t.Logf("job %s logs:\n%s", job, b)
	}
}

type state struct {
	Idle     bool
	Evidence string
}

// states reads the label and evidence of every Deployment and StatefulSet in ns.
func states(ctx context.Context) (map[string]state, error) {
	out := map[string]state{}
	ds, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range ds.Items {
		out[d.Name] = state{d.Labels[idleLbl] == "true", d.Annotations[evidKey]}
	}
	ss, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, s := range ss.Items {
		out[s.Name] = state{s.Labels[idleLbl] == "true", s.Annotations[evidKey]}
	}
	return out, nil
}
