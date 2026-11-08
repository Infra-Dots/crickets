// Package providers holds the profiles that map an exposure to a Prometheus query.
package providers

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"sigs.k8s.io/yaml"
)

//go:embed profiles.yaml
var builtin []byte

const (
	KindIngress   = "Ingress"
	KindHTTPRoute = "HTTPRoute"
	KindService   = "Service"
)

type Match struct {
	Kinds              []string `json:"kinds"`
	IngressClasses     []string `json:"ingressClasses"`
	GatewayControllers []string `json:"gatewayControllers"`
	GatewayClasses     []string `json:"gatewayClasses"`
	Annotations        []string `json:"annotations"`
	HostnameSuffixes   []string `json:"hostnameSuffixes"`
}

type Profile struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Unit      string `json:"unit"`
	Match     Match  `json:"match"`
	Query     string `json:"query"`
	Health    string `json:"health"`

	tmpl *template.Template
}

// Exposure is the template data for Profile.Query.
type Exposure struct {
	Namespace, Kind, Workload string
	ExposureKind, Service     string
	ServicePorts              []int32
	Ingress, Route            string
	RouteNamespace            string
	Gateway, GatewayNamespace string
	Hosts                     []string
	LBHostname, LBIP          string
	Annotations               map[string]string
	Window                    string
}

// Facts is what Match looks at; discovery fills it next to the Exposure.
type Facts struct {
	Kind              string // Ingress | HTTPRoute | Service
	IngressClass      string
	GatewayController string
	GatewayClass      string
	Annotations       map[string]string
	LBHostnames       []string
}

// Set is an ordered list of profiles; the first supported match wins.
type Set struct{ profiles []*Profile }

// Parse reads and validates a profile list.
func Parse(b []byte) ([]Profile, error) {
	var ps []Profile
	if err := yaml.UnmarshalStrict(b, &ps); err != nil {
		return nil, fmt.Errorf("parse profiles: %w", err)
	}
	seen := map[string]bool{}
	for i := range ps {
		if err := ps[i].compile(); err != nil {
			return nil, fmt.Errorf("profile %q: %w", ps[i].Name, err)
		}
		if seen[ps[i].Name] {
			return nil, fmt.Errorf("duplicate profile %q", ps[i].Name)
		}
		seen[ps[i].Name] = true
	}
	return ps, nil
}

func (p *Profile) compile() error {
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(p.Match.Kinds) == 0 {
		return fmt.Errorf("match.kinds is required")
	}
	for _, k := range p.Match.Kinds {
		if k != KindIngress && k != KindHTTPRoute && k != KindService {
			return fmt.Errorf("unknown kind %q", k)
		}
	}
	if !p.Supported {
		return nil
	}
	m := p.Match
	if len(m.IngressClasses)+len(m.GatewayControllers)+len(m.GatewayClasses)+len(m.Annotations)+len(m.HostnameSuffixes) == 0 {
		return fmt.Errorf("a supported profile needs a matcher besides kinds")
	}
	if strings.TrimSpace(p.Health) == "" || strings.TrimSpace(p.Query) == "" {
		return fmt.Errorf("query and health are required")
	}
	t, err := template.New(p.Name).Funcs(template.FuncMap{"lbName": LBName, "need": need}).Option("missingkey=error").Parse(p.Query)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	p.tmpl = t
	return nil
}

// Load returns user profiles first, then the built-ins; a user profile replaces a built-in of the same name.
func Load(user []Profile) (*Set, error) {
	base, err := Parse(builtin)
	if err != nil {
		return nil, err
	}
	s := &Set{}
	names := map[string]bool{}
	for i := range user {
		if err := user[i].compile(); err != nil {
			return nil, fmt.Errorf("provider %q: %w", user[i].Name, err)
		}
		names[user[i].Name] = true
		s.profiles = append(s.profiles, &user[i])
	}
	for i := range base {
		if !names[base[i].Name] {
			s.profiles = append(s.profiles, &base[i])
		}
	}
	return s, nil
}

// Match returns the first supported profile that fits, or nil.
func (s *Set) Match(f Facts) *Profile {
	for _, p := range s.profiles {
		if p.Supported && p.matches(f) {
			return p
		}
	}
	return nil
}

func (p *Profile) matches(f Facts) bool {
	m := p.Match
	if !slices.Contains(m.Kinds, f.Kind) {
		return false
	}
	if slices.Contains(m.IngressClasses, f.IngressClass) && f.IngressClass != "" {
		return true
	}
	if f.GatewayController != "" && slices.Contains(m.GatewayControllers, f.GatewayController) {
		return true
	}
	if f.GatewayClass != "" {
		for _, g := range m.GatewayClasses {
			if ok, _ := path.Match(g, f.GatewayClass); ok {
				return true
			}
		}
	}
	for _, a := range m.Annotations {
		if _, ok := f.Annotations[a]; ok {
			return true
		}
	}
	for _, h := range f.LBHostnames {
		for _, suf := range m.HostnameSuffixes {
			if strings.HasSuffix(h, suf) {
				return true
			}
		}
	}
	return false
}

// WindowString renders whole seconds, e.g. 604800s; Go's 168h0m0s is not valid PromQL.
func WindowString(d time.Duration) string { return fmt.Sprintf("%ds", int64(d/time.Second)) }

// Render fills the query template for one exposure.
func (p *Profile) Render(e Exposure, window time.Duration) (string, error) {
	e.Window = WindowString(window)
	// Annotations are user-written and land inside PromQL string literals.
	esc := make(map[string]string, len(e.Annotations))
	for k, v := range e.Annotations {
		esc[k] = promEscape.Replace(v)
	}
	e.Annotations = esc
	var b bytes.Buffer
	if err := p.tmpl.Execute(&b, e); err != nil {
		return "", fmt.Errorf("render %s: %w", p.Name, err)
	}
	return b.String(), nil
}

var promEscape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// need fails the render on an empty join key, so the exposure is unknown instead of matching nothing (= idle).
func need(v string) (string, error) {
	if v == "" {
		return "", errors.New("missing join key")
	}
	return v, nil
}

var (
	nlbHost = regexp.MustCompile(`^(?:internal-)?(.+)-[0-9a-f]{16}\.elb\.[a-z0-9-]+\.amazonaws\.com(?:\.cn)?$`)
	albHost = regexp.MustCompile(`^(?:internal-|dualstack\.)*(.+)-[0-9]+\.[a-z0-9-]+\.elb\.amazonaws\.com(?:\.cn)?$`)
)

// LBName turns an AWS ALB/NLB hostname into the LB name YACE reports; "" for anything else.
func LBName(hostname string) string {
	for _, re := range []*regexp.Regexp{nlbHost, albHost} {
		if m := re.FindStringSubmatch(hostname); m != nil {
			return m[1]
		}
	}
	return ""
}
