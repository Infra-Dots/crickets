//go:build e2e

// Package e2e runs the crickets CronJob against a k3d cluster (hack/e2e-up.sh).
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func networkingIngress(host string) networkingv1.IngressLoadBalancerIngress {
	return networkingv1.IngressLoadBalancerIngress{Hostname: host}
}

func TestMain(m *testing.M) {
	if err := initClients(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// want: true = must be labeled idle, false = must not be.
var wantRun1 = map[string]bool{
	"busy-ing": false, "idle-ing": true, "rel-ignore": true, "rel-gone": true, "ignored-ing": false, "haproxy-ing": false, "unexposed": false,
	"gw-busy": false, "gw-idle": true, "mixed": false,
	"hz-idle": true, "hz-busy": false, "hz-empty": false, "hz-missing": false,
	"alb-busy": false, "alb-idle": true,
}

func TestCrickets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	if err := apply(ctx); err != nil {
		t.Fatal(err)
	}
	for n, h := range map[string]string{
		"alb-busy": "fixture-aws-alb-1234567890.us-east-1.elb.amazonaws.com",
		"alb-idle": "fixture-aws-alb-idle-1234567890.us-east-1.elb.amazonaws.com",
	} {
		if err := setIngressHostname(ctx, n, h); err != nil {
			t.Fatal(err)
		}
	}
	// Undo the release scenario's ignore annotation from a previous run.
	reset := []byte(`{"metadata":{"annotations":{"crickets.io/ignore":null}}}`)
	if _, err := cs.AppsV1().Deployments(ns).Patch(ctx, "rel-ignore", types.MergePatchType, reset, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := waitWorkloadsReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := waitHistory(ctx); err != nil {
		t.Fatalf("prometheus has no history: %v", err)
	}

	// Keep the busy edges busy for the whole run (the window is 2m).
	busy := map[string]string{
		"busy-ing.e2e.test": `crickets-e2e-busy-ing-.*@kubernetes`,
		"mixed.e2e.test":    `crickets-e2e-mixed-.*@kubernetes`,
		"gw-busy.e2e.test":  `crickets-e2e-gw-busy-https?-[0-9]+@kubernetesgateway`,
	}
	stop := make(chan struct{})
	defer close(stop)
	for h := range busy {
		go func(h string) {
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Second):
					get(h)
				}
			}
		}(h)
	}
	for h, re := range busy {
		if err := sendUntilSeen(ctx, h, re); err != nil {
			t.Fatalf("no traffic seen for %s: %v", h, err)
		}
	}

	t.Run("first run", func(t *testing.T) {
		if !runJob(ctx, t) {
			t.Fatal("job failed")
		}
		st, err := states(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range wantRun1 {
			t.Run(name, func(t *testing.T) {
				if got := st[name].Idle; got != want {
					t.Errorf("idle label = %v, want %v", got, want)
				}
			})
		}
		t.Run("evidence", func(t *testing.T) {
			var ev struct {
				Exposures []struct{ Provider string }
			}
			if err := json.Unmarshal([]byte(st["idle-ing"].Evidence), &ev); err != nil {
				t.Fatalf("evidence is not JSON: %v", err)
			}
			if len(ev.Exposures) != 1 || ev.Exposures[0].Provider != "traefik" {
				t.Errorf("evidence = %s", st["idle-ing"].Evidence)
			}
		})
		for name, want := range wantRun1 {
			if !want && st[name].Evidence != "" {
				t.Errorf("%s has evidence but no label", name)
			}
		}
	})

	t.Run("release", func(t *testing.T) {
		// Labeled workloads lose the label once ignored or no longer exposed.
		patch := []byte(`{"metadata":{"annotations":{"crickets.io/ignore":"true"}}}`)
		if _, err := cs.AppsV1().Deployments(ns).Patch(ctx, "rel-ignore", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := cs.NetworkingV1().Ingresses(ns).Delete(ctx, "rel-gone", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if !runJob(ctx, t) {
			t.Fatal("job failed")
		}
		st, _ := states(ctx)
		for _, n := range []string{"rel-ignore", "rel-gone"} {
			if st[n].Idle || st[n].Evidence != "" {
				t.Errorf("%s not released: %+v", n, st[n])
			}
		}
	})

	t.Run("recovery", func(t *testing.T) {
		if err := sendUntilSeen(ctx, "idle-ing.e2e.test", `crickets-e2e-idle-ing-.*@kubernetes`); err != nil {
			t.Fatal(err)
		}
		if !runJob(ctx, t) {
			t.Fatal("job failed")
		}
		st, _ := states(ctx)
		if st["idle-ing"].Idle || st["idle-ing"].Evidence != "" {
			t.Errorf("idle-ing still marked: %+v", st["idle-ing"])
		}
		if !st["gw-idle"].Idle {
			t.Error("gw-idle lost its label")
		}
	})

	t.Run("prometheus down", func(t *testing.T) {
		before, err := states(ctx)
		if err != nil {
			t.Fatal(err)
		}
		scale(ctx, t, 0)
		t.Cleanup(func() { scale(context.Background(), t, 1) })
		if err := poll(ctx, 2*time.Second, 2*time.Minute, func() (bool, error) {
			p, err := cs.CoreV1().Pods(sysNS).List(ctx, metav1.ListOptions{LabelSelector: "app=prometheus"})
			return err == nil && len(p.Items) == 0, err
		}); err != nil {
			t.Fatal(err)
		}
		if runJob(ctx, t) {
			t.Error("job succeeded without Prometheus")
		}
		after, _ := states(ctx)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("state changed:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}

func scale(ctx context.Context, t *testing.T, n int32) {
	t.Helper()
	d := cs.AppsV1().Deployments(sysNS)
	sc, err := d.GetScale(ctx, "prometheus", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sc.Spec.Replicas = n
	if _, err := d.UpdateScale(ctx, "prometheus", sc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return
	}
	_ = poll(ctx, 2*time.Second, 2*time.Minute, func() (bool, error) {
		p, err := d.Get(ctx, "prometheus", metav1.GetOptions{})
		return err == nil && p.Status.ReadyReplicas == n, nil
	})
}
