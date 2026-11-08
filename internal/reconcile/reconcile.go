// Package reconcile decides which workloads are idle and labels them.
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Infra-Dots/crickets/internal/config"
	"github.com/Infra-Dots/crickets/internal/discovery"
	"github.com/Infra-Dots/crickets/internal/prom"
	"github.com/Infra-Dots/crickets/internal/providers"
)

type Verdict string

const (
	Idle    Verdict = "idle"
	Active  Verdict = "active"
	Unknown Verdict = "unknown"
)

type EvidenceExposure struct {
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
	Provider string  `json:"provider"`
	Query    string  `json:"query"`
	Value    float64 `json:"value"`
}

type Evidence struct {
	Since     time.Time          `json:"since"`
	Window    string             `json:"window"`
	Threshold float64            `json:"threshold"`
	Exposures []EvidenceExposure `json:"exposures"`
}

type Summary struct {
	Workloads, Idle, Active, Unknown, Unexposed, Ignored, Young int
	Labeled, Unlabeled, WriteErrors                             int
}

type Reconciler struct {
	Disc      *discovery.Discoverer
	Providers *providers.Set
	Prom      prom.Querier
	Writer    Writer
	Cfg       *config.Config
	Now       func() time.Time
	Log       *slog.Logger
}

type health struct{ ok bool }

type decision struct {
	w        discovery.Workload
	verdict  Verdict
	evidence []EvidenceExposure
}

// Run does one pass. A Prometheus error aborts before any write.
func (r *Reconciler) Run(ctx context.Context) (Summary, error) {
	now := r.Now()
	var s Summary
	res, err := r.Disc.Discover(ctx, discovery.Options{
		Include: r.Cfg.Namespaces.Include, Exclude: r.Cfg.Namespaces.Exclude,
		Selector: r.Cfg.Selector, MinAge: r.Cfg.MinAge, Now: now,
	})
	if err != nil {
		return s, err
	}
	s.Ignored, s.Young = res.Ignored, res.Young

	healthy := map[string]health{} // per profile, per pass
	var decisions []decision
	for _, w := range res.Workloads {
		if len(w.Exposures) == 0 {
			s.Unexposed++
			continue
		}
		d, err := r.decide(ctx, w, now, healthy)
		if err != nil {
			return s, fmt.Errorf("prometheus: %w", err)
		}
		s.Workloads++
		switch d.verdict {
		case Idle:
			s.Idle++
		case Active:
			s.Active++
		default:
			s.Unknown++
		}
		decisions = append(decisions, d)
	}

	var errs []error
	for _, d := range decisions {
		if err := r.write(ctx, d, now, &s); err != nil {
			s.WriteErrors++
			errs = append(errs, err)
			r.Log.Error("write failed", "kind", d.w.Kind, "namespace", d.w.Namespace, "name", d.w.Name, "error", err)
		}
	}
	return s, errors.Join(errs...)
}

func (r *Reconciler) decide(ctx context.Context, w discovery.Workload, now time.Time, cache map[string]health) (decision, error) {
	d := decision{w: w}
	unknown, busy := false, false
	for _, e := range w.Exposures {
		log := r.Log.With("kind", w.Kind, "namespace", w.Namespace, "name", w.Name, "exposure", e.ExposureKind)
		p := r.Providers.Match(e.Facts)
		if p == nil {
			log.Info("unknown: no provider profile")
			unknown = true
			continue
		}
		h, ok := cache[p.Name]
		if !ok {
			var err error
			if h.ok, err = r.healthy(ctx, p, now); err != nil {
				return d, err
			}
			cache[p.Name] = h
		}
		if !h.ok {
			log.Info("unknown: source unhealthy or retention shorter than window", "provider", p.Name)
			unknown = true
			continue
		}
		q, err := p.Render(e.Exposure, r.Cfg.Window)
		if err != nil {
			log.Error("unknown: query does not render", "provider", p.Name, "error", err)
			unknown = true
			continue
		}
		v, _, err := r.Prom.Query(ctx, q, now) // empty from a healthy source is 0
		if err != nil {
			return d, err
		}
		d.evidence = append(d.evidence, EvidenceExposure{e.ExposureKind, exposureName(e), p.Name, q, v})
		if v > r.Cfg.Threshold {
			busy = true
		}
	}
	switch {
	case busy:
		d.verdict = Active
	case unknown:
		d.verdict = Unknown
	default:
		d.verdict = Idle
	}
	return d, nil
}

func (r *Reconciler) healthy(ctx context.Context, p *providers.Profile, now time.Time) (bool, error) {
	for _, at := range []time.Time{now, now.Add(-r.Cfg.Window)} {
		_, empty, err := r.Prom.Query(ctx, p.Health, at)
		if err != nil {
			return false, err
		}
		if empty {
			return false, nil
		}
	}
	return true, nil
}

func exposureName(e discovery.Exposure) string {
	switch e.ExposureKind {
	case providers.KindIngress:
		return e.Ingress
	case providers.KindHTTPRoute:
		return e.Route
	}
	return e.Service
}

func (r *Reconciler) write(ctx context.Context, d decision, now time.Time, s *Summary) error {
	label, evKey := r.Cfg.Label, r.Cfg.EvidenceKey()
	labeled := d.w.Labels[label] == "true"
	log := r.Log.With("kind", d.w.Kind, "namespace", d.w.Namespace, "name", d.w.Name)

	switch d.verdict {
	case Idle:
		since := now
		if labeled {
			var old Evidence
			if json.Unmarshal([]byte(d.w.Annotations[evKey]), &old) == nil && !old.Since.IsZero() {
				since = old.Since
			}
		}
		b, err := json.Marshal(Evidence{since, providers.WindowString(r.Cfg.Window), r.Cfg.Threshold, d.evidence})
		if err != nil {
			return err
		}
		if labeled && d.w.Annotations[evKey] == string(b) {
			return nil
		}
		if r.Cfg.DryRun {
			if !labeled {
				log.Info("would label", "evidence", string(b))
				s.Labeled++
			}
			return nil
		}
		if err := r.Writer.Apply(ctx, d.w.Kind, d.w.Namespace, d.w.Name,
			map[string]string{label: "true"}, map[string]string{evKey: string(b)}); err != nil {
			return err
		}
		if !labeled {
			log.Info("labeled idle", "evidence", string(b))
			s.Labeled++
		}
	case Active:
		if !labeled {
			return nil
		}
		if r.Cfg.DryRun {
			log.Info("would unlabel")
			s.Unlabeled++
			return nil
		}
		if err := r.Writer.Apply(ctx, d.w.Kind, d.w.Namespace, d.w.Name, nil, nil); err != nil {
			return err
		}
		log.Info("unlabeled, traffic seen")
		s.Unlabeled++
	}
	return nil
}
